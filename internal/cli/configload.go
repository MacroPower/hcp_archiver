package cli

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"

	"github.com/spf13/cobra"

	"go.jacobcolvin.com/hcp_archiver/pkg/config"
	"go.jacobcolvin.com/hcp_archiver/pkg/view"
)

// defaultConfigName is the configuration file every command looks for in the
// working directory when neither --config nor the environment names one.
const defaultConfigName = ".hcp_archiver.yaml"

// ErrNoConfig indicates no configuration file was found anywhere: the flag and
// the environment named none, and the default file is absent.
var ErrNoConfig = errors.New(
	"no configuration file: create ./" + defaultConfigName +
		", or name one with --" + flagConfig + " or $" + config.EnvConfigPath)

// registerConfigFlag binds --config/-c onto cmd and returns the string the
// flag writes into. A command registers it once: pflag panics on a redefined
// name, which is why the root command's [archiveFlags] holds the returned
// pointer rather than binding a second --config of its own.
func registerConfigFlag(cmd *cobra.Command) *string {
	path := new(string)

	cmd.Flags().StringVarP(path, flagConfig, "c", "",
		fmt.Sprintf("path to the YAML configuration file (defaults to $%s, then ./%s)",
			config.EnvConfigPath, defaultConfigName))

	return path
}

// loadConfigFile resolves the configuration file path from the --config flag,
// then the environment, then the default name in the working directory, and
// loads it. Only the default path may be absent, reported as [ErrNoConfig]; a
// file the flag or the environment names must exist and load, and any failure
// is reported as-is. The returned path is the one the file was loaded from,
// the base a relative path inside it resolves against.
func loadConfigFile(flagPath string) (*config.File, string, error) {
	path := flagPath
	if path == "" {
		path = os.Getenv(config.EnvConfigPath)
	}

	fromDefault := false
	if path == "" {
		path = defaultConfigName
		fromDefault = true
	}

	file, err := config.LoadFile(path)
	if err != nil {
		if fromDefault && errors.Is(err, fs.ErrNotExist) {
			return nil, "", ErrNoConfig
		}

		return nil, "", err //nolint:wrapcheck // Source-annotated configuration errors render as-is.
	}

	return file, path, nil
}

// cmdConfig is the loaded configuration a command runs against: the file, the
// path it was loaded from, and the archive directory its archive.path names.
// Create instances with [loadCmdConfig].
type cmdConfig struct {
	file       *config.File
	path       string
	archiveDir string
}

// loadCmdConfig loads the configuration file per [loadConfigFile] and derives
// the archive directory its archive.path names.
func loadCmdConfig(flagPath string) (cmdConfig, error) {
	file, path, err := loadConfigFile(flagPath)
	if err != nil {
		return cmdConfig{}, err
	}

	return cmdConfig{
		file:       file,
		path:       path,
		archiveDir: configDir(path, file.Archive.Path),
	}, nil
}

// open opens the archive the configuration names under ctx, against its
// mirror when the remote section names one. The commands run it under a
// progress reporter's open phase, whose spinner is what says a long mirror
// listing is still working.
func (c cmdConfig) open(ctx context.Context) (*view.Archive, error) {
	return openArchive(ctx, c.archiveDir, remoteFromFile(c.file))
}
