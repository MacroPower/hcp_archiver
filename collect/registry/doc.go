// Package registry archives an organization's private registry: modules,
// providers, and the GPG public keys that sign them.
//
// Module and provider metadata is always captured. A module is stored with its
// versions and last commits and, hydrated through the no-code-modules include,
// its no-code configuration, whose per-version variable options come from a
// separate, still-beta read. A provider is stored with its versions, platforms,
// and shasum and signature material. GPG keys are namespaced rather than a flat
// organization list and are read through the private-listing endpoint scoped to
// the organization's namespace.
//
// The deeper version, platform, and binary detail multiplies request volume, so
// it is gathered only when the registry scope toggle is on. Several of these
// objects resist full capture: some registry types do not enumerate and are
// reachable only by a known id, some endpoints are beta, and neither module
// tarballs nor provider binaries have a typed download method, only a source
// URL or an opaque links map, so those blobs are best-effort while their
// metadata and shasums are kept.
package registry
