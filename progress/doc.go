// Package progress reports the live status of an archive run to standard error,
// because a full-organization archive is a long, mostly-I/O job that must show
// forward motion rather than sit silent until it finishes.
//
// The human mode splits on the terminal. On an interactive terminal it drives a
// Bubble Tea panel pinned to the bottom of the screen: a spinner, the current
// phase, the progress bar (a marquee until the phase is determinate) with its
// percent and, once an estimate exists, a remaining time, and the target on the
// first line; the colored per-status counts on a second line; and bytes, rate,
// and elapsed time on a closing third line. The panel's rate averages a
// short trailing window, so it reflects current throughput and visibly drops on
// a stall. A reporter built with [WithRateStatus] also shows the client's
// adaptive request rate beside the byte rate; while a rate-limit cooldown
// pauses launches, an amber paused readout marks the wait (a cooldown parks
// every in-flight request, so without it the run would just look stuck); and
// once any rate limiting has been observed, an amber running total of 429
// responses appears, so a slowed rate explains itself. Logfmt carries the
// same as rps=, paused=, and rateLimited=, JSON as requestsPerSecond,
// pausedMs, and rateLimited, and the closing summary keeps the run's
// rate-limited total.
//
// Two rules keep the panel visually stable. The frame never shrinks: the
// panel pads to the tallest frame it has rendered, because the inline
// renderer erases and resizes on a shrinking frame, which corrupts the screen
// when a log line is inserted at the same moment. And nothing writes to the
// terminal beside the renderer: log output routes through a [LogSink] that
// queues lines for the panel to drain from inside its own event loop, printed
// in order, hard-wrapped to the terminal width, and batched, so log lines
// scroll in scrollback above the panel instead of corrupting it; lines still
// queued when the panel stops flush to the sink's fallback writer, so none
// are lost. Off a terminal (a pipe,
// a redirect, or a test buffer) the same signal falls back to a logfmt line.
// The machine mode ([config.ProgressModeJSON]) is one JSON object per line for
// wrapping in CI or a watcher. The mode defaults to the panel on an interactive
// terminal and to quiet otherwise.
//
// The panel's bar tracks unit progress, a per-phase weighted count the archiver
// sets and advances, distinct from the object tally. During the workspaces phase
// each workspace is weighted by one plus its probed run and state-version
// counts, so the bar reflects real work and reaches 100% as the last workspace
// finishes; phases with no cheap pre-count show a spinner instead. Long work
// items register as a [Task], whose unit advances flow into the phase bar as
// they land, so one huge workspace cannot park the bar for its whole duration.
// While tasks are in flight, the panel lists each one under the combined phase
// bar on its own line (bar, percent, unit fraction, and name), in registration
// order so rows hold still, and caps the list to a screenful with the overflow
// counted. The target stays empty while tasks name the work: workspaces archive
// concurrently, so no single name is the target. The line-oriented modes stay
// single-line and report the item with the most remaining work: logfmt as
// completed=x/y with an eta once units have landed (plus task=, taskCompleted=,
// and tasks= while items are in flight), and JSON as phaseTotal and
// phaseCompleted with task, taskTotal, taskCompleted, and tasksActive, each
// present only while determinate. A final summary prints when the run ends
// with the totals per status class and the wall time; the archiver logs each
// object still errored or forbidden in full just above it, so failure text is
// never truncated to fit the panel.
//
// The per-status counts are cumulative: they reflect every object the ledger has
// recorded by its current status, across runs, so a resumed run opens with the
// objects a prior run already settled rather than climbing from zero. Such a run
// is tagged "resumed" in every form (a dimmed marker on the panel, resumed=true
// in logfmt, a resumed field in JSON). The bytes, rate, and elapsed figures stay
// per-run, since they measure this run's download activity.
//
// The reporter only reads the tally the ledger already maintains and formats it,
// so the per-status counts an operator sees always match what the ledger has
// recorded.
package progress
