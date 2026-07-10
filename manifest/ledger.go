package manifest

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"go.jacobcolvin.com/hcp_archiver/atomicfile"
)

// schemaVersion is the on-disk manifest format version.
const schemaVersion = 1

// defaultCompactThreshold is the per-shard log size past which a flush folds the
// log back into that shard's snapshot. It bounds a single run's log growth and
// how much a resume replays, while staying large enough that ordinary flushes
// only append.
const defaultCompactThreshold = 64 << 20

// ErrCorruptManifest indicates that an existing manifest could not be parsed.
var ErrCorruptManifest = errors.New("manifest is corrupt")

// document is the serialized shape of one shard's snapshot on disk. The run-level
// fields are populated only for the org-root shard; other shards leave them zero.
type document struct {
	LastRunAt      time.Time            `json:"lastRunAt,omitzero"`
	LastRun        *RunRecord           `json:"lastRun,omitempty"`
	HighWaterMarks map[string]time.Time `json:"highWaterMarks,omitempty"`
	Entries        map[string]*Entry    `json:"entries,omitempty"`
	Completed      map[string]bool      `json:"completedCollections,omitempty"`
	Settled        map[string]bool      `json:"settledCollections,omitempty"`
	Version        int                  `json:"version"`
	RunCount       int                  `json:"runCount"`
}

// Ledger is the durable per-object record backing resume, incremental re-run,
// and progress reporting.
//
// It guards its in-memory state with a single mutex, so record methods called
// from many workspace workers are race-free and the live tally never drifts from
// the recorded entries. A second mutex serializes flushes so a shard's log takes
// one writer at a time. Create instances with [Load].
//
// # Sharded storage
//
// The ledger partitions its state into per-workspace, per-stack, per-config-
// version, and org-root shards (see [shardKey]), each persisted as a compacted
// snapshot plus an append-only log in a co-located .ledger directory. Every entry
// is keyed by its archive-relative path exactly as before; the path only
// determines which shard the entry lives in, so resume, the newest-first
// early-stop, and the watermarks are unchanged. A flush appends only the records
// changed since the last flush to the shards that changed, and folds a shard's
// log back into its snapshot once the log outgrows it or a run finishes, so a
// re-run's cost tracks the workspaces it touched rather than the whole archive.
// The per-status tally stays global to the ledger, seeded on load from every
// shard's entries.
type Ledger struct {
	now              func() time.Time
	shards           map[string]*shard
	rootShard        *shard
	counts           map[Status]int
	cumulative       map[Status]int
	runStartedAt     time.Time
	root             string
	target           string
	bytes            int64
	compactThreshold int64
	mu               sync.RWMutex
	flushMu          sync.Mutex
	recheckAbsent    bool
	resumed          bool
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

// Load reads the sharded manifest under root, or starts empty when none exists.
//
// It discovers and loads every shard beneath root. Resume and a clean first run
// are one code path: a first run simply finds no shards. A shard whose snapshot
// or log cannot be parsed returns [ErrCorruptManifest].
func Load(root string, opts ...Option) (*Ledger, error) {
	l := &Ledger{
		now:              time.Now,
		shards:           make(map[string]*shard),
		counts:           make(map[Status]int),
		cumulative:       make(map[Status]int),
		root:             root,
		compactThreshold: defaultCompactThreshold,
	}

	for _, opt := range opts {
		opt(l)
	}

	l.rootShard = newShard(l.shardDir(""))
	l.shards[""] = l.rootShard

	dirs, err := discoverShards(root)
	if err != nil {
		return nil, err
	}

	for sk, dir := range dirs {
		sh, ok := l.shards[sk]
		if !ok {
			sh = newShard(dir)
			l.shards[sk] = sh
		}

		err = sh.load()
		if err != nil {
			return nil, err
		}
	}

	// Validate and seed the cumulative tally from every shard's entries, so a
	// resumed run's counts start from the prior run's settled work rather than
	// from zero. An unrecognized status is rejected rather than seeded under a
	// key Tally never reads, which would report a zero total.
	for _, sh := range l.shards {
		for relPath, e := range sh.entries {
			if e == nil {
				return nil, fmt.Errorf("%w: entry %q is null", ErrCorruptManifest, relPath)
			}

			if !e.Status.Valid() {
				return nil, fmt.Errorf("%w: entry %q has unknown status %q",
					ErrCorruptManifest, relPath, e.Status)
			}

			l.cumulative[e.Status]++
		}
	}

	return l, nil
}

// shardDir returns the absolute .ledger directory for a shard key, co-located
// with the subtree it indexes; the empty key is the org root.
func (l *Ledger) shardDir(sk string) string {
	if sk == "" {
		return filepath.Join(l.root, ledgerDirName)
	}

	return filepath.Join(l.root, filepath.FromSlash(sk), ledgerDirName)
}

// discoverShards finds the .ledger directories beneath root and maps each to its
// shard key. It globs the known structural depths rather than walking the whole
// tree, so discovery costs the number of workspaces and stacks, not the number
// of archived objects.
func discoverShards(root string) (map[string]string, error) {
	patterns := []string{
		filepath.Join(root, ledgerDirName),
		filepath.Join(root, "config-versions", ledgerDirName),
		filepath.Join(root, "projects", "*", "workspaces", "*", ledgerDirName),
		filepath.Join(root, "projects", "*", "stacks", "*", ledgerDirName),
	}

	out := make(map[string]string)

	for _, pattern := range patterns {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			return nil, fmt.Errorf("glob shards %q: %w", pattern, err)
		}

		for _, dir := range matches {
			info, statErr := os.Stat(dir)
			if statErr != nil || !info.IsDir() {
				continue
			}

			rel, relErr := filepath.Rel(root, filepath.Dir(dir))
			if relErr != nil {
				continue
			}

			sk := filepath.ToSlash(rel)
			if sk == "." {
				sk = ""
			}

			out[sk] = dir
		}
	}

	return out, nil
}

// shardFor returns the shard that owns key, creating it if absent. Callers that
// mutate must hold the write lock; a first record into a new workspace creates
// its shard here.
func (l *Ledger) shardFor(key string) *shard {
	sk := shardKey(key)

	sh, ok := l.shards[sk]
	if !ok {
		sh = newShard(l.shardDir(sk))
		l.shards[sk] = sh
	}

	return sh
}

// lookupShard returns the shard that owns key when it already exists, without
// creating one. Read paths use it so a lookup never materializes a shard.
func (l *Ledger) lookupShard(key string) (*shard, bool) {
	sh, ok := l.shards[shardKey(key)]

	return sh, ok
}

// totalEntries returns the number of entries across every shard.
func (l *Ledger) totalEntries() int {
	n := 0
	for _, sh := range l.shards {
		n += len(sh.entries)
	}

	return n
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

	sh, ok := l.lookupShard(relPath)
	if !ok {
		return true
	}

	e, ok := sh.entries[relPath]
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

	sh, ok := l.lookupShard(relPath)
	if !ok {
		return Entry{}, false
	}

	e, ok := sh.entries[relPath]
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

// record applies status and mutate to the entry at relPath in its shard and
// keeps both tallies in step: the per-run counts swap any status this entry
// already contributed to the run, and the cumulative counts swap the entry's
// prior settled status so they always reflect every entry's current state.
func (l *Ledger) record(relPath string, status Status, mutate func(now time.Time, e *Entry)) {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	sh := l.shardFor(relPath)

	e, ok := sh.entries[relPath]
	if !ok {
		e = &Entry{FirstSeen: now}
		sh.entries[relPath] = e
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

	sh.dirtyEntries[relPath] = struct{}{}
}

// HighWaterMark returns the recorded watermark for key, or the zero time when
// none is set.
//
// The same keyed store holds both the newest CreatedAt archived for an
// append-mostly collection and the audit trail's Since cursor.
func (l *Ledger) HighWaterMark(key string) time.Time {
	l.mu.RLock()
	defer l.mu.RUnlock()

	sh, ok := l.lookupShard(key)
	if !ok {
		return time.Time{}
	}

	return sh.watermarks[key]
}

// AdvanceHighWaterMark advances the watermark for key toward t, keeping the
// later of the two so the mark only ever moves forward.
func (l *Ledger) AdvanceHighWaterMark(key string, t time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()

	sh := l.shardFor(key)

	if t.After(sh.watermarks[key]) {
		sh.watermarks[key] = t
		sh.dirtyWatermarks[key] = struct{}{}
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

	sh, ok := l.lookupShard(key)
	if !ok {
		return false
	}

	return sh.completed[key]
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

	sh := l.shardFor(key)
	sh.completed[key] = true
	sh.dirtyCompleted[key] = struct{}{}
}

// IsCollectionSettled reports whether the newest full walk of the collection
// under key settled it: saw only terminal elements and left no unsettled child.
//
// Unlike completion it is not sticky. It is false until a full walk records it,
// and a later walk that reaches a still-running element flips it back to false
// (see [Ledger.SetCollectionSettled]), so a collection with work still in flight
// re-pages in full rather than stopping at a done boundary that a later,
// out-of-order element sits behind.
func (l *Ledger) IsCollectionSettled(key string) bool {
	l.mu.RLock()
	defer l.mu.RUnlock()

	sh, ok := l.lookupShard(key)
	if !ok {
		return false
	}

	return sh.settled[key]
}

// SetCollectionSettled records whether the newest full walk of the collection
// under key settled it, so a later re-run may stop early only while it stays
// true.
//
// It is deliberately non-monotonic: passing false undoes an earlier true, which
// is how a walk that reaches a still-running element forces the next run to
// re-page the collection until that element settles.
func (l *Ledger) SetCollectionSettled(key string, settled bool) {
	l.mu.Lock()
	defer l.mu.Unlock()

	sh := l.shardFor(key)
	sh.settled[key] = settled
	sh.dirtySettled[key] = struct{}{}
}

// HasUnsettledUnder reports whether any entry beneath prefix (a key beginning
// with prefix + "/") is recorded in a status a normal re-run still retries.
//
// It scans the shard that owns prefix, so a collection's walk can tell whether
// an errored or forbidden child left below its newest done boundary still needs
// reaching. A prefix whose shard holds no such child returns false, which
// includes a synthetic cursor key that is not a genuine path prefix of any
// entry (a stack walk's id cursor).
func (l *Ledger) HasUnsettledUnder(prefix string) bool {
	l.mu.RLock()
	defer l.mu.RUnlock()

	sh, ok := l.lookupShard(prefix)
	if !ok {
		return false
	}

	under := prefix + "/"

	for relPath, e := range sh.entries {
		if strings.HasPrefix(relPath, under) && !e.Status.Settled() {
			return true
		}
	}

	return false
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

// Failures returns every object currently recorded as [StatusErrored] or
// [StatusForbidden], errored entries first and each class sorted by path, so a
// run summary can name what failed rather than only count it.
func (l *Ledger) Failures() []Failure {
	l.mu.RLock()
	defer l.mu.RUnlock()

	var out []Failure

	for _, sh := range l.shards {
		for relPath, e := range sh.entries {
			if e.Status != StatusErrored && e.Status != StatusForbidden {
				continue
			}

			out = append(out, Failure{
				RelPath: relPath,
				Error:   e.LastError,
				Status:  e.Status,
			})
		}
	}

	slices.SortFunc(out, func(a, b Failure) int {
		if a.Status != b.Status {
			if a.Status == StatusErrored {
				return -1
			}

			return 1
		}

		return strings.Compare(a.RelPath, b.RelPath)
	})

	return out
}

// StartRun opens a new run: it advances the run count and start time and resets
// the per-run tally so counts and bytes reflect only the new run. It records
// whether the run resumed prior work, and leaves the cumulative tally intact so
// a resumed run's reported counts carry the prior run's settled objects.
func (l *Ledger) StartRun() {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	l.rootShard.runCount++
	l.rootShard.lastRunAt = now
	l.rootShard.runDirty = true
	l.runStartedAt = now
	l.resumed = l.totalEntries() > 0
	l.counts = make(map[Status]int)
	l.bytes = 0
	l.target = ""

	for _, sh := range l.shards {
		for _, e := range sh.entries {
			e.counted = false
		}
	}
}

// FinishRun closes the current run, writing its summary into the org-root shard
// and returning a copy for the final progress line. It marks the run for
// compaction, so a finished run leaves each touched shard with a current
// snapshot and no leftover log.
func (l *Ledger) FinishRun() RunRecord {
	l.mu.Lock()
	defer l.mu.Unlock()

	totals := make(map[Status]int, len(l.counts))

	for s, n := range l.counts {
		if n != 0 {
			totals[s] = n
		}
	}

	l.rootShard.lastRun = &RunRecord{
		StartedAt:       l.runStartedAt,
		FinishedAt:      l.now(),
		Totals:          totals,
		BytesDownloaded: l.bytes,
	}
	l.rootShard.runDirty = true
	l.compactNext = true

	out := *l.rootShard.lastRun
	out.Totals = copyStatusCounts(totals)

	return out
}

// RunCount returns the number of runs recorded, including one opened by
// [Ledger.StartRun] but not yet finished.
func (l *Ledger) RunCount() int {
	l.mu.RLock()
	defer l.mu.RUnlock()

	return l.rootShard.runCount
}

// LastRunAt returns the start time of the most recent run, or the zero time
// when none has started.
func (l *Ledger) LastRunAt() time.Time {
	l.mu.RLock()
	defer l.mu.RUnlock()

	return l.rootShard.lastRunAt
}

// LastRun returns a copy of the most recent run summary and whether one exists.
func (l *Ledger) LastRun() (RunRecord, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()

	if l.rootShard.lastRun == nil {
		return RunRecord{}, false
	}

	out := *l.rootShard.lastRun
	out.Totals = copyStatusCounts(l.rootShard.lastRun.Totals)

	return out, true
}

// Flush persists the state recorded since the last flush durably, so a hard kill
// loses at most that batch.
//
// It appends each changed shard's delta to that shard's append-only log, then
// folds a log back into its snapshot when the log has outgrown it or a run has
// just finished. An append-only flush costs the recent delta rather than the
// whole ledger, and only the shards that changed are touched. Flushes are
// serialized, so each shard's log takes one writer at a time.
func (l *Ledger) Flush() error {
	l.flushMu.Lock()
	defer l.flushMu.Unlock()

	l.mu.Lock()

	type pending struct {
		sh   *shard
		recs []walRecord
	}

	var work []pending

	for _, sh := range l.shards {
		if sh.hasDirty() {
			work = append(work, pending{sh: sh, recs: sh.drainDirty()})
		}
	}

	compactAll := l.compactNext
	l.compactNext = false

	l.mu.Unlock()

	for i, w := range work {
		n, err := appendLog(w.sh.logPath(), w.recs)
		if err != nil {
			// The drained records never reached the log; put them and every
			// not-yet-appended shard's records back so a retry re-appends them
			// rather than dropping the delta. Restore a pending compaction request
			// too, so a finished run still folds its shards' logs into their
			// snapshots on the retry rather than leaving them uncompacted.
			l.mu.Lock()

			for _, rem := range work[i:] {
				rem.sh.restoreDirty(rem.recs)
			}

			if compactAll {
				l.compactNext = true
			}

			l.mu.Unlock()

			return fmt.Errorf("append shard log %q: %w", w.sh.dir, err)
		}

		l.mu.Lock()

		w.sh.logBytes += n
		l.mu.Unlock()
	}

	l.mu.RLock()

	var toCompact []*shard

	for _, sh := range l.shards {
		outgrown := sh.logBytes > l.compactThreshold && sh.logBytes > sh.snapshotBytes
		if (compactAll && sh.logBytes > 0) || outgrown {
			toCompact = append(toCompact, sh)
		}
	}

	l.mu.RUnlock()

	for _, sh := range toCompact {
		err := l.compactShard(sh)
		if err != nil {
			return err
		}
	}

	return nil
}

// compactShard folds one shard's append-only log back into its snapshot: it
// writes the shard's full document via an atomic temp-and-rename, then removes
// the log, so the snapshot alone reflects the shard and its log starts empty.
//
// It compacts only a shard with no pending dirty state, so the snapshot it
// writes reflects exactly what the log already holds. The snapshot is written
// before the log is removed, so a crash between the two replays the log's
// records onto a snapshot that already accounts for them rather than losing or
// regressing an entry. A shard re-dirtied since the last drain (a worker
// recorded during the flush) is skipped and folded on a later flush, once its
// newest state has reached the log; without that guard the snapshot could
// capture an in-memory status the log does not yet carry, and a crash before the
// log removal would replay the stale log record over the newer snapshot. The
// marshal holds a read lock, briefly blocking recording workers, but spans one
// shard rather than the whole ledger.
func (l *Ledger) compactShard(sh *shard) error {
	l.mu.RLock()

	if sh.hasDirty() {
		l.mu.RUnlock()

		return nil
	}

	doc := sh.document()
	data, err := json.MarshalIndent(doc, "", "  ")

	l.mu.RUnlock()

	if err != nil {
		return fmt.Errorf("marshal shard %q: %w", sh.dir, err)
	}

	err = atomicfile.WriteFile(sh.snapshotPath(), data)
	if err != nil {
		return fmt.Errorf("write shard %q: %w", sh.dir, err)
	}

	err = os.Remove(sh.logPath())
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("truncate shard log %q: %w", sh.dir, err)
	}

	l.mu.Lock()

	sh.snapshotBytes = int64(len(data))
	sh.logBytes = 0
	l.mu.Unlock()

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
