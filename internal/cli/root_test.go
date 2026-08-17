package cli_test

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.jacobcolvin.com/hcp_archiver/internal/cli"
	"go.jacobcolvin.com/hcp_archiver/pkg/config"
)

func TestNewRootCmd(t *testing.T) {
	t.Parallel()

	cmd := cli.NewRootCmd()
	require.NotNil(t, cmd)

	assert.True(t, cmd.Runnable())
	assert.NotNil(t, cmd.Flags().Lookup("config"))
	assert.NotNil(t, cmd.Flags().Lookup("archive-dir"))
	assert.NotNil(t, cmd.Flags().Lookup("progress"))

	registered := map[string]bool{}
	for _, sub := range cmd.Commands() {
		registered[sub.Name()] = true
	}

	for _, name := range []string{"version", "view", "list", "show", "extract"} {
		assert.True(t, registered[name], "%s subcommand is registered", name)
	}
}

func TestConfigFromArgs(t *testing.T) {
	fullConfig := "address: https://tfe.example.com\n" +
		"rateLimit: 10\n" +
		"organizations:\n  - acme\n  - globex\n" +
		"scope:\n  stacks: true\n  hyok: true\n  registryDetail: true\n  auditTrail: true\n"

	tcs := map[string]struct {
		want       func(*testing.T, *config.Config)
		token      string
		configYAML string
		args       []string
		configEnv  bool
		err        error
	}{
		"defaults with output and token": {
			token: "secret",
			args:  []string{"--archive-dir", "/tmp/archive"},
			want: func(t *testing.T, cfg *config.Config) {
				t.Helper()
				assert.Equal(t, "/tmp/archive", cfg.OutputDir)
				assert.Equal(t, "secret", cfg.Token)
				assert.Equal(t, config.DefaultAddress, cfg.Address)
				assert.Empty(t, cfg.Organizations)
				assert.Equal(t, config.ProgressModeAuto, cfg.ProgressMode)
				assert.False(t, cfg.Stacks)
			},
		},
		"per-run flags": {
			token: "secret",
			args: []string{
				"--archive-dir", "/tmp/a",
				"--progress", "json",
				"--progress-interval", "10s",
				"--retry-absent",
			},
			want: func(t *testing.T, cfg *config.Config) {
				t.Helper()
				assert.Equal(t, config.ProgressModeJSON, cfg.ProgressMode)
				assert.Equal(t, 10*time.Second, cfg.ProgressInterval)
				assert.True(t, cfg.RetryAbsent)
			},
		},
		"config file via flag drives archive settings": {
			token:      "secret",
			args:       []string{"--archive-dir", "/tmp/a"},
			configYAML: fullConfig,
			want: func(t *testing.T, cfg *config.Config) {
				t.Helper()
				assert.Equal(t, "https://tfe.example.com", cfg.Address)
				assert.InEpsilon(t, 10.0, cfg.RateLimit, 1e-9)
				assert.Equal(t, []string{"acme", "globex"}, cfg.Organizations)
				assert.True(t, cfg.Stacks)
				assert.True(t, cfg.HYOK)
				assert.True(t, cfg.RegistryDetail)
				assert.True(t, cfg.AuditTrail)
			},
		},
		"config file via environment": {
			token:      "secret",
			args:       []string{"--archive-dir", "/tmp/a"},
			configYAML: fullConfig,
			configEnv:  true,
			want: func(t *testing.T, cfg *config.Config) {
				t.Helper()
				assert.Equal(t, []string{"acme", "globex"}, cfg.Organizations)
			},
		},
		"remote section maps onto the config": {
			token:      "secret",
			args:       []string{"--archive-dir", "/tmp/a"},
			configYAML: "remote:\n  url: s3://my-archive\n  prefix: hcp\n",
			want: func(t *testing.T, cfg *config.Config) {
				t.Helper()
				require.NotNil(t, cfg.Remote)
				assert.Equal(t, "s3://my-archive", cfg.Remote.URL)
				assert.Equal(t, "hcp", cfg.Remote.Prefix)
			},
		},
		"no remote section leaves offloading disabled": {
			token: "secret",
			args:  []string{"--archive-dir", "/tmp/a"},
			want: func(t *testing.T, cfg *config.Config) {
				t.Helper()
				assert.Nil(t, cfg.Remote)
			},
		},
		"config archiveDir supplies output": {
			token:      "secret",
			args:       []string{},
			configYAML: "archiveDir: /tmp/from-file\n",
			want: func(t *testing.T, cfg *config.Config) {
				t.Helper()
				assert.Equal(t, "/tmp/from-file", cfg.OutputDir)
			},
		},
		"output flag wins over archiveDir": {
			token:      "secret",
			args:       []string{"--archive-dir", "/tmp/from-flag"},
			configYAML: "archiveDir: /tmp/from-file\n",
			want: func(t *testing.T, cfg *config.Config) {
				t.Helper()
				assert.Equal(t, "/tmp/from-flag", cfg.OutputDir)
			},
		},
		"missing output": {
			token: "secret",
			args:  []string{},
			err:   config.ErrMissingOutputDir,
		},
		"config without archiveDir still requires output": {
			token:      "secret",
			args:       []string{},
			configYAML: "organizations:\n  - acme\n",
			err:        config.ErrMissingOutputDir,
		},
		"missing token": {
			args: []string{"--archive-dir", "/tmp/a"},
			err:  config.ErrMissingToken,
		},
		"invalid progress mode": {
			token: "secret",
			args:  []string{"--archive-dir", "/tmp/a", "--progress", "bogus"},
			err:   config.ErrInvalidProgressMode,
		},
		"unreadable config file": {
			token: "secret",
			args:  []string{"--archive-dir", "/tmp/a", "--config", "/no/such/config.yaml"},
			err:   config.ErrReadConfig,
		},
	}

	for name, tc := range tcs {
		t.Run(name, func(t *testing.T) {
			t.Setenv(config.EnvToken, tc.token)
			t.Setenv(config.EnvTokenTFC, "")
			t.Setenv(config.EnvTokenFallback, "")
			t.Setenv(config.EnvConfigPath, "")

			args := tc.args
			if tc.configYAML != "" {
				path := writeConfigFile(t, tc.configYAML)
				if tc.configEnv {
					t.Setenv(config.EnvConfigPath, path)
				} else {
					args = append(append([]string{}, args...), "--config", path)
				}
			}

			cfg, err := cli.ConfigFromArgs(args)
			if tc.err != nil {
				require.ErrorIs(t, err, tc.err)
				assert.Nil(t, cfg)

				return
			}

			require.NoError(t, err)
			require.NotNil(t, cfg)
			tc.want(t, cfg)
		})
	}
}

func TestConfigFromArgs_ConfigFlagBeatsEnv(t *testing.T) {
	t.Setenv(config.EnvToken, "secret")
	t.Setenv(config.EnvTokenTFC, "")
	t.Setenv(config.EnvTokenFallback, "")

	// With both sources set, the --config flag wins over the environment.
	flagCfg := writeConfigFile(t, "organizations:\n  - from-flag\n")
	envCfg := writeConfigFile(t, "organizations:\n  - from-env\n")
	t.Setenv(config.EnvConfigPath, envCfg)

	cfg, err := cli.ConfigFromArgs([]string{"--archive-dir", "/tmp/a", "--config", flagCfg})
	require.NoError(t, err)
	require.NotNil(t, cfg)
	assert.Equal(t, []string{"from-flag"}, cfg.Organizations)
}

// TestConfigFromArgs_RelativeArchiveDir pins the resolution base for a
// relative archiveDir: the configuration file's own directory, not the
// process working directory.
func TestConfigFromArgs_RelativeArchiveDir(t *testing.T) {
	t.Setenv(config.EnvToken, "secret")
	t.Setenv(config.EnvTokenTFC, "")
	t.Setenv(config.EnvTokenFallback, "")
	t.Setenv(config.EnvConfigPath, "")

	cfgPath := writeConfigFile(t, "archiveDir: archive\n")

	cfg, err := cli.ConfigFromArgs([]string{"--config", cfgPath})
	require.NoError(t, err)
	require.NotNil(t, cfg)
	assert.Equal(t, filepath.Join(filepath.Dir(cfgPath), "archive"), cfg.OutputDir)
}

// writeConfigFile writes yaml to a temporary file and returns its path.
func writeConfigFile(t *testing.T, yaml string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(yaml), 0o600))

	return path
}

func TestRootHelp(t *testing.T) {
	t.Parallel()

	cmd := cli.NewRootCmd()

	out := &bytes.Buffer{}
	cmd.SetOut(out)
	cmd.SetErr(out)
	cmd.SetArgs([]string{"--help"})

	require.NoError(t, cmd.Execute())
	assert.Contains(t, out.String(), "archive-dir")
}

func TestVersionSubcommand(t *testing.T) {
	t.Parallel()

	cmd := cli.NewRootCmd()

	out := &bytes.Buffer{}
	errOut := &bytes.Buffer{}

	cmd.SetOut(out)
	cmd.SetErr(errOut)
	cmd.SetArgs([]string{"version"})

	require.NoError(t, cmd.Execute())
	assert.NotEmpty(t, out.String())
	assert.Empty(t, errOut.String())
}

// TestVersionSubcommandStdoutFallback runs the version subcommand without
// configuring command output, mirroring the real binary. It swaps os.Stdout
// for a pipe, so it must not run in parallel.
func TestVersionSubcommandStdoutFallback(t *testing.T) { //nolint:paralleltest // swaps os.Stdout
	origStdout := os.Stdout

	r, w, err := os.Pipe()
	require.NoError(t, err)

	os.Stdout = w

	t.Cleanup(func() { os.Stdout = origStdout })

	cmd := cli.NewRootCmd()
	cmd.SetArgs([]string{"version"})

	execErr := cmd.Execute()

	os.Stdout = origStdout

	require.NoError(t, w.Close())

	got, readErr := io.ReadAll(r)
	require.NoError(t, readErr)
	require.NoError(t, execErr)
	assert.NotEmpty(t, string(got), "version text is written to stdout")
}
