package store

import (
	"path"
	"strings"
	"time"

	"go.jacobcolvin.com/hcp_archiver/pathkit"
)

// timeLayout formats a timestamp into the lexically-sortable, filesystem-safe
// stamp that orders state-version filenames. It carries no colons and, paired
// with a literal "Z", denotes UTC.
const timeLayout = "20060102T150405"

// Path-name segments the collector writes and that the sweeps and readers
// match on, so the names have one owner.
const (
	// BundlesDirName is the workspace subdirectory holding sealed cold
	// bundles and their sidecar indexes (see [Store.BundleDir]).
	BundlesDirName = "bundles"

	// ConfigVersionsDirName is the org-level directory holding the
	// deduplicated configuration-version tarballs (see
	// [Store.ConfigVersionTarball]).
	ConfigVersionsDirName = "config-versions"

	// RollupsDirName is the workspace subdirectory holding NDJSON roll-ups
	// (see [Store.RollupDir]).
	RollupsDirName = "rollups"

	// IdentityFileName is the sidecar, kept beside a name-keyed directory's
	// archived objects, that binds the directory to the id of the object it
	// archives. The dot prefix keeps it out of the way of the browsable tree,
	// matching the co-located .ledger directory. The collector writes it; the
	// readers filter it out of listings as the archive's own bookkeeping.
	IdentityFileName = ".identity.json"
)

// InSealedForm reports whether an archive-relative path lies in a workspace's
// sealed-form machinery: the [BundlesDirName] or [RollupsDirName] directory
// sitting directly under a workspace level
// (projects/<project>/workspaces/<workspace>/). The match is positional rather
// than by segment name alone, so a project or workspace whose own display name
// collides with the directory names is never mistaken for machinery.
func InSealedForm(relPath string) bool {
	const sealedIdx = 4 // projects/<project>/workspaces/<workspace>/<sealed>

	segs := strings.Split(relPath, "/")

	return len(segs) > sealedIdx && segs[0] == "projects" && segs[2] == "workspaces" &&
		(segs[sealedIdx] == BundlesDirName || segs[sealedIdx] == RollupsDirName)
}

// Join composes archive-relative segments into a single clean forward-slash
// path, confining the result to the root so no ".." can escape.
//
// It is the safe primitive the named builders rest on; callers that need a leaf
// the builders do not name can compose one against a builder's directory.
func (s *Store) Join(elem ...string) string {
	return cleanJoin(elem...)
}

// Org returns the relative path of the organization metadata file.
func (s *Store) Org() string {
	return cleanJoin("org.json")
}

// Memberships returns the relative path of the organization roster file.
func (s *Store) Memberships() string {
	return cleanJoin("memberships.json")
}

// User returns the relative path of a user's metadata file, keyed on the user
// id.
//
// A run's created-by, a run event's actor, a team's members, and each
// membership's user are hydrated User sub-objects; go-tfe exposes no user list,
// so archiving each one here is the only path to capturing who acted, keyed by
// id so every one of those references resolves to a single file.
func (s *Store) User(id string) string {
	return cleanJoin("users", seg(id)+".json")
}

// GitHubAppInstallations returns the relative path of the GitHub App VCS
// installations file.
func (s *Store) GitHubAppInstallations() string {
	return cleanJoin("github-app-installations.json")
}

// RunTasks returns the relative path of the organization run-task definitions
// file.
func (s *Store) RunTasks() string {
	return cleanJoin("run-tasks.json")
}

// TokenTTLPolicies returns the relative path of the organization token-TTL
// governance file.
func (s *Store) TokenTTLPolicies() string {
	return cleanJoin("token-ttl-policies.json")
}

// ReservedTagKeys returns the relative path of the organization reserved
// tag-keys file.
func (s *Store) ReservedTagKeys() string {
	return cleanJoin("reserved-tag-keys.json")
}

// TeamFile returns the relative path of a leaf named name under a team's
// directory (e.g. team.json, notification-configs.json).
func (s *Store) TeamFile(id, name string) string {
	return cleanJoin("teams", seg(id), seg(name))
}

// OAuthClientFile returns the relative path of a leaf named name under a VCS
// OAuth client's directory (e.g. oauth-client.json).
func (s *Store) OAuthClientFile(id, name string) string {
	return cleanJoin("oauth-clients", seg(id), seg(name))
}

// OAuthTokenFile returns the relative path of a VCS OAuth token's metadata file
// under its client's tokens directory, keyed on the token id.
//
// The token carries no secret (only its uid, creation time, and service-provider
// user); a dedicated builder is required because [seg] collapses "/", so the
// multi-segment tokens/<id> leaf cannot be composed against the client directory.
func (s *Store) OAuthTokenFile(id, tokenID string) string {
	return cleanJoin("oauth-clients", seg(id), "tokens", seg(tokenID)+".json")
}

// VariableSetFile returns the relative path of a leaf named name under a
// variable set's directory (e.g. variable-set.json, variables.json).
func (s *Store) VariableSetFile(id, name string) string {
	return cleanJoin("variable-sets", seg(id), seg(name))
}

// PolicySetFile returns the relative path of a leaf named name under a policy
// set's directory (e.g. policy-set.json, parameters.json).
func (s *Store) PolicySetFile(id, name string) string {
	return cleanJoin("policy-sets", seg(id), seg(name))
}

// Policy returns the relative path of a policy artifact keyed on its id, with
// ext selecting the metadata (json) or the Sentinel/OPA source file.
func (s *Store) Policy(id, ext string) string {
	return cleanJoin("policies", seg(id)+"."+seg(ext))
}

// AgentPool returns the relative path of an agent pool file, keyed on its id.
func (s *Store) AgentPool(id string) string {
	return cleanJoin("agent-pools", seg(id)+".json")
}

// AuditTrailDir returns the directory that holds audit-trail objects.
func (s *Store) AuditTrailDir() string {
	return cleanJoin("audit-trails")
}

// AuditTrailFile returns the relative path of a leaf named name under the
// audit-trail directory (e.g. config.json, a page file).
func (s *Store) AuditTrailFile(name string) string {
	return cleanJoin("audit-trails", seg(name))
}

// HYOKConfigurationFile returns the relative path of a leaf named name under a
// HYOK configuration's directory (e.g. hyok-configuration.json,
// oidc-configuration.json).
func (s *Store) HYOKConfigurationFile(id, name string) string {
	return cleanJoin("hyok-configurations", seg(id), seg(name))
}

// HYOKKeyVersionFile returns the relative path of a HYOK customer key version's
// file under its configuration's key-versions directory, keyed on the key
// version id.
//
// A dedicated builder is required because [seg] collapses "/", so the
// multi-segment key-versions/<id> leaf cannot be composed against the
// configuration directory.
func (s *Store) HYOKKeyVersionFile(id, kvID string) string {
	return cleanJoin("hyok-configurations", seg(id), "key-versions", seg(kvID)+".json")
}

// RegistryModuleFile returns the relative path of a leaf named file under a
// registry module's directory (e.g. module.json).
//
// The registry name ("private" or "public") is a path level because the API
// keys module identity on it: a private module's namespace is the org name,
// and when that equals a public namespace the org curates, the same
// namespace/name/provider tuple names two distinct modules. Each of namespace,
// name, and provider is likewise its own path level rather than one
// hyphen-joined segment because all three legally contain hyphens, so joining
// them would alias distinct modules onto one path (and one ledger entry); seg
// maps "/" to "_", so the nesting is injective.
func (s *Store) RegistryModuleFile(registryName, namespace, name, provider, file string) string {
	return cleanJoin(
		"registry",
		"modules",
		seg(registryName),
		seg(namespace),
		seg(name),
		seg(provider),
		seg(file),
	)
}

// RegistryNoCodeModule returns the relative path of a no-code module file,
// keyed on its id.
func (s *Store) RegistryNoCodeModule(id string) string {
	return cleanJoin("registry", "no-code-modules", seg(id)+".json")
}

// RegistryNoCodeModuleVariables returns the relative path of a no-code module's
// variable-options sidecar, keyed on its id.
//
// It lives under a sibling directory of the module files, not beside them, so a
// module whose id ends in "-variables" can never alias another module's sidecar:
// [Store.RegistryNoCodeModule] only ever writes under "no-code-modules", never
// "no-code-module-variables".
func (s *Store) RegistryNoCodeModuleVariables(id string) string {
	return cleanJoin("registry", "no-code-module-variables", seg(id)+".json")
}

// RegistryProviderFile returns the relative path of a leaf named file under a
// registry provider's directory (e.g. provider.json).
//
// The registry name ("private" or "public") is a path level because the API
// keys provider identity on it, so an org's private provider and a curated
// public one sharing the namespace/name tuple stay distinct. Namespace and
// name are separate path levels so two providers whose hyphenated namespace
// and name would otherwise join to the same segment stay distinct.
func (s *Store) RegistryProviderFile(registryName, namespace, name, file string) string {
	return cleanJoin(
		"registry",
		"providers",
		seg(registryName),
		seg(namespace),
		seg(name),
		seg(file),
	)
}

// RegistryGPGKey returns the relative path of a registry GPG key file, nesting
// its namespace and key id as directory levels so a hyphenated namespace and
// key id cannot alias onto one path.
func (s *Store) RegistryGPGKey(namespace, keyID string) string {
	return cleanJoin("registry", "gpg-keys", seg(namespace), seg(keyID)+".json")
}

// ConfigVersionTarball returns the relative path of a configuration version
// tarball in the org-wide deduplicated directory, keyed on its globally-unique
// id.
func (s *Store) ConfigVersionTarball(cvID string) string {
	return cleanJoin(ConfigVersionsDirName, seg(cvID)+".tar.gz")
}

// ProjectDir returns the directory that holds a project's archived objects and
// its nested workspaces and stacks, keyed on the project name.
func (s *Store) ProjectDir(project string) string {
	return cleanJoin("projects", seg(project))
}

// ProjectFile returns the relative path of a leaf named name under a project's
// directory (e.g. project.json, notification-configs.json).
func (s *Store) ProjectFile(project, name string) string {
	return cleanJoin("projects", seg(project), seg(name))
}

// WorkspaceDir returns the directory that holds a workspace's archived objects,
// nested beneath its project and keyed on the workspace name.
func (s *Store) WorkspaceDir(project, ws string) string {
	return cleanJoin("projects", seg(project), "workspaces", seg(ws))
}

// WorkspaceFile returns the relative path of a leaf named name under a
// workspace's directory (e.g. workspace.json, variables.json, readme.md).
func (s *Store) WorkspaceFile(project, ws, name string) string {
	return cleanJoin("projects", seg(project), "workspaces", seg(ws), seg(name))
}

// BundleDir returns the directory that holds a workspace's sealed cold bundles
// and their sidecar indexes, a sibling of its runs and state-versions.
func (s *Store) BundleDir(project, ws string) string {
	return cleanJoin("projects", seg(project), "workspaces", seg(ws), BundlesDirName)
}

// RollupDir returns the directory that holds a workspace's NDJSON roll-ups, the
// coalesced form of its frozen immutable per-run and per-state-version metadata.
func (s *Store) RollupDir(project, ws string) string {
	return cleanJoin("projects", seg(project), "workspaces", seg(ws), RollupsDirName)
}

// StateVersionDir returns the directory that holds a workspace's state-version
// files.
func (s *Store) StateVersionDir(project, ws string) string {
	return cleanJoin("projects", seg(project), "workspaces", seg(ws), "state-versions")
}

// StateVersionStem returns the shared filename stem for a state version:
// <created-at>-<id>, formatting createdAt as a lexically-sortable UTC stamp so
// state versions order by creation time rather than by their non-unique,
// non-zero-padded serial.
func (s *Store) StateVersionStem(createdAt time.Time, id string) string {
	stamp := createdAt.UTC().Format(timeLayout) + "Z"

	return stamp + "-" + seg(id)
}

// StateVersionFile returns the relative path of a state-version leaf, joining
// the [Store.StateVersionStem] with suffix (e.g. tfstate.json, json,
// meta.json).
func (s *Store) StateVersionFile(project, ws string, createdAt time.Time, id, suffix string) string {
	return cleanJoin(
		"projects",
		seg(project),
		"workspaces",
		seg(ws),
		"state-versions",
		s.StateVersionStem(createdAt, id)+"."+seg(suffix),
	)
}

// RunDir returns the directory that holds a run's archived objects, keyed on
// the run id beneath its workspace.
func (s *Store) RunDir(project, ws, runID string) string {
	return cleanJoin("projects", seg(project), "workspaces", seg(ws), "runs", seg(runID))
}

// RunFile returns the relative path of a leaf named name under a run's
// directory (e.g. run.json, plan.log, apply.log).
func (s *Store) RunFile(project, ws, runID, name string) string {
	return cleanJoin(
		"projects",
		seg(project),
		"workspaces",
		seg(ws),
		"runs",
		seg(runID),
		seg(name),
	)
}

// StackDir returns the directory that holds a stack's archived objects, nested
// beneath its project and keyed on the stack name.
func (s *Store) StackDir(project, stack string) string {
	return cleanJoin("projects", seg(project), "stacks", seg(stack))
}

// StackFile returns the relative path of a leaf named name directly under a
// stack's directory (e.g. stack.json).
func (s *Store) StackFile(project, stack, name string) string {
	return cleanJoin("projects", seg(project), "stacks", seg(stack), seg(name))
}

// StackConfigurationFile returns the relative path of a leaf named name under a
// stack configuration's directory (e.g. configuration.json).
func (s *Store) StackConfigurationFile(project, stack, configID, name string) string {
	return cleanJoin(
		"projects",
		seg(project),
		"stacks",
		seg(stack),
		"configurations",
		seg(configID),
		seg(name),
	)
}

// StackDeploymentGroupDir returns the directory for a deployment group under a
// stack configuration, keyed on the group id.
func (s *Store) StackDeploymentGroupDir(project, stack, configID, groupID string) string {
	return cleanJoin(
		"projects",
		seg(project),
		"stacks",
		seg(stack),
		"configurations",
		seg(configID),
		"deployment-groups",
		seg(groupID),
	)
}

// StackDeploymentGroupFile returns the relative path of a leaf named name under
// a deployment group's directory (e.g. group.json).
func (s *Store) StackDeploymentGroupFile(project, stack, configID, groupID, name string) string {
	return cleanJoin(
		"projects",
		seg(project),
		"stacks",
		seg(stack),
		"configurations",
		seg(configID),
		"deployment-groups",
		seg(groupID),
		seg(name),
	)
}

// StackRunFile returns the relative path of a leaf named name under a stack
// run's directory (e.g. run.json).
func (s *Store) StackRunFile(project, stack, configID, groupID, runID, name string) string {
	return cleanJoin(
		"projects",
		seg(project),
		"stacks",
		seg(stack),
		"configurations",
		seg(configID),
		"deployment-groups",
		seg(groupID),
		"runs",
		seg(runID),
		seg(name),
	)
}

// StackStepFile returns the relative path of a leaf named name under a stack
// step's directory (per-step plan/apply artifacts and diagnostics).
//
//nolint:gocritic // The step path is genuinely this deep; each id names a level.
func (s *Store) StackStepFile(project, stack, configID, groupID, runID, stepID, name string) string {
	return cleanJoin(
		"projects",
		seg(project),
		"stacks",
		seg(stack),
		"configurations",
		seg(configID),
		"deployment-groups",
		seg(groupID),
		"runs",
		seg(runID),
		"steps",
		seg(stepID),
		seg(name),
	)
}

// StackDeploymentFile returns the relative path of a leaf named file under a
// named deployment's directory (e.g. deployment.json).
func (s *Store) StackDeploymentFile(project, stack, name, file string) string {
	return cleanJoin(
		"projects",
		seg(project),
		"stacks",
		seg(stack),
		"deployments",
		seg(name),
		seg(file),
	)
}

// StackStateFile returns the relative path of a stack state file, keyed on the
// deployment and the state generation.
//
// Generation counters are per-deployment, not stack-global, so two of a stack's
// deployments can each carry a state at the same generation; the deployment
// name disambiguates them. A deployment that is empty falls back to keying on
// the generation alone.
func (s *Store) StackStateFile(project, stack, deployment, generation string) string {
	stem := seg(generation)
	if deployment != "" {
		stem = seg(deployment) + "-" + seg(generation)
	}

	return cleanJoin(
		"projects",
		seg(project),
		"stacks",
		seg(stack),
		"states",
		stem+".json",
	)
}

// cleanJoin joins already-sanitized segments into a clean forward-slash path
// confined beneath the root (see [pathkit.ConfineSlash]), so any residual
// ".." collapses at the root rather than escaping it.
func cleanJoin(elem ...string) string {
	return pathkit.ConfineSlash(path.Join(elem...))
}

// seg neutralizes a single untrusted path segment: path separators and control
// characters become underscores so the segment cannot introduce a new level or
// smuggle terminal escapes, and a segment that is empty or made only of dots
// (including "." and "..") collapses to a single underscore so it can never act
// as a traversal.
func seg(name string) string {
	var b strings.Builder

	b.Grow(len(name))

	for _, r := range name {
		switch {
		case r == '/' || r == '\\':
			b.WriteByte('_')
		case r < 0x20 || r == 0x7f:
			b.WriteByte('_')
		default:
			b.WriteRune(r)
		}
	}

	out := b.String()
	if strings.Trim(out, ".") == "" {
		return "_"
	}

	return out
}
