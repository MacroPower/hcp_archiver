package archiver

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/hashicorp/go-tfe"

	"go.jacobcolvin.com/hcp_archiver/collect"
	"go.jacobcolvin.com/hcp_archiver/manifest"
	"go.jacobcolvin.com/hcp_archiver/progress"
	"go.jacobcolvin.com/hcp_archiver/store"
	"go.jacobcolvin.com/hcp_archiver/tfeclient"
)

// resolveOrgs resolves the organizations to archive.
//
// When cfg names one or more organizations only those are archived and list is
// never called; otherwise it enumerates every organization the token can see.
func (a *Archiver) resolveOrgs(ctx context.Context) ([]string, error) {
	return resolveOrgs(ctx, a.cfg.Organizations, func(ctx context.Context) ([]string, error) {
		return listOrgNames(ctx, a.client)
	})
}

// resolveOrgs picks the named organizations, or defers to list when none are
// named. Factoring the choice out of the client wiring keeps it testable
// without a network.
func resolveOrgs(ctx context.Context, orgs []string, list func(context.Context) ([]string, error)) ([]string, error) {
	if len(orgs) > 0 {
		// Clone so the returned slice never aliases the caller's config backing
		// array; the run loop owns its list.
		return slices.Clone(orgs), nil
	}

	return list(ctx)
}

// listOrgNames enumerates the names of every organization visible to the
// client, paginating through the shared governor.
func listOrgNames(ctx context.Context, client *tfeclient.Client) ([]string, error) {
	orgs, err := tfeclient.Paginate(ctx, client,
		func(ctx context.Context, tc *tfe.Client, o tfe.ListOptions) ([]*tfe.Organization, *tfe.Pagination, error) {
			l, e := tc.Organizations.List(ctx, &tfe.OrganizationListOptions{ListOptions: o})
			if e != nil {
				return nil, nil, fmt.Errorf("list organizations: %w", e)
			}

			return l.Items, l.Pagination, nil
		})
	if err != nil {
		return nil, fmt.Errorf("paginate organizations: %w", err)
	}

	names := make([]string, 0, len(orgs))
	for _, o := range orgs {
		names = append(names, o.Name)
	}

	return names, nil
}

// runOrg archives one organization end to end.
//
// It builds the org's store, ledger, environment, and reporter, opens a run,
// starts the background progress and flush goroutines bounded to a cancelable
// child context, drives the collectors with the parent ctx so an interrupt
// cancels the work, and then closes the run: it writes the run record, stops
// the goroutines, flushes the ledger a final time, and prints the summary.
//
// The int result is how many files the close sweep failed to mirror, so the
// caller sees the sweep's outcome alongside the tally.
func (a *Archiver) runOrg(ctx context.Context, orgName string) (manifest.Tally, int, error) {
	st := store.New(filepath.Join(a.cfg.OutputDir, orgName))
	envOpts := []collect.Option{collect.WithLogger(a.logger)}

	if a.remote != nil {
		envOpts = append(envOpts,
			collect.WithRemote(a.remote, remoteConfig(a.cfg.Remote), orgName),
		)
	}

	ledger, err := manifest.Load(
		st.Root(),
		manifest.WithRetryAbsent(a.cfg.RetryAbsent),
	)
	if err != nil {
		return manifest.Tally{}, 0, fmt.Errorf("load manifest: %w", err)
	}

	env := collect.NewEnv(a.client, st, ledger, envOpts...)

	// The marker is written before any collector runs, so even an archive
	// interrupted mid-run records where its evicted bundles live — but after
	// manifest.Load acquired the cross-process flock, since it mutates the
	// org root like any other write and must not race a run in progress.
	if a.remote != nil {
		markerErr := a.writeRemoteMarker(ctx, env, st)
		if markerErr != nil {
			closeErr := ledger.Close()
			if closeErr != nil {
				a.logger.LogAttrs(ctx, slog.LevelWarn, "manifest_close_error",
					slog.String("org", orgName),
					slog.String("error", closeErr.Error()),
				)
			}

			return manifest.Tally{}, 0, markerErr
		}
	}

	reporterOpts := []progress.Option{
		progress.WithInterval(a.cfg.ProgressInterval),
		progress.WithInterrupt(a.cancelRun),
		progress.WithLogSink(a.logSink),
		progress.WithWireBytes(a.wireBytes),
		progress.WithRateStatus(a.client.RateStatus),
		progress.WithRateLimited(a.rateLimited),
	}

	// With a remote configured the reporter watches the environment's run-wide
	// transfer tally, so uploads and evictions read live in every output form.
	// A plain conversion carries the snapshot across the package boundary: the
	// two structs are field-identical, and a drift in either fails to compile
	// here rather than silently dropping a field.
	if a.remote != nil {
		reporterOpts = append(reporterOpts,
			progress.WithRemoteStats(func() progress.RemoteStats {
				return progress.RemoteStats(env.RemoteTally())
			}),
			progress.WithUploadWireBytes(a.uploadWireBytes),
		)
	}

	reporter := progress.New(a.w, a.cfg.ProgressMode, ledger, reporterOpts...)

	ledger.StartRun()

	orgCtx, cancelOrg := context.WithCancel(ctx)

	var wg sync.WaitGroup

	wg.Add(2)

	go func() {
		defer wg.Done()

		// The interval is already configured via WithInterval above; a non-positive
		// value here falls back to it, so it need not be passed again.
		rerr := reporter.Run(orgCtx, 0)
		if rerr != nil {
			a.logger.LogAttrs(orgCtx, slog.LevelWarn, "progress_report_error",
				slog.String("org", orgName),
				slog.String("error", rerr.Error()),
			)
		}
	}()

	go func() {
		defer wg.Done()

		a.flushLoop(orgCtx, orgName, ledger)
	}()

	// The closeRun function below settles the organization's run exactly
	// once, recording how many files the close sweep failed to mirror. It is
	// called explicitly on the normal path so its outcome reaches the caller,
	// and deferred for the panic path so an unwinding run still flushes and
	// releases the ledger; the sync.Once keeps the two from running it twice.
	var (
		closeOnce  sync.Once
		syncFailed int
	)

	closeRun := func() {
		closeOnce.Do(func() {
			ledger.FinishRun()
			cancelOrg()
			wg.Wait()

			ferr := ledger.Flush()
			if ferr != nil {
				a.logFlushError(ctx, orgName, ferr)
			}

			a.logFailures(ctx, orgName, ledger)
			a.logDroppedSurfaces(ctx, orgName, ledger)

			// The close sweep runs after the final flush above, so every touched
			// shard is compacted (its durable form is the snapshot the sweep
			// mirrors), and before Close below, so the cross-process flock still
			// guards the tree. An interrupted run skips it; the next run sweeps.
			syncFailed = a.syncOrg(ctx, env, orgName).Failed

			// The sweep deliberately skips the per-shard replay logs and mirrors
			// only the snapshots, on the guarantee the final flush just made
			// them current. A failed flush breaks that guarantee: the sweep
			// mirrored a ledger the run knows is behind, and a restore from the
			// bucket would replay resurrected state. Local disk self-heals on
			// the next flush, but the mirror is the long-term record, so the
			// run must not exit clean over it.
			if ferr != nil && a.remote != nil && ctx.Err() == nil {
				syncFailed++

				a.logger.LogAttrs(ctx, slog.LevelError, "remote_ledger_stale",
					slog.String("org", orgName),
					slog.String("detail", "the final ledger flush failed, so the mirrored "+
						"snapshots are stale; the run is marked incomplete and the next "+
						"run's flush and sweep re-mirror them"),
				)
			}

			serr := reporter.Summary()
			if serr != nil {
				a.logger.LogAttrs(ctx, slog.LevelWarn, "progress_summary_error",
					slog.String("org", orgName),
					slog.String("error", serr.Error()),
				)
			}

			// Release the org's cross-process ledger lock last, after the final
			// flush above, so no other process can open the root while this run's
			// unflushed state is still in memory.
			cerr := ledger.Close()
			if cerr != nil {
				a.logger.LogAttrs(ctx, slog.LevelWarn, "manifest_close_error",
					slog.String("org", orgName),
					slog.String("error", cerr.Error()),
				)
			}
		})
	}

	defer closeRun()

	// Capture the final per-status counts before the close runs; the
	// collectors are done once collectOrg returns, so the tally is settled.
	// Run uses it to tell an organization that captured nothing but failures
	// from a clean one, since the collectors record per-object failures in
	// the ledger and return non-nil only on cancellation.
	collectErr := a.collectOrg(ctx, env, reporter, orgName)
	tally := ledger.Tally()

	closeRun()

	return tally, syncFailed, collectErr
}

// orgIncomplete is the one predicate deciding whether an organization's run
// leaves the whole run incomplete, so a scheduled run exits non-zero rather
// than reporting success over a gap. Two conditions mark it:
//
//   - a dropped surface: some listing failed for a non-cancellation reason,
//     so an unknown number of objects were never even named. Every collector
//     records its drops through [collect.Env.MarkSurfaceDropped], which is what
//     makes this check cover all of them rather than whichever ones remembered
//     to plumb an error back.
//   - a wholly-failed org: no object settled done while at least one errored or
//     was forbidden. An empty organization (nothing recorded) is not a failure,
//     and a run whose listings completed is merely partial if some objects
//     failed. Those failures are visible as errored entries, logged at close,
//     and retried next run.
func orgIncomplete(t manifest.Tally) bool {
	whollyFailed := t.Done == 0 && (t.Errored > 0 || t.Forbidden > 0)

	return t.SurfacesDropped > 0 || whollyFailed
}

// flushLoop flushes the ledger on a fixed cadence until ctx is done, then
// flushes once more so a hard kill loses at most the last unflushed batch.
func (a *Archiver) flushLoop(ctx context.Context, orgName string, ledger *manifest.Ledger) {
	ticker := time.NewTicker(a.flushInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			err := ledger.Flush()
			if err != nil {
				a.logFlushError(ctx, orgName, err)
			}

			return

		case <-ticker.C:
			err := ledger.Flush()
			if err != nil {
				a.logFlushError(ctx, orgName, err)
			}
		}
	}
}

// logFailures writes one error log per object still recorded errored or
// forbidden when the organization's run closes. The log stream truncates
// nothing, so the full failure text survives there, and the summary that
// follows only counts the failures. The per-object errors are recorded
// silently as the walk proceeds, so this is where they surface.
func (a *Archiver) logFailures(ctx context.Context, orgName string, ledger *manifest.Ledger) {
	for _, f := range ledger.Failures() {
		a.logger.LogAttrs(ctx, slog.LevelError, "object_archive_error",
			slog.String("org", orgName),
			slog.String("status", string(f.Status)),
			slog.String("path", f.RelPath),
			slog.String("error", f.Error),
		)
	}
}

// logDroppedSurfaces writes one error log per enumeration surface dropped this
// run when the organization's run closes, naming each listing whose failure
// left the archive's extent unknown. The drops are recorded silently as the
// collectors proceed, so this is where they surface alongside the per-object
// failures.
func (a *Archiver) logDroppedSurfaces(ctx context.Context, orgName string, ledger *manifest.Ledger) {
	for _, d := range ledger.DroppedSurfaces() {
		a.logger.LogAttrs(ctx, slog.LevelError, "surface_dropped",
			slog.String("org", orgName),
			slog.String("surface", d.Surface),
			slog.String("error", d.Error),
		)
	}
}

// logFlushError records a non-fatal manifest flush failure.
func (a *Archiver) logFlushError(ctx context.Context, orgName string, err error) {
	a.logger.LogAttrs(ctx, slog.LevelWarn, "manifest_flush_error",
		slog.String("org", orgName),
		slog.String("error", err.Error()),
	)
}
