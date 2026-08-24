package restore

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"sync/atomic"

	"golang.org/x/sync/errgroup"

	"go.jacobcolvin.com/hcp_archiver/pkg/atomicfile"
	"go.jacobcolvin.com/hcp_archiver/pkg/collect"
	"go.jacobcolvin.com/hcp_archiver/pkg/pathkit"
	"go.jacobcolvin.com/hcp_archiver/pkg/remote"
)

// progressInterval is how many settled entries pass between pull_progress
// events, matching the sweep's own cadence.
const progressInterval = 1000

// Restorer restores one organization's warm layer from its mirror. Create
// instances with [NewRestorer].
type Restorer struct {
	client      *remote.Client
	logger      *slog.Logger
	progress    func(relPath string, bytes int64, err error)
	cfg         remote.Config
	concurrency int
}

// Option configures a [Restorer] passed to [NewRestorer].
//
// The available options are:
//   - [WithLogger]
//   - [WithConcurrency]
//   - [WithProgress]
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
	// Restored counts the files written and verified.
	Restored int
	// Skipped counts the files already verified identical locally.
	Skipped int
	// Failed counts the files that could not be restored.
	Failed int
	// Refused counts the conflicts the restore declined to touch.
	Refused int
	// Bytes totals the restored files' sizes.
	Bytes int64
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
// absent files), and the marker is rewritten to its final form only when
// every entry in the set is present and verified. A plan that changes
// nothing writes nothing, except that a leftover restoring marker from an
// interrupted run is still finalized, since the tree it guards is proven
// whole.
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

	if plan.RestoreFiles == 0 {
		err := r.finalizeLeftoverMarker(ctx, orgRoot, org, sum)
		if err != nil {
			return sum, err
		}

		r.logComplete(ctx, org, sum)

		return sum, nil
	}

	err := r.writeMarker(orgRoot, r.cfg.RestoringMarker())
	if err != nil {
		return sum, fmt.Errorf("record restoring marker: %w", err)
	}

	data, snapshots := splitEntries(plan.Entries)

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
		err = r.finalizeMarker(orgRoot)
		if err != nil {
			return sum, fmt.Errorf("finalize marker: %w", err)
		}
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
// it to the progress callback. The caller serializes access to sum.
func (r *Restorer) tally(ctx context.Context, org string, e PlanEntry, n int64, err error, sum *Summary) {
	if err != nil {
		// An interrupt surfacing through a transfer is the wind-down, not a
		// per-file fault, but it still counts: the set is not on disk, the
		// marker stays, and the exit must say so.
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

	r.logger.LogAttrs(ctx, slog.LevelInfo, "pull_restored",
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

// finalizeLeftoverMarker finalizes a restoring marker left by an interrupted
// run whose work a re-run then found complete: the plan changed nothing, so
// the tree is proven whole, and only the marker still says otherwise.
// Leaving it would strand the archive behind the restore-in-progress
// refusals forever. A tree with no marker, or a settled one, is left
// untouched, which is what makes a re-run against a restored archive change
// nothing at all.
func (r *Restorer) finalizeLeftoverMarker(ctx context.Context, orgRoot, org string, sum Summary) error {
	if sum.incomplete() {
		return nil
	}

	existing, ok, err := remote.ReadMarker(orgRoot)
	if err != nil || !ok || !existing.Restoring {
		return err //nolint:wrapcheck // The marker reader names the file and the fault.
	}

	err = r.finalizeMarker(orgRoot)
	if err != nil {
		return fmt.Errorf("finalize marker: %w", err)
	}

	r.logger.LogAttrs(ctx, slog.LevelInfo, "pull_marker_finalized",
		slog.String("org", org),
		slog.String("detail", "an interrupted restore had already landed every file; its marker is settled"),
	)

	return nil
}

// finalizeMarker rewrites the marker in its settled form: the steady-state
// version with the partial flag, since the restored warm layer does not by
// itself account for the mirror's evicted tarballs (their local stubs are
// never mirrored); the next clean archive run backfills the stubs and
// promotes the marker complete.
func (r *Restorer) finalizeMarker(orgRoot string) error {
	marker := r.cfg.Marker()
	marker.Partial = true

	return r.writeMarker(orgRoot, marker)
}

// writeMarker durably records marker at the org root.
func (r *Restorer) writeMarker(orgRoot string, marker remote.Marker) error {
	data, err := json.MarshalIndent(marker, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal remote marker: %w", err)
	}

	data = append(data, '\n')

	err = atomicfile.WriteFile(filepath.Join(orgRoot, remote.MarkerName), data)
	if err != nil {
		return fmt.Errorf("write remote marker: %w", err)
	}

	return nil
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
		slog.Int64("bytes", sum.Bytes),
	)
}
