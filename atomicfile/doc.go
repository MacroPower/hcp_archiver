// Package atomicfile provides crash-safe writes to the local filesystem.
//
// A write goes to a temporary path in the destination directory, is flushed to
// stable storage, and is then renamed into place. Because the rename is atomic,
// a reader never observes a half-written file, and an interrupted process never
// leaves a truncated one that looks complete, whether a partial state blob or
// configuration tarball in the object tree or a partial ledger. Any directories
// created for the destination are flushed as well, so a first write into a fresh
// subtree is durable, not just the file. That guarantee lets an interrupted run
// resume safely: every file that exists is whole.
//
// Both the on-disk object tree and the durable run ledger persist through this
// one mechanism instead of each reimplementing temp-and-rename. Keeping it a
// standalone leaf means the ledger can obtain crash-safety without depending on
// the archive tree, and the archive tree without depending on the ledger.
package atomicfile
