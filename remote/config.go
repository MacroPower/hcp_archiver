package remote

import (
	"path"
	"strings"
)

// MarkerName is the filename of the organization-root marker recording where
// the organization's archive is mirrored, written beside org.json.
const MarkerName = ".remote.json"

// MarkerVersion is the marker schema version this build writes and the newest
// it understands. A marker is read by binaries built long after the archive
// was written, so the version is the one escape hatch for changing its shape:
// a reader rejects a marker whose recorded version is greater than this.
const MarkerVersion = 1

// Config describes how to reach the S3-compatible backend the archive is
// mirrored to: transport settings plus transfer tuning, never write policy —
// the storage class of a write is an argument to [Client.Upload] and
// [Client.Put], chosen by the caller per motion.
//
// It carries no credentials: a [Client] authenticates through the AWS SDK
// default chain, so nothing secret ever lives in a configuration file.
type Config struct {
	// Bucket names the bucket bundles are stored in.
	Bucket string
	// Prefix is an optional key prefix every object key composes under.
	Prefix string
	// Endpoint overrides the S3 endpoint URL for compatible stores (MinIO,
	// R2, Ceph RGW); empty resolves the AWS default for the region.
	Endpoint string
	// Region is the bucket's region; empty defers to the SDK default chain.
	Region string
	// PartSize is the multipart upload part size in bytes; zero takes the
	// transfer manager's default.
	PartSize int64
	// Concurrency is the number of upload parts in flight per bundle; zero
	// takes the transfer manager's default.
	Concurrency int
	// ForcePathStyle addresses the bucket as a path segment rather than a
	// virtual host, the shape MinIO and Ceph RGW expect.
	ForcePathStyle bool
	// DisableChecksums turns off the flexible-checksum headers on writes,
	// for compatible stores that reject them. With checksums off the remote
	// verify gate is existence and size alone.
	DisableChecksums bool
}

// Key composes the object key for an archive-relative path within one
// organization's tree: <Prefix>/<org>/<relPath>, mirroring the local layout
// so a bucket listing reads like the archive. An empty prefix drops its
// segment.
func (cfg Config) Key(org, relPath string) string {
	return strings.TrimPrefix(path.Join("/", cfg.Prefix, org, relPath), "/")
}

// Marker is the read-relevant slice of a [Config], persisted at an
// organization's archive root under [MarkerName] so a later viewer can reach
// the offloaded bundles without the original archive configuration. Like the
// [Config] it derives from, it never carries a credential.
type Marker struct {
	// Bucket names the bucket the bundles were offloaded to.
	Bucket string `json:"bucket"`
	// Prefix is the key prefix the bundles were written under.
	Prefix string `json:"prefix,omitempty"`
	// Endpoint is the S3 endpoint override the bundles were written through.
	Endpoint string `json:"endpoint,omitempty"`
	// Region is the bucket's region.
	Region string `json:"region,omitempty"`
	// Version is the marker schema version, [MarkerVersion] when written by
	// this build; zero in markers from builds that predate versioning, which
	// read as version-1 shapes.
	Version int `json:"version"`
	// ForcePathStyle records path-style bucket addressing.
	ForcePathStyle bool `json:"forcePathStyle,omitempty"`
}

// Marker extracts the read-relevant fields of the [Config].
func (cfg Config) Marker() Marker {
	return Marker{
		Version:        MarkerVersion,
		Bucket:         cfg.Bucket,
		Prefix:         cfg.Prefix,
		Endpoint:       cfg.Endpoint,
		Region:         cfg.Region,
		ForcePathStyle: cfg.ForcePathStyle,
	}
}

// Config expands the marker back into the [Config] a read-side [Client] is
// built from.
func (m Marker) Config() Config {
	return Config{
		Bucket:         m.Bucket,
		Prefix:         m.Prefix,
		Endpoint:       m.Endpoint,
		Region:         m.Region,
		ForcePathStyle: m.ForcePathStyle,
	}
}
