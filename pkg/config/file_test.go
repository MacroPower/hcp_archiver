package config_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.jacobcolvin.com/niceyaml"

	"go.jacobcolvin.com/hcp_archiver/pkg/config"
)

// archiveYAML is the required archive section, prepended to documents whose
// case is about something else.
const archiveYAML = "archive:\n  path: ./archive\n"

func TestDefaultFile(t *testing.T) {
	t.Parallel()

	file := config.DefaultFile()

	assert.Equal(t, config.DefaultAddress, file.Address)
	assert.Zero(t, file.RateLimit)
	assert.Empty(t, file.Organizations)
	assert.Empty(t, file.Projects)
	assert.Empty(t, file.Workspaces)
	assert.Equal(t, config.FileRunHistory{}, file.RunHistory)
	assert.Equal(t, config.FileInclude{}, file.Include)
}

func TestLoadFile(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		want func(*testing.T, *config.File)
		yaml string
	}{
		"minimal document keeps defaults": {
			yaml: archiveYAML,
			want: func(t *testing.T, file *config.File) {
				t.Helper()
				assert.Equal(t, config.DefaultAddress, file.Address)
				assert.Empty(t, file.Organizations)
				assert.Equal(t, "./archive", file.Archive.Path)
			},
		},
		"content alongside a comment document is not a second document": {
			yaml: archiveYAML + "organizations:\n  - acme\n---\n# trailing comment\n",
			want: func(t *testing.T, file *config.File) {
				t.Helper()
				assert.Equal(t, []string{"acme"}, file.Organizations)
			},
		},
		"partial document defaults the rest": {
			yaml: archiveYAML + "organizations:\n  - acme\n",
			want: func(t *testing.T, file *config.File) {
				t.Helper()
				assert.Equal(t, []string{"acme"}, file.Organizations)
				assert.Equal(t, config.DefaultAddress, file.Address)
			},
		},
		"full document overrides every default": {
			yaml: "# yaml-language-server: $schema=./config.schema.json\n" +
				archiveYAML +
				"address: https://tfe.example.com\n" +
				"rateLimit: 12.5\n" +
				"organizations:\n  - one\n  - two\n" +
				"projects:\n  - networking\n" +
				"workspaces:\n  - vpc\n  - dns\n" +
				"runHistory:\n  fetch:\n    count: 250\n    age: 90d\n" +
				"include:\n  stacks: true\n  hyok: true\n  registryDetail: true\n  auditTrail: true\n",
			want: func(t *testing.T, file *config.File) {
				t.Helper()
				assert.Equal(t, "https://tfe.example.com", file.Address)
				assert.InEpsilon(t, 12.5, file.RateLimit, 1e-9)
				assert.Equal(t, []string{"one", "two"}, file.Organizations)
				assert.Equal(t, []string{"networking"}, file.Projects)
				assert.Equal(t, []string{"vpc", "dns"}, file.Workspaces)
				assert.Equal(t, 250, file.RunHistory.Fetch.Count)
				assert.Equal(t, config.Duration(2160*time.Hour), file.RunHistory.Fetch.Age)
				assert.True(t, file.Include.Stacks)
				assert.True(t, file.Include.HYOK)
				assert.True(t, file.Include.RegistryDetail)
				assert.True(t, file.Include.AuditTrail)
			},
		},
		"run history bounds are each optional": {
			yaml: archiveYAML + "runHistory:\n  fetch:\n    count: 100\n",
			want: func(t *testing.T, file *config.File) {
				t.Helper()
				assert.Equal(t, 100, file.RunHistory.Fetch.Count)
				assert.Zero(t, file.RunHistory.Fetch.Age)
			},
		},
		"run history age zero means unbounded": {
			yaml: archiveYAML + "runHistory:\n  fetch:\n    count: 100\n    age: 0\n",
			want: func(t *testing.T, file *config.File) {
				t.Helper()
				assert.Equal(t, 100, file.RunHistory.Fetch.Count)
				assert.Zero(t, file.RunHistory.Fetch.Age)
			},
		},
		"run history age accepts a float zero": {
			yaml: archiveYAML + "runHistory:\n  fetch:\n    age: 0.0\n",
			want: func(t *testing.T, file *config.File) {
				t.Helper()
				assert.Zero(t, file.RunHistory.Fetch.Age)
			},
		},
		"bare section keys are unset": {
			yaml: archiveYAML + "export:\nextract:\ninclude:\nremote:\nrunHistory:\n",
			want: func(t *testing.T, file *config.File) {
				t.Helper()
				assert.Equal(t, config.DefaultAddress, file.Address)
				assert.Empty(t, file.Export.Templates.Path)
				assert.Empty(t, file.Extract.Path)
				assert.True(t, file.Remote.IsZero())
				assert.Equal(t, config.FileRunHistory{}, file.RunHistory)
				assert.Equal(t, config.FileInclude{}, file.Include)
			},
		},
		"remote section decodes": {
			yaml: archiveYAML + "remote:\n" +
				"  url: s3://my-archive?region=us-east-1\n" +
				"  prefix: hcp\n" +
				"  upload:\n" +
				"    partSize: 67108864\n" +
				"    concurrency: 4\n",
			want: func(t *testing.T, file *config.File) {
				t.Helper()
				assert.False(t, file.Remote.IsZero())
				assert.Equal(t, config.RemoteConfig{
					URL:         "s3://my-archive?region=us-east-1",
					Prefix:      "hcp",
					PartSize:    67108864,
					Concurrency: 4,
				}, file.Remote.RemoteConfig())
			},
		},
		"remote part size accepts a suffixed string": {
			yaml: archiveYAML + "remote:\n  url: s3://b\n  upload:\n    partSize: 64MiB\n",
			want: func(t *testing.T, file *config.File) {
				t.Helper()
				assert.Equal(t, int64(67108864), file.Remote.RemoteConfig().PartSize)
			},
		},
		"bare run history fetch section is unset": {
			yaml: archiveYAML + "runHistory:\n  fetch:\n",
			want: func(t *testing.T, file *config.File) {
				t.Helper()
				assert.Equal(t, config.FileRunHistory{}, file.RunHistory)
			},
		},
		"bare upload section is unset": {
			yaml: archiveYAML + "remote:\n  url: s3://b\n  upload:\n",
			want: func(t *testing.T, file *config.File) {
				t.Helper()
				assert.False(t, file.Remote.IsZero())
				assert.Zero(t, file.Remote.RemoteConfig().PartSize)
				assert.Zero(t, file.Remote.RemoteConfig().Concurrency)
			},
		},
		"archive and extract paths decode": {
			yaml: "archive:\n  path: ./archive\nextract:\n  path: /mnt/restore\n",
			want: func(t *testing.T, file *config.File) {
				t.Helper()
				assert.Equal(t, "./archive", file.Archive.Path)
				assert.Equal(t, "/mnt/restore", file.Extract.Path)
			},
		},
		"empty extract section stays empty": {
			yaml: archiveYAML + "extract: {}\n",
			want: func(t *testing.T, file *config.File) {
				t.Helper()
				assert.Empty(t, file.Extract.Path)
			},
		},
		"extract and export paths left unset stay empty": {
			yaml: archiveYAML + "organizations:\n  - acme\n",
			want: func(t *testing.T, file *config.File) {
				t.Helper()
				assert.Empty(t, file.Extract.Path)
				assert.Empty(t, file.Export.Path)
			},
		},
		"remote left unset disables offloading": {
			yaml: archiveYAML + "organizations:\n  - acme\n",
			want: func(t *testing.T, file *config.File) {
				t.Helper()
				assert.True(t, file.Remote.IsZero())
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

// TestLoadFile_MissingArchivePath covers the documents that never reach schema
// validation: nothing decodes, so the plain sentinel reports the absent
// archive path rather than a source-annotated error.
func TestLoadFile_MissingArchivePath(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		yaml string
	}{
		"empty document": {
			yaml: "",
		},
		"comment-only document": {
			yaml: "# yaml-language-server: $schema=./config.schema.json\n",
		},
		"explicit null document": {
			yaml: "null\n",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			path := writeConfig(t, tc.yaml)

			file, err := config.LoadFile(path)
			require.ErrorIs(t, err, config.ErrMissingArchivePath)
			assert.Nil(t, file)
		})
	}
}

func TestLoadFile_SourceAnnotatedErrors(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		yaml string
	}{
		"missing archive key": {
			yaml: "organizations:\n  - acme\n",
		},
		"bare archive key": {
			yaml: "archive:\n",
		},
		"empty archive section": {
			yaml: "archive: {}\n",
		},
		"empty archive path": {
			yaml: "archive:\n  path: \"\"\n",
		},
		"concurrency below the schema minimum": {
			yaml: archiveYAML + "remote:\n  url: s3://b\n  upload:\n    concurrency: 0\n",
		},
		"unknown property": {
			yaml: archiveYAML + "notaproperty: true\n",
		},
		"empty organization name": {
			yaml: archiveYAML + "organizations:\n  - \"\"\n",
		},
		"duplicate organization": {
			yaml: archiveYAML + "organizations:\n  - acme\n  - acme\n",
		},
		"empty project name": {
			yaml: archiveYAML + "projects:\n  - \"\"\n",
		},
		"duplicate project": {
			yaml: archiveYAML + "projects:\n  - networking\n  - networking\n",
		},
		"empty workspace name": {
			yaml: archiveYAML + "workspaces:\n  - \"\"\n",
		},
		"duplicate workspace": {
			yaml: archiveYAML + "workspaces:\n  - vpc\n  - vpc\n",
		},
		"negative run history count": {
			yaml: archiveYAML + "runHistory:\n  fetch:\n    count: -1\n",
		},
		"retired keepCount key is rejected": {
			yaml: archiveYAML + "runHistory:\n  keepCount: 100\n",
		},
		"retired keepAge key is rejected": {
			yaml: archiveYAML + "runHistory:\n  keepAge: 90d\n",
		},
		"retired flat fetchCount key is rejected": {
			yaml: archiveYAML + "runHistory:\n  fetchCount: 100\n",
		},
		"retired flat fetchAge key is rejected": {
			yaml: archiveYAML + "runHistory:\n  fetchAge: 90d\n",
		},
		"zero rate limit": {
			yaml: archiveYAML + "rateLimit: 0\n",
		},
		"negative rate limit": {
			yaml: archiveYAML + "rateLimit: -1\n",
		},
		"run history age must be a duration string": {
			yaml: archiveYAML + "runHistory:\n  fetch:\n    age: 90\n",
		},
		"run history age rejects a non-zero fraction": {
			yaml: archiveYAML + "runHistory:\n  fetch:\n    age: 0.5\n",
		},
		"address without a scheme": {
			yaml: archiveYAML + "address: app.terraform.io\n",
		},
		"address with a non-http scheme": {
			yaml: archiveYAML + "address: ftp://app.terraform.io\n",
		},
		"run history age must not be negative": {
			yaml: archiveYAML + "runHistory:\n  fetch:\n    age: -24h\n",
		},
		"run history age rejects unknown units": {
			yaml: archiveYAML + "runHistory:\n  fetch:\n    age: 1w\n",
		},
		"archive path must be a string": {
			yaml: "archive:\n  path:\n    - x\n",
		},
		"extract path must be a string": {
			yaml: archiveYAML + "extract:\n  path:\n    - x\n",
		},
		"retired flat archiveDir key is rejected": {
			yaml: archiveYAML + "archiveDir: ./archive\n",
		},
		"retired flat extractDir key is rejected": {
			yaml: archiveYAML + "extractDir: ./restore\n",
		},
		"remote without a bucket": {
			yaml: archiveYAML + "remote:\n  prefix: hcp\n",
		},
		"empty remote section": {
			yaml: archiveYAML + "remote: {}\n",
		},
		"empty remote url": {
			yaml: archiveYAML + "remote:\n  url: \"\"\n",
		},
		"negative remote part size": {
			yaml: archiveYAML + "remote:\n  url: s3://b\n  upload:\n    partSize: -1\n",
		},
		"lowercase part size suffix": {
			yaml: archiveYAML + "remote:\n  url: s3://b\n  upload:\n    partSize: 64mib\n",
		},
		"unknown part size suffix": {
			yaml: archiveYAML + "remote:\n  url: s3://b\n  upload:\n    partSize: 64ZiB\n",
		},
		"fractional byte part size": {
			yaml: archiveYAML + "remote:\n  url: s3://b\n  upload:\n    partSize: 1.5B\n",
		},
		"fractional part size without a multiplier": {
			yaml: archiveYAML + "remote:\n  url: s3://b\n  upload:\n    partSize: \"1.5\"\n",
		},
		"negative remote concurrency": {
			yaml: archiveYAML + "remote:\n  url: s3://b\n  upload:\n    concurrency: -1\n",
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
	// schema, and its uncommented keys must all carry defaults (beyond the
	// required archive path), so an unedited copy of it is a working starting
	// point that archives everything the token can see.
	file, err := config.LoadFile(filepath.Join("..", "..", "hcp_archiver.example.yaml"))
	require.NoError(t, err)
	assert.Equal(t, config.DefaultAddress, file.Address)
	assert.Empty(t, file.Organizations)
	assert.Equal(t, "./archive", file.Archive.Path)
}

func TestLoadFile_ReadError(t *testing.T) {
	t.Parallel()

	_, err := config.LoadFile(filepath.Join(t.TempDir(), "does-not-exist.yaml"))
	require.ErrorIs(t, err, config.ErrReadConfig)
}

func TestLoadFile_MultipleDocuments(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, archiveYAML+"organizations:\n  - one\n---\norganizations:\n  - two\n")

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
