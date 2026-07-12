package registry

import (
	"context"
	"fmt"
	"log/slog"

	"go.jacobcolvin.com/hcp_archiver/collect"
)

// msgListSkipped is the static log message emitted when a registry list read
// does not complete and the family is skipped for this run.
const msgListSkipped = "registry list read did not complete; skipping collection this run"

// Collector archives an organization's private registry: modules, providers,
// and the GPG public keys that sign them.
//
// Module and provider metadata is always captured as mutable, re-read on every
// run. The deeper version, platform, and binary detail multiplies request
// volume, so it is gathered only when the collector is built [WithDetail]. It
// satisfies [collect.Collector]. Create instances with [New].
type Collector struct {
	env    *collect.Env
	org    string
	detail bool
}

// Option configures a [Collector] passed to [New].
//
// Options of this type:
//   - [WithDetail]
type Option func(*Collector)

// WithDetail toggles collection of the deeper version, platform, and binary
// detail (module versions and last commits, provider versions and platforms,
// and the beta per-version no-code variable options). It returns an [Option].
func WithDetail(detail bool) Option {
	return func(c *Collector) {
		c.detail = detail
	}
}

// New creates a new [Collector] archiving the registry of org into env.
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
	return "registry"
}

// Collect archives the registry's modules, providers, and GPG keys.
//
// Each family is best-effort and independent: a per-object miss is recorded by
// the [collect.Env] primitives and a failure to enumerate one family does not
// stop the others. Only a cancellation of ctx aborts the collector.
func (c *Collector) Collect(ctx context.Context) error {
	err := c.collectModules(ctx)
	if err != nil {
		return err
	}

	err = c.collectProviders(ctx)
	if err != nil {
		return err
	}

	return c.collectGPGKeys(ctx)
}

// listFailed maps a collection-level list error onto the best-effort contract:
// a cancellation of ctx propagates so the run can wind down, while any other
// enumeration failure is logged and swallowed so an unreachable or disabled
// registry family does not abort the archive (a re-run retries, having recorded
// nothing settled). The drop is recorded through [collect.Env.MarkSurfaceDropped]
// so the run still reports incomplete over the missing family, and logged so an
// operator sees which family was omitted, matching the org-scoped and stacks
// collectors.
func (c *Collector) listFailed(ctx context.Context, family string, cause error) error {
	ctxErr := ctx.Err()
	if ctxErr != nil {
		return fmt.Errorf("list registry %s: %w", family, ctxErr)
	}

	c.env.MarkSurfaceDropped("registry/"+family, cause)

	slog.WarnContext(ctx, msgListSkipped,
		slog.String("family", family),
		slog.String("org", c.org),
		slog.Any("error", cause),
	)

	return nil
}

// wrap adds op as context to a non-nil error surfaced by the shared collect and
// tfeclient primitives, preserving the cause for classification. Routing these
// cross-package errors through a local helper keeps the wrapping in one place.
func wrap(op string, err error) error {
	if err == nil {
		return nil
	}

	return fmt.Errorf("%s: %w", op, err)
}
