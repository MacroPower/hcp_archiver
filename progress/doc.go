// Package progress reports the live status of an archive run to standard error,
// because a full-organization archive is a long, mostly-I/O job that must show
// forward motion rather than sit silent until it finishes.
//
// On each interval it renders a snapshot of the ledger tally. The human form is
// a single line naming the current organization, project, and workspace, the
// objects done, errored, and remaining, the bytes downloaded, and the elapsed
// time and a rough rate. The machine form is one JSON object per line (phase,
// counts, current target) for wrapping in CI or a watcher. The mode selects
// between them, defaulting to the human line on an interactive terminal and to
// quiet when standard error is not a terminal. A final summary prints when the
// run ends: totals per status class, wall time, and anything that errored.
//
// The reporter only reads the tally the ledger already maintains and formats it,
// so what an operator sees on screen always matches what the ledger has
// recorded.
package progress
