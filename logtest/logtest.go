package logtest

import (
	"context"
	"log/slog"
	"slices"
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
