package store_test

import (
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.jacobcolvin.com/hcp_archiver/store"
)

func TestStore_pathBuilders(t *testing.T) {
	t.Parallel()

	createdAt := time.Date(2023, time.March, 4, 5, 6, 7, 0, time.UTC)

	tests := map[string]struct {
		build func(s *store.Store) string
		want  string
	}{
		"org": {
			build: func(s *store.Store) string { return s.Org() },
			want:  "org.json",
		},
		"memberships": {
			build: func(s *store.Store) string { return s.Memberships() },
			want:  "memberships.json",
		},
		"github app installations": {
			build: func(s *store.Store) string { return s.GitHubAppInstallations() },
			want:  "github-app-installations.json",
		},
		"team file": {
			build: func(s *store.Store) string { return s.TeamFile("team-abc", "team.json") },
			want:  "teams/team-abc/team.json",
		},
		"oauth client file": {
			build: func(s *store.Store) string { return s.OAuthClientFile("oc-xyz", "oauth-client.json") },
			want:  "oauth-clients/oc-xyz/oauth-client.json",
		},
		"oauth token file": {
			build: func(s *store.Store) string { return s.OAuthTokenFile("oc-xyz", "ot-1") },
			want:  "oauth-clients/oc-xyz/tokens/ot-1.json",
		},
		"variable set file": {
			build: func(s *store.Store) string { return s.VariableSetFile("vs-1", "variables.json") },
			want:  "variable-sets/vs-1/variables.json",
		},
		"policy set file": {
			build: func(s *store.Store) string { return s.PolicySetFile("ps-1", "parameters.json") },
			want:  "policy-sets/ps-1/parameters.json",
		},
		"policy source": {
			build: func(s *store.Store) string { return s.Policy("pol-1", "sentinel") },
			want:  "policies/pol-1.sentinel",
		},
		"agent pool": {
			build: func(s *store.Store) string { return s.AgentPool("apool-1") },
			want:  "agent-pools/apool-1.json",
		},
		"audit trail file": {
			build: func(s *store.Store) string { return s.AuditTrailFile("config.json") },
			want:  "audit-trails/config.json",
		},
		"hyok configuration file": {
			build: func(s *store.Store) string { return s.HYOKConfigurationFile("hyok-1", "hyok-configuration.json") },
			want:  "hyok-configurations/hyok-1/hyok-configuration.json",
		},
		"hyok key version file": {
			build: func(s *store.Store) string { return s.HYOKKeyVersionFile("hyok-1", "kv-1") },
			want:  "hyok-configurations/hyok-1/key-versions/kv-1.json",
		},
		"user": {
			build: func(s *store.Store) string { return s.User("user-1") },
			want:  "users/user-1.json",
		},
		"registry module file": {
			build: func(s *store.Store) string { return s.RegistryModuleFile("ns", "vpc", "aws", "module.json") },
			want:  "registry/modules/ns/vpc/aws/module.json",
		},
		"registry no-code module": {
			build: func(s *store.Store) string { return s.RegistryNoCodeModule("nocode-1") },
			want:  "registry/no-code-modules/nocode-1.json",
		},
		"registry no-code module variables": {
			build: func(s *store.Store) string { return s.RegistryNoCodeModuleVariables("nocode-1") },
			want:  "registry/no-code-module-variables/nocode-1.json",
		},
		"registry provider file": {
			build: func(s *store.Store) string { return s.RegistryProviderFile("ns", "aws", "provider.json") },
			want:  "registry/providers/ns/aws/provider.json",
		},
		"registry gpg key": {
			build: func(s *store.Store) string { return s.RegistryGPGKey("ns", "ABCD1234") },
			want:  "registry/gpg-keys/ns/ABCD1234.json",
		},
		"config version tarball": {
			build: func(s *store.Store) string { return s.ConfigVersionTarball("cv-1") },
			want:  "config-versions/cv-1.tar.gz",
		},
		"project file": {
			build: func(s *store.Store) string { return s.ProjectFile("Default Project", "project.json") },
			want:  "projects/Default Project/project.json",
		},
		"workspace file": {
			build: func(s *store.Store) string { return s.WorkspaceFile("proj", "ws", "workspace.json") },
			want:  "projects/proj/workspaces/ws/workspace.json",
		},
		"state version dir": {
			build: func(s *store.Store) string { return s.StateVersionDir("proj", "ws") },
			want:  "projects/proj/workspaces/ws/state-versions",
		},
		"state version file": {
			build: func(s *store.Store) string {
				return s.StateVersionFile("proj", "ws", createdAt, "sv-1", "tfstate.json")
			},
			want: "projects/proj/workspaces/ws/state-versions/20230304T050607Z-sv-1.tfstate.json",
		},
		"run file": {
			build: func(s *store.Store) string { return s.RunFile("proj", "ws", "run-1", "plan.log") },
			want:  "projects/proj/workspaces/ws/runs/run-1/plan.log",
		},
		"stack file": {
			build: func(s *store.Store) string { return s.StackFile("proj", "mystack", "stack.json") },
			want:  "projects/proj/stacks/mystack/stack.json",
		},
		"stack configuration file": {
			build: func(s *store.Store) string {
				return s.StackConfigurationFile("proj", "mystack", "cfg-1", "configuration.json")
			},
			want: "projects/proj/stacks/mystack/configurations/cfg-1/configuration.json",
		},
		"stack deployment group file": {
			build: func(s *store.Store) string {
				return s.StackDeploymentGroupFile("proj", "mystack", "cfg-1", "grp-1", "group.json")
			},
			want: "projects/proj/stacks/mystack/configurations/cfg-1/deployment-groups/grp-1/group.json",
		},
		"stack run file": {
			build: func(s *store.Store) string {
				return s.StackRunFile("proj", "mystack", "cfg-1", "grp-1", "run-1", "run.json")
			},
			want: "projects/proj/stacks/mystack/configurations/cfg-1/deployment-groups/grp-1/runs/run-1/run.json",
		},
		"stack step file": {
			build: func(s *store.Store) string {
				return s.StackStepFile("proj", "mystack", "cfg-1", "grp-1", "run-1", "step-1", "plan.json")
			},
			//nolint:lll // The documented path is genuinely this deep.
			want: "projects/proj/stacks/mystack/configurations/cfg-1/deployment-groups/grp-1/runs/run-1/steps/step-1/plan.json",
		},
		"stack deployment file": {
			build: func(s *store.Store) string {
				return s.StackDeploymentFile("proj", "mystack", "prod", "deployment.json")
			},
			want: "projects/proj/stacks/mystack/deployments/prod/deployment.json",
		},
		"stack state file": {
			build: func(s *store.Store) string { return s.StackStateFile("proj", "mystack", "prod", "42") },
			want:  "projects/proj/stacks/mystack/states/prod-42.json",
		},
		"stack state file without deployment": {
			build: func(s *store.Store) string { return s.StackStateFile("proj", "mystack", "", "42") },
			want:  "projects/proj/stacks/mystack/states/42.json",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			s := store.New(t.TempDir())
			assert.Equal(t, tc.want, tc.build(s))
		})
	}
}

func TestStore_registryPathsDoNotCollide(t *testing.T) {
	t.Parallel()

	s := store.New(t.TempDir())

	// A namespace, name, and provider all legally contain hyphens, so joining them
	// with a hyphen aliased distinct keys onto one path and one ledger entry.
	// Nesting each as its own level keeps the two ambiguous tuples distinct.
	tests := map[string]struct {
		a string
		b string
	}{
		"module file": {
			a: s.RegistryModuleFile("foo-bar", "baz", "aws", "module.json"),
			b: s.RegistryModuleFile("foo", "bar-baz", "aws", "module.json"),
		},
		"provider file": {
			a: s.RegistryProviderFile("foo-bar", "baz", "provider.json"),
			b: s.RegistryProviderFile("foo", "bar-baz", "provider.json"),
		},
		"gpg key": {
			a: s.RegistryGPGKey("foo-bar", "baz"),
			b: s.RegistryGPGKey("foo", "bar-baz"),
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			assert.NotEqual(t, tc.a, tc.b, "distinct registry keys must map to distinct paths")
		})
	}
}

func TestStore_stateVersionStemSortsByCreationTime(t *testing.T) {
	t.Parallel()

	s := store.New(t.TempDir())

	// A later creation time paired with a lexically-smaller id and a rolled-back
	// serial must still sort after an earlier one: ordering keys on the stamp.
	early := s.StateVersionStem(time.Date(2023, time.January, 1, 0, 0, 0, 0, time.UTC), "sv-9")
	late := s.StateVersionStem(time.Date(2024, time.January, 1, 0, 0, 0, 0, time.UTC), "sv-1")

	stems := []string{late, early}
	sort.Strings(stems)

	assert.Equal(t, []string{early, late}, stems, "stems must sort by creation time")
	assert.NotContains(t, early, ":", "stamp must be filesystem-safe")
}

func TestStore_stateVersionStemNormalizesToUTC(t *testing.T) {
	t.Parallel()

	s := store.New(t.TempDir())

	loc := time.FixedZone("UTC+5", 5*60*60)
	local := time.Date(2023, time.March, 4, 10, 6, 7, 0, loc)
	// 10:06:07 at UTC+5 is 05:06:07 UTC.
	assert.Equal(t, "20230304T050607Z-sv-1", s.StateVersionStem(local, "sv-1"))
}

func TestStore_sanitizationConfinesToRoot(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	s := store.New(root)

	hostile := []string{
		s.WorkspaceDir("../..", "../../../etc/passwd"),
		s.WorkspaceFile("..", "..", "../../secret"),
		s.RunFile("p", "ws", "../../../../../root", ".ssh/authorized_keys"),
		s.StateVersionFile("p", "..", time.Unix(0, 0).UTC(), "../../id", "../../../evil"),
		s.Join("..", "..", "..", "etc", "passwd"),
		s.Join("/absolute/escape"),
		s.OAuthClientFile("../../escape", "../../oauth-client.json"),
		s.OAuthTokenFile("../../escape", "../../ot"),
		s.HYOKKeyVersionFile("../../escape", "../../kv"),
		s.User("../../escape"),
		s.RegistryModuleFile("../../../etc", "..", "../..", "passwd"),
		s.RegistryProviderFile("../../..", "../../etc", "provider.json"),
		s.RegistryGPGKey("..", "../../../root/.ssh/authorized_keys"),
	}

	for _, rel := range hostile {
		assert.False(t, strings.HasPrefix(rel, "/"), "relative path must not be absolute: %q", rel)
		assert.False(
			t,
			strings.HasPrefix(rel, ".."),
			"relative path must not climb out of root: %q",
			rel,
		)

		abs := s.AbsPath(rel)

		within, err := filepath.Rel(root, abs)
		require.NoError(t, err)
		assert.False(
			t,
			within == ".." || strings.HasPrefix(within, ".."+string(filepath.Separator)),
			"absolute path %q escaped root %q (rel %q)",
			abs,
			root,
			within,
		)
	}
}

func TestStore_segCollapsesDegenerateNames(t *testing.T) {
	t.Parallel()

	s := store.New(t.TempDir())

	// A name of only dots or empty collapses so it cannot act as "." or "..".
	assert.Equal(t, "projects/_/project.json", s.ProjectFile("..", "project.json"))
	assert.Equal(t, "projects/_/project.json", s.ProjectFile("", "project.json"))
	assert.Equal(t, "projects/_/project.json", s.ProjectFile(".", "project.json"))
}
