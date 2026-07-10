package archiver

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
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
		return orgs, nil
	}

	return list(ctx)
}

// listOrgNames enumerates the names of every organization visible to the
// client, paginating through the shared limiter.
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
func (a *Archiver) runOrg(ctx context.Context, orgName string) error {
	st := store.New(filepath.Join(a.cfg.OutputDir, orgName))

	ledger, err := manifest.Load(
		st.Root(),
		manifest.WithRecheckAbsent(a.cfg.RecheckAbsent),
	)
	if err != nil {
		return fmt.Errorf("load manifest: %w", err)
	}

	env := collect.NewEnv(a.client, st, ledger)
	reporter := progress.New(a.w, a.cfg.ProgressMode, ledger,
		progress.WithInterval(a.cfg.ProgressInterval),
		progress.WithInterrupt(a.cancelRun),
		progress.WithLogSink(a.logSink),
	)

	ledger.StartRun()

	orgCtx, cancelOrg := context.WithCancel(ctx)

	var wg sync.WaitGroup

	wg.Add(2)

	go func() {
		defer wg.Done()

		rerr := reporter.Run(orgCtx, a.cfg.ProgressInterval)
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

	defer func() {
		ledger.FinishRun()
		cancelOrg()
		wg.Wait()

		ferr := ledger.Flush()
		if ferr != nil {
			a.logFlushError(ctx, orgName, ferr)
		}

		serr := reporter.Summary()
		if serr != nil {
			a.logger.LogAttrs(ctx, slog.LevelWarn, "progress_summary_error",
				slog.String("org", orgName),
				slog.String("error", serr.Error()),
			)
		}
	}()

	return a.collectOrg(ctx, env, reporter, orgName)
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

// logFlushError records a non-fatal manifest flush failure.
func (a *Archiver) logFlushError(ctx context.Context, orgName string, err error) {
	a.logger.LogAttrs(ctx, slog.LevelWarn, "manifest_flush_error",
		slog.String("org", orgName),
		slog.String("error", err.Error()),
	)
}
