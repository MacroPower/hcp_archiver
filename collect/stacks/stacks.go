package stacks

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"golang.org/x/sync/errgroup"

	tfe "github.com/hashicorp/go-tfe/v2"

	"go.jacobcolvin.com/hcp_archiver/collect"
	"go.jacobcolvin.com/hcp_archiver/manifest"
	"go.jacobcolvin.com/hcp_archiver/namefilter"
	"go.jacobcolvin.com/hcp_archiver/tfeclient"
)

// name identifies this collector in progress output and logs.
const name = "stacks"

// ErrProjectUnresolved reports that a stack's project name did not read back on
// an answer a later run is bound to repeat, so the stack has no directory to
// archive under this run. A stack's whole archive path hangs off its project's
// display name, and only a stable stand-in (a stack naming no project, a project
// that reads back nameless, a project that is gone) may key that path; a blip, a
// rate limit, or a scope gap a broader token closes leaves the stack skipped
// with its surface dropped instead.
var ErrProjectUnresolved = errors.New("project name unresolved")

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

	// Cache of project lookups by id. Guarded by projectsMu: stacks archive
	// concurrently, and many stacks share a project.
	projects map[string]resolvedProject

	// Allow-list of project names, narrowing this collector to the stacks nested
	// under them. A nil filter admits every project.
	projectFilter namefilter.Filter

	org string

	projectsMu sync.Mutex
}

// resolvedProject is the outcome of a project-name lookup: the segment a
// stack's archive path nests under, and whether that segment is the project's
// display name. When no name came back a stand-in fills the segment (the
// project id, or a sentinel when the stack names no project), which the archive
// tolerates as a path segment but a name filter never matches.
type resolvedProject struct {
	name  string
	named bool
}

// Option configures a [Collector] passed to [New].
//
// Options of this type:
//   - [WithLogger]
//   - [WithProjects]
type Option func(*Collector)

// WithLogger sets the logger the collector reports list-level failures through.
// It returns an [Option].
func WithLogger(log *slog.Logger) Option {
	return func(c *Collector) {
		c.log = log
	}
}

// WithProjects limits the collector to stacks whose project the names admit,
// matching on the project name the stack's archive path is keyed on. An empty
// list archives every stack in the organization. It returns an [Option].
func WithProjects(names []string) Option {
	return func(c *Collector) {
		c.projectFilter = namefilter.New(names)
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
		projects: make(map[string]resolvedProject),
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

// Collect archives every stack in the organization whose project the filter
// admits.
//
// It returns only on a context cancellation: a single missing object is recorded
// by the [collect.Env] primitives and a list-level failure for one stack is
// logged and skipped, so neither aborts the whole collector.
func (c *Collector) Collect(ctx context.Context) error {
	// The listing stays organization-wide even under a filter. The API narrows a
	// stack listing by project id, but the filter names projects, and a stack's
	// project name is known only once the project behind it reads back, so the
	// filter applies per stack rather than narrowing the listing.
	stacks, err := tfeclient.Paginate(
		ctx, c.env.Client(),
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
// deployments, and states. A stack the project filter excludes, or one whose
// project name did not resolve, is skipped whole. Only a context cancellation
// propagates; a list-level failure within one collection is logged and skipped.
func (c *Collector) collectStack(ctx context.Context, stack *tfe.Stack) error {
	resolved, err := c.resolveProject(ctx, stack)
	if err != nil {
		// The stack's entire archive path hangs off its project's name, so a
		// name that did not read back leaves nothing to key it on. Archiving
		// under the id anyway is worse than skipping: the next run resolves the
		// real name and re-archives the stack under it, while the rename
		// detection behind ClaimDir compares only siblings under one parent, so
		// the id-keyed tree is never stamped as superseded and stays behind
		// looking live while it silently goes stale. Skipping records the
		// surface instead, which marks the run incomplete and leaves the stack
		// to a later pass that can name its project.
		return c.tolerate(ctx, c.env.Store().StackDir(resolved.name, stack.Name), err)
	}

	project := resolved.name

	// Skip ahead of the claim below. An excluded stack then costs nothing past
	// the project read behind it, which the cache normally shares across all of
	// that project's stacks. It also leaves no project directory behind: ClaimDir
	// would create the stack's own directory to hold its identity sidecar, and
	// the project directory appearing around it reads back as a project with no
	// identity of its own and no workspaces.
	if !c.projectFilter.Admits(project) {
		// An exclusion the filter never really judged is a scope gap, not a
		// genuine exclusion: the project carries no name of its own (the stack
		// names no project, or the project reads back nameless or gone), so the
		// filter compared against a stand-in and an in-scope stack may have been
		// dropped. Recording the surface makes the run report incomplete rather
		// than closing clean over it.
		if !resolved.named {
			c.env.MarkSurfaceDropped(c.env.Store().StackDir(project, stack.Name),
				fmt.Errorf("stack %q: project %s name unresolved, so the project filter cannot judge it",
					stack.Name, project))
			c.log.WarnContext(
				ctx, "stack project name unresolved; excluded by the project filter",
				slog.String("stack", stack.Name),
				slog.String("project", project),
			)
		}

		return nil
	}

	// Bind the name-keyed directory to this stack's id before the first write;
	// a failed claim skips the stack with its surface dropped (recorded by
	// ClaimDir), so a reused name never overwrites the deleted stack's archive.
	renamedFrom, err := c.env.ClaimDir(c.env.Store().StackDir(project, stack.Name), stack.ID)
	if err != nil {
		c.log.ErrorContext(
			ctx, "stack directory not claimed; skipping",
			slog.String("stack", stack.Name),
			slog.Any("error", err),
		)

		return nil
	}

	if renamedFrom != "" {
		c.log.WarnContext(
			ctx, "stack was renamed; its prior archive is kept",
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

// resolveProject resolves the display name of a stack's project, caching every
// stable answer. A stack normally carries a project relation; without one the
// sentinel "unknown-project" fills the segment, and when the project reads back
// with no name, or is gone, the id does. Either way the returned
// [resolvedProject] marks the segment unnamed, and the answer is one every later
// run repeats, so the segment it keys is stable across runs.
//
// A read that fails on anything else is not an answer at all: a blip, a rate
// limit, or a scope gap a broader token closes all read back differently later,
// so nothing is cached and [ErrProjectUnresolved] is returned rather than a
// segment the next run would contradict. The returned [resolvedProject] still
// carries the id, for naming the stack in a report, but never for keying an
// archive path.
//
// Concurrent stacks that miss the cache on the same project may each read it;
// both cache the same stable answer, so the duplicate read is harmless.
func (c *Collector) resolveProject(ctx context.Context, stack *tfe.Stack) (resolvedProject, error) {
	if stack.Project == nil || stack.Project.ID == "" {
		return resolvedProject{name: "unknown-project"}, nil
	}

	id := stack.Project.ID
	if cached, ok := c.cachedProject(id); ok {
		return cached, nil
	}

	var project *tfe.Project

	err := c.env.Client().Do(ctx, func(ctx context.Context, tc *tfe.Client) error {
		var e error

		project, e = tc.Projects.Read(ctx, id)

		return wrap(e)
	})
	if err != nil {
		if !tfeclient.IsTerminal(err) {
			return resolvedProject{name: id}, fmt.Errorf("%w: project %s: %w", ErrProjectUnresolved, id, err)
		}

		// A project that is gone answers as stably as one that reads back
		// nameless: no later run resolves a name for it, so the id caches and
		// stands in for the segment, and the stack keeps archiving where it
		// always has.
		c.cacheProject(id, resolvedProject{name: id})

		//nolint:nilerr // A terminal read answers stably; the id stands in.
		return resolvedProject{name: id}, nil
	}

	if project == nil || project.Name == "" {
		c.cacheProject(id, resolvedProject{name: id})

		return resolvedProject{name: id}, nil
	}

	resolved := resolvedProject{name: project.Name, named: true}
	c.cacheProject(id, resolved)

	return resolved, nil
}

// cachedProject reads a project id's cached lookup, reporting whether one is
// cached.
func (c *Collector) cachedProject(id string) (resolvedProject, bool) {
	c.projectsMu.Lock()
	defer c.projectsMu.Unlock()

	p, ok := c.projects[id]

	return p, ok
}

// cacheProject records a project id's lookup.
func (c *Collector) cacheProject(id string, p resolvedProject) {
	c.projectsMu.Lock()
	defer c.projectsMu.Unlock()

	c.projects[id] = p
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

	c.log.WarnContext(
		ctx, "skipping stacks object after failure",
		slog.String("org", c.org),
		slog.String("surface", surface),
		slog.Any("cause", err),
	)

	return nil
}

// listingLeaf names the obligation marker recording a child enumeration's
// outcome, placed inside the directory the enumeration fills. The marker is a
// ledger entry only — no file is ever written at the path — and cannot collide
// with a real archived object, whose directories key on API-prefixed ids.
const listingLeaf = "listing"

// tolerateEnumeration runs a child enumeration under an obligation marker
// (see [manifest.Obligation]) and defers a failure to [Collector.tolerate].
// The enumerations this guards run beneath elements a [collect.Walk] can
// freeze (a configuration's deployment groups, a terminal run's steps), where
// a run-scoped dropped surface alone would let the enclosing walks settle
// over the gap and never revisit the element. The marker opens before the
// enumeration, so even a crash mid-listing leaves a pending record the walks'
// gates find; a failure records it failed so a later pass retries; a success
// settles it. A cancellation propagates with the marker left open, the
// fail-safe direction.
func (c *Collector) tolerateEnumeration(
	ctx context.Context,
	ob *manifest.Obligation,
	enumerate func(context.Context) error,
) error {
	ob.Open()

	err := enumerate(ctx)
	if err == nil {
		ob.Settle()

		return nil
	}

	if ctx.Err() == nil {
		c.env.FailObligation(ob, err)
	}

	return c.tolerate(ctx, ob.Key(), err)
}

// tolerateWalk runs a nested [collect.Walk] under an obligation marker and
// settles the marker only when the nested collection itself settled. A nested
// walk that saw a still-running element (or was interrupted) withholds its
// own settlement, but that flag is invisible to the enclosing walk's entry
// gate — a run recorded done while in flight leaves nothing unsettled for
// [manifest.Collection.HasUnsettled] to find — so without the marker a
// terminal configuration could freeze over a deployment run still executing
// beneath it, permanently stranding the run's final state and steps. The
// pending marker carries the nested walk's openness into an entry both
// enclosing gates scan, and clears once a later pass finds the nested
// collection settled.
func (c *Collector) tolerateWalk(
	ctx context.Context,
	ob *manifest.Obligation,
	col *manifest.Collection,
	walk func(context.Context) error,
) error {
	ob.Open()

	err := walk(ctx)
	if err == nil {
		if col.Settled() {
			ob.Settle()
		}

		return nil
	}

	if ctx.Err() == nil {
		c.env.FailObligation(ob, err)
	}

	return c.tolerate(ctx, ob.Key(), err)
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
