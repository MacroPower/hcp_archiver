package restore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"

	"golang.org/x/sync/errgroup"

	"go.jacobcolvin.com/hcp_archiver/pkg/atomicfile"
	"go.jacobcolvin.com/hcp_archiver/pkg/collect"
	"go.jacobcolvin.com/hcp_archiver/pkg/pathkit"
	"go.jacobcolvin.com/hcp_archiver/pkg/remote"
	"go.jacobcolvin.com/hcp_archiver/pkg/serialize"
	"go.jacobcolvin.com/hcp_archiver/pkg/store"
)

// progressInterval is how many settled entries pass between pull_progress
// events, matching the sweep's own cadence.
const progressInterval = 1000

// errStubConflict reports a valid local stub contradicting the mirrored
// object it records; the conflict emits its own event, so the stub tally
// counts it without a second generic warning.
var errStubConflict = errors.New("the recorded stub contradicts the mirrored object")

// Restorer restores one organization's warm layer from its mirror. Create
// instances with [NewRestorer].
type Restorer struct {
	client      *remote.Client
	logger      *slog.Logger
	progress    func(relPath string, bytes int64, err error)
	sink        ProgressSink
	cfg         remote.Config
	concurrency int
}

// Option configures a [Restorer] passed to [NewRestorer].
//
// The available options are:
//   - [WithLogger]
//   - [WithConcurrency]
//   - [WithProgress]
//   - [WithProgressSink]
type Option func(*Restorer)

// WithLogger is an [Option] that sets the logger the restorer emits its
// structured events through; the default discards them.
func WithLogger(logger *slog.Logger) Option {
	return func(r *Restorer) {
		if logger != nil {
			r.logger = logger
		}
	}
}

// WithConcurrency is an [Option] that bounds how many objects classify and
// transfer at once; the default matches the sweep's own bound.
func WithConcurrency(n int) Option {
	return func(r *Restorer) {
		if n > 0 {
			r.concurrency = n
		}
	}
}

// WithProgress is an [Option] that sets a per-file callback: each restored
// file reports its path and byte count with a nil error, and each failure
// reports its path and error. Calls may arrive from concurrent transfers.
func WithProgress(fn func(relPath string, bytes int64, err error)) Option {
	return func(r *Restorer) {
		if fn != nil {
			r.progress = fn
		}
	}
}

// NewRestorer creates a new [Restorer] over the mirror client and the
// configuration whose URL and prefix locate the archive in it.
func NewRestorer(client *remote.Client, cfg remote.Config, opts ...Option) *Restorer {
	r := &Restorer{
		client:      client,
		cfg:         cfg,
		logger:      slog.New(slog.DiscardHandler),
		progress:    func(string, int64, error) {},
		sink:        nopSink{},
		concurrency: collect.DefaultConcurrency,
	}

	for _, opt := range opts {
		opt(r)
	}

	return r
}

// Failure names one path that could not be restored and why.
type Failure struct {
	Err  error
	Path string
}

// Summary is one executed restore's tally.
type Summary struct {
	// Failures names each path that failed, in the order the failures landed.
	Failures []Failure
	// Leftovers names the mirrored keys nothing in the restored tree
	// accounts for; while any stand, a marker not already complete settles
	// partial.
	Leftovers []string
	// Restored counts the files written and verified.
	Restored int
	// Skipped counts the files already verified identical locally.
	Skipped int
	// Failed counts the files that could not be restored.
	Failed int
	// Refused counts the conflicts the restore declined to touch.
	Refused int
	// Stubs counts the eviction stubs written or verified against the
	// mirror.
	Stubs int
	// StubsFailed counts the stubs that could not be ensured; they hold a
	// marker not already complete at partial but never fail the restore.
	StubsFailed int
	// Bytes totals the restored files' sizes.
	Bytes int64
	// Complete reports that the settled marker records the tree as
	// complete, whether this run proved the accounting or an earlier one
	// did.
	Complete bool
}

// incomplete reports whether the restored set is not fully on disk: any
// failure or refusal leaves the restore unfinished and the restoring marker
// standing.
func (s Summary) incomplete() bool {
	return s.Failed > 0 || s.Refused > 0
}

// Pull executes a plan against the local tree at orgRoot. The caller holds
// the archive lock and has already settled the marker preconditions (a
// recorded mirror that conflicts with the configured one refuses before any
// plan exists).
//
// The order is the safety property: the restoring marker lands durably
// before the first byte (while it stands, no run prunes the mirror and no
// archiver opens the tree), every data file lands and verifies before any
// ledger snapshot does (so the tree never holds ledger entries describing
// absent files), the eviction stubs for the mirror's tarballs land after
// both, and the marker settles last. It settles complete when the run
// accounts for every mirrored key (each restorable object restored or
// verified, each evicted tarball's stub ensured, each bundle zip's sidecar
// present, no foreign keys), else partial with the unaccounted keys named;
// a marker already recording complete is never demoted, though a run
// interrupted after it stamped the restoring marker settles by proof alone
// and may land partial, which only re-enables the mirror fallback. The
// stub repair under a complete marker is best-effort: a failure is counted
// and retried by the next run. A plan that changes nothing writes nothing,
// except that a leftover restoring or partial marker is still settled,
// which is also what promotes a restored tree whose stubs were never
// backfilled.
//
// A per-file failure never aborts the run; it is counted, named in the
// summary and the log, and leaves the marker standing for a re-run to
// finish. Only an infrastructural failure (the marker itself not landing)
// returns an error.
func (r *Restorer) Pull(ctx context.Context, orgRoot, org string, plan Plan) (Summary, error) {
	sum := Summary{Refused: len(plan.Refusals)}

	for _, e := range plan.Refusals {
		r.logger.LogAttrs(ctx, slog.LevelError, "pull_refused",
			slog.String("org", org),
			slog.String("path", e.Rel),
			slog.String("reason", e.Reason),
		)

		sum.Failures = append(sum.Failures, Failure{Path: e.Rel, Err: fmt.Errorf("refused: %s", e.Reason)})
	}

	sum.Skipped = plan.Skipped

	// The pre-run marker is read before the restoring stamp overwrites it:
	// it is the never-demote evidence, and the only record of it.
	pre, hadPre, err := remote.ReadMarker(orgRoot)
	if err != nil {
		return sum, err //nolint:wrapcheck // The marker reader names the file and the fault.
	}

	if plan.RestoreFiles == 0 {
		return r.settleOnly(ctx, orgRoot, org, plan, sum, pre, hadPre)
	}

	err = r.writeMarker(orgRoot, r.cfg.RestoringMarker())
	if err != nil {
		return sum, fmt.Errorf("record restoring marker: %w", err)
	}

	data, snapshots := splitEntries(plan.Entries)

	r.sink.SetPhase(PhaseRestore)
	r.sink.SetTotal(len(data) + len(snapshots))

	r.restoreAll(ctx, orgRoot, org, data, &sum)

	// The barrier: snapshots land only over a fully proven data layer. A
	// data-phase failure (or an interrupt) holds every snapshot back, so an
	// interrupted restore is never a ledger describing files that are not
	// there; the standing marker carries the incompleteness instead.
	if sum.Failed == 0 && ctx.Err() == nil {
		for _, e := range snapshots {
			r.restoreOne(ctx, orgRoot, org, e, &sum)
		}
	} else if len(snapshots) > 0 {
		r.logger.LogAttrs(ctx, slog.LevelWarn, "pull_snapshots_deferred",
			slog.Int("count", len(snapshots)),
			slog.String("detail", "ledger snapshots are restored only after every data file has "+
				"landed and verified; re-run pull to finish"),
		)
	}

	if !sum.incomplete() && ctx.Err() == nil {
		r.ensureStubs(ctx, orgRoot, org, plan.Stubs, &sum)
	}

	err = r.settleMarker(ctx, orgRoot, org, plan, &sum, pre, hadPre)
	if err != nil {
		return sum, fmt.Errorf("settle marker: %w", err)
	}

	r.logComplete(ctx, org, sum)

	return sum, nil
}

// splitEntries separates a plan's writable entries into the data files and
// the ledger snapshots, the two phases of the restore's ordering.
func splitEntries(entries []PlanEntry) ([]PlanEntry, []PlanEntry) {
	var data, snapshots []PlanEntry

	for _, e := range entries {
		if e.Action != ActionCreate && e.Action != ActionReplace {
			continue
		}

		if e.Snapshot {
			snapshots = append(snapshots, e)
		} else {
			data = append(data, e)
		}
	}

	return data, snapshots
}

// restoreAll transfers the data entries under the restorer's concurrency
// bound, tallying into sum. A canceled context stops issuing new transfers;
// in-flight ones drain into the atomic-write discipline, leaving no partial
// file.
func (r *Restorer) restoreAll(ctx context.Context, orgRoot, org string, entries []PlanEntry, sum *Summary) {
	var (
		mu      sync.Mutex
		settled atomic.Int64
	)

	var g errgroup.Group

	g.SetLimit(r.concurrency)

	for _, e := range entries {
		if ctx.Err() != nil {
			break
		}

		g.Go(func() error {
			n, err := r.fetchVerified(ctx, orgRoot, org, e)

			mu.Lock()
			r.tally(ctx, org, e, n, err, sum)
			mu.Unlock()

			if s := settled.Add(1); s%progressInterval == 0 {
				r.logger.LogAttrs(ctx, slog.LevelInfo, "pull_progress",
					slog.String("org", org),
					slog.Int64("settled", s),
					slog.Int("planned", len(entries)),
				)
			}

			return nil
		})
	}

	//nolint:errcheck // Workers report through the tally, never an error.
	_ = g.Wait()
}

// restoreOne transfers one entry sequentially, tallying into sum.
func (r *Restorer) restoreOne(ctx context.Context, orgRoot, org string, e PlanEntry, sum *Summary) {
	n, err := r.fetchVerified(ctx, orgRoot, org, e)
	r.tally(ctx, org, e, n, err, sum)
}

// tally records one settled transfer into sum, emits its event, and reports
// it to the progress callback and the sink. The caller serializes access to
// sum.
func (r *Restorer) tally(ctx context.Context, org string, e PlanEntry, n int64, err error, sum *Summary) {
	// A failed transfer still advances the phase: the unit is settled either
	// way, and the failure carries through the callback and the summary.
	r.sink.Advance(1)

	if err != nil {
		// An interrupt surfacing through a transfer is the wind-down, not a
		// per-file fault, but it still counts: the set is not on disk, the
		// marker stays, and the exit must say so.
		r.sink.Errored(1)

		sum.Failed++
		sum.Failures = append(sum.Failures, Failure{Path: e.Rel, Err: err})

		r.logger.LogAttrs(ctx, slog.LevelError, "pull_error",
			slog.String("org", org),
			slog.String("path", e.Rel),
			slog.String("error", err.Error()),
		)
		r.progress(e.Rel, 0, err)

		return
	}

	sum.Restored++
	sum.Bytes += n

	r.logger.LogAttrs(ctx, slog.LevelDebug, "pull_restored",
		slog.String("org", org),
		slog.String("path", e.Rel),
		slog.Int64("bytes", n),
	)
	r.progress(e.Rel, n, nil)
}

// fetchVerified materializes one mirrored object at its local path: a Head
// resolves the digests the plan's listing could not carry, and the body
// streams through the digest check into an atomic write, so the file appears
// only whole and proven, or not at all. The restored file in place is the
// completion signal a re-run keys on; there is no other bookkeeping to
// repair after a crash.
func (r *Restorer) fetchVerified(ctx context.Context, orgRoot, org string, e PlanEntry) (int64, error) {
	key := r.cfg.Key(org, e.Rel)

	info, err := r.client.Head(ctx, key)
	if err != nil {
		return 0, fmt.Errorf("resolve digest: %w", err)
	}

	var n int64

	err = atomicfile.Write(pathkit.ConfineJoin(orgRoot, e.Rel), func(w io.Writer) error {
		var derr error

		n, derr = r.client.DownloadVerified(ctx, key, info, w)
		if derr != nil {
			return fmt.Errorf("fetch %q: %w", key, derr)
		}

		return nil
	})
	if err != nil {
		return 0, err //nolint:wrapcheck // Both write and download name the path and the fault.
	}

	return n, nil
}

// settleOnly settles a run whose plan transfers nothing: the tree already
// holds the restorable set, but stub backfill and marker promotion may
// still be owed, especially on a restored tree whose marker records partial
// and whose stubs are absent. A tree whose marker already records complete
// needs only its missing stubs repaired (a complete marker's reads have no
// mirror fallback, so a locally lost stub would strand its tarball; in the
// steady state every stub is present and nothing is probed or written), and
// a tree with no marker at all is left untouched, which is what makes a
// re-run against a restored archive change nothing.
func (r *Restorer) settleOnly(
	ctx context.Context, orgRoot, org string, plan Plan, sum Summary,
	pre remote.Marker, hadPre bool,
) (Summary, error) {
	switch {
	case sum.incomplete() || ctx.Err() != nil || !hadPre:
		// A refused plan leaves the marker carrying the incompleteness; an
		// absent marker means nothing claimed the mirror stands in, and a
		// settlement would invent that claim.

	case preComplete(pre, hadPre):
		if missing := missingStubs(plan.Stubs); len(missing) > 0 {
			r.ensureStubs(ctx, orgRoot, org, missing, &sum)
		}

		sum.Complete = true
		sum.Leftovers = plan.Leftovers

		if len(plan.Leftovers) > 0 || sum.StubsFailed > 0 {
			r.logMarkerLeftovers(ctx, org, plan, sum)
		}

	default:
		r.ensureStubs(ctx, orgRoot, org, plan.Stubs, &sum)

		err := r.settleMarker(ctx, orgRoot, org, plan, &sum, pre, hadPre)
		if err != nil {
			return sum, fmt.Errorf("settle marker: %w", err)
		}
	}

	r.logComplete(ctx, org, sum)

	//nolint:nilerr // An interrupt settles nothing and reports the tally, not an error.
	return sum, nil
}

// logMarkerLeftovers warns that unaccounted keys or unensured stubs stand
// under a marker that already records complete: the never-demote override
// keeps the marker, and the next archiver run reconciles what the event
// names.
func (r *Restorer) logMarkerLeftovers(ctx context.Context, org string, plan Plan, sum Summary) {
	r.logger.LogAttrs(ctx, slog.LevelWarn, "pull_marker_leftovers",
		slog.String("org", org),
		slog.Any("leftovers", plan.Leftovers),
		slog.Int("stubs_failed", sum.StubsFailed),
		slog.String("detail", "the marker already records complete; the next archiver run reconciles"),
	)
}

// missingStubs filters a plan's stub work to the entries with no local stub
// file, the only repair a complete marker still owes.
func missingStubs(stubs []StubEntry) []StubEntry {
	var missing []StubEntry

	for _, e := range stubs {
		if !e.Existing {
			missing = append(missing, e)
		}
	}

	return missing
}

// preComplete reports whether the pre-run marker already recorded the tree
// as complete, the state settlement never demotes.
func preComplete(pre remote.Marker, hadPre bool) bool {
	return hadPre && !pre.Restoring && !pre.Partial && pre.URL != ""
}

// settleMarker rewrites the marker in its settled form once the restored set
// is proven on disk: complete when the run accounts for every mirrored key
// (each restorable object restored or verified, each evicted tarball's stub
// ensured, each bundle zip's sidecar present, no foreign keys), else partial
// with the unaccounted keys named in the log. A marker that already recorded
// complete is never demoted; leftovers under one are the next archiver run's
// business. An incomplete or interrupted run leaves the restoring marker
// standing, exactly as an interrupted transfer does.
func (r *Restorer) settleMarker(
	ctx context.Context, orgRoot, org string, plan Plan, sum *Summary,
	pre remote.Marker, hadPre bool,
) error {
	if sum.incomplete() || ctx.Err() != nil {
		//nolint:nilerr // The restoring marker carries the incompleteness; nothing settles.
		return nil
	}

	r.sink.SetPhase(PhaseSettle)
	r.sink.SetTotal(0)

	sum.Leftovers = plan.Leftovers

	proven := sum.StubsFailed == 0 && len(plan.Leftovers) == 0
	wasComplete := preComplete(pre, hadPre)

	marker := r.cfg.Marker()
	marker.Partial = !proven && !wasComplete
	sum.Complete = !marker.Partial

	switch {
	case proven:
	case wasComplete:
		r.logMarkerLeftovers(ctx, org, plan, *sum)

	default:
		r.logger.LogAttrs(ctx, slog.LevelWarn, "pull_marker_partial",
			slog.String("org", org),
			slog.Any("leftovers", plan.Leftovers),
			slog.Int("stubs_failed", sum.StubsFailed),
		)
	}

	wrote, err := r.writeMarkerChanged(orgRoot, marker)
	if err != nil {
		return err
	}

	if wrote && sum.Complete {
		r.logger.LogAttrs(ctx, slog.LevelInfo, "pull_marker_promoted",
			slog.String("org", org),
		)
	}

	return nil
}

// ensureStubs materializes or verifies the eviction stub beside every
// mirrored configuration-version tarball, from the mirror's own record of
// it: one Head resolves the size and, when the upload recorded one, the
// digest, the same synthesis the viewer's merged fallback performs. A stub
// that cannot be ensured is a read-model gap, not a custody one: the run
// warns, counts it, holds a marker not already complete at partial, and
// never fails.
func (r *Restorer) ensureStubs(ctx context.Context, orgRoot, org string, stubs []StubEntry, sum *Summary) {
	if len(stubs) == 0 {
		return
	}

	r.sink.SetPhase(PhaseStubs)
	r.sink.SetTotal(len(stubs))

	var (
		mu sync.Mutex
		g  errgroup.Group
	)

	g.SetLimit(r.concurrency)

	for _, e := range stubs {
		if ctx.Err() != nil {
			break
		}

		g.Go(func() error {
			wrote, err := r.ensureStub(ctx, orgRoot, org, e)

			mu.Lock()
			defer mu.Unlock()

			// A stub that could not be ensured still advances the phase: the
			// unit is settled either way, and the tally carries the failure.
			r.sink.Advance(1)

			if err != nil {
				sum.StubsFailed++

				// A conflict already emitted its own richer event, with the
				// recorded and mirrored values side by side.
				if !errors.Is(err, errStubConflict) {
					r.logger.LogAttrs(ctx, slog.LevelWarn, "pull_stub_error",
						slog.String("org", org),
						slog.String("path", store.RemoteStubPath(e.Rel)),
						slog.String("error", err.Error()),
					)
				}

				return nil
			}

			sum.Stubs++

			if wrote {
				r.logger.LogAttrs(ctx, slog.LevelInfo, "pull_stub_written",
					slog.String("org", org),
					slog.String("path", store.RemoteStubPath(e.Rel)),
					slog.Bool("repaired", e.Existing),
				)
			}

			return nil
		})
	}

	//nolint:errcheck // Workers report through the tally, never an error.
	_ = g.Wait()
}

// ensureStub settles one tarball's stub against the mirror, reporting
// whether it wrote. An absent, corrupt, or invalid stub is written from the
// Head (the write is the repair), and a valid digestless stub whose size
// matches a digest-bearing Head is upgraded with the digest, a
// strengthening, never a weakening. A valid stub that contradicts the Head
// (a size mismatch, or two nonempty digests that differ) is left standing and
// reported: the stub is the only independent record of a mirror-side change,
// and under it a fetch fails verification loudly, while a replacement would
// serve the changed bytes silently; the partial marker the conflict forces
// keeps the viewer's mirror fallback reachable. One-sided digest absence at
// an equal size is never a conflict, the same trust the classification
// extends to digestless objects. A stub recording a schema version newer
// than this build's is never touched: a newer build wrote it, and this one
// cannot judge its fields.
func (r *Restorer) ensureStub(ctx context.Context, orgRoot, org string, e StubEntry) (bool, error) {
	info, err := r.client.Head(ctx, r.cfg.Key(org, e.Rel))
	if err != nil {
		return false, fmt.Errorf("resolve mirrored tarball: %w", err)
	}

	want := store.RemoteStub{
		Version: store.RemoteStubVersion,
		Size:    info.Size,
		SHA256:  info.SHA256,
	}

	abs := pathkit.ConfineJoin(orgRoot, store.RemoteStubPath(e.Rel))

	existing, state := readStubFile(abs)

	switch state {
	case stubReadError:
		return false, fmt.Errorf("read existing stub %q: %w", abs, existing.err)
	case stubNewer:
		return false, fmt.Errorf("stub at %q records schema version %d, newer than this build's %d",
			abs, existing.stub.Version, store.RemoteStubVersion)

	case stubValid:
		switch {
		case existing.stub.Size != want.Size,
			existing.stub.SHA256 != "" && want.SHA256 != "" && existing.stub.SHA256 != want.SHA256:
			r.logger.LogAttrs(ctx, slog.LevelWarn, "pull_stub_conflict",
				slog.String("org", org),
				slog.String("path", store.RemoteStubPath(e.Rel)),
				slog.Int64("recorded_bytes", existing.stub.Size),
				slog.Int64("mirror_bytes", want.Size),
				slog.String("recorded_sha256", existing.stub.SHA256),
				slog.String("mirror_sha256", want.SHA256),
			)

			return false, errStubConflict

		case existing.stub.SHA256 == want.SHA256 || want.SHA256 == "":
			// Identical, or the mirror records no digest to add: the stub in
			// place, digest-bearing or not, is already the stronger record.
			return false, nil
		}

		// A digestless stub against a digest-bearing Head at the same size
		// falls through: the rewrite adds the digest it lacked.

	case stubAbsent, stubInvalid:
	}

	data, err := serialize.Marshal(want)
	if err != nil {
		return false, fmt.Errorf("marshal eviction stub: %w", err)
	}

	err = atomicfile.WriteFile(abs, data)
	if err != nil {
		return false, fmt.Errorf("write eviction stub: %w", err)
	}

	return true, nil
}

// stubState classifies what stands at a stub's path for
// [Restorer.ensureStub].
type stubState int

const (
	// No stub file on disk.
	stubAbsent stubState = iota
	// A well-formed stub this build understands.
	stubValid
	// A stub that does not parse or records an impossible shape; the
	// rewrite is the repair.
	stubInvalid
	// A stub recording a schema version newer than this build writes; it
	// is never touched.
	stubNewer
	// A stub whose bytes could not be read at all.
	stubReadError
)

// readStub carries the parsed stub or the fault that kept it from being
// read; which field is meaningful follows from the [stubState].
type readStub struct {
	err  error
	stub store.RemoteStub
}

// readStubFile reads and classifies the stub file at abs.
func readStubFile(abs string) (readStub, stubState) {
	//nolint:gosec // The path is confined under the org root being restored.
	data, err := os.ReadFile(abs)

	switch {
	case errors.Is(err, fs.ErrNotExist):
		return readStub{}, stubAbsent
	case err != nil:
		return readStub{err: err}, stubReadError
	}

	var stub store.RemoteStub

	err = json.Unmarshal(data, &stub)

	switch {
	case err != nil, stub.Version < 1, stub.Size < 0:
		return readStub{}, stubInvalid
	case stub.Version > store.RemoteStubVersion:
		return readStub{stub: stub}, stubNewer
	}

	return readStub{stub: stub}, stubValid
}

// writeMarkerChanged durably records marker at the org root, skipping the
// write when the file already holds exactly those bytes, and reports whether
// it wrote.
func (r *Restorer) writeMarkerChanged(orgRoot string, marker remote.Marker) (bool, error) {
	data, err := markerBytes(marker)
	if err != nil {
		return false, err
	}

	//nolint:gosec // The path is composed from the org root being restored.
	current, rerr := os.ReadFile(filepath.Join(orgRoot, remote.MarkerName))
	if rerr == nil && bytes.Equal(current, data) {
		return false, nil
	}

	err = atomicfile.WriteFile(filepath.Join(orgRoot, remote.MarkerName), data)
	if err != nil {
		return false, fmt.Errorf("write remote marker: %w", err)
	}

	return true, nil
}

// writeMarker durably records marker at the org root.
func (r *Restorer) writeMarker(orgRoot string, marker remote.Marker) error {
	data, err := markerBytes(marker)
	if err != nil {
		return err
	}

	err = atomicfile.WriteFile(filepath.Join(orgRoot, remote.MarkerName), data)
	if err != nil {
		return fmt.Errorf("write remote marker: %w", err)
	}

	return nil
}

// markerBytes renders a marker exactly as every writer of the file does, so
// a byte comparison against the file on disk is meaningful.
func markerBytes(marker remote.Marker) ([]byte, error) {
	data, err := json.MarshalIndent(marker, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal remote marker: %w", err)
	}

	return append(data, '\n'), nil
}

// logComplete emits the run's closing tally.
func (r *Restorer) logComplete(ctx context.Context, org string, sum Summary) {
	level := slog.LevelInfo
	if sum.incomplete() {
		level = slog.LevelWarn
	}

	r.logger.LogAttrs(ctx, level, "pull_complete",
		slog.String("org", org),
		slog.Int("restored", sum.Restored),
		slog.Int("skipped", sum.Skipped),
		slog.Int("failed", sum.Failed),
		slog.Int("refused", sum.Refused),
		slog.Int("stubs", sum.Stubs),
		slog.Int("stubs_failed", sum.StubsFailed),
		slog.Int64("bytes", sum.Bytes),
		slog.Bool("complete", sum.Complete),
	)
}
