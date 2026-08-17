// Package seal coalesces an archive's frozen cold artifacts two ways so a large
// archive keeps far fewer files on disk. [Seal] packs bulk artifacts into a
// compressed, indexed zip bundle beside a loose, greppable sidecar index;
// [Rollup] folds small immutable metadata into an append-only, greppable NDJSON
// roll-up.
//
// A bundle is a zip (chosen over a tar.gz for its central directory, which is a
// built-in member index, its per-member framing, which isolates corruption to
// one member, and its mixed per-member methods, which let one container store
// raw state uncompressed while deflating logs). Beside each bundle sits a plain
// sidecar: one JSON line per member recording the member's archive-relative name,
// its bundle, size, method, and CRC-32 and SHA-256 digests, so a search over the
// loose metadata resolves a hit to a bundle and member without opening the zip.
//
// Sealing is verify-before-delete: the bundle is written durably, every member
// is read back and checked against its recorded digest, the sidecar is written
// beside it, and only then are the loose sources removed. The loose files stay
// canonical until the bundle is proven intact, so an interrupted seal loses
// nothing and simply re-runs. Members marked to store rather than compress keep
// their bytes contiguous and uncompressed in the zip, so a raw state blob stays
// greppable on disk and readable with nothing but unzip years from now.
//
// A roll-up is the counterpart for small immutable metadata: [Rollup] appends
// each file's content, keyed by its archive-relative name, as one JSON line to
// an append-only NDJSON, then removes the loose source. The content is carried
// byte for byte as an escaped JSON string encoded with HTML escaping off, so an
// extract reproduces the file and a raw search still finds a token; a source
// whose bytes are not valid UTF-8 cannot be carried losslessly and is refused
// with [ErrContentNotUTF8]. The append is flushed and read back before the
// source is removed, and a crash between the two re-folds an identical line a
// reader dedupes by path, so an interrupted roll-up loses nothing.
package seal
