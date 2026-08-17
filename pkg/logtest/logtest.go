package logtest

import (
	"context"
	"log/slog"
	"slices"
	"sync"
	"testing"
)

// FailOn returns a [*slog.Logger] that fails t when a record carrying one of
// the named messages is emitted, and passes every other record through
// silently. Hand it to the system under test wherever it accepts a logger,
// so a safety valve firing where none should becomes a test failure instead
// of a warning nobody reads.
func FailOn(tb testing.TB, messages ...string) *slog.Logger {
	tb.Helper()

	return slog.New(failHandler{tb: tb, messages: messages})
}

// failHandler is the [slog.Handler] behind [FailOn].
type failHandler struct {
	tb       testing.TB
	messages []string
}

// Enabled reports every level enabled, so a forbidden event cannot slip
// through a level filter.
func (h failHandler) Enabled(context.Context, slog.Level) bool { return true }

// Handle fails the test on a forbidden message and drops everything else.
func (h failHandler) Handle(_ context.Context, r slog.Record) error {
	if slices.Contains(h.messages, r.Message) {
		h.tb.Errorf("forbidden log event %q fired", r.Message)
	}

	return nil
}

// WithAttrs returns the handler unchanged; attributes do not affect matching.
func (h failHandler) WithAttrs([]slog.Attr) slog.Handler { return h }

// WithGroup returns the handler unchanged; groups do not affect matching.
func (h failHandler) WithGroup(string) slog.Handler { return h }

// Event is one captured log record.
type Event struct {
	// Attrs holds the record's attributes by key, resolved to their Go
	// values, so slog.Int reads back as an int64 and slog.String as a
	// string.
	Attrs map[string]any
	// Message is the record's message: the event's name.
	Message string
	// Level is the level the record was emitted at.
	Level slog.Level
}

// Recorder captures structured log events so a test can assert an event fired
// and carried the fields an operator would act on. It is [FailOn]'s mirror:
// FailOn pins a valve's silence, a Recorder pins its announcement.
//
// A Recorder is safe for concurrent use, so a system under test may log from
// several goroutines at once.
//
// Create instances with [NewRecorder].
type Recorder struct {
	events []Event
	mu     sync.Mutex
}

// NewRecorder creates a new [Recorder].
func NewRecorder() *Recorder {
	return &Recorder{}
}

// Logger returns a [*slog.Logger] capturing every record it is handed, at
// every level. Hand it to the system under test wherever it accepts a logger.
func (r *Recorder) Logger() *slog.Logger {
	return slog.New(recordHandler{rec: r})
}

// Events returns the captured events carrying message, oldest first, so a
// test asserts not only that an event fired but how often. It returns an
// empty slice when nothing matched.
func (r *Recorder) Events(message string) []Event {
	r.mu.Lock()
	defer r.mu.Unlock()

	out := []Event{}

	for _, e := range r.events {
		if e.Message == message {
			out = append(out, e)
		}
	}

	return out
}

// add appends one captured event.
func (r *Recorder) add(e Event) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.events = append(r.events, e)
}

// recordHandler is the [slog.Handler] behind [Recorder.Logger]. Clones made
// by WithAttrs share the recorder, so every logger derived from one captures
// into the same event list.
type recordHandler struct {
	rec   *Recorder
	attrs []slog.Attr
}

// Enabled reports every level enabled, so no event slips through a level
// filter unrecorded.
func (h recordHandler) Enabled(context.Context, slog.Level) bool { return true }

// Handle captures the record with the attributes bound to the handler ahead
// of those carried by the record itself.
func (h recordHandler) Handle(_ context.Context, r slog.Record) error {
	attrs := make(map[string]any, len(h.attrs)+r.NumAttrs())

	for _, a := range h.attrs {
		attrs[a.Key] = a.Value.Resolve().Any()
	}

	r.Attrs(func(a slog.Attr) bool {
		attrs[a.Key] = a.Value.Resolve().Any()

		return true
	})

	h.rec.add(Event{Message: r.Message, Attrs: attrs, Level: r.Level})

	return nil
}

// WithAttrs returns a handler binding attrs to every record it captures, so
// fields a caller attached through [slog.Logger.With] are asserted on the
// same way as those passed at the call site.
func (h recordHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return recordHandler{rec: h.rec, attrs: append(slices.Clip(h.attrs), attrs...)}
}

// WithGroup returns the handler unchanged: nothing in the archive logs
// through a grouped logger, so keys are captured unqualified.
func (h recordHandler) WithGroup(string) slog.Handler { return h }
