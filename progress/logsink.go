package progress

import (
	"fmt"
	"io"
	"strings"
	"sync"
)

// maxQueuedLogLines bounds the sink's queue so a panel that has stopped
// draining (a wedged terminal awaiting the reporter's shutdown escalation)
// cannot grow it without limit. When the queue is full the oldest lines give
// way — scrollback semantics, the newest lines matter most — and the next
// drain opens with a marker counting what was lost.
const maxQueuedLogLines = 1000

// LogSink queues log lines for the terminal UI while it owns the screen, so
// log output and the live panel funnel through the one renderer instead of
// corrupting each other with interleaved writes.
//
// The reporter activates the sink while the TUI runs and deactivates it when
// the TUI stops; the panel's model drains the queue from inside the program's
// event loop. See [LogWriter] for an implementation.
type LogSink interface {
	// Activate begins queueing written lines for the panel to drain.
	Activate()
	// Drain returns and clears the queued lines, oldest first.
	Drain() []string
	// Deactivate stops queueing and flushes any undrained lines to the
	// fallback destination, so no line is lost when the panel stops.
	Deactivate()
}

// LogWriter is an [io.Writer] that sits between a log handler and its
// destination. While active it queues each log line for the terminal UI's
// model to drain and print above the pinned panel; otherwise it funnels the
// write through to a fallback writer, the behavior when no TUI is running.
//
// It satisfies [LogSink]. Queueing rather than writing is what keeps the
// panel intact: the panel's model drains the queue from inside the Bubble Tea
// event loop, so every insertion above the panel is serialized with the
// panel's own repaints by the one renderer that owns the screen, no matter
// how many goroutines log concurrently. [LogWriter.Deactivate] flushes
// undrained lines to the fallback, so lines logged as the TUI stops land on
// the terminal below the erased panel instead of vanishing.
//
// The mutex guards the queue and the active flag; the lock is released before
// any fallback write, so concurrent writes serialize only as far as their
// destination does (the production fallback is stderr behind a
// write-serializing slog handler, which must itself be safe for concurrent
// use). Create instances with [NewLogWriter].
type LogWriter struct {
	fallback io.Writer
	queue    []string
	dropped  int
	active   bool
	mu       sync.Mutex
}

// NewLogWriter creates a new [LogWriter] that writes to fallback until
// [LogWriter.Activate] switches it to queueing.
func NewLogWriter(fallback io.Writer) *LogWriter {
	return &LogWriter{fallback: fallback}
}

// Activate switches the writer to queueing lines for the panel to drain.
func (w *LogWriter) Activate() {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.active = true
}

// Drain returns and clears the queued lines, oldest first. When the queue
// overflowed since the last drain, the returned batch opens with a marker
// counting the lines that gave way, so the loss is visible where the lines
// would have been.
func (w *LogWriter) Drain() []string {
	w.mu.Lock()
	defer w.mu.Unlock()

	return w.drainLocked()
}

// drainLocked clears and returns the queue with its overflow marker. The
// caller holds the mutex.
func (w *LogWriter) drainLocked() []string {
	lines := w.queue
	w.queue = nil

	if w.dropped > 0 {
		lines = append([]string{fmt.Sprintf("… %d log lines dropped", w.dropped)}, lines...)
		w.dropped = 0
	}

	return lines
}

// Deactivate switches the writer back to its fallback and flushes any
// undrained lines to it, one per line, so lines logged while the panel was
// stopping still reach the terminal. Flush errors are dropped: the fallback
// is the writer's last resort, so there is nowhere further to report them.
func (w *LogWriter) Deactivate() {
	w.mu.Lock()

	lines := w.drainLocked()
	w.active = false

	w.mu.Unlock()

	// Written outside the lock, matching Write's contract that the lock never
	// spans a fallback write.
	if len(lines) > 0 {
		//nolint:errcheck // The fallback is the last resort; nowhere to report.
		_, _ = io.WriteString(w.fallback, strings.Join(lines, "\n")+"\n")
	}
}

// Write routes p to the fallback writer, or, while the sink is active, splits
// it into lines and queues each for the panel to drain.
//
// It reports len(p) consumed on the queue path so log handlers never see a
// short write. The queue is bounded at [maxQueuedLogLines]; past it the
// oldest lines give way and the next drain reports the loss.
func (w *LogWriter) Write(p []byte) (int, error) {
	w.mu.Lock()

	if w.active {
		defer w.mu.Unlock()

		w.queue = append(w.queue, splitLogLines(p)...)
		if excess := len(w.queue) - maxQueuedLogLines; excess > 0 {
			w.queue = w.queue[excess:]
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
