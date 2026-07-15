package remote

import (
	"bytes"
	"context"
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

// Preflight proves the client can manage objects in the configured store by
// round-tripping a probe under the prefix — write it, read its metadata back,
// find it in a listing, delete it — the same motions an archive run's mirror
// performs, so a misconfigured bucket, endpoint, region, or credential set
// surfaces before any archive work begins. The write carries the same
// checksum settings as real uploads, so a store that rejects the
// flexible-checksum headers (the reason [Config.DisableChecksums] exists)
// also reports here rather than on the first bundle.
//
// The probe key is fixed, so a probe stranded by an interrupted run is
// overwritten and removed by the next preflight rather than accreting; it is
// written in the store's default storage class, never an archival one.
func (c *Client) Preflight(ctx context.Context) error {
	key := strings.TrimPrefix(path.Join("/", c.cfg.Prefix, preflightName), "/")

	err := c.Put(ctx, key, bytes.NewReader(preflightBody), "")
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

	listed, err := c.List(ctx, key)
	if err != nil {
		return fmt.Errorf("preflight: %w", err)
	}

	if _, ok := listed[key]; !ok {
		return fmt.Errorf("preflight: probe %q missing from its own listing", key)
	}

	_, err = c.Delete(ctx, []string{key})
	if err != nil {
		return fmt.Errorf("preflight: %w", err)
	}

	return nil
}
