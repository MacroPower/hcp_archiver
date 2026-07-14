// Package remotetest provides an in-memory S3-compatible fake implementing
// [remote.S3API], so tests exercise the remote upload, head, and ranged-read
// paths without a network or a real store.
package remotetest

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// Object is one stored object with the metadata the fake serves.
type Object struct {
	// StorageClass is the class the object reports; GLACIER and DEEP_ARCHIVE
	// gate reads behind Restore.
	StorageClass string
	// Restore is the raw x-amz-restore header value, empty when no restore
	// was ever requested.
	Restore string
	// Data is the object's content.
	Data []byte
}

// archived reports whether the object's bytes are parked behind a restore.
func (o Object) archived() bool {
	return o.StorageClass == string(types.StorageClassGlacier) ||
		o.StorageClass == string(types.StorageClassDeepArchive)
}

// restored reports whether a completed restore currently serves the bytes.
func (o Object) restored() bool {
	return strings.Contains(o.Restore, `ongoing-request="false"`)
}

// upload is one in-flight multipart upload.
type upload struct {
	parts map[int32][]byte
	key   string
	class string
}

// Fake is an in-memory S3-compatible store implementing [remote.S3API].
//
// It records the calls it serves (puts, part uploads, aborts, byte ranges)
// so tests can assert how the client drove it, and its error fields inject
// faults. Create instances with [New]. A Fake is safe for concurrent use;
// the transfer manager uploads parts in parallel.
type Fake struct {
	objects map[string]Object
	uploads map[string]*upload

	// PutErr fails every PutObject call.
	PutErr error
	// UploadPartErr fails every UploadPart call, driving the transfer
	// manager down its abort path.
	UploadPartErr error
	// HeadErr fails every HeadObject call.
	HeadErr error
	// GetErr fails every GetObject call.
	GetErr error

	bucket       string
	getRanges    []string
	putChecksums []string
	mu           sync.Mutex
	nextUploadID int
	putCalls     int
	headCalls    int
	completed    int
	aborted      int
}

// New creates a new [Fake] serving bucket; a request naming any other bucket
// answers NoSuchBucket.
func New(bucket string) *Fake {
	return &Fake{
		bucket:  bucket,
		objects: make(map[string]Object),
		uploads: make(map[string]*upload),
	}
}

// SetObject stores obj at key directly, bypassing the upload path.
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

// PutCalls returns how many PutObject calls were served.
func (f *Fake) PutCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.putCalls
}

// HeadCalls returns how many HeadObject calls were served.
func (f *Fake) HeadCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.headCalls
}

// PutChecksums returns the checksum algorithm of each PutObject and
// CreateMultipartUpload call in order, "" for calls that carried none.
func (f *Fake) PutChecksums() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return slices.Clone(f.putChecksums)
}

// GetRanges returns the Range header of each GetObject call in order, "" for
// calls that carried none.
func (f *Fake) GetRanges() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return slices.Clone(f.getRanges)
}

// Completed returns how many multipart uploads completed.
func (f *Fake) Completed() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.completed
}

// Aborted returns how many multipart uploads were aborted.
func (f *Fake) Aborted() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.aborted
}

// OpenUploads returns how many multipart uploads are still in flight,
// neither completed nor aborted — storage a crashed real upload would leak.
func (f *Fake) OpenUploads() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return len(f.uploads)
}

// checkBucket answers NoSuchBucket for a request naming any other bucket.
func (f *Fake) checkBucket(bucket *string) error {
	if bucket == nil || *bucket != f.bucket {
		return &types.NoSuchBucket{}
	}

	return nil
}

// PutObject stores the body as one object.
func (f *Fake) PutObject(
	_ context.Context, in *s3.PutObjectInput, _ ...func(*s3.Options),
) (*s3.PutObjectOutput, error) {
	bucketErr := f.checkBucket(in.Bucket)
	if bucketErr != nil {
		return nil, bucketErr
	}

	data, err := io.ReadAll(in.Body)
	if err != nil {
		return nil, fmt.Errorf("read put body: %w", err)
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	if f.PutErr != nil {
		return nil, f.PutErr
	}

	f.putCalls++
	f.putChecksums = append(f.putChecksums, string(in.ChecksumAlgorithm))
	f.objects[*in.Key] = Object{Data: data, StorageClass: string(in.StorageClass)}

	return &s3.PutObjectOutput{}, nil
}

// CreateMultipartUpload opens a multipart upload and returns its id.
func (f *Fake) CreateMultipartUpload(
	_ context.Context, in *s3.CreateMultipartUploadInput, _ ...func(*s3.Options),
) (*s3.CreateMultipartUploadOutput, error) {
	bucketErr := f.checkBucket(in.Bucket)
	if bucketErr != nil {
		return nil, bucketErr
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	f.nextUploadID++
	id := "upload-" + strconv.Itoa(f.nextUploadID)
	f.putChecksums = append(f.putChecksums, string(in.ChecksumAlgorithm))
	f.uploads[id] = &upload{
		key:   *in.Key,
		class: string(in.StorageClass),
		parts: make(map[int32][]byte),
	}

	return &s3.CreateMultipartUploadOutput{UploadId: &id}, nil
}

// UploadPart stores one part of an open multipart upload.
func (f *Fake) UploadPart(
	_ context.Context, in *s3.UploadPartInput, _ ...func(*s3.Options),
) (*s3.UploadPartOutput, error) {
	bucketErr := f.checkBucket(in.Bucket)
	if bucketErr != nil {
		return nil, bucketErr
	}

	data, err := io.ReadAll(in.Body)
	if err != nil {
		return nil, fmt.Errorf("read part body: %w", err)
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	if f.UploadPartErr != nil {
		return nil, f.UploadPartErr
	}

	up, ok := f.uploads[*in.UploadId]
	if !ok {
		return nil, &types.NoSuchUpload{}
	}

	up.parts[*in.PartNumber] = data
	etag := fmt.Sprintf("etag-%d", *in.PartNumber)

	return &s3.UploadPartOutput{ETag: &etag}, nil
}

// CompleteMultipartUpload assembles the stored parts, in part-number order,
// into one object.
func (f *Fake) CompleteMultipartUpload(
	_ context.Context, in *s3.CompleteMultipartUploadInput, _ ...func(*s3.Options),
) (*s3.CompleteMultipartUploadOutput, error) {
	bucketErr := f.checkBucket(in.Bucket)
	if bucketErr != nil {
		return nil, bucketErr
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	up, ok := f.uploads[*in.UploadId]
	if !ok {
		return nil, &types.NoSuchUpload{}
	}

	var data []byte

	for _, num := range slices.Sorted(maps.Keys(up.parts)) {
		data = append(data, up.parts[num]...)
	}

	f.objects[up.key] = Object{Data: data, StorageClass: up.class}
	delete(f.uploads, *in.UploadId)

	f.completed++

	return &s3.CompleteMultipartUploadOutput{}, nil
}

// AbortMultipartUpload discards an open multipart upload's parts.
func (f *Fake) AbortMultipartUpload(
	_ context.Context, in *s3.AbortMultipartUploadInput, _ ...func(*s3.Options),
) (*s3.AbortMultipartUploadOutput, error) {
	bucketErr := f.checkBucket(in.Bucket)
	if bucketErr != nil {
		return nil, bucketErr
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	delete(f.uploads, *in.UploadId)

	f.aborted++

	return &s3.AbortMultipartUploadOutput{}, nil
}

// HeadObject serves an object's metadata: length, storage class, and the raw
// restore header. An absent object answers the typed NotFound.
func (f *Fake) HeadObject(
	_ context.Context, in *s3.HeadObjectInput, _ ...func(*s3.Options),
) (*s3.HeadObjectOutput, error) {
	bucketErr := f.checkBucket(in.Bucket)
	if bucketErr != nil {
		return nil, bucketErr
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	if f.HeadErr != nil {
		return nil, f.HeadErr
	}

	f.headCalls++

	obj, ok := f.objects[*in.Key]
	if !ok {
		return nil, &types.NotFound{}
	}

	size := int64(len(obj.Data))
	out := &s3.HeadObjectOutput{
		ContentLength: &size,
		StorageClass:  types.StorageClass(obj.StorageClass),
	}

	if obj.Restore != "" {
		out.Restore = &obj.Restore
	}

	return out, nil
}

// GetObject serves an object's bytes, honoring a bytes=start-end range. An
// object parked unrestored in an archival class answers InvalidObjectState,
// as S3 does.
func (f *Fake) GetObject(
	_ context.Context, in *s3.GetObjectInput, _ ...func(*s3.Options),
) (*s3.GetObjectOutput, error) {
	bucketErr := f.checkBucket(in.Bucket)
	if bucketErr != nil {
		return nil, bucketErr
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	if f.GetErr != nil {
		return nil, f.GetErr
	}

	obj, ok := f.objects[*in.Key]
	if !ok {
		return nil, &types.NoSuchKey{}
	}

	if obj.archived() && !obj.restored() {
		return nil, &types.InvalidObjectState{
			StorageClass: types.StorageClass(obj.StorageClass),
		}
	}

	var rangeHeader string

	if in.Range != nil {
		rangeHeader = *in.Range
	}

	f.getRanges = append(f.getRanges, rangeHeader)

	start, end, err := parseRange(rangeHeader, int64(len(obj.Data)))
	if err != nil {
		return nil, err
	}

	body := obj.Data[start : end+1]
	size := int64(len(body))

	return &s3.GetObjectOutput{
		Body:          io.NopCloser(bytes.NewReader(body)),
		ContentLength: &size,
	}, nil
}

// parseRange resolves a bytes=start-end header (end inclusive, possibly
// open-ended) against an object of size bytes; an empty header is the whole
// object.
func parseRange(header string, size int64) (int64, int64, error) {
	if header == "" {
		return 0, size - 1, nil
	}

	spec, ok := strings.CutPrefix(header, "bytes=")
	if !ok {
		return 0, 0, fmt.Errorf("unsupported range %q", header)
	}

	startStr, endStr, ok := strings.Cut(spec, "-")
	if !ok || startStr == "" {
		return 0, 0, fmt.Errorf("unsupported range %q", header)
	}

	start, err := strconv.ParseInt(startStr, 10, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("parse range %q: %w", header, err)
	}

	end := size - 1

	if endStr != "" {
		end, err = strconv.ParseInt(endStr, 10, 64)
		if err != nil {
			return 0, 0, fmt.Errorf("parse range %q: %w", header, err)
		}
	}

	if start > end || start >= size {
		return 0, 0, fmt.Errorf("range %q outside object of %d bytes", header, size)
	}

	return start, min(end, size-1), nil
}
