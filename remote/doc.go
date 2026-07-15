// Package remote offloads sealed cold bundles to an object store and reads
// them back on demand.
//
// The archive's byte-heavy bundles (a workspace's logs.genNNNN.zip and
// state.genNNNN.zip) are write-once: once a bundle has sealed and verified
// locally, its bytes never change, so it can move to remote object storage
// while the grep-able search layer (loose JSON, roll-ups, sidecar indexes)
// stays on local disk. A [Client] uploads a bundle, confirms it landed with
// [Client.Head], and serves later reads through [Client.ReadAt], whose ranged
// reads let a zip central directory be parsed and a single member fetched
// without downloading the bundle. [Client.Preflight] round-trips a small
// probe object through the write, head, list, and delete motions at startup,
// so a misconfigured store surfaces before any archive work rather than
// partway through a run.
//
// The backend is selected by [Config.URL]'s scheme, resolved through
// [gocloud.dev/blob]: s3:// (AWS S3, or a compatible store such as MinIO,
// Cloudflare R2, or Ceph RGW via endpoint and use_path_style query
// parameters), azblob:// (Azure Blob Storage), or file:// (a local directory
// tree). Credentials are never part of [Config]; each backend authenticates
// through its provider's default chain (the AWS SDK chain for s3://, Azure's
// environment variables or DefaultAzureCredential for azblob://).
//
// Every object is written in the store's default storage class or access
// tier. Lifecycle rules that move mirrored objects into an archival tier
// (S3 Glacier, Azure Archive) are unsupported: an object parked behind a
// restore fails its reads with the backend's opaque error.
//
// [Marker] records the read-relevant backend settings at an organization's
// archive root so a later viewer can find the bundles without the original
// archive configuration.
package remote
