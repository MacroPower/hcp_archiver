package logtest_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"go.jacobcolvin.com/hcp_archiver/logtest"
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
