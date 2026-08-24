package remote

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// MarkerName is the filename of the organization-root marker recording where
// the organization's archive is mirrored, written beside org.json.
const MarkerName = ".remote.json"

// MarkerVersion is the schema version a settled marker carries. A marker is
// read by binaries built long after the archive was written, so the version
// is the one escape hatch for changing its shape: a reader rejects a marker
// whose recorded version is greater than its ceiling, which is
// [MarkerVersionRestoring], the version a restore stamps mid-flight.
const MarkerVersion = 1

// MarkerVersionRestoring is the marker schema version a restore stamps while
// it holds the tree mid-restore, and the newest version this build reads. The
// bump is deliberate: a build predating restores rejects the marker outright
// at [ReadMarker]'s version ceiling, so no older binary can archive, prune,
// or browse a tree whose data a restore has only partially landed. The
// restore rewrites the marker back to [MarkerVersion] once every file is
// present and verified.
const MarkerVersionRestoring = 2

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
	// Partial records that the local tree beside the marker is a browse cache
	// materialized on demand from the mirror, holding any subset of it, so a
	// reader must union the mirror's inventory into its listings and fall
	// through to it on a miss. The archiver preserves the flag when it
	// rewrites the marker at the start of a run and promotes it to complete
	// only at a fully clean close, once the sweep has proven the local tree
	// accounts for everything the mirror holds.
	Partial bool `json:"partial,omitempty"`
	// Restoring records that a bulk restore from the mirror is in progress or
	// was interrupted: the tree may hold any subset of the restored set, and
	// its ledger state must not be read as evidence of local deletion. While
	// the flag is set the prune refuses to run and the archiver refuses the
	// tree outright; the restore clears it, rewriting the marker at
	// [MarkerVersion], only after every file in the restored set is present
	// and verified. It rides only on markers stamped
	// [MarkerVersionRestoring], so builds that predate it refuse the marker
	// by version instead of ignoring the flag.
	Restoring bool `json:"restoring,omitempty"`
}

// ReadMarker reads and validates the remote marker at an organization's
// archive root, reporting whether one exists. A marker written by a newer
// build is refused per the versioning contract; an absent marker is not an
// error.
func ReadMarker(root string) (Marker, bool, error) {
	//nolint:gosec // The path is composed from the archive root being read.
	data, err := os.ReadFile(filepath.Join(root, MarkerName))

	switch {
	case errors.Is(err, fs.ErrNotExist):
		return Marker{}, false, nil
	case err != nil:
		return Marker{}, false, fmt.Errorf("read remote marker: %w", err)
	}

	var marker Marker

	err = json.Unmarshal(data, &marker)
	if err != nil {
		return Marker{}, false, fmt.Errorf("parse remote marker %q: %w", MarkerName, err)
	}

	if marker.Version > MarkerVersionRestoring {
		return Marker{}, false, fmt.Errorf("remote marker %q is version %d, newer than this build reads (%d)",
			MarkerName, marker.Version, MarkerVersionRestoring)
	}

	return marker, true, nil
}

// Conflicts reports whether the marker records a different mirror location
// than the one other names. A marker recording no bucket (a hand-cleared
// file, the operator's consent to re-record) conflicts with nothing.
func (m Marker) Conflicts(other Marker) bool {
	return m.URL != "" && (m.URL != other.URL || m.Prefix != other.Prefix)
}

// Marker extracts the read-relevant fields of the [Config].
func (cfg Config) Marker() Marker {
	return Marker{
		Version: MarkerVersion,
		URL:     cfg.URL,
		Prefix:  cfg.Prefix,
	}
}

// RestoringMarker builds the marker a restore stamps before its first write:
// [Marker.Restoring] under [MarkerVersionRestoring], with [Marker.Partial]
// set because a mid-restore tree holds an arbitrary subset of the mirror and
// a reader that does open it must union the mirror into its listings.
func (cfg Config) RestoringMarker() Marker {
	return Marker{
		Version:   MarkerVersionRestoring,
		URL:       cfg.URL,
		Prefix:    cfg.Prefix,
		Partial:   true,
		Restoring: true,
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
