// Package remote offloads sealed cold bundles to an object store and reads
// them back on demand.
//
// The archive's byte-heavy bundles (a workspace's logs.genNNNN.zip and
// state.genNNNN.zip) are write-once: once a bundle has sealed and verified
// locally, its bytes never change, so it can move to remote object storage
// while the grep-able search layer (loose JSON, roll-ups, sidecar indexes)
// stays on local disk. A [Client] uploads a bundle with its full-object
// digests recorded as object metadata ([Digests]), confirms it landed with
// [Client.Head] (which serves those digests back for egress-free
// comparison), and serves later reads in two shapes: [Client.ReadAt], whose
// ranged reads let a zip central directory be parsed and a single member
// fetched without downloading the bundle, and [Client.Download], which
// streams a whole object into a writer, for the surfaces whose bytes are the
// object itself rather than a span of one (a configuration tarball).
//
// [Client.Preflight] round-trips a small probe object through the write,
// head, list, ranged-read, and delete motions at startup: the metadata
// digests must read back exactly (the mirror's digest comparisons depend on
// them, so a metadata-dropping store fails the run before any archive work),
// while a backend digest attribute that mismatches the written bytes (an
// SSE-KMS bucket's ETag is hex but no content MD5) just downgrades the client
// to the metadata digests alone.
//
// The mirror is the archive's long-term record, so every operation retries
// a transient store failure under a bounded doubling backoff ([WithRetry],
// on by default) — the same in-run persistence the API transport gives
// fetches — while errors the store pins on the request (an absent key, a
// permission denial, a failed precondition) surface immediately. A
// transport-level failure (a DNS blip, a dial fault) is never trusted as a
// request fault, whatever code the driver stamped on it, so it can neither
// settle a delete nor answer an absence; and each attempt runs under a
// stall watchdog ([WithStallTimeout]) that cancels it after a window with
// no progress, so one wedged connection costs a window, not a hung worker.
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
