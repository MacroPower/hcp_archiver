package export

import (
	"fmt"
	"net/url"
	"strings"
)

// cellEscaper neutralizes user-controlled text for a markdown table cell or
// heading in a single pass: backslashes, pipes, backticks, and square
// brackets are escaped so content cannot alter table, code, or link
// structure, angle brackets and ampersands become entities so inline HTML
// never passes through to a generator that renders it, and line breaks fold
// to <br> so a multi-line value stays one row. The single pass matters:
// replacements are never rescanned, so the inserted <br> survives the
// angle-bracket escaping.
var cellEscaper = strings.NewReplacer(
	`\`, `\\`,
	`|`, `\|`,
	"`", "\\`",
	`[`, `\[`,
	`]`, `\]`,
	"&", "&amp;",
	"<", "&lt;",
	">", "&gt;",
	"\r\n", "<br>",
	"\r", "<br>",
	"\n", "<br>",
)

// escapeCell neutralizes user-controlled text for inline markdown.
func escapeCell(s string) string {
	return cellEscaper.Replace(s)
}

// mdLink renders a markdown link whose text is escaped and whose target path
// segments are URL-escaped, so a display name cannot break out of the link
// and a path segment with reserved characters still resolves.
func mdLink(text string, segments ...string) string {
	escaped := make([]string, 0, len(segments))
	for _, seg := range segments {
		escaped = append(escaped, url.PathEscape(seg))
	}

	return fmt.Sprintf("[%s](%s)", escapeCell(text), strings.Join(escaped, "/"))
}

// page accumulates one markdown file's content.
type page struct {
	b strings.Builder
}

// h1 writes the page title.
func (p *page) h1(title string) {
	fmt.Fprintf(&p.b, "# %s\n", escapeCell(title))
}

// h2 writes a section heading.
func (p *page) h2(title string) {
	fmt.Fprintf(&p.b, "\n## %s\n", escapeCell(title))
}

// para writes one paragraph of already-safe markdown: link and count text the
// renderers compose, never raw archive content.
func (p *page) para(text string) {
	fmt.Fprintf(&p.b, "\n%s\n", text)
}

// kv writes a bulleted definition list, skipping rows whose value is empty so
// an absent attribute leaves no dangling label. Values must already be
// escaped (or renderer-composed).
func (p *page) kv(rows [][2]string) {
	wrote := false

	for _, row := range rows {
		if row[1] == "" {
			continue
		}

		if !wrote {
			p.b.WriteString("\n")

			wrote = true
		}

		fmt.Fprintf(&p.b, "- **%s**: %s\n", row[0], row[1])
	}
}

// table writes a markdown table. Cells must already be escaped (or
// renderer-composed, like links). A call with no rows writes nothing, so an
// empty collection leaves no headers-only stub.
func (p *page) table(headers []string, rows [][]string) {
	if len(rows) == 0 {
		return
	}

	p.b.WriteString("\n| " + strings.Join(headers, " | ") + " |\n")
	p.b.WriteString("|" + strings.Repeat(" --- |", len(headers)) + "\n")

	for _, row := range rows {
		p.b.WriteString("| " + strings.Join(row, " | ") + " |\n")
	}
}

// bytes returns the accumulated page content.
func (p *page) bytes() []byte {
	return []byte(p.b.String())
}
