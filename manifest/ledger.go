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
// It guards every field with a single mutex, so record methods called from many
// workspace workers are race-free and the live tally never drifts from the
// recorded entries. Create instances with [Load].
type Ledger struct {
	now           func() time.Time
	entries       map[string]*Entry
	watermarks    map[string]time.Time
	completed     map[string]bool
	counts        map[Status]int
	cumulative    map[Status]int
	lastRun       *RunRecord
	runStartedAt  time.Time
	lastRunAt     time.Time
	path          string
	target        string
	bytes         int64
	runCount      int
	mu            sync.RWMutex
	recheckAbsent bool
	resumed       bool
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
		now:        time.Now,
		entries:    make(map[string]*Entry),
		watermarks: make(map[string]time.Time),
		completed:  make(map[string]bool),
		counts:     make(map[Status]int),
		cumulative: make(map[Status]int),
		path:       path,
	}

	for _, opt := range opts {
		opt(l)
	}

	//nolint:gosec // The manifest path is chosen by the operator by design.
	data, err := os.ReadFile(path)

	switch {
	case errors.Is(err, fs.ErrNotExist):
		return l, nil
	case err != nil:
		return nil, fmt.Errorf("read manifest %q: %w", path, err)
	}

	var doc document

	err = json.Unmarshal(data, &doc)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrCorruptManifest, err)
	}

	if doc.Version > schemaVersion {
		return nil, fmt.Errorf("%w: schema version %d is newer than supported %d",
			ErrCorruptManifest, doc.Version, schemaVersion)
	}

	if doc.Entries != nil {
		for relPath, e := range doc.Entries {
			if e == nil {
				return nil, fmt.Errorf("%w: entry %q is null", ErrCorruptManifest, relPath)
			}

			// Reject an unrecognized status rather than seed the cumulative
			// tally under a key Tally never reads, which would report a resumed
			// run with a zero total.
			if !e.Status.Valid() {
				return nil, fmt.Errorf("%w: entry %q has unknown status %q",
					ErrCorruptManifest, relPath, e.Status)
			}
		}

		l.entries = doc.Entries

		// Seed the cumulative tally from the loaded entries so a resumed run's
		// counts start from the prior run's settled work rather than from zero.
		for _, e := range l.entries {
			l.cumulative[e.Status]++
		}
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

	return l, nil
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

// Flush writes the manifest durably via an atomic temp-and-rename, so a hard
// kill loses at most the entries recorded since the last flush.
func (l *Ledger) Flush() error {
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

	return nil
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
