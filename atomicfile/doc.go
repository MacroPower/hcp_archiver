// Package atomicfile provides crash-safe writes to the local filesystem.
//
// A write goes to a temporary path in the destination directory, is flushed to
// stable storage, and is then renamed into place. Because the rename is atomic,
// a reader never observes a half-written file, and an interrupted process never
// leaves a truncated one that looks complete, whether a partial state blob or
// configuration tarball in the object tree or a partial ledger snapshot. Any
// directories created for the destination are flushed as well, so a first write
// into a fresh subtree is durable, not just the file. That guarantee lets an
// interrupted run resume safely: every file that exists is whole.
//
// The package offers two crash-safe primitives. Whole-file replacement ([Write]
// and its byte-slice and streaming forms) stages, flushes, and renames as
// above. Append-only durable records ([Append]) commit each record on a newline
// boundary and drop any torn trailing fragment a crash leaves past it, in place
// and without a rename. The on-disk object tree rides the first; the durable run
// ledger uses both, taking the rename path for its compacted snapshot and the
// append path for the log beside it, so neither reimplements these. Keeping this
// a standalone leaf means the ledger can obtain crash-safety without depending
// on the archive tree, and the archive tree without depending on the ledger.
package atomicfile
