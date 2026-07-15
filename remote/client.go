package remote

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync/atomic"

	"gocloud.dev/blob"
	"gocloud.dev/gcerrors"
	"golang.org/x/sync/errgroup"

	// Each blank import registers one bucket URL scheme with
	// [blob.OpenBucket]: s3:// (AWS S3 and compatible stores), azblob://
	// (Azure Blob Storage), and file:// (a local directory tree).
	_ "gocloud.dev/blob/azureblob"
	_ "gocloud.dev/blob/fileblob"
	_ "gocloud.dev/blob/s3blob"
)

// Sentinel errors reported by [New] and the [Client] methods.
var (
	// ErrMissingURL indicates a [Config] that names no bucket URL.
	ErrMissingURL = errors.New("remote bucket url is required")

	// ErrNotFound indicates the object at a key does not exist in the store.
	ErrNotFound = errors.New("object not found in remote store")
)

// maxUploadParts is the smallest part-count ceiling among the backends that
// split an upload into parts (S3 caps a multipart upload at 10,000 parts;
// Azure caps a block blob at 50,000 blocks). [Client.Upload] grows its part
// size so any body fits under it.
const maxUploadParts = 10_000

// Client reads and writes an organization's mirrored archive objects in one
// object-store bucket: evicted cold surfaces through [Client.Upload] and the
// synced search layer through [Client.Put], with [Client.List] and
// [Client.Delete] serving the sync sweep's inventory and prune.
//
// A Client is safe for concurrent use; the archiver shares one across every
// organization it archives. Create instances with [New]; a client that is no
// longer needed releases its backend resources through [Client.Close].
type Client struct {
	bucket *blob.Bucket
	cfg    Config
}

// Option configures a [Client] passed to [New].
//
// Options of this type:
//   - [WithBucket]
type Option func(*Client)

// WithBucket injects the opened bucket the client calls, replacing the one
// [blob.OpenBucket] would resolve from the configured URL; tests inject an
// in-memory bucket through it. A nil bucket keeps the default. It returns an
// [Option].
func WithBucket(bucket *blob.Bucket) Option {
	return func(c *Client) {
		if bucket != nil {
			c.bucket = bucket
		}
	}
}

// New creates a new [Client] over cfg.
//
// Unless [WithBucket] supplies one, the bucket is opened from cfg's URL,
// whose scheme selects the backend; credentials never come from cfg — each
// backend authenticates through its provider's default chain. It returns
// [ErrMissingURL] when cfg names no URL.
func New(ctx context.Context, cfg Config, opts ...Option) (*Client, error) {
	c := &Client{cfg: cfg}

	for _, opt := range opts {
		opt(c)
	}

	if c.bucket == nil {
		if cfg.URL == "" {
			return nil, ErrMissingURL
		}

		bucket, err := blob.OpenBucket(ctx, cfg.URL)
		if err != nil {
			return nil, fmt.Errorf("open bucket: %w", err)
		}

		c.bucket = bucket
	}

	return c, nil
}

// Close releases the resources held by the client's backend connection. The
// client must not be used after Close.
func (c *Client) Close() error {
	err := c.bucket.Close()
	if err != nil {
		return fmt.Errorf("close bucket: %w", err)
	}

	return nil
}

// ObjectInfo describes one stored object as observed by [Client.Head] and
// [Client.List].
type ObjectInfo struct {
	// MD5 is the store's recorded full-object MD5 digest as raw bytes,
	// comparable against a locally computed sum. It is nil when the store
	// records none for the object (a parted upload on most backends). Some
	// backends' listings omit a digest the object still carries, so a nil
	// from [Client.List] may yet resolve through [Client.Head].
	MD5 []byte
	// Size is the object's length in bytes.
	Size int64
}

// Head reads the object's metadata at key. An absent object returns
// [ErrNotFound].
func (c *Client) Head(ctx context.Context, key string) (ObjectInfo, error) {
	attrs, err := c.bucket.Attributes(ctx, key)
	if err != nil {
		if isNotFound(err) {
			return ObjectInfo{}, fmt.Errorf("%w: %s", ErrNotFound, key)
		}

		return ObjectInfo{}, fmt.Errorf("head %q: %w", key, err)
	}

	return ObjectInfo{MD5: attrs.MD5, Size: attrs.Size}, nil
}

// Upload streams r to the object at key, letting the backend split a large
// body into a concurrent parted upload; a write that dies midway is aborted
// rather than committed truncated. The object only becomes visible at the
// key once the final flush succeeds.
//
// The size argument does not bound the upload — the bytes streamed are
// whatever r yields — it only grows the part size when needed so a very
// large body still fits the backend's part-count ceiling.
func (c *Client) Upload(ctx context.Context, key string, r io.Reader, size int64) error {
	err := c.write(ctx, key, r, &blob.WriterOptions{
		BufferSize:                  partSizeFor(size, c.cfg.PartSize),
		MaxConcurrency:              c.cfg.Concurrency,
		DisableContentTypeDetection: true,
	})
	if err != nil {
		return fmt.Errorf("upload %q: %w", key, err)
	}

	return nil
}

// Put writes data to the object at key, recording the body's MD5 digest
// with the object so later [Client.Head] and [Client.List] calls can compare
// the stored content against local bytes; the digest doubles as the write's
// integrity check. The whole body rides in memory — the shape of the small
// search-layer files Put serves; bulk bytes stream through [Client.Upload].
func (c *Client) Put(ctx context.Context, key string, data []byte) error {
	// WriteAll digests the body itself and carries the digest as the
	// write's ContentMD5.
	err := c.bucket.WriteAll(ctx, key, data, &blob.WriterOptions{
		DisableContentTypeDetection: true,
	})
	if err != nil {
		return fmt.Errorf("put %q: %w", key, err)
	}

	return nil
}

// write streams r into one committed object at key. A body that errs midway
// cancels the write before the final flush, so a truncated read can never
// commit a truncated object under the key.
func (c *Client) write(ctx context.Context, key string, r io.Reader, opts *blob.WriterOptions) error {
	// The writer commits on Close unless its context is canceled first; the
	// derived context is the abort lever for a body that fails mid-copy.
	wctx, cancel := context.WithCancel(ctx)
	defer cancel()

	w, err := c.bucket.NewWriter(wctx, key, opts)
	if err != nil {
		return fmt.Errorf("open writer: %w", err)
	}

	_, copyErr := io.Copy(w, r)
	if copyErr != nil {
		cancel()
	}

	closeErr := w.Close()

	switch {
	case copyErr != nil:
		return fmt.Errorf("stream body: %w", copyErr)
	case closeErr != nil:
		return fmt.Errorf("commit object: %w", closeErr)
	}

	return nil
}

// List enumerates every object under prefix, keyed by full object key, at
// roughly one request per listing page. It is the bulk inventory the sync
// sweep gates uploads and prunes stale keys from, replacing a Head per file.
func (c *Client) List(ctx context.Context, prefix string) (map[string]ObjectInfo, error) {
	out := make(map[string]ObjectInfo)
	iter := c.bucket.List(&blob.ListOptions{Prefix: prefix})

	for {
		obj, err := iter.Next(ctx)
		if errors.Is(err, io.EOF) {
			return out, nil
		}

		if err != nil {
			return nil, fmt.Errorf("list %q: %w", prefix, err)
		}

		if obj.IsDir {
			continue
		}

		out[obj.Key] = ObjectInfo{MD5: obj.MD5, Size: obj.Size}
	}
}

// deleteConcurrency bounds how many delete requests fly at once. The
// backends take one request per key, so a prune of thousands of stale keys
// must not serialize one round-trip per key.
const deleteConcurrency = 16

// Delete removes the objects at keys and returns how many keys it durably
// settled. Deletes are idempotent: a key that does not exist settles as
// removed and counts. An error the store reports stops the fan-out, and the
// count reflects only the keys that settled, so a caller's tally stays
// truthful across a partial delete.
func (c *Client) Delete(ctx context.Context, keys []string) (int, error) {
	var deleted atomic.Int64

	g, ctx := errgroup.WithContext(ctx)
	g.SetLimit(deleteConcurrency)

	for _, key := range keys {
		g.Go(func() error {
			err := c.bucket.Delete(ctx, key)
			if err != nil && !isNotFound(err) {
				return fmt.Errorf("delete %q: %w", key, err)
			}

			deleted.Add(1)

			return nil
		})
	}

	err := g.Wait()
	if err != nil {
		return int(deleted.Load()), err //nolint:wrapcheck // Each worker wraps its own error.
	}

	return int(deleted.Load()), nil
}

// defaultPartSize is the smallest default part size among the parted-upload
// backends (S3's 5 MiB); it floors the grow-to-fit logic so a small body
// never shrinks the part size below what any backend would pick itself.
const defaultPartSize = 5 << 20

// partSizeFor returns the part size for a body of size bytes: the configured
// size, grown when needed to fit the body within [maxUploadParts], and zero
// (the backend default) when nothing is configured and nothing demands more.
func partSizeFor(size, configured int64) int {
	need := (size + maxUploadParts - 1) / maxUploadParts
	if need > max(configured, defaultPartSize) {
		return int(need)
	}

	return int(configured)
}

// isNotFound reports whether the backend classifies err as "no such object".
func isNotFound(err error) bool {
	return gcerrors.Code(err) == gcerrors.NotFound
}
