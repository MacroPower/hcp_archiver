package store

import (
	"path"
	"strings"
	"time"
)

// timeLayout formats a timestamp into the lexically-sortable, filesystem-safe
// stamp that orders state-version filenames. It carries no colons and, paired
// with a literal "Z", denotes UTC.
const timeLayout = "20060102T150405"

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

// TeamDir returns the directory that holds a team's archived objects, keyed on
// the team id.
func (s *Store) TeamDir(id string) string {
	return cleanJoin("teams", seg(id))
}

// TeamFile returns the relative path of a leaf named name under a team's
// directory (e.g. team.json, notification-configs.json).
func (s *Store) TeamFile(id, name string) string {
	return cleanJoin("teams", seg(id), seg(name))
}

// OAuthClient returns the relative path of a VCS OAuth client file, keyed on
// its id.
func (s *Store) OAuthClient(id string) string {
	return cleanJoin("oauth-clients", seg(id)+".json")
}

// VariableSetDir returns the directory that holds a variable set's archived
// objects, keyed on its id.
func (s *Store) VariableSetDir(id string) string {
	return cleanJoin("variable-sets", seg(id))
}

// VariableSetFile returns the relative path of a leaf named name under a
// variable set's directory (e.g. variable-set.json, variables.json).
func (s *Store) VariableSetFile(id, name string) string {
	return cleanJoin("variable-sets", seg(id), seg(name))
}

// PolicySetDir returns the directory that holds a policy set's archived
// objects, keyed on its id.
func (s *Store) PolicySetDir(id string) string {
	return cleanJoin("policy-sets", seg(id))
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

// HYOKConfiguration returns the relative path of a HYOK configuration file,
// keyed on its id.
func (s *Store) HYOKConfiguration(id string) string {
	return cleanJoin("hyok-configurations", seg(id)+".json")
}

// RegistryModuleDir returns the directory for a registry module, nesting its
// namespace, name, and provider as directory levels.
//
// Each is its own level rather than one hyphen-joined segment because all three
// legally contain hyphens, so joining them would alias distinct modules onto one
// path (and one ledger entry); seg maps "/" to "_", so the nesting is injective.
func (s *Store) RegistryModuleDir(namespace, name, provider string) string {
	return cleanJoin("registry", "modules", seg(namespace), seg(name), seg(provider))
}

// RegistryModuleFile returns the relative path of a leaf named file under a
// registry module's directory (e.g. module.json).
func (s *Store) RegistryModuleFile(namespace, name, provider, file string) string {
	return cleanJoin(
		"registry",
		"modules",
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

// RegistryProviderDir returns the directory for a registry provider, nesting its
// namespace and name as directory levels so two providers whose hyphenated
// namespace and name would otherwise join to the same segment stay distinct.
func (s *Store) RegistryProviderDir(namespace, name string) string {
	return cleanJoin("registry", "providers", seg(namespace), seg(name))
}

// RegistryProviderFile returns the relative path of a leaf named file under a
// registry provider's directory (e.g. provider.json).
func (s *Store) RegistryProviderFile(namespace, name, file string) string {
	return cleanJoin("registry", "providers", seg(namespace), seg(name), seg(file))
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
	return cleanJoin("config-versions", seg(cvID)+".tar.gz")
}

// ProjectDir returns the directory that holds a project's archived objects,
// keyed on the project name (sanitized so a hostile name cannot escape root).
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
	return cleanJoin("projects", seg(project), "workspaces", seg(ws), "bundles")
}

// RollupDir returns the directory that holds a workspace's NDJSON roll-ups, the
// coalesced form of its frozen immutable per-run and per-state-version metadata.
func (s *Store) RollupDir(project, ws string) string {
	return cleanJoin("projects", seg(project), "workspaces", seg(ws), "rollups")
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

// StackConfigurationDir returns the directory for a stack configuration, keyed
// on its id.
func (s *Store) StackConfigurationDir(project, stack, configID string) string {
	return cleanJoin(
		"projects",
		seg(project),
		"stacks",
		seg(stack),
		"configurations",
		seg(configID),
	)
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

// StackRunDir returns the directory for a run under a deployment group, keyed
// on the run id.
func (s *Store) StackRunDir(project, stack, configID, groupID, runID string) string {
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

// StackStepDir returns the directory for a step under a stack run, keyed on the
// step id.
func (s *Store) StackStepDir(project, stack, configID, groupID, runID, stepID string) string {
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

// StackDeploymentDir returns the directory for a named stack deployment, keyed
// on the deployment name.
func (s *Store) StackDeploymentDir(project, stack, name string) string {
	return cleanJoin(
		"projects",
		seg(project),
		"stacks",
		seg(stack),
		"deployments",
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
// and confines it beneath the root by resolving the join as if absolute, so any
// residual ".." collapses at the root rather than escaping it.
func cleanJoin(elem ...string) string {
	joined := path.Join(elem...)

	return strings.TrimPrefix(path.Clean("/"+joined), "/")
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
