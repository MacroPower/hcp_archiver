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
	"slices"
	"strings"
	"time"
)

// LedgerDirName is the directory, co-located with the subtree it indexes, that
// holds a shard's snapshot; the org root's additionally holds the ledger's log.
const LedgerDirName = ".ledger"

// snapshotFileName is a shard's compacted state.
const snapshotFileName = "snapshot.json"

// LogFileName is the ledger's append-only log of changes since the snapshots,
// kept in the org-root shard's directory.
const LogFileName = "log.ndjson"

// configVersionsSegment is the archive path segment, and the shard key, for the
// org-wide configuration-version objects, which key to their own shard.
const configVersionsSegment = "config-versions"

// shardKey routes an archive-relative key (an object's path or a collection's
// archive prefix) to the shard that owns it.
//
// A workspace or stack subtree keys to its own shard so the shard stays scoped
// to one bounded slice of the tree; the org-wide configuration versions key to
// theirs; everything else (org-level objects and org-level collection
// prefixes) keys to the org-root shard, the empty key.
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

// ShardScope returns the archive-relative directory of the shard that owns
// relPath: a workspace or stack subtree, the configuration-versions
// directory, or "" for the org root. It is the scope [Ledger.ResumedUnder]
// answers for, exported so a caller can pair a shard's resume state with the
// on-disk presence of the subtree it indexes.
func ShardScope(relPath string) string {
	return shardKey(relPath)
}

// shard is one slice of the ledger (a workspace, a stack, the org-wide
// configuration versions, or the org root) persisted as a compacted snapshot
// in a co-located [LedgerDirName] directory. Changes since the snapshot live
// in the ledger's single org-level log (see [Ledger.Flush]), tagged with the
// shard's key so a replay routes them back.
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
	inflightEntries map[string]struct{}
	dirtyWatermarks map[string]struct{}
	lastRun         *RunRecord
	entries         map[string]*Entry
	dir             string
	runCount        int
	loadedVersion   int
	runDirty        bool
	stale           bool
	// Whether the shard held any entry when the current run started (see
	// [Ledger.StartRun]), the shard-scoped reading behind
	// [Ledger.ResumedUnder]: an organization-wide resume proves nothing about
	// one subtree whose own ledger directory was lost.
	resumed bool
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
		inflightEntries: make(map[string]struct{}),
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

// loadSnapshot reads the shard's compacted snapshot into its in-memory state.
// A missing snapshot is an empty start; a corrupt one returns
// [ErrCorruptManifest]. The org-level log's records layer on top afterwards,
// replayed and routed by the ledger (see [Ledger.Load]).
func (s *shard) loadSnapshot() error {
	//nolint:gosec // The shard directory is derived from the operator-chosen root.
	data, err := os.ReadFile(s.snapshotPath())

	var doc document

	switch {
	case errors.Is(err, fs.ErrNotExist):
		// No snapshot yet; any log records replay onto the empty state.
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

	// A missing snapshot leaves the version 0: unknown, since log records carry
	// no version of their own (see [Migration]).
	s.loadedVersion = doc.Version

	s.applyDocument(&doc)

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

	// The drained entries stay in flight until the append that carries them
	// is acknowledged (see [shard.ackDrained]), so a durability read between
	// the drain and the fsync never mistakes a not-yet-written record for a
	// durable one.
	for k := range d.entries {
		s.inflightEntries[k] = struct{}{}
	}

	s.dirtyEntries = make(map[string]struct{})
	s.dirtyWatermarks = make(map[string]struct{})
	s.dirtyCompleted = make(map[string]struct{})
	s.dirtySettled = make(map[string]struct{})
	s.dirtyUnsettled = make(map[string]struct{})
	s.runDirty = false

	d.recs = make([]walRecord, 0,
		len(d.unsettled)+len(d.entries)+len(d.watermarks)+len(d.completed)+len(d.settled)+1)

	// The batch is a durability order: the log is replayed as a prefix after a
	// crash (a torn tail truncates), so within one append every guard must
	// precede every record that could act as a skip signal for it. The rule,
	// applied by class: collection unsettlements lead, then unsettled-status
	// entries, then settled-status entries, then the collection-level skip
	// signals (watermarks, completion, settlement). Each class iterates in
	// sorted key order so the layout is deterministic rather than map-random.
	//
	// The entry split is what makes a same-batch guard safe: a settled entry
	// can be a skip signal for unsettled work drained beside it (run-events
	// recorded done while its actors gate is still pending, say), and were the
	// two to land in map order, a tear between them could persist the skip
	// signal without the guard. Unsettled-first, every prefix that contains a
	// settled entry also contains every guard drained with it; the reverse
	// tear only loses a skip signal, which costs a retry.
	for _, key := range slices.Sorted(maps.Keys(d.unsettled)) {
		d.recs = append(d.recs, walRecord{Kind: walSettled, Key: key})
	}

	entryPaths := slices.Sorted(maps.Keys(d.entries))

	for _, settled := range []bool{false, true} {
		for _, relPath := range entryPaths {
			e := s.entries[relPath]
			if e == nil || e.Status.Settled() != settled {
				continue
			}

			cp := cloneEntry(*e)

			d.recs = append(d.recs, walRecord{Kind: walEntry, Path: relPath, Entry: &cp})
		}
	}

	for _, key := range slices.Sorted(maps.Keys(d.watermarks)) {
		d.recs = append(d.recs, walRecord{Kind: walWatermark, Key: key, At: s.watermarks[key]})
	}

	for _, key := range slices.Sorted(maps.Keys(d.completed)) {
		d.recs = append(d.recs, walRecord{Kind: walCompleted, Key: key})
	}

	for _, key := range slices.Sorted(maps.Keys(d.settled)) {
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
		delete(s.inflightEntries, k)
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

// ackDrained marks a drain's entries durable: the append carrying their
// records reached the fsynced log, closing the in-flight window
// [shard.drainDirty] opened. It runs under the owning ledger's write lock.
func (s *shard) ackDrained(d drainedState) {
	for k := range d.entries {
		delete(s.inflightEntries, k)
	}
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
