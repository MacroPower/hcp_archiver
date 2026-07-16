package remote

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

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

// Object metadata keys recording the full-object digests of an upload, the
// mirror's egress-free comparison currency: "md5" carries the digest the
// incremental sync gate compares where the backend's own attribute is absent
// (a parted upload), and "sha256" the digest the eviction confirm matches
// against the local proof that releases the only local copy.
const (
	metadataKeyMD5    = "md5"
	metadataKeySHA256 = "sha256"
)

// Client reads and writes an organization's mirrored archive objects in one
// object-store bucket: evicted cold surfaces through [Client.Upload] and the
// synced search layer through [Client.Put], with [Client.List] and
// [Client.Delete] serving the sync sweep's inventory and prune.
//
// Every operation retries a transient store failure under a bounded doubling
// backoff (see [DefaultRetries]), mirroring the persistence the API transport
// gives fetches, so one blip does not defer mirror work to a later run.
//
// A Client is safe for concurrent use; the archiver shares one across every
// organization it archives. Create instances with [New]; a client that is no
// longer needed releases its backend resources through [Client.Close].
type Client struct {
	bucket     *blob.Bucket
	wireBytes  *atomic.Int64
	cfg        Config
	retries    int
	retryDelay time.Duration
}

// Option configures a [Client] passed to [New].
//
// Options of this type:
//   - [WithBucket]
//   - [WithRetry]
//   - [WithWireBytes]
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

// WithRetry sets how each store operation retries a transient failure within
// the run: retries is the number of additional attempts after the first, and
// delay is the wait before the first retry, doubling on each retry after that
// (bounded by an internal cap). A zero or negative retries disables in-client
// retrying, leaving each failure to the caller's own recovery (the next
// sweep); a non-positive delay retries immediately. It returns an [Option].
func WithRetry(retries int, delay time.Duration) Option {
	return func(c *Client) {
		c.retries = max(retries, 0)
		c.retryDelay = delay
	}
}

// WithWireBytes sets a shared counter that accumulates upload bytes as they
// stream to the store, so a progress view can derive live throughput while a
// large object is still in flight. Bytes are counted as they move, so a
// retried attempt recounts what it re-streams. A nil counter leaves bytes
// uncounted. It returns an [Option].
func WithWireBytes(counter *atomic.Int64) Option {
	return func(c *Client) {
		c.wireBytes = counter
	}
}

// New creates a new [Client] over cfg.
//
// Unless [WithBucket] supplies one, the bucket is opened from cfg's URL,
// whose scheme selects the backend; credentials never come from cfg — each
// backend authenticates through its provider's default chain. It returns
// [ErrMissingURL] when cfg names no URL.
func New(ctx context.Context, cfg Config, opts ...Option) (*Client, error) {
	c := &Client{
		cfg:        cfg,
		retries:    DefaultRetries,
		retryDelay: DefaultRetryDelay,
	}

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

// Digests carries the full-object content digests an upload records with its
// object. The MD5 doubles as the write's integrity check — the commit fails
// unless the streamed bytes hash to it — and both digests land in the
// object's metadata so a later probe can compare stored content against
// local bytes without a byte of egress. Either field may be empty; only what
// is present is checked and recorded.
type Digests struct {
	// SHA256 is the lowercase hex full-object SHA-256 of the body, the
	// currency of the archive's local proofs (ledger signatures, sidecar
	// entries).
	SHA256 string
	// MD5 is the raw full-object MD5 digest of the body.
	MD5 []byte
}

// metadata renders the digests as the object metadata recorded with an
// upload, or nil when neither digest is present.
func (d Digests) metadata() map[string]string {
	m := make(map[string]string, 2)

	if len(d.MD5) > 0 {
		m[metadataKeyMD5] = hex.EncodeToString(d.MD5)
	}

	if d.SHA256 != "" {
		m[metadataKeySHA256] = d.SHA256
	}

	if len(m) == 0 {
		return nil
	}

	return m
}

// ObjectInfo describes one stored object as observed by [Client.Head] and
// [Client.List].
type ObjectInfo struct {
	// SHA256 is the lowercase hex full-object SHA-256 recorded in the
	// object's metadata by an [Client.Upload] with [Digests], empty when none
	// was recorded. Listings never carry metadata, so it resolves only
	// through [Client.Head].
	SHA256 string
	// MD5 is the store's recorded full-object MD5 digest as raw bytes,
	// comparable against a locally computed sum: the backend's own attribute
	// when it records one, else the metadata digest an [Client.Upload] with
	// [Digests] recorded (a parted upload carries no backend attribute on
	// most stores). It is nil when neither exists. Some backends' listings
	// omit a digest the object still carries, so a nil from [Client.List]
	// may yet resolve through [Client.Head].
	MD5 []byte
	// Size is the object's length in bytes.
	Size int64
}

// objectInfo maps one object's stored attributes onto [ObjectInfo],
// resolving the digest ladder: the backend's own MD5 first, else the
// metadata digest this tool's uploads record.
func objectInfo(attrs *blob.Attributes) ObjectInfo {
	info := ObjectInfo{
		MD5:    attrs.MD5,
		SHA256: attrs.Metadata[metadataKeySHA256],
		Size:   attrs.Size,
	}

	if info.MD5 == nil {
		if s := attrs.Metadata[metadataKeyMD5]; s != "" {
			sum, err := hex.DecodeString(s)
			if err == nil {
				info.MD5 = sum
			}
		}
	}

	return info
}

// Head reads the object's metadata at key. An absent object returns
// [ErrNotFound].
func (c *Client) Head(ctx context.Context, key string) (ObjectInfo, error) {
	var attrs *blob.Attributes

	err := c.withRetry(ctx, func() error {
		var aerr error

		attrs, aerr = c.bucket.Attributes(ctx, key)

		return aerr //nolint:wrapcheck // Wrapped uniformly below.
	})
	if err != nil {
		if isNotFound(err) {
			return ObjectInfo{}, fmt.Errorf("%w: %s", ErrNotFound, key)
		}

		return ObjectInfo{}, fmt.Errorf("head %q: %w", key, err)
	}

	return objectInfo(attrs), nil
}

// Upload streams body to the object at key, letting the backend split a
// large body into a concurrent parted upload; a write that dies midway is
// aborted rather than committed truncated, and a transient failure rewinds
// the body and retries. The object only becomes visible at the key once the
// final flush succeeds.
//
// The digests, when present, ride with the write: the MD5 is verified
// against the streamed bytes at commit, and both digests are recorded as
// object metadata for later egress-free comparison (see [Digests]).
//
// The size argument does not bound the upload — the bytes streamed are
// whatever body yields — it only grows the part size when needed so a very
// large body still fits the backend's part-count ceiling.
func (c *Client) Upload(ctx context.Context, key string, body io.ReadSeeker, size int64, digests Digests) error {
	opts := &blob.WriterOptions{
		BufferSize:                  partSizeFor(size, c.cfg.PartSize),
		MaxConcurrency:              c.cfg.Concurrency,
		DisableContentTypeDetection: true,
		ContentMD5:                  digests.MD5,
		Metadata:                    digests.metadata(),
	}

	err := c.withRetry(ctx, func() error {
		_, serr := body.Seek(0, io.SeekStart)
		if serr != nil {
			return fmt.Errorf("rewind body: %w", serr)
		}

		return c.write(ctx, key, body, opts)
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
	err := c.withRetry(ctx, func() error {
		// WriteAll digests the body itself and carries the digest as the
		// write's ContentMD5.
		//nolint:wrapcheck // Wrapped uniformly below.
		return c.bucket.WriteAll(ctx, key, data, &blob.WriterOptions{
			DisableContentTypeDetection: true,
		})
	})
	if err != nil {
		return fmt.Errorf("put %q: %w", key, err)
	}

	// WriteAll offers no streaming hook, so the whole body counts once on
	// success; against a retried streaming upload's recount this is a
	// negligible asymmetry at the small sizes Put serves.
	if c.wireBytes != nil {
		c.wireBytes.Add(int64(len(data)))
	}

	return nil
}

// countingReader accumulates the bytes read through it into wire, feeding
// live upload throughput to a progress view. A nil wire counts nothing.
type countingReader struct {
	r    io.Reader
	wire *atomic.Int64
}

// Read delegates to the wrapped reader, counting delivered bytes before any
// error handling: [io.Reader] permits n > 0 alongside a non-nil error, and
// those bytes moved.
func (cr countingReader) Read(p []byte) (int, error) {
	n, err := cr.r.Read(p)
	if n > 0 && cr.wire != nil {
		cr.wire.Add(int64(n))
	}

	return n, err //nolint:wrapcheck // A transparent reader wrapper.
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

	_, copyErr := io.Copy(w, countingReader{r: r, wire: c.wireBytes})
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
// A transient failure mid-walk restarts the enumeration from the beginning,
// so the returned inventory is always one whole listing, never a splice.
func (c *Client) List(ctx context.Context, prefix string) (map[string]ObjectInfo, error) {
	var out map[string]ObjectInfo

	err := c.withRetry(ctx, func() error {
		out = make(map[string]ObjectInfo)
		iter := c.bucket.List(&blob.ListOptions{Prefix: prefix})

		for {
			obj, err := iter.Next(ctx)
			if errors.Is(err, io.EOF) {
				return nil
			}

			if err != nil {
				return err //nolint:wrapcheck // Wrapped uniformly below.
			}

			if obj.IsDir {
				continue
			}

			out[obj.Key] = ObjectInfo{MD5: obj.MD5, Size: obj.Size}
		}
	})
	if err != nil {
		return nil, fmt.Errorf("list %q: %w", prefix, err)
	}

	return out, nil
}

// deleteConcurrency bounds how many delete requests fly at once. The
// backends take one request per key, so a prune of thousands of stale keys
// must not serialize one round-trip per key.
const deleteConcurrency = 16

// maxDeleteErrs bounds how many per-key failures the returned error details;
// the remainder is summarized by count, so a store-wide outage cannot render
// thousands of identical lines into one error.
const maxDeleteErrs = 8

// Delete removes the objects at keys and returns how many keys it durably
// settled. Deletes are idempotent: a key that does not exist settles as
// removed and counts. A key whose delete fails past its retries is skipped
// while the fan-out settles the rest — one bad key must not strand every
// other stale key for another run — and the failures come back as one
// joined error beside the truthful count. A context cancellation stops the
// fan-out early, leaving the rest to the next run.
func (c *Client) Delete(ctx context.Context, keys []string) (int, error) {
	var (
		deleted atomic.Int64
		mu      sync.Mutex
		errs    []error
		failed  int
	)

	var g errgroup.Group

	g.SetLimit(deleteConcurrency)

	for _, key := range keys {
		if ctx.Err() != nil {
			break
		}

		g.Go(func() error {
			// A cancellation mid-fan-out is the wind-down: the started worker
			// settles nothing and leaves its key to the next run.
			//nolint:nilerr // See above: a canceled worker settles nothing.
			if ctx.Err() != nil {
				return nil
			}

			err := c.withRetry(ctx, func() error {
				derr := c.bucket.Delete(ctx, key)
				if derr != nil && !isNotFound(derr) {
					return derr //nolint:wrapcheck // Wrapped per key below.
				}

				return nil
			})
			if err != nil {
				mu.Lock()

				failed++
				if len(errs) < maxDeleteErrs {
					errs = append(errs, fmt.Errorf("delete %q: %w", key, err))
				}

				mu.Unlock()

				// A per-key failure is collected, not returned: failing the
				// group would strand the other stale keys.
				return nil //nolint:nilerr // See above.
			}

			deleted.Add(1)

			return nil
		})
	}

	//nolint:errcheck // Workers never return an error; Wait is the barrier.
	_ = g.Wait()

	if failed > 0 {
		if failed > len(errs) {
			errs = append(errs, fmt.Errorf("%d further deletes failed", failed-len(errs)))
		}

		return int(deleted.Load()), fmt.Errorf(
			"%d of %d deletes failed: %w", failed, len(keys), errors.Join(errs...))
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
