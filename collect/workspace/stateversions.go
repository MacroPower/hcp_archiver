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

	// A state version can be deleted through the API while the walk is between
	// pages, which shifts the listing under a page-number pager; the stable pager
	// re-lists rather than let the shift skip a version (see [stablePager]).
	pager := newStablePager(func(sv *tfe.StateVersion) string { return sv.ID },
		func(ctx context.Context, page int) ([]*tfe.StateVersion, *tfe.Pagination, error) {
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
				return nil, nil, fmt.Errorf("list state versions page %d: %w", page, err)
			}

			return list.Items, list.Pagination, nil
		})

	describe := func(sv *tfe.StateVersion) collect.Item {
		return collect.Item{
			RelPath:   st.StateVersionFile(project, wsName, sv.CreatedAt, sv.ID, "meta.json"),
			CreatedAt: sv.CreatedAt,
			Terminal:  tfeclient.StateVersionTerminal(sv.Status),
			Archive: func(ctx context.Context) error {
				err := c.archiveStateVersion(ctx, project, wsName, sv)
				if err == nil && progress != nil {
					progress(1)
				}

				return err
			},
		}
	}

	return wrapArchive(key, collect.Walk(ctx, c.env, c.env.Collection(key), pager.page, describe))
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
		if tfeclient.StateVersionTerminal(sv.Status) {
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
		if tfeclient.StateVersionTerminal(sv.Status) {
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

	if !tfeclient.StateVersionTerminal(sv.Status) {
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
