package view

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestYAMLViewerScreen(t *testing.T) {
	t.Parallel()

	s := newYAMLViewerScreen("meta.json", `{"serial": 7, "status": "finalized"}`)
	s.setSize(60, 10)

	assert.Equal(t, "meta.json", s.crumb())

	// The document's tokens survive lexing and render into the body; the exact
	// styling belongs to niceyaml, so only the content is asserted.
	body := s.view()
	assert.Contains(t, body, "serial")
	assert.Contains(t, body, "finalized")
	assert.Contains(t, body, "esc back")
}

func TestFileViewerRouting(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		relPath string
		content string
		json    bool
	}{
		"json document":    {relPath: "org.json", content: `{"data": []}`, json: true},
		"state blob":       {relPath: "sv-1.tfstate.json", content: `{"serial": 1}`, json: true},
		"metadata sidecar": {relPath: "sv-1.meta.json", content: `{"id": "sv-1"}`, json: true},
		"plain log":        {relPath: "plan.log", content: "Terraform will perform...", json: false},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(dir, tc.relPath), []byte(tc.content), 0o600))

			org := &Org{Name: "test", root: dir}

			s, err := newFileViewer(org, tc.relPath, nil)
			require.NoError(t, err)

			if tc.json {
				assert.IsType(t, &yamlViewerScreen{}, s)
			} else {
				assert.IsType(t, &viewerScreen{}, s)
			}
		})
	}
}
