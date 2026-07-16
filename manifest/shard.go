package manifest

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// LedgerDirName is the directory, co-located with the subtree it indexes, that
// holds a shard's snapshot and log.
const LedgerDirName = ".ledger"

// snapshotFileName is a shard's compacted state.
const snapshotFileName = "snapshot.json"

// LogFileName is a shard's append-only log of changes since its snapshot.
const LogFileName = "log.ndjson"

// configVersionsSegment is the archive path segment, and the shard key, for the
// org-wide configuration-version objects, which key to their own shard.
const configVersionsSegment = "config-versions"

// shardKey routes an archive-relative key (an object's path or an append-mostly
// collection's high-water-mark key) to the shard that owns it.
//
// A workspace or stack subtree keys to its own shard so the shard stays scoped
// to one bounded slice of the tree; the org-wide configuration versions key to
// theirs; everything else (org-level objects, and the stack and audit cursors,
// whose keys are id- rather than path-shaped) keys to the org-root shard, the
// empty key.
func shardKey(key string) string {
	segs := strings.Split(key, "/")

	switch {
	case len(segs) >= 4 && segs[0] == "projects" && (segs[2] == "workspaces" || segs[2] == "stacks"):
		return path.Join(segs[0], segs[1], segs[2], segs[3])
	case segs[0] == configVersionsSegment:
		return configVersionsSegment
	default:
		return ""
	}
}

// shard is one slice of the ledger (a workspace, a stack, the org-wide
// configuration versions, or the org root) persisted as a compacted snapshot
// plus an append-only log in a co-located [LedgerDirName] directory.
//
// It holds the entries, watermarks, and completion flags whose keys route to it
// (see [shardKey]); the org-root shard additionally carries the run-level
// metadata, which is global to the archive rather than scoped to a subtree. Its
// maps are guarded by the owning [Ledger]'s mutex, so a shard has no lock of its
// own.
type shard struct {
	lastRunAt       time.Time
	dirtyCompleted  map[string]struct{}
	watermarks      map[string]time.Time
	completed       map[string]bool
	settled         map[string]bool
	dirtySettled    map[string]struct{}
	dirtyUnsettled  map[string]struct{}
	dirtyEntries    map[string]struct{}
	dirtyWatermarks map[string]struct{}
	lastRun         *RunRecord
	entries         map[string]*Entry
	dir             string
	logBytes        int64
	snapshotBytes   int64
	runCount        int
	runDirty        bool
}

// newShard creates an empty shard rooted at dir.
func newShard(dir string) *shard {
	return &shard{
		dir:             dir,
		entries:         make(map[string]*Entry),
		watermarks:      make(map[string]time.Time),
		completed:       make(map[string]bool),
		settled:         make(map[string]bool),
		dirtyEntries:    make(map[string]struct{}),
		dirtyWatermarks: make(map[string]struct{}),
		dirtyCompleted:  make(map[string]struct{}),
		dirtySettled:    make(map[string]struct{}),
		dirtyUnsettled:  make(map[string]struct{}),
	}
}

// snapshotPath returns the path of the shard's compacted snapshot.
func (s *shard) snapshotPath() string {
	return filepath.Join(s.dir, snapshotFileName)
}

// logPath returns the path of the shard's append-only log.
func (s *shard) logPath() string {
	return filepath.Join(s.dir, LogFileName)
}

// load reads the shard's snapshot and replays its log on top, populating its
// in-memory state. A missing snapshot or log is an empty start; a torn trailing
// log line is dropped; a corrupt snapshot or a complete but unparsable log line
// returns [ErrCorruptManifest].
func (s *shard) load() error {
	//nolint:gosec // The shard directory is derived from the operator-chosen root.
	data, err := os.ReadFile(s.snapshotPath())

	var doc document

	switch {
	case errors.Is(err, fs.ErrNotExist):
		// No snapshot yet; the log alone, if any, is replayed below.
	case err != nil:
		return fmt.Errorf("read snapshot %q: %w", s.snapshotPath(), err)
	default:
		err = json.Unmarshal(data, &doc)
		if err != nil {
			return fmt.Errorf("%w: %w", ErrCorruptManifest, err)
		}

		if doc.Version > schemaVersion {
			return fmt.Errorf("%w: schema version %d is newer than supported %d",
				ErrCorruptManifest, doc.Version, schemaVersion)
		}
	}

	s.applyDocument(&doc)

	s.snapshotBytes = int64(len(data))

	recs, logBytes, err := replayLog(s.logPath())
	if err != nil {
		return err
	}

	for i := range recs {
		err = s.applyRecord(&recs[i])
		if err != nil {
			return err
		}
	}

	s.logBytes = logBytes

	return nil
}

// applyDocument seeds the shard from a loaded snapshot document.
func (s *shard) applyDocument(doc *document) {
	if doc.Entries != nil {
		s.entries = doc.Entries
	}

	if doc.HighWaterMarks != nil {
		s.watermarks = doc.HighWaterMarks
	}

	if doc.Completed != nil {
		s.completed = doc.Completed
	}

	if doc.Settled != nil {
		s.settled = doc.Settled
	}

	s.lastRunAt = doc.LastRunAt
	s.runCount = doc.RunCount
	s.lastRun = doc.LastRun
}

// applyRecord applies one replayed log record to the shard.
func (s *shard) applyRecord(rec *walRecord) error {
	switch rec.Kind {
	case walEntry:
		if rec.Entry == nil {
			return fmt.Errorf("%w: log entry %q has no record", ErrCorruptManifest, rec.Path)
		}

		s.entries[rec.Path] = rec.Entry

	case walWatermark:
		s.watermarks[rec.Key] = rec.At
	case walCompleted:
		s.completed[rec.Key] = true
	case walSettled:
		s.settled[rec.Key] = rec.Settled
	case walRun:
		s.lastRunAt = rec.LastRunAt
		s.runCount = rec.RunCount
		s.lastRun = rec.LastRun

	default:
		return fmt.Errorf("%w: unknown log record kind %q", ErrCorruptManifest, rec.Kind)
	}

	return nil
}

// hasDirty reports whether the shard has state to append on the next flush.
func (s *shard) hasDirty() bool {
	return len(s.dirtyEntries) > 0 ||
		len(s.dirtyWatermarks) > 0 ||
		len(s.dirtyCompleted) > 0 ||
		len(s.dirtySettled) > 0 ||
		len(s.dirtyUnsettled) > 0 ||
		s.runDirty
}

// drainedState is one drain's output: the log records to append and the dirty
// sets they were built from, taken whole.
//
// Carrying the sets themselves (rather than re-deriving them from the records'
// kinds) makes restore a plain union with no per-kind dispatch: a record kind
// added later cannot be silently dropped on the restore path, because there is
// no second list of kinds to keep in step with the drain.
type drainedState struct {
	entries    map[string]struct{}
	watermarks map[string]struct{}
	completed  map[string]struct{}
	settled    map[string]struct{}
	unsettled  map[string]struct{}
	recs       []walRecord
	run        bool
}

// drainDirty builds the log records for everything changed since the last flush
// and hands the dirty sets to the returned [drainedState], leaving the shard's
// sets empty, so a failed append can restore exactly what was drained (see
// [shard.restoreDirty]).
//
// It runs under the owning ledger's write lock; the entry it emits is a copy, so
// a concurrent record does not race the marshal that follows outside the lock.
func (s *shard) drainDirty() drainedState {
	d := drainedState{
		entries:    s.dirtyEntries,
		watermarks: s.dirtyWatermarks,
		completed:  s.dirtyCompleted,
		settled:    s.dirtySettled,
		unsettled:  s.dirtyUnsettled,
		run:        s.runDirty,
	}

	s.dirtyEntries = make(map[string]struct{})
	s.dirtyWatermarks = make(map[string]struct{})
	s.dirtyCompleted = make(map[string]struct{})
	s.dirtySettled = make(map[string]struct{})
	s.dirtyUnsettled = make(map[string]struct{})
	s.runDirty = false

	d.recs = make([]walRecord, 0,
		len(d.unsettled)+len(d.entries)+len(d.watermarks)+len(d.completed)+len(d.settled)+1)

	// A collection's unsettlement leads every entry in the batch. The log is
	// replayed as a prefix after a crash (a torn tail drops), so the order
	// within one append is a durability order: were the unsettle record to
	// trail the entries, a crash between them would persist freshly frozen
	// entries under a stale settled flag, and the next walk's early stop
	// would halt above elements the interrupted walk never listed. Leading,
	// the record is durable before any entry it guards; a settlement earned
	// later in the same batch still lands after the entries as usual.
	for key := range d.unsettled {
		d.recs = append(d.recs, walRecord{Kind: walSettled, Key: key})
	}

	for relPath := range d.entries {
		e := s.entries[relPath]
		if e == nil {
			continue
		}

		cp := cloneEntry(*e)

		d.recs = append(d.recs, walRecord{Kind: walEntry, Path: relPath, Entry: &cp})
	}

	for key := range d.watermarks {
		d.recs = append(d.recs, walRecord{Kind: walWatermark, Key: key, At: s.watermarks[key]})
	}

	for key := range d.completed {
		d.recs = append(d.recs, walRecord{Kind: walCompleted, Key: key})
	}

	for key := range d.settled {
		// A key drained unsettled whose value is still false already leads the
		// batch; re-appending it here would only duplicate the line.
		if _, led := d.unsettled[key]; led && !s.settled[key] {
			continue
		}

		d.recs = append(d.recs, walRecord{Kind: walSettled, Key: key, Settled: s.settled[key]})
	}

	if d.run {
		rec := walRecord{Kind: walRun, LastRunAt: s.lastRunAt, RunCount: s.runCount}

		if s.lastRun != nil {
			lr := cloneRunRecord(*s.lastRun)
			rec.LastRun = &lr
		}

		d.recs = append(d.recs, rec)
	}

	return d
}

// restoreDirty unions a drain's dirty sets back into the shard, undoing a
// [shard.drainDirty] whose records never reached the log. The union direction
// matters: a key re-dirtied by a worker during the failed flush stays dirty,
// and the drained keys rejoin it, so a retry re-appends the whole delta rather
// than dropping it. It runs under the owning ledger's write lock.
func (s *shard) restoreDirty(d drainedState) {
	for k := range d.entries {
		s.dirtyEntries[k] = struct{}{}
	}

	for k := range d.watermarks {
		s.dirtyWatermarks[k] = struct{}{}
	}

	for k := range d.completed {
		s.dirtyCompleted[k] = struct{}{}
	}

	for k := range d.settled {
		s.dirtySettled[k] = struct{}{}
	}

	for k := range d.unsettled {
		s.dirtyUnsettled[k] = struct{}{}
	}

	s.runDirty = s.runDirty || d.run
}

// document builds a fully detached snapshot document for the shard: its maps
// and each entry are copied rather than shared with the live shard, so the
// caller may marshal it after releasing the ledger lock while records keep
// mutating the shard. It runs under the owning ledger's read lock, and mirrors
// the per-entry copy [shard.drainDirty] makes for the same reason.
func (s *shard) document() document {
	entries := make(map[string]*Entry, len(s.entries))

	for relPath, e := range s.entries {
		if e == nil {
			continue
		}

		cp := cloneEntry(*e)

		entries[relPath] = &cp
	}

	var lastRun *RunRecord

	if s.lastRun != nil {
		lr := cloneRunRecord(*s.lastRun)
		lastRun = &lr
	}

	return document{
		Version:        schemaVersion,
		LastRunAt:      s.lastRunAt,
		LastRun:        lastRun,
		RunCount:       s.runCount,
		HighWaterMarks: maps.Clone(s.watermarks),
		Entries:        entries,
		Completed:      maps.Clone(s.completed),
		Settled:        maps.Clone(s.settled),
	}
}
