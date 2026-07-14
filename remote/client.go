package remote

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
)

// Sentinel errors reported by [New] and the [Client] methods.
var (
	// ErrMissingBucket indicates a [Config] that names no bucket.
	ErrMissingBucket = errors.New("remote bucket is required")

	// ErrNotFound indicates the object at a key does not exist in the store.
	ErrNotFound = errors.New("object not found in remote store")

	// ErrRestoreRequired indicates the object sits unrestored in an archival
	// storage class (GLACIER, DEEP_ARCHIVE), whose bytes cannot be read until
	// a restore is requested and completes.
	ErrRestoreRequired = errors.New("object requires a restore from its archival storage class")
)

// S3API is the slice of the S3 service a [Client] calls: the transfer
// manager's upload operations plus the head and ranged-get reads. The real
// [*s3.Client] implements it; tests inject an in-memory fake through
// [WithS3API].
type S3API interface {
	manager.UploadAPIClient

	// HeadObject reads an object's metadata without its body.
	HeadObject(ctx context.Context, in *s3.HeadObjectInput, opts ...func(*s3.Options)) (*s3.HeadObjectOutput, error)
	// GetObject reads an object, honoring a byte-range request.
	GetObject(ctx context.Context, in *s3.GetObjectInput, opts ...func(*s3.Options)) (*s3.GetObjectOutput, error)
}

// Client reads and writes sealed bundles in one S3-compatible bucket.
//
// A Client is safe for concurrent use; the archiver shares one across every
// organization it archives. Create instances with [New].
type Client struct {
	api S3API
	//nolint:staticcheck // The manager module is deprecated in favor of
	// transfermanager, which is still v0.x; migrate once it stabilizes.
	uploader *manager.Uploader
	cfg      Config
}

// Option configures a [Client] passed to [New].
//
// Options of this type:
//   - [WithS3API]
type Option func(*Client)

// WithS3API injects the S3 API implementation the client calls, replacing
// the one built from the AWS SDK default chain; tests inject an in-memory
// fake through it. A nil api keeps the default. It returns an [Option].
func WithS3API(api S3API) Option {
	return func(c *Client) {
		if api != nil {
			c.api = api
		}
	}
}

// New creates a new [Client] over cfg.
//
// Unless [WithS3API] supplies one, the S3 client is built from the AWS SDK
// default chain (environment variables, shared configuration, an instance or
// task role), applying cfg's endpoint, region, path-style, and checksum
// settings; credentials never come from cfg. It returns [ErrMissingBucket]
// when cfg names no bucket.
func New(ctx context.Context, cfg Config, opts ...Option) (*Client, error) {
	if cfg.Bucket == "" {
		return nil, ErrMissingBucket
	}

	c := &Client{cfg: cfg}

	for _, opt := range opts {
		opt(c)
	}

	if c.api == nil {
		api, err := newSDKClient(ctx, cfg)
		if err != nil {
			return nil, err
		}

		c.api = api
	}

	//nolint:staticcheck // See the uploader field's deprecation note.
	c.uploader = manager.NewUploader(c.api, func(u *manager.Uploader) {
		if cfg.PartSize > 0 {
			u.PartSize = cfg.PartSize
		}

		if cfg.Concurrency > 0 {
			u.Concurrency = cfg.Concurrency
		}
	})

	return c, nil
}

// newSDKClient builds the real S3 client from the SDK default chain and cfg's
// endpoint, region, path-style, and checksum settings.
func newSDKClient(ctx context.Context, cfg Config) (S3API, error) {
	var loadOpts []func(*awsconfig.LoadOptions) error

	if cfg.Region != "" {
		loadOpts = append(loadOpts, awsconfig.WithRegion(cfg.Region))
	}

	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, loadOpts...)
	if err != nil {
		return nil, fmt.Errorf("load aws config: %w", err)
	}

	return s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		if cfg.Endpoint != "" {
			o.BaseEndpoint = aws.String(cfg.Endpoint)
		}

		o.UsePathStyle = cfg.ForcePathStyle

		// Some S3-compatible stores reject the flexible-checksum headers;
		// when checksums are off, calculate and validate them only where the
		// API requires one.
		if cfg.DisableChecksums {
			o.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
			o.ResponseChecksumValidation = aws.ResponseChecksumValidationWhenRequired
		}
	}), nil
}

// ObjectInfo describes one stored object as observed by [Client.Head].
type ObjectInfo struct {
	// StorageClass is the class the object is stored in, empty when the
	// store reports none (AWS omits it for STANDARD).
	StorageClass string
	// Size is the object's length in bytes.
	Size int64
	// Archived reports an archival storage class (GLACIER, DEEP_ARCHIVE)
	// whose bytes cannot be read without a restore.
	Archived bool
	// Restored reports a completed, still-valid restore of an archived
	// object, during which its bytes read normally.
	Restored bool
}

// Head reads the object's metadata at key. An absent object returns
// [ErrNotFound].
func (c *Client) Head(ctx context.Context, key string) (ObjectInfo, error) {
	out, err := c.api.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(c.cfg.Bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		if isNotFound(err) {
			return ObjectInfo{}, fmt.Errorf("%w: %s", ErrNotFound, key)
		}

		return ObjectInfo{}, fmt.Errorf("head %q: %w", key, err)
	}

	info := ObjectInfo{
		StorageClass: string(out.StorageClass),
		Archived:     archivalClass(out.StorageClass),
		Restored:     restoreComplete(out.Restore),
	}

	if out.ContentLength != nil {
		info.Size = *out.ContentLength
	}

	return info, nil
}

// Exists reports whether an object is present at key, returning its metadata
// when it is. Unlike [Client.Head], an absent object is a false report, not
// an error.
func (c *Client) Exists(ctx context.Context, key string) (bool, ObjectInfo, error) {
	info, err := c.Head(ctx, key)

	switch {
	case errors.Is(err, ErrNotFound):
		return false, ObjectInfo{}, nil
	case err != nil:
		return false, ObjectInfo{}, err
	}

	return true, info, nil
}

// Upload streams r to the object at key, letting the transfer manager split
// a large body into a concurrent multipart upload; the parts of an upload
// that dies midway are aborted rather than left to accrue storage (a bucket
// lifecycle rule aborting incomplete multipart uploads still catches the
// parts a crash strands). Unless checksums are disabled, the body carries a
// SHA-256 checksum the server validates on receipt, and the configured
// storage class is applied.
//
// The size argument does not bound the upload — the bytes streamed are
// whatever r yields — it only grows the part size when needed so a very
// large body still fits the transfer manager's part-count ceiling.
func (c *Client) Upload(ctx context.Context, key string, r io.Reader, size int64) error {
	input := &s3.PutObjectInput{
		Bucket: aws.String(c.cfg.Bucket),
		Key:    aws.String(key),
		Body:   r,
	}

	if c.cfg.StorageClass != "" {
		input.StorageClass = types.StorageClass(c.cfg.StorageClass)
	}

	if !c.cfg.DisableChecksums {
		input.ChecksumAlgorithm = types.ChecksumAlgorithmSha256
	}

	//nolint:staticcheck // See the uploader field's deprecation note.
	_, err := c.uploader.Upload(ctx, input, func(u *manager.Uploader) {
		if need := partSizeFor(size); need > u.PartSize {
			u.PartSize = need
		}
	})
	if err != nil {
		return fmt.Errorf("upload %q: %w", key, err)
	}

	return nil
}

// partSizeFor returns the smallest part size that fits a body of size bytes
// within the transfer manager's part-count ceiling.
func partSizeFor(size int64) int64 {
	parts := int64(manager.MaxUploadParts)

	return (size + parts - 1) / parts
}

// archivalClass reports whether class parks an object's bytes behind a
// restore: GLACIER (flexible retrieval) and DEEP_ARCHIVE. GLACIER_IR serves
// reads directly and is not archival in this sense.
func archivalClass(class types.StorageClass) bool {
	return class == types.StorageClassGlacier || class == types.StorageClassDeepArchive
}

// restoreComplete reports whether the x-amz-restore header records a
// completed restore, which it marks with ongoing-request="false"; an ongoing
// restore carries "true" and no header means none was requested.
func restoreComplete(restore *string) bool {
	return restore != nil && strings.Contains(*restore, `ongoing-request="false"`)
}

// isNotFound classifies the S3 error shapes that mean "no such object": the
// typed NotFound (HeadObject) and NoSuchKey (GetObject) errors, plus the bare
// API error codes some compatible stores answer with.
func isNotFound(err error) bool {
	var (
		notFound  *types.NotFound
		noSuchKey *types.NoSuchKey
		apiErr    smithy.APIError
	)

	switch {
	case errors.As(err, &notFound), errors.As(err, &noSuchKey):
		return true
	case errors.As(err, &apiErr):
		code := apiErr.ErrorCode()

		return code == "NotFound" || code == "NoSuchKey"

	default:
		return false
	}
}
