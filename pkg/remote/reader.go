package remote

import (
	"bytes"
	"context"
	"crypto/md5" //nolint:gosec // Content MD5 is the stores' integrity currency, not a security control.
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"time"
)

// ReadAt reads len(p) bytes of the object at key starting at off, in a single
// ranged read. The size argument is the object's total length, as reported by
// [Client.Head]; reads at or past it answer [io.EOF] without a request. A
// transient failure (a request that never opened, or a body that died
// mid-fill) reopens the range and refills p from off under the client's
// bounded retries, so one blip does not surface as a failed screen or a
// failed sweep.
//
// An object holding fewer bytes than size is a different matter: the span
// asked for runs past its end, and would on every reopen, so the read reports
// [ErrShortObject] from the attempt that observes it rather than spending the
// retry budget on a length nothing can change. The store's own reported
// length settles it, so a body cut short of an object that is long enough
// still reads as the transient fault it is.
//
// It honors the [io.ReaderAt] contract (a non-nil error whenever it fills
// less than all of p, with [io.EOF] marking a read clipped by the object's
// end), so a caller can adapt it to [io.ReaderAt] with a context of its
// choosing per call. The shape suits [archive/zip.NewReader], which parses a
// bundle's central directory from a handful of reads near the end of the
// object; callers extracting a member should read its compressed span in one
// ReadAt rather than stream through a decompressor, which would issue
// thousands of tiny requests.
func (c *Client) ReadAt(ctx context.Context, key string, size int64, p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, fmt.Errorf("read %q: negative offset %d", key, off)
	}

	if len(p) == 0 {
		return 0, nil
	}

	if off >= size {
		return 0, io.EOF
	}

	want := min(int64(len(p)), size-off)

	var n int

	err := c.withRetry(ctx, func() error {
		return c.runAttempt(ctx, c.stallTimeout, func(ctx context.Context, touch func()) error {
			n = 0

			r, err := c.bucket.NewRangeReader(ctx, key, off, want, nil)
			if err != nil {
				return err //nolint:wrapcheck // Wrapped uniformly below.
			}

			defer func() {
				//nolint:errcheck // Read-only body; the ReadFull result is what matters.
				_ = r.Close()
			}()

			// The length riding the response is what tells a short object from
			// a short body: the first can never fill p, however often the
			// range reopens, so it settles here instead of reaching io.ReadFull
			// as an EOF the classifier would read as one more blip.
			if off+want > r.Size() {
				return fmt.Errorf("%w: %w: %s holds %d bytes, read against %d",
					errPermanent, ErrShortObject, key, r.Size(), size)
			}

			// Each delivered chunk is progress, so a large member span stays
			// alive while it moves and a wedged body stalls out and refills.
			n, err = io.ReadFull(countingReader{r: r, touch: touch}, p[:want])

			return err //nolint:wrapcheck // Wrapped uniformly below.
		})
	})
	if err != nil {
		if isNotFound(err) {
			return 0, fmt.Errorf("%w: %s", ErrNotFound, key)
		}

		return n, fmt.Errorf("ranged read %q at %d: %w", key, off, err)
	}

	if int64(n) < int64(len(p)) {
		return n, io.EOF
	}

	return n, nil
}

// Download streams the whole object at key into w and returns how many bytes
// it delivered. The size argument is the object's total length, as reported
// by [Client.Head] or recorded by whatever wrote the object; a size of zero
// issues no request and writes nothing, and a negative one is refused rather
// than read as empty. It serves the reads that want a whole object rather than
// a span of one (an evicted tarball recovered into a file, a mirrored copy
// hashed back to prove custody) in bounded memory, so an arbitrarily large
// body streams into its destination as it arrives.
//
// The bytes reach w forward-only and exactly once. A transient failure
// mid-body resumes from the byte count already delivered rather than
// restarting, because w has no rewind. A restart would re-deliver a prefix w
// already holds, and the caller would hash or store duplicated content. Every
// request is still a bounded range, so a resumed download reads the remainder
// and nothing more.
//
// A resume appends to bytes w already holds, so it may only read the object
// those bytes came from. Each attempt pins the version it opened (see
// [objectVersion]), and a resume finding a different one abandons the
// transfer with [ErrObjectChanged] rather than delivering a splice of two
// versions: a stream that hashes to neither, which every caller here would
// report as a healthy object gone corrupt. The pin binds only once a byte has
// landed, so a replacement observed before that re-pins and streams whole; a
// download holding nothing is free to serve what the key now holds, exactly
// as a fresh call would. The signal is the length and modification time every
// backend reports on a read, no portable precondition being available for a
// ranged one, so a replacement of identical length within the modification
// time's resolution reads as unchanged. The caller's own digest check remains
// the last word on content, as it already is.
//
// A store that serves fewer than size bytes and ends cleanly returns that
// count with a nil error, since a short object is not a transient fault and
// retrying it would only short-serve again. The length check therefore belongs
// to the caller, which is also the only side that knows what the object should
// measure. An absent object returns [ErrNotFound]. A failure of w itself is
// never retried, since the store did nothing wrong and would only be re-read
// to fail on the same write.
func (c *Client) Download(ctx context.Context, key string, size int64, w io.Writer) (int64, error) {
	if size < 0 {
		return 0, fmt.Errorf("download %q: negative size %d", key, size)
	}

	if size == 0 {
		return 0, nil
	}

	var (
		n      int64
		served objectVersion
	)

	dst := destWriter{w: w}

	err := c.withRetry(ctx, func() error {
		return c.runAttempt(ctx, c.stallTimeout, func(ctx context.Context, touch func()) error {
			// A body that failed after delivering everything (io.Copy reports
			// the bytes of its final Read alongside the error) leaves nothing to
			// resume: requesting bytes=size- would be a range past the object's
			// end, which the stores answer with a 416 rather than an empty body,
			// turning a download that in fact succeeded into a hard failure.
			if n >= size {
				return nil
			}

			// The same holds against the pinned version's own length when the
			// caller-passed size overshoots it (a stale size cached across a
			// foreign overwrite): the transfer has delivered every byte the
			// version holds, and the range past its end would 416 through the
			// whole retry budget. Ending here serves the short-object contract:
			// the delivered count returns with a nil error, and the caller's
			// length or digest check is the judge of the shortfall.
			if n > 0 && n >= served.size {
				return nil
			}

			r, err := c.bucket.NewRangeReader(ctx, key, n, size-n, nil)
			if err != nil {
				return err //nolint:wrapcheck // Wrapped uniformly below.
			}

			defer func() {
				//nolint:errcheck // Read-only body; the copy's result is what matters.
				_ = r.Close()
			}()

			// The bytes already in w cannot be taken back, so this range may only
			// extend the version they came from. Until one lands there is nothing
			// to splice, and whatever the key holds now becomes the pin.
			opened := objectVersion{modTime: r.ModTime(), size: r.Size()}

			switch {
			case n == 0:
				served = opened
			case !served.matches(opened):
				return fmt.Errorf("%w: %w after %d bytes: %s became %s",
					errPermanent, ErrObjectChanged, n, served, opened)
			}

			// Each delivered chunk is progress, so a multi-gigabyte object stays
			// alive while it moves and a wedged body stalls out and resumes.
			copied, err := io.Copy(dst, countingReader{r: r, touch: touch})
			n += copied

			return err //nolint:wrapcheck // Wrapped uniformly below.
		})
	})
	if err != nil {
		if isNotFound(err) {
			return n, fmt.Errorf("%w: %s", ErrNotFound, key)
		}

		return n, fmt.Errorf("download %q: %w", key, err)
	}

	return n, nil
}

// DownloadVerified streams the whole object at key into w and returns how
// many bytes it delivered, proving the body against info before reporting
// success: the byte count must match [ObjectInfo.Size], and the content must
// hash to the recorded SHA-256 when one is present, else to the recorded MD5
// when one is present. An info carrying no digest at all verifies on length
// alone, since there is nothing else to compare and refusing would strand an
// object the mirror holds; note that listings never carry digests, so info
// should come from [Client.Head] (or the write that recorded it), not from
// [Client.List], or the check silently weakens to the length. A mismatch of
// either kind reports [ErrDigestMismatch] or a length error after the bytes
// have already reached w, so callers restoring files must stage the stream
// through an atomic write and let the failure discard it.
func (c *Client) DownloadVerified(ctx context.Context, key string, info ObjectInfo, w io.Writer) (int64, error) {
	var sum hash.Hash

	dst := w

	switch {
	case info.SHA256 != "":
		sum = sha256.New()
	case len(info.MD5) > 0:
		sum = md5.New() //nolint:gosec // Content MD5 is the stores' integrity currency, not a security control.
	}

	if sum != nil {
		dst = io.MultiWriter(w, sum)
	}

	n, err := c.Download(ctx, key, info.Size, dst)
	if err != nil {
		return n, err
	}

	if n != info.Size {
		return n, fmt.Errorf("remote object %q served %d of the %d bytes its upload recorded", key, n, info.Size)
	}

	switch {
	case info.SHA256 != "":
		if hex.EncodeToString(sum.Sum(nil)) != info.SHA256 {
			return n, fmt.Errorf("%w: %s (sha256)", ErrDigestMismatch, key)
		}

	case len(info.MD5) > 0:
		if !bytes.Equal(sum.Sum(nil), info.MD5) {
			return n, fmt.Errorf("%w: %s (md5)", ErrDigestMismatch, key)
		}
	}

	return n, nil
}

// objectVersion identifies which version of an object one read opened, as
// the change signal a resumed download pins its later ranges to. It carries
// the object's length and modification time, the whole of the version
// identity a read exposes portably: gocloud's reader surfaces neither an
// ETag nor a generation, and its ranged read takes no precondition, so the
// attributes riding every response are what a resume has to compare. Two
// reads of an object nothing has touched report the same pair.
type objectVersion struct {
	modTime time.Time
	size    int64
}

// matches reports whether other opened the same version of the object as v.
func (v objectVersion) matches(other objectVersion) bool {
	return v.size == other.size && v.modTime.Equal(other.modTime)
}

// String renders the version as the mid-download replacement error names it:
// the length always, and the modification time when the backend reports one.
func (v objectVersion) String() string {
	if v.modTime.IsZero() {
		return fmt.Sprintf("%d bytes", v.size)
	}

	return fmt.Sprintf("%d bytes modified %s", v.size, v.modTime.UTC().Format(time.RFC3339))
}

// errPermanent marks a failure that would recur identically on another
// attempt, so [retryable] refuses it whatever code the driver would otherwise
// leave on it. It carries the settlements this package reaches for itself,
// none of which the classification of a store's response describes: a
// caller's destination refusing the bytes a download delivers (a full disk
// under an extract target), a replacement observed part way through a resumed
// download, an object shorter than the size a ranged read is measured
// against. Each classifies unknown, and each would only spend the request
// again to fail the same way.
var errPermanent = errors.New("permanent failure")

// destWriter marks the destination's own write faults with [errPermanent], so
// a download tells them apart from the store's. [io.Copy] reports a source and
// a destination failure identically, and only the destination's may not retry:
// re-requesting the object would spend the egress to fail on the same write.
type destWriter struct {
	w io.Writer
}

// Write delegates to the wrapped writer, returning its byte count unchanged so
// [io.Copy] still accounts for a partial write, which is what a resumed
// download measures its remaining range from.
func (d destWriter) Write(p []byte) (int, error) {
	n, err := d.w.Write(p)
	if err != nil {
		return n, fmt.Errorf("%w: write destination: %w", errPermanent, err)
	}

	return n, nil
}
