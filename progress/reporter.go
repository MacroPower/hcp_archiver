package progress

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	tea "charm.land/bubbletea/v2"

	"go.jacobcolvin.com/hcp_archiver/config"
	"go.jacobcolvin.com/hcp_archiver/manifest"
)

// TallySource supplies the live counters the reporter renders.
//
// The ledger satisfies it through its Tally method, so the reporter reads the
// same counters that back the manifest and the two can never disagree. See
// [manifest.Ledger] for an implementation.
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
	phaseStart  time.Time
	start       time.Time
	in          io.Reader
	source      TallySource
	rateStatus  func() (rps float64, pausedFor time.Duration)
	sink        LogSink
	w           io.Writer
	now         func() time.Time
	ttyForce    *bool
	interrupt   func()
	wireBytes   *atomic.Int64
	rateLimited *atomic.Int64
	tasks       map[*Task]struct{}
	mode        config.ProgressMode
	phase       string
	taskSeq     uint64
	interval    time.Duration
	total       int
	completed   int
	mu          sync.Mutex
	writeMu     sync.Mutex
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
//   - [WithWireBytes]
//   - [WithRateStatus]
//   - [WithRateLimited]
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

// WithWireBytes sets the shared counter of response-body bytes as they are
// delivered to the reader -- after any transport decompression, so not raw
// compressed bytes on the wire -- which the terminal UI's throughput window
// samples so the rate reads live while a large transfer is still in flight
// rather than only when whole objects commit. The displayed byte total stays
// the tally's committed archive bytes; only the rate's source changes. A nil
// counter keeps the rate sourced from committed bytes. It returns an [Option].
func WithWireBytes(counter *atomic.Int64) Option {
	return func(r *Reporter) {
		r.wireBytes = counter
	}
}

// WithRateStatus sets the source of the client's adaptive rate: the current
// requests-per-second the rate governor admits and how much of a rate-limit
// cooldown pause remains. Every output form renders it, so the run slowing
// under the server's pushback and recovering is visible as it happens; while
// a cooldown parks every request, the paused readout says why nothing is
// moving. A nil fn leaves the rate readout off. It returns an [Option].
func WithRateStatus(fn func() (rps float64, pausedFor time.Duration)) Option {
	return func(r *Reporter) {
		if fn != nil {
			r.rateStatus = fn
		}
	}
}

// WithRateLimited sets the shared counter of rate-limited (HTTP 429) responses
// observed on the wire, which the views surface alongside the rate readout
// so an operator can see why the rate dropped. A nil counter leaves the
// readout off. It returns an [Option].
func WithRateLimited(counter *atomic.Int64) Option {
	return func(r *Reporter) {
		r.rateLimited = counter
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
		tasks:    make(map[*Task]struct{}),
		interval: config.DefaultProgressInterval,
		total:    -1,
		mode:     mode,
	}

	for _, opt := range opts {
		opt(r)
	}

	r.start = r.now()
	r.phaseStart = r.start
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

// SetTotal sets the current phase's unit-progress denominator, resets the
// completed counter to zero so each phase starts its bar fresh, and restarts
// the phase clock that anchors the eta estimate. A non-positive value marks the
// phase indeterminate, rendering a spinner instead of a bar. It is safe to call
// concurrently.
func (r *Reporter) SetTotal(total int) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.total = total
	r.completed = 0
	r.phaseStart = r.now()

	// A new phase orphans any task still registered; end them so a straggling
	// Advance cannot leak units into the new phase's count.
	for t := range r.tasks {
		t.ended = true
	}

	clear(r.tasks)
}

// Advance adds n to the phase's completed unit count, moving the bar toward its
// total. It is safe to call concurrently.
func (r *Reporter) Advance(n int) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.completed += n
}

// Task tracks one named work item within the current phase (one workspace in
// the workspaces phase), so the panel can show progress inside a long item and
// the phase bar can move as the item's units land rather than only when the
// whole item finishes. Create instances with [Reporter.StartTask]. A Task is
// safe for concurrent use.
type Task struct {
	r     *Reporter
	name  string
	seq   uint64
	total int
	done  int
	ended bool
}

// StartTask registers a task named name carrying total phase units and returns
// its [Task]. Registering moves nothing; units flow into the phase's completed
// count through [Task.Advance] and [Task.Done]. Tasks render in registration
// order, so the panel's rows hold still as their bars move. It is safe to call
// concurrently.
func (r *Reporter) StartTask(name string, total int) *Task {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.taskSeq++

	t := &Task{r: r, name: name, seq: r.taskSeq, total: total}
	r.tasks[t] = struct{}{}

	return t
}

// Advance adds n units to the task and the same n to the phase's completed
// count, moving both bars. A task carries exactly its registered total in phase
// units, so advances past that total are absorbed: an item that turns out
// larger than its estimated weight (a workspace whose run listing outgrows its
// advertised run count) reads complete while the extra work drains, rather than
// pushing either bar past what the item was budgeted. Advances after
// [Task.Done] are dropped so a straggler cannot double-count. It is safe to
// call concurrently.
func (t *Task) Advance(n int) {
	t.r.mu.Lock()
	defer t.r.mu.Unlock()

	if t.ended {
		return
	}

	n = min(n, t.total-t.done)
	if n <= 0 {
		return
	}

	t.done += n
	t.r.completed += n
}

// Done settles the task: it commits any un-advanced remainder to the phase's
// completed count, so an item that failed midway or stopped early on settled
// history still moves the bar by its full weight, and removes the task from the
// panel. It is idempotent and safe to call concurrently.
func (t *Task) Done() {
	t.r.mu.Lock()
	defer t.r.mu.Unlock()

	if t.ended {
		return
	}

	t.ended = true
	delete(t.r.tasks, t)

	if remainder := t.total - t.done; remainder > 0 {
		t.done = t.total
		t.r.completed += remainder
	}
}

// taskProgress is one in-flight task's state captured into a snapshot.
type taskProgress struct {
	name  string
	total int
	done  int
}

// takeTasks copies the registered tasks in registration order, so the panel's
// rows hold still while their bars move. The caller holds the mutex.
func (r *Reporter) takeTasks() []taskProgress {
	if len(r.tasks) == 0 {
		return nil
	}

	ordered := make([]*Task, 0, len(r.tasks))
	for t := range r.tasks {
		ordered = append(ordered, t)
	}

	slices.SortFunc(ordered, func(a, b *Task) int {
		return cmp.Compare(a.seq, b.seq)
	})

	tasks := make([]taskProgress, 0, len(ordered))
	for _, t := range ordered {
		tasks = append(tasks, taskProgress{name: t.name, total: t.total, done: t.done})
	}

	return tasks
}

// snapshot is one rendered frame's worth of derived state, computed under lock.
//
// The hasRate flag is set from the option's presence ([WithRateStatus]), not
// from the sampled figures, so a reading that is transiently zero does not
// drop the readout at exactly the moment throttling is most interesting.
type snapshot struct {
	phase        string
	tasks        []taskProgress
	tally        manifest.Tally
	elapsed      time.Duration
	phaseElapsed time.Duration
	rate         float64
	rps          float64
	pausedFor    time.Duration
	wireBytes    int64
	rateLimited  int64
	total        int
	completed    int
	hasRate      bool
}

// hasBar reports whether the phase is determinate, so the view renders a bar and
// a completed=x/y fraction rather than a spinner alone.
func (s snapshot) hasBar() bool {
	return s.total > 0
}

// hasTask reports whether any work item is in flight, so the view renders the
// per-item progress under the phase bar.
func (s snapshot) hasTask() bool {
	return len(s.tasks) > 0
}

// largestTask returns the in-flight task with the most remaining units, ties
// broken by name: the one item the line-oriented modes report, and the item
// that will hold the phase open longest. It must not be called on a snapshot
// with no tasks.
func (s snapshot) largestTask() taskProgress {
	best := s.tasks[0]

	for _, t := range s.tasks[1:] {
		tRem, bestRem := t.total-t.done, best.total-best.done
		if tRem > bestRem || (tRem == bestRem && t.name < best.name) {
			best = t
		}
	}

	return best
}

// eta estimates the current phase's remaining time by extrapolating its elapsed
// time over the units still outstanding. It reports false when no estimate
// exists: an indeterminate phase, no unit landed yet, a finished (or
// overcounted) phase, or a phase clock that has not advanced.
func (s snapshot) eta() (time.Duration, bool) {
	if !s.hasBar() || s.completed <= 0 || s.completed >= s.total || s.phaseElapsed <= 0 {
		return 0, false
	}

	perUnit := s.phaseElapsed / time.Duration(s.completed)

	return perUnit * time.Duration(s.total-s.completed), true
}

// take builds a [snapshot] from the current tally and clock. The caller holds
// the mutex.
func (r *Reporter) take() snapshot {
	t := r.source.Tally()
	now := r.now()
	elapsed := now.Sub(r.start)

	rate := 0.0
	if elapsed > 0 {
		rate = float64(t.BytesDownloaded) / elapsed.Seconds()
	}

	// Without a wire counter the committed bytes stand in, so the terminal UI's
	// throughput window still moves for a reporter built without the option.
	wire := t.BytesDownloaded
	if r.wireBytes != nil {
		wire = r.wireBytes.Load()
	}

	var (
		rps       float64
		pausedFor time.Duration
	)

	// Sampling nests reporter.mu -> governor.mu; safe, since the governor
	// never calls back into the reporter.
	if r.rateStatus != nil {
		rps, pausedFor = r.rateStatus()
	}

	var rateLimited int64

	if r.rateLimited != nil {
		rateLimited = r.rateLimited.Load()
	}

	return snapshot{
		tally:        t,
		phase:        r.phase,
		tasks:        r.takeTasks(),
		elapsed:      elapsed,
		phaseElapsed: now.Sub(r.phaseStart),
		rate:         rate,
		rps:          rps,
		pausedFor:    pausedFor,
		wireBytes:    wire,
		rateLimited:  rateLimited,
		total:        r.total,
		completed:    r.completed,
		hasRate:      r.rateStatus != nil,
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
//
// The snapshot is taken under the state mutex, which is then released before the
// write, so a slow or back-pressured writer never stalls the hot-path
// [Task.Advance] and [Reporter.StartTask] callers that contend for that mutex.
// A dedicated write mutex serializes the writes among themselves.
func (r *Reporter) Report() error {
	snap := r.lockedTake()

	r.writeMu.Lock()
	defer r.writeMu.Unlock()

	return r.render(snap, false)
}

// Summary renders the final totals per status class, the wall time, and the
// errored count. On an interactive terminal in human mode it writes a styled
// block; otherwise it renders the logfmt or JSON summary line. It writes nothing
// in quiet mode and returns any write error.
func (r *Reporter) Summary() error {
	snap := r.lockedTake()

	r.writeMu.Lock()
	defer r.writeMu.Unlock()

	// The panel check reads only static state, so it needs no state lock here.
	if r.usesPanel() {
		return r.writeString(r.summaryBlock(snap))
	}

	return r.render(snap, true)
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

// quitGrace is how long a ctx-canceled panel gets to process the quit request
// and erase itself in a final render before it is killed outright, so a wedged
// program can never hang the run's shutdown.
const quitGrace = 2 * time.Second

// runTUI runs the Bubble Tea panel until ctx is done or the operator quits.
//
// A ctx cancel (an external SIGINT mapped upstream, or the archiver's own
// cancel) requests a graceful quit rather than killing the program: the
// model's final render then erases the panel, so the closing log lines and
// summary flow on a clean tail instead of around a stale frame, which a kill
// would strand by skipping that render. A program that fails to quit within
// [quitGrace] is killed, and the kill's [tea.ErrProgramKilled] is mapped to a
// clean nil. It registers the program on the log sink so log lines route
// through the one renderer, clearing it again on return so the fallback is
// restored before the next org logs.
func (r *Reporter) runTUI(ctx context.Context) error {
	program := tea.NewProgram(
		newTUIModel(r.lockedTake, r.interrupt),
		tea.WithOutput(r.w),
		tea.WithInput(r.in),
		tea.WithoutSignalHandler(),
	)

	if r.sink != nil {
		r.sink.SetProgram(program)
		defer r.sink.SetProgram(nil)
	}

	finished := make(chan struct{})
	defer close(finished)

	go func() {
		select {
		case <-finished:
			return
		case <-ctx.Done():
		}

		program.Send(quitRequestMsg{})

		select {
		case <-finished:
		case <-time.After(quitGrace):
			program.Kill()
		}
	}()

	_, err := program.Run()
	if err != nil {
		if errors.Is(err, tea.ErrProgramKilled) {
			return nil
		}

		return fmt.Errorf("run progress ui: %w", err)
	}

	return nil
}

// render writes one snapshot in the resolved mode. It reads only the snapshot
// and static reporter state, so it runs with the state mutex released; the
// caller holds writeMu to serialize the writes among themselves.
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

	fmt.Fprintf(&b, " done=%d errored=%d forbidden=%d retried=%d", t.Done, t.Errored, t.Forbidden, t.Retried)

	if summary {
		fmt.Fprintf(
			&b,
			" absent=%d skipped=%d n/a=%d total=%d",
			t.Absent,
			t.Skipped,
			t.NotApplicable,
			t.Total(),
		)
	} else if snap.hasBar() {
		fmt.Fprintf(&b, " completed=%d/%d", snap.completed, snap.total)

		if eta, ok := snap.eta(); ok {
			fmt.Fprintf(&b, " eta=%s", compactDuration(eta))
		}
	}

	if !summary && snap.hasTask() {
		task := snap.largestTask()
		fmt.Fprintf(&b, " task=%q taskCompleted=%d/%d tasks=%d",
			task.name, task.done, task.total, len(snap.tasks))
	}

	// The request rate is a live figure, so only progress lines carry it (a
	// paused readout marks a rate-limit cooldown); the rate-limited total is a
	// run outcome worth keeping on the summary too. The key is rps, since
	// rate= already names byte throughput on this line.
	if !summary && snap.hasRate {
		fmt.Fprintf(&b, " rps=%.0f", snap.rps)

		if snap.pausedFor > 0 {
			fmt.Fprintf(&b, " paused=%s", compactDuration(snap.pausedFor))
		}
	}

	if snap.rateLimited > 0 {
		fmt.Fprintf(&b, " rateLimited=%d", snap.rateLimited)
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
// on an interactive terminal, ending in a newline. A left rule frames the block
// as one unit, and the heading's glyph carries the outcome at a glance: a green
// check for a clean run, and the errored or forbidden mark (errors taking
// precedence) when anything went wrong. The block only counts failures; the
// archiver logs each one in full, above the block, where nothing truncates the
// error text.
func (r *Reporter) summaryBlock(snap snapshot) string {
	t := snap.tally

	glyph := styleDone.Render(glyphDone)

	switch {
	case t.Errored > 0:
		glyph = styleErrored.Render(glyphErrored)
	case t.Forbidden > 0:
		glyph = styleForbidden.Render(glyphForbidden)
	}

	title := "archive complete"
	if t.Resumed {
		title += " (resumed)"
	}

	meta := fmt.Sprintf(
		"absent %d · skipped %d · n/a %d · total %d · %s · %s",
		t.Absent,
		t.Skipped,
		t.NotApplicable,
		t.Total(),
		humanBytes(t.BytesDownloaded),
		snap.elapsed.Round(time.Second),
	)

	lines := []string{
		glyph + " " + styleSummaryHead.Render(title),
		// Zero widths keep the summary's counts tight; the live panel pads them
		// so its columns hold still.
		statusCounts(t, 0, 0, 0, 0),
		styleMeta.Render(meta),
	}

	// How throttled the run was is an outcome worth closing on; the amber
	// readout mirrors the live panel's so the two connect at a glance.
	if snap.rateLimited > 0 {
		lines[2] += " " + styleRateLimited.Render(fmt.Sprintf("· rate limited %d", snap.rateLimited))
	}

	return styleSummaryRule.Render(strings.Join(lines, "\n")) + "\n"
}

// jsonLine is the machine-readable shape emitted one per line.
//
// PhaseTotal and PhaseCompleted report the current phase's weighted unit
// progress and are present only while a phase is determinate. Task, TaskTotal,
// and TaskCompleted report the in-flight work item with the most remaining
// units, with TasksActive counting every registered item; all are present only
// while at least one is registered. RequestsPerSecond reports the adaptive
// rate governor's current admitted request rate, present on progress lines
// only when the reporter watches a rate source, with PausedMs carrying the
// remaining rate-limit cooldown while one is in force; RateLimited is the
// cumulative count of rate-limited (429) responses, present once any were
// observed.
type jsonLine struct {
	PhaseTotal        *int     `json:"phaseTotal,omitempty"`
	PhaseCompleted    *int     `json:"phaseCompleted,omitempty"`
	TaskTotal         *int     `json:"taskTotal,omitempty"`
	TaskCompleted     *int     `json:"taskCompleted,omitempty"`
	RequestsPerSecond *float64 `json:"requestsPerSecond,omitempty"`
	Phase             string   `json:"phase,omitempty"`
	Task              string   `json:"task,omitempty"`
	Target            string   `json:"target,omitempty"`
	TasksActive       int      `json:"tasksActive,omitempty"`
	PausedMs          int64    `json:"pausedMs,omitempty"`
	RateLimited       int64    `json:"rateLimited,omitempty"`
	Done              int      `json:"done"`
	Absent            int      `json:"absent"`
	Skipped           int      `json:"skipped"`
	Errored           int      `json:"errored"`
	Forbidden         int      `json:"forbidden"`
	NotApplicable     int      `json:"notApplicable"`
	Retried           int64    `json:"retried"`
	Total             int      `json:"total"`
	BytesDownloaded   int64    `json:"bytesDownloaded"`
	ElapsedSeconds    float64  `json:"elapsedSeconds"`
	BytesPerSecond    float64  `json:"bytesPerSecond"`
	Summary           bool     `json:"summary,omitempty"`
	Resumed           bool     `json:"resumed,omitempty"`
}

// writeJSON encodes one snapshot as a compact JSON object followed by a
// newline.
func (r *Reporter) writeJSON(snap snapshot, summary bool) error {
	t := snap.tally

	line := jsonLine{
		Phase:           snap.phase,
		Target:          t.Target,
		Done:            t.Done,
		Absent:          t.Absent,
		Skipped:         t.Skipped,
		Errored:         t.Errored,
		Forbidden:       t.Forbidden,
		NotApplicable:   t.NotApplicable,
		Retried:         t.Retried,
		Total:           t.Total(),
		BytesDownloaded: t.BytesDownloaded,
		ElapsedSeconds:  snap.elapsed.Seconds(),
		BytesPerSecond:  snap.rate,
		RateLimited:     snap.rateLimited,
		Summary:         summary,
		Resumed:         t.Resumed,
	}

	// A live figure, kept off the summary line to match logfmt: once the run
	// has ended the momentary rate is meaningless. A pointer, not a bare
	// float, so a rate that reads zero mid-cooldown is still emitted rather
	// than dropped by omitempty -- that is the moment throttling is most
	// worth reporting.
	if !summary && snap.hasRate {
		rps := snap.rps
		line.RequestsPerSecond = &rps
		line.PausedMs = snap.pausedFor.Milliseconds()
	}

	if snap.hasBar() {
		total := snap.total
		completed := snap.completed
		line.PhaseTotal = &total
		line.PhaseCompleted = &completed
	}

	if !summary && snap.hasTask() {
		task := snap.largestTask()
		taskTotal := task.total
		taskDone := task.done
		line.Task = task.name
		line.TaskTotal = &taskTotal
		line.TaskCompleted = &taskDone
		line.TasksActive = len(snap.tasks)
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
