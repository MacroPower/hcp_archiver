package store

import "strings"

// RemoteStubSuffix is appended to an archive-relative path to name the stub
// left in place of an evicted object: an evicted
// config-versions/<id>.tar.gz leaves config-versions/<id>.tar.gz.remote.json
// beside where it was.
//
// It is the eviction trace for the one cold surface that leaves nothing else
// behind. A sealed bundle's sidecar index survives its zip, so the readers can
// see that the object exists and lives remotely; a configuration-version
// tarball has no such companion, and without a stub its removal is
// indistinguishable from never having been archived.
//
// The spelling matches the organization root's remote marker by design, since
// both are about the mirror, one recording where it is and one recording that
// an object's bytes went there. The two never collide: the marker is matched
// as a whole file name, the stub only ever as a suffix over an archived
// object's path.
const RemoteStubSuffix = ".remote.json"

// RemoteStubVersion is the stub schema version this build writes and the
// newest it understands. A reader that meets a newer stub cannot trust its
// fields and omits the object from listings rather than reporting a size it
// may have misread.
const RemoteStubVersion = 1

// RemoteStub is the record a [RemoteStubSuffix] file carries: enough for a
// reader to list the evicted object at its true size without consulting the
// ledger or the network, and enough for a later verify to have something to
// compare the remote copy against.
//
// The mirror's location is deliberately absent. It is already recorded once,
// at the organization root, and duplicating it per object would let the two
// drift.
type RemoteStub struct {
	// SHA256 is the lowercase hex SHA-256 of the evicted content, or empty
	// when the proof that settled the object recorded none.
	SHA256 string `json:"sha256,omitempty"`
	// Size is the evicted content's length in bytes.
	Size int64 `json:"size"`
	// Version is the stub schema version, [RemoteStubVersion] when written by
	// this build.
	Version int `json:"version"`
}

// RemoteStubPath returns the archive-relative path of the stub standing in for
// the object at relPath.
func RemoteStubPath(relPath string) string {
	return relPath + RemoteStubSuffix
}

// RemoteStubTarget returns the archive-relative path of the object a stub
// stands in for, and whether relPath names a stub at all.
//
// Only a stub over a configuration-version tarball is recognized, matching the
// only surface eviction writes one for. A file named like a stub anywhere else
// is an ordinary archived object: reading it as a stub would hide it from
// listings and invent an entry at a path holding nothing.
func RemoteStubTarget(relPath string) (string, bool) {
	target, ok := strings.CutSuffix(relPath, RemoteStubSuffix)
	if !ok || !IsConfigTarball(target) {
		return "", false
	}

	return target, true
}

// IsConfigTarball reports whether an archive-relative path names an org-wide
// configuration-version tarball, the eviction surface that leaves no companion
// index behind (see [RemoteStubSuffix]).
func IsConfigTarball(relPath string) bool {
	return strings.HasPrefix(relPath, ConfigVersionsDirName+"/") && strings.HasSuffix(relPath, ".tar.gz")
}

// IsBundleZip reports whether an archive-relative path names a sealed bundle
// zip: a .zip beneath a [BundlesDirName] segment. It is the other eviction
// surface, the one whose sidecar index survives beside it, so its members
// stay readable through ranged requests without the zip ever coming back.
func IsBundleZip(relPath string) bool {
	if !strings.HasSuffix(relPath, ".zip") {
		return false
	}

	for seg := range strings.SplitSeq(relPath, "/") {
		if seg == BundlesDirName {
			return true
		}
	}

	return false
}
