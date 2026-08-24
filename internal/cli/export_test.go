package cli

import (
	"io"

	"github.com/spf13/cobra"

	"go.jacobcolvin.com/hcp_archiver/pkg/config"
	"go.jacobcolvin.com/hcp_archiver/pkg/export"
	"go.jacobcolvin.com/hcp_archiver/pkg/progress"
)

// CmdProgress exposes the commands' shared progress adapter for offline
// tests: the export-facing hook plus the errored counter the other commands
// feed. See [cmdProgress] for the implementation.
type CmdProgress interface {
	export.Progress

	// Errored counts n more failed units.
	Errored(n int)
}

// CmdProgressForTest builds a command's progress adapter over a reporter
// rendering to w in mode, returning the adapter and its reporter. It exposes
// the commands' shared wiring for offline tests.
func CmdProgressForTest(
	w io.Writer,
	mode config.ProgressMode,
	opts ...progress.Option,
) (CmdProgress, *progress.Reporter) {
	p := &cmdProgress{}
	r := progress.New(w, mode, p, opts...)
	p.reporter = r

	return p, r
}

// RemoteFromFile exposes the configuration-to-remote mapping for offline
// tests.
var RemoteFromFile = remoteFromFile

// ConfigFromArgs registers the archive flags on a fresh command, parses args
// against them, and resolves the result into a [config.Config]. It exposes the
// flag-to-config binding for offline tests without executing the archive.
func ConfigFromArgs(args []string) (*config.Config, error) {
	cmd := &cobra.Command{Use: "test"}
	af := registerArchiveFlags(cmd)

	err := cmd.Flags().Parse(args)
	if err != nil {
		return nil, err
	}

	return af.config()
}
