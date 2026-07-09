package progress

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"

	"go.jacobcolvin.com/hcp_archiver/config"
	"go.jacobcolvin.com/hcp_archiver/manifest"
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
// [Reporter.Summary], [Reporter.SetPhase], [Reporter.SetTotal], and
// [Reporter.Advance] are guarded by a mutex, so a background [Reporter.Run] loop
// and the archiver may touch it at once.
//
// On an interactive terminal the human mode drives a Bubble Tea panel; off one
// (a pipe, a redirect, or a test buffer) it falls back to a logfmt line. The
// total and completed counters track a phase's unit progress (a weighted
// count the archiver sets and advances) and are independent of the object
// tally read from source.
type Reporter struct {
	w         io.Writer
	in        io.Reader
	source    TallySource
	now       func() time.Time
	ttyForce  *bool
	sink      LogSink
	interrupt func()
	start     time.Time
	phase     string
	mode      config.ProgressMode
	interval  time.Duration
	total     int
	completed int
	mu        sync.Mutex
}

// Option configures a [Reporter] passed to [New].
//
// The available options are:
//   - [WithInterval]
//   - [WithClock]
//   - [WithTTY]
//   - [WithTotal]
//   - [WithInput]
//   - [WithLogSink]
//   - [WithInterrupt]
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

// WithTotal seeds the current phase's unit-progress denominator, the same value
// [Reporter.SetTotal] sets later. A non-positive value means indeterminate,
// which is the default and shows a spinner rather than a bar. It returns an
// [Option].
func WithTotal(total int) Option {
	return func(r *Reporter) {
		r.total = total
	}
}

// WithInput sets the reader the terminal UI reads keys from, defaulting to
// [os.Stdin]. A nil reader keeps the default. It returns an [Option].
func WithInput(in io.Reader) Option {
	return func(r *Reporter) {
		if in != nil {
			r.in = in
		}
	}
}

// WithLogSink routes the terminal UI's log output through sink so log lines and
// the live panel share one renderer. Without it the UI still runs, but
// concurrent log writes to the same terminal can corrupt the panel. It returns
// an [Option].
func WithLogSink(sink LogSink) Option {
	return func(r *Reporter) {
		r.sink = sink
	}
}

// WithInterrupt sets the callback the terminal UI invokes when the operator
// presses ctrl+c or q, used to cancel the whole run under raw mode where the
// kernel does not raise SIGINT. It returns an [Option].
func WithInterrupt(fn func()) Option {
	return func(r *Reporter) {
		r.interrupt = fn
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
		in:       os.Stdin,
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

// SetTotal sets the current phase's unit-progress denominator and resets the
// completed counter to zero, so each phase starts its bar fresh. A non-positive
// value marks the phase indeterminate, rendering a spinner instead of a bar. It
// is safe to call concurrently.
func (r *Reporter) SetTotal(total int) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.total = total
	r.completed = 0
}

// Advance adds n to the phase's completed unit count, moving the bar toward its
// total. It is safe to call concurrently.
func (r *Reporter) Advance(n int) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.completed += n
}

// snapshot is one rendered line's worth of derived state, computed under lock.
type snapshot struct {
	phase     string
	tally     manifest.Tally
	elapsed   time.Duration
	rate      float64
	total     int
	completed int
}

// hasBar reports whether the phase is determinate, so the view renders a bar and
// a completed=x/y fraction rather than a spinner alone.
func (s snapshot) hasBar() bool {
	return s.total > 0
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

	return snapshot{
		tally:     t,
		phase:     r.phase,
		elapsed:   elapsed,
		rate:      rate,
		total:     r.total,
		completed: r.completed,
	}
}

// usesPanel reports whether the live view is the Bubble Tea panel rather than a
// logfmt/JSON line: human mode on an interactive terminal. It is resolved from
// static state, so both [Reporter.Run] and [Reporter.Summary] agree on it.
func (r *Reporter) usesPanel() bool {
	return r.mode == config.ProgressModeHuman && r.isTTY()
}

// lockedTake returns a snapshot under the mutex, the accessor the panel's model
// calls each frame. It is safe to call concurrently.
func (r *Reporter) lockedTake() snapshot {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.take()
}

// Report renders one snapshot in the resolved mode. It writes nothing in quiet
// mode and returns any write error.
func (r *Reporter) Report() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.render(r.take(), false)
}

// Summary renders the final totals per status class, the wall time, and the
// errored count. On an interactive terminal in human mode it writes a styled
// block; otherwise it renders the logfmt or JSON summary line. It writes nothing
// in quiet mode and returns any write error.
func (r *Reporter) Summary() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.usesPanel() {
		return r.writeString(r.summaryBlock(r.take()))
	}

	return r.render(r.take(), true)
}

// Run drives the live view until ctx is done, then returns nil.
//
// In human mode on an interactive terminal it runs the Bubble Tea panel, whose
// spinner drives repaints; otherwise it reports on a fixed cadence. A
// non-positive interval falls back to the reporter's configured interval. It is
// a convenience over [Reporter.Report]; the archiver may drive Report directly
// instead.
func (r *Reporter) Run(ctx context.Context, interval time.Duration) error {
	if r.usesPanel() {
		return r.runTUI(ctx)
	}

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

// runTUI runs the Bubble Tea panel until ctx is done or the operator quits.
//
// It binds the program to ctx so an external SIGINT (mapped upstream to a ctx
// cancel) or the archiver's own cancel stops it, and registers the program on
// the log sink so log lines route through the one renderer, clearing it again
// on return so the fallback is restored before the next org logs. A ctx-driven
// stop surfaces as [tea.ErrProgramKilled], which is mapped to a clean nil.
func (r *Reporter) runTUI(ctx context.Context) error {
	program := tea.NewProgram(
		newTUIModel(r.lockedTake, r.interrupt),
		tea.WithContext(ctx),
		tea.WithOutput(r.w),
		tea.WithInput(r.in),
		tea.WithoutSignalHandler(),
	)

	if r.sink != nil {
		r.sink.SetProgram(program)
		defer r.sink.SetProgram(nil)
	}

	_, err := program.Run()
	if err != nil {
		if errors.Is(err, tea.ErrProgramKilled) {
			return nil
		}

		return fmt.Errorf("run progress ui: %w", err)
	}

	return nil
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

	if t.Resumed {
		b.WriteString(" resumed=true")
	}

	fmt.Fprintf(&b, " done=%d errored=%d forbidden=%d", t.Done, t.Errored, t.Forbidden)

	if summary {
		fmt.Fprintf(
			&b,
			" absent=%d skipped=%d n/a=%d total=%d",
			t.AbsentPermanently,
			t.Skipped,
			t.NotApplicable,
			t.Total(),
		)
	} else if snap.hasBar() {
		fmt.Fprintf(&b, " completed=%d/%d", snap.completed, snap.total)
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

// summaryBlock formats the styled multi-line summary written when the run ends
// on an interactive terminal, ending in a newline.
func (r *Reporter) summaryBlock(snap snapshot) string {
	t := snap.tally

	title := "archive complete"
	if t.Resumed {
		title += " (resumed)"
	}

	head := styleSummaryHead.Render(title)

	// Zero widths keep the summary's counts tight; the live panel pads them so
	// its columns hold still.
	counts := "  " + statusCounts(t, 0, 0, 0)

	rest := styleMeta.Render(fmt.Sprintf(
		"  absent=%d skipped=%d n/a=%d total=%d bytes=%s elapsed=%s",
		t.AbsentPermanently,
		t.Skipped,
		t.NotApplicable,
		t.Total(),
		humanBytes(t.BytesDownloaded),
		snap.elapsed.Round(time.Second),
	))

	return head + "\n" + counts + "\n" + rest + "\n"
}

// jsonLine is the machine-readable shape emitted one per line.
//
// PhaseTotal and PhaseCompleted report the current phase's weighted unit
// progress and are present only while a phase is determinate.
type jsonLine struct {
	PhaseTotal        *int    `json:"phaseTotal,omitempty"`
	PhaseCompleted    *int    `json:"phaseCompleted,omitempty"`
	Phase             string  `json:"phase,omitempty"`
	Target            string  `json:"target,omitempty"`
	Done              int     `json:"done"`
	AbsentPermanently int     `json:"absentPermanently"`
	Skipped           int     `json:"skipped"`
	Errored           int     `json:"errored"`
	Forbidden         int     `json:"forbidden"`
	NotApplicable     int     `json:"notApplicable"`
	Total             int     `json:"total"`
	BytesDownloaded   int64   `json:"bytesDownloaded"`
	ElapsedSeconds    float64 `json:"elapsedSeconds"`
	BytesPerSecond    float64 `json:"bytesPerSecond"`
	Summary           bool    `json:"summary,omitempty"`
	Resumed           bool    `json:"resumed,omitempty"`
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
		Forbidden:         t.Forbidden,
		NotApplicable:     t.NotApplicable,
		Total:             t.Total(),
		BytesDownloaded:   t.BytesDownloaded,
		ElapsedSeconds:    snap.elapsed.Seconds(),
		BytesPerSecond:    snap.rate,
		Summary:           summary,
		Resumed:           t.Resumed,
	}

	if snap.hasBar() {
		total := snap.total
		completed := snap.completed
		line.PhaseTotal = &total
		line.PhaseCompleted = &completed
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
