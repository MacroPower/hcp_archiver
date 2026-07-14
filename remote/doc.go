// Package remote offloads sealed cold bundles to an S3-compatible object
// store and reads them back on demand.
//
// The archive's byte-heavy bundles (a workspace's logs.genNNNN.zip and
// state.genNNNN.zip) are write-once: once a bundle has sealed and verified
// locally, its bytes never change, so it can move to archival object storage
// while the grep-able search layer (loose JSON, roll-ups, sidecar indexes)
// stays on local disk. A [Client] uploads a bundle with server-validated
// checksums, confirms it landed with [Client.Head], and serves later reads
// through [Client.ReadAt], whose ranged GETs let a zip central directory be
// parsed and a single member fetched without downloading the bundle.
//
// The backend is anything speaking the S3 API: AWS S3 itself, or a compatible
// store such as MinIO, Cloudflare R2, or Ceph RGW via [Config.Endpoint] and
// [Config.ForcePathStyle]. Credentials are never part of [Config]; the client
// authenticates through the AWS SDK default chain (environment variables,
// shared configuration, an instance or task role).
//
// Objects parked in an archival storage class (GLACIER, DEEP_ARCHIVE) cannot
// be read until restored; reads of one surface [ErrRestoreRequired] rather
// than blocking. [Marker] records the read-relevant backend settings at an
// organization's archive root so a later viewer can find the bundles without
// the original archive configuration.
package remote
