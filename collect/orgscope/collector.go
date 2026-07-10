package orgscope

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/hashicorp/go-tfe"

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
// payload changes. The raw Sentinel or OPA policy source is the one immutable
// artifact and is fetched only once. Create instances with [New]; it satisfies
// [collect.Collector].
type Collector struct {
	env *collect.Env

	// Set of user ids already archived this Collect, so the many teams and roster
	// entries that reference the same users skip a duplicate re-serialization. It
	// needs no lock because a Collector runs its org scope on a single goroutine.
	seenUsers map[string]struct{}

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
		env:       env,
		org:       org,
		seenUsers: make(map[string]struct{}),
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
// It returns the accumulated items, or a nil slice when the read does not
// complete: a cancellation of ctx propagates so the run can wind down, while any
// other read error is logged and skipped so one unavailable collection never
// aborts the collector.
func paginate[T any](
	ctx context.Context,
	c *Collector,
	collection string,
	fetch func(context.Context, *tfe.Client, tfe.ListOptions) ([]T, *tfe.Pagination, error),
) ([]T, error) {
	items, err := tfeclient.Paginate(ctx, c.env.Client(), fetch)
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("list %s: %w", collection, ctx.Err())
		}

		slog.WarnContext(ctx, msgListSkipped,
			slog.String("collection", collection),
			slog.String("org", c.org),
			slog.Any("error", err),
		)

		return nil, nil
	}

	return items, nil
}

// enumerate lists an org-scoped collection and archives each item through
// archive.
//
// The list read is best-effort: a read that does not complete is logged and the
// collection skipped (see [paginate]), so nothing is archived this run and a
// re-run retries. Only a cancellation of ctx surfaced by archive propagates.
func enumerate[T any](
	ctx context.Context,
	c *Collector,
	collection string,
	fetch func(context.Context, *tfe.Client, tfe.ListOptions) ([]T, *tfe.Pagination, error),
	archive func(context.Context, T) error,
) error {
	items, err := paginate(ctx, c, collection, fetch)
	if err != nil {
		return err
	}

	for _, item := range items {
		aerr := archive(ctx, item)
		if aerr != nil {
			return aerr
		}
	}

	return nil
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
// metadata, once per Collect.
//
// The go-tfe SDK exposes no user list, only ReadCurrent, so a User hydrated on a
// team or membership is the only path to capturing who is on a team or in the
// org; every other reference is a permanently-opaque id. Teams and the roster
// reference the same users in bulk, so a seen-set skips the duplicate
// re-serialization. A nil user is a no-op.
func (c *Collector) archiveUser(ctx context.Context, u *tfe.User) error {
	if u == nil {
		return nil
	}

	if _, ok := c.seenUsers[u.ID]; ok {
		return nil
	}

	c.seenUsers[u.ID] = struct{}{}

	return c.mutableValue(ctx, c.env.Store().User(u.ID), u)
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
