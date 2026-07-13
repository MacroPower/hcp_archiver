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

	marqueeBlock = 5 // moving block width within the indeterminate track.

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

// tuiModel is the Bubble Tea model backing the human view on a terminal.
//
// It owns no counters of its own: each render pulls a fresh [snapshot] through
// take, so the panel always mirrors the live ledger. The spinner is both the
// liveness indicator and the repaint clock. Its tick drives re-renders,
// advances the indeterminate marquee, and records a throughput sample, so the
// bar, counts, and rate move without any separate ticker. Create instances with
// [newTUIModel].
type tuiModel struct {
	bar       progressbar.Model
	spin      spinner.Model
	take      func() snapshot
	interrupt func()
	samples   []rateSample
	snap      snapshot
	width     int
	height    int
	tick      int
	// The high-water line count of the task region (task rows plus the overflow
	// line), advanced on each tick. The region is padded up to this mark so the
	// panel holds its height as work items finish rather than shrinking, a change
	// that would force the inline renderer to erase and resize mid-run.
	taskRegionHigh int
	quitting       bool
	sampled        bool
}

// newTUIModel creates a new [tuiModel] that renders snapshots from take and, on
// a quit key, invokes interrupt before quitting. Either function may be nil in
// tests that only exercise rendering. It returns a pointer so Bubble Tea passes
// the heavy model around by reference.
func newTUIModel(take func() snapshot, interrupt func()) *tuiModel {
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
	}
}

// Init starts the spinner, whose ticks drive every subsequent repaint.
func (m *tuiModel) Init() tea.Cmd {
	return m.spin.Tick
}

// Update advances the spinner, marquee, and throughput window on each tick,
// records the terminal size, and quits on a [quitRequestMsg] or, running the
// interrupt callback first, on ctrl+c or q (raw mode suppresses the kernel's
// SIGINT, so the quit keys are handled here). Every quit path marks the model
// quitting so the final render erases the panel.
func (m *tuiModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case quitRequestMsg:
		m.quitting = true

		return m, tea.Quit

	case tea.KeyPressMsg:
		switch msg.String() {
		case "ctrl+c", "q":
			if m.interrupt != nil {
				m.interrupt()
			}

			m.quitting = true

			return m, tea.Quit
		}

	case tea.WindowSizeMsg:
		// Record the size to clip each line and bound the task list, and shrink
		// the bar so the panel never wraps onto an extra line. A fresh model (as
		// the golden tests use) keeps the full barWidth and does not clip.
		m.width = msg.Width
		m.height = msg.Height

		if msg.Width > 0 {
			m.bar.SetWidth(min(barWidth, msg.Width))
		}

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

			// Advance the task region's high-water mark so render can hold the
			// panel's height as work items finish. Growth-only here; render caps
			// it to what the terminal fits.
			m.taskRegionHigh = max(m.taskRegionHigh, taskRegionLines(len(m.snap.tasks), m.taskLineBudget()))
		}

		m.spin, cmd = m.spin.Update(msg)

		return m, cmd
	}

	return m, nil
}

// View renders the current snapshot into an inline (non-alt-screen) view, so log
// lines printed by the sink stay in scrollback above the pinned panel. A
// quitting model renders nothing, so the program's final render erases the
// panel rather than stranding its last frame in the output flow.
func (m *tuiModel) View() tea.View {
	if m.quitting {
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

// observe appends one throughput sample and trims the window's tail, keeping at
// least two samples so a rate can always be derived once ticks have flowed. It
// samples the snapshot's wire-byte figure (the shared wire counter when the
// reporter has one, committed bytes otherwise), so the rate reads live while a
// large object is still streaming and decays to zero only on true silence.
func (m *tuiModel) observe(snap snapshot) {
	m.samples = append(m.samples, rateSample{at: snap.elapsed, bytes: snap.wireBytes})

	for len(m.samples) > 2 && m.samples[len(m.samples)-1].at-m.samples[0].at > rateWindow {
		m.samples = m.samples[1:]
	}
}

// throughput returns the download rate over the sampled window, in bytes per
// second. Before two distinct samples exist (a fresh model, or a test render
// with no ticks) it falls back to the snapshot's lifetime average.
func (m *tuiModel) throughput(snap snapshot) float64 {
	if len(m.samples) < 2 {
		return snap.rate
	}

	first, last := m.samples[0], m.samples[len(m.samples)-1]

	span := (last.at - first.at).Seconds()
	if span <= 0 {
		return snap.rate
	}

	return float64(last.bytes-first.bytes) / span
}

// render formats a snapshot as the panel. Line one is the combined view: the
// spinner, a fixed-width phase, the bar or its indeterminate marquee, the
// percent, the eta, and the target. One line per in-flight work item follows,
// each with its own bar, percent, unit fraction, and name, in registration
// order so rows hold still as their bars move; items past the cap are counted
// on an overflow line. This region holds at its high-water line count, padded
// with blank lines, so the panel does not shrink as work items finish. The last
// line carries the colored per-status counts and the byte, rate, and elapsed
// metadata. Every field before a line's trailing name or metadata holds a
// constant width, so the panel does not reflow as values change.
func (m *tuiModel) render(snap snapshot) string {
	var line1 strings.Builder

	line1.WriteString(m.spin.View())
	line1.WriteString(stylePhase.Render(padRight(snap.phase, phaseWidth)))
	line1.WriteByte(' ')

	if snap.hasBar() {
		// Clamp to [0,1] so a miscounted completed/total can never widen the
		// percent past its 4-cell reserve (or read negative), keeping the bar and
		// its label in lockstep. Truncating toward zero floors the percent, so it
		// never reads 100% before the last unit lands.
		percent := min(max(float64(snap.completed)/float64(snap.total), 0), 1)
		line1.WriteString(m.bar.ViewAs(percent))
		fmt.Fprintf(&line1, " %s", styleCount.Render(fmt.Sprintf("%3d%%", int(percent*100))))
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
		glyphBytes, metaBytesWidth, humanBytes(t.BytesDownloaded),
		glyphRate, metaRateWidth, humanBytes(int64(m.throughput(snap)))+"/s",
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

	budget := m.taskLineBudget()

	// Capacity: line one, up to budget task rows, the overflow slot, and the two
	// footer lines.
	lines := make([]string, 0, budget+4)
	lines = append(lines, m.fit(line1.String()))

	visible := min(len(snap.tasks), budget)
	for _, task := range snap.tasks[:visible] {
		lines = append(lines, m.fit(m.renderTask(task)))
	}

	if hidden := len(snap.tasks) - visible; hidden > 0 {
		lines = append(lines, m.fit("  "+styleMeta.Render(fmt.Sprintf("… +%d more active", hidden))))
	}

	// Pad the task region up to its high-water line count so the panel holds its
	// height as work items finish rather than shrinking. A shrinking inline frame
	// forces the renderer to erase and resize, which corrupts the panel when a
	// log line is inserted above it at the same moment. The mark is capped here
	// at what the terminal fits (budget task rows plus the overflow slot), so a
	// shorter terminal pulls it back down.
	for target := min(m.taskRegionHigh, budget+1); len(lines)-1 < target; {
		lines = append(lines, "")
	}

	lines = append(lines, m.fit(counts), m.fit(metaLine))

	return strings.Join(lines, "\n")
}

// taskLineBudget bounds how many task lines the panel may show: the fixed cap,
// tightened on a short terminal so the panel (its first line, the task lines,
// a possible overflow line, the counts line, and the metadata line) never
// outgrows the screen. An unknown height (before the first size message, and
// in the golden tests) keeps the fixed cap.
func (m *tuiModel) taskLineBudget() int {
	if m.height <= 0 {
		return maxTaskLines
	}

	return min(maxTaskLines, max(m.height-4, 0))
}

// taskRegionLines is the natural line count of the task region for a pool of the
// given size under budget: one line per shown item, plus an overflow line once
// the pool outruns the budget. The high-water mark advances to this count.
func taskRegionLines(tasks, budget int) int {
	visible := min(tasks, budget)
	if tasks > visible {
		return visible + 1
	}

	return visible
}

// renderTask formats one in-flight work item's line: its bar aligned under the
// phase bar, its percent, its unit fraction in the eta column, and its name in
// the target position, so the panel's lines read as one grid. The items shown
// are independent of line one's target (the target is wherever a worker last
// started), so each line names its own item.
func (m *tuiModel) renderTask(task taskProgress) string {
	var b strings.Builder

	b.WriteString(strings.Repeat(" ", phaseWidth+3))

	// Guard the division: a task with no total (the same total==0 case the phase
	// bar skips via hasBar) would otherwise divide by zero into a NaN or an
	// infinity and render a garbage percent.
	var percent float64

	if task.total > 0 {
		percent = min(max(float64(task.done)/float64(task.total), 0), 1)
	}

	b.WriteString(m.bar.ViewAs(percent))
	fmt.Fprintf(&b, " %s", styleCount.Render(fmt.Sprintf("%3d%%", int(percent*100))))

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
		styleDone.Render(fmt.Sprintf(glyphDone+" done %-*d", doneWidth, t.Done)),
		styleErrored.Render(fmt.Sprintf(glyphErrored+" errored %-*d", erroredWidth, t.Errored)),
		styleForbidden.Render(fmt.Sprintf(glyphForbidden+" forbidden %-*d", forbiddenWidth, t.Forbidden)),
		styleRetried.Render(fmt.Sprintf(glyphRetried+" retried %-*d", retriedWidth, t.Retried)),
	)
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
