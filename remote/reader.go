package remote

import (
	"context"
	"fmt"
	"io"
)

// ReadAt reads len(p) bytes of the object at key starting at off, in a single
// ranged read. The size argument is the object's total length, as reported by
// [Client.Head]; reads at or past it answer [io.EOF] without a request.
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

	r, err := c.bucket.NewRangeReader(ctx, key, off, want, nil)
	if err != nil {
		if isNotFound(err) {
			return 0, fmt.Errorf("%w: %s", ErrNotFound, key)
		}

		return 0, fmt.Errorf("ranged read %q: %w", key, err)
	}

	defer func() {
		//nolint:errcheck // Read-only body; the ReadFull result is what matters.
		_ = r.Close()
	}()

	n, err := io.ReadFull(r, p[:want])
	if err != nil {
		return n, fmt.Errorf("read %q at %d: %w", key, off, err)
	}

	if int64(n) < int64(len(p)) {
		return n, io.EOF
	}

	return n, nil
}
