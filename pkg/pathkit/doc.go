// Package pathkit holds the archive's lexical path checks: containment,
// overlap, prefix scoping, and root confinement, defined once so every guard
// agrees on what "inside" means.
//
// Every check here is lexical. Symlinks are never resolved; physical identity
// (two spellings of one on-disk location) belongs to the fsid package, and a
// caller that needs it resolves first. The guards built on these checks accept
// that a symlinked path can dodge them, a documented tradeoff at each guard.
//
// # Path conventions
//
// The archive traffics in three string shapes, and a signature names its
// parameters after the shape it expects:
//
//   - absPath: an absolute-physical path in the host's separators, the shape
//     [Contains], [Overlaps], and [ConfineJoin]'s root take.
//   - relPath: an archive-relative forward-slash path, rooted at one
//     organization's directory, the shape [UnderPrefix] and [ConfineSlash]
//     take. The store's builders produce these; seal.ValidName validates an
//     untrusted one.
//   - archivePath: an org-prefixed "<org>/<relPath>" path, the shape the
//     archive browser's cross-organization surfaces accept.
//
// The export package additionally writes target-relative site paths beneath
// its output directory; those never cross into the checks here.
package pathkit
