package cli_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"go.jacobcolvin.com/hcp_archiver/internal/cli"
	"go.jacobcolvin.com/hcp_archiver/pkg/config"
	"go.jacobcolvin.com/hcp_archiver/pkg/remote"
)

func TestRemoteFromFile(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		file *config.File
		want *remote.Config
	}{
		"no remote section resolves to nil": {
			file: &config.File{},
			want: nil,
		},
		"remote section maps every field": {
			file: &config.File{Remote: config.FileRemote{
				URL:    "s3://config-bucket?region=us-east-1",
				Prefix: "cfg",
				Upload: config.FileRemoteUpload{
					PartSize:    config.ByteSize(8 << 20),
					Concurrency: 4,
				},
			}},
			want: &remote.Config{
				URL:         "s3://config-bucket?region=us-east-1",
				Prefix:      "cfg",
				PartSize:    8388608,
				Concurrency: 4,
			},
		},
		"url alone leaves the tuning at the backend defaults": {
			file: &config.File{Remote: config.FileRemote{URL: "s3://b"}},
			want: &remote.Config{URL: "s3://b"},
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tc.want, cli.RemoteFromFile(tc.file))
		})
	}
}
