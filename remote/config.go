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

// Config describes how to reach the object-store backend the archive is
// mirrored to: the bucket URL plus transfer tuning.
//
// It carries no credentials: the URL selects a backend by scheme, and each
// backend authenticates through its provider's default chain (the AWS SDK
// chain for s3://, Azure's environment variables or DefaultAzureCredential
// for azblob://), so nothing secret ever lives in a configuration file.
type Config struct {
	// URL locates the bucket in gocloud.dev form, selecting the backend by
	// scheme: "s3://bucket?region=us-east-1" (AWS S3, or a compatible store
	// via endpoint and use_path_style query parameters), "azblob://container"
	// (Azure Blob Storage), or "file:///path" (a local directory tree).
	URL string
	// Prefix is an optional key prefix every object key composes under.
	Prefix string
	// PartSize is the upload part size in bytes for backends that split a
	// large body into parts; zero takes the backend's default. A positive
	// size is floored at S3's 5 MiB multipart minimum, and very large bodies
	// grow it further to fit the backend's part-count ceiling.
	PartSize int64
	// Concurrency is the number of upload parts in flight per bundle; zero
	// takes the backend's default.
	Concurrency int
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
	// URL is the gocloud.dev bucket URL the bundles were offloaded to.
	URL string `json:"url"`
	// Prefix is the key prefix the bundles were written under.
	Prefix string `json:"prefix,omitempty"`
	// Version is the marker schema version, [MarkerVersion] when written by
	// this build.
	Version int `json:"version"`
}

// Marker extracts the read-relevant fields of the [Config].
func (cfg Config) Marker() Marker {
	return Marker{
		Version: MarkerVersion,
		URL:     cfg.URL,
		Prefix:  cfg.Prefix,
	}
}

// Config expands the marker back into the [Config] a read-side [Client] is
// built from.
func (m Marker) Config() Config {
	return Config{
		URL:    m.URL,
		Prefix: m.Prefix,
	}
}
