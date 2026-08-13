package main

import (
	"github.com/spf13/cobra"

	"go.jacobcolvin.com/hcp_archiver/config"
	"go.jacobcolvin.com/hcp_archiver/remote"
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

// ResolveRemoteFromArgs registers the mirror-location flags on a fresh
// command, parses args against them, and resolves the result. It exposes the
// flag-to-remote binding (--remote over the configuration file, prefix
// override) for offline tests without opening an archive.
func ResolveRemoteFromArgs(args []string) (*remote.Config, error) {
	cmd := &cobra.Command{Use: "test"}
	rf := registerRemoteFlags(cmd)

	err := cmd.Flags().Parse(args)
	if err != nil {
		return nil, err
	}

	return rf.resolve()
}
