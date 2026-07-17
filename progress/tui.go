package progress

import (
	"fmt"
	"strings"
	"time"

	"charm.land/bubbles/v2/spinner"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	progressbar "charm.land/bubbles/v2/progress"
	tea "charm.land/bubbletea/v2"

	"go.jacobcolvin.com/hcp_archiver/manifest"
	"go.jacobcolvin.com/hcp_archiver/theme"
)

// Fixed column widths that keep the panel from reflowing as values change. Every
// field before the trailing target (line one) or metadata (line two) occupies a
// constant width, so a growing count or an appearing bar never shifts what
// follows it. The bar, percent, and eta hold their columns whether the phase is
// determinate or not, so the target stays put across phase transitions.
const (
	barWidth   = 25 // determinate bar and indeterminate marquee track.
	phaseWidth = 10 // widest phase name ("workspaces").
	pctWidth   = 4  // "100%" down to "  0%".
	etaWidth   = 10 // "eta 59m59s", the widest value compactDuration emits.

	// The display width of the spinner cell heading line one (the glyph and its
	// trailing space). The task rows indent by the whole lead — spinner cell,
	// phase, separating space — so their bars sit exactly under the phase bar;
	// deriving both from the same constants is what keeps the grid aligned.
	spinnerCellWidth = 2
	rowLeadWidth     = spinnerCellWidth + phaseWidth + 1

	marqueeBlock = 5 // moving block width within the indeterminate track.

	// The width assumed when the terminal reports none (an output that is not
	// a real tty cannot be sized), so the panel and the log flow still run
	// instead of waiting forever for a real measurement.
	fallbackWidth = 80

	// Digit reserves for the per-status counts; the numbers left-align into
	// invisible trailing pad, so the next column never moves until a count
	// exceeds its reserve (rare, and then only once as it gains a digit). Done
	// spans the whole org, so it reserves the most.
	countDoneWidth      = 6
	countErroredWidth   = 4
	countForbiddenWidth = 4
	countRetriedWidth   = 4

	// The metadata readouts pad to the width of the status column above them
	// (its label plus digit reserve), so the panel's closing two lines align as
	// one grid. The request-rate readout closes the line and needs no pad.
	metaBytesWidth   = len("done ") + countDoneWidth
	metaRateWidth    = len("errored ") + countErroredWidth
	metaElapsedWidth = len("forbidden ") + countForbiddenWidth
)

// maxTaskLines caps how many in-flight work items the panel lists, so a wide
// fan-out cannot grow the panel past a screenful; the overflow line counts
// the rest.
const maxTaskLines = 8

// Bar cell glyphs, matching the determinate bar's defaults so the marquee looks
// like the same component.
const (
	barFullChar  = "▌"
	barEmptyChar = "░"
)

// rateWindow bounds the span of throughput samples the panel averages over, so
// the rate readout tracks what the run is doing now (and visibly drops on a
// stall) rather than a lifetime average that goes stale as the run ages.
const rateWindow = 5 * time.Second

// rateSample is one throughput observation: the run's cumulative byte count at
// a point on its elapsed clock.
type rateSample struct {
	at    time.Duration
	bytes int64
}

// quitRequestMsg asks the model to quit gracefully: it marks the model
// quitting, whose view is empty, so the program's final render erases the
// panel and the log stream and summary flow on a clean tail instead of around
// a stale frame. The reporter sends it when its context is canceled.
type quitRequestMsg struct{}

// logFlushedMsg acknowledges that the previously emitted log batch has been
// printed above the panel, releasing the next batch. Sequencing batches behind
// this ack keeps at most one in flight, so two batches can never race each
// other through the command goroutines and print out of order.
type logFlushedMsg struct{}

// logFeed is the view of a [LogSink] the model consumes: peek the queued
// lines, print them, and commit them once the printing is confirmed. The
// reporter hands the model a revocable implementation, so an abandoned
// program cannot keep consuming a sink a later panel has taken over.
type logFeed interface {
	// Peek returns the queued lines without removing them, and the cursor to
	// commit them under.
	Peek() ([]string, uint64)
	// Commit removes the lines the peek under cursor returned.
	Commit(cursor uint64)
}

// tuiModel is the Bubble Tea model backing the human view on a terminal.
//
// It owns no counters of its own: each render pulls a fresh [snapshot] through
// take, so the panel always mirrors the live ledger. The spinner is both the
// liveness indicator and the repaint clock. Its tick drives re-renders,
// advances the indeterminate marquee, and records a throughput sample, so the
// bar, counts, and rate move without any separate ticker. Create instances with
// [newTUIModel].
type tuiModel struct {
	bar           progressbar.Model
	spin          spinner.Model
	take          func() snapshot
	interrupt     func()
	logs          logFeed
	samples       []rateSample
	uploadSamples []rateSample
	snap          snapshot
	width         int
	height        int
	tick          int
	// The held height of the rendered frame in lines. It ratchets up to the
	// tallest frame rendered so far and never comes back down while the program
	// runs (only a terminal too short for it re-clamps it): the inline renderer
	// erases and resizes on a shrinking frame, which corrupts the panel when a
	// log line is inserted above it at the same moment. Shorter content pads
	// with trailing blank lines up to this height (see padFrame).
	frame    int
	quitting bool
	sampled  bool
	// Whether a log batch is in flight: emitted but not yet acknowledged by its
	// [logFlushedMsg]. The next batch waits for the ack, so batches print in
	// order. The cursor identifies the in-flight batch's lines, committed out
	// of the feed only once the ack confirms they printed.
	logsInFlight bool
	logCursor    uint64
}

// newTUIModel creates a new [tuiModel] that renders snapshots from take, on a
// quit key invokes interrupt before quitting, and prints the log lines fed by
// logs above the panel. Any of the arguments may be nil in tests that only
// exercise rendering (a nil feed simply never prints). It returns a pointer
// so Bubble Tea passes the heavy model around by reference.
func newTUIModel(take func() snapshot, interrupt func(), logs logFeed) *tuiModel {
	spin := spinner.New(spinner.WithSpinner(spinner.Dot))
	spin.Style = styleSpinner

	bar := progressbar.New(
		progressbar.WithWidth(barWidth),
		progressbar.WithoutPercentage(),
		progressbar.WithColors(barBlendStart, barBlendEnd),
	)
	bar.EmptyColor = barEmptyColor

	return &tuiModel{
		spin:      spin,
		bar:       bar,
		take:      take,
		interrupt: interrupt,
		logs:      logs,
	}
}

// Init starts the spinner, whose ticks drive every subsequent repaint.
func (m *tuiModel) Init() tea.Cmd {
	return m.spin.Tick
}

// Update advances the spinner, marquee, and throughput window on each tick,
// drains queued log lines into the stream above the panel, records the
// terminal size, and quits on a [quitRequestMsg] or, running the interrupt
// callback first, on ctrl+c or q (raw mode suppresses the kernel's SIGINT, so
// the quit keys are handled here). Every quit path marks the model quitting so
// the final render erases the panel, and sequences a last log flush before the
// quit so queued lines land in scrollback rather than waiting for the sink's
// fallback flush.
func (m *tuiModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case quitRequestMsg:
		cmd := m.quit()

		return m, cmd

	case logFlushedMsg:
		// The in-flight batch has printed: commit its lines out of the feed —
		// only now, so a program that died mid-batch leaves them queued for
		// the sink's fallback flush instead of losing them — and release the
		// next batch, so a burst of logging flows across acks rather than
		// waiting for ticks.
		m.logsInFlight = false
		if m.logs != nil {
			m.logs.Commit(m.logCursor)
		}

		cmd := m.flushLogs()

		return m, cmd

	case tea.KeyPressMsg:
		switch msg.String() {
		case "ctrl+c", "q":
			if m.interrupt != nil {
				m.interrupt()
			}

			cmd := m.quit()

			return m, cmd
		}

	case tea.WindowSizeMsg:
		// Record the size to clip each line and bound the task list, and shrink
		// the bar so the panel never wraps onto an extra line. A fresh model (as
		// the golden tests use) keeps the full barWidth and does not clip. A
		// size message reporting no width (the program's output is not a real
		// terminal, so the size cannot be queried) falls back to the
		// conventional 80 columns: the panel and the log flow both gate on a
		// known width, and without the fallback they would stay dark for the
		// whole run.
		m.width = msg.Width
		if m.width <= 0 {
			m.width = fallbackWidth
		}

		m.height = msg.Height
		m.bar.SetWidth(min(barWidth, m.width))

		return m, nil

	case spinner.TickMsg:
		var cmd tea.Cmd

		m.tick++
		if m.take != nil {
			// Snapshot once per tick and cache it, so the View that follows this
			// Update renders the same snapshot the sample was drawn from rather
			// than locking the reporter and re-tallying the ledger a second time.
			m.snap = m.take()
			m.sampled = true
			m.observe(m.snap)
		}

		m.spin, cmd = m.spin.Update(msg)

		return m, tea.Batch(cmd, m.flushLogs())
	}

	return m, nil
}

// quit marks the model quitting and returns the command ending the program: a
// final log flush sequenced ahead of the quit, so lines queued at shutdown
// print above the panel — and their ack commits them — before the empty final
// render erases it. The flush honors the in-flight ack gate like any other —
// with a batch still in flight, printing another here could race it — and
// whatever stays uncommitted is not lost: the reporter's deactivation flushes
// the sink's residue to the fallback writer once the program has stopped.
func (m *tuiModel) quit() tea.Cmd {
	m.quitting = true

	if flush := m.flushLogs(); flush != nil {
		return tea.Sequence(flush, tea.Quit)
	}

	return tea.Quit
}

// flushLogs peeks the feed's queued log lines and returns a command printing
// them above the panel, or nil when there is nothing to print, a batch is
// already in flight, or the terminal size is still unknown (unwrapped lines
// would corrupt the renderer's row accounting; the queue holds until the
// first size message). Each line is hard-wrapped to the terminal width — the
// inline renderer estimates a printed line's height from its width to scroll
// the panel out of the way, and a line left wider than the terminal makes
// that estimate wrong, shifting the panel's origin so every later repaint
// draws misaligned; pre-wrapped lines make the estimate exact. The print is
// sequenced with a [logFlushedMsg] ack, so the next batch waits until this
// one has landed (batches can never print out of order) and the lines commit
// out of the feed only once confirmed printed.
func (m *tuiModel) flushLogs() tea.Cmd {
	if m.logs == nil || m.logsInFlight || m.width <= 0 {
		return nil
	}

	lines, cursor := m.logs.Peek()
	if len(lines) == 0 {
		return nil
	}

	for i, line := range lines {
		lines[i] = ansi.Hardwrap(line, m.width, true)
	}

	m.logsInFlight = true
	m.logCursor = cursor

	return tea.Sequence(
		tea.Println(strings.Join(lines, "\n")),
		func() tea.Msg { return logFlushedMsg{} },
	)
}

// View renders the current snapshot into an inline (non-alt-screen) view, so log
// lines printed by the sink stay in scrollback above the pinned panel. A
// quitting model renders nothing, so the program's final render erases the
// panel rather than stranding its last frame in the output flow. An unsized
// model (before the first window-size message lands) also renders nothing:
// fit cannot clip without a width, and a line left wider than the terminal
// wraps onto extra rows the inline renderer does not account for, skewing its
// frame from the very first paint. The panel appears one message later, sized.
func (m *tuiModel) View() tea.View {
	if m.quitting || m.width <= 0 {
		return tea.NewView("")
	}

	// Render the snapshot the last tick cached; take a fresh one only before the
	// first tick has sampled (or in a render-only test with no take), so a normal
	// tick-then-repaint costs one snapshot, not two.
	snap := m.snap
	if !m.sampled && m.take != nil {
		snap = m.take()
	}

	return tea.NewView(m.render(snap))
}

// observe appends one throughput sample to each series and trims the windows'
// tails, keeping at least two samples so a rate can always be derived once
// ticks have flowed. Each series samples its snapshot wire-byte figure (the
// shared wire counter when the reporter has one, committed bytes otherwise),
// so the rates read live while a large object is still streaming and decay to
// zero only on true silence. Without a remote the upload figure is always
// zero and its rate never renders.
func (m *tuiModel) observe(snap snapshot) {
	m.samples = observeInto(m.samples, snap.elapsed, snap.wireBytes)
	m.uploadSamples = observeInto(m.uploadSamples, snap.elapsed, snap.uploadWireBytes)
}

// observeInto appends one throughput sample to samples and trims the window's
// tail, returning the updated series.
func observeInto(samples []rateSample, at time.Duration, bytes int64) []rateSample {
	samples = append(samples, rateSample{at: at, bytes: bytes})

	for len(samples) > 2 && samples[len(samples)-1].at-samples[0].at > rateWindow {
		samples = samples[1:]
	}

	return samples
}

// throughput returns the download rate over the sampled window, in bytes per
// second. Before two distinct samples exist (a fresh model, or a test render
// with no ticks) it falls back to the snapshot's lifetime average.
func (m *tuiModel) throughput(snap snapshot) float64 {
	return windowedRate(m.samples, snap.rate)
}

// uploadThroughput returns the remote upload rate over the sampled window, in
// bytes per second, with the same lifetime-average fallback as throughput.
func (m *tuiModel) uploadThroughput(snap snapshot) float64 {
	return windowedRate(m.uploadSamples, snap.uploadRate)
}

// windowedRate derives a rate from the first and last samples of a window,
// falling back to fallback before two spanning samples exist.
func windowedRate(samples []rateSample, fallback float64) float64 {
	if len(samples) < 2 {
		return fallback
	}

	first, last := samples[0], samples[len(samples)-1]

	span := (last.at - first.at).Seconds()
	if span <= 0 {
		return fallback
	}

	return float64(last.bytes-first.bytes) / span
}

// render formats a snapshot as the panel. Line one is the combined view: the
// spinner, a fixed-width phase, the bar or its indeterminate marquee, the
// percent, the eta, and the target. One line per in-flight work item follows,
// each with its own bar, percent, unit fraction, and name, in registration
// order so rows hold still as their bars move; items past the cap are counted
// on an overflow line. The closing lines carry the colored per-status counts,
// the byte, rate, and elapsed metadata, and — when a remote is configured —
// the remote-transfer readout. Every field before a line's trailing name or
// metadata holds a constant width, so the panel does not reflow as values
// change, and the whole frame pads out to its held height (see padFrame) so
// it never shrinks as work items finish or a smaller phase begins.
func (m *tuiModel) render(snap snapshot) string {
	var line1 strings.Builder

	line1.WriteString(m.spin.View())
	line1.WriteString(stylePhase.Render(padRight(snap.phase, phaseWidth)))
	line1.WriteByte(' ')

	if snap.hasBar() {
		line1.WriteString(m.barCell(snap.completed, snap.total))
	} else {
		line1.WriteString(marquee(m.bar.Width(), m.tick))
		fmt.Fprintf(&line1, " %s", strings.Repeat(" ", pctWidth))
	}

	// The eta column blanks when no estimate exists (an indeterminate phase, or
	// a determinate one before its first unit lands) but keeps its width, so the
	// target never moves as estimates come and go.
	if eta, ok := snap.eta(); ok {
		fmt.Fprintf(&line1, " %s", styleCount.Render(padRight("eta "+compactDuration(eta), etaWidth)))
	} else {
		fmt.Fprintf(&line1, " %s", strings.Repeat(" ", etaWidth))
	}

	if snap.tally.Target != "" {
		fmt.Fprintf(&line1, " %s %s",
			styleTargetMark.Render("▸"),
			styleTarget.Render(snap.tally.Target),
		)
	}

	// The resumed tag rides at the end of the line so nothing follows it, and it
	// is fixed for the whole run, so it never causes a mid-run reflow.
	if snap.tally.Resumed {
		fmt.Fprintf(&line1, "  %s", styleResumed.Render("(resumed)"))
	}

	t := snap.tally

	// The metadata line mirrors the counts line above it: each readout sits
	// under a status column, led by its own glyph and padded to that column's
	// width, so the panel's two closing lines read as one grid.
	meta := fmt.Sprintf("%s %-*s %s %-*s %s %-*s",
		glyphBytes, metaBytesWidth, theme.HumanBytes(t.BytesDownloaded),
		glyphRate, metaRateWidth, theme.HumanBytes(int64(m.throughput(snap)))+"/s",
		glyphElapsed, metaElapsedWidth, snap.elapsed.Round(time.Second).String(),
	)

	// The request-rate readout shows the adaptive governor moving: the rate it
	// currently admits. During a rate-limit cooldown an amber paused readout
	// follows (a cooldown parks every in-flight request, so without it the
	// panel would just look stuck), and once any rate limiting has been
	// observed the amber 429 total rides along too, so a slowed rate carries
	// its own explanation.
	if snap.hasRate {
		meta += fmt.Sprintf(" "+glyphRPS+" %.0f/s", snap.rps)
	}

	counts := "  " + statusCounts(t, countDoneWidth, countErroredWidth, countForbiddenWidth, countRetriedWidth)

	metaLine := "  " + styleMeta.Render(meta)
	if snap.hasRate && snap.pausedFor > 0 {
		metaLine += " " + styleRateLimited.Render("· paused "+compactDuration(snap.pausedFor))
	}

	if snap.rateLimited > 0 {
		metaLine += " " + styleRateLimited.Render(fmt.Sprintf("· 429s %d", snap.rateLimited))
	}

	budget := m.taskLineBudget(snap.hasRemote)

	// The body: line one and the task region.
	body := make([]string, 0, budget+2)
	body = append(body, m.fit(line1.String()))

	visible := min(len(snap.tasks), budget)
	for _, task := range snap.tasks[:visible] {
		body = append(body, m.fit(m.renderTask(task)))
	}

	if hidden := len(snap.tasks) - visible; hidden > 0 {
		body = append(body, m.fit("  "+styleMeta.Render(fmt.Sprintf("… +%d more active", hidden))))
	}

	footer := []string{m.fit(counts), m.fit(metaLine)}
	if snap.hasRemote {
		footer = append(footer, m.fit("  "+remoteReadout(snap.remote, m.uploadThroughput(snap), true)))
	}

	return strings.Join(m.composeFrame(body, footer), "\n")
}

// composeFrame joins the body and footer into the panel's frame, padding
// between them with blank lines up to the frame's held height and ratcheting
// that height to the natural line count. The frame only ever grows while the
// program runs — the inline renderer erases and resizes on a shrinking frame,
// which corrupts the panel when a log line is inserted above it at the same
// moment — so as work items finish or a phase with fewer rows begins, the
// frame holds steady instead of resizing. The padding sits between the task
// region and the footer, so the footer stays glued to the frame's last rows
// rather than bouncing with the live task count. Only a terminal too short
// for the held height pulls it back down, the one resize the renderer must
// handle anyway (the task budget keeps the natural content within the screen,
// so the clamped frame never truncates a live row).
func (m *tuiModel) composeFrame(body, footer []string) []string {
	m.frame = max(m.frame, len(body)+len(footer))
	if m.height > 0 {
		m.frame = min(m.frame, m.height)
	}

	lines := make([]string, 0, m.frame)
	lines = append(lines, body...)

	for len(lines)+len(footer) < m.frame {
		lines = append(lines, "")
	}

	return append(lines, footer...)
}

// chromeLines is how many non-task lines the panel renders around the task
// region: line one, the overflow slot, the counts line, the metadata line,
// and — when a remote is configured — the remote readout. The task-line
// budget and render's capacity hint both derive from it, so the two cannot
// drift apart.
func chromeLines(hasRemote bool) int {
	if hasRemote {
		return 5
	}

	return 4
}

// taskLineBudget bounds how many task lines the panel may show: the fixed cap,
// tightened on a short terminal so the panel (the task lines plus its
// chrome, see [chromeLines]) never outgrows the screen. An unknown height
// (before the first size message, and in the golden tests) keeps the fixed
// cap.
func (m *tuiModel) taskLineBudget(hasRemote bool) int {
	if m.height <= 0 {
		return maxTaskLines
	}

	return min(maxTaskLines, max(m.height-chromeLines(hasRemote), 0))
}

// renderTask formats one in-flight work item's line: its bar aligned under the
// phase bar, its percent, its unit fraction in the eta column, and its name in
// the target position, so the panel's lines read as one grid. The items shown
// are independent of line one's target (the target is wherever a worker last
// started), so each line names its own item.
func (m *tuiModel) renderTask(task taskProgress) string {
	var b strings.Builder

	b.WriteString(strings.Repeat(" ", rowLeadWidth))
	b.WriteString(m.barCell(task.done, task.total))

	// The fraction rides in the eta column; a fraction outgrowing the reserve
	// shifts only its own trailing name.
	fraction := fmt.Sprintf("%d/%d", task.done, task.total)
	fmt.Fprintf(&b, " %s", styleCount.Render(fmt.Sprintf("%-*s", etaWidth, fraction)))

	fmt.Fprintf(&b, " %s %s",
		styleTargetMark.Render("▸"),
		styleTarget.Render(task.name),
	)

	return b.String()
}

// barCell renders the bar-and-percent columns shared by line one and the task
// rows, so the two cannot drift apart. The completion ratio clamps to [0,1]:
// a miscounted done/total can never widen the percent past its 4-cell reserve
// or read negative, keeping the bar and its label in lockstep, and a
// non-positive total (which would otherwise divide to a NaN or an infinity)
// renders an empty bar at 0%. Truncating toward zero floors the percent, so
// it never reads 100% before the last unit lands.
func (m *tuiModel) barCell(done, total int) string {
	var percent float64

	if total > 0 {
		percent = min(max(float64(done)/float64(total), 0), 1)
	}

	return m.bar.ViewAs(percent) +
		" " + styleCount.Render(fmt.Sprintf("%3d%%", int(percent*100)))
}

// fit clips a rendered line to the terminal width so the panel never wraps onto
// an extra line. A zero width (before the first size message) leaves it whole.
func (m *tuiModel) fit(line string) string {
	if m.width <= 0 {
		return line
	}

	return ansi.Truncate(line, m.width, "")
}

// statusCounts renders the styled done/errored/forbidden/retried counts shared
// by the live panel and the summary block, each count led by its status glyph.
// The width arguments left-pad each count so the live panel's columns stay put
// as values grow; pass zero widths for the summary's tight spacing.
func statusCounts(t manifest.Tally, doneWidth, erroredWidth, forbiddenWidth, retriedWidth int) string {
	return fmt.Sprintf("%s %s %s %s",
		styleDone.Render(fmt.Sprintf(theme.GlyphOK+" done %-*d", doneWidth, t.Done)),
		styleErrored.Render(fmt.Sprintf(theme.GlyphError+" errored %-*d", erroredWidth, t.Errored)),
		styleForbidden.Render(fmt.Sprintf(theme.GlyphBlocked+" forbidden %-*d", forbiddenWidth, t.Forbidden)),
		styleRetried.Render(fmt.Sprintf(theme.GlyphRetry+" retried %-*d", retriedWidth, t.Retried)),
	)
}

// remoteReadout renders the remote-transfer tally shared by the live panel
// and the summary block: the transferred bytes, uploads, and evictions in the
// muted metadata tone, with the failed count riding in amber only once
// anything has failed, the same convention as the 429 readout. When showRate
// is set the live upload rate follows the byte total — the panel shows it,
// while the summary omits it because a momentary rate is meaningless once the
// run has ended. The rate takes no glyph of its own: the ·-separated position
// on the ☁ line and the bare /s suffix carry the meaning.
func remoteReadout(rs RemoteStats, rate float64, showRate bool) string {
	rateSeg := ""
	if showRate {
		rateSeg = fmt.Sprintf(" · %s/s", theme.HumanBytes(int64(rate)))
	}

	out := styleMeta.Render(fmt.Sprintf(glyphCloud+" %s%s · uploaded %d · evicted %d",
		theme.HumanBytes(rs.UploadedBytes), rateSeg, rs.Uploaded, rs.Evicted))

	if rs.Failed > 0 {
		out += " " + styleRateLimited.Render(fmt.Sprintf("· failed %d", rs.Failed))
	}

	return out
}

// compactDuration formats d for the eta column: bare seconds under a minute,
// minutes and seconds under an hour, then hours and minutes, saturating at
// ">99h" so the widest value ("eta 59m59s") bounds the column.
func compactDuration(d time.Duration) string {
	d = d.Round(time.Second)

	switch {
	case d < 0:
		return "0s"
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d/time.Second))
	case d < time.Hour:
		return fmt.Sprintf("%dm%ds", int(d/time.Minute), int(d%time.Minute/time.Second))
	case d < 100*time.Hour:
		return fmt.Sprintf("%dh%dm", int(d/time.Hour), int(d%time.Hour/time.Minute))
	default:
		return ">99h"
	}
}

// marquee renders an indeterminate bar of width cells: a block that ping-pongs
// across the track, its position derived from tick so a fresh model (tick zero)
// renders it deterministically at the left edge. The block carries the same
// blend as the determinate bar's fill, so the two read as one component.
func marquee(width, tick int) string {
	if width <= 0 {
		return ""
	}

	block := min(marqueeBlock, width)

	start := 0
	if span := width - block; span > 0 {
		phase := tick % (2 * span)
		if phase <= span {
			start = phase
		} else {
			start = 2*span - phase
		}
	}

	var b strings.Builder

	writeRun(&b, styleBarTrack, barEmptyChar, start)

	for _, c := range lipgloss.Blend1D(block, barBlendStart, barBlendEnd) {
		b.WriteString(lipgloss.NewStyle().Foreground(c).Render(barFullChar))
	}

	writeRun(&b, styleBarTrack, barEmptyChar, width-start-block)

	return b.String()
}

// writeRun writes n copies of char styled by style, or nothing when n is zero so
// no empty escape sequence is emitted.
func writeRun(b *strings.Builder, style lipgloss.Style, char string, n int) {
	if n <= 0 {
		return
	}

	b.WriteString(style.Render(strings.Repeat(char, n)))
}

// padRight returns s padded with spaces to width, or truncated to width. Phase
// names and eta values are ASCII, so byte length equals cell width.
func padRight(s string, width int) string {
	if len(s) >= width {
		return s[:width]
	}

	return s + strings.Repeat(" ", width-len(s))
}
