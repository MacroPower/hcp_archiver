package remote

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// ReaderAt returns an [io.ReaderAt] over the object at key, issuing one
// ranged GET per ReadAt call. The size argument is the object's total
// length, as reported by [Client.Head]; reads at or past it answer [io.EOF]
// without a request.
//
// The shape suits [archive/zip.NewReader], which parses a bundle's central
// directory from a handful of reads near the end of the object; callers
// extracting a member should read its compressed span in one ReadAt rather
// than stream through a decompressor, which would issue thousands of tiny
// requests. A read of an object sitting unrestored in an archival storage
// class returns [ErrRestoreRequired] rather than blocking on a restore that
// was never requested.
//
// The returned reader is bound to ctx: every ReadAt uses it for its request,
// so canceling ctx retires the reader.
func (c *Client) ReaderAt(ctx context.Context, key string, size int64) io.ReaderAt {
	return &objectReaderAt{ctx: ctx, c: c, key: key, size: size}
}

// objectReaderAt adapts ranged GETs of one object to [io.ReaderAt].
//
// It holds the context it was created under because the [io.ReaderAt]
// contract has nowhere to accept one per call.
type objectReaderAt struct {
	ctx  context.Context //nolint:containedctx // ReadAt has no context parameter.
	c    *Client
	key  string
	size int64
}

// ReadAt reads len(p) bytes at off with a single ranged GET, honoring the
// [io.ReaderAt] contract: it returns a non-nil error whenever it fills less
// than all of p, with [io.EOF] marking a read clipped by the object's end.
func (r *objectReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, fmt.Errorf("read %q: negative offset %d", r.key, off)
	}

	if len(p) == 0 {
		return 0, nil
	}

	if off >= r.size {
		return 0, io.EOF
	}

	want := min(int64(len(p)), r.size-off)

	out, err := r.c.api.GetObject(r.ctx, &s3.GetObjectInput{
		Bucket: aws.String(r.c.cfg.Bucket),
		Key:    aws.String(r.key),
		Range:  aws.String(fmt.Sprintf("bytes=%d-%d", off, off+want-1)),
	})
	if err != nil {
		var invalidState *types.InvalidObjectState

		if errors.As(err, &invalidState) {
			return 0, fmt.Errorf("%w: %s", ErrRestoreRequired, r.key)
		}

		return 0, fmt.Errorf("ranged get %q: %w", r.key, err)
	}

	defer func() {
		//nolint:errcheck // Read-only body; the ReadFull result is what matters.
		_ = out.Body.Close()
	}()

	n, err := io.ReadFull(out.Body, p[:want])
	if err != nil {
		return n, fmt.Errorf("read %q at %d: %w", r.key, off, err)
	}

	if int64(n) < int64(len(p)) {
		return n, io.EOF
	}

	return n, nil
}
