package cli_test

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.jacobcolvin.com/hcp_archiver/internal/cli"
	"go.jacobcolvin.com/hcp_archiver/pkg/config"
)

func TestNewRootCmd(t *testing.T) {
	t.Parallel()

	cmd := cli.NewRootCmd()
	require.NotNil(t, cmd)

	assert.False(t, cmd.Runnable(), "the root command only dispatches; run performs the archive")

	registered := map[string]*cobra.Command{}
	for _, sub := range cmd.Commands() {
		registered[sub.Name()] = sub
	}

	for _, name := range []string{"run", "version", "view", "list", "show", "extract"} {
		assert.NotNil(t, registered[name], "%s subcommand is registered", name)
	}

	run := registered["run"]
	require.NotNil(t, run)
	assert.NotNil(t, run.Flags().Lookup("config"))
	assert.Nil(t, run.Flags().Lookup("archive-path"), "the archive root comes from the configuration file")
	assert.NotNil(t, run.Flags().Lookup("progress"))
}

// TestRootBareInvocation pins the dispatch behavior: invoked without a
// subcommand, the root prints its help, naming run, and exits clean.
func TestRootBareInvocation(t *testing.T) {
	t.Parallel()

	cmd := cli.NewRootCmd()

	out := &bytes.Buffer{}
	cmd.SetOut(out)
	cmd.SetErr(out)
	cmd.SetArgs([]string{})

	require.NoError(t, cmd.Execute())
	assert.Contains(t, out.String(), "Usage:", "the bare invocation prints help")
	assert.Contains(t, out.String(), "run", "the help names the run subcommand")
}

func TestConfigFromArgs(t *testing.T) {
	archiveYAML := "archive:\n  path: /tmp/a\n"
	fullConfig := archiveYAML +
		"address: https://tfe.example.com\n" +
		"rateLimit: 10\n" +
		"organizations:\n  - acme\n  - globex\n" +
		"include:\n  stacks: true\n  hyok: true\n  registryDetail: true\n  auditTrail: true\n"

	tcs := map[string]struct {
		want       func(*testing.T, *config.Config)
		token      string
		configYAML string
		args       []string
		configEnv  bool
		err        error
	}{
		"defaults with archive path and token": {
			token:      "secret",
			configYAML: "archive:\n  path: /tmp/archive\n",
			want: func(t *testing.T, cfg *config.Config) {
				t.Helper()
				assert.Equal(t, "/tmp/archive", cfg.ArchiveDir)
				assert.Equal(t, "secret", cfg.Token)
				assert.Equal(t, config.DefaultAddress, cfg.Address)
				assert.Empty(t, cfg.Organizations)
				assert.Equal(t, config.ProgressModeAuto, cfg.ProgressMode)
				assert.False(t, cfg.Stacks)
			},
		},
		"per-run flags": {
			token:      "secret",
			configYAML: archiveYAML,
			args: []string{
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
			configYAML: fullConfig,
			configEnv:  true,
			want: func(t *testing.T, cfg *config.Config) {
				t.Helper()
				assert.Equal(t, []string{"acme", "globex"}, cfg.Organizations)
			},
		},
		"remote section maps onto the config": {
			token:      "secret",
			configYAML: archiveYAML + "remote:\n  url: s3://my-archive\n  prefix: hcp\n",
			want: func(t *testing.T, cfg *config.Config) {
				t.Helper()
				require.NotNil(t, cfg.Remote)
				assert.Equal(t, "s3://my-archive", cfg.Remote.URL)
				assert.Equal(t, "hcp", cfg.Remote.Prefix)
			},
		},
		"no remote section leaves offloading disabled": {
			token:      "secret",
			configYAML: archiveYAML,
			want: func(t *testing.T, cfg *config.Config) {
				t.Helper()
				assert.Nil(t, cfg.Remote)
			},
		},
		"config archive.path supplies the archive dir": {
			token:      "secret",
			configYAML: "archive:\n  path: /tmp/from-file\n",
			want: func(t *testing.T, cfg *config.Config) {
				t.Helper()
				assert.Equal(t, "/tmp/from-file", cfg.ArchiveDir)
			},
		},
		"empty config file is refused": {
			token:      "secret",
			configYAML: "# a comment-only document names no archive\n",
			err:        config.ErrMissingArchivePath,
		},
		"missing token": {
			configYAML: archiveYAML,
			err:        config.ErrMissingToken,
		},
		"invalid progress mode": {
			token:      "secret",
			configYAML: archiveYAML,
			args:       []string{"--progress", "bogus"},
			err:        config.ErrInvalidProgressMode,
		},
		"unreadable config file": {
			token: "secret",
			args:  []string{"--config", "/no/such/config.yaml"},
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
	flagCfg := writeConfigFile(t, "archive:\n  path: /tmp/a\norganizations:\n  - from-flag\n")
	envCfg := writeConfigFile(t, "archive:\n  path: /tmp/a\norganizations:\n  - from-env\n")
	t.Setenv(config.EnvConfigPath, envCfg)

	cfg, err := cli.ConfigFromArgs([]string{"--config", flagCfg})
	require.NoError(t, err)
	require.NotNil(t, cfg)
	assert.Equal(t, []string{"from-flag"}, cfg.Organizations)
}

// TestConfigFromArgs_DefaultConfigFile pins the last resolution step: with no
// flag and no environment, the default file in the working directory is read.
func TestConfigFromArgs_DefaultConfigFile(t *testing.T) {
	t.Setenv(config.EnvToken, "secret")
	t.Setenv(config.EnvTokenTFC, "")
	t.Setenv(config.EnvTokenFallback, "")
	t.Setenv(config.EnvConfigPath, "")

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".hcp_archiver.yaml"),
		[]byte("archive:\n  path: /tmp/from-default\n"), 0o600))
	t.Chdir(dir)

	cfg, err := cli.ConfigFromArgs(nil)
	require.NoError(t, err)
	require.NotNil(t, cfg)
	assert.Equal(t, "/tmp/from-default", cfg.ArchiveDir)
}

// TestConfigFromArgs_EnvNamedMissingFile pins the hard error: a file the
// environment names must exist; only the default file may be absent.
func TestConfigFromArgs_EnvNamedMissingFile(t *testing.T) {
	t.Setenv(config.EnvToken, "secret")
	t.Setenv(config.EnvTokenTFC, "")
	t.Setenv(config.EnvTokenFallback, "")
	t.Setenv(config.EnvConfigPath, "/nonexistent/config.yaml")

	cfg, err := cli.ConfigFromArgs(nil)
	require.ErrorIs(t, err, config.ErrReadConfig)
	require.NotErrorIs(t, err, cli.ErrNoConfig)
	assert.Nil(t, cfg)
}

// TestConfigFromArgs_NoConfigAnywhere pins the refusal: nothing named and no
// default file present is an error naming every way to supply one.
func TestConfigFromArgs_NoConfigAnywhere(t *testing.T) {
	t.Setenv(config.EnvToken, "secret")
	t.Setenv(config.EnvTokenTFC, "")
	t.Setenv(config.EnvTokenFallback, "")
	t.Setenv(config.EnvConfigPath, "")
	t.Chdir(t.TempDir())

	cfg, err := cli.ConfigFromArgs(nil)
	require.ErrorIs(t, err, cli.ErrNoConfig)
	assert.Nil(t, cfg)
}

// TestConfigFromArgs_RelativeArchiveDir pins the resolution base for a
// relative archive.path: the configuration file's own directory, not the
// process working directory.
func TestConfigFromArgs_RelativeArchiveDir(t *testing.T) {
	t.Setenv(config.EnvToken, "secret")
	t.Setenv(config.EnvTokenTFC, "")
	t.Setenv(config.EnvTokenFallback, "")
	t.Setenv(config.EnvConfigPath, "")

	cfgPath := writeConfigFile(t, "archive:\n  path: archive\n")

	cfg, err := cli.ConfigFromArgs([]string{"--config", cfgPath})
	require.NoError(t, err)
	require.NotNil(t, cfg)
	assert.Equal(t, filepath.Join(filepath.Dir(cfgPath), "archive"), cfg.ArchiveDir)
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
	assert.Contains(t, out.String(), ".hcp_archiver.yaml",
		"the help names the default configuration file")
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

func TestRootCmd_LogFileTeesTheStream(t *testing.T) {
	t.Parallel()

	root := buildMiniArchive(t)
	logPath := filepath.Join(t.TempDir(), "run.log")

	_, _, err := runCmdIn(t, root, "", "list", "--log-file", logPath, "--log-level", "debug")
	require.NoError(t, err)

	// The opening announcement rides the handler's own stream, so its
	// presence proves the file receives what stderr receives.
	data, err := os.ReadFile(logPath)
	require.NoError(t, err)
	assert.Contains(t, string(data), "log_file_opened",
		"the handler's output is teed into the file")
}

func TestRootCmd_LogFileOpenRefused(t *testing.T) {
	t.Parallel()

	root := buildMiniArchive(t)
	logPath := filepath.Join(t.TempDir(), "missing", "run.log")

	_, _, err := runCmdIn(t, root, "", "list", "--log-file", logPath)
	require.ErrorIs(t, err, cli.ErrLogFile,
		"an unopenable destination refuses the run rather than logging to stderr alone")
}
