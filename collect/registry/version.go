package registry

import (
	"strconv"
	"strings"

	"github.com/hashicorp/go-tfe"
)

// Registry leaf filenames written under a module or provider directory.
const (
	moduleFile        = "module.json"
	moduleCommitsFile = "commits.json"
	providerFile      = "provider.json"
)

// versionLatest is the floating version pin that does not address a specific
// version, so it cannot drive a per-version read.
const versionLatest = "latest"

// moduleVersionFilename returns the leaf a module version's frozen metadata is
// archived under, keyed on its version.
func moduleVersionFilename(version string) string {
	return "version-" + version + ".json"
}

// providerVersionFilename returns the leaf a provider version's frozen metadata
// is archived under, keyed on its version.
func providerVersionFilename(version string) string {
	return "version-" + version + ".json"
}

// providerPlatformsFilename returns the leaf a provider version's platform list
// is archived under, keyed on its version.
func providerPlatformsFilename(version string) string {
	return "version-" + version + "-platforms.json"
}

// noCodeVariablesPath derives the variable-options leaf beside a no-code
// module's file, reusing the module's own path so the two sit together.
func noCodeVariablesPath(base string) string {
	return strings.TrimSuffix(base, ".json") + "-variables.json"
}

// isConcreteVersion reports whether v names a specific version rather than the
// floating "latest" pin or an empty value, so it can address a per-version read.
func isConcreteVersion(v string) bool {
	return v != "" && v != versionLatest
}

// resolveNoCodeVersion picks the concrete module version whose no-code variable
// options should be read: the pin itself when it is concrete, otherwise the
// newest concrete version among the module's version statuses. It returns the
// empty string when no concrete version can be resolved.
func resolveNoCodeVersion(pin string, statuses []tfe.RegistryModuleVersionStatuses) string {
	if isConcreteVersion(pin) {
		return pin
	}

	return latestModuleVersion(statuses)
}

// latestModuleVersion returns the newest concrete version among statuses, or the
// empty string when none is concrete.
func latestModuleVersion(statuses []tfe.RegistryModuleVersionStatuses) string {
	best := ""

	for _, s := range statuses {
		if !isConcreteVersion(s.Version) {
			continue
		}

		if best == "" || compareVersions(s.Version, best) > 0 {
			best = s.Version
		}
	}

	return best
}

// compareVersions orders two dotted version strings, comparing each segment
// numerically when both parse as integers and lexically otherwise. It returns a
// negative number when a sorts before b, zero when they are equal, and a
// positive number when a sorts after b.
func compareVersions(a, b string) int {
	as := strings.Split(a, ".")
	bs := strings.Split(b, ".")
	n := max(len(as), len(bs))

	for i := range n {
		x := segmentAt(as, i)
		y := segmentAt(bs, i)

		xn, xErr := strconv.Atoi(x)
		yn, yErr := strconv.Atoi(y)

		if xErr == nil && yErr == nil {
			if xn != yn {
				return xn - yn
			}

			continue
		}

		if c := strings.Compare(x, y); c != 0 {
			return c
		}
	}

	return 0
}

// segmentAt returns the i-th segment of parts, or the empty string when i is out
// of range, so two version strings of different depth stay comparable.
func segmentAt(parts []string, i int) string {
	if i < len(parts) {
		return parts[i]
	}

	return ""
}
