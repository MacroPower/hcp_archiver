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

// versionFilename returns the leaf a module or provider version's frozen
// metadata is archived under, keyed on its version. Both registries share the
// scheme; a provider's platform list sits beside it under
// [providerPlatformsFilename], whose distinct prefix cannot alias a version.
func versionFilename(version string) string {
	return "version-" + version + ".json"
}

// providerPlatformsFilename returns the leaf a provider version's platform list
// is archived under, keyed on its version. Its "platforms-" prefix is one
// versionFilename never emits, so a provider version literally named
// "<v>-platforms" cannot alias onto version "<v>"'s platform file.
func providerPlatformsFilename(version string) string {
	return "platforms-" + version + ".json"
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

// latestModuleVersion returns the newest stable concrete version among statuses,
// falling back to the newest prerelease only when no stable version exists, and
// the empty string when none is concrete. The registry serves a "latest" pin as
// the newest stable, so a prerelease must not shadow it when resolving the
// version whose no-code variable options to archive.
func latestModuleVersion(statuses []tfe.RegistryModuleVersionStatuses) string {
	bestStable := ""
	bestAny := ""

	for _, s := range statuses {
		if !isConcreteVersion(s.Version) {
			continue
		}

		if bestAny == "" || compareVersions(s.Version, bestAny) > 0 {
			bestAny = s.Version
		}

		if _, pre := splitPrerelease(s.Version); pre == "" {
			if bestStable == "" || compareVersions(s.Version, bestStable) > 0 {
				bestStable = s.Version
			}
		}
	}

	if bestStable != "" {
		return bestStable
	}

	return bestAny
}

// compareVersions orders two dotted version strings by SemVer-style precedence.
// It compares the numeric release core segment by segment (numerically when both
// segments parse as integers, lexically otherwise); when the cores are equal, a
// version carrying no pre-release outranks one that does, and two pre-releases
// compare by SemVer identifier precedence (see [comparePrerelease]). It returns a
// negative number when a sorts before b, zero when they are equal, and a positive
// number when a sorts after b.
func compareVersions(a, b string) int {
	aCore, aPre := splitPrerelease(a)
	bCore, bPre := splitPrerelease(b)

	if c := compareCore(aCore, bCore); c != 0 {
		return c
	}

	switch {
	case aPre == "" && bPre == "":
		return 0
	case aPre == "":
		// A final release outranks its own pre-releases (1.2.0 > 1.2.0-rc1).
		return 1
	case bPre == "":
		return -1
	default:
		return comparePrerelease(aPre, bPre)
	}
}

// comparePrerelease orders two non-empty pre-release remainders by SemVer
// identifier precedence: it compares dot-separated identifiers left to right,
// numerically when both parse as integers, lexically when both are alphanumeric,
// and ranks a numeric identifier below an alphanumeric one; when every shared
// identifier ties, the remainder carrying more identifiers outranks the shorter
// (1.0.0-rc.2 < 1.0.0-rc.10 < 1.0.0-rc.10.1).
func comparePrerelease(a, b string) int {
	as := strings.Split(a, ".")
	bs := strings.Split(b, ".")
	n := min(len(as), len(bs))

	for i := range n {
		if c := comparePrereleaseID(as[i], bs[i]); c != 0 {
			return c
		}
	}

	return len(as) - len(bs)
}

// comparePrereleaseID orders two pre-release identifiers: both numeric compare
// numerically, both alphanumeric compare lexically, and a numeric identifier
// ranks below an alphanumeric one.
func comparePrereleaseID(a, b string) int {
	an, aErr := strconv.Atoi(a)
	bn, bErr := strconv.Atoi(b)

	switch {
	case aErr == nil && bErr == nil:
		return an - bn
	case aErr == nil:
		// A numeric identifier has lower precedence than an alphanumeric one.
		return -1
	case bErr == nil:
		return 1
	default:
		return strings.Compare(a, b)
	}
}

// splitPrerelease separates a version's numeric release core from its
// pre-release remainder at the first "-", returning the whole string as the core
// when it carries no pre-release.
func splitPrerelease(v string) (string, string) {
	core, prerelease, _ := strings.Cut(v, "-")

	return core, prerelease
}

// compareCore orders two dotted release cores segment by segment, comparing each
// segment numerically when both parse as integers and lexically otherwise.
func compareCore(a, b string) int {
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

// segmentAt returns the i-th segment of parts, or "0" when i is out of range, so
// a version with fewer segments compares as if padded with explicit zeros (1.2
// equals 1.2.0) rather than an empty segment sorting below a "0" it should equal.
func segmentAt(parts []string, i int) string {
	if i < len(parts) {
		return parts[i]
	}

	return "0"
}
