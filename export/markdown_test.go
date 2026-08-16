package export_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"go.jacobcolvin.com/hcp_archiver/export"
)

func TestEscapeCell(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		in   string
		want string
	}{
		"plain text passes through": {
			in:   "us-east-1",
			want: "us-east-1",
		},
		"pipes cannot break the table": {
			in:   "a|b",
			want: `a\|b`,
		},
		"backslashes are escaped before they can escape anything": {
			in:   `a\|b`,
			want: `a\\\|b`,
		},
		"newlines fold to br and stay one row": {
			in:   "line1\nline2",
			want: "line1<br>line2",
		},
		"crlf folds to one br": {
			in:   "line1\r\nline2",
			want: "line1<br>line2",
		},
		"inline html is neutralized": {
			in:   `<script>alert("x")</script>`,
			want: `&lt;script&gt;alert("x")&lt;/script&gt;`,
		},
		"ampersands become entities": {
			in:   "a&b",
			want: "a&amp;b",
		},
		"backticks cannot open code spans": {
			in:   "`code`",
			want: "\\`code\\`",
		},
		"square brackets cannot forge links": {
			in:   "a](http://evil)",
			want: `a\](http://evil)`,
		},
		"the inserted br survives the angle-bracket escaping": {
			in:   "<\n>",
			want: "&lt;<br>&gt;",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tc.want, export.EscapeCell(tc.in))
		})
	}
}

func TestShellQuote(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		in   string
		want string
	}{
		"plain path stays one quoted word": {
			in:   "acme/projects/default",
			want: "'acme/projects/default'",
		},
		"embedded single quote closes and reopens the word": {
			in:   "o'brien/projects",
			want: `'o'\''brien/projects'`,
		},
		"spaces and glob characters stay literal": {
			in:   "my org/state *.json",
			want: "'my org/state *.json'",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tc.want, export.ShellQuote(tc.in))
		})
	}
}
