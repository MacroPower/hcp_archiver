package manifest

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// ledgerDirName is the directory, co-located with the subtree it indexes, that
// holds a shard's snapshot and log.
const ledgerDirName = ".ledger"

// snapshotFileName is a shard's compacted state.
const snapshotFileName = "snapshot.json"

// logFileName is a shard's append-only log of changes since its snapshot.
const logFileName = "log.ndjson"

// legacyManifestName is the pre-shard single-file manifest that a load migrates
// into shards.
const legacyManifestName = "manifest.json"

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
	case len(segs) >= 4 && segs[0] == "projects" && segs[2] == "workspaces":
		return path.Join(segs[0], segs[1], segs[2], segs[3])
	case len(segs) >= 4 && segs[0] == "projects" && segs[2] == "stacks":
		return path.Join(segs[0], segs[1], segs[2], segs[3])
	case segs[0] == "config-versions":
		return "config-versions"
	default:
		return ""
	}
}

// shard is one slice of the ledger — a workspace, a stack, the org-wide
// configuration versions, or the org root — persisted as a compacted snapshot
// plus an append-only log in a co-located [ledgerDirName] directory.
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
		dirtyEntries:    make(map[string]struct{}),
		dirtyWatermarks: make(map[string]struct{}),
		dirtyCompleted:  make(map[string]struct{}),
	}
}

// snapshotPath returns the path of the shard's compacted snapshot.
func (s *shard) snapshotPath() string {
	return filepath.Join(s.dir, snapshotFileName)
}

// logPath returns the path of the shard's append-only log.
func (s *shard) logPath() string {
	return filepath.Join(s.dir, logFileName)
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

	recs, err := replayLog(s.logPath())
	if err != nil {
		return err
	}

	for i := range recs {
		err = s.applyRecord(&recs[i])
		if err != nil {
			return err
		}
	}

	fi, err := os.Stat(s.logPath())
	if err == nil {
		s.logBytes = fi.Size()
	}

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
		s.runDirty
}

// drainDirty builds the log records for everything changed since the last flush
// and clears the dirty sets, so their state is carried by the returned records.
//
// It runs under the owning ledger's write lock; the entry it emits is a copy, so
// a concurrent record does not race the marshal that follows outside the lock.
func (s *shard) drainDirty() []walRecord {
	recs := make([]walRecord, 0, len(s.dirtyEntries)+len(s.dirtyWatermarks)+len(s.dirtyCompleted)+1)

	for relPath := range s.dirtyEntries {
		e := s.entries[relPath]
		if e == nil {
			continue
		}

		cp := *e
		if e.Signature != nil {
			sig := *e.Signature
			cp.Signature = &sig
		}

		recs = append(recs, walRecord{Kind: walEntry, Path: relPath, Entry: &cp})
	}

	for key := range s.dirtyWatermarks {
		recs = append(recs, walRecord{Kind: walWatermark, Key: key, At: s.watermarks[key]})
	}

	for key := range s.dirtyCompleted {
		recs = append(recs, walRecord{Kind: walCompleted, Key: key})
	}

	if s.runDirty {
		rec := walRecord{Kind: walRun, LastRunAt: s.lastRunAt, RunCount: s.runCount}

		if s.lastRun != nil {
			lr := *s.lastRun
			lr.Totals = copyStatusCounts(s.lastRun.Totals)
			rec.LastRun = &lr
		}

		recs = append(recs, rec)
	}

	clear(s.dirtyEntries)
	clear(s.dirtyWatermarks)
	clear(s.dirtyCompleted)

	s.runDirty = false

	return recs
}

// restoreDirty re-marks the state carried by recs as dirty, undoing a
// [shard.drainDirty] whose records never reached the log. It runs under the
// owning ledger's write lock, so a failed flush can put the shard back and a
// retry re-appends the same delta rather than dropping it.
func (s *shard) restoreDirty(recs []walRecord) {
	for i := range recs {
		switch recs[i].Kind {
		case walEntry:
			s.dirtyEntries[recs[i].Path] = struct{}{}
		case walWatermark:
			s.dirtyWatermarks[recs[i].Key] = struct{}{}
		case walCompleted:
			s.dirtyCompleted[recs[i].Key] = struct{}{}
		case walRun:
			s.runDirty = true
		}
	}
}

// document builds the shard's full snapshot document. It runs under the owning
// ledger's read lock.
func (s *shard) document() document {
	return document{
		Version:        schemaVersion,
		LastRunAt:      s.lastRunAt,
		LastRun:        s.lastRun,
		RunCount:       s.runCount,
		HighWaterMarks: s.watermarks,
		Entries:        s.entries,
		Completed:      s.completed,
	}
}
