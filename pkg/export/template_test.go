package export_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.jacobcolvin.com/hcp_archiver/pkg/export"
)

// wsPage is the fixture workspace's page in the export tree.
const wsPage = "my-org/projects/default/workspaces/app/index.md"

// writeTemplates lays out a template override directory holding the named
// template contents and returns its path.
func writeTemplates(t *testing.T, templates map[string]string) string {
	t.Helper()

	dir := t.TempDir()
	for name, content := range templates {
		writeFile(t, dir, name, content)
	}

	return dir
}

// readDirNames returns the names of the top-level entries fsys holds.
func readDirNames(t *testing.T, fsys fs.FS) []string {
	t.Helper()

	entries, err := fs.ReadDir(fsys, ".")
	require.NoError(t, err)

	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}

	return names
}

// runExportWithTemplates exports the fixture archive with an override
// directory and returns the target tree.
func runExportWithTemplates(t *testing.T, templates map[string]string) map[string]string {
	t.Helper()

	root := buildArchive(t)
	target := filepath.Join(t.TempDir(), "site")

	_, err := export.New(openFixture(t, root), target,
		export.WithTemplatesDir(writeTemplates(t, templates))).Run(t.Context())
	require.NoError(t, err)

	return readTree(t, target)
}

func TestExportTemplateOverride(t *testing.T) {
	t.Parallel()

	defaults := func() map[string]string {
		target, _ := runExport(t)

		return readTree(t, target)
	}()

	files := runExportWithTemplates(t, map[string]string{
		"workspace.md.tmpl": `# {{escape .Title}} (custom)`,
	})

	assert.Equal(t, "# app (custom)", files[wsPage],
		"the overridden page renders through the supplied template")

	for rel, content := range defaults {
		if rel == wsPage {
			continue
		}

		assert.Equal(t, content, files[rel], "%s must keep its default rendering", rel)
	}
}

func TestExportTemplateFuncsAvailable(t *testing.T) {
	t.Parallel()

	files := runExportWithTemplates(t, map[string]string{
		"teams.md.tmpl": `{{countNoun (len .Teams) "team" "teams"}}: ` +
			`{{range .Teams}}{{link .Name "teams" .ID}} {{escape .Name}}{{end}}`,
	})

	assert.Equal(t, "1 team: [owners](teams/team-1) owners", files["my-org/teams/index.md"])
}

// TestExportTemplateOverrideCanBlankAPage pins that an override suppressing a
// page wins over the default it replaces. A template set keeps the definition
// it already holds when a replacement parses to an empty tree, and counts a
// body of comments or whitespace as empty, so a blank override is exactly the
// case that silently renders the default instead.
func TestExportTemplateOverrideCanBlankAPage(t *testing.T) {
	t.Parallel()

	tests := map[string]string{
		"a comment-only override":    `{{/* workspaces intentionally omitted */}}`,
		"a whitespace-only override": "  \n\t\n",
		"an empty override":          "",
	}

	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			files := runExportWithTemplates(t, map[string]string{"workspace.md.tmpl": body})

			assert.Empty(t, strings.TrimSpace(files[wsPage]),
				"the page renders through the override, not the default")
		})
	}
}

// TestExportTemplateOverridesEveryPage covers the boundary where an override
// replaces every page and no embedded default is left to parse.
func TestExportTemplateOverridesEveryPage(t *testing.T) {
	t.Parallel()

	templates := map[string]string{}
	for _, name := range readDirNames(t, export.DefaultTemplates()) {
		templates[name] = "# every page overridden"
	}

	files := runExportWithTemplates(t, templates)

	assert.Equal(t, "# every page overridden", files[wsPage],
		"the export loads with no default left to parse")
}

func TestExportTemplateRefusals(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		templates map[string]string
		err       error
	}{
		"an override that does not parse is refused": {
			templates: map[string]string{"workspace.md.tmpl": `{{escape .Title`},
			err:       export.ErrTemplateInvalid,
		},
		"an unrecognized template name is refused": {
			templates: map[string]string{"workspac.md.tmpl": `# typo`},
			err:       export.ErrUnknownTemplate,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			root := buildArchive(t)

			// A pre-existing forced target: a refused template load must
			// leave its contents untouched, never clearing first.
			target := t.TempDir()
			stale := writeFile(t, target, "stale.md", "previous site")

			_, err := export.New(openFixture(t, root), target,
				export.WithForce(), export.WithTemplatesDir(writeTemplates(t, tc.templates))).
				Run(t.Context())
			require.ErrorIs(t, err, tc.err)

			assert.FileExists(t, stale, "a template refusal must precede the forced clear")
		})
	}
}

func TestExportTemplatesDirUnreadable(t *testing.T) {
	t.Parallel()

	root := buildArchive(t)
	target := filepath.Join(t.TempDir(), "site")

	_, err := export.New(openFixture(t, root), target,
		export.WithTemplatesDir(filepath.Join(t.TempDir(), "absent"))).Run(t.Context())
	require.ErrorIs(t, err, export.ErrTemplatesUnreadable)
}

func TestExportTemplatesDirIgnoresOtherFiles(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeFile(t, dir, "README.md", "about these templates")
	writeFile(t, dir, "notes.txt", "scratch")
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "partials"), 0o755))

	root := buildArchive(t)
	target := filepath.Join(t.TempDir(), "site")

	_, err := export.New(openFixture(t, root), target,
		export.WithTemplatesDir(dir)).Run(t.Context())
	require.NoError(t, err)

	defaultTarget, _ := runExport(t)
	assert.Equal(t, readTree(t, defaultTarget), readTree(t, target),
		"a directory holding no *.md.tmpl files leaves every default in place")
}

// TestExportHostileTemplateCannotLeakSecrets pins the redaction boundary: the
// page data is built with sensitive values already replaced, so even a
// template that dumps its entire context verbatim finds nothing to leak.
func TestExportHostileTemplateCannotLeakSecrets(t *testing.T) {
	t.Parallel()

	files := runExportWithTemplates(t, map[string]string{
		"workspace.md.tmpl":     `{{printf "%#v" .}}`,
		"variable-sets.md.tmpl": `{{printf "%#v" .}}`,
		"policy-sets.md.tmpl":   `{{printf "%#v" .}}`,
	})

	for rel, content := range files {
		assert.NotContains(t, content, secretMarker,
			"%s must not carry a sensitive fixture value under any template", rel)
	}

	assert.Contains(t, files[wsPage], "(sensitive)",
		"the dumped context carries the redaction marker, not the value")
}

func TestExportTemplateExecutionError(t *testing.T) {
	t.Parallel()

	root := buildArchive(t)
	target := filepath.Join(t.TempDir(), "site")

	_, err := export.New(openFixture(t, root), target,
		export.WithTemplatesDir(writeTemplates(t, map[string]string{
			"workspace.md.tmpl": `{{.NoSuchField}}`,
		}))).Run(t.Context())

	require.Error(t, err)
	assert.ErrorContains(t, err, "workspace.md.tmpl",
		"an execution error names the template that raised it")
}
