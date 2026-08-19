package cli

import (
	"io"

	"github.com/spf13/cobra"

	"go.jacobcolvin.com/hcp_archiver/pkg/config"
	"go.jacobcolvin.com/hcp_archiver/pkg/export"
	"go.jacobcolvin.com/hcp_archiver/pkg/progress"
)

// ExportProgressForTest builds the export command's progress adapter over a
// reporter rendering to w in mode, returning the adapter and its reporter. It
// exposes the command's wiring for offline tests.
func ExportProgressForTest(
	w io.Writer,
	mode config.ProgressMode,
	opts ...progress.Option,
) (export.Progress, *progress.Reporter) {
	p := &exportProgress{}
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
