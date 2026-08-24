// Package restore reconstructs a local archive's warm layer from its mirror.
//
// The archive's sync pushes local files to the mirror, evicts the cold
// surfaces (sealed-bundle zips and configuration-version tarballs) into it,
// and prunes what nothing local backs. Restore is the missing direction back:
// it materializes, from the mirror alone, everything a healthy run keeps
// local, so an operator who lost a tree recovers the search layer, the
// ledger snapshots, and the identity records without a collection run and
// without contacting the API at all. The evicted surfaces stay remote, as
// they do by design, and so do the files a local archive must never receive
// from a mirror: the ledger's replay log above all, whose restoration beside
// a newer snapshot would replay superseded state (see [collect.Restorable],
// the set's single owner).
//
// A restore is planned first and executed second. [Restorer.Plan] lists the
// mirror, classifies every restorable object against the local tree (absent,
// verified-identical, differing, or in conflict), and returns the work
// without writing anything, which is also the whole of a dry run.
// [Restorer.Pull] executes a plan in a fixed order that keeps every
// intermediate state safe: the restoring marker first (see
// [remote.Marker.Restoring]; while it stands, no run prunes the mirror and
// no archiver opens the tree), then every data file, each digest-verified
// and atomically renamed into place, and only after all of them the ledger
// snapshots, so the tree never holds ledger entries describing files that
// are not on disk. The marker is rewritten to its final form only once every
// file in the set is present and verified; an interrupted restore keeps it,
// and a re-run resumes by digest, downloading only what is missing.
package restore
