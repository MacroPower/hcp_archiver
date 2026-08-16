package audit

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	tfe "github.com/hashicorp/go-tfe"

	"go.jacobcolvin.com/hcp_archiver/collect"
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
//
// Those same persisted events are also what a page still to be written must be
// filtered against. Events arriving between runs shift the newest-first listing
// back a slot, so a page the prior run never settled re-lists events its settled
// sibling already holds; writing them verbatim would archive the same event in
// two page files under one cursor. The walk therefore carries the ids of
// everything persisted for this cursor so far and treats a re-listed one as
// already archived. Accumulating them page by page is enough to cover the whole
// cursor: a shift can only push an event onto a later page, and every page is
// visited in order, so the page holding an event is always reached before the
// page that re-lists it.
func (c *Collector) collectTrails(ctx context.Context) error {
	st := c.env.Store()
	key := st.AuditTrailDir()
	col := c.env.Collection(key)
	since := col.HighWaterMark()

	var newest time.Time

	archived := archivedIDs{}

	for page := 1; ; {
		list, listErr := c.listPage(ctx, since, page)

		// Keep only events strictly newer than the watermark and not already
		// persisted under this cursor, dropping both the events the whole-second
		// wire cursor re-lists when a walk resumes from a sub-second watermark and
		// the ones a shifted page carries over from a settled sibling.
		var fresh []*tfe.AuditTrail

		if listErr == nil {
			fresh = eventsAfter(list.Items, since, archived)
		}

		relPath := st.AuditTrailFile(pageName(since, page))

		// Whether Object will persist fresh or short-circuit on an already-settled
		// page is fixed by the ledger before the call decides it.
		settled := c.pageShortCircuited(relPath)

		// Write the page unless it lists cleanly but carries only already-archived
		// events (an empty page among them). Skipping such a page avoids duplicating
		// events under a fresh name and settling an empty page under the unchanged
		// cursor (a later run's events would then be skipped as already done and
		// lost). The trail's page order is unspecified, so an empty page never ends
		// the walk: only the pagination reporting no next page does, and a non-empty
		// later page may still follow an empty one.
		if listErr != nil || len(fresh) > 0 {
			halt, err := c.archiveTrailPage(ctx, relPath, fresh, listErr)
			if err != nil {
				return err
			}

			if halt {
				return nil
			}
		}

		// A page already settled under this cursor is accounted for even when the
		// filter left it with nothing fresh: its stored events are the ones a
		// shifted later page re-lists, so they must join the archived set, and they
		// are persisted, so they are a valid source for the watermark.
		if settled || len(fresh) > 0 {
			persisted, halt := c.persistedEvents(key, relPath, fresh, settled)
			if halt {
				return nil
			}

			archived.record(persisted)

			if t := newestTimestamp(persisted); t.After(newest) {
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
		col.AdvanceHighWaterMark(newest)
	}

	return nil
}

// archiveTrailPage records one audit-trail page's fresh events and reports
// whether the walk must halt without advancing the watermark. A list error the
// walk cannot page past, and a write that did not settle, both halt so the next
// run retries the page from the same cursor; a halt also records the trail as a
// dropped surface, since the pages past it were never reached and nothing else
// marks the gap. It is called only for a page carrying fresh events or a list
// error; a clean page of only already-archived events is skipped by the caller.
func (c *Collector) archiveTrailPage(
	ctx context.Context,
	relPath string,
	fresh []*tfe.AuditTrail,
	listErr error,
) (bool, error) {
	// A page-enumeration failure is not the page object's absence. The page is a
	// synthetic slot keyed on the walk's cursor, not a resource that can 404, so
	// routing a list error through Object would let a terminal one (an infra 404,
	// or org auditing toggled off then on) settle the slot absent. Object then
	// short-circuits that slot on every later run, so the real events a later list
	// returns for it are never written and the walk wedges on the never-created
	// file. Record the fetch failure as a dropped surface and halt with the slot
	// left unsettled, so the next run re-lists and writes it.
	if listErr != nil {
		// A cancellation must propagate, or the walk returns nil on a real
		// cancellation and the run logs a canceled org as a clean finish.
		ctxErr := ctx.Err()
		if ctxErr != nil {
			return false, fmt.Errorf("archive audit trail page: %w", ctxErr)
		}

		// The walk cannot paginate past an unreadable page, so halt without
		// advancing the watermark; the next run retries from the same cursor.
		c.env.MarkSurfaceDropped(c.env.Store().AuditTrailDir(), listErr)

		return true, nil
	}

	err := c.env.Object(ctx, relPath, func(context.Context) (any, error) {
		return fresh, nil
	})
	if err != nil {
		return false, fmt.Errorf("archive audit trail page: %w", err)
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

// persistedEvents returns the events actually archived for a page (the source
// both the watermark and the walk's archived-id set draw from) and whether a
// read-back failure forces the walk to halt.
//
// A page Object short-circuited this run kept its stored events (the re-listed
// set was never written), so they are read back from the stored file; a freshly
// written page persisted its fresh events, so those are the source. Sourcing
// from the raw re-listed items instead would step the cursor past an event the
// short-circuited write never persisted, dropping it. A short-circuited page is
// Done (a list error no longer settles a page absent; see archiveTrailPage),
// search-layer JSON that is never evicted, so a read-back failure is not
// expected; if it happens, the page contributes nothing to the watermark and the
// walk halts, so a transient read error cannot advance the cursor.
func (c *Collector) persistedEvents(
	key, relPath string,
	fresh []*tfe.AuditTrail,
	settled bool,
) ([]*tfe.AuditTrail, bool) {
	if !settled {
		return fresh, false
	}

	stored, err := c.readPersistedPage(relPath)
	if err != nil {
		c.env.MarkSurfaceDropped(key, err)

		return nil, true
	}

	return stored, false
}

// pageShortCircuited reports whether [collect.Env.Object] will skip its write
// for the audit-trail page at relPath: it has a ledger entry that is settled and
// not up for a re-fetch, so the call leaves the stored file untouched. This is
// broader than StatusDone: an Absent page (a prior terminal list error) is
// settled too, and Object short-circuits it identically. That makes it exactly
// the condition under which the page's contribution must be read back from the
// stored file rather than taken from the re-listed events the write never
// persisted.
func (c *Collector) pageShortCircuited(relPath string) bool {
	_, ok := c.env.Entry(relPath)

	return ok && !c.env.ShouldFetch(relPath)
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
