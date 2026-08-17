// Package cli builds the hcp_archiver command-line interface. It binds flags
// and environment variables into a configuration, constructs the archiver, runs
// it under a signal-aware context, and maps the outcome to an exit code. It
// carries no archiving logic itself; every behavior lives in the packages it
// wires together.
package cli
