// Package config resolves and validates the settings that govern a single
// archive run, so the rest of the program receives one already-checked value
// rather than reaching for the environment or flag tree itself.
//
// The archiving identity is an HCP Terraform API token, read from HCP_TOKEN,
// TFC_TOKEN, or TFE_TOKEN. The API address defaults to https://app.terraform.io.
// An empty organization filter means "every organization the token can see",
// archived in turn. The output directory is the archive root; because resume and
// incremental re-run are driven by the ledger already on disk, pointing a run
// at a directory that already holds an archive is what makes it a resume rather
// than a fresh start.
//
// Beyond those, a run carries a workspace-concurrency level, a progress mode
// (auto, human, json, or quiet) with a reporting interval, an absent-recheck
// toggle that forces re-probing of objects previously recorded as permanently
// gone, and a set of scope toggles for the heavy or optional surfaces (Stacks,
// hold-your-own-key configurations, the deeper registry
// version/platform/binary detail, and the audit trail), each off unless a run
// opts into it.
//
// Configuration holds plain values and performs no I/O beyond reading the
// environment, so it stays a leaf: the command layer binds flags into it and
// the orchestrator maps its fields onto each collaborator, and nothing else
// needs to know where a setting came from.
package config
