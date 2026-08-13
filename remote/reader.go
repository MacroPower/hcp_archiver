package remote

import (
	"context"
	"errors"
	"fmt"
	"io"
)

// ReadAt reads len(p) bytes of the object at key starting at off, in a single
// ranged read. The size argument is the object's total length, as reported by
// [Client.Head]; reads at or past it answer [io.EOF] without a request. A
// transient failure — a request that never opened, or a body that died
// mid-fill — reopens the range and refills p from off under the client's
// bounded retries, so one blip does not surface as a failed screen or a
// failed sweep.
//
// It honors the [io.ReaderAt] contract — a non-nil error whenever it fills
// less than all of p, with [io.EOF] marking a read clipped by the object's
// end — so a caller can adapt it to [io.ReaderAt] with a context of its
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

	var n int64

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

			r, err := c.bucket.NewRangeReader(ctx, key, n, size-n, nil)
			if err != nil {
				return err //nolint:wrapcheck // Wrapped uniformly below.
			}

			defer func() {
				//nolint:errcheck // Read-only body; the copy's result is what matters.
				_ = r.Close()
			}()

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

// errPermanent marks a failure that is none of the store's doing and would
// recur identically on another attempt, so [retryable] refuses it whatever
// code the driver would otherwise leave on it. It exists for the one fault
// that reaches this package from the far side of a transfer: a caller's
// destination refusing the bytes a download delivers (a full disk under an
// unseal target) classifies unknown, which would otherwise re-request the
// whole object only to fail on the same write.
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
