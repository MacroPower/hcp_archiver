package remote

import (
	"bytes"
	"context"
	"encoding/hex"
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

// PreflightReport carries what [Client.Preflight] learned about the store
// beyond pass/fail.
type PreflightReport struct {
	// AttrDigestsUntrusted reports that the store's own digest attribute did
	// not match the probe's written bytes (the shape of an SSE-KMS-encrypted
	// S3 bucket, whose ETags are hex but are not content MD5s), so the client
	// will serve only the metadata digests this tool's writes record for the
	// rest of the run. Digest comparisons stay content-aware; size-matched
	// files just pay one Head where a listing's attribute would have served.
	AttrDigestsUntrusted bool
}

// Preflight proves the client can manage objects in the configured store by
// round-tripping a probe under the prefix: it writes the probe through the
// eviction path with recorded digests, reads its attributes back, finds it in
// a listing, fetches a ranged span of it, and deletes it. Those are the same
// motions an archive run's mirror and a later `view` of an evicted bundle
// perform, so a misconfigured bucket URL or credential set surfaces before
// any archive work begins rather than hours into it.
//
// The attributes read is also the digest check, in two parts. The recorded
// metadata digests must read back exactly: they are the currency of the
// eviction confirm and the incremental sync gate, and a store that drops or
// mangles object metadata would silently degrade the custody transfer of the
// archive's only copies to a size-only comparison, so it fails preflight
// outright. The backend's own digest attribute, by contrast, is merely
// scored: one that mismatches the written bytes (an SSE-KMS bucket's ETag)
// marks the attribute untrusted in the returned [PreflightReport] and the
// client serves metadata digests alone from then on, while a store that
// records no attribute at all passes unremarked. The metadata carries the
// comparisons either way.
//
// The probe key is fixed, so a probe stranded by an interrupted run is
// overwritten and removed by the next preflight rather than accreting.
func (c *Client) Preflight(ctx context.Context) (PreflightReport, error) {
	var report PreflightReport

	key := strings.TrimPrefix(path.Join("/", c.cfg.Prefix, preflightName), "/")
	digests := DigestsOf(preflightBody)

	err := c.Upload(ctx, key, bytes.NewReader(preflightBody), int64(len(preflightBody)), digests)
	if err != nil {
		return report, fmt.Errorf("preflight: %w", err)
	}

	// The raw attributes, not [Client.Head]: the digest ladder would prefer
	// the metadata digest and hide the backend attribute this probe scores.
	attrs, err := c.attributes(ctx, key)
	if err != nil {
		return report, fmt.Errorf("preflight: head probe %q: %w", key, err)
	}

	if want := int64(len(preflightBody)); attrs.Size != want {
		return report, fmt.Errorf("preflight: probe %q reads back %d bytes, wrote %d", key, attrs.Size, want)
	}

	if attrs.Metadata[metadataKeySHA256] != digests.SHA256 ||
		attrs.Metadata[metadataKeyMD5] != hex.EncodeToString(digests.MD5) {
		return report, fmt.Errorf(
			"preflight: probe %q does not return the object metadata recorded with it; "+
				"the store drops or mangles metadata, which the mirror's digest verification depends on", key)
	}

	if attrs.MD5 != nil && !bytes.Equal(attrs.MD5, digests.MD5) {
		c.attrsUntrusted.Store(true)

		report.AttrDigestsUntrusted = true
	}

	listed, err := c.List(ctx, key)
	if err != nil {
		return report, fmt.Errorf("preflight: %w", err)
	}

	if _, ok := listed[key]; !ok {
		return report, fmt.Errorf("preflight: probe %q missing from its own listing", key)
	}

	err = c.preflightRangedRead(ctx, key)
	if err != nil {
		return report, err
	}

	_, err = c.Delete(ctx, []string{key})
	if err != nil {
		return report, fmt.Errorf("preflight: %w", err)
	}

	return report, nil
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
