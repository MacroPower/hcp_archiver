package main_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	main "go.jacobcolvin.com/hcp_archiver/cmd/hcp_archiver"
	"go.jacobcolvin.com/hcp_archiver/config"
	"go.jacobcolvin.com/hcp_archiver/remote"
)

// remoteYAML is a configuration file whose remote section names a mirror.
const remoteYAML = `remote:
  url: s3://config-bucket?region=us-east-1
  prefix: cfg
  partSize: 8388608
  concurrency: 4
`

func TestResolveRemote(t *testing.T) {
	cfgPath := writeConfigFile(t, remoteYAML)
	noRemotePath := writeConfigFile(t, "organizations:\n  - my-org\n")

	tests := map[string]struct {
		args []string
		want *remote.Config
		err  string
	}{
		"nothing named resolves to nil": {
			args: nil,
			want: nil,
		},
		"remote flag alone": {
			args: []string{"--remote", "s3://flag-bucket", "--remote-prefix", "fp"},
			want: &remote.Config{URL: "s3://flag-bucket", Prefix: "fp"},
		},
		"config file remote section": {
			args: []string{"--config", cfgPath},
			want: &remote.Config{
				URL:         "s3://config-bucket?region=us-east-1",
				Prefix:      "cfg",
				PartSize:    8388608,
				Concurrency: 4,
			},
		},
		"remote flag wins over config file": {
			args: []string{"--config", cfgPath, "--remote", "s3://flag-bucket"},
			want: &remote.Config{URL: "s3://flag-bucket"},
		},
		"prefix flag overrides config file prefix": {
			args: []string{"--config", cfgPath, "--remote-prefix", "override"},
			want: &remote.Config{
				URL:         "s3://config-bucket?region=us-east-1",
				Prefix:      "override",
				PartSize:    8388608,
				Concurrency: 4,
			},
		},
		"config file without remote section resolves to nil": {
			args: []string{"--config", noRemotePath},
			want: nil,
		},
		"prefix without any remote refuses": {
			args: []string{"--remote-prefix", "orphan"},
			err:  "--remote-prefix needs",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			// The environment fallback would otherwise leak the developer's
			// own configuration into the "nothing named" cases. Setenv also
			// keeps these cases serial, which the shared variable needs.
			t.Setenv(config.EnvConfigPath, "")

			got, err := main.ResolveRemoteFromArgs(tc.args)

			if tc.err != "" {
				require.ErrorContains(t, err, tc.err)

				return
			}

			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestResolveRemote_EnvironmentConfigPath(t *testing.T) {
	cfgPath := writeConfigFile(t, remoteYAML)
	t.Setenv(config.EnvConfigPath, cfgPath)

	got, err := main.ResolveRemoteFromArgs(nil)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "s3://config-bucket?region=us-east-1", got.URL)
}
