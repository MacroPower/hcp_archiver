// Package remotetest provides an in-memory bucket driver implementing
// [gocloud.dev/blob/driver.Bucket], so tests exercise the remote upload,
// head, ranged-read, list, and delete paths without a network or a real
// store, with fault injection and call recording no real backend offers.
package remotetest

import (
	"bytes"
	"context"
	"crypto/md5" //nolint:gosec // Content MD5 is the stores' integrity currency, not a security control.
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"gocloud.dev/blob"
	"gocloud.dev/blob/driver"
	"gocloud.dev/gcerrors"
)

// MD5Sum returns the raw MD5 digest of data, the form the store records and
// [remote.ObjectInfo] serves; tests compose expected digests with it.
func MD5Sum(data []byte) []byte {
	//nolint:gosec // Content MD5 is the stores' integrity currency, not a security control.
	sum := md5.Sum(data)

	return sum[:]
}

// Object is one stored object with the metadata the fake serves.
type Object struct {
	// Data is the object's content.
	Data []byte
	// MD5 is the recorded full-object digest, nil when the store recorded
	// none (the shape a parted upload leaves on most backends).
	MD5 []byte
}

// Range is one recorded ranged read: a length of -1 reads to the object's
// end.
type Range struct {
	// Offset is the first byte the read requested.
	Offset int64
	// Length is how many bytes the read requested.
	Length int64
}

// errNotFound is the fake's "no such object" error, classified as
// [gcerrors.NotFound] by [Fake.ErrorCode].
var errNotFound = errors.New("object does not exist")

// Fake is an in-memory bucket driver with fault injection and call
// recording.
//
// It records the calls it serves so tests can assert how the client drove
// it, and its error fields inject faults. Wrap it for a client with
// [Fake.Bucket]. Create instances with [New]. A Fake is safe for concurrent
// use; the sync sweep settles files in parallel.
type Fake struct {
	objects map[string]Object

	// PutErr fails every write at its commit.
	PutErr error
	// HeadErr fails every attributes read.
	HeadErr error
	// ListErr fails every listing page.
	ListErr error
	// DeleteErr fails every delete.
	DeleteErr error
	// HeadHook, when set, runs at the start of each attributes read with the
	// call's context. A test cancels a context it controls from inside it to
	// model a cancellation surfacing mid-flight; the read then returns the
	// context's error instead of serving the object.
	HeadHook func(ctx context.Context)

	ranges    []Range
	deleted   []string
	mu        sync.Mutex
	putCalls  int
	headCalls int
	listCalls int
}

// New creates a new [Fake] holding no objects.
func New() *Fake {
	return &Fake{objects: make(map[string]Object)}
}

// Bucket wraps the fake into the [*blob.Bucket] a client is built over,
// via [remote.WithBucket].
func (f *Fake) Bucket() *blob.Bucket {
	return blob.NewBucket(f)
}

// SetObject stores obj at key directly, bypassing the write path.
func (f *Fake) SetObject(key string, obj Object) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.objects[key] = obj
}

// Object returns the object stored at key and whether one exists.
func (f *Fake) Object(key string) (Object, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()

	obj, ok := f.objects[key]

	return obj, ok
}

// Keys returns the stored object keys, sorted.
func (f *Fake) Keys() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return slices.Sorted(maps.Keys(f.objects))
}

// PutCalls returns how many writes were committed or failed at commit.
func (f *Fake) PutCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.putCalls
}

// HeadCalls returns how many attributes reads were served.
func (f *Fake) HeadCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.headCalls
}

// ListCalls returns how many listing pages were served.
func (f *Fake) ListCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.listCalls
}

// Deleted returns the keys removed by delete calls, in request order.
func (f *Fake) Deleted() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return slices.Clone(f.deleted)
}

// Ranges returns the span of each ranged read in request order.
func (f *Fake) Ranges() []Range {
	f.mu.Lock()
	defer f.mu.Unlock()

	return slices.Clone(f.ranges)
}

// ErrorCode classifies the fake's errors: a missing object reports
// [gcerrors.NotFound], everything else (the injected faults) is unknown.
func (f *Fake) ErrorCode(err error) gcerrors.ErrorCode {
	if errors.Is(err, errNotFound) {
		return gcerrors.NotFound
	}

	return gcerrors.Unknown
}

// As reports no driver-specific types.
func (f *Fake) As(any) bool { return false }

// ErrorAs reports no driver-specific error types.
func (f *Fake) ErrorAs(error, any) bool { return false }

// Close releases nothing; the fake holds only memory.
func (f *Fake) Close() error { return nil }

// Attributes serves an object's metadata: its length and recorded MD5. An
// absent object reports not-found.
func (f *Fake) Attributes(ctx context.Context, key string) (*driver.Attributes, error) {
	if f.HeadHook != nil {
		f.HeadHook(ctx)

		err := ctx.Err()
		if err != nil {
			return nil, err //nolint:wrapcheck // A faked mid-flight cancellation.
		}
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	if f.HeadErr != nil {
		return nil, f.HeadErr
	}

	f.headCalls++

	obj, ok := f.objects[key]
	if !ok {
		return nil, fmt.Errorf("%w: %s", errNotFound, key)
	}

	return &driver.Attributes{
		Size: int64(len(obj.Data)),
		MD5:  obj.MD5,
	}, nil
}

// fakeWriter accumulates one write's bytes and commits them at Close.
type fakeWriter struct {
	//nolint:containedctx // The driver contract aborts a write via its context.
	ctx  context.Context
	f    *Fake
	key  string
	md5  []byte
	body bytes.Buffer
}

// Write buffers p into the pending object.
func (w *fakeWriter) Write(p []byte) (int, error) {
	return w.body.Write(p) //nolint:wrapcheck // A transparent in-memory buffer.
}

// Close commits the buffered bytes as one object, unless the write's context
// was canceled (the client's abort path) or a fault is injected.
func (w *fakeWriter) Close() error {
	err := w.ctx.Err()
	if err != nil {
		return err //nolint:wrapcheck // The driver contract returns ctx.Err().
	}

	w.f.mu.Lock()
	defer w.f.mu.Unlock()

	w.f.putCalls++

	if w.f.PutErr != nil {
		return w.f.PutErr
	}

	w.f.objects[w.key] = Object{Data: w.body.Bytes(), MD5: w.md5}

	return nil
}

// NewTypedWriter opens a write to the object at key. The committed object
// records an MD5 digest only when the write carries one, mirroring the real
// backends: the client's Put digests its body, while a parted Upload records
// none.
func (f *Fake) NewTypedWriter(
	ctx context.Context, key, _ string, opts *driver.WriterOptions,
) (driver.Writer, error) {
	return &fakeWriter{ctx: ctx, f: f, key: key, md5: opts.ContentMD5}, nil
}

// fakeReader serves one ranged read from an object's bytes.
type fakeReader struct {
	io.Reader
	attrs driver.ReaderAttributes
}

// Attributes reports the read's metadata.
func (r *fakeReader) Attributes() *driver.ReaderAttributes { return &r.attrs }

// As reports no driver-specific types.
func (r *fakeReader) As(any) bool { return false }

// Close releases nothing; the reader serves memory.
func (r *fakeReader) Close() error { return nil }

// NewRangeReader serves at most length bytes of the object at key from
// offset, recording the requested span; a negative length reads to the end.
// An absent object reports not-found, and an offset past the object's end
// errs as the real backends' range handling does, so a caller that fails to
// bound its reads cannot pass against the fake.
func (f *Fake) NewRangeReader(
	_ context.Context, key string, offset, length int64, _ *driver.ReaderOptions,
) (driver.Reader, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	obj, ok := f.objects[key]
	if !ok {
		return nil, fmt.Errorf("%w: %s", errNotFound, key)
	}

	size := int64(len(obj.Data))

	if offset > 0 && offset >= size {
		return nil, fmt.Errorf("offset %d outside object of %d bytes", offset, size)
	}

	f.ranges = append(f.ranges, Range{Offset: offset, Length: length})

	end := size

	if length >= 0 {
		end = min(offset+length, size)
	}

	return &fakeReader{
		Reader: bytes.NewReader(obj.Data[offset:end]),
		attrs:  driver.ReaderAttributes{Size: size, ModTime: time.Time{}},
	}, nil
}

// listPageSize is how many keys one listing page serves without an explicit
// page size, matching the real backends' thousand-key pages.
const listPageSize = 1000

// ListPaged serves one sorted page of the keys under the requested prefix,
// honoring the page size and continuation token (an opaque start-after key),
// the same scheme gocloud's own memblob driver pages by.
func (f *Fake) ListPaged(_ context.Context, opts *driver.ListOptions) (*driver.ListPage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.ListErr != nil {
		return nil, f.ListErr
	}

	f.listCalls++

	pageSize := opts.PageSize
	if pageSize <= 0 {
		pageSize = listPageSize
	}

	after := string(opts.PageToken)

	var keys []string

	for _, key := range slices.Sorted(maps.Keys(f.objects)) {
		if strings.HasPrefix(key, opts.Prefix) && key > after {
			keys = append(keys, key)
		}
	}

	page := &driver.ListPage{}

	truncated := len(keys) > pageSize
	if truncated {
		keys = keys[:pageSize]
		page.NextPageToken = []byte(keys[len(keys)-1])
	}

	for _, key := range keys {
		obj := f.objects[key]
		page.Objects = append(page.Objects, &driver.ListObject{
			Key:  key,
			Size: int64(len(obj.Data)),
			MD5:  obj.MD5,
		})
	}

	return page, nil
}

// Delete removes the object at key, recording it; an absent key reports
// not-found, per the driver contract, which the client settles as a no-op.
func (f *Fake) Delete(_ context.Context, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.DeleteErr != nil {
		return f.DeleteErr
	}

	_, ok := f.objects[key]
	if !ok {
		return fmt.Errorf("%w: %s", errNotFound, key)
	}

	delete(f.objects, key)

	f.deleted = append(f.deleted, key)

	return nil
}

// Copy is unsupported; the client never copies.
func (f *Fake) Copy(context.Context, string, string, *driver.CopyOptions) error {
	return errors.ErrUnsupported
}

// SignedURL is unsupported; the client never signs.
func (f *Fake) SignedURL(context.Context, string, *driver.SignedURLOptions) (string, error) {
	return "", errors.ErrUnsupported
}
