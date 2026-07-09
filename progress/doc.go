// Package progress reports the live status of an archive run to standard error,
// because a full-organization archive is a long, mostly-I/O job that must show
// forward motion rather than sit silent until it finishes.
//
// The human mode splits on the terminal. On an interactive terminal it drives a
// Bubble Tea panel pinned to the bottom of the screen: a spinner and the current
// phase and target on the first line, colored per-status counts with bytes,
// rate, and elapsed time on the second, and, when the phase is determinate, a
// progress bar. Log output routes through a [LogSink] so log lines scroll in
// scrollback above the panel instead of corrupting it. Off a terminal (a pipe,
// a redirect, or a test buffer) the same signal falls back to a logfmt line.
// The machine mode ([config.ProgressModeJSON]) is one JSON object per line for
// wrapping in CI or a watcher. The mode defaults to the panel on an interactive
// terminal and to quiet otherwise.
//
// The panel's bar tracks unit progress, a per-phase weighted count the archiver
// sets and advances, distinct from the object tally. During the workspaces phase
// each workspace is weighted by one plus its run count, so the bar reflects real
// work and reaches 100% as the last workspace finishes; phases with no cheap
// pre-count show a spinner instead. The logfmt line reports the same as
// completed=x/y and the JSON line as phaseTotal and phaseCompleted, present only
// while a phase is determinate. A final summary prints when the run ends: totals
// per status class, wall time, and anything that errored.
//
// The reporter only reads the tally the ledger already maintains and formats it,
// so the per-status counts an operator sees always match what the ledger has
// recorded.
package progress
