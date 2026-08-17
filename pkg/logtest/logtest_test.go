package logtest_test

import (
	"log/slog"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.jacobcolvin.com/hcp_archiver/pkg/logtest"
)

// recordingTB captures failures instead of failing the real test, so the
// failer under test can be observed.
type recordingTB struct {
	testing.TB
	failed bool
}

func (r *recordingTB) Errorf(string, ...any) {
	r.failed = true
}

func TestFailOnFailsOnlyForNamedEvents(t *testing.T) {
	t.Parallel()

	rec := &recordingTB{TB: t}
	logger := logtest.FailOn(rec, "valve_fired")

	logger.Info("ordinary_event")
	logger.Warn("another_event")
	assert.False(t, rec.failed, "unnamed events pass through")

	logger.Debug("valve_fired")
	assert.True(t, rec.failed, "a named event fails the test at any level")
}

func TestRecorderCapturesEventsInOrder(t *testing.T) {
	t.Parallel()

	rec := logtest.NewRecorder()
	logger := rec.Logger()

	logger.Warn("valve_fired",
		slog.String("path", "a.ndjson"),
		slog.Int("records", 3),
	)
	logger.Debug("valve_fired",
		slog.String("path", "b.ndjson"),
		slog.Int("records", 1),
	)

	events := rec.Events("valve_fired")
	require.Len(t, events, 2, "every level is captured, oldest first")

	assert.Equal(t, "valve_fired", events[0].Message)
	assert.Equal(t, slog.LevelWarn, events[0].Level)
	assert.Equal(t, "a.ndjson", events[0].Attrs["path"])
	assert.Equal(t, int64(3), events[0].Attrs["records"],
		"slog.Int reads back as the int64 slog stores it as")

	assert.Equal(t, slog.LevelDebug, events[1].Level)
	assert.Equal(t, "b.ndjson", events[1].Attrs["path"])
}

func TestRecorderEventsFiltersByMessage(t *testing.T) {
	t.Parallel()

	rec := logtest.NewRecorder()
	logger := rec.Logger()

	logger.Info("first_event")
	logger.Info("second_event")
	logger.Info("first_event")

	assert.Len(t, rec.Events("first_event"), 2)
	assert.Len(t, rec.Events("second_event"), 1)
	assert.Empty(t, rec.Events("never_fired"))
}

func TestRecorderCapturesBoundAttrs(t *testing.T) {
	t.Parallel()

	// Attributes bound through With must land alongside call-site ones, or an
	// assertion over a field the system under test attaches once, up front,
	// would pass vacuously against an empty map.
	rec := logtest.NewRecorder()
	logger := rec.Logger().With(slog.String("org", "acme"))

	logger.With(slog.String("phase", "collect")).Warn("valve_fired",
		slog.Int("records", 2),
	)

	events := rec.Events("valve_fired")
	require.Len(t, events, 1)
	assert.Equal(t, "acme", events[0].Attrs["org"])
	assert.Equal(t, "collect", events[0].Attrs["phase"])
	assert.Equal(t, int64(2), events[0].Attrs["records"])
}

func TestRecorderIsSafeForConcurrentUse(t *testing.T) {
	t.Parallel()

	// The system under test may log from several workers at once, so the
	// capture itself must not be the race the suite reports.
	const workers = 8

	rec := logtest.NewRecorder()
	logger := rec.Logger()

	var wg sync.WaitGroup

	wg.Add(workers)

	for i := range workers {
		go func() {
			defer wg.Done()

			logger.Warn("valve_fired", slog.Int("worker", i))
		}()
	}

	wg.Wait()

	assert.Len(t, rec.Events("valve_fired"), workers)
}
