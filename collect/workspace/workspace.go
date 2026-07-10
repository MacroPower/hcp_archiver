package workspace

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/hashicorp/go-tfe"
)

// CollectWorkspace archives one workspace: its settings and adjacent metadata,
// then its append-mostly state versions and runs. The projectName argument is
// the name of the workspace's project, resolved by the orchestrator, so the
// collector can build project-scoped paths.
//
// The progress callback, when non-nil, is called with 1 after each run the walk
// handles, so the orchestrator can move its unit-progress accounting while a
// large workspace is still in flight rather than only when it finishes. The
// count of callbacks is the walk's own, not the workspace's advertised run
// count: a walk that stops early on settled history reports fewer, and a run
// listing that outgrows the advertised count (speculative runs, a stale
// counter) reports more, so the orchestrator reconciles against its own budget.
//
// It does not touch the progress target: workspaces archive concurrently, so
// no single name is the target, and progress reporting names the in-flight
// workspaces through their tasks instead.
//
// It returns only on a context cancellation; a single missing or failed object
// is recorded in the ledger and does not abort the collector.
func (c *Collector) CollectWorkspace(
	ctx context.Context,
	projectName string,
	ws *tfe.Workspace,
	progress func(n int),
) error {
	err := c.collectWorkspaceSettings(ctx, projectName, ws)
	if err != nil {
		return err
	}

	err = c.collectStateVersions(ctx, projectName, ws)
	if err != nil {
		return err
	}

	return c.collectRuns(ctx, projectName, ws, progress)
}

// collectWorkspaceSettings archives the workspace record and its adjacent
// mutable metadata (variables, readme, tags, team access, notifications, run
// triggers, run tasks, and, only when global remote state is off, the remote
// state consumers).
func (c *Collector) collectWorkspaceSettings(ctx context.Context, project string, ws *tfe.Workspace) error {
	st := c.env.Store()
	wsID := ws.ID

	err := mutableOne(ctx, c, st.WorkspaceFile(project, ws.Name, "workspace.json"),
		func(ctx context.Context, tc *tfe.Client) (*tfe.Workspace, error) {
			return tc.Workspaces.ReadByIDWithOptions(ctx, wsID, &tfe.WorkspaceReadOptions{
				Include: []tfe.WSIncludeOpt{tfe.WSProject},
			})
		})
	if err != nil {
		return err
	}

	err = listMutable(ctx, c, st.WorkspaceFile(project, ws.Name, "variables.json"),
		func(ctx context.Context, tc *tfe.Client, o tfe.ListOptions) ([]*tfe.Variable, *tfe.Pagination, error) {
			l, e := tc.Variables.ListAll(ctx, wsID, &tfe.VariableListOptions{ListOptions: o})
			if e != nil {
				return nil, nil, fmt.Errorf("list all variables: %w", e)
			}

			return l.Items, l.Pagination, nil
		})
	if err != nil {
		return err
	}

	err = c.collectReadme(ctx, project, ws)
	if err != nil {
		return err
	}

	err = c.collectTags(ctx, project, ws)
	if err != nil {
		return err
	}

	err = c.collectWorkspaceWiring(ctx, project, ws)
	if err != nil {
		return err
	}

	return c.collectRemoteStateConsumers(ctx, project, ws)
}

// collectReadme archives the workspace readme as raw markdown. A workspace with
// no readme yields an empty reader, which records an empty file rather than a
// gap.
func (c *Collector) collectReadme(ctx context.Context, project string, ws *tfe.Workspace) error {
	st := c.env.Store()
	wsID := ws.ID

	relPath := st.WorkspaceFile(project, ws.Name, "readme.md")

	return wrapArchive(relPath, c.env.Blob(ctx, relPath,
		func(ctx context.Context) (io.Reader, error) {
			var r io.Reader

			err := c.env.Client().Do(ctx, func(ctx context.Context, tc *tfe.Client) error {
				rr, e := tc.Workspaces.Readme(ctx, wsID)
				r = rr

				if e != nil {
					return fmt.Errorf("read readme: %w", e)
				}

				return nil
			})
			if err != nil {
				return nil, fmt.Errorf("read readme: %w", err)
			}

			if r == nil {
				return strings.NewReader(""), nil
			}

			return r, nil
		}))
}

// collectTags archives the workspace's own and effective tag bindings together
// as one mutable file.
func (c *Collector) collectTags(ctx context.Context, project string, ws *tfe.Workspace) error {
	st := c.env.Store()
	wsID := ws.ID

	relPath := st.WorkspaceFile(project, ws.Name, "tags.json")

	return wrapArchive(relPath, c.env.Mutable(ctx, relPath,
		func(ctx context.Context) (any, error) {
			var out tagBindings

			err := c.env.Client().Do(ctx, func(ctx context.Context, tc *tfe.Client) error {
				bindings, e := tc.Workspaces.ListTagBindings(ctx, wsID)
				if e != nil {
					return fmt.Errorf("list tag bindings: %w", e)
				}

				effective, e := tc.Workspaces.ListEffectiveTagBindings(ctx, wsID)
				if e != nil {
					return fmt.Errorf("list effective tag bindings: %w", e)
				}

				out = tagBindings{Bindings: bindings, Effective: effective}

				return nil
			})
			if err != nil {
				return nil, fmt.Errorf("read tag bindings: %w", err)
			}

			return out, nil
		}))
}

// tagBindings pairs a workspace's own tag bindings with its effective bindings
// (which include those inherited from its project) for the tags file.
type tagBindings struct {
	Bindings  []*tfe.TagBinding          `json:"tag-bindings"`
	Effective []*tfe.EffectiveTagBinding `json:"effective-tag-bindings"`
}

// collectWorkspaceWiring archives the workspace's team access, notification
// configurations, inbound run triggers, and run-task bindings.
func (c *Collector) collectWorkspaceWiring(ctx context.Context, project string, ws *tfe.Workspace) error {
	st := c.env.Store()
	wsID := ws.ID

	err := listMutable(ctx, c, st.WorkspaceFile(project, ws.Name, "team-access.json"),
		func(ctx context.Context, tc *tfe.Client, o tfe.ListOptions) ([]*tfe.TeamAccess, *tfe.Pagination, error) {
			l, e := tc.TeamAccess.List(ctx, &tfe.TeamAccessListOptions{ListOptions: o, WorkspaceID: wsID})
			if e != nil {
				return nil, nil, fmt.Errorf("list team access: %w", e)
			}

			return l.Items, l.Pagination, nil
		})
	if err != nil {
		return err
	}

	err = listMutable(
		ctx,
		c,
		st.WorkspaceFile(project, ws.Name, "notification-configs.json"),
		func(ctx context.Context, tc *tfe.Client, o tfe.ListOptions) ([]*tfe.NotificationConfiguration, *tfe.Pagination, error) {
			l, e := tc.NotificationConfigurations.List(
				ctx,
				wsID,
				&tfe.NotificationConfigurationListOptions{ListOptions: o},
			)
			if e != nil {
				return nil, nil, fmt.Errorf("list notification configs: %w", e)
			}

			return l.Items, l.Pagination, nil
		},
	)
	if err != nil {
		return err
	}

	err = listMutable(ctx, c, st.WorkspaceFile(project, ws.Name, "run-triggers.json"),
		func(ctx context.Context, tc *tfe.Client, o tfe.ListOptions) ([]*tfe.RunTrigger, *tfe.Pagination, error) {
			l, e := tc.RunTriggers.List(ctx, wsID, &tfe.RunTriggerListOptions{
				ListOptions:    o,
				RunTriggerType: tfe.RunTriggerInbound,
			})
			if e != nil {
				return nil, nil, fmt.Errorf("list run triggers: %w", e)
			}

			return l.Items, l.Pagination, nil
		})
	if err != nil {
		return err
	}

	return listMutable(ctx, c, st.WorkspaceFile(project, ws.Name, "run-tasks.json"),
		func(ctx context.Context, tc *tfe.Client, o tfe.ListOptions) ([]*tfe.WorkspaceRunTask, *tfe.Pagination, error) {
			l, e := tc.WorkspaceRunTasks.List(ctx, wsID, &tfe.WorkspaceRunTaskListOptions{ListOptions: o})
			if e != nil {
				return nil, nil, fmt.Errorf("list run tasks: %w", e)
			}

			return l.Items, l.Pagination, nil
		})
}

// collectRemoteStateConsumers archives the workspace's remote-state consumers,
// which are meaningful only when global remote state is disabled. When it is
// enabled the file is recorded not-applicable so a re-run does not treat its
// absence as a gap.
func (c *Collector) collectRemoteStateConsumers(ctx context.Context, project string, ws *tfe.Workspace) error {
	st := c.env.Store()
	relPath := st.WorkspaceFile(project, ws.Name, "remote-state-consumers.json")

	if ws.GlobalRemoteState {
		c.env.NotApplicable(relPath)

		return nil
	}

	wsID := ws.ID

	return listMutable(ctx, c, relPath,
		func(ctx context.Context, tc *tfe.Client, o tfe.ListOptions) ([]*tfe.Workspace, *tfe.Pagination, error) {
			l, e := tc.Workspaces.ListRemoteStateConsumers(
				ctx,
				wsID,
				&tfe.RemoteStateConsumersListOptions{ListOptions: o},
			)
			if e != nil {
				return nil, nil, fmt.Errorf("list remote state consumers: %w", e)
			}

			return l.Items, l.Pagination, nil
		})
}
