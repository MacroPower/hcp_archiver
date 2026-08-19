package cli_test

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.jacobcolvin.com/hcp_archiver/internal/cli"
	"go.jacobcolvin.com/hcp_archiver/pkg/config"
	"go.jacobcolvin.com/hcp_archiver/pkg/export"
	"go.jacobcolvin.com/hcp_archiver/pkg/progress"
)

func TestExportCmd(t *testing.T) {
	t.Parallel()

	root := buildMiniArchive(t)
	target := filepath.Join(t.TempDir(), "site")

	out, _, err := runCmd(t, "export", root, "--export-path", target)
	require.NoError(t, err)
	assert.Contains(t, out, "exported")
	assert.Contains(t, out, target)

	page, err := os.ReadFile(filepath.Join(target, "mini-org", filepath.FromSlash(miniWs), "index.md"))
	require.NoError(t, err)

	// The bundled artifact lists by name; its content never renders.
	assert.Contains(t, string(page), "plan.log")
	assert.NotContains(t, string(page), miniPlanContent)
}

func TestExportCmd_RequiresTarget(t *testing.T) {
	t.Parallel()

	root := buildMiniArchive(t)

	_, _, err := runCmd(t, "export", root)
	require.ErrorIs(t, err, export.ErrNoTarget)
}

func TestExportCmd_RefusesTargetInsideArchive(t *testing.T) {
	t.Parallel()

	root := buildMiniArchive(t)

	_, _, err := runCmd(t, "export", root, "--export-path", filepath.Join(root, "site"))
	require.ErrorIs(t, err, cli.ErrTargetInArchive)
}

func TestExportCmd_TargetFromConfig(t *testing.T) {
	t.Parallel()

	root := buildMiniArchive(t)
	target := filepath.Join(t.TempDir(), "site")
	cfgPath := writeConfigFile(t, "export:\n  path: '"+target+"'\n")

	out, _, err := runCmd(t, "export", root, "--config", cfgPath)
	require.NoError(t, err)
	assert.Contains(t, out, "exported")
	assert.Contains(t, out, target)

	_, err = os.Stat(filepath.Join(target, "mini-org", "index.md"))
	require.NoError(t, err)
}

func TestExportCmd_TemplatesFromConfig(t *testing.T) {
	t.Parallel()

	root := buildMiniArchive(t)
	target := filepath.Join(t.TempDir(), "site")

	// The templates directory sits beside the configuration file and is named
	// relatively, resolving against the file's directory.
	cfgDir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(cfgDir, "export-templates"), 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(cfgDir, "export-templates", "workspace.md.tmpl"),
		[]byte("# {{escape .Title}} (custom)"), 0o600))

	cfgPath := filepath.Join(cfgDir, "config.yaml")
	require.NoError(t, os.WriteFile(cfgPath, []byte("export:\n  templates:\n    path: export-templates\n"), 0o600))

	_, _, err := runCmd(t, "export", root, "--export-path", target, "--config", cfgPath)
	require.NoError(t, err)

	page, err := os.ReadFile(filepath.Join(target, "mini-org", filepath.FromSlash(miniWs), "index.md"))
	require.NoError(t, err)
	assert.Equal(t, "# w1 (custom)", string(page))
}

func TestExportCmd_BadTemplatesPreserveForcedTarget(t *testing.T) {
	t.Parallel()

	root := buildMiniArchive(t)

	target := t.TempDir()
	stale := filepath.Join(target, "stale.md")
	require.NoError(t, os.WriteFile(stale, []byte("previous site"), 0o600))

	cfgDir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(cfgDir, "tmpl"), 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(cfgDir, "tmpl", "workspace.md.tmpl"), []byte("{{escape .Title"), 0o600))

	cfgPath := filepath.Join(cfgDir, "config.yaml")
	require.NoError(t, os.WriteFile(cfgPath, []byte("export:\n  templates:\n    path: tmpl\n"), 0o600))

	_, _, err := runCmd(t, "export", root, "--export-path", target, "--force", "--config", cfgPath)
	require.ErrorIs(t, err, export.ErrTemplateInvalid)

	assert.FileExists(t, stale, "a template refusal must precede the forced clear")
}

func TestExportCmd_NonEmptyTargetHintsForce(t *testing.T) {
	t.Parallel()

	root := buildMiniArchive(t)
	target := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(target, "stale.md"), []byte("old"), 0o600))

	_, _, err := runCmd(t, "export", root, "--export-path", target)
	require.ErrorIs(t, err, export.ErrTargetNotEmpty)
	require.ErrorContains(t, err, "--force")

	_, _, err = runCmd(t, "export", root, "--export-path", target, "--force")
	require.NoError(t, err)
	assert.NoFileExists(t, filepath.Join(target, "stale.md"))
}

func TestExportCmd_RejectsBadProgressMode(t *testing.T) {
	t.Parallel()

	root := buildMiniArchive(t)
	target := filepath.Join(t.TempDir(), "site")

	_, _, err := runCmd(t, "export", root, "--export-path", target, "--progress", "bogus")
	require.ErrorIs(t, err, config.ErrInvalidProgressMode)
}

func TestExportCmd_ProgressAdapter(t *testing.T) {
	t.Parallel()

	buf := &bytes.Buffer{}
	base := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)
	adapter, reporter := cli.ExportProgressForTest(buf, config.ProgressModeHuman,
		progress.WithClock(func() time.Time { return base }))

	// The command's RunE owns the phase; the adapter carries only the
	// target, total, and advances.
	reporter.SetPhase("export")

	adapter.SetTarget("my-org")
	adapter.SetTotal(5)
	adapter.Advance(2)

	require.NoError(t, reporter.Report())

	line := buf.String()
	assert.Contains(t, line, "phase=export")
	assert.Contains(t, line, "target=my-org")
	assert.Contains(t, line, "done=2")
	assert.Contains(t, line, "completed=2/5")

	// A new organization's total resets both counters, so done= and
	// completed= keep reading per-organization.
	buf.Reset()
	adapter.SetTarget("next-org")
	adapter.SetTotal(7)
	adapter.Advance(3)

	require.NoError(t, reporter.Report())

	line = buf.String()
	assert.Contains(t, line, "target=next-org")
	assert.Contains(t, line, "done=3")
	assert.Contains(t, line, "completed=3/7")
}
