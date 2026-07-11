package config_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.jacobcolvin.com/niceyaml"

	"go.jacobcolvin.com/hcp_archiver/config"
)

func TestDefaultFile(t *testing.T) {
	t.Parallel()

	file := config.DefaultFile()

	assert.Equal(t, config.DefaultAddress, file.Address)
	assert.Equal(t, config.DefaultMaxConcurrency, file.MaxConcurrency)
	assert.Empty(t, file.Organizations)
	assert.Equal(t, config.FileScope{}, file.Scope)
}

func TestLoadFile(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		want func(*testing.T, *config.File)
		yaml string
	}{
		"empty document keeps defaults": {
			yaml: "",
			want: func(t *testing.T, file *config.File) {
				t.Helper()
				assert.Equal(t, config.DefaultAddress, file.Address)
				assert.Empty(t, file.Organizations)
			},
		},
		"comment-only document keeps defaults": {
			yaml: "# yaml-language-server: $schema=./config.schema.json\n",
			want: func(t *testing.T, file *config.File) {
				t.Helper()
				assert.Equal(t, config.DefaultAddress, file.Address)
				assert.Empty(t, file.Organizations)
			},
		},
		"explicit null document keeps defaults": {
			yaml: "null\n",
			want: func(t *testing.T, file *config.File) {
				t.Helper()
				assert.Equal(t, config.DefaultAddress, file.Address)
				assert.Empty(t, file.Organizations)
			},
		},
		"content alongside a comment document is not a second document": {
			yaml: "organizations:\n  - acme\n---\n# trailing comment\n",
			want: func(t *testing.T, file *config.File) {
				t.Helper()
				assert.Equal(t, []string{"acme"}, file.Organizations)
			},
		},
		"partial document defaults the rest": {
			yaml: "organizations:\n  - acme\n",
			want: func(t *testing.T, file *config.File) {
				t.Helper()
				assert.Equal(t, []string{"acme"}, file.Organizations)
				assert.Equal(t, config.DefaultAddress, file.Address)
			},
		},
		"full document overrides every default": {
			yaml: "# yaml-language-server: $schema=./config.schema.json\n" +
				"address: https://tfe.example.com\n" +
				"organizations:\n  - one\n  - two\n" +
				"maxConcurrency: 32\n" +
				"scope:\n  stacks: true\n  hyok: true\n  registryDetail: true\n  auditTrail: true\n",
			want: func(t *testing.T, file *config.File) {
				t.Helper()
				assert.Equal(t, "https://tfe.example.com", file.Address)
				assert.Equal(t, []string{"one", "two"}, file.Organizations)
				assert.Equal(t, 32, file.MaxConcurrency)
				assert.True(t, file.Scope.Stacks)
				assert.True(t, file.Scope.HYOK)
				assert.True(t, file.Scope.RegistryDetail)
				assert.True(t, file.Scope.AuditTrail)
			},
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			path := writeConfig(t, tc.yaml)

			file, err := config.LoadFile(path)
			require.NoError(t, err)
			require.NotNil(t, file)
			tc.want(t, file)
		})
	}
}

func TestLoadFile_SourceAnnotatedErrors(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		yaml string
	}{
		"concurrency below the schema minimum": {
			yaml: "concurrency: 0\n",
		},
		"unknown property": {
			yaml: "notaproperty: true\n",
		},
		"empty organization name": {
			yaml: "organizations:\n  - \"\"\n",
		},
		"duplicate organization": {
			yaml: "organizations:\n  - acme\n  - acme\n",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			path := writeConfig(t, tc.yaml)

			file, err := config.LoadFile(path)
			require.Error(t, err)
			assert.Nil(t, file)

			var nerr *niceyaml.Error

			require.ErrorAs(t, err, &nerr, "error carries source annotations")
		})
	}
}

func TestLoadFile_Example(t *testing.T) {
	t.Parallel()

	// The example shipped at the repository root must stay valid against the
	// schema so a copy of it is a working starting point.
	file, err := config.LoadFile(filepath.Join("..", "hcp_archiver.example.yaml"))
	require.NoError(t, err)
	assert.Equal(t, config.DefaultAddress, file.Address)
	assert.Equal(t, []string{"my-org"}, file.Organizations)
}

func TestLoadFile_ReadError(t *testing.T) {
	t.Parallel()

	_, err := config.LoadFile(filepath.Join(t.TempDir(), "does-not-exist.yaml"))
	require.ErrorIs(t, err, config.ErrReadConfig)
}

func TestLoadFile_MultipleDocuments(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, "organizations:\n  - one\n---\norganizations:\n  - two\n")

	_, err := config.LoadFile(path)
	require.ErrorIs(t, err, config.ErrMultipleDocuments)
}

// writeConfig writes yaml to a temporary file and returns its path.
func writeConfig(t *testing.T, yaml string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(yaml), 0o600))

	return path
}
