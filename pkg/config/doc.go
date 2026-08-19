// Package config resolves and validates the settings that govern a single
// archive run, so the rest of the program receives one already-checked value
// rather than reaching for the environment, a file, or the flag tree itself.
//
// Settings are grouped by how much they vary between runs. The archiving
// identity is an HCP Terraform API token, a secret read from HCP_TOKEN,
// TFC_TOKEN, or TFE_TOKEN and never stored in a file. What and how to archive
// is stable across runs and lives in a YAML [File]: the archive path (its one
// required key) naming the archive root, the API address (default
// https://app.terraform.io), the organizations to archive (empty means every
// organization the token can see, each in turn), the project and workspace
// filters (each empty means everything within each archived organization;
// with both set a workspace must satisfy both), an optional bound on each
// workspace's archived run history (a newest-run count and/or an age window
// written as a [Duration], keeping whichever admits more history when both are
// set), and a set of include toggles for the heavy or optional surfaces (Stacks,
// hold-your-own-key configurations, the deeper registry version/platform/binary
// detail, and the audit trail), each off unless a configuration opts into it.
// [LoadFile] validates the document against a JSON schema generated from the Go
// type and reports any violation against the offending source line, so a bad
// file fails before any network call. Because resume and incremental re-run
// are driven by the ledger already on disk, pointing a run at a directory that
// already holds an archive is what makes it a resume rather than a fresh
// start.
//
// The per-run settings are supplied separately. A progress mode (auto, human,
// json, or quiet) with a reporting interval selects live output, and a
// retry-absent toggle forces re-probing of objects previously recorded as
// absent.
//
// [Config] is the resolved result. It holds plain values and performs no I/O
// beyond reading the environment, so it stays a leaf: the command layer merges
// the file and the flags into it and the orchestrator maps its fields onto each
// collaborator, and nothing else needs to know where a setting came from.
package config
