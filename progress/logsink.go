package progress

import (
	"fmt"
	"io"
	"strings"
	"sync"

	tea "charm.land/bubbletea/v2"
)

// LogSink receives the running Bubble Tea program so terminal output can be
// funneled through the one renderer that owns the screen, keeping log lines and
// the live panel from corrupting each other.
//
// The reporter registers its program on the sink while the TUI runs and clears
// it again when the TUI stops. See [LogWriter] for the implementation.
type LogSink interface {
	// SetProgram registers the active program, or clears it with nil so writes
	// revert to the fallback.
	SetProgram(program *tea.Program)
}

// LogWriter is an [io.Writer] that sits between a log handler and its
// destination. While a Bubble Tea program is registered it hands each log line
// to that program so the line scrolls above the pinned panel; otherwise it
// funnels the write through to a fallback writer, the behavior when no TUI is
// active.
//
// It satisfies [LogSink]. The mutex guards the program registration, so a
// concurrent SetProgram and Write never race on the program pointer and each
// Write sees a consistent one; the lock is released before the underlying write,
// so concurrent writes serialize only as far as their destination does. The
// program's Send is safe for concurrent use, and the fallback must be too (the
// production fallback is stderr behind a write-serializing slog handler). Create
// instances with [NewLogWriter].
type LogWriter struct {
	fallback io.Writer
	program  *tea.Program
	mu       sync.Mutex
}

// NewLogWriter creates a new [LogWriter] that writes to fallback until a program
// is registered with [LogWriter.SetProgram].
func NewLogWriter(fallback io.Writer) *LogWriter {
	return &LogWriter{fallback: fallback}
}

// SetProgram registers p as the active program, or clears it with nil. Clearing
// reverts subsequent writes to the fallback so the stderr path is restored
// before the next run logs.
func (w *LogWriter) SetProgram(p *tea.Program) {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.program = p
}

// Write routes p to the fallback writer, or, while a program is active, splits
// it into lines and prints each above the program's panel.
//
// It reports len(p) consumed on the program path so log handlers never see a
// short write. Lines are delivered through the context-guarded [tea.Program.Send]
// (never [tea.Program.Println], whose unbuffered channel would block a caller
// that logs after the program has stopped): a stopped program drops the line
// instead of deadlocking the run.
func (w *LogWriter) Write(p []byte) (int, error) {
	program := w.activeProgram()
	if program == nil {
		n, err := w.fallback.Write(p)
		if err != nil {
			return n, fmt.Errorf("write log: %w", err)
		}

		return n, nil
	}

	for _, line := range splitLogLines(p) {
		program.Send(tea.Println(line)())
	}

	return len(p), nil
}

// activeProgram returns the registered program, or nil when none is set.
func (w *LogWriter) activeProgram() *tea.Program {
	w.mu.Lock()
	defer w.mu.Unlock()

	return w.program
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
