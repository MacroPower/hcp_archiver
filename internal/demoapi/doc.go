// Package demoapi serves the fictional HCP Terraform organization the
// documentation recordings archive.
//
// The recordings in docs/tapes have to show `hcp_archiver run` against a
// populated organization without reaching HCP Terraform and without publishing
// anyone's real one, so this serves a stand-in: one organization whose
// projects, workspaces, runs, and state history derive from a fixed seed and
// one fixed clock, over the slice of the API a default archive run reads. The
// archive the recordings then browse is the product of a real collection
// rather than a tree written by hand: the archiver walked every collector to
// build it, sealed its frozen history itself, and left a ledger behind.
//
// A [Server] answers on a listener the caller binds, because the artifact
// download URLs it advertises must be absolute and only a bound listener knows
// the address they resolve against. [WithChaos] turns on a deterministic
// failure injector, which gives the run recording its rate-limit pauses and
// its retries; the browsing recordings collect their archive with it off.
//
// It is a documentation tool, not a fake for tests of the archiver's behavior:
// it accepts any non-empty token, ignores most query parameters, and answers
// only what a default run asks for.
package demoapi
