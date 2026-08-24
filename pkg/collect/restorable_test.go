package collect_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"go.jacobcolvin.com/hcp_archiver/pkg/collect"
)

func TestRestorable(t *testing.T) {
	t.Parallel()

	const ws = "projects/prod/workspaces/api"

	tests := map[string]struct {
		relPath string
		want    bool
	}{
		// The warm layer a restore materializes.
		"bundle sidecar":        {relPath: ws + "/bundles/runs.gen0001.zip.sidecar.ndjson", want: true},
		"roll-up":               {relPath: ws + "/runs.ndjson", want: true},
		"loose metadata":        {relPath: ws + "/runs/run-1/run.json", want: true},
		"workspace settings":    {relPath: ws + "/workspace.json", want: true},
		"history sidecar":       {relPath: ws + "/workspace.history.ndjson", want: true},
		"identity sidecar":      {relPath: ws + "/.identity.json", want: true},
		"organization marker":   {relPath: ".remote.json", want: true},
		"org-root snapshot":     {relPath: ".ledger/snapshot.json", want: true},
		"workspace snapshot":    {relPath: ws + "/.ledger/snapshot.json", want: true},
		"organization metadata": {relPath: "org.json", want: true},

		// The evicted surfaces, which live remotely by design.
		"sealed bundle zip": {relPath: ws + "/bundles/runs.gen0001.zip"},
		"config tarball":    {relPath: "config-versions/cv-1.tar.gz"},

		// The files a local archive must never receive from a mirror. The
		// replay log is the dangerous one: restored beside a newer snapshot it
		// would replay superseded ledger state.
		"replay log":          {relPath: ".ledger/log.ndjson"},
		"ledger lock":         {relPath: ".ledger/lock"},
		"staging temp":        {relPath: ws + "/.atomicfile-12345.tmp"},
		"config tarball stub": {relPath: "config-versions/cv-1.tar.gz.remote.json"},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tt.want, collect.Restorable(tt.relPath))
		})
	}
}
