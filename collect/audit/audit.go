package audit

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	tfe "github.com/hashicorp/go-tfe"

	"go.jacobcolvin.com/hcp_archiver/collect"
	"go.jacobcolvin.com/hcp_archiver/manifest"
)

// configFile is the leaf name of the organization audit-configuration file
// under the audit-trail directory.
const configFile = "config.json"

// Collector archives an organization's audit surface: its audit configuration
// and the windowed audit-trail pages.
//
// The trail requires an elevated token (an organization owner or an audit
// token) and covers only HCP's retention window, so a page the token cannot
// read or a window that has aged out is recorded and skipped rather than
// aborting the collector. Unlike the jsonapi collections, audit trails are
// plain JSON and page by a time cursor: a re-run reads the recorded Since
// watermark and walks forward from it, appending only newer pages.
//
// Create instances with [New]. It satisfies [collect.Collector].
type Collector struct {
	env *collect.Env
	org string
}

// New creates a new [Collector] archiving the audit surface of org into env.
//
// The archiver constructs and runs it only when the audit-trail scope is
// enabled and the client carries a token elevated enough to read the trail.
func New(env *collect.Env, org string) *Collector {
	return &Collector{
		env: env,
		org: org,
	}
}

// Name identifies the collector for progress and logs.
func (c *Collector) Name() string {
	return "audit"
}

// Collect archives the organization audit configuration and then the
// audit-trail pages.
//
// It returns only on a cancellation of ctx; a missing configuration, an
// unelevated token, or an aged-out window is recorded and does not abort the
// collector.
func (c *Collector) Collect(ctx context.Context) error {
	err := c.collectConfig(ctx)
	if err != nil {
		return err
	}

	return c.collectTrails(ctx)
}

// collectConfig archives the organization's audit configuration, the record of
// whether and how auditing is enabled. It is mutable metadata, refreshed on
// every run.
func (c *Collector) collectConfig(ctx context.Context) error {
	relPath := c.env.Store().AuditTrailFile(configFile)

	err := c.env.Mutable(ctx, relPath, func(ctx context.Context) (any, error) {
		var out *tfe.OrganizationAuditConfiguration

		derr := c.env.Client().Do(ctx, func(ctx context.Context, tc *tfe.Client) error {
			var e error

			out, e = tc.OrganizationAuditConfigurations.Read(ctx, c.org)
			if e != nil {
				return fmt.Errorf("read audit configuration: %w", e)
			}

			return nil
		})

		return out, derr //nolint:wrapcheck // Wrapped in the Do closure.
	})
	if err != nil {
		return fmt.Errorf("archive audit configuration: %w", err)
	}

	return nil
}

// collectTrails walks the audit-trail pages forward from the recorded Since
// watermark, appending each page as an immutable object and advancing the
// watermark once the whole walk completes.
//
// The watermark advances only after a complete walk, not per page: the trail's
// page order is unspecified, so advancing mid-walk could skip an unfetched
// older page on the next run. Page files are keyed on the Since cursor rather
// than a bare page number, so a re-run under an unchanged watermark (after an
// interruption) resumes over the same immutable file names, and a re-run under
// an advanced watermark appends a fresh set. A page the token cannot read halts
// the walk without advancing the watermark, so a later elevated run retries
// from the same cursor.
//
// The watermark is sourced from the events actually persisted this run, never
// from the raw re-listed items: a page settled on a prior run is skipped by
// Object's settled-guard, so a newer event the re-list carries on it is not
// archived and must not step the cursor past it. That page's contribution is
// read back from its stored file; a freshly written page's is its own events.
// Because visible audit events are ordered by creation time, a genuinely new
// event sorts later than every persisted one, so the cursor advances only up to
// what was persisted and the new event is captured on the following run, once
// the advanced cursor lists it under a fresh page name.
func (c *Collector) collectTrails(ctx context.Context) error {
	st := c.env.Store()
	key := st.AuditTrailDir()
	since := c.env.HighWaterMark(key)

	var newest time.Time

	for page := 1; ; {
		list, listErr := c.listPage(ctx, since, page)

		// Keep only events strictly newer than the watermark, dropping the
		// already-archived events the whole-second wire cursor re-lists when a walk
		// resumes from a sub-second watermark.
		var fresh []*tfe.AuditTrail

		if listErr == nil {
			fresh = eventsAfter(list.Items, since)
		}

		// Write the page unless it lists cleanly but carries only already-archived
		// events -- an empty page among them. Skipping such a page avoids
		// duplicating events under a fresh name and settling an empty page under
		// the unchanged cursor (a later run's events would then be skipped as
		// already done and lost). The trail's page order is unspecified, so an
		// empty page never ends the walk: only the pagination reporting no next
		// page does, and a non-empty later page may still follow an empty one.
		if listErr != nil || len(fresh) > 0 {
			relPath := st.AuditTrailFile(pageName(since, page))

			// Whether Object will persist fresh or short-circuit on an already-Done
			// page is fixed by the ledger before the call decides it.
			preDone := c.pageDone(relPath)

			halt, err := c.archiveTrailPage(ctx, since, page, fresh, listErr)
			if err != nil {
				return err
			}

			if halt {
				return nil
			}

			t, halt := c.persistedNewest(key, relPath, fresh, preDone)
			if halt {
				return nil
			}

			if t.After(newest) {
				newest = t
			}
		}

		p := list.AuditTrailPagination
		if p == nil || p.NextPage == 0 {
			break
		}

		// A NextPage that does not advance past the current page (a misbehaving
		// server or a cycle) means the walk cannot reach the pages the server
		// still claims exist. Halt without advancing the watermark, exactly like
		// the fetch- and write-error paths, so the next run retries from the same
		// cursor rather than stepping past the unreached pages' events under a
		// watermark it would never revisit. The unreached pages are a dropped
		// surface: nothing records them, so the run must not report complete.
		if p.NextPage <= page {
			c.env.MarkSurfaceDropped(key,
				fmt.Errorf("audit trail pagination stalled: page %d reports next page %d", page, p.NextPage))

			return nil
		}

		page = p.NextPage
	}

	if !newest.IsZero() {
		c.env.AdvanceHighWaterMark(key, newest)
	}

	return nil
}

// archiveTrailPage records one audit-trail page -- its fresh events, or the list
// error -- and reports whether the walk must halt without advancing the
// watermark. A fetch error the walk cannot page past, and a write that did not
// settle, both halt so the next run retries the page from the same cursor; a
// halt also records the trail as a dropped surface, since the pages past it were
// never reached and nothing else marks the gap. It is called only for a page
// carrying fresh events or a list error; a clean page of only already-archived
// events is skipped by the caller.
func (c *Collector) archiveTrailPage(
	ctx context.Context,
	since time.Time,
	page int,
	fresh []*tfe.AuditTrail,
	listErr error,
) (bool, error) {
	relPath := c.env.Store().AuditTrailFile(pageName(since, page))

	err := c.env.Object(ctx, relPath, func(context.Context) (any, error) {
		if listErr != nil {
			return nil, listErr
		}

		return fresh, nil
	})
	if err != nil {
		return false, fmt.Errorf("archive audit trail page: %w", err)
	}

	if listErr != nil {
		// A cancellation must propagate even when Object short-circuited on an
		// already-settled page and so never observed it, or the walk returns nil on
		// a real cancellation and the run logs a canceled org as a clean finish.
		ctxErr := ctx.Err()
		if ctxErr != nil {
			return false, fmt.Errorf("archive audit trail page: %w", ctxErr)
		}

		// The page fetch is recorded by Object; the walk cannot paginate past an
		// unreadable page, so halt without advancing the watermark.
		c.env.MarkSurfaceDropped(c.env.Store().AuditTrailDir(), listErr)

		return true, nil
	}

	// A page whose write failed is recorded errored rather than settled, and its
	// file name is keyed on the unadvanced Since cursor. Halt without advancing so
	// the next run retries the page from the same cursor instead of stepping past
	// its events under a name it would never revisit.
	if entry, ok := c.env.Entry(relPath); ok && !entry.Status.Settled() {
		c.env.MarkSurfaceDropped(c.env.Store().AuditTrailDir(),
			fmt.Errorf("audit trail page %q did not settle: %s", relPath, entry.LastError))

		return true, nil
	}

	return false, nil
}

// persistedNewest returns the newest timestamp actually archived for a page --
// the source the watermark advances from -- and whether a read-back failure
// forces the walk to halt.
//
// A page already Done before this run's Object call kept its stored events
// (Object short-circuited the re-listed set), so its newest is read back from
// the stored file; a freshly written page persisted its fresh events, so those
// are the source. Sourcing from the raw re-listed items instead would step the
// cursor past an event the settled-guard never persisted, dropping it. A Done
// page is search-layer JSON that is never evicted, so a read-back failure is not
// expected; if it happens, the page contributes nothing to the watermark and the
// walk halts, so a transient read error cannot advance the cursor.
func (c *Collector) persistedNewest(
	key, relPath string,
	fresh []*tfe.AuditTrail,
	preDone bool,
) (time.Time, bool) {
	persisted := fresh

	if preDone {
		stored, err := c.readPersistedPage(relPath)
		if err != nil {
			c.env.MarkSurfaceDropped(key, err)

			return time.Time{}, true
		}

		persisted = stored
	}

	return newestTimestamp(persisted), false
}

// pageDone reports whether the audit-trail page at relPath is already recorded
// [manifest.StatusDone], the state in which [collect.Env.Object] short-circuits
// its write.
func (c *Collector) pageDone(relPath string) bool {
	entry, ok := c.env.Entry(relPath)

	return ok && entry.Status == manifest.StatusDone
}

// readPersistedPage decodes the stored audit-trail page at relPath into its
// events. Pages are archived as plain JSON arrays of audit trails.
func (c *Collector) readPersistedPage(relPath string) ([]*tfe.AuditTrail, error) {
	//nolint:gosec // The path is composed by the store from its archive root.
	data, err := os.ReadFile(c.env.Store().AbsPath(relPath))
	if err != nil {
		return nil, fmt.Errorf("read audit trail page %q: %w", relPath, err)
	}

	var events []*tfe.AuditTrail

	err = json.Unmarshal(data, &events)
	if err != nil {
		return nil, fmt.Errorf("decode audit trail page %q: %w", relPath, err)
	}

	return events, nil
}

// listPage reads one page of audit trails created after since, routed through
// the shared governor.
func (c *Collector) listPage(ctx context.Context, since time.Time, page int) (*tfe.AuditTrailList, error) {
	var list *tfe.AuditTrailList

	err := c.env.Client().Do(ctx, func(ctx context.Context, tc *tfe.Client) error {
		var e error

		list, e = tc.AuditTrails.List(ctx, &tfe.AuditTrailListOptions{
			Since:       since,
			ListOptions: &tfe.ListOptions{PageNumber: page},
		})
		if e != nil {
			return fmt.Errorf("list audit trails: %w", e)
		}

		return nil
	})
	if err != nil {
		return nil, err //nolint:wrapcheck // Wrapped in the Do closure.
	}

	return list, nil
}
