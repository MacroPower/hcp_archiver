package main_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	main "go.jacobcolvin.com/hcp_archiver/cmd/hcp_archiver"
	"go.jacobcolvin.com/hcp_archiver/export"
)

func TestExportCmd(t *testing.T) {
	t.Parallel()

	root := buildMiniArchive(t)
	target := filepath.Join(t.TempDir(), "site")

	out, _, err := runCmd(t, "export", root, "--target", target)
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

	_, _, err := runCmd(t, "export", root, "--target", filepath.Join(root, "site"))
	require.ErrorIs(t, err, main.ErrTargetInArchive)
}

func TestExportCmd_NonEmptyTargetHintsForce(t *testing.T) {
	t.Parallel()

	root := buildMiniArchive(t)
	target := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(target, "stale.md"), []byte("old"), 0o600))

	_, _, err := runCmd(t, "export", root, "--target", target)
	require.ErrorIs(t, err, export.ErrTargetNotEmpty)
	require.ErrorContains(t, err, "--force")

	_, _, err = runCmd(t, "export", root, "--target", target, "--force")
	require.NoError(t, err)
	assert.NoFileExists(t, filepath.Join(target, "stale.md"))
}
