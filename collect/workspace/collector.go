package workspace

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/hashicorp/go-tfe"

	"go.jacobcolvin.com/hcp_archiver/collect"
	"go.jacobcolvin.com/hcp_archiver/tfeclient"
)

// Collector archives the project-scoped core of an organization: each project
// and its settings, and every workspace with its state versions and runs.
//
// The orchestrator enumerates projects and workspaces itself and fans
// workspaces across the shared request gate, so the collector exposes
// granular methods ([Collector.CollectProject] and
// [Collector.CollectWorkspace]) rather than the
// single-method collector contract. Work within one workspace fans out too:
// the state-version and run walks run concurrently and each page's items
// archive concurrently, with the client's request gate bounding the whole
// run's real parallelism.
//
// Create instances with [New].
type Collector struct {
	runHistoryOldest time.Time
	env              *collect.Env
	org              string
	runHistoryCount  int
}

// Option configures a [Collector] passed to [New].
//
// The available options are:
//   - [WithRunHistoryLimit]
type Option func(*Collector)

// WithRunHistoryLimit bounds each workspace's run walk to recent history: the
// newest count runs (when count is positive) and any run created at or after
// oldest (when oldest is non-zero), keeping whichever window admits more when
// both are set. Zero values leave run history unbounded, the default. It
// returns an [Option].
func WithRunHistoryLimit(count int, oldest time.Time) Option {
	return func(c *Collector) {
		c.runHistoryCount = count
		c.runHistoryOldest = oldest
	}
}

// New creates a new [Collector] archiving org into env.
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

// wrapArchive adds the relative path as context to an archive error, the single
// place per-object failures from the [collect.Env] primitives are wrapped.
func wrapArchive(relPath string, err error) error {
	if err != nil {
		return fmt.Errorf("archive %q: %w", relPath, err)
	}

	return nil
}

// listMutable archives the whole of a paginated list as one mutable file at
// relPath, re-reading and overwriting it when the payload changes.
func listMutable[T any](
	ctx context.Context,
	c *Collector,
	relPath string,
	list func(context.Context, *tfe.Client, tfe.ListOptions) ([]T, *tfe.Pagination, error),
) error {
	return wrapArchive(relPath, c.env.Mutable(ctx, relPath, func(ctx context.Context) (any, error) {
		items, err := tfeclient.Paginate(ctx, c.env.Client(), list)
		if err != nil {
			return nil, fmt.Errorf("paginate %q: %w", relPath, err)
		}

		return items, nil
	}))
}

// listObject archives the whole of a paginated list as one immutable file at
// relPath, skipping it once the ledger has it settled.
func listObject[T any](
	ctx context.Context,
	c *Collector,
	relPath string,
	list func(context.Context, *tfe.Client, tfe.ListOptions) ([]T, *tfe.Pagination, error),
) error {
	return wrapArchive(relPath, c.env.Object(ctx, relPath, func(ctx context.Context) (any, error) {
		items, err := tfeclient.Paginate(ctx, c.env.Client(), list)
		if err != nil {
			return nil, fmt.Errorf("paginate %q: %w", relPath, err)
		}

		return items, nil
	}))
}

// mutableOne archives a single value read through read as one mutable file at
// relPath, re-reading it on every run.
func mutableOne[T any](
	ctx context.Context,
	c *Collector,
	relPath string,
	read func(context.Context, *tfe.Client) (T, error),
) error {
	return wrapArchive(relPath, c.env.Mutable(ctx, relPath, func(ctx context.Context) (any, error) {
		v, err := doRead(ctx, c, relPath, read)
		if err != nil {
			return nil, err
		}

		return v, nil
	}))
}

// objectOne archives a single value read through read as one immutable file at
// relPath, skipping it once the ledger has it settled.
func objectOne[T any](
	ctx context.Context,
	c *Collector,
	relPath string,
	read func(context.Context, *tfe.Client) (T, error),
) error {
	return wrapArchive(relPath, c.env.Object(ctx, relPath, func(ctx context.Context) (any, error) {
		v, err := doRead(ctx, c, relPath, read)
		if err != nil {
			return nil, err
		}

		return v, nil
	}))
}

// object archives a value already in hand as one immutable file at relPath.
func (c *Collector) object(ctx context.Context, relPath string, value any) error {
	return wrapArchive(relPath, c.env.Object(ctx, relPath, func(_ context.Context) (any, error) {
		return value, nil
	}))
}

// recordErrored funnels a read failure that feeds an immutable object into the
// ledger through the self-gating [collect.Env.RecordFailure], so a settled path
// is left untouched (never regressed done->errored) while an unsettled one
// records the outcome under the client's classification and a re-run retries.
// It is how a shared read that splits into several derived files reports its
// failure without inventing a settled state or aborting the run walk.
//
// A terminal cause (a 404) settles the derived paths absent, so every read
// whose failure lands here must carry the in-run confirmation itself — run it
// through [readConfirmed] or [paginateAll] — since no primitive's own
// confirming re-probe stands between the read and the recorded outcome.
func (c *Collector) recordErrored(ctx context.Context, relPath string, cause error) error {
	return wrapArchive(relPath, c.env.RecordFailure(ctx, relPath, cause))
}

// mutable archives a value already in hand as one mutable file at relPath.
func (c *Collector) mutable(ctx context.Context, relPath string, value any) error {
	return wrapArchive(relPath, c.env.Mutable(ctx, relPath, func(_ context.Context) (any, error) {
		return value, nil
	}))
}

// archiveUser archives a hydrated user sub-object at users/<id>.json as mutable
// metadata. A nil user is a no-op.
//
// The go-tfe SDK exposes no user list, only ReadCurrent, so a User hydrated as a
// run's created-by or a run event's actor is the only path to capturing who ran
// or confirmed a run; every other reference is a permanently-opaque id.
//
// One user's file is written from everywhere at once: the Collector is shared
// across concurrent workspace workers, runs archive concurrently within a page,
// and a creator or actor recurs across both. The file is mutable metadata, so
// its commit retains history, which is more than the atomic rename covers. The
// write therefore goes through the run's claim
// ([collect.Env.ArchiveShared]), which reduces that crowd to a single writer.
func (c *Collector) archiveUser(ctx context.Context, u *tfe.User) error {
	if u == nil {
		return nil
	}

	relPath := c.env.Store().User(u.ID)

	//nolint:wrapcheck // The claim is transparent; mutable wraps with the path context.
	return c.env.ArchiveShared(ctx, relPath, func(ctx context.Context) error {
		return c.mutable(ctx, relPath, u)
	})
}

// doRead runs read through the shared client so it passes the rate governor,
// returning the value it yields.
func doRead[T any](
	ctx context.Context,
	c *Collector,
	relPath string,
	read func(context.Context, *tfe.Client) (T, error),
) (T, error) {
	var out T

	err := c.env.Client().Do(ctx, func(ctx context.Context, tc *tfe.Client) error {
		v, readErr := read(ctx, tc)
		out = v

		return readErr
	})
	if err != nil {
		return out, fmt.Errorf("read %q: %w", relPath, err)
	}

	return out, nil
}

// readConfirmed runs read through [doRead] with the terminal-confirmation
// semantics of the archive primitives (see [collect.Confirmed]): a 404 is
// re-probed once before it is believed. The shared reads that split into
// several derived files use it because their failures reach the ledger through
// [Collector.recordErrored] rather than a primitive's own confirmed fetch;
// without it a single eventual-consistency 404 would settle the derived paths
// absent. A read that feeds a primitive directly uses [doRead], which the
// primitive confirms itself.
func readConfirmed[T any](
	ctx context.Context,
	c *Collector,
	relPath string,
	read func(context.Context, *tfe.Client) (T, error),
) (T, error) {
	//nolint:wrapcheck // Confirmed is transparent; doRead wraps with the path context.
	return collect.Confirmed(ctx, c.env, func(ctx context.Context) (T, error) {
		return doRead(ctx, c, relPath, read)
	})
}

// paginateAll accumulates every page of a paginated list into one slice through
// the shared client, re-probing a terminal first answer once (see
// [collect.Confirmed]) because its callers report a failure through
// [Collector.recordErrored], outside any primitive's own confirming re-probe.
func paginateAll[T any](
	ctx context.Context,
	c *Collector,
	list func(context.Context, *tfe.Client, tfe.ListOptions) ([]T, *tfe.Pagination, error),
) ([]T, error) {
	//nolint:wrapcheck // Confirmed is transparent; the closure wraps the paginate error.
	return collect.Confirmed(ctx, c.env, func(ctx context.Context) ([]T, error) {
		items, err := tfeclient.Paginate(ctx, c.env.Client(), list)
		if err != nil {
			return nil, fmt.Errorf("paginate: %w", err)
		}

		return items, nil
	})
}

// logBlob streams a log opened through open into an immutable blob at relPath.
func (c *Collector) logBlob(
	ctx context.Context,
	relPath string,
	open func(context.Context, *tfe.Client) (io.Reader, error),
) error {
	return wrapArchive(relPath, c.env.Blob(ctx, relPath, func(ctx context.Context) (io.Reader, error) {
		var r io.Reader

		err := c.env.Client().Do(ctx, func(ctx context.Context, tc *tfe.Client) error {
			rr, e := open(ctx, tc)
			r = rr

			return e
		})
		if err != nil {
			return nil, fmt.Errorf("open log %q: %w", relPath, err)
		}

		return r, nil
	}))
}

// blob streams a raw artifact opened through open into an immutable blob at
// relPath, calling open directly: the [tfeclient.Client] Open methods already
// route through the shared client and own their request gate, so wrapping open
// in another [tfeclient.Client.Do] would re-acquire a slot while holding one and
// deadlock at small pool sizes. The reader that open returns streams to disk
// through [collect.Env.Blob] without buffering, and a nil reader settles a
// not-applicable gap.
func (c *Collector) blob(
	ctx context.Context,
	relPath string,
	open func(context.Context) (io.ReadCloser, error),
) error {
	return wrapArchive(relPath, c.env.Blob(ctx, relPath, func(ctx context.Context) (io.Reader, error) {
		return open(ctx)
	}))
}
