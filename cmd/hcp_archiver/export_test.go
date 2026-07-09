package main

import (
	"github.com/spf13/cobra"

	"go.jacobcolvin.com/hcp_archiver/config"
)

// NewRootCmd exposes newRootCmd for tests.
var NewRootCmd = newRootCmd

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
