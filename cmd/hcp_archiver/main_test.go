package main_test

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	main "go.jacobcolvin.com/hcp_archiver/cmd/hcp_archiver"
	"go.jacobcolvin.com/hcp_archiver/config"
)

func TestNewRootCmd(t *testing.T) {
	t.Parallel()

	cmd := main.NewRootCmd()
	require.NotNil(t, cmd)

	assert.True(t, cmd.Runnable())
	assert.NotNil(t, cmd.Flags().Lookup("config"))
	assert.NotNil(t, cmd.Flags().Lookup("output"))
	assert.NotNil(t, cmd.Flags().Lookup("progress"))

	var found bool

	for _, sub := range cmd.Commands() {
		if sub.Name() == "version" {
			found = true
		}
	}

	assert.True(t, found, "version subcommand is registered")
}

func TestConfigFromArgs(t *testing.T) {
	fullConfig := "address: https://tfe.example.com\n" +
		"organizations:\n  - acme\n  - globex\n" +
		"maxConcurrency: 32\n" +
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
			args:  []string{"--output", "/tmp/archive"},
			want: func(t *testing.T, cfg *config.Config) {
				t.Helper()
				assert.Equal(t, "/tmp/archive", cfg.OutputDir)
				assert.Equal(t, "secret", cfg.Token)
				assert.Equal(t, config.DefaultAddress, cfg.Address)
				assert.Empty(t, cfg.Organizations)
				assert.Equal(t, config.DefaultMaxConcurrency, cfg.MaxConcurrency)
				assert.Equal(t, config.ProgressModeAuto, cfg.ProgressMode)
				assert.False(t, cfg.Stacks)
			},
		},
		"per-run flags": {
			token: "secret",
			args: []string{
				"--output", "/tmp/a",
				"--progress", "json",
				"--progress-interval", "10s",
				"--recheck-absent",
			},
			want: func(t *testing.T, cfg *config.Config) {
				t.Helper()
				assert.Equal(t, config.ProgressModeJSON, cfg.ProgressMode)
				assert.Equal(t, 10*time.Second, cfg.ProgressInterval)
				assert.True(t, cfg.RecheckAbsent)
			},
		},
		"config file via flag drives archive settings": {
			token:      "secret",
			args:       []string{"--output", "/tmp/a"},
			configYAML: fullConfig,
			want: func(t *testing.T, cfg *config.Config) {
				t.Helper()
				assert.Equal(t, "https://tfe.example.com", cfg.Address)
				assert.Equal(t, []string{"acme", "globex"}, cfg.Organizations)
				assert.Equal(t, 32, cfg.MaxConcurrency)
				assert.True(t, cfg.Stacks)
				assert.True(t, cfg.HYOK)
				assert.True(t, cfg.RegistryDetail)
				assert.True(t, cfg.AuditTrail)
			},
		},
		"config file via environment": {
			token:      "secret",
			args:       []string{"--output", "/tmp/a"},
			configYAML: fullConfig,
			configEnv:  true,
			want: func(t *testing.T, cfg *config.Config) {
				t.Helper()
				assert.Equal(t, []string{"acme", "globex"}, cfg.Organizations)
			},
		},
		"missing output": {
			token: "secret",
			args:  []string{},
			err:   config.ErrMissingOutputDir,
		},
		"missing token": {
			args: []string{"--output", "/tmp/a"},
			err:  config.ErrMissingToken,
		},
		"invalid progress mode": {
			token: "secret",
			args:  []string{"--output", "/tmp/a", "--progress", "bogus"},
			err:   config.ErrInvalidProgressMode,
		},
		"unreadable config file": {
			token: "secret",
			args:  []string{"--output", "/tmp/a", "--config", "/no/such/config.yaml"},
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

			cfg, err := main.ConfigFromArgs(args)
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

	cfg, err := main.ConfigFromArgs([]string{"--output", "/tmp/a", "--config", flagCfg})
	require.NoError(t, err)
	require.NotNil(t, cfg)
	assert.Equal(t, []string{"from-flag"}, cfg.Organizations)
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

	cmd := main.NewRootCmd()

	out := &bytes.Buffer{}
	cmd.SetOut(out)
	cmd.SetErr(out)
	cmd.SetArgs([]string{"--help"})

	require.NoError(t, cmd.Execute())
	assert.Contains(t, out.String(), "output")
}

func TestVersionSubcommand(t *testing.T) {
	t.Parallel()

	cmd := main.NewRootCmd()

	out := &bytes.Buffer{}
	cmd.SetOut(out)
	cmd.SetErr(out)
	cmd.SetArgs([]string{"version"})

	require.NoError(t, cmd.Execute())
}
