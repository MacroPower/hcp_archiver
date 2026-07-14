package archiver

import (
	"encoding/json"
	"fmt"
	"path/filepath"

	"go.jacobcolvin.com/hcp_archiver/atomicfile"
	"go.jacobcolvin.com/hcp_archiver/config"
	"go.jacobcolvin.com/hcp_archiver/remote"
	"go.jacobcolvin.com/hcp_archiver/store"
)

// remoteConfig maps the validated configuration surface onto the remote
// client's configuration — the one place the two shapes meet, so the config
// package never imports the AWS SDK.
func remoteConfig(rc *config.RemoteConfig) remote.Config {
	return remote.Config{
		Bucket:           rc.Bucket,
		Prefix:           rc.Prefix,
		Endpoint:         rc.Endpoint,
		Region:           rc.Region,
		StorageClass:     rc.StorageClass,
		PartSize:         rc.PartSize,
		Concurrency:      rc.Concurrency,
		ForcePathStyle:   rc.ForcePathStyle,
		DisableChecksums: rc.DisableChecksums,
	}
}

// writeRemoteMarker records the read-relevant remote settings at the
// organization's archive root, so a later `view` of the archive can reach
// the offloaded bundles without the original configuration file.
func (a *Archiver) writeRemoteMarker(st *store.Store) error {
	marker := remoteConfig(a.cfg.Remote).Marker()

	data, err := json.MarshalIndent(marker, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal remote marker: %w", err)
	}

	err = atomicfile.WriteFile(filepath.Join(st.Root(), remote.MarkerName), append(data, '\n'))
	if err != nil {
		return fmt.Errorf("write remote marker: %w", err)
	}

	return nil
}
