package registry

import (
	"testing"

	"github.com/hashicorp/go-tfe/v2"
	"github.com/stretchr/testify/assert"
)

func TestIsConcreteVersion(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		version string
		want    bool
	}{
		"a semver is concrete":  {version: "1.2.3", want: true},
		"latest is not":         {version: "latest", want: false},
		"empty is not":          {version: "", want: false},
		"a prerelease is still": {version: "2.0.0-rc1", want: true},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tc.want, isConcreteVersion(tc.version))
		})
	}
}

func TestLatestModuleVersion(t *testing.T) {
	t.Parallel()

	versions := func(vs ...string) []tfe.RegistryModuleVersionStatuses {
		out := make([]tfe.RegistryModuleVersionStatuses, len(vs))
		for i, v := range vs {
			out[i] = tfe.RegistryModuleVersionStatuses{
				Version: v,
				Status:  tfe.RegistryModuleVersionStatusOk,
			}
		}

		return out
	}

	tests := map[string]struct {
		statuses []tfe.RegistryModuleVersionStatuses
		want     string
	}{
		"numeric segments compare numerically, not lexically": {
			statuses: versions("1.9.9", "1.10.0", "1.2.3"),
			want:     "1.10.0",
		},
		"a shorter core pads with zeros": {
			statuses: versions("1.2", "1.2.1"),
			want:     "1.2.1",
		},
		"a stable release outranks a newer prerelease": {
			statuses: versions("2.0.0-rc1", "1.9.0"),
			want:     "1.9.0",
		},
		"the newest prerelease wins when no stable exists": {
			statuses: versions("1.0.0-rc.2", "1.0.0-rc.10"),
			want:     "1.0.0-rc.10",
		},
		"a numeric prerelease identifier ranks below alphanumeric": {
			statuses: versions("1.0.0-1", "1.0.0-alpha"),
			want:     "1.0.0-alpha",
		},
		"floating and empty pins are ignored": {
			statuses: versions("latest", "", "0.1.0"),
			want:     "0.1.0",
		},
		"nothing concrete resolves to empty": {
			statuses: versions("latest", ""),
			want:     "",
		},
		"an unparsable version never outranks a real release": {
			statuses: versions("not-a-version-at-all!", "0.0.1"),
			want:     "0.0.1",
		},
		"an unparsable version is the last resort": {
			statuses: versions("not-a-version-at-all!"),
			want:     "not-a-version-at-all!",
		},
		"a newer version that failed ingress is skipped": {
			statuses: append(
				versions("1.0.0"),
				tfe.RegistryModuleVersionStatuses{
					Version: "2.0.0",
					Status:  tfe.RegistryModuleVersionStatusRegIngressFailed,
				},
			),
			want: "1.0.0",
		},
		"a version still ingressing is skipped": {
			statuses: append(
				versions("1.0.0"),
				tfe.RegistryModuleVersionStatuses{
					Version: "2.0.0",
					Status:  tfe.RegistryModuleVersionStatusPending,
				},
			),
			want: "1.0.0",
		},
		"no ok version resolves to empty": {
			statuses: []tfe.RegistryModuleVersionStatuses{{
				Version: "1.0.0",
				Status:  tfe.RegistryModuleVersionStatusCloneFailed,
			}},
			want: "",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tc.want, latestModuleVersion(tc.statuses))
		})
	}
}

func TestResolveNoCodeVersion(t *testing.T) {
	t.Parallel()

	ok := tfe.RegistryModuleVersionStatusOk

	statuses := []tfe.RegistryModuleVersionStatuses{
		{Version: "1.2.0", Status: ok},
		{Version: "1.10.1", Status: ok},
		{Version: "latest", Status: ok},
		{Version: "1.9.9", Status: ok},
	}

	tests := map[string]struct {
		pin      string
		statuses []tfe.RegistryModuleVersionStatuses
		want     string
	}{
		"a concrete pin wins outright": {
			pin:      "3.1.4",
			statuses: statuses,
			want:     "3.1.4",
		},
		"latest resolves to the newest concrete status": {
			pin:      "latest",
			statuses: statuses,
			want:     "1.10.1",
		},
		"empty pin resolves to the newest concrete status": {
			pin:      "",
			statuses: statuses,
			want:     "1.10.1",
		},
		"no concrete status resolves to empty": {
			pin: "latest",
			statuses: []tfe.RegistryModuleVersionStatuses{
				{Version: "latest", Status: ok},
				{Version: "", Status: ok},
			},
			want: "",
		},
		"a final release wins over its prerelease": {
			pin: "latest",
			statuses: []tfe.RegistryModuleVersionStatuses{
				{Version: "1.2.0-rc1", Status: ok},
				{Version: "1.2.0", Status: ok},
			},
			want: "1.2.0",
		},
		"latest skips a broken newest version": {
			pin: "latest",
			statuses: []tfe.RegistryModuleVersionStatuses{
				{Version: "1.2.0", Status: ok},
				{Version: "1.3.0", Status: tfe.RegistryModuleVersionStatusRegIngressFailed},
			},
			want: "1.2.0",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tc.want, resolveNoCodeVersion(tc.pin, tc.statuses))
		})
	}
}

func TestVersionFilenames(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "version-1.2.3.json", versionFilename("1.2.3"))
	assert.Equal(t, "platforms-1.2.3.json", providerPlatformsFilename("1.2.3"))

	// The two schemes cannot collide: a version literally named "1.2.3-platforms"
	// keeps a distinct leaf from version "1.2.3"'s platform file.
	assert.NotEqual(t, versionFilename("1.2.3-platforms"), providerPlatformsFilename("1.2.3"))
}
