package config_test

import (
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.jacobcolvin.com/hcp_archiver/pkg/config"
)

func TestNew_Defaults(t *testing.T) {
	t.Parallel()

	cfg, err := config.New(
		config.WithToken("tok"),
		config.WithOutputDir("/tmp/archive"),
	)
	require.NoError(t, err)

	assert.Equal(t, "tok", cfg.Token)
	assert.Equal(t, config.DefaultAddress, cfg.Address)
	assert.Empty(t, cfg.Organizations)
	assert.Empty(t, cfg.Projects)
	assert.Empty(t, cfg.Workspaces)
	assert.Equal(t, config.ProgressModeAuto, cfg.ProgressMode)
	assert.Equal(t, config.DefaultProgressInterval, cfg.ProgressInterval)
	assert.Zero(t, cfg.RunHistoryCount)
	assert.Zero(t, cfg.RunHistoryAge)
	assert.Zero(t, cfg.RateLimit)
	assert.False(t, cfg.RetryAbsent)
	assert.False(t, cfg.Stacks)
	assert.False(t, cfg.HYOK)
	assert.False(t, cfg.RegistryDetail)
	assert.False(t, cfg.AuditTrail)
}

func TestNew_EmptyAddressKeepsDefault(t *testing.T) {
	t.Parallel()

	cfg, err := config.New(
		config.WithToken("tok"),
		config.WithOutputDir("/tmp/archive"),
		config.WithAddress(""),
	)
	require.NoError(t, err)

	assert.Equal(t, config.DefaultAddress, cfg.Address)
}

func TestNew_OptionsOverrideDefaults(t *testing.T) {
	t.Parallel()

	cfg, err := config.New(
		config.WithToken("tok"),
		config.WithOutputDir("/tmp/archive"),
		config.WithAddress("https://tfe.example.com"),
		config.WithOrganizations([]string{"acme"}),
		config.WithProjects([]string{"networking"}),
		config.WithWorkspaces([]string{"vpc"}),
		config.WithProgressMode(config.ProgressModeJSON),
		config.WithProgressInterval(10*time.Second),
		config.WithRunHistoryCount(500),
		config.WithRunHistoryAge(90*24*time.Hour),
		config.WithRateLimit(12.5),
		config.WithRetryAbsent(true),
		config.WithStacks(true),
		config.WithHYOK(true),
		config.WithRegistryDetail(true),
		config.WithAuditTrail(true),
	)
	require.NoError(t, err)

	assert.Equal(t, "https://tfe.example.com", cfg.Address)
	assert.Equal(t, []string{"acme"}, cfg.Organizations)
	assert.Equal(t, []string{"networking"}, cfg.Projects)
	assert.Equal(t, []string{"vpc"}, cfg.Workspaces)
	assert.Equal(t, config.ProgressModeJSON, cfg.ProgressMode)
	assert.Equal(t, 10*time.Second, cfg.ProgressInterval)
	assert.Equal(t, 500, cfg.RunHistoryCount)
	assert.Equal(t, 90*24*time.Hour, cfg.RunHistoryAge)
	assert.InEpsilon(t, 12.5, cfg.RateLimit, 1e-9)
	assert.True(t, cfg.RetryAbsent)
	assert.True(t, cfg.Stacks)
	assert.True(t, cfg.HYOK)
	assert.True(t, cfg.RegistryDetail)
	assert.True(t, cfg.AuditTrail)
}

func TestNew_NonPositiveRateLimitKeepsDefault(t *testing.T) {
	t.Parallel()

	cfg, err := config.New(
		config.WithToken("tok"),
		config.WithOutputDir("/tmp/archive"),
		config.WithRateLimit(0),
		config.WithRateLimit(-5),
	)
	require.NoError(t, err)

	assert.Zero(t, cfg.RateLimit)
}

func TestNew_TokenFromEnv(t *testing.T) {
	tests := map[string]struct {
		hcpToken    *string
		tfcToken    *string
		tfeToken    *string
		optionToken string
		want        string
		wantErr     error
	}{
		"hcp token used": {
			hcpToken: new("hcp-value"),
			want:     "hcp-value",
		},
		"tfc token used when hcp unset": {
			tfcToken: new("tfc-value"),
			want:     "tfc-value",
		},
		"tfe token used when hcp and tfc unset": {
			tfeToken: new("tfe-value"),
			want:     "tfe-value",
		},
		"hcp precedence over tfc": {
			hcpToken: new("hcp-value"),
			tfcToken: new("tfc-value"),
			want:     "hcp-value",
		},
		"hcp precedence over tfe": {
			hcpToken: new("hcp-value"),
			tfeToken: new("tfe-value"),
			want:     "hcp-value",
		},
		"tfc precedence over tfe": {
			tfcToken: new("tfc-value"),
			tfeToken: new("tfe-value"),
			want:     "tfc-value",
		},
		"empty hcp falls back to tfc": {
			hcpToken: new(""),
			tfcToken: new("tfc-value"),
			want:     "tfc-value",
		},
		"empty tfc falls back to tfe": {
			tfcToken: new(""),
			tfeToken: new("tfe-value"),
			want:     "tfe-value",
		},
		"option token beats environment": {
			optionToken: "opt-value",
			hcpToken:    new("hcp-value"),
			want:        "opt-value",
		},
		"all empty is missing token": {
			hcpToken: new(""),
			tfcToken: new(""),
			tfeToken: new(""),
			wantErr:  config.ErrMissingToken,
		},
		"none set is missing token": {
			wantErr: config.ErrMissingToken,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			if tc.hcpToken != nil {
				t.Setenv(config.EnvToken, *tc.hcpToken)
			} else {
				t.Setenv(config.EnvToken, "")
			}

			if tc.tfcToken != nil {
				t.Setenv(config.EnvTokenTFC, *tc.tfcToken)
			} else {
				t.Setenv(config.EnvTokenTFC, "")
			}

			if tc.tfeToken != nil {
				t.Setenv(config.EnvTokenFallback, *tc.tfeToken)
			} else {
				t.Setenv(config.EnvTokenFallback, "")
			}

			opts := []config.Option{config.WithOutputDir("/tmp/archive")}
			if tc.optionToken != "" {
				opts = append(opts, config.WithToken(tc.optionToken))
			}

			cfg, err := config.New(opts...)
			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)

				return
			}

			require.NoError(t, err)
			assert.Equal(t, tc.want, cfg.Token)
		})
	}
}

func TestValidateOrganizationName(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		name string
		err  error
	}{
		"plain name":             {name: "acme"},
		"dashes and underscores": {name: "acme-corp_2"},
		"dots inside the name":   {name: "acme.corp"},
		"leading dot":            {name: ".hidden"},
		"unicode name":           {name: "acmé"},
		"empty":                  {name: "", err: config.ErrInvalidOrganization},
		"current directory":      {name: ".", err: config.ErrInvalidOrganization},
		"parent directory":       {name: "..", err: config.ErrInvalidOrganization},
		"traversal prefix":       {name: "../../home/user/.ssh", err: config.ErrInvalidOrganization},
		"traversal suffix":       {name: "acme/..", err: config.ErrInvalidOrganization},
		"nested segments":        {name: "acme/sub", err: config.ErrInvalidOrganization},
		"absolute path":          {name: "/etc/cron.d", err: config.ErrInvalidOrganization},
		"trailing separator":     {name: "acme/", err: config.ErrInvalidOrganization},
		"windows separator":      {name: `acme\sub`, err: config.ErrInvalidOrganization},
		"newline":                {name: "acme\nevil", err: config.ErrInvalidOrganization},
		"nul byte":               {name: "acme\x00", err: config.ErrInvalidOrganization},
		"terminal escape":        {name: "\x1b[2Jacme", err: config.ErrInvalidOrganization},
		"delete character":       {name: "acme\x7f", err: config.ErrInvalidOrganization},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			err := config.ValidateOrganizationName(tc.name)

			if tc.err != nil {
				require.ErrorIs(t, err, tc.err)
				assert.Contains(t, err.Error(), strconv.Quote(tc.name))

				return
			}

			require.NoError(t, err)
		})
	}
}

func TestNew_ValidationErrors(t *testing.T) {
	tests := map[string]struct {
		opts    []config.Option
		wantErr error
	}{
		"missing token": {
			opts:    []config.Option{config.WithOutputDir("/tmp/archive")},
			wantErr: config.ErrMissingToken,
		},
		"missing output dir": {
			opts:    []config.Option{config.WithToken("tok")},
			wantErr: config.ErrMissingOutputDir,
		},
		"negative run history count": {
			opts: []config.Option{
				config.WithToken("tok"),
				config.WithOutputDir("/tmp/archive"),
				config.WithRunHistoryCount(-1),
			},
			wantErr: config.ErrInvalidRunHistoryCount,
		},
		"negative run history age": {
			opts: []config.Option{
				config.WithToken("tok"),
				config.WithOutputDir("/tmp/archive"),
				config.WithRunHistoryAge(-time.Hour),
			},
			wantErr: config.ErrInvalidRunHistoryAge,
		},
		"invalid progress mode": {
			opts: []config.Option{
				config.WithToken("tok"),
				config.WithOutputDir("/tmp/archive"),
				config.WithProgressMode(config.ProgressMode("loud")),
			},
			wantErr: config.ErrInvalidProgressMode,
		},
		"remote without a url": {
			opts: []config.Option{
				config.WithToken("tok"),
				config.WithOutputDir("/tmp/archive"),
				config.WithRemote(config.RemoteConfig{Prefix: "hcp"}),
			},
			wantErr: config.ErrMissingRemoteURL,
		},
		"negative remote part size": {
			opts: []config.Option{
				config.WithToken("tok"),
				config.WithOutputDir("/tmp/archive"),
				config.WithRemote(config.RemoteConfig{URL: "s3://b", PartSize: -1}),
			},
			wantErr: config.ErrInvalidRemotePartSize,
		},
		"negative remote concurrency": {
			opts: []config.Option{
				config.WithToken("tok"),
				config.WithOutputDir("/tmp/archive"),
				config.WithRemote(config.RemoteConfig{URL: "s3://b", Concurrency: -1}),
			},
			wantErr: config.ErrInvalidRemoteConcurrency,
		},
		"traversing organization name": {
			opts: []config.Option{
				config.WithToken("tok"),
				config.WithOutputDir("/tmp/archive"),
				config.WithOrganizations([]string{"acme", "../../etc"}),
			},
			wantErr: config.ErrInvalidOrganization,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			// Ensure the environment does not supply a token for these cases.
			t.Setenv(config.EnvToken, "")
			t.Setenv(config.EnvTokenTFC, "")
			t.Setenv(config.EnvTokenFallback, "")

			cfg, err := config.New(tc.opts...)
			require.ErrorIs(t, err, tc.wantErr)
			assert.Nil(t, cfg)
		})
	}
}
