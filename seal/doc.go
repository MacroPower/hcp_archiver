// Package seal packs an archive's frozen cold artifacts into compressed, indexed
// zip bundles, so a large archive keeps far fewer files on disk while its
// metadata stays loose and greppable.
//
// A bundle is a zip (chosen over a tar.gz for its central directory, which is a
// built-in member index, its per-member framing, which isolates corruption to
// one member, and its mixed per-member methods, which let one container store
// raw state uncompressed while deflating logs). Beside each bundle sits a plain
// sidecar: one JSON line per member recording the member's archive-relative name,
// its bundle, size, method, and CRC-32 and SHA-256 digests, so a search over the
// loose metadata resolves a hit to a bundle and member without opening the zip.
//
// Sealing is verify-before-delete: the bundle and sidecar are written durably,
// every member is read back and checked against its recorded digest, and only
// then are the loose sources removed. The loose files stay canonical until the
// bundle is proven intact, so an interrupted seal loses nothing and simply
// re-runs. Members marked to store rather than compress keep their bytes
// contiguous and uncompressed in the zip, so a raw state blob stays greppable on
// disk and readable with nothing but unzip years from now.
package seal
