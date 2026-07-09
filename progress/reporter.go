package progress

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/MacroPower/tfc_archiver/config"
	"github.com/MacroPower/tfc_archiver/manifest"
)

// TallySource supplies the live counters the reporter renders.
//
// The ledger satisfies it through its Tally method, so the reporter reads the
// same counters that back the manifest and the two can never disagree.
type TallySource interface {
	// Tally returns a point-in-time snapshot of the live counters.
	Tally() manifest.Tally
}

// Reporter renders the live status of an archive run to a writer.
//
// It reads a [TallySource] and formats snapshots in the resolved
// [config.ProgressMode]; it never mutates ledger state. Create instances with
// [New]. A Reporter is safe for concurrent use: [Reporter.Report],
// [Reporter.Summary], [Reporter.SetPhase], and [Reporter.SetTotal] are guarded
// by a mutex, so a background [Reporter.Run] loop and the archiver may touch it
// at once.
type Reporter struct {
	w        io.Writer
	source   TallySource
	now      func() time.Time
	ttyForce *bool
	start    time.Time
	phase    string
	mode     config.ProgressMode
	interval time.Duration
	total    int
	mu       sync.Mutex
}

// Option configures a [Reporter] passed to [New].
//
// The available options are:
//   - [WithInterval]
//   - [WithClock]
//   - [WithTTY]
//   - [WithTotal]
type Option func(*Reporter)

// WithInterval sets the default cadence used by [Reporter.Run] when it is
// called with a non-positive interval. A non-positive value keeps the default.
// It returns an [Option].
func WithInterval(d time.Duration) Option {
	return func(r *Reporter) {
		if d > 0 {
			r.interval = d
		}
	}
}

// WithClock sets the time source used for elapsed time and rate, defaulting to
// [time.Now]. Inject a deterministic clock in tests. It returns an [Option].
func WithClock(now func() time.Time) Option {
	return func(r *Reporter) {
		if now != nil {
			r.now = now
		}
	}
}

// WithTTY forces terminal detection on or off when resolving
// [config.ProgressModeAuto], overriding inspection of the writer. It returns an
// [Option].
func WithTTY(isTTY bool) Option {
	return func(r *Reporter) {
		r.ttyForce = &isTTY
	}
}

// WithTotal sets the number of objects the run expects to process, letting the
// reporter show a remaining count. A negative value means unknown, which is the
// default. It returns an [Option].
func WithTotal(total int) Option {
	return func(r *Reporter) {
		r.total = total
	}
}

// New creates a new [Reporter].
//
// It writes to w in the resolved form of mode and reads counters from source.
// The mode is resolved once: [config.ProgressModeAuto] becomes
// [config.ProgressModeHuman] when w is an interactive terminal and
// [config.ProgressModeQuiet] otherwise. Terminal detection inspects w for an
// [*os.File] whose mode carries [os.ModeCharDevice], unless [WithTTY] overrides
// it. The run's elapsed clock starts now.
func New(w io.Writer, mode config.ProgressMode, source TallySource, opts ...Option) *Reporter {
	r := &Reporter{
		w:        w,
		source:   source,
		now:      time.Now,
		interval: config.DefaultProgressInterval,
		total:    -1,
		mode:     mode,
	}

	for _, opt := range opts {
		opt(r)
	}

	r.start = r.now()
	r.mode = r.resolve(mode)

	return r
}

// resolve turns [config.ProgressModeAuto] into a concrete mode and normalizes
// any unrecognized value to [config.ProgressModeQuiet].
func (r *Reporter) resolve(mode config.ProgressMode) config.ProgressMode {
	switch mode {
	case config.ProgressModeHuman, config.ProgressModeJSON, config.ProgressModeQuiet:
		return mode
	case config.ProgressModeAuto:
		if r.isTTY() {
			return config.ProgressModeHuman
		}

		return config.ProgressModeQuiet

	default:
		return config.ProgressModeQuiet
	}
}

// isTTY reports whether the writer is an interactive terminal, honoring a
// [WithTTY] override.
func (r *Reporter) isTTY() bool {
	if r.ttyForce != nil {
		return *r.ttyForce
	}

	f, ok := r.w.(*os.File)
	if !ok {
		return false
	}

	info, err := f.Stat()
	if err != nil {
		return false
	}

	return info.Mode()&os.ModeCharDevice != 0
}

// Mode returns the resolved [config.ProgressMode] the reporter renders in,
// after [config.ProgressModeAuto] has been reduced to human or quiet.
func (r *Reporter) Mode() config.ProgressMode {
	return r.mode
}

// SetPhase records the current phase of the run (for example the collection
// being walked). It appears in both output forms and is safe to call
// concurrently.
func (r *Reporter) SetPhase(phase string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.phase = phase
}

// SetTotal updates the number of objects the run expects to process. A negative
// value means unknown, which suppresses the remaining count. It is safe to call
// concurrently.
func (r *Reporter) SetTotal(total int) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.total = total
}

// snapshot is one rendered line's worth of derived state, computed under lock.
type snapshot struct {
	phase     string
	tally     manifest.Tally
	elapsed   time.Duration
	rate      float64
	remaining int
	hasRem    bool
}

// take builds a [snapshot] from the current tally and clock. The caller holds
// the mutex.
func (r *Reporter) take() snapshot {
	t := r.source.Tally()
	elapsed := r.now().Sub(r.start)

	rate := 0.0
	if elapsed > 0 {
		rate = float64(t.BytesDownloaded) / elapsed.Seconds()
	}

	snap := snapshot{
		tally:   t,
		phase:   r.phase,
		elapsed: elapsed,
		rate:    rate,
	}

	if r.total >= 0 {
		snap.hasRem = true
		snap.remaining = max(r.total-t.Total(), 0)
	}

	return snap
}

// Report renders one snapshot in the resolved mode. It writes nothing in quiet
// mode and returns any write error.
func (r *Reporter) Report() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.render(r.take(), false)
}

// Summary renders the final totals per status class, the wall time, and the
// errored count. It writes nothing in quiet mode and returns any write error.
func (r *Reporter) Summary() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.render(r.take(), true)
}

// Run reports on a fixed cadence until ctx is done, then returns nil. A
// non-positive interval falls back to the reporter's configured interval. It is
// a convenience over [Reporter.Report]; the archiver may drive Report directly
// instead.
func (r *Reporter) Run(ctx context.Context, interval time.Duration) error {
	if interval <= 0 {
		interval = r.interval
	}

	if interval <= 0 {
		interval = config.DefaultProgressInterval
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			err := r.Report()
			if err != nil {
				return err
			}
		}
	}
}

// render writes one snapshot in the resolved mode. The caller holds the mutex.
func (r *Reporter) render(snap snapshot, summary bool) error {
	switch r.mode {
	case config.ProgressModeHuman:
		return r.writeString(r.humanLine(snap, summary))
	case config.ProgressModeJSON:
		return r.writeJSON(snap, summary)
	case config.ProgressModeQuiet, config.ProgressModeAuto:
		return nil
	default:
		return nil
	}
}

// writeString writes s to the underlying writer, wrapping any error.
func (r *Reporter) writeString(s string) error {
	_, err := io.WriteString(r.w, s)
	if err != nil {
		return fmt.Errorf("write progress: %w", err)
	}

	return nil
}

// humanLine formats a single human-readable line ending in a newline.
func (r *Reporter) humanLine(snap snapshot, summary bool) string {
	var b strings.Builder

	t := snap.tally

	if summary {
		b.WriteString("summary")
	} else {
		b.WriteString("progress")
	}

	if snap.phase != "" {
		fmt.Fprintf(&b, " phase=%s", snap.phase)
	}

	if t.Target != "" {
		fmt.Fprintf(&b, " target=%s", t.Target)
	}

	fmt.Fprintf(&b, " done=%d errored=%d", t.Done, t.Errored)

	if summary {
		fmt.Fprintf(
			&b,
			" absent=%d skipped=%d n/a=%d total=%d",
			t.AbsentPermanently,
			t.Skipped,
			t.NotApplicable,
			t.Total(),
		)
	} else if snap.hasRem {
		fmt.Fprintf(&b, " remaining=%d", snap.remaining)
	}

	fmt.Fprintf(
		&b,
		" bytes=%s elapsed=%s rate=%s/s\n",
		humanBytes(t.BytesDownloaded),
		snap.elapsed.Round(time.Second),
		humanBytes(int64(snap.rate)),
	)

	return b.String()
}

// jsonLine is the machine-readable shape emitted one per line.
type jsonLine struct {
	Remaining         *int    `json:"remaining,omitempty"`
	Phase             string  `json:"phase,omitempty"`
	Target            string  `json:"target,omitempty"`
	Done              int     `json:"done"`
	AbsentPermanently int     `json:"absentPermanently"`
	Skipped           int     `json:"skipped"`
	Errored           int     `json:"errored"`
	NotApplicable     int     `json:"notApplicable"`
	Total             int     `json:"total"`
	BytesDownloaded   int64   `json:"bytesDownloaded"`
	ElapsedSeconds    float64 `json:"elapsedSeconds"`
	BytesPerSecond    float64 `json:"bytesPerSecond"`
	Summary           bool    `json:"summary,omitempty"`
}

// writeJSON encodes one snapshot as a compact JSON object followed by a
// newline.
func (r *Reporter) writeJSON(snap snapshot, summary bool) error {
	t := snap.tally

	line := jsonLine{
		Phase:             snap.phase,
		Target:            t.Target,
		Done:              t.Done,
		AbsentPermanently: t.AbsentPermanently,
		Skipped:           t.Skipped,
		Errored:           t.Errored,
		NotApplicable:     t.NotApplicable,
		Total:             t.Total(),
		BytesDownloaded:   t.BytesDownloaded,
		ElapsedSeconds:    snap.elapsed.Seconds(),
		BytesPerSecond:    snap.rate,
		Summary:           summary,
	}

	if snap.hasRem {
		rem := snap.remaining
		line.Remaining = &rem
	}

	data, err := json.Marshal(line)
	if err != nil {
		return fmt.Errorf("encode progress: %w", err)
	}

	data = append(data, '\n')

	_, err = r.w.Write(data)
	if err != nil {
		return fmt.Errorf("write progress: %w", err)
	}

	return nil
}

// humanBytes renders a byte count in binary (IEC) units to one decimal place.
func humanBytes(n int64) string {
	const unit = 1024

	if n < unit {
		return fmt.Sprintf("%d B", n)
	}

	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}

	units := [...]string{"KiB", "MiB", "GiB", "TiB", "PiB", "EiB"}

	return fmt.Sprintf("%.1f %s", float64(n)/float64(div), units[exp])
}
