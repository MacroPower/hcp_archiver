package registry

import (
	"testing"

	"github.com/hashicorp/go-tfe"
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

func TestCompareVersions(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		a    string
		b    string
		want int // sign only
	}{
		"equal":                        {a: "1.2.3", b: "1.2.3", want: 0},
		"numeric minor lower":          {a: "1.2.3", b: "1.10.0", want: -1},
		"numeric major higher":         {a: "2.0.0", b: "1.9.9", want: 1},
		"different depth, shorter":     {a: "1.2", b: "1.2.1", want: -1},
		"missing segment is zero":      {a: "1.2", b: "1.2.0", want: 0},
		"two prereleases compare text": {a: "1.2.0-rc2", b: "1.2.0-rc1", want: 1},
		"release outranks prerelease":  {a: "1.2.0", b: "1.2.0-rc1", want: 1},
		"prerelease sorts below":       {a: "2.0.0-beta", b: "2.0.0", want: -1},
		"numbered prerelease numeric":  {a: "1.0.0-rc.10", b: "1.0.0-rc.2", want: 1},
		"numeric id below alphanum":    {a: "1.0.0-1", b: "1.0.0-alpha", want: -1},
		"more identifiers outrank":     {a: "1.0.0-rc.1", b: "1.0.0-rc.1.1", want: -1},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got := compareVersions(tc.a, tc.b)

			switch {
			case tc.want < 0:
				assert.Negative(t, got)
			case tc.want > 0:
				assert.Positive(t, got)
			default:
				assert.Zero(t, got)
			}
		})
	}
}

func TestResolveNoCodeVersion(t *testing.T) {
	t.Parallel()

	statuses := []tfe.RegistryModuleVersionStatuses{
		{Version: "1.2.0"},
		{Version: "1.10.1"},
		{Version: "latest"},
		{Version: "1.9.9"},
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
			pin:      "latest",
			statuses: []tfe.RegistryModuleVersionStatuses{{Version: "latest"}, {Version: ""}},
			want:     "",
		},
		"a final release wins over its prerelease": {
			pin:      "latest",
			statuses: []tfe.RegistryModuleVersionStatuses{{Version: "1.2.0-rc1"}, {Version: "1.2.0"}},
			want:     "1.2.0",
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

func TestNoCodeVariablesPath(t *testing.T) {
	t.Parallel()

	base := "registry/no-code-modules/nocm-abc.json"

	assert.Equal(t, "registry/no-code-modules/nocm-abc-variables.json", noCodeVariablesPath(base))
}
