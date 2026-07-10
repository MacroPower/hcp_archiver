package manifest

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"sync"
	"time"

	"go.jacobcolvin.com/hcp_archiver/atomicfile"
)

// schemaVersion is the on-disk manifest format version.
const schemaVersion = 1

// defaultCompactThreshold is the log size past which a flush folds the
// append-only log back into the snapshot. It bounds a single run's log growth
// and how much a resume replays, while staying large enough that ordinary
// flushes only append.
const defaultCompactThreshold = 64 << 20

// ErrCorruptManifest indicates that an existing manifest could not be parsed.
var ErrCorruptManifest = errors.New("manifest is corrupt")

// document is the serialized shape of the manifest on disk.
type document struct {
	LastRunAt      time.Time            `json:"lastRunAt,omitzero"`
	LastRun        *RunRecord           `json:"lastRun,omitempty"`
	HighWaterMarks map[string]time.Time `json:"highWaterMarks,omitempty"`
	Entries        map[string]*Entry    `json:"entries,omitempty"`
	Completed      map[string]bool      `json:"completedCollections,omitempty"`
	Version        int                  `json:"version"`
	RunCount       int                  `json:"runCount"`
}

// Ledger is the durable per-object record backing resume, incremental re-run,
// and progress reporting.
//
// It guards its in-memory state with a single mutex, so record methods called
// from many workspace workers are race-free and the live tally never drifts from
// the recorded entries. A second mutex serializes flushes so the append-only log
// takes one writer at a time. Create instances with [Load].
//
// # Durability
//
// The ledger persists as a compacted snapshot (the manifest file) plus an
// append-only log beside it. A [Ledger.Flush] appends only the records changed
// since the last flush, so a flush costs the recent delta rather than the whole
// ledger, and folds the log back into the snapshot once the log outgrows it (or
// when a run finishes). [Load] reads the snapshot and replays the log on top, so
// resume and a clean start are one path and a crash loses at most the last
// unflushed batch.
type Ledger struct {
	now              func() time.Time
	entries          map[string]*Entry
	watermarks       map[string]time.Time
	completed        map[string]bool
	counts           map[Status]int
	cumulative       map[Status]int
	dirtyEntries     map[string]struct{}
	dirtyWatermarks  map[string]struct{}
	dirtyCompleted   map[string]struct{}
	lastRun          *RunRecord
	runStartedAt     time.Time
	lastRunAt        time.Time
	path             string
	target           string
	bytes            int64
	compactThreshold int64
	logBytes         int64
	snapshotBytes    int64
	runCount         int
	mu               sync.RWMutex
	flushMu          sync.Mutex
	recheckAbsent    bool
	resumed          bool
	runDirty         bool
	compactNext      bool
}

// Option configures a [Ledger] passed to [Load].
//
// The available options are:
//   - [WithClock]
//   - [WithRecheckAbsent]
type Option func(*Ledger)

// WithClock injects the clock used for every recorded timestamp, defaulting to
// [time.Now]. A nil function leaves the default in place. It returns an
// [Option].
func WithClock(now func() time.Time) Option {
	return func(l *Ledger) {
		if now != nil {
			l.now = now
		}
	}
}

// WithRecheckAbsent toggles re-probing of [StatusAbsentPermanently] objects, so
// [Ledger.ShouldFetch] returns true for an object an operator suspects has been
// restored. It returns an [Option].
func WithRecheckAbsent(recheck bool) Option {
	return func(l *Ledger) {
		l.recheckAbsent = recheck
	}
}

// Load reads the manifest at path, or starts empty when the file does not
// exist.
//
// Resume and a clean first run are one code path: a first run simply loads no
// entries. A file that exists but cannot be parsed returns [ErrCorruptManifest]
// rather than being silently discarded.
func Load(path string, opts ...Option) (*Ledger, error) {
	l := &Ledger{
		now:              time.Now,
		entries:          make(map[string]*Entry),
		watermarks:       make(map[string]time.Time),
		completed:        make(map[string]bool),
		counts:           make(map[Status]int),
		cumulative:       make(map[Status]int),
		dirtyEntries:     make(map[string]struct{}),
		dirtyWatermarks:  make(map[string]struct{}),
		dirtyCompleted:   make(map[string]struct{}),
		path:             path,
		compactThreshold: defaultCompactThreshold,
	}

	for _, opt := range opts {
		opt(l)
	}

	//nolint:gosec // The manifest path is chosen by the operator by design.
	data, err := os.ReadFile(path)

	var doc document

	switch {
	case errors.Is(err, fs.ErrNotExist):
		// No snapshot yet; the document stays empty and any log is replayed on
		// top of it below, so a first flush that only appended is still resumed.
	case err != nil:
		return nil, fmt.Errorf("read manifest %q: %w", path, err)
	default:
		err = json.Unmarshal(data, &doc)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrCorruptManifest, err)
		}

		if doc.Version > schemaVersion {
			return nil, fmt.Errorf("%w: schema version %d is newer than supported %d",
				ErrCorruptManifest, doc.Version, schemaVersion)
		}
	}

	if doc.Entries != nil {
		l.entries = doc.Entries
	}

	if doc.HighWaterMarks != nil {
		l.watermarks = doc.HighWaterMarks
	}

	if doc.Completed != nil {
		l.completed = doc.Completed
	}

	l.runCount = doc.RunCount
	l.lastRunAt = doc.LastRunAt
	l.lastRun = doc.LastRun
	l.snapshotBytes = int64(len(data))

	err = l.replay()
	if err != nil {
		return nil, err
	}

	// Validate and seed the cumulative tally from the merged snapshot-plus-log
	// state, so a resumed run's counts start from the prior run's settled work
	// rather than from zero. An unrecognized status is rejected rather than
	// seeded under a key Tally never reads, which would report a zero total.
	for relPath, e := range l.entries {
		if e == nil {
			return nil, fmt.Errorf("%w: entry %q is null", ErrCorruptManifest, relPath)
		}

		if !e.Status.Valid() {
			return nil, fmt.Errorf("%w: entry %q has unknown status %q",
				ErrCorruptManifest, relPath, e.Status)
		}

		l.cumulative[e.Status]++
	}

	return l, nil
}

// replay applies the append-only log on top of the loaded snapshot, so the
// in-memory state reflects every flushed record. It runs during [Load] before
// the tally is seeded.
func (l *Ledger) replay() error {
	recs, err := replayLog(l.logPath())
	if err != nil {
		return err
	}

	for i := range recs {
		rec := &recs[i]

		switch rec.Kind {
		case walEntry:
			if rec.Entry == nil {
				return fmt.Errorf("%w: log entry %q has no record", ErrCorruptManifest, rec.Path)
			}

			l.entries[rec.Path] = rec.Entry

		case walWatermark:
			l.watermarks[rec.Key] = rec.At
		case walCompleted:
			l.completed[rec.Key] = true
		case walRun:
			l.lastRunAt = rec.LastRunAt
			l.runCount = rec.RunCount
			l.lastRun = rec.LastRun

		default:
			return fmt.Errorf("%w: unknown log record kind %q", ErrCorruptManifest, rec.Kind)
		}
	}

	fi, err := os.Stat(l.logPath())
	if err == nil {
		l.logBytes = fi.Size()
	}

	return nil
}

// ShouldFetch reports whether the current pass should fetch the object at
// relPath.
//
// An object absent from the ledger and one recorded as [StatusErrored] are
// fetched; settled statuses are not. Permanent absence is fetched only when
// recheck is enabled (see [WithRecheckAbsent]).
func (l *Ledger) ShouldFetch(relPath string) bool {
	l.mu.RLock()
	defer l.mu.RUnlock()

	e, ok := l.entries[relPath]
	if !ok {
		return true
	}

	if e.Status == StatusAbsentPermanently {
		return l.recheckAbsent
	}

	return !e.Status.Settled()
}

// Entry returns a copy of the recorded entry for relPath and whether it exists.
func (l *Ledger) Entry(relPath string) (Entry, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()

	e, ok := l.entries[relPath]
	if !ok {
		return Entry{}, false
	}

	out := *e
	if e.Signature != nil {
		sig := *e.Signature
		out.Signature = &sig
	}

	return out, true
}

// RecordDone records a successful fetch of relPath with its content signature.
func (l *Ledger) RecordDone(relPath string, sig Signature) {
	stored := sig

	l.record(relPath, StatusDone, func(now time.Time, e *Entry) {
		e.FetchedAt = now
		e.Signature = &stored
		e.LastError = ""
		e.LastErrorAt = time.Time{}
		e.Transient = false
	})
}

// RecordAbsent records relPath as permanently gone (a 404 or 410).
func (l *Ledger) RecordAbsent(relPath string) {
	l.record(relPath, StatusAbsentPermanently, func(now time.Time, e *Entry) {
		e.FetchedAt = now
		e.LastError = ""
		e.LastErrorAt = time.Time{}
		e.Transient = false
	})
}

// RecordErrored records a failed fetch of relPath, noting whether the cause was
// transient rather than terminal so a rate-limit blip is not mistaken for a
// permanent absence.
func (l *Ledger) RecordErrored(relPath string, cause error, transient bool) {
	msg := ""
	if cause != nil {
		msg = cause.Error()
	}

	l.record(relPath, StatusErrored, func(now time.Time, e *Entry) {
		e.LastError = msg
		e.LastErrorAt = now
		e.Transient = transient
	})
}

// RecordForbidden records relPath as inaccessible to the archiving identity (an
// HTTP 403). It keeps the cause text so the entry documents the permission gap,
// and unlike [Ledger.RecordAbsent] it is not settled, so a re-run under a
// token with broader access retries it.
func (l *Ledger) RecordForbidden(relPath string, cause error) {
	msg := ""
	if cause != nil {
		msg = cause.Error()
	}

	l.record(relPath, StatusForbidden, func(now time.Time, e *Entry) {
		e.LastError = msg
		e.LastErrorAt = now
		e.Transient = false
	})
}

// RecordSkipped records relPath as intentionally deferred, a settled state that
// is not mistaken for a gap.
func (l *Ledger) RecordSkipped(relPath string) {
	l.record(relPath, StatusSkipped, func(_ time.Time, e *Entry) {
		e.LastError = ""
		e.LastErrorAt = time.Time{}
		e.Transient = false
	})
}

// RecordNotApplicable records relPath as not applicable to this archive, a
// settled state that is not mistaken for a gap.
func (l *Ledger) RecordNotApplicable(relPath string) {
	l.record(relPath, StatusNotApplicable, func(_ time.Time, e *Entry) {
		e.LastError = ""
		e.LastErrorAt = time.Time{}
		e.Transient = false
	})
}

// record applies status and mutate to the entry at relPath and keeps both
// tallies in step: the per-run counts swap any status this entry already
// contributed to the run, and the cumulative counts swap the entry's prior
// settled status so they always reflect every entry's current state.
func (l *Ledger) record(relPath string, status Status, mutate func(now time.Time, e *Entry)) {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()

	e, ok := l.entries[relPath]
	if !ok {
		e = &Entry{FirstSeen: now}
		l.entries[relPath] = e
	}

	if e.counted {
		l.counts[e.Status]--
	}

	// A pre-existing entry already contributes its old status to the cumulative
	// tally; move it to the new status. A brand-new entry has none to move.
	if ok {
		l.cumulative[e.Status]--
	}

	e.Status = status
	e.Attempts++

	mutate(now, e)

	e.counted = true
	l.counts[status]++
	l.cumulative[status]++

	l.dirtyEntries[relPath] = struct{}{}
}

// HighWaterMark returns the recorded watermark for key, or the zero time when
// none is set.
//
// The same keyed store holds both the newest CreatedAt archived for an
// append-mostly collection and the audit trail's Since cursor.
func (l *Ledger) HighWaterMark(key string) time.Time {
	l.mu.RLock()
	defer l.mu.RUnlock()

	return l.watermarks[key]
}

// AdvanceHighWaterMark advances the watermark for key toward t, keeping the
// later of the two so the mark only ever moves forward.
func (l *Ledger) AdvanceHighWaterMark(key string, t time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if t.After(l.watermarks[key]) {
		l.watermarks[key] = t
		l.dirtyWatermarks[key] = struct{}{}
	}
}

// IsCollectionComplete reports whether the append-mostly collection under key
// was walked to its end in a prior run.
//
// It is false until the collection has been fully paged at least once, so an
// interrupted first walk (which leaves a newest-first prefix archived and an
// older tail missing) is not mistaken for a complete collection. A re-run may
// stop early at the newest already-archived element only once this is true.
func (l *Ledger) IsCollectionComplete(key string) bool {
	l.mu.RLock()
	defer l.mu.RUnlock()

	return l.completed[key]
}

// MarkCollectionComplete records that the collection under key has been walked
// to its end, so later re-runs may stop early once they reach already-archived
// history.
//
// It is sticky: an append-mostly collection only grows at its newest end, so
// once its tail is fully archived that tail stays archived.
func (l *Ledger) MarkCollectionComplete(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.completed[key] = true
	l.dirtyCompleted[key] = struct{}{}
}

// AddBytes adds n to the run's downloaded-bytes counter.
func (l *Ledger) AddBytes(n int64) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.bytes += n
}

// SetTarget sets the current org, project, or workspace shown by progress.
func (l *Ledger) SetTarget(target string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.target = target
}

// Tally returns a snapshot of the live counters.
//
// The per-status counts are cumulative across runs (every entry counted by its
// current status), so a resumed run reflects the prior run's settled work;
// BytesDownloaded stays per-run.
func (l *Ledger) Tally() Tally {
	l.mu.RLock()
	defer l.mu.RUnlock()

	return Tally{
		Target:            l.target,
		Done:              l.cumulative[StatusDone],
		AbsentPermanently: l.cumulative[StatusAbsentPermanently],
		Skipped:           l.cumulative[StatusSkipped],
		Errored:           l.cumulative[StatusErrored],
		Forbidden:         l.cumulative[StatusForbidden],
		NotApplicable:     l.cumulative[StatusNotApplicable],
		BytesDownloaded:   l.bytes,
		Resumed:           l.resumed,
	}
}

// StartRun opens a new run: it advances the run count and start time and resets
// the per-run tally so counts and bytes reflect only the new run. It records
// whether the run resumed prior work, and leaves the cumulative tally intact so
// a resumed run's reported counts carry the prior run's settled objects.
func (l *Ledger) StartRun() {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	l.runCount++
	l.lastRunAt = now
	l.runStartedAt = now
	l.resumed = len(l.entries) > 0
	l.counts = make(map[Status]int)
	l.bytes = 0
	l.target = ""
	l.runDirty = true

	for _, e := range l.entries {
		e.counted = false
	}
}

// FinishRun closes the current run, writing its summary into the ledger and
// returning a copy for the final progress line.
func (l *Ledger) FinishRun() RunRecord {
	l.mu.Lock()
	defer l.mu.Unlock()

	totals := make(map[Status]int, len(l.counts))

	for s, n := range l.counts {
		if n != 0 {
			totals[s] = n
		}
	}

	l.lastRun = &RunRecord{
		StartedAt:       l.runStartedAt,
		FinishedAt:      l.now(),
		Totals:          totals,
		BytesDownloaded: l.bytes,
	}

	// A finished run's summary is worth folding the log into the snapshot for, so
	// a completed archive leaves a current manifest and an empty log.
	l.runDirty = true
	l.compactNext = true

	out := *l.lastRun
	out.Totals = copyStatusCounts(totals)

	return out
}

// RunCount returns the number of runs recorded, including one opened by
// [Ledger.StartRun] but not yet finished.
func (l *Ledger) RunCount() int {
	l.mu.RLock()
	defer l.mu.RUnlock()

	return l.runCount
}

// LastRunAt returns the start time of the most recent run, or the zero time
// when none has started.
func (l *Ledger) LastRunAt() time.Time {
	l.mu.RLock()
	defer l.mu.RUnlock()

	return l.lastRunAt
}

// LastRun returns a copy of the most recent run summary and whether one exists.
func (l *Ledger) LastRun() (RunRecord, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()

	if l.lastRun == nil {
		return RunRecord{}, false
	}

	out := *l.lastRun
	out.Totals = copyStatusCounts(l.lastRun.Totals)

	return out, true
}

// Flush persists the state recorded since the last flush durably, so a hard kill
// loses at most that batch.
//
// It appends the changed entries, watermarks, completion flags, and run record
// to the append-only log, then folds the log back into the snapshot when the log
// has outgrown it or a run has just finished. An append-only flush costs the
// recent delta rather than the whole ledger. Flushes are serialized, so the log
// takes one writer at a time.
func (l *Ledger) Flush() error {
	l.flushMu.Lock()
	defer l.flushMu.Unlock()

	l.mu.Lock()

	recs := l.drainDirtyLocked()
	compactNow := l.compactNext
	l.mu.Unlock()

	if len(recs) > 0 {
		n, err := appendLog(l.logPath(), recs)
		if err != nil {
			return fmt.Errorf("append manifest log: %w", err)
		}

		l.mu.Lock()

		l.logBytes += n
		l.mu.Unlock()
	}

	l.mu.RLock()

	needCompact := compactNow || (l.logBytes > l.compactThreshold && l.logBytes > l.snapshotBytes)
	l.mu.RUnlock()

	if !needCompact {
		return nil
	}

	return l.compact()
}

// drainDirtyLocked builds the log records for everything changed since the last
// flush and clears the dirty sets, so their state is carried by the returned
// records. It runs under the write lock; the entry it emits is a copy, so a
// concurrent record does not race the marshal that follows outside the lock.
func (l *Ledger) drainDirtyLocked() []walRecord {
	recs := make([]walRecord, 0, len(l.dirtyEntries)+len(l.dirtyWatermarks)+len(l.dirtyCompleted)+1)

	for relPath := range l.dirtyEntries {
		e := l.entries[relPath]
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

	for key := range l.dirtyWatermarks {
		recs = append(recs, walRecord{Kind: walWatermark, Key: key, At: l.watermarks[key]})
	}

	for key := range l.dirtyCompleted {
		recs = append(recs, walRecord{Kind: walCompleted, Key: key})
	}

	if l.runDirty {
		rec := walRecord{Kind: walRun, LastRunAt: l.lastRunAt, RunCount: l.runCount}

		if l.lastRun != nil {
			lr := *l.lastRun
			lr.Totals = copyStatusCounts(l.lastRun.Totals)
			rec.LastRun = &lr
		}

		recs = append(recs, rec)
	}

	clear(l.dirtyEntries)
	clear(l.dirtyWatermarks)
	clear(l.dirtyCompleted)

	l.runDirty = false

	return recs
}

// compact folds the append-only log back into the snapshot: it writes the full
// document to the snapshot path via an atomic temp-and-rename, then removes the
// log, so the snapshot alone reflects every recorded object and the log starts
// empty again.
//
// The snapshot is written before the log is removed, so a crash between the two
// leaves the log's records applied on top of a complete snapshot rather than
// losing them; because the in-memory entries are only ever added to, the
// snapshot is a superset of the log it replaces. The marshal holds a read lock,
// briefly blocking recording workers.
func (l *Ledger) compact() error {
	l.mu.RLock()

	doc := document{
		Version:        schemaVersion,
		LastRunAt:      l.lastRunAt,
		LastRun:        l.lastRun,
		RunCount:       l.runCount,
		HighWaterMarks: l.watermarks,
		Entries:        l.entries,
		Completed:      l.completed,
	}

	data, err := json.MarshalIndent(doc, "", "  ")

	l.mu.RUnlock()

	if err != nil {
		return fmt.Errorf("marshal manifest: %w", err)
	}

	err = atomicfile.WriteFile(l.path, data)
	if err != nil {
		return fmt.Errorf("write manifest: %w", err)
	}

	err = os.Remove(l.logPath())
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("truncate manifest log: %w", err)
	}

	l.mu.Lock()

	l.snapshotBytes = int64(len(data))
	l.logBytes = 0
	l.compactNext = false
	l.mu.Unlock()

	return nil
}

// logPath returns the path of the append-only log beside the snapshot.
func (l *Ledger) logPath() string {
	return l.path + ".log"
}

// copyStatusCounts returns a shallow copy of a per-status count map, or nil.
func copyStatusCounts(src map[Status]int) map[Status]int {
	if src == nil {
		return nil
	}

	dst := make(map[Status]int, len(src))
	maps.Copy(dst, src)

	return dst
}
