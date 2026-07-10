package workspace

import (
	"context"
	"fmt"
	"io"

	"github.com/hashicorp/go-tfe"

	"go.jacobcolvin.com/hcp_archiver/collect"
	"go.jacobcolvin.com/hcp_archiver/tfeclient"
)

// Collector archives the project-scoped core of an organization: each project
// and its settings, and every workspace with its state versions and runs.
//
// The orchestrator enumerates projects and workspaces itself and fans
// workspaces across a worker pool, so the collector exposes granular methods
// ([Collector.CollectProject] and [Collector.CollectWorkspace]) rather than the
// single-method collector contract. Work within one workspace is sequential.
//
// Create instances with [New].
type Collector struct {
	env *collect.Env
	org string
}

// New creates a new [Collector] archiving org into env.
func New(env *collect.Env, org string) *Collector {
	return &Collector{
		env: env,
		org: org,
	}
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
// ledger through the self-gating [collect.Env.Object], so a settled path is left
// untouched (never regressed done->errored) while an unsettled one records the
// error with the client's transient classification and a re-run retries. It is
// how a shared read that splits into several derived files reports its failure
// without inventing a settled state or aborting the run walk.
func (c *Collector) recordErrored(ctx context.Context, relPath string, cause error) error {
	return wrapArchive(relPath, c.env.Object(ctx, relPath, func(context.Context) (any, error) {
		return nil, cause
	}))
}

// mutable archives a value already in hand as one mutable file at relPath.
func (c *Collector) mutable(ctx context.Context, relPath string, value any) error {
	return wrapArchive(relPath, c.env.Mutable(ctx, relPath, func(_ context.Context) (any, error) {
		return value, nil
	}))
}

// doRead runs read through the shared client so it passes the rate limiter,
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

// paginateAll accumulates every page of a paginated list into one slice through
// the shared client.
func paginateAll[T any](
	ctx context.Context,
	c *Collector,
	list func(context.Context, *tfe.Client, tfe.ListOptions) ([]T, *tfe.Pagination, error),
) ([]T, error) {
	items, err := tfeclient.Paginate(ctx, c.env.Client(), list)
	if err != nil {
		return nil, fmt.Errorf("paginate: %w", err)
	}

	return items, nil
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

// bytesFromDo buffers a raw artifact fetched through fetch into an immutable
// blob at relPath.
func (c *Collector) bytesFromDo(
	ctx context.Context,
	relPath string,
	fetch func(context.Context, *tfe.Client) ([]byte, error),
) error {
	return wrapArchive(relPath, c.env.Bytes(ctx, relPath, func(ctx context.Context) ([]byte, error) {
		var b []byte

		err := c.env.Client().Do(ctx, func(ctx context.Context, tc *tfe.Client) error {
			bb, e := fetch(ctx, tc)
			b = bb

			return e
		})
		if err != nil {
			return nil, fmt.Errorf("fetch %q: %w", relPath, err)
		}

		return b, nil
	}))
}

// bytes buffers a raw artifact fetched through fetch into an immutable blob at
// relPath, running fetch directly (it already routes through the shared client).
func (c *Collector) bytes(ctx context.Context, relPath string, fetch func(context.Context) ([]byte, error)) error {
	return wrapArchive(relPath, c.env.Bytes(ctx, relPath, fetch))
}
