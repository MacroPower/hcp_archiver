package remote

import (
	"bytes"
	"context"
	"crypto/md5" //nolint:gosec // Content MD5 is the stores' integrity currency, not a security control.
	"fmt"
	"path"
	"strings"
)

// preflightName is the probe object's filename, composed directly under the
// configured prefix; the leading dot keeps it beside, never inside, any
// organization's subtree.
const preflightName = ".preflight"

// preflightBody is the probe object's content: small enough to cost nothing,
// non-empty so the size read back proves the bytes landed.
var preflightBody = []byte("hcp_archiver remote store probe\n")

// preflightRangeOffset is where the probe's ranged read begins: past zero, so
// a store that silently ignores range requests and answers whole bodies is
// caught by the byte comparison instead of passing by luck.
const preflightRangeOffset = 4

// Preflight proves the client can manage objects in the configured store by
// round-tripping a probe under the prefix — write it, read its metadata
// back, find it in a listing, fetch a ranged span of it, delete it — the
// same motions an archive run's mirror and a later `view` of an evicted
// bundle perform, so a misconfigured bucket URL or credential set surfaces
// before any archive work begins rather than hours into it.
//
// The metadata read is also the digest check: the sync sweep's incremental
// gate and the eviction confirm both compare recorded MD5s, so a store that
// answers a digest that does not match the written bytes fails here, while a
// store that records none (an encrypted bucket whose ETags are not MD5s)
// passes and simply leaves those comparisons gating on size, as designed.
//
// The probe key is fixed, so a probe stranded by an interrupted run is
// overwritten and removed by the next preflight rather than accreting.
func (c *Client) Preflight(ctx context.Context) error {
	key := strings.TrimPrefix(path.Join("/", c.cfg.Prefix, preflightName), "/")

	err := c.Put(ctx, key, preflightBody)
	if err != nil {
		return fmt.Errorf("preflight: %w", err)
	}

	info, err := c.Head(ctx, key)
	if err != nil {
		return fmt.Errorf("preflight: %w", err)
	}

	if want := int64(len(preflightBody)); info.Size != want {
		return fmt.Errorf("preflight: probe %q reads back %d bytes, wrote %d", key, info.Size, want)
	}

	//nolint:gosec // Content MD5 is the stores' integrity currency, not a security control.
	if want := md5.Sum(preflightBody); info.MD5 != nil && !bytes.Equal(info.MD5, want[:]) {
		return fmt.Errorf("preflight: probe %q records a digest that does not match the written bytes", key)
	}

	listed, err := c.List(ctx, key)
	if err != nil {
		return fmt.Errorf("preflight: %w", err)
	}

	if _, ok := listed[key]; !ok {
		return fmt.Errorf("preflight: probe %q missing from its own listing", key)
	}

	err = c.preflightRangedRead(ctx, key)
	if err != nil {
		return err
	}

	_, err = c.Delete(ctx, []string{key})
	if err != nil {
		return fmt.Errorf("preflight: %w", err)
	}

	return nil
}

// preflightRangedRead proves the store serves ranged reads faithfully: the
// span read back from a non-zero offset must be exactly the probe's bytes at
// that offset. `view` reaches an evicted bundle's members only through
// ranged reads, so a store that rejects or mangles them must surface at
// startup, not at the first audit years later.
func (c *Client) preflightRangedRead(ctx context.Context, key string) error {
	want := preflightBody[preflightRangeOffset:]
	span := make([]byte, len(want))

	_, err := c.ReadAt(ctx, key, int64(len(preflightBody)), span, preflightRangeOffset)
	if err != nil {
		return fmt.Errorf("preflight: ranged read: %w", err)
	}

	if !bytes.Equal(span, want) {
		return fmt.Errorf("preflight: probe %q ranged read returns different bytes", key)
	}

	return nil
}
