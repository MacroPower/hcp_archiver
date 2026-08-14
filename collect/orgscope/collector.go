package orgscope

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/hashicorp/go-tfe"
	"golang.org/x/sync/errgroup"

	"go.jacobcolvin.com/hcp_archiver/collect"
	"go.jacobcolvin.com/hcp_archiver/tfeclient"
)

// name identifies this collector in progress output and logs.
const name = "orgscope"

// msgListSkipped is the static log message emitted when a paginated list read
// does not complete and the collection is skipped for this run.
const msgListSkipped = "org-scoped list read did not complete; skipping collection this run"

// Collector archives the objects an organization owns directly, independent of
// any single project.
//
// It treats this metadata as mutable: every run re-reads the organization
// record, teams, memberships, VCS connections, governance objects, and the
// remaining org-level configuration and overwrites the stored copy when the
// payload changes. The raw Sentinel or OPA policy source is archived per
// revision instead: each revision is immutable and fetched only once, and a
// policy whose source is replaced archives the replacement under a name of its
// own (see [Collector.policySourcePath]). Create instances with [New]; it
// satisfies [collect.Collector].
type Collector struct {
	env  *collect.Env
	org  string
	hyok bool
}

// Option configures a [Collector] passed to [New].
//
// Options of this type:
//   - [WithHYOK]
type Option func(*Collector)

// WithHYOK enables archiving hold-your-own-key configurations when on. It
// returns an [Option].
func WithHYOK(enabled bool) Option {
	return func(c *Collector) {
		c.hyok = enabled
	}
}

// New creates a new [Collector] for the org owned by env.
//
// The archiver builds one per organization and runs it alongside the other
// domain collectors sharing the same [collect.Env].
func New(env *collect.Env, org string, opts ...Option) *Collector {
	c := &Collector{
		env: env,
		org: org,
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

// Collect archives every org-scoped object family in turn.
//
// Each family is best-effort: a single missing or failed object is recorded by
// the [collect.Env] primitives and does not abort the run, so only a
// cancellation of ctx stops the walk.
func (c *Collector) Collect(ctx context.Context) error {
	c.env.SetTarget(c.org)

	steps := []func(context.Context) error{
		c.collectOrganization,
		c.collectTeams,
		c.collectMemberships,
		c.collectOAuthClients,
		c.collectGitHubAppInstallations,
		c.collectVariableSets,
		c.collectPolicySets,
		c.collectPolicies,
		c.collectRunTasks,
		c.collectAgentPools,
		c.collectTokenTTLPolicies,
		c.collectReservedTagKeys,
		c.collectHYOKConfigurations,
		c.collectNotApplicable,
	}

	for _, step := range steps {
		err := step(ctx)
		if err != nil {
			return err
		}
	}

	return nil
}

// collectNotApplicable records the org-scoped surfaces that hold no archivable
// material, so a re-run does not mistake their absence for a gap.
//
// Organization token secrets are write-only and read back blank, and SSH keys
// expose only an id and name (the private key lives solely on the write-only
// create options), so neither yields anything worth a file.
func (c *Collector) collectNotApplicable(_ context.Context) error {
	st := c.env.Store()

	c.env.NotApplicable(st.Join("organization-tokens.json"))
	c.env.NotApplicable(st.Join("ssh-keys.json"))

	return nil
}

// paginate reads every page of an org-scoped list through the shared client.
//
// The bool reports whether the read completed: it is true on success (including
// a successful empty read, whose items are nil) and false when the read is
// skipped, so a caller that writes a whole-collection file can tell an empty
// roster from an unavailable one. A cancellation of ctx propagates so the run
// can wind down, while any other read error is recorded as a dropped surface
// ([collect.Env.MarkSurfaceDropped]), logged, and skipped, so one unavailable
// collection never aborts the collector yet still marks the run incomplete.
func paginate[T any](
	ctx context.Context,
	c *Collector,
	collection string,
	fetch func(context.Context, *tfe.Client, tfe.ListOptions) ([]T, *tfe.Pagination, error),
) ([]T, bool, error) {
	items, err := tfeclient.Paginate(ctx, c.env.Client(), fetch)
	if err != nil {
		if ctx.Err() != nil {
			return nil, false, fmt.Errorf("list %s: %w", collection, ctx.Err())
		}

		c.env.MarkSurfaceDropped(name+"/"+collection, err)

		slog.WarnContext(ctx, msgListSkipped,
			slog.String("collection", collection),
			slog.String("org", c.org),
			slog.Any("error", err),
		)

		return nil, false, nil
	}

	return items, true, nil
}

// fanOut archives every item concurrently through archive, capped at the
// environment's ceiling ([collect.Env.Concurrency]).
//
// Each goroutine is only a coordinator: the request it causes takes a slot from
// the client's gate, so the gate, not this fan-out, bounds the real parallelism.
// The cap keeps a huge collection, whose items mostly hit one endpoint (each
// team's notification configs, say), from parking thousands of goroutines on the
// gate at once. An archive returns non-nil only on a cancellation of ctx, which
// cancels the group and propagates.
func fanOut[T any](
	ctx context.Context,
	c *Collector,
	items []T,
	archive func(context.Context, T) error,
) error {
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(c.env.Concurrency())

	for _, item := range items {
		g.Go(func() error {
			return archive(gctx, item)
		})
	}

	return g.Wait() //nolint:wrapcheck // Archive closures already return contextual errors.
}

// enumerate lists an org-scoped collection and archives each item through
// archive, fanning the items across goroutines (see [fanOut]).
//
// The list read is best-effort: a read that does not complete is logged and the
// collection skipped (see [paginate]), so nothing is archived this run and a
// re-run retries. Items write under distinct per-id paths, and the user
// sub-objects shared across teams collapse to one write through the run's claim
// (see [Collector.archiveUser]), so concurrent archives never contend on one
// ledger entry.
func enumerate[T any](
	ctx context.Context,
	c *Collector,
	collection string,
	fetch func(context.Context, *tfe.Client, tfe.ListOptions) ([]T, *tfe.Pagination, error),
	archive func(context.Context, T) error,
) error {
	// Each item is archived on its own, so a successful empty read simply
	// archives nothing; only the completion error matters here, not the flag.
	items, _, err := paginate(ctx, c, collection, fetch)
	if err != nil {
		return err
	}

	return fanOut(ctx, c, items, archive)
}

// mutableValue archives a single already-listed object at relPath as mutable
// metadata, re-serializing the value the caller holds without a second read.
func (c *Collector) mutableValue(ctx context.Context, relPath string, value any) error {
	err := c.env.Mutable(ctx, relPath, func(context.Context) (any, error) {
		return value, nil
	})
	if err != nil {
		return fmt.Errorf("archive %q: %w", relPath, err)
	}

	return nil
}

// archiveUser archives a hydrated user sub-object at users/<id>.json as mutable
// metadata, once per run.
//
// The go-tfe SDK exposes no user list, only ReadCurrent, so a User hydrated on a
// team or membership is the only path to capturing who is on a team or in the
// org; every other reference is a permanently-opaque id. Teams and the roster
// reference the same users in bulk, and the workspace collector writes the same
// files from run creators and event actors, so the write goes through the run's
// claim ([collect.Env.ArchiveShared]): one caller writes each user and the rest
// wait on its outcome, which both skips the duplicate re-serialization and
// keeps the concurrent team archives off one file. A nil user is a no-op.
//
// One write per run means the first collector to reach a user fixes that run's
// payload, and this one runs before the workspace walk, so a user on a team is
// archived as the team hydrated them rather than as a run's created-by. The
// two hydrate the same resource, so the choice is invisible today; it stops
// being invisible if they ever diverge.
func (c *Collector) archiveUser(ctx context.Context, u *tfe.User) error {
	if u == nil {
		return nil
	}

	relPath := c.env.Store().User(u.ID)

	//nolint:wrapcheck // The claim is transparent; mutableValue wraps with the path context.
	return c.env.ArchiveShared(ctx, relPath, func(ctx context.Context) error {
		return c.mutableValue(ctx, relPath, u)
	})
}

// archiveList archives a whole paginated collection as one mutable file at
// relPath.
//
// The list read runs inside the [collect.Env] primitive, so a read error is
// recorded against the object and leaves the prior file untouched rather than
// overwriting it with an empty list; only a cancellation of ctx propagates.
func archiveList[T any](
	ctx context.Context,
	c *Collector,
	relPath string,
	collection string,
	fetch func(context.Context, *tfe.Client, tfe.ListOptions) ([]T, *tfe.Pagination, error),
) error {
	err := c.env.Mutable(ctx, relPath, func(ctx context.Context) (any, error) {
		items, e := tfeclient.Paginate(ctx, c.env.Client(), fetch)
		if e != nil {
			return nil, fmt.Errorf("list %s: %w", collection, e)
		}

		return items, nil
	})
	if err != nil {
		return fmt.Errorf("archive %s: %w", collection, err)
	}

	return nil
}
