package workspace

import (
	"context"
	"fmt"
	"io"

	"github.com/hashicorp/go-tfe"

	"go.jacobcolvin.com/hcp_archiver/collect"
	"go.jacobcolvin.com/hcp_archiver/tfeclient"
)

// collectStateVersions archives the workspace's state versions newest-first.
// Each state version is immutable once created, so the walk halts as soon as it
// reaches one already archived. The progress callback, when non-nil, is called
// with 1 after each state version is handled, including a settled one the
// primitives skip, so progress tracks the walk itself rather than only fresh
// downloads.
func (c *Collector) collectStateVersions(
	ctx context.Context,
	project string,
	ws *tfe.Workspace,
	progress func(n int),
) error {
	st := c.env.Store()
	key := st.StateVersionDir(project, ws.Name)
	wsName := ws.Name
	org := c.org

	pager := func(ctx context.Context, page int) ([]*tfe.StateVersion, bool, error) {
		var list *tfe.StateVersionList

		err := c.env.Client().Do(ctx, func(ctx context.Context, tc *tfe.Client) error {
			l, e := tc.StateVersions.List(ctx, &tfe.StateVersionListOptions{
				// Request the max page size against the metered list bucket, as
				// the runs pager does, so a deep state-version history costs the
				// fewest round-trips.
				ListOptions:  tfe.ListOptions{PageNumber: page, PageSize: tfeclient.MaxPageSize},
				Organization: org,
				Workspace:    wsName,
			})
			list = l

			if e != nil {
				return fmt.Errorf("list state versions: %w", e)
			}

			return nil
		})
		if err != nil {
			return nil, false, fmt.Errorf("list state versions page %d: %w", page, err)
		}

		return list.Items, hasNextPage(list.Pagination), nil
	}

	describe := func(sv *tfe.StateVersion) collect.Item {
		return collect.Item{
			RelPath:   st.StateVersionFile(project, wsName, sv.CreatedAt, sv.ID, "meta.json"),
			CreatedAt: sv.CreatedAt,
			Terminal:  stateVersionTerminal(sv.Status),
			Archive: func(ctx context.Context) error {
				err := c.archiveStateVersion(ctx, project, wsName, sv)
				if err == nil && progress != nil {
					progress(1)
				}

				return err
			},
		}
	}

	return wrapArchive(key, collect.Walk(ctx, c.env, key, pager, describe))
}

// archiveStateVersion archives one state version: the raw state blob, the
// JSON-format state when one is available, and a metadata sidecar carrying the
// serial, creation time, originating run, size, and VCS commit SHA.
func (c *Collector) archiveStateVersion(ctx context.Context, project, ws string, sv *tfe.StateVersion) error {
	st := c.env.Store()

	rawPath := st.StateVersionFile(project, ws, sv.CreatedAt, sv.ID, "tfstate.json")

	if sv.DownloadURL == "" {
		// A pending state version's URL is only transiently empty; recording it
		// settled would permanently lose the state once it finalizes. Record
		// nothing so a later pass re-fetches once the URL populates. A finalized or
		// discarded version's empty URL is a genuine, permanent absence.
		if stateVersionTerminal(sv.Status) {
			c.env.NotApplicable(rawPath)
		}
	} else {
		downloadURL := sv.DownloadURL

		err := c.blob(ctx, rawPath, func(ctx context.Context) (io.ReadCloser, error) {
			return c.env.Client().OpenState(ctx, downloadURL)
		})
		if err != nil {
			return err
		}
	}

	jsonPath := st.StateVersionFile(project, ws, sv.CreatedAt, sv.ID, "json")

	if sv.JSONDownloadURL == "" {
		if stateVersionTerminal(sv.Status) {
			c.env.NotApplicable(jsonPath)
		}
	} else {
		jsonURL := sv.JSONDownloadURL

		err := c.blob(ctx, jsonPath, func(ctx context.Context) (io.ReadCloser, error) {
			return c.env.Client().OpenState(ctx, jsonURL)
		})
		if err != nil {
			return err
		}
	}

	if !stateVersionTerminal(sv.Status) {
		// A pending version's meta would record status=pending with empty
		// finalized fields and then settle, so it would never be refreshed once
		// the version finalizes. Record nothing until it is terminal, matching the
		// raw and JSON blobs above, so a later walk captures the complete metadata.
		return nil
	}

	svID := sv.ID

	return objectOne(ctx, c, st.StateVersionFile(project, ws, sv.CreatedAt, sv.ID, "meta.json"),
		func(ctx context.Context, tc *tfe.Client) (*tfe.StateVersion, error) {
			return tc.StateVersions.ReadWithOptions(ctx, svID, &tfe.StateVersionReadOptions{
				Include: []tfe.StateVersionIncludeOpt{tfe.SVrun},
			})
		})
}

// stateVersionTerminal reports whether a state version has settled so it needs
// no further refresh.
//
// A pending version is non-terminal: its raw and JSON download URLs are
// transiently empty until it finalizes, so treating it as terminal would settle
// those blobs as not-applicable and permanently lose irreplaceable state once
// they populate. Marking it non-terminal keeps the collection re-walking
// (through the walk's settled machinery) until it finalizes.
//
// The polarity is an explicit allowlist: only a status positively known to be
// final is terminal, and an unrecognized status (one HashiCorp adds after this
// list was written) falls through to non-terminal. Mistaking a live status for
// terminal settles a transiently-empty blob as a permanent absence, silent and
// irreversible; mistaking a final status for live only costs re-walks until the
// list is updated. Every terminality predicate in the collectors keeps this
// polarity (see runTerminal and the stacks classifiers).
func stateVersionTerminal(status tfe.StateVersionStatus) bool {
	switch status {
	case tfe.StateVersionFinalized, tfe.StateVersionDiscarded:
		return true
	case "":
		// A server that predates the status attribute omits it. Such a server has
		// no pending state: versions exist only once uploaded, so an empty status
		// is a finalized version, and treating it as live would re-walk every
		// collection forever without ever settling its metadata.
		return true
	case tfe.StateVersionPending:
		return false
	default:
		return false
	}
}

// hasNextPage reports whether pagination points at a further page, tolerating a
// nil pagination from an endpoint that omits it.
func hasNextPage(p *tfe.Pagination) bool {
	return p != nil && p.NextPage != 0
}
