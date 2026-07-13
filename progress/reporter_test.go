package progress_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.jacobcolvin.com/hcp_archiver/config"
	"go.jacobcolvin.com/hcp_archiver/manifest"
	"go.jacobcolvin.com/hcp_archiver/progress"
)

// fakeSource is a static [progress.TallySource] for tests.
type fakeSource struct {
	tally manifest.Tally
}

func (f fakeSource) Tally() manifest.Tally {
	return f.tally
}

// fixedClock returns a clock that yields each of times in turn on successive
// calls, holding the last value once they are exhausted.
func fixedClock(times ...time.Time) func() time.Time {
	i := 0

	return func() time.Time {
		t := times[i]
		if i < len(times)-1 {
			i++
		}

		return t
	}
}

func TestReporter_HumanLine(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)
	src := fakeSource{tally: manifest.Tally{
		Target:          "org/acme",
		Done:            10,
		Errored:         2,
		Retried:         3,
		BytesDownloaded: 2 * 1024 * 1024,
	}}

	buf := &bytes.Buffer{}
	r := progress.New(
		buf,
		config.ProgressModeHuman,
		src,
		progress.WithClock(fixedClock(base, base.Add(10*time.Second))),
	)
	r.SetPhase("workspaces")
	// Unit progress is decoupled from the object tally: seven of twenty weighted
	// units are done regardless of the twelve objects the tally records.
	r.SetTotal(20)
	r.Advance(7)

	require.NoError(t, r.Report())

	line := buf.String()
	assert.True(t, strings.HasSuffix(line, "\n"), "line ends with newline")
	assert.Contains(t, line, "phase=workspaces")
	assert.Contains(t, line, "target=org/acme")
	assert.Contains(t, line, "done=10")
	assert.Contains(t, line, "errored=2")
	assert.Contains(t, line, "retried=3")
	assert.Contains(t, line, "completed=7/20")
	assert.Contains(t, line, "bytes=2.0 MiB")
	assert.Contains(t, line, "elapsed=10s")
	assert.Contains(t, line, "rate=")
}

func TestReporter_UnitProgressResets(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)
	src := fakeSource{tally: manifest.Tally{Done: 1}}

	buf := &bytes.Buffer{}
	r := progress.New(
		buf,
		config.ProgressModeHuman,
		src,
		progress.WithClock(fixedClock(base, base.Add(time.Second))),
	)
	r.SetPhase("workspaces")

	// A prior phase leaves units done; SetTotal for the next phase resets the
	// completed count so its bar starts fresh rather than inheriting the total.
	r.SetTotal(4)
	r.Advance(3)
	r.SetTotal(8)
	r.Advance(2)

	require.NoError(t, r.Report())
	assert.Contains(t, buf.String(), "completed=2/8")
}

func TestReporter_Resumed(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)
	src := fakeSource{tally: manifest.Tally{Done: 1200, Resumed: true}}

	t.Run("human", func(t *testing.T) {
		t.Parallel()

		buf := &bytes.Buffer{}
		r := progress.New(buf, config.ProgressModeHuman, src,
			progress.WithClock(fixedClock(base, base.Add(time.Second))))

		require.NoError(t, r.Report())
		assert.Contains(t, buf.String(), "resumed=true")
	})

	t.Run("json", func(t *testing.T) {
		t.Parallel()

		buf := &bytes.Buffer{}
		r := progress.New(buf, config.ProgressModeJSON, src,
			progress.WithClock(fixedClock(base, base.Add(time.Second))))

		require.NoError(t, r.Report())

		var line struct {
			Resumed bool `json:"resumed"`
			Done    int  `json:"done"`
		}

		require.NoError(t, json.Unmarshal(buf.Bytes(), &line))
		assert.True(t, line.Resumed)
		assert.Equal(t, 1200, line.Done, "counts are cumulative, carried into the resumed run")
	})
}

func TestReporter_HumanLine_UnknownTotal(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)
	src := fakeSource{tally: manifest.Tally{Done: 3}}

	buf := &bytes.Buffer{}
	r := progress.New(
		buf,
		config.ProgressModeHuman,
		src,
		progress.WithClock(fixedClock(base, base.Add(time.Second))),
	)

	require.NoError(t, r.Report())
	assert.NotContains(t, buf.String(), "completed=")
}

func TestReporter_Tasks(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)

	newReporter := func(buf *bytes.Buffer) *progress.Reporter {
		return progress.New(buf, config.ProgressModeHuman, fakeSource{},
			progress.WithClock(fixedClock(base, base.Add(time.Second))))
	}

	t.Run("advance flows into phase completed", func(t *testing.T) {
		t.Parallel()

		buf := &bytes.Buffer{}
		r := newReporter(buf)
		r.SetPhase("workspaces")
		r.SetTotal(100)

		task := r.StartTask("big", 60)
		task.Advance(10)

		require.NoError(t, r.Report())
		assert.Contains(t, buf.String(), "completed=10/100")
		assert.Contains(t, buf.String(), `task="big" taskCompleted=10/60`)
	})

	t.Run("advances past the total are absorbed", func(t *testing.T) {
		t.Parallel()

		buf := &bytes.Buffer{}
		r := newReporter(buf)
		r.SetTotal(100)

		// The task's total is an estimate; the real listing can outgrow it.
		task := r.StartTask("underestimated", 30)
		for range 58 {
			task.Advance(1)
		}

		require.NoError(t, r.Report())
		assert.Contains(t, buf.String(), "completed=30/100",
			"the phase count never exceeds the task's budgeted weight")
		assert.Contains(t, buf.String(), "taskCompleted=30/30",
			"the task reads complete, not overfull")

		task.Done()
		task.Advance(1)

		buf.Reset()
		require.NoError(t, r.Report())
		assert.Contains(t, buf.String(), "completed=30/100", "done adds nothing after an overrun")
	})

	t.Run("done commits the remainder once", func(t *testing.T) {
		t.Parallel()

		buf := &bytes.Buffer{}
		r := newReporter(buf)
		r.SetTotal(100)

		task := r.StartTask("big", 60)
		task.Advance(10)
		task.Done()
		task.Done()
		task.Advance(5)

		require.NoError(t, r.Report())
		assert.Contains(t, buf.String(), "completed=60/100",
			"remainder committed exactly once; post-done advances dropped")
		assert.NotContains(t, buf.String(), "task=", "a settled task leaves the panel")
	})

	t.Run("displays the task with the most remaining", func(t *testing.T) {
		t.Parallel()

		buf := &bytes.Buffer{}
		r := newReporter(buf)
		r.SetTotal(100)

		small := r.StartTask("small", 5)
		r.StartTask("big", 80)

		require.NoError(t, r.Report())
		assert.Contains(t, buf.String(), `task="big"`)

		_ = small
	})

	t.Run("a new phase orphans stale tasks", func(t *testing.T) {
		t.Parallel()

		buf := &bytes.Buffer{}
		r := newReporter(buf)
		r.SetTotal(10)

		task := r.StartTask("stale", 8)
		r.SetTotal(20)
		task.Advance(3)

		require.NoError(t, r.Report())
		assert.Contains(t, buf.String(), "completed=0/20",
			"an orphaned task's advances stay out of the new phase")
		assert.NotContains(t, buf.String(), "task=")
	})
}

func TestReporter_JSONLine(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)
	src := fakeSource{tally: manifest.Tally{
		Target:          "org/acme",
		Done:            10,
		Absent:          1,
		Skipped:         2,
		Errored:         3,
		Forbidden:       5,
		NotApplicable:   4,
		BytesDownloaded: 1024,
	}}

	buf := &bytes.Buffer{}
	r := progress.New(
		buf,
		config.ProgressModeJSON,
		src,
		progress.WithClock(fixedClock(base, base.Add(4*time.Second))),
	)
	r.SetPhase("runs")
	r.SetTotal(30)
	r.Advance(10)

	require.NoError(t, r.Report())

	out := buf.String()
	assert.True(t, strings.HasSuffix(out, "\n"), "one line ends with newline")
	assert.Equal(t, 1, strings.Count(strings.TrimRight(out, "\n"), "\n")+1)

	var line struct {
		Phase           string  `json:"phase"`
		Target          string  `json:"target"`
		PhaseTotal      *int    `json:"phaseTotal"`
		PhaseCompleted  *int    `json:"phaseCompleted"`
		Remaining       *int    `json:"remaining"`
		Done            int     `json:"done"`
		Errored         int     `json:"errored"`
		Forbidden       int     `json:"forbidden"`
		Total           int     `json:"total"`
		BytesDownloaded int64   `json:"bytesDownloaded"`
		ElapsedSeconds  float64 `json:"elapsedSeconds"`
		BytesPerSecond  float64 `json:"bytesPerSecond"`
		Summary         bool    `json:"summary"`
	}

	require.NoError(t, json.Unmarshal([]byte(out), &line))
	assert.Equal(t, "runs", line.Phase)
	assert.Equal(t, "org/acme", line.Target)
	assert.Equal(t, 10, line.Done)
	assert.Equal(t, 3, line.Errored)
	assert.Equal(t, 5, line.Forbidden)
	assert.Equal(t, 25, line.Total)
	assert.Equal(t, int64(1024), line.BytesDownloaded)
	assert.InEpsilon(t, 4.0, line.ElapsedSeconds, 1e-9)
	assert.InEpsilon(t, 256.0, line.BytesPerSecond, 1e-9)
	assert.False(t, line.Summary)
	// The JSON line carries no object-based remaining field; explicit phase
	// progress appears only while the phase is determinate.
	assert.Nil(t, line.Remaining)
	require.NotNil(t, line.PhaseTotal)
	require.NotNil(t, line.PhaseCompleted)
	assert.Equal(t, 30, *line.PhaseTotal)
	assert.Equal(t, 10, *line.PhaseCompleted)
}

func TestReporter_Summary(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)
	src := fakeSource{tally: manifest.Tally{
		Done:            100,
		Absent:          3,
		Skipped:         2,
		Errored:         4,
		Forbidden:       6,
		NotApplicable:   1,
		Retried:         5,
		BytesDownloaded: 5 * 1024 * 1024,
	}}

	t.Run("human", func(t *testing.T) {
		t.Parallel()

		buf := &bytes.Buffer{}
		r := progress.New(
			buf,
			config.ProgressModeHuman,
			src,
			progress.WithClock(fixedClock(base, base.Add(200*time.Second))),
		)

		require.NoError(t, r.Summary())

		out := buf.String()
		assert.Contains(t, out, "summary")
		assert.Contains(t, out, "done=100")
		assert.Contains(t, out, "absent=3")
		assert.Contains(t, out, "skipped=2")
		assert.Contains(t, out, "errored=4")
		assert.Contains(t, out, "forbidden=6")
		assert.Contains(t, out, "retried=5")
		assert.Contains(t, out, "n/a=1")
		assert.Contains(t, out, "total=116")
		assert.Contains(t, out, "elapsed=3m20s")
	})

	t.Run("json", func(t *testing.T) {
		t.Parallel()

		buf := &bytes.Buffer{}
		r := progress.New(
			buf,
			config.ProgressModeJSON,
			src,
			progress.WithClock(fixedClock(base, base.Add(200*time.Second))),
		)

		require.NoError(t, r.Summary())

		var line struct {
			Summary bool  `json:"summary"`
			Total   int   `json:"total"`
			Retried int64 `json:"retried"`
		}

		require.NoError(t, json.Unmarshal(buf.Bytes(), &line))
		assert.True(t, line.Summary)
		assert.Equal(t, 116, line.Total)
		assert.Equal(t, int64(5), line.Retried)
	})
}

func TestReporter_AutoResolution(t *testing.T) {
	t.Parallel()

	src := fakeSource{}

	tests := map[string]struct {
		want config.ProgressMode
		opts []progress.Option
	}{
		"buffer resolves quiet": {want: config.ProgressModeQuiet},
		"forced tty resolves human": {
			opts: []progress.Option{progress.WithTTY(true)},
			want: config.ProgressModeHuman,
		},
		"forced no tty resolves quiet": {
			opts: []progress.Option{progress.WithTTY(false)},
			want: config.ProgressModeQuiet,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			buf := &bytes.Buffer{}
			r := progress.New(buf, config.ProgressModeAuto, src, tc.opts...)
			assert.Equal(t, tc.want, r.Mode())
		})
	}
}

func TestReporter_QuietWritesNothing(t *testing.T) {
	t.Parallel()

	src := fakeSource{tally: manifest.Tally{Done: 5}}
	buf := &bytes.Buffer{}
	r := progress.New(buf, config.ProgressModeQuiet, src)

	require.NoError(t, r.Report())
	require.NoError(t, r.Summary())
	assert.Empty(t, buf.String())
}

func TestReporter_AutoQuietBufferWritesNothing(t *testing.T) {
	t.Parallel()

	src := fakeSource{tally: manifest.Tally{Done: 5}}
	buf := &bytes.Buffer{}
	r := progress.New(buf, config.ProgressModeAuto, src)

	require.Equal(t, config.ProgressModeQuiet, r.Mode())
	require.NoError(t, r.Report())
	assert.Empty(t, buf.String())
}

func TestReporter_Run(t *testing.T) {
	t.Parallel()

	src := fakeSource{tally: manifest.Tally{Done: 1}}
	buf := &bytes.Buffer{}
	r := progress.New(buf, config.ProgressModeHuman, src)

	ctx, cancel := context.WithCancel(t.Context())

	done := make(chan error, 1)
	go func() {
		done <- r.Run(ctx, time.Millisecond)
	}()

	cancel()

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("run did not stop after cancel")
	}
}

func TestReporter_Run_PanelQuitsOnCancel(t *testing.T) {
	t.Parallel()

	// The panel path must shut down gracefully on a ctx cancel (erasing its
	// final frame) and must never hang the run's close-out.
	pr, pw := io.Pipe()
	t.Cleanup(func() {
		assert.NoError(t, pw.Close())
	})

	buf := &bytes.Buffer{}
	r := progress.New(buf, config.ProgressModeHuman, fakeSource{},
		progress.WithTTY(true),
		progress.WithInput(pr),
	)

	ctx, cancel := context.WithCancel(t.Context())

	done := make(chan error, 1)
	go func() {
		done <- r.Run(ctx, 0)
	}()

	// Give the program a moment to start and paint before canceling, so the
	// quit path exercises a live renderer.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("panel did not stop after cancel")
	}
}

func TestReporter_TTYDetection(t *testing.T) {
	t.Parallel()

	// A regular file is not a character device, so auto resolves to quiet.
	f, err := os.CreateTemp(t.TempDir(), "progress")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, f.Close()) })

	r := progress.New(f, config.ProgressModeAuto, fakeSource{})
	assert.Equal(t, config.ProgressModeQuiet, r.Mode())
}

func TestHumanBytes(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		want string
		n    int64
	}{
		"bytes":      {n: 512, want: "512 B"},
		"kib":        {n: 2048, want: "2.0 KiB"},
		"mib":        {n: 3 * 1024 * 1024, want: "3.0 MiB"},
		"gib":        {n: 5 * 1024 * 1024 * 1024, want: "5.0 GiB"},
		"zero":       {n: 0, want: "0 B"},
		"fractional": {n: 1536, want: "1.5 KiB"},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			buf := &bytes.Buffer{}
			src := fakeSource{tally: manifest.Tally{BytesDownloaded: tc.n}}
			base := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)
			r := progress.New(
				buf,
				config.ProgressModeHuman,
				src,
				progress.WithClock(fixedClock(base, base)),
			)

			require.NoError(t, r.Report())
			assert.Contains(t, buf.String(), "bytes="+tc.want)
		})
	}
}

// staticRateStatus returns a rate-status source that always reports rps and
// pausedFor, standing in for the client's governor.
func staticRateStatus(rps float64, pausedFor time.Duration) func() (float64, time.Duration) {
	return func() (float64, time.Duration) {
		return rps, pausedFor
	}
}

func TestReporter_HumanLine_RateStatus(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)
	src := fakeSource{tally: manifest.Tally{Done: 1}}

	rateLimited := new(atomic.Int64)
	rateLimited.Store(12)

	buf := &bytes.Buffer{}
	r := progress.New(
		buf,
		config.ProgressModeHuman,
		src,
		progress.WithClock(fixedClock(base, base.Add(time.Second), base.Add(2*time.Second))),
		progress.WithRateStatus(staticRateStatus(12, 4*time.Second)),
		progress.WithRateLimited(rateLimited),
	)

	require.NoError(t, r.Report())

	line := buf.String()
	assert.Contains(t, line, "rps=12")
	assert.Contains(t, line, "paused=4s")
	assert.Contains(t, line, "rateLimited=12")

	// The summary keeps the run's rate-limited total but drops the live rate
	// readout, which is meaningless once the run has ended.
	buf.Reset()
	require.NoError(t, r.Summary())

	line = buf.String()
	assert.NotContains(t, line, "rps=")
	assert.NotContains(t, line, "paused=")
	assert.Contains(t, line, "rateLimited=12")
}

func TestReporter_HumanLine_NoPausedWhileFlowing(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)
	src := fakeSource{tally: manifest.Tally{Done: 1}}

	buf := &bytes.Buffer{}
	r := progress.New(
		buf,
		config.ProgressModeHuman,
		src,
		progress.WithClock(fixedClock(base, base.Add(time.Second))),
		progress.WithRateStatus(staticRateStatus(30, 0)),
	)

	require.NoError(t, r.Report())

	line := buf.String()
	assert.Contains(t, line, "rps=30")
	assert.NotContains(t, line, "paused=", "no cooldown means no paused readout")
}

func TestReporter_HumanLine_NoRateWithoutSource(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)
	src := fakeSource{tally: manifest.Tally{Done: 1}}

	buf := &bytes.Buffer{}
	r := progress.New(
		buf,
		config.ProgressModeHuman,
		src,
		progress.WithClock(fixedClock(base, base.Add(time.Second))),
	)

	require.NoError(t, r.Report())

	line := buf.String()
	assert.NotContains(t, line, "rps=")
	assert.NotContains(t, line, "rateLimited=")
}

func TestReporter_JSONLine_RateStatus(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)
	src := fakeSource{tally: manifest.Tally{Done: 1}}

	rateLimited := new(atomic.Int64)
	rateLimited.Store(7)

	buf := &bytes.Buffer{}
	r := progress.New(
		buf,
		config.ProgressModeJSON,
		src,
		progress.WithClock(fixedClock(base, base.Add(time.Second))),
		progress.WithRateStatus(staticRateStatus(7.5, 1500*time.Millisecond)),
		progress.WithRateLimited(rateLimited),
	)

	require.NoError(t, r.Report())

	var line struct {
		RequestsPerSecond *float64 `json:"requestsPerSecond"`
		PausedMs          *int64   `json:"pausedMs"`
		RateLimited       *int64   `json:"rateLimited"`
	}

	require.NoError(t, json.Unmarshal(buf.Bytes(), &line))
	require.NotNil(t, line.RequestsPerSecond)
	require.NotNil(t, line.PausedMs)
	require.NotNil(t, line.RateLimited)
	assert.InEpsilon(t, 7.5, *line.RequestsPerSecond, 1e-9)
	assert.Equal(t, int64(1500), *line.PausedMs)
	assert.Equal(t, int64(7), *line.RateLimited)

	// The summary drops the live rate figures, matching logfmt.
	buf.Reset()
	require.NoError(t, r.Summary())

	var summary map[string]any

	require.NoError(t, json.Unmarshal(buf.Bytes(), &summary))
	assert.NotContains(t, summary, "requestsPerSecond")
	assert.NotContains(t, summary, "pausedMs")
	assert.Contains(t, summary, "rateLimited")
}

func TestReporter_JSONLine_NoRateWithoutSource(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)
	src := fakeSource{tally: manifest.Tally{Done: 1}}

	buf := &bytes.Buffer{}
	r := progress.New(
		buf,
		config.ProgressModeJSON,
		src,
		progress.WithClock(fixedClock(base, base.Add(time.Second))),
	)

	require.NoError(t, r.Report())

	var line map[string]any

	require.NoError(t, json.Unmarshal(buf.Bytes(), &line))
	assert.NotContains(t, line, "requestsPerSecond")
	assert.NotContains(t, line, "pausedMs")
	assert.NotContains(t, line, "rateLimited")
}
