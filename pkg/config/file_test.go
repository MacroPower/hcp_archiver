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
			yaml: "runHistory:\n  fetch:\n    count: 100\n",
			want: func(t *testing.T, file *config.File) {
				t.Helper()
				assert.Equal(t, 100, file.RunHistory.Fetch.Count)
				assert.Zero(t, file.RunHistory.Fetch.Age)
			},
		},
		"run history age zero means unbounded": {
			yaml: "runHistory:\n  fetch:\n    count: 100\n    age: 0\n",
			want: func(t *testing.T, file *config.File) {
				t.Helper()
				assert.Equal(t, 100, file.RunHistory.Fetch.Count)
				assert.Zero(t, file.RunHistory.Fetch.Age)
			},
		},
		"run history age accepts a float zero": {
			yaml: "runHistory:\n  fetch:\n    age: 0.0\n",
			want: func(t *testing.T, file *config.File) {
				t.Helper()
				assert.Zero(t, file.RunHistory.Fetch.Age)
			},
		},
		"bare section keys are unset": {
			yaml: "archive:\nexport:\nextract:\ninclude:\nremote:\nrunHistory:\n",
			want: func(t *testing.T, file *config.File) {
				t.Helper()
				assert.Equal(t, config.DefaultAddress, file.Address)
				assert.Empty(t, file.Archive.Path)
				assert.Empty(t, file.Export.Templates.Path)
				assert.Empty(t, file.Extract.Path)
				assert.True(t, file.Remote.IsZero())
				assert.Equal(t, config.FileRunHistory{}, file.RunHistory)
				assert.Equal(t, config.FileInclude{}, file.Include)
			},
		},
		"remote section decodes": {
			yaml: "remote:\n" +
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
			yaml: "remote:\n  url: s3://b\n  upload:\n    partSize: 64MiB\n",
			want: func(t *testing.T, file *config.File) {
				t.Helper()
				assert.Equal(t, int64(67108864), file.Remote.RemoteConfig().PartSize)
			},
		},
		"bare run history fetch section is unset": {
			yaml: "runHistory:\n  fetch:\n",
			want: func(t *testing.T, file *config.File) {
				t.Helper()
				assert.Equal(t, config.FileRunHistory{}, file.RunHistory)
			},
		},
		"bare upload section is unset": {
			yaml: "remote:\n  url: s3://b\n  upload:\n",
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
		"empty archive and extract sections stay empty": {
			yaml: "archive: {}\nextract: {}\n",
			want: func(t *testing.T, file *config.File) {
				t.Helper()
				assert.Empty(t, file.Archive.Path)
				assert.Empty(t, file.Extract.Path)
			},
		},
		"paths left unset stay empty": {
			yaml: "organizations:\n  - acme\n",
			want: func(t *testing.T, file *config.File) {
				t.Helper()
				assert.Empty(t, file.Archive.Path)
				assert.Empty(t, file.Extract.Path)
			},
		},
		"remote left unset disables offloading": {
			yaml: "organizations:\n  - acme\n",
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

func TestLoadFile_SourceAnnotatedErrors(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		yaml string
	}{
		"concurrency below the schema minimum": {
			yaml: "remote:\n  url: s3://b\n  upload:\n    concurrency: 0\n",
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
		"empty project name": {
			yaml: "projects:\n  - \"\"\n",
		},
		"duplicate project": {
			yaml: "projects:\n  - networking\n  - networking\n",
		},
		"empty workspace name": {
			yaml: "workspaces:\n  - \"\"\n",
		},
		"duplicate workspace": {
			yaml: "workspaces:\n  - vpc\n  - vpc\n",
		},
		"negative run history count": {
			yaml: "runHistory:\n  fetch:\n    count: -1\n",
		},
		"retired keepCount key is rejected": {
			yaml: "runHistory:\n  keepCount: 100\n",
		},
		"retired keepAge key is rejected": {
			yaml: "runHistory:\n  keepAge: 90d\n",
		},
		"retired flat fetchCount key is rejected": {
			yaml: "runHistory:\n  fetchCount: 100\n",
		},
		"retired flat fetchAge key is rejected": {
			yaml: "runHistory:\n  fetchAge: 90d\n",
		},
		"zero rate limit": {
			yaml: "rateLimit: 0\n",
		},
		"negative rate limit": {
			yaml: "rateLimit: -1\n",
		},
		"run history age must be a duration string": {
			yaml: "runHistory:\n  fetch:\n    age: 90\n",
		},
		"run history age rejects a non-zero fraction": {
			yaml: "runHistory:\n  fetch:\n    age: 0.5\n",
		},
		"address without a scheme": {
			yaml: "address: app.terraform.io\n",
		},
		"address with a non-http scheme": {
			yaml: "address: ftp://app.terraform.io\n",
		},
		"run history age must not be negative": {
			yaml: "runHistory:\n  fetch:\n    age: -24h\n",
		},
		"run history age rejects unknown units": {
			yaml: "runHistory:\n  fetch:\n    age: 1w\n",
		},
		"archive path must be a string": {
			yaml: "archive:\n  path:\n    - x\n",
		},
		"extract path must be a string": {
			yaml: "extract:\n  path:\n    - x\n",
		},
		"retired flat archiveDir key is rejected": {
			yaml: "archiveDir: ./archive\n",
		},
		"retired flat extractDir key is rejected": {
			yaml: "extractDir: ./restore\n",
		},
		"remote without a bucket": {
			yaml: "remote:\n  prefix: hcp\n",
		},
		"empty remote section": {
			yaml: "remote: {}\n",
		},
		"empty remote url": {
			yaml: "remote:\n  url: \"\"\n",
		},
		"negative remote part size": {
			yaml: "remote:\n  url: s3://b\n  upload:\n    partSize: -1\n",
		},
		"lowercase part size suffix": {
			yaml: "remote:\n  url: s3://b\n  upload:\n    partSize: 64mib\n",
		},
		"unknown part size suffix": {
			yaml: "remote:\n  url: s3://b\n  upload:\n    partSize: 64ZiB\n",
		},
		"fractional byte part size": {
			yaml: "remote:\n  url: s3://b\n  upload:\n    partSize: 1.5B\n",
		},
		"fractional part size without a multiplier": {
			yaml: "remote:\n  url: s3://b\n  upload:\n    partSize: \"1.5\"\n",
		},
		"negative remote concurrency": {
			yaml: "remote:\n  url: s3://b\n  upload:\n    concurrency: -1\n",
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
	// schema, and its uncommented keys must all carry defaults, so an unedited
	// copy of it is a working starting point that archives everything the
	// token can see.
	file, err := config.LoadFile(filepath.Join("..", "..", "hcp_archiver.example.yaml"))
	require.NoError(t, err)
	assert.Equal(t, config.DefaultAddress, file.Address)
	assert.Empty(t, file.Organizations)
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
