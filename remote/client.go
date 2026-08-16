package remote

import (
	"bytes"
	"context"
	"crypto/md5" //nolint:gosec // Content MD5 is the stores' integrity currency, not a security control.
	"crypto/sha256"
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

	// ErrShortObject indicates the object at a key holds fewer bytes than the
	// size [Client.ReadAt] was given to read it against, so the span asked for
	// runs past its end. It reports a mismatch between the object and the
	// length a caller carries for it (a truncated mirror copy, or a size
	// cached across a foreign overwrite), never a fault of the store or the
	// path to it: the recovery is to re-measure the object with [Client.Head]
	// and read it against what it now holds.
	ErrShortObject = errors.New("object shorter than the size read against")

	// ErrObjectChanged indicates a write replaced the object at a key while
	// [Client.Download] was streaming it, so the transfer was abandoned part
	// way rather than resumed against the new version and delivered as a
	// splice of the two. It impugns neither the bytes that reached the
	// destination nor the object now at the key: the recovery is to discard
	// the partial stream and download the settled object afresh.
	ErrObjectChanged = errors.New("object changed during download")
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
	bucket        *blob.Bucket
	wireBytes     *atomic.Int64
	cfg           Config
	retries       int
	retryDelay    time.Duration
	stallTimeout  time.Duration
	serverCopyCap int64

	// The attrsUntrusted flag records that the backend's own digest attribute
	// is not a content MD5 (an SSE-KMS-encrypted S3 bucket's ETag is hex but
	// hashes nothing readable), as proven by [Client.Preflight]'s probe. Once
	// set, [Client.Head] and [Client.List] serve only the metadata digests
	// this tool's writes record, so a bogus backend digest can neither refuse
	// an eviction confirm nor force perpetual re-uploads.
	attrsUntrusted atomic.Bool
}

// Option configures a [Client] passed to [New].
//
// Options of this type:
//   - [WithBucket]
//   - [WithRetry]
//   - [WithServerCopyCap]
//   - [WithStallTimeout]
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

// WithStallTimeout sets the window an attempt may go without progress
// before the watchdog cancels it as stalled and it retries as a transient
// failure, overriding [DefaultStallTimeout]. A non-positive timeout disables
// the watchdog, leaving a wedged connection to the caller's context. It
// returns an [Option].
func WithStallTimeout(d time.Duration) Option {
	return func(c *Client) {
		c.stallTimeout = d
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

// WithServerCopyCap sets the source size at and above which [Client.Copy]
// streams the bytes through the client instead of asking the backend for a
// server-side copy, overriding [DefaultServerCopyCap]. A backend whose
// single-request copy ceiling differs from S3's can be matched here; a
// non-positive cap streams every copy. It returns an [Option].
func WithServerCopyCap(size int64) Option {
	return func(c *Client) {
		c.serverCopyCap = size
	}
}

// New creates a new [Client] over cfg.
//
// Unless [WithBucket] supplies one, the bucket is opened from cfg's URL,
// whose scheme selects the backend; credentials never come from cfg, since
// each backend authenticates through its provider's default chain. It returns
// [ErrMissingURL] when cfg names no URL.
func New(ctx context.Context, cfg Config, opts ...Option) (*Client, error) {
	c := &Client{
		cfg:           cfg,
		retries:       DefaultRetries,
		retryDelay:    DefaultRetryDelay,
		stallTimeout:  DefaultStallTimeout,
		serverCopyCap: DefaultServerCopyCap,
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
// object. The MD5 doubles as the write's integrity check (the commit fails
// unless the streamed bytes hash to it), and both digests land in the
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

// DigestsOf computes the [Digests] of a body held whole in memory, the shape
// [Client.Put] serves; streamed bodies digest as they are read instead.
func DigestsOf(data []byte) Digests {
	//nolint:gosec // Content MD5 is the stores' integrity currency, not a security control.
	sum := md5.Sum(data)
	sha := sha256.Sum256(data)

	return Digests{
		SHA256: hex.EncodeToString(sha[:]),
		MD5:    sum[:],
	}
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
	// object's metadata by a write carrying [Digests], empty when none was
	// recorded. Listings never carry metadata, so it resolves only through
	// [Client.Head].
	SHA256 string
	// MD5 is the recorded full-object MD5 digest as raw bytes, comparable
	// against a locally computed sum: the metadata digest this tool's writes
	// record when present (computed from the exact bytes streamed and checked
	// against them at commit), else the backend's own attribute. Where the
	// preflight probe proved that attribute is not a content MD5 (an SSE-KMS
	// bucket's ETag, say), only metadata digests are served. It is nil when
	// nothing comparable exists. Listings carry no metadata, so a nil from
	// [Client.List] may yet resolve through [Client.Head].
	MD5 []byte
	// Size is the object's length in bytes.
	Size int64
}

// objectInfo maps one object's stored attributes onto [ObjectInfo]. The
// digest ladder prefers the metadata digest this tool's writes record over
// the backend's own attribute: the metadata digest was computed from the
// exact bytes handed to the store and verified against the streamed body at
// commit, while the attribute can be an ETag that is no content MD5 at all
// (an SSE-KMS-encrypted S3 object's is hex but hashes nothing readable). The
// attribute is used only as the fallback for objects recorded without
// metadata (an older build's write, a foreign object), and not even then once
// preflight has proven it untrue.
func (c *Client) objectInfo(attrs *blob.Attributes) ObjectInfo {
	info := ObjectInfo{
		SHA256: attrs.Metadata[metadataKeySHA256],
		Size:   attrs.Size,
	}

	if s := attrs.Metadata[metadataKeyMD5]; s != "" {
		sum, err := hex.DecodeString(s)
		if err == nil {
			info.MD5 = sum
		}
	}

	if info.MD5 == nil && !c.attrsUntrusted.Load() {
		info.MD5 = attrs.MD5
	}

	return info
}

// attributes reads the raw stored attributes at key under the client's
// retries: the shared body of [Client.Head], and the preflight probe's
// window onto the backend digest attribute before the trust ladder filters
// it.
func (c *Client) attributes(ctx context.Context, key string) (*blob.Attributes, error) {
	var attrs *blob.Attributes

	err := c.withRetry(ctx, func() error {
		return c.runAttempt(ctx, c.stallTimeout, func(ctx context.Context, _ func()) error {
			var aerr error

			attrs, aerr = c.bucket.Attributes(ctx, key)

			return aerr //nolint:wrapcheck // Wrapped by the callers.
		})
	})
	if err != nil {
		return nil, err //nolint:wrapcheck // Wrapped by the callers.
	}

	return attrs, nil
}

// Head reads the object's metadata at key. An absent object returns
// [ErrNotFound].
func (c *Client) Head(ctx context.Context, key string) (ObjectInfo, error) {
	attrs, err := c.attributes(ctx, key)
	if err != nil {
		if isNotFound(err) {
			return ObjectInfo{}, fmt.Errorf("%w: %s", ErrNotFound, key)
		}

		return ObjectInfo{}, fmt.Errorf("head %q: %w", key, err)
	}

	return c.objectInfo(attrs), nil
}

// Copy duplicates the object at srcKey to dstKey. A source below the
// server-copy cap copies server-side: the bytes never leave the backend and
// the recorded digest metadata rides along. A source at or past the cap
// streams through the client instead (see [DefaultServerCopyCap]), since the
// backend's single-request copy would reject it identically forever. The
// sweep's rename healing uses it to converge an only-copy stored under a
// pre-rename key onto the object's current name. An absent source returns
// [ErrNotFound].
func (c *Client) Copy(ctx context.Context, srcKey, dstKey string) error {
	attrs, err := c.attributes(ctx, srcKey)
	if err != nil {
		if isNotFound(err) {
			return fmt.Errorf("%w: %s", ErrNotFound, srcKey)
		}

		return fmt.Errorf("copy %q to %q: %w", srcKey, dstKey, err)
	}

	if c.serverCopyCap > 0 && attrs.Size < c.serverCopyCap {
		err = c.withRetry(ctx, func() error {
			return c.runAttempt(ctx, c.stallTimeout, func(ctx context.Context, _ func()) error {
				return c.bucket.Copy(ctx, dstKey, srcKey, nil) //nolint:wrapcheck // Wrapped below.
			})
		})
	} else {
		err = c.streamCopy(ctx, srcKey, dstKey, attrs)
	}

	if err != nil {
		if isNotFound(err) {
			return fmt.Errorf("%w: %s", ErrNotFound, srcKey)
		}

		return fmt.Errorf("copy %q to %q: %w", srcKey, dstKey, err)
	}

	return nil
}

// streamCopy copies srcKey to dstKey by streaming the bytes through the
// client, for sources the backend's single-request copy cannot carry. It
// costs egress, unlike the server-side path, but an oversized heal runs it
// once and converges permanently. The source's recorded digest metadata
// rides to the destination, and its MD5, when recorded, is re-verified
// against the streamed bytes at commit.
func (c *Client) streamCopy(ctx context.Context, srcKey, dstKey string, attrs *blob.Attributes) error {
	opts := &blob.WriterOptions{
		BufferSize:                  partSizeFor(attrs.Size, c.cfg.PartSize),
		MaxConcurrency:              c.cfg.Concurrency,
		DisableContentTypeDetection: true,
		ContentMD5:                  c.objectInfo(attrs).MD5,
		Metadata:                    attrs.Metadata,
	}

	//nolint:wrapcheck // The caller wraps with both keys.
	return c.withRetry(ctx, func() error {
		return c.runAttempt(ctx, c.writeWindow(attrs.Size), func(ctx context.Context, touch func()) error {
			r, err := c.bucket.NewReader(ctx, srcKey, nil)
			if err != nil {
				return err //nolint:wrapcheck // Wrapped by the caller.
			}

			defer r.Close() //nolint:errcheck // The write's own error decides the attempt.

			return c.write(ctx, dstKey, r, touch, opts)
		})
	})
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
// The size argument does not bound the upload (the bytes streamed are
// whatever body yields); it only grows the part size when needed, so a very
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

		return c.runAttempt(ctx, c.writeWindow(size), func(ctx context.Context, touch func()) error {
			return c.write(ctx, key, body, touch, opts)
		})
	})
	if err != nil {
		return fmt.Errorf("upload %q: %w", key, err)
	}

	return nil
}

// Put writes data to the object at key. Like [Client.Upload], the body's
// digests ride with the write: the MD5 is checked against the streamed bytes
// at commit, and both digests are recorded as object metadata so later
// [Client.Head] and [Client.List] calls can compare the stored content
// against local bytes even where the backend records no digest of its own. A
// body large enough to part (16 MiB on S3) leaves no backend attribute, so
// without the metadata a same-size content change would be invisible to the
// sync gate forever. The whole body rides in memory, the shape of the small
// search-layer files Put serves; bulk bytes stream through [Client.Upload].
func (c *Client) Put(ctx context.Context, key string, data []byte) error {
	digests := DigestsOf(data)
	opts := &blob.WriterOptions{
		DisableContentTypeDetection: true,
		ContentMD5:                  digests.MD5,
		Metadata:                    digests.metadata(),
	}

	err := c.withRetry(ctx, func() error {
		// The body streams through the shared write path rather than riding a
		// one-shot WriteAll, so its bytes feed the stall watchdog and the wire
		// counter exactly as an upload's do.
		return c.runAttempt(ctx, c.writeWindow(int64(len(data))), func(ctx context.Context, touch func()) error {
			return c.write(ctx, key, bytes.NewReader(data), touch, opts)
		})
	})
	if err != nil {
		return fmt.Errorf("put %q: %w", key, err)
	}

	return nil
}

// countingReader accumulates the bytes read through it into wire, feeding
// live upload throughput to a progress view, and reports each delivered
// chunk to the stall watchdog through touch. A nil wire counts nothing; a
// nil touch reports nothing.
type countingReader struct {
	r     io.Reader
	wire  *atomic.Int64
	touch func()
}

// Read delegates to the wrapped reader, counting delivered bytes before any
// error handling: [io.Reader] permits n > 0 alongside a non-nil error, and
// those bytes moved.
func (cr countingReader) Read(p []byte) (int, error) {
	n, err := cr.r.Read(p)
	if n > 0 {
		if cr.wire != nil {
			cr.wire.Add(int64(n))
		}

		if cr.touch != nil {
			cr.touch()
		}
	}

	return n, err //nolint:wrapcheck // A transparent reader wrapper.
}

// write streams r into one committed object at key, reporting progress to
// touch. A body that errs midway cancels the write before the final flush,
// so a truncated read can never commit a truncated object under the key.
func (c *Client) write(ctx context.Context, key string, r io.Reader, touch func(), opts *blob.WriterOptions) error {
	// The writer commits on Close unless its context is canceled first; the
	// derived context is the abort lever for a body that fails mid-copy.
	wctx, cancel := context.WithCancel(ctx)
	defer cancel()

	w, err := c.bucket.NewWriter(wctx, key, opts)
	if err != nil {
		return fmt.Errorf("open writer: %w", err)
	}

	_, copyErr := io.Copy(w, countingReader{r: r, wire: c.wireBytes, touch: touch})
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
		return c.runAttempt(ctx, c.stallTimeout, func(ctx context.Context, touch func()) error {
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

				// Each delivered object is progress: a long listing of a large
				// mirror stays alive page after page, while a wedged page fetch
				// stalls out and the enumeration retries whole.
				touch()

				if obj.IsDir {
					continue
				}

				// Listings carry only the backend attribute, never metadata, so a
				// distrusted attribute leaves the entry digestless; the sync gate
				// then resolves the metadata digest through one Head instead of
				// comparing against a value that is no content MD5.
				md5sum := obj.MD5
				if c.attrsUntrusted.Load() {
					md5sum = nil
				}

				out[obj.Key] = ObjectInfo{MD5: md5sum, Size: obj.Size}
			}
		})
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
// while the fan-out settles the rest (one bad key must not strand every other
// stale key for another run), and the failures come back as one joined error
// beside the truthful count. A context cancellation stops the fan-out early,
// leaving the rest to the next run.
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
				return c.runAttempt(ctx, c.stallTimeout, func(ctx context.Context, _ func()) error {
					derr := c.bucket.Delete(ctx, key)
					if derr != nil && !isNotFound(derr) {
						return derr //nolint:wrapcheck // Wrapped per key below.
					}

					return nil
				})
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
// backends and S3's multipart minimum (5 MiB); it floors both a configured
// part size and the grow-to-fit logic, because S3-compatible stores reject a
// multipart upload whose non-final parts are smaller with EntityTooSmall, and
// only after the whole body has streamed. That rejection is a pure function
// of size and configuration, so it would repeat on every retry and every run.
const defaultPartSize = 5 << 20

// partSizeFor returns the part size for a body of size bytes: the configured
// size floored at [defaultPartSize], grown when needed to fit the body within
// [maxUploadParts], and zero (the backend default) when nothing is configured
// and nothing demands more.
//
// Both permanently-repeating failure modes gate here: a part size below the
// floor is rejected by S3-compatible stores at commit, and a part count past
// the ceiling is rejected mid-stream, so neither a configured value nor the
// growth result may cross its bound.
func partSizeFor(size, configured int64) int {
	need := (size + maxUploadParts - 1) / maxUploadParts

	if configured <= 0 {
		if need > defaultPartSize {
			return int(need)
		}

		return 0
	}

	configured = max(configured, defaultPartSize)

	if need > configured {
		return int(need)
	}

	return int(configured)
}

// isNotFound reports whether err is a store response proving no object at
// the key. A transport-level failure never qualifies, whatever code the
// driver stamped on it (azblob classifies DNS failures as NotFound): absence
// is a destructive fact (a delete settles, an eviction probe re-uploads, a
// viewer reports a missing bundle), and only the store can attest it.
func isNotFound(err error) bool {
	return gcerrors.Code(err) == gcerrors.NotFound && !isTransportError(err)
}
