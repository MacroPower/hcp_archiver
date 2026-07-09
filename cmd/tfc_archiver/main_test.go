package main_test

import (
	"bytes"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	main "github.com/MacroPower/tfc_archiver/cmd/tfc_archiver"
	"github.com/MacroPower/tfc_archiver/config"
)

func TestNewRootCmd(t *testing.T) {
	t.Parallel()

	cmd := main.NewRootCmd()
	require.NotNil(t, cmd)

	assert.True(t, cmd.Runnable())
	assert.NotNil(t, cmd.Flags().Lookup("output"))
	assert.NotNil(t, cmd.Flags().Lookup("organization"))
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
	tcs := map[string]struct {
		want  func(*testing.T, *config.Config)
		token string
		args  []string
		err   error
	}{
		"defaults with output and token": {
			token: "secret",
			args:  []string{"--output", "/tmp/archive"},
			want: func(t *testing.T, cfg *config.Config) {
				t.Helper()
				assert.Equal(t, "/tmp/archive", cfg.OutputDir)
				assert.Equal(t, "secret", cfg.Token)
				assert.Equal(t, config.DefaultAddress, cfg.Address)
				assert.Equal(t, config.DefaultWorkspaceConcurrency, cfg.WorkspaceConcurrency)
				assert.Equal(t, config.ProgressModeAuto, cfg.ProgressMode)
			},
		},
		"all flags set": {
			token: "secret",
			args: []string{
				"--output", "/tmp/a",
				"--address", "https://tfe.example.com",
				"--organization", "acme",
				"--concurrency", "8",
				"--progress", "json",
				"--progress-interval", "10s",
				"--recheck-absent",
				"--stacks",
				"--hyok",
				"--registry-detail",
				"--audit-trail",
			},
			want: func(t *testing.T, cfg *config.Config) {
				t.Helper()
				assert.Equal(t, "https://tfe.example.com", cfg.Address)
				assert.Equal(t, "acme", cfg.Organization)
				assert.Equal(t, 8, cfg.WorkspaceConcurrency)
				assert.Equal(t, config.ProgressModeJSON, cfg.ProgressMode)
				assert.Equal(t, 10*time.Second, cfg.ProgressInterval)
				assert.True(t, cfg.RecheckAbsent)
				assert.True(t, cfg.Stacks)
				assert.True(t, cfg.HYOK)
				assert.True(t, cfg.RegistryDetail)
				assert.True(t, cfg.AuditTrail)
			},
		},
		"org alias maps to organization": {
			token: "secret",
			args:  []string{"--output", "/tmp/a", "--org", "acme"},
			want: func(t *testing.T, cfg *config.Config) {
				t.Helper()
				assert.Equal(t, "acme", cfg.Organization)
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
	}

	for name, tc := range tcs {
		t.Run(name, func(t *testing.T) {
			t.Setenv(config.EnvToken, tc.token)
			t.Setenv(config.EnvTokenFallback, "")

			cfg, err := main.ConfigFromArgs(tc.args)
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
