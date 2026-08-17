package remote

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestPartSizeFor(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		size       int64
		configured int64
		want       int
	}{
		"unconfigured small body keeps the backend default": {
			size:       2 << 20,
			configured: 0,
			want:       0,
		},
		"unconfigured body under the default ceiling keeps the backend default": {
			// 10,000 parts at the 5 MiB default cover ~48.8 GiB.
			size:       40 << 30,
			configured: 0,
			want:       0,
		},
		"unconfigured huge body grows past the default": {
			size:       100 << 30,
			configured: 0,
			want:       int((int64(100<<30) + maxUploadParts - 1) / maxUploadParts),
		},
		"configured size within the ceiling is kept": {
			size:       2 << 30,
			configured: 64 << 20,
			want:       64 << 20,
		},
		"configured size at the floor is kept": {
			size:       100 << 20,
			configured: defaultPartSize,
			want:       defaultPartSize,
		},
		"sub-minimum configured size floors at the multipart minimum": {
			// S3-compatible stores reject non-final parts under 5 MiB at
			// commit, so a 1 MiB configured size must not reach the backend.
			size:       100 << 20,
			configured: 1 << 20,
			want:       defaultPartSize,
		},
		"sub-minimum configured size grows no further than the floor": {
			// 30 GiB over 512 KiB parts is ~61k parts, past every backend's
			// cap, but the floored 5 MiB size already fits the body under
			// maxUploadParts, so the floor is the answer.
			size:       30 << 30,
			configured: 512 << 10,
			want:       defaultPartSize,
		},
		"sub-minimum configured size still grows to fit the ceiling": {
			// 100 GiB needs ~10.7 MiB parts to fit the cap, past the floor.
			size:       100 << 30,
			configured: 512 << 10,
			want:       int((int64(100<<30) + maxUploadParts - 1) / maxUploadParts),
		},
		"configured size grows for a body past its own ceiling": {
			size:       int64(64<<20)*maxUploadParts + 1,
			configured: 64 << 20,
			want:       int((int64(64<<20)*maxUploadParts + 1 + maxUploadParts - 1) / maxUploadParts),
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got := partSizeFor(tc.size, tc.configured)

			assert.Equal(t, tc.want, got)

			// Whatever the ladder picks, the body must fit the part-count
			// ceiling at the size the backend will actually use.
			effective := int64(got)
			if effective == 0 {
				effective = defaultPartSize
			}

			parts := (tc.size + effective - 1) / effective
			assert.LessOrEqual(t, parts, int64(maxUploadParts),
				"the effective part size must fit the body under the ceiling")
			assert.GreaterOrEqual(t, effective, int64(defaultPartSize),
				"the effective part size must clear the S3 multipart minimum")
		})
	}
}
