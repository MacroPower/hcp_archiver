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
