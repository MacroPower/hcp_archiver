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
	// ModTime is the object's last modification time, reported on a head and
	// on every read the way the backends report theirs. A zero value models a
	// store recording none, and a write through the client leaves it zero;
	// a test proving a resumed download notices a replaced object stores the
	// new version through [Fake.SetObject] with a later one.
	ModTime time.Time
	// Metadata is the object's recorded key-value metadata, nil when the
	// write carried none.
	Metadata map[string]string
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

	// PutErr fails every write at its commit; a positive PutErrN bounds it
	// to the first n commits, modeling a transient fault that heals.
	PutErr error
	// HeadErr fails every attributes read; a positive HeadErrN bounds it to
	// the first n reads.
	HeadErr error
	// ListErr fails every listing page; a positive ListErrN bounds it to the
	// first n pages, and a positive ListErrAfter spares that many pages first,
	// so a test can fault a page deep in a walk rather than its opening one.
	ListErr error
	// DeleteErr fails every delete; a positive DeleteErrN bounds it to the
	// first n deletes.
	DeleteErr error
	// RangeErr fails every ranged read at its open; a positive RangeErrN
	// bounds it to the first n reads.
	RangeErr error
	// RangeBodyErr fails every ranged read's body once RangeBodyErrAfter
	// bytes have been delivered, rather than at the open the way RangeErr
	// does; a positive RangeBodyErrN bounds it to the first n reads. It
	// models the failure a reader cannot retry blindly: bytes already
	// reached the caller, so a reopen must resume past them.
	RangeBodyErr error
	// RangeHook, when set, runs at the start of each ranged read with the
	// call's context, serving the same mid-flight cancellation modeling as
	// HeadHook.
	RangeHook func(ctx context.Context)
	// HeadHook, when set, runs at the start of each attributes read with the
	// call's context. A test cancels a context it controls from inside it to
	// model a cancellation surfacing mid-flight; the read then returns the
	// context's error instead of serving the object.
	HeadHook func(ctx context.Context)
	// PutHook, when set, runs as each write commits, before the fake's mutex
	// is taken, so concurrent commits run it concurrently: a test measures
	// in-flight write parallelism from inside it, or cancels a context it
	// controls to model a cancellation surfacing mid-commit.
	PutHook func(ctx context.Context)
	// DeleteHook, when set, runs at the start of each delete with the call's
	// context, serving the same mid-flight cancellation modeling as HeadHook.
	DeleteHook func(ctx context.Context)
	// ListHook, when set, runs at the start of each listing page with the
	// call's context, before the fake's mutex is taken. A test blocks in it to
	// model a mirror whose enumeration runs long, or cancels a context it
	// controls to model a cancellation surfacing mid-listing.
	ListHook func(ctx context.Context)
	// CopyErr fails every server-side copy; a positive CopyErrN bounds it to
	// the first n copies.
	CopyErr error
	// DeleteErrKeys, when non-empty, confines DeleteErr (and DeleteErrN's
	// budget) to the named keys, so a test can fail some of a fan-out's keys
	// while the rest settle.
	DeleteErrKeys []string

	ranges  []Range
	deleted []string
	copies  []CopyRecord
	mu      sync.Mutex

	// RangeBodyErrAfter is how many bytes a ranged read delivers before
	// RangeBodyErr fires; zero fails the body before its first byte.
	RangeBodyErrAfter int64

	// ListErrAfter is how many listing pages ListErr spares before it fires.
	ListErrAfter int

	// PutErrN, HeadErrN, ListErrN, DeleteErrN, RangeErrN, RangeBodyErrN, and
	// CopyErrN bound their error's blast radius to the first n calls; zero
	// keeps the error firing on every call.
	PutErrN       int
	HeadErrN      int
	ListErrN      int
	DeleteErrN    int
	RangeErrN     int
	RangeBodyErrN int
	CopyErrN      int

	// ErrCode, when set, is the [gcerrors.ErrorCode] stamped on every
	// injected fault, so a test can model a driver-classified failure (a
	// permission denial, a failed precondition at commit, even a driver that
	// mistakes a transport fault for NotFound) rather than only an unknown
	// one. A genuinely missing object still classifies NotFound regardless.
	ErrCode gcerrors.ErrorCode

	putCalls       int
	headCalls      int
	listCalls      int
	putFails       int
	headFails      int
	listFails      int
	delFails       int
	rangeFails     int
	rangeBodyFails int
	copyFails      int
}

// CopyRecord is one recorded server-side copy, source to destination.
type CopyRecord struct {
	// Src is the key copied from.
	Src string
	// Dst is the key copied to.
	Dst string
}

// takeFault reports whether an armed fault fires for this call, consuming
// one arming from its bounded budget; a zero limit arms every call. It must
// run under the fake's mutex.
func takeFault(err error, fired *int, limit int) bool {
	if err == nil {
		return false
	}

	if limit <= 0 {
		return true
	}

	if *fired < limit {
		*fired++

		return true
	}

	return false
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

// Copies returns the server-side copies served, in request order.
func (f *Fake) Copies() []CopyRecord {
	f.mu.Lock()
	defer f.mu.Unlock()

	return slices.Clone(f.copies)
}

// ErrorCode classifies the fake's errors: a missing object reports
// [gcerrors.NotFound], and everything else (the injected faults) is unknown
// unless [Fake.ErrCode] stamps a specific code.
func (f *Fake) ErrorCode(err error) gcerrors.ErrorCode {
	if errors.Is(err, errNotFound) {
		return gcerrors.NotFound
	}

	if f.ErrCode != gcerrors.OK {
		return f.ErrCode
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

	if takeFault(f.HeadErr, &f.headFails, f.HeadErrN) {
		return nil, f.HeadErr
	}

	f.headCalls++

	obj, ok := f.objects[key]
	if !ok {
		return nil, fmt.Errorf("%w: %s", errNotFound, key)
	}

	return &driver.Attributes{
		Size:     int64(len(obj.Data)),
		MD5:      obj.MD5,
		ModTime:  obj.ModTime,
		Metadata: maps.Clone(obj.Metadata),
	}, nil
}

// fakeWriter accumulates one write's bytes and commits them at Close.
type fakeWriter struct {
	//nolint:containedctx // The driver contract aborts a write via its context.
	ctx      context.Context
	f        *Fake
	key      string
	md5      []byte
	metadata map[string]string
	body     bytes.Buffer
}

// Write buffers p into the pending object.
func (w *fakeWriter) Write(p []byte) (int, error) {
	return w.body.Write(p) //nolint:wrapcheck // A transparent in-memory buffer.
}

// Close commits the buffered bytes as one object, unless the write's context
// was canceled (the client's abort path) or a fault is injected.
func (w *fakeWriter) Close() error {
	if w.f.PutHook != nil {
		w.f.PutHook(w.ctx)
	}

	err := w.ctx.Err()
	if err != nil {
		return err //nolint:wrapcheck // The driver contract returns ctx.Err().
	}

	w.f.mu.Lock()
	defer w.f.mu.Unlock()

	w.f.putCalls++

	if takeFault(w.f.PutErr, &w.f.putFails, w.f.PutErrN) {
		return w.f.PutErr
	}

	w.f.objects[w.key] = Object{Data: w.body.Bytes(), MD5: w.md5, Metadata: maps.Clone(w.metadata)}

	return nil
}

// NewTypedWriter opens a write to the object at key. The committed object
// records an MD5 digest only when the write carries one, mirroring the real
// backends: the client's Put digests its body, while a parted Upload records
// one only when its caller supplies it. Metadata rides along as given.
func (f *Fake) NewTypedWriter(
	ctx context.Context, key, _ string, opts *driver.WriterOptions,
) (driver.Writer, error) {
	return &fakeWriter{ctx: ctx, f: f, key: key, md5: opts.ContentMD5, metadata: maps.Clone(opts.Metadata)}, nil
}

// fakeReader serves one ranged read from an object's bytes, optionally failing
// its body partway through.
type fakeReader struct {
	io.Reader
	bodyErr error
	attrs   driver.ReaderAttributes
	failAt  int64
	served  int64
}

// Read serves the range's bytes, failing with the injected body error once
// failAt bytes have been delivered. The bytes served before the failure are
// really delivered, which is what a resuming reader has to account for; a
// reader that restarts from zero re-delivers them and can be caught doing it.
func (r *fakeReader) Read(p []byte) (int, error) {
	if r.bodyErr == nil {
		return r.Reader.Read(p) //nolint:wrapcheck // A transparent in-memory reader.
	}

	if r.served >= r.failAt {
		return 0, r.bodyErr
	}

	if int64(len(p)) > r.failAt-r.served {
		p = p[:r.failAt-r.served]
	}

	n, err := r.Reader.Read(p)
	r.served += int64(n)

	return n, err //nolint:wrapcheck // A transparent in-memory reader.
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
	ctx context.Context, key string, offset, length int64, _ *driver.ReaderOptions,
) (driver.Reader, error) {
	if f.RangeHook != nil {
		f.RangeHook(ctx)

		err := ctx.Err()
		if err != nil {
			return nil, err //nolint:wrapcheck // A faked mid-flight cancellation.
		}
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	if takeFault(f.RangeErr, &f.rangeFails, f.RangeErrN) {
		return nil, f.RangeErr
	}

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

	// The attributes describe the whole object the range was cut from, not the
	// span served, which is what lets a resumed download tell a replacement
	// under the key from the object it began reading.
	r := &fakeReader{
		Reader: bytes.NewReader(obj.Data[offset:end]),
		attrs:  driver.ReaderAttributes{Size: size, ModTime: obj.ModTime},
	}

	if takeFault(f.RangeBodyErr, &f.rangeBodyFails, f.RangeBodyErrN) {
		r.bodyErr = f.RangeBodyErr
		r.failAt = f.RangeBodyErrAfter
	}

	return r, nil
}

// listPageSize is how many keys one listing page serves without an explicit
// page size, matching the real backends' thousand-key pages.
const listPageSize = 1000

// ListPaged serves one sorted page of the keys under the requested prefix,
// honoring the page size and continuation token (an opaque start-after key),
// the same scheme gocloud's own memblob driver pages by.
//
// A non-empty delimiter folds every key that carries one past the prefix into
// the common prefix it shares with its siblings, delivered once as an entry
// with IsDir set, the way the real drivers answer a delimited listing. Keys
// with no delimiter past the prefix come through as themselves, interleaved in
// key order.
func (f *Fake) ListPaged(ctx context.Context, opts *driver.ListOptions) (*driver.ListPage, error) {
	if f.ListHook != nil {
		f.ListHook(ctx)
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	f.listCalls++

	if f.listCalls > f.ListErrAfter && takeFault(f.ListErr, &f.listFails, f.ListErrN) {
		return nil, f.ListErr
	}

	pageSize := opts.PageSize
	if pageSize <= 0 {
		pageSize = listPageSize
	}

	after := string(opts.PageToken)

	// Folding runs before the page is cut, so the continuation token is a
	// folded key and the resume comparison below skips exactly the entries
	// already served: a token of "<prefix>orgA/" excludes itself and admits
	// "<prefix>orgB/". Folding a page that was already cut would repeat a
	// common prefix straddling the boundary and leave pages of wildly
	// different sizes.
	entries := f.foldedEntries(opts.Prefix, opts.Delimiter)

	keys := make([]string, 0, len(entries))

	for _, key := range slices.Sorted(maps.Keys(entries)) {
		if key > after {
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
		if entries[key] {
			page.Objects = append(page.Objects, &driver.ListObject{Key: key, IsDir: true})

			continue
		}

		obj := f.objects[key]
		page.Objects = append(page.Objects, &driver.ListObject{
			Key:  key,
			Size: int64(len(obj.Data)),
			MD5:  obj.MD5,
		})
	}

	return page, nil
}

// foldedEntries maps every listing entry the prefix and delimiter select to
// whether it is a common prefix rather than an object. An empty delimiter
// selects the objects themselves, so the recursive listing folds nothing.
// Call this while holding the fake's mutex.
func (f *Fake) foldedEntries(prefix, delim string) map[string]bool {
	entries := make(map[string]bool, len(f.objects))

	for key := range f.objects {
		if !strings.HasPrefix(key, prefix) {
			continue
		}

		if delim == "" {
			entries[key] = false

			continue
		}

		rest := strings.TrimPrefix(key, prefix)

		segment, _, folds := strings.Cut(rest, delim)
		if !folds {
			entries[key] = false

			continue
		}

		entries[prefix+segment+delim] = true
	}

	return entries
}

// Delete removes the object at key, recording it; an absent key reports
// not-found, per the driver contract, which the client settles as a no-op.
func (f *Fake) Delete(ctx context.Context, key string) error {
	if f.DeleteHook != nil {
		f.DeleteHook(ctx)

		err := ctx.Err()
		if err != nil {
			return err //nolint:wrapcheck // A faked mid-flight cancellation.
		}
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	if (len(f.DeleteErrKeys) == 0 || slices.Contains(f.DeleteErrKeys, key)) &&
		takeFault(f.DeleteErr, &f.delFails, f.DeleteErrN) {
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

// Copy duplicates the object at srcKey to dstKey, recording the pair; an
// absent source reports not-found.
func (f *Fake) Copy(_ context.Context, dstKey, srcKey string, _ *driver.CopyOptions) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if takeFault(f.CopyErr, &f.copyFails, f.CopyErrN) {
		return f.CopyErr
	}

	obj, ok := f.objects[srcKey]
	if !ok {
		return fmt.Errorf("%w: %s", errNotFound, srcKey)
	}

	f.objects[dstKey] = Object{
		Data:     slices.Clone(obj.Data),
		MD5:      slices.Clone(obj.MD5),
		ModTime:  obj.ModTime,
		Metadata: maps.Clone(obj.Metadata),
	}
	f.copies = append(f.copies, CopyRecord{Src: srcKey, Dst: dstKey})

	return nil
}

// SignedURL is unsupported; the client never signs.
func (f *Fake) SignedURL(context.Context, string, *driver.SignedURLOptions) (string, error) {
	return "", errors.ErrUnsupported
}
