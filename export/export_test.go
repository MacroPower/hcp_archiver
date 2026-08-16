package export

var (
	// EscapeCell exposes escapeCell so the external test package can exercise
	// the markdown neutralization directly.
	EscapeCell = escapeCell

	// CountEntries exposes countEntries so the external test package can
	// exercise the inventory counting directly.
	CountEntries = countEntries

	// ShellQuote exposes shellQuote so the external test package can exercise
	// the snippet quoting directly.
	ShellQuote = shellQuote
)
