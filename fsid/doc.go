// Package fsid owns the archive tree's physical identity.
//
// The archive supports symlinks inside its tree: an operator can relocate a
// subtree to another volume and leave a link in its place, or leave a rename
// link beside a renamed directory. The filesystem therefore maps many paths
// to one physical directory, and every subsystem that walks or indexes the
// tree must agree on when two paths are the same place — a subsystem that
// re-derives that mapping locally will miss a case another already handles.
//
// This package is the single owner of that mapping. [Canonical] resolves any
// path, existing or not, to the identity of the physical location it denotes,
// fit for use as a map key. [WalkFiles] traverses a tree following symlinked
// directories while visiting every physical directory exactly once. Raw
// [path/filepath.EvalSymlinks] and [path/filepath.WalkDir] are banned outside
// this package by lint, so a new tree consumer cannot quietly grow its own
// aliasing rules.
package fsid
