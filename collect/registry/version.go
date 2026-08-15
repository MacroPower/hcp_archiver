package registry

import (
	"github.com/hashicorp/go-tfe/v2"

	goversion "github.com/hashicorp/go-version"
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

// isConcreteVersion reports whether v names a specific version rather than the
// floating "latest" pin or an empty value, so it can address a per-version read.
func isConcreteVersion(v string) bool {
	return v != "" && v != versionLatest
}

// resolveNoCodeVersion picks the concrete module version whose no-code variable
// options should be read: the pin itself when it is concrete, otherwise the
// newest concrete ok version among the module's version statuses. It returns
// the empty string when no concrete version can be resolved.
func resolveNoCodeVersion(pin string, statuses []tfe.RegistryModuleVersionStatuses) string {
	if isConcreteVersion(pin) {
		return pin
	}

	return latestModuleVersion(statuses)
}

// latestModuleVersion returns the newest stable concrete ok version among
// statuses, falling back to the newest prerelease only when no stable version
// exists, and the empty string when none qualifies. The registry serves a
// "latest" pin as the newest stable ok version, so neither a prerelease nor a
// version that failed or has not finished ingress must shadow it when resolving
// the version whose no-code variable options to archive: a per-version read
// against a never-ingressed version would settle its record absent while the
// version the registry actually serves goes unarchived.
//
// Precedence comes from [github.com/hashicorp/go-version], the library behind
// HashiCorp's own registries, rather than a hand-rolled comparator, so the
// ordering here cannot drift from the ordering the data source itself applies.
// A version the parser rejects is kept only as a last resort when nothing
// parses, so one malformed status never outranks a real release yet a registry
// serving only surprising strings still resolves to something archivable.
func latestModuleVersion(statuses []tfe.RegistryModuleVersionStatuses) string {
	var bestStable, bestAny *goversion.Version

	bestStableRaw, bestAnyRaw, fallback := "", "", ""

	for _, s := range statuses {
		if s.Status != tfe.RegistryModuleVersionStatusOk || !isConcreteVersion(s.Version) {
			continue
		}

		v, err := goversion.NewVersion(s.Version)
		if err != nil {
			if fallback == "" {
				fallback = s.Version
			}

			continue
		}

		if bestAny == nil || v.GreaterThan(bestAny) {
			bestAny, bestAnyRaw = v, s.Version
		}

		if v.Prerelease() == "" && (bestStable == nil || v.GreaterThan(bestStable)) {
			bestStable, bestStableRaw = v, s.Version
		}
	}

	// The raw string as served is returned, not the parsed form's rendering: it
	// keys the per-version API read, which must address the version by the exact
	// spelling the registry listed.
	switch {
	case bestStable != nil:
		return bestStableRaw
	case bestAny != nil:
		return bestAnyRaw
	default:
		return fallback
	}
}
