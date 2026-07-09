package workspace

import (
	"context"
	"fmt"

	"github.com/hashicorp/go-tfe"

	"go.jacobcolvin.com/hcp_archiver/collect"
)

// collectStateVersions archives the workspace's state versions newest-first.
// Each state version is immutable once created, so the walk halts as soon as it
// reaches one already archived.
func (c *Collector) collectStateVersions(ctx context.Context, project string, ws *tfe.Workspace) error {
	st := c.env.Store()
	key := st.StateVersionDir(project, ws.Name)
	wsName := ws.Name
	org := c.org

	pager := func(ctx context.Context, page int) ([]*tfe.StateVersion, bool, error) {
		var list *tfe.StateVersionList

		err := c.env.Client().Do(ctx, func(ctx context.Context, tc *tfe.Client) error {
			l, e := tc.StateVersions.List(ctx, &tfe.StateVersionListOptions{
				ListOptions:  tfe.ListOptions{PageNumber: page},
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
			Terminal:  true,
			Archive: func(ctx context.Context) error {
				return c.archiveStateVersion(ctx, project, wsName, sv)
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
		c.env.NotApplicable(rawPath)
	} else {
		downloadURL := sv.DownloadURL

		err := c.bytes(ctx, rawPath, func(ctx context.Context) ([]byte, error) {
			return c.env.Client().DownloadState(ctx, downloadURL)
		})
		if err != nil {
			return err
		}
	}

	jsonPath := st.StateVersionFile(project, ws, sv.CreatedAt, sv.ID, "json")

	if sv.JSONDownloadURL == "" {
		c.env.NotApplicable(jsonPath)
	} else {
		jsonURL := sv.JSONDownloadURL

		err := c.bytes(ctx, jsonPath, func(ctx context.Context) ([]byte, error) {
			return c.env.Client().DownloadState(ctx, jsonURL)
		})
		if err != nil {
			return err
		}
	}

	svID := sv.ID

	return objectOne(ctx, c, st.StateVersionFile(project, ws, sv.CreatedAt, sv.ID, "meta.json"),
		func(ctx context.Context, tc *tfe.Client) (*tfe.StateVersion, error) {
			return tc.StateVersions.ReadWithOptions(ctx, svID, &tfe.StateVersionReadOptions{
				Include: []tfe.StateVersionIncludeOpt{tfe.SVrun},
			})
		})
}

// hasNextPage reports whether pagination points at a further page, tolerating a
// nil pagination from an endpoint that omits it.
func hasNextPage(p *tfe.Pagination) bool {
	return p != nil && p.NextPage != 0
}
