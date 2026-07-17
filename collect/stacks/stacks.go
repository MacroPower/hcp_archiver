package stacks

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"golang.org/x/sync/errgroup"

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
// other collector. Stacks archive concurrently, and each stack's collections
// share its wall-clock; the client's request gate bounds the run's real
// parallelism. Create instances with [New].
type Collector struct {
	env *collect.Env
	log *slog.Logger

	// Cache of resolved project display names by id. Guarded by projectsMu:
	// stacks archive concurrently, and many stacks share a project.
	projects map[string]string

	org string

	projectsMu sync.Mutex
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

	// The stacks archive concurrently, mirroring the workspace fan-out: each
	// goroutine is only a coordinator, every request it causes takes a slot from
	// the client's gate, so the pool's live size bounds the real parallelism,
	// and the fan-out is capped at the environment's ceiling. No single stack
	// is the target then, so clear it for the whole phase. A stack goroutine
	// returns non-nil only on a cancellation, which cancels the group.
	c.env.SetTarget("")

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(c.env.Concurrency())

	for _, stack := range stacks {
		g.Go(func() error {
			return c.collectStack(gctx, stack)
		})
	}

	return g.Wait() //nolint:wrapcheck // Stack errors already carry their context.
}

// collectStack archives one stack: its metadata, configurations, named
// deployments, and states. Only a context cancellation propagates; a list-level
// failure within one collection is logged and skipped.
func (c *Collector) collectStack(ctx context.Context, stack *tfe.Stack) error {
	project := c.projectName(ctx, stack)

	// Bind the name-keyed directory to this stack's id before the first write;
	// a failed claim skips the stack with its surface dropped (recorded by
	// ClaimDir), so a reused name never overwrites the deleted stack's archive.
	renamedFrom, err := c.env.ClaimDir(c.env.Store().StackDir(project, stack.Name), stack.ID)
	if err != nil {
		c.log.ErrorContext(ctx, "stack directory not claimed; skipping",
			slog.String("stack", stack.Name),
			slog.Any("error", err),
		)

		return nil
	}

	if renamedFrom != "" {
		c.log.WarnContext(ctx, "stack was renamed; its prior archive is kept",
			slog.String("stack", stack.Name),
			slog.String("previous_name", renamedFrom),
		)
	}

	stackFile := c.env.Store().StackFile(project, stack.Name, "stack.json")

	err = c.env.Mutable(ctx, stackFile, func(_ context.Context) (any, error) {
		return stack, nil
	})
	if err != nil {
		return wrap(err)
	}

	stackSurface := c.env.Store().StackDir(project, stack.Name)

	// The three collections are independent and write disjoint paths, so they
	// share the stack's wall-clock, mirroring the workspace collector's run and
	// state-version walks. Each is wrapped in tolerate, so only a cancellation
	// cancels the group.
	g, gctx := errgroup.WithContext(ctx)

	g.Go(func() error {
		return c.tolerate(gctx, stackSurface+"/configurations", c.collectConfigurations(gctx, project, stack))
	})

	g.Go(func() error {
		return c.tolerate(gctx, stackSurface+"/deployments", c.collectDeployments(gctx, project, stack))
	})

	g.Go(func() error {
		return c.tolerate(gctx, stackSurface+"/states", c.collectStates(gctx, project, stack))
	})

	return g.Wait() //nolint:wrapcheck // Collection errors already carry their context.
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
// the id, since that answer is stable. Concurrent stacks that miss the cache on
// the same project may each read it; both cache the same stable answer, so the
// duplicate read is harmless.
func (c *Collector) projectName(ctx context.Context, stack *tfe.Stack) string {
	if stack.Project == nil || stack.Project.ID == "" {
		return "unknown-project"
	}

	id := stack.Project.ID
	if cached, ok := c.cachedProjectName(id); ok {
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
		c.cacheProjectName(id, id)

		return id
	}

	c.cacheProjectName(id, project.Name)

	return project.Name
}

// cachedProjectName reads the resolved display name for a project id, reporting
// whether one is cached.
func (c *Collector) cachedProjectName(id string) (string, bool) {
	c.projectsMu.Lock()
	defer c.projectsMu.Unlock()

	name, ok := c.projects[id]

	return name, ok
}

// cacheProjectName records the resolved display name for a project id.
func (c *Collector) cacheProjectName(id, name string) {
	c.projectsMu.Lock()
	defer c.projectsMu.Unlock()

	c.projects[id] = name
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

// listingLeaf names the ledger marker recording a child enumeration's outcome,
// placed inside the directory the enumeration fills. The marker is a ledger
// entry only — no file is ever written at the path — and cannot collide with a
// real archived object, whose directories key on API-prefixed ids.
const listingLeaf = "listing"

// tolerateListing records the outcome of a child enumeration that runs beneath
// an element a [collect.Walk] can freeze (a configuration's deployment groups,
// a group's runs walk, a terminal run's steps) and then defers to
// [Collector.tolerate]. The dropped-surface record tolerate leaves is
// run-scoped, and a failed enumeration writes no per-object entries, so on its
// own the enclosing walks would settle over the gap and their early stop would
// never revisit the element: the missing children would be lost silently and
// permanently. The marker entry closes that hole, mirroring how the workspace
// collector persists run-child listing failures. A failure records marker
// errored ([collect.Env.Errored]), which [manifest.Ledger.HasUnsettledUnder]
// finds under the walks' archive prefixes, keeping them open until a later
// pass re-runs the enumeration; a success settles marker as a not-applicable
// gap (no file exists at the path) so the walks can close once every child
// lands. A cancellation propagates unrecorded, matching the archive
// primitives.
func (c *Collector) tolerateListing(ctx context.Context, marker string, err error) error {
	if err == nil {
		if c.env.ShouldFetch(marker) {
			c.env.NotApplicable(marker)
		}

		return nil
	}

	if ctx.Err() == nil {
		c.env.Errored(marker, err)
	}

	return c.tolerate(ctx, marker, err)
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
