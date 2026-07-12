package stacks

import (
	"context"
	"fmt"
	"log/slog"

	tfe "github.com/hashicorp/go-tfe"

	"go.jacobcolvin.com/hcp_archiver/collect"
	"go.jacobcolvin.com/hcp_archiver/tfeclient"
)

// name identifies this collector in progress output and logs.
const name = "stacks"

// Collector archives the stacks deployment model for one organization.
//
// Stacks are a project-scoped alternative to workspaces, so each stack nests
// under its project. The collector walks every stack's configurations (with
// their JSON schemas and diagnostics), the deployment groups beneath each
// configuration, and the runs and per-step artifacts that hang off those
// groups; it also captures each stack's named deployments and its per-generation
// states. It archives only through the [collect.Env] it is built with, so the
// per-object ledger, serialization, and rate limiting are shared with every
// other collector. Create instances with [New].
type Collector struct {
	env      *collect.Env
	log      *slog.Logger
	projects map[string]string
	org      string
}

// Option configures a [Collector] passed to [New].
//
// Options of this type:
//   - [WithLogger]
type Option func(*Collector)

// WithLogger sets the logger the collector reports list-level failures through.
// It returns an [Option].
func WithLogger(log *slog.Logger) Option {
	return func(c *Collector) {
		c.log = log
	}
}

// New creates a new [Collector] archiving org's stacks into env.
//
// The archiver constructs it only when the stacks scope toggle is on, so the
// collector need not re-check the toggle itself.
func New(env *collect.Env, org string, opts ...Option) *Collector {
	c := &Collector{
		env:      env,
		org:      org,
		log:      slog.Default(),
		projects: make(map[string]string),
	}

	for _, opt := range opts {
		opt(c)
	}

	return c
}

// Name identifies the collector for progress and logs.
func (c *Collector) Name() string {
	return name
}

// Collect archives every stack in the organization.
//
// It returns only on a context cancellation: a single missing object is recorded
// by the [collect.Env] primitives and a list-level failure for one stack is
// logged and skipped, so neither aborts the whole collector.
func (c *Collector) Collect(ctx context.Context) error {
	stacks, err := tfeclient.Paginate(ctx, c.env.Client(),
		func(ctx context.Context, tc *tfe.Client, o tfe.ListOptions) ([]*tfe.Stack, *tfe.Pagination, error) {
			l, e := tc.Stacks.List(ctx, c.org, &tfe.StackListOptions{ListOptions: o})
			if e != nil {
				return nil, nil, fmt.Errorf("list stacks: %w", e)
			}

			return l.Items, l.Pagination, nil
		},
	)
	if err != nil {
		return c.tolerate(ctx, name, err)
	}

	for _, stack := range stacks {
		err = c.collectStack(ctx, stack)
		if err != nil {
			return err
		}
	}

	return nil
}

// collectStack archives one stack: its metadata, configurations, named
// deployments, and states. Only a context cancellation propagates; a list-level
// failure within one collection is logged and skipped.
func (c *Collector) collectStack(ctx context.Context, stack *tfe.Stack) error {
	project := c.projectName(ctx, stack)

	c.env.SetTarget(project + "/" + stack.Name)

	stackFile := c.env.Store().StackFile(project, stack.Name, "stack.json")

	err := c.env.Mutable(ctx, stackFile, func(_ context.Context) (any, error) {
		return stack, nil
	})
	if err != nil {
		return wrap(err)
	}

	stackSurface := c.env.Store().StackDir(project, stack.Name)

	err = c.tolerate(ctx, stackSurface+"/configurations", c.collectConfigurations(ctx, project, stack))
	if err != nil {
		return err
	}

	err = c.tolerate(ctx, stackSurface+"/deployments", c.collectDeployments(ctx, project, stack))
	if err != nil {
		return err
	}

	return c.tolerate(ctx, stackSurface+"/states", c.collectStates(ctx, project, stack))
}

// projectName resolves the display name of a stack's project, caching each
// resolved name. A stack always carries a project relation; when the project
// has no readable name the id stands in.
//
// A read failure is a transient blip or a permission gap, not a stable answer,
// so it falls back to the id for this stack without caching it. That keeps a
// single blip on one stack from freezing the whole project's stacks under the
// id, and lets a later stack or a re-run under a broader token still resolve
// the real name. Only a project that genuinely reads back without a name caches
// the id, since that answer is stable.
func (c *Collector) projectName(ctx context.Context, stack *tfe.Stack) string {
	if stack.Project == nil || stack.Project.ID == "" {
		return "unknown-project"
	}

	id := stack.Project.ID
	if cached, ok := c.projects[id]; ok {
		return cached
	}

	var project *tfe.Project

	err := c.env.Client().Do(ctx, func(ctx context.Context, tc *tfe.Client) error {
		var e error

		project, e = tc.Projects.Read(ctx, id)

		return wrap(e)
	})
	if err != nil {
		return id
	}

	if project == nil || project.Name == "" {
		c.projects[id] = id

		return id
	}

	c.projects[id] = project.Name

	return project.Name
}

// tolerate turns a list-level failure into either a propagated cancellation or a
// logged, swallowed error so the collector keeps going on a transient blip. The
// drop is recorded under surface through [collect.Env.MarkSurfaceDropped] so the
// run still reports incomplete over the skipped collection. A nil error passes
// through unchanged.
func (c *Collector) tolerate(ctx context.Context, surface string, err error) error {
	if err == nil {
		return nil
	}

	if ctx.Err() != nil {
		return err
	}

	c.env.MarkSurfaceDropped(surface, err)

	c.log.WarnContext(ctx, "skipping stacks object after failure",
		slog.String("org", c.org),
		slog.String("surface", surface),
		slog.Any("cause", err),
	)

	return nil
}

// wrap gives a bare error from the archive engine or client a stacks-package
// frame so it is never surfaced unwrapped. A nil error passes through as nil.
func wrap(err error) error {
	if err == nil {
		return nil
	}

	return fmt.Errorf("archive stacks: %w", err)
}

// newPager adapts a single-page list call into a [collect.Pager] that reports
// whether a further page exists, the shared building block for every stack
// collection walked newest-first.
func newPager[T any](
	env *collect.Env,
	fetch func(ctx context.Context, tc *tfe.Client, o tfe.ListOptions) ([]T, *tfe.Pagination, error),
) collect.Pager[T] {
	return func(ctx context.Context, page int) ([]T, bool, error) {
		var (
			items []T
			pg    *tfe.Pagination
		)

		err := env.Client().Do(ctx, func(ctx context.Context, tc *tfe.Client) error {
			var e error

			items, pg, e = fetch(ctx, tc, tfe.ListOptions{PageNumber: page})

			return e
		})
		if err != nil {
			return nil, false, fmt.Errorf("list stack page: %w", err)
		}

		return items, pg != nil && pg.NextPage != 0, nil
	}
}
