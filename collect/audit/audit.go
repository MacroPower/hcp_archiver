package audit

import (
	"context"
	"fmt"
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
func (c *Collector) collectTrails(ctx context.Context) error {
	st := c.env.Store()
	key := st.AuditTrailDir()
	since := c.env.HighWaterMark(key)

	var newest time.Time

	for page := 1; ; {
		list, listErr := c.listPage(ctx, since, page)
		relPath := st.AuditTrailFile(pageName(since, page))

		err := c.env.Object(ctx, relPath, func(context.Context) (any, error) {
			if listErr != nil {
				return nil, listErr
			}

			return list.Items, nil
		})
		if err != nil {
			return fmt.Errorf("archive audit trail page: %w", err)
		}

		if listErr != nil {
			// A cancellation must propagate even when Object short-circuited on an
			// already-settled page and so never observed it, or the walk returns nil
			// on a real cancellation and the run logs a canceled org as a clean
			// finish.
			ctxErr := ctx.Err()
			if ctxErr != nil {
				return fmt.Errorf("archive audit trail page: %w", ctxErr)
			}

			// The page fetch is recorded by Object; the walk cannot paginate past
			// an unreadable page, so stop without advancing the watermark.
			return nil //nolint:nilerr // The page error is recorded, not fatal.
		}

		if t := newestTimestamp(list.Items); t.After(newest) {
			newest = t
		}

		p := list.AuditTrailPagination
		if len(list.Items) == 0 || p == nil || p.NextPage == 0 {
			break
		}

		page = p.NextPage
	}

	if !newest.IsZero() {
		c.env.AdvanceHighWaterMark(key, newest)
	}

	return nil
}

// listPage reads one page of audit trails created after since, routed through
// the shared limiter.
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
