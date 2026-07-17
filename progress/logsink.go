package progress

import (
	"fmt"
	"io"
	"strings"
	"sync"
)

// maxQueuedLogLines bounds the sink's queue so a panel that has stopped
// draining (a wedged terminal awaiting the reporter's shutdown escalation)
// cannot grow it without limit. The cap is far above any drain-interval burst
// a healthy run produces, so in practice only a wedged panel ever reaches it.
// When the queue is full the oldest lines give way — scrollback semantics,
// the newest lines matter most — and the next batch opens with a marker
// counting what was lost.
const maxQueuedLogLines = 10000

// LogSink queues log lines for the terminal UI while it owns the screen, so
// log output and the live panel funnel through the one renderer instead of
// corrupting each other with interleaved writes.
//
// The reporter activates the sink while the TUI runs and deactivates it when
// the TUI stops; the panel's model consumes the queue from inside the
// program's event loop with a peek-then-commit handshake, so a line leaves
// the sink only once its printing is confirmed. See [LogWriter] for an
// implementation.
type LogSink interface {
	// Activate begins queueing written lines for the panel to print.
	Activate()
	// Peek returns the queued lines without removing them, oldest first, and
	// a cursor identifying them for [LogSink.Commit] once they have printed.
	Peek() ([]string, uint64)
	// Commit removes the lines a Peek returned under cursor, confirming they
	// printed. Lines never committed are flushed to the fallback destination
	// by Deactivate instead.
	Commit(cursor uint64)
	// Deactivate stops queueing and flushes any uncommitted lines to the
	// fallback destination, so no line is lost when the panel stops.
	Deactivate()
}

// LogWriter is an [io.Writer] that sits between a log handler and its
// destination. While active it queues each log line for the terminal UI's
// model to print above the pinned panel; otherwise it funnels the write
// through to a fallback writer, the behavior when no TUI is running.
//
// It satisfies [LogSink]. Queueing rather than writing is what keeps the
// panel intact: the panel's model consumes the queue from inside the Bubble
// Tea event loop, so every insertion above the panel is serialized with the
// panel's own repaints by the one renderer that owns the screen, no matter
// how many goroutines log concurrently. Delivery is at-least-once: a line
// stays queued from [LogWriter.Peek] until the panel confirms it printed with
// [LogWriter.Commit], and [LogWriter.Deactivate] flushes everything still
// uncommitted to the fallback. A program killed between a batch's print and
// its commit may thus repeat those lines on the fallback, but no path loses
// them.
//
// The mutex guards the queue and the active flag. It is released before
// Write's fallback write, so concurrent writes serialize only as far as their
// destination does (the production fallback is stderr behind a
// write-serializing slog handler, which must itself be safe for concurrent
// use); Deactivate's residue flush alone holds the mutex across its write, so
// a write racing the deactivation cannot land ahead of the older residue.
// Create instances with [NewLogWriter].
type LogWriter struct {
	fallback io.Writer
	queue    []string
	// The sequence number of the first queued line. It advances as lines are
	// committed or dropped, so a commit cursor identifies exactly the lines
	// its peek saw even when overflow trimmed the queue in between.
	head uint64
	// Lines lost to overflow but not yet reported by a printed marker, and
	// the portion of that count carried by the outstanding peek's marker.
	dropped     int
	peekDropped int
	active      bool
	mu          sync.Mutex
}

// NewLogWriter creates a new [LogWriter] that writes to fallback until
// [LogWriter.Activate] switches it to queueing.
func NewLogWriter(fallback io.Writer) *LogWriter {
	return &LogWriter{fallback: fallback}
}

// Activate switches the writer to queueing lines for the panel to print.
func (w *LogWriter) Activate() {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.active = true
}

// Peek returns the queued lines without removing them, oldest first, and the
// cursor to pass to [LogWriter.Commit] once they have printed. When lines
// were dropped to overflow since the last commit, the batch opens with a
// marker counting them, so the loss is visible where the lines would have
// been.
func (w *LogWriter) Peek() ([]string, uint64) {
	w.mu.Lock()
	defer w.mu.Unlock()

	cursor := w.head + uint64(len(w.queue))

	if len(w.queue) == 0 && w.dropped == 0 {
		return nil, cursor
	}

	lines := make([]string, 0, len(w.queue)+1)

	if w.dropped > 0 {
		lines = append(lines, fmt.Sprintf("… %d log lines dropped", w.dropped))
	}

	w.peekDropped = w.dropped

	return append(lines, w.queue...), cursor
}

// Commit removes the lines the peek identified by cursor returned, confirming
// they printed, along with the overflow count that peek's marker reported.
// Lines dropped by overflow since the peek have already left the queue, so a
// commit never removes lines no peek has seen.
func (w *LogWriter) Commit(cursor uint64) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if cursor > w.head {
		n := min(cursor-w.head, uint64(len(w.queue)))
		w.queue = w.queue[n:]
		w.head += n
	}

	w.dropped = max(w.dropped-w.peekDropped, 0)
	w.peekDropped = 0
}

// Deactivate switches the writer back to its fallback and flushes any
// uncommitted lines to it, one per line, so lines the panel never confirmed
// printing still reach the terminal. The mutex spans the flush so a write
// racing the deactivation cannot land on the fallback ahead of the older
// residue. Flush errors are dropped: the fallback is the writer's last
// resort, so there is nowhere further to report them.
func (w *LogWriter) Deactivate() {
	w.mu.Lock()
	defer w.mu.Unlock()

	lines := w.queue
	if w.dropped > 0 {
		lines = append([]string{fmt.Sprintf("… %d log lines dropped", w.dropped)}, lines...)
	}

	w.head += uint64(len(w.queue))
	w.queue = nil
	w.dropped = 0
	w.peekDropped = 0
	w.active = false

	if len(lines) > 0 {
		//nolint:errcheck // The fallback is the last resort; nowhere to report.
		_, _ = io.WriteString(w.fallback, strings.Join(lines, "\n")+"\n")
	}
}

// Write routes p to the fallback writer, or, while the sink is active, splits
// it into lines and queues each for the panel to print.
//
// It reports len(p) consumed on the queue path so log handlers never see a
// short write. The queue is bounded at [maxQueuedLogLines]; past it the
// oldest lines give way and the next peeked batch reports the loss.
func (w *LogWriter) Write(p []byte) (int, error) {
	w.mu.Lock()

	if w.active {
		defer w.mu.Unlock()

		w.queue = append(w.queue, splitLogLines(p)...)
		if excess := len(w.queue) - maxQueuedLogLines; excess > 0 {
			w.queue = w.queue[excess:]
			w.head += uint64(excess)
			w.dropped += excess
		}

		return len(p), nil
	}

	w.mu.Unlock()

	n, err := w.fallback.Write(p)
	if err != nil {
		return n, fmt.Errorf("write log: %w", err)
	}

	return n, nil
}

// splitLogLines splits a log write into individual lines, trimming the trailing
// newlines the handler appends (each line is printed on its own line by the
// program) so they do not print a blank line after the record, and dropping a
// write that is empty once trimmed. A blank line inside the record is kept, so
// the record's own line breaks survive intact.
func splitLogLines(p []byte) []string {
	text := strings.TrimRight(string(p), "\n")
	if text == "" {
		return nil
	}

	return strings.Split(text, "\n")
}
