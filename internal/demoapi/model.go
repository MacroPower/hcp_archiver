package demoapi

import (
	"fmt"
	"strings"
	"time"

	"github.com/hashicorp/go-tfe"
)

// Back-references are always stubs: a relation hydrated in both directions
// would send the JSON:API marshaler around a cycle, so an object that another
// object already hydrates is referenced by identifier alone. A relation is
// hydrated only where the archiver reads its attributes: team members, run
// creators and event actors, a run's plan and apply, a configuration version's
// ingress attributes, and a project's effective tag bindings.

// orgRef returns a bare reference to the served organization.
func orgRef() *tfe.Organization {
	return &tfe.Organization{Name: orgName}
}

// newOrganization builds the organization record.
func (w *world) newOrganization() *tfe.Organization {
	return &tfe.Organization{
		Name:                       orgName,
		CollaboratorAuthPolicy:     tfe.AuthPolicyPassword,
		CostEstimationEnabled:      true,
		CreatedAt:                  epoch.AddDate(-4, -2, 0),
		DefaultExecutionMode:       remoteExecution,
		Email:                      "platform@example.com",
		ExternalID:                 w.ids.id("org", orgName),
		AllowForceDeleteWorkspaces: true,
		TwoFactorConformant:        true,
		SessionRemember:            20160,
		SessionTimeout:             20160,
		Permissions: &tfe.OrganizationPermissions{
			CanCreateProject:   true,
			CanCreateTeam:      true,
			CanCreateWorkspace: true,
			CanTraverse:        true,
			CanUpdate:          true,
		},
		DefaultProject: &tfe.Project{ID: w.ids.project(projects[0].name)},
	}
}

// newUsers builds the organization's users, in roster order.
func (w *world) newUsers() []*tfe.User {
	users := make([]*tfe.User, 0, len(people))

	for _, p := range people {
		users = append(users, &tfe.User{
			ID:               w.ids.user(p.username),
			Username:         p.username,
			Email:            p.email,
			AvatarURL:        "https://www.gravatar.com/avatar/" + strings.Repeat("0", 32),
			IsServiceAccount: p.username == "terraform-ci",
		})
	}

	return users
}

// newMemberships builds the organization roster, each membership hydrating the
// user it names and referencing that user's teams by identifier.
func (w *world) newMemberships() []*tfe.OrganizationMembership {
	memberships := make([]*tfe.OrganizationMembership, 0, len(people))

	for i, p := range people {
		memberships = append(memberships, &tfe.OrganizationMembership{
			ID:           w.ids.id("ou", p.username),
			Email:        p.email,
			Status:       tfe.OrganizationMembershipActive,
			Organization: orgRef(),
			User:         w.users[i],
			Teams:        w.teamRefsFor(i),
		})
	}

	return memberships
}

// teamMembers returns the roster positions of the users on team number i,
// spread across the roster so the teams overlap the way real ones do.
func teamMembers(i int) []int {
	members := make([]int, 0, teams[i].members)

	for k := range teams[i].members {
		members = append(members, (i+k)%len(people))
	}

	return members
}

// teamRefsFor returns bare references to the teams the user at roster position
// idx belongs to.
func (w *world) teamRefsFor(idx int) []*tfe.Team {
	var refs []*tfe.Team

	for i, t := range teams {
		for _, member := range teamMembers(i) {
			if member == idx {
				refs = append(refs, &tfe.Team{ID: w.ids.team(t.name)})
			}
		}
	}

	return refs
}

// newTeams builds the organization's teams, each hydrating its member users and
// their memberships (the listing the archiver reads them from asks for both).
func (w *world) newTeams() []*tfe.Team {
	built := make([]*tfe.Team, 0, len(teams))

	for i, t := range teams {
		team := &tfe.Team{
			ID:                 w.ids.team(t.name),
			Name:               t.name,
			Visibility:         t.visibility,
			UserCount:          t.members,
			OrganizationAccess: &tfe.OrganizationAccess{ManageWorkspaces: t.name != "data-engineering"},
			Permissions: &tfe.TeamPermissions{
				CanDestroy:          t.name == "owners",
				CanUpdateMembership: true,
			},
		}

		for _, member := range teamMembers(i) {
			team.Users = append(team.Users, w.users[member])
			team.OrganizationMemberships = append(team.OrganizationMemberships, &tfe.OrganizationMembership{
				ID:     w.memberships[member].ID,
				Email:  w.memberships[member].Email,
				Status: tfe.OrganizationMembershipActive,
				User:   w.users[member],
			})
		}

		built = append(built, team)
	}

	return built
}

// newVariableSets builds the organization's variable sets.
func (w *world) newVariableSets() []*tfe.VariableSet {
	return []*tfe.VariableSet{{
		ID:           w.ids.id("varset", "aws-credentials"),
		Name:         "aws-credentials",
		Description:  "Dynamic credentials for every production workspace.",
		Organization: orgRef(),
	}}
}

// newVariableSetVariables builds the variables one variable set carries.
func (w *world) newVariableSetVariables(set *tfe.VariableSet) []*tfe.VariableSetVariable {
	ref := &tfe.VariableSet{ID: set.ID}

	return []*tfe.VariableSetVariable{{
		ID:          w.ids.id("var", set.Name+"/role-arn"),
		Key:         "TFC_AWS_RUN_ROLE_ARN",
		Value:       "arn:aws:iam::123456789012:role/terraform-run",
		Description: "The role every production run assumes into.",
		Category:    tfe.CategoryEnv,
		VariableSet: ref,
	}, {
		ID:          w.ids.id("var", set.Name+"/provider-auth"),
		Key:         "TFC_AWS_PROVIDER_AUTH",
		Value:       "true",
		Description: "Enables dynamic credentials for the AWS provider.",
		Category:    tfe.CategoryEnv,
		VariableSet: ref,
	}}
}

// newRunTasks builds the organization's run-task definitions.
func (w *world) newRunTasks() []*tfe.RunTask {
	return []*tfe.RunTask{{
		ID:           w.ids.id("task", "conftest"),
		Name:         "conftest",
		URL:          "https://policy.jacobcolvin-com.example/hooks/conftest",
		Category:     "task",
		Description:  "Rego policy gate for production plans.",
		Enabled:      true,
		Organization: orgRef(),
	}}
}

// newReservedTagKeys builds the organization's reserved tag-key governance.
func (w *world) newReservedTagKeys() []*tfe.ReservedTagKey {
	return []*tfe.ReservedTagKey{{
		ID:        w.ids.id("rtk", tagKeyEnvironment),
		Key:       tagKeyEnvironment,
		CreatedAt: epoch.AddDate(-1, 0, 0),
		UpdatedAt: epoch.AddDate(-1, 0, 0),
	}}
}

// newPolicies builds the organization's Sentinel policies.
func (w *world) newPolicies() []*tfe.Policy {
	return []*tfe.Policy{{
		ID:               w.ids.id("pol", "mandatory-tags"),
		Name:             "mandatory-tags",
		Kind:             tfe.Sentinel,
		Description:      "Every production resource carries environment and owner tags.",
		EnforcementLevel: tfe.EnforcementSoft,
		PolicySetCount:   1,
		UpdatedAt:        epoch.AddDate(0, -3, 0),
		Organization:     orgRef(),
	}}
}

// newProject builds one project record with its effective tag bindings, which
// the archiver splits into a file of their own.
func (w *world) newProject(spec projectSpec) *tfe.Project {
	id := w.ids.project(spec.name)

	return &tfe.Project{
		ID:                   id,
		Name:                 spec.name,
		Description:          spec.description,
		DefaultExecutionMode: remoteExecution,
		SettingOverwrites:    &tfe.ProjectSettingOverwrites{},
		Organization:         orgRef(),
		EffectiveTagBindings: []*tfe.EffectiveTagBinding{{
			ID:    w.ids.id("etb", spec.name+"/owner"),
			Key:   "owner",
			Value: spec.name,
		}},
	}
}

// newProjectAccess builds the team access one project grants.
func (w *world) newProjectAccess(project *tfe.Project) []*tfe.TeamProjectAccess {
	return []*tfe.TeamProjectAccess{{
		ID:      w.ids.id("tprj", project.Name),
		Access:  tfe.TeamProjectAccessAdmin,
		Team:    &tfe.Team{ID: w.ids.team("platform-engineering")},
		Project: &tfe.Project{ID: project.ID},
	}}
}

// newWorkspace builds one workspace record.
func (w *world) newWorkspace(project string, spec workspaceSpec, model *tfe.Project) *tfe.Workspace {
	env := environmentOf(spec.name)

	return &tfe.Workspace{
		ID:                         w.ids.workspace(project, spec.name),
		Name:                       spec.name,
		Description:                spec.description,
		AllowDestroyPlan:           true,
		AutoApply:                  spec.autoApply,
		CreatedAt:                  epoch.AddDate(-2, 0, 0),
		UpdatedAt:                  runStart(project, spec.name, 0),
		Environment:                "default",
		ExecutionMode:              remoteExecution,
		FileTriggersEnabled:        true,
		GlobalRemoteState:          spec.globalRemoteState,
		Operations:                 true,
		ResourceCount:              spec.resources,
		RunsCount:                  w.runsPerWS,
		SpeculativeEnabled:         true,
		StructuredRunOutputEnabled: true,
		TerraformVersion:           spec.tfVersion,
		WorkingDirectory:           spec.dir,
		// The API reports both averages in milliseconds; go-tfe scales them into
		// a duration as it reads them.
		ApplyDurationAverage: 47_000,
		PlanDurationAverage:  22_000,
		TagNames:             []string{"environment:" + env, "team:platform"},
		TriggerPrefixes:      []string{spec.dir},
		SettingOverwrites:    &tfe.WorkspaceSettingOverwrites{},
		Permissions:          &tfe.WorkspacePermissions{CanUpdate: true, CanQueueRun: true, CanLock: true},
		Actions:              &tfe.WorkspaceActions{IsDestroyable: true},
		VCSRepo: &tfe.VCSRepo{
			Identifier:        spec.repo,
			Branch:            mainBranch,
			RepositoryHTTPURL: "https://github.com/" + spec.repo,
			ServiceProvider:   "github_app",
		},
		Organization:                orgRef(),
		Project:                     model,
		CurrentRun:                  &tfe.Run{ID: w.ids.run(project, spec.name, 0)},
		CurrentStateVersion:         &tfe.StateVersion{ID: w.ids.stateVersion(project, spec.name, 0)},
		CurrentConfigurationVersion: &tfe.ConfigurationVersion{ID: w.ids.configVersion(project, spec.name, 0)},
	}
}

// newVariables builds a workspace's variables, including the write-only
// sensitive value the API returns blank.
func (w *world) newVariables(project string, spec workspaceSpec) []*tfe.Variable {
	seed := project + "/" + spec.name
	ref := &tfe.Workspace{ID: w.ids.workspace(project, spec.name)}

	return []*tfe.Variable{{
		ID:          w.ids.id("var", seed+"/environment"),
		Key:         tagKeyEnvironment,
		Value:       environmentOf(spec.name),
		Description: "The environment this workspace manages.",
		Category:    tfe.CategoryTerraform,
		Workspace:   ref,
	}, {
		ID:          w.ids.id("var", seed+"/tags"),
		Key:         "tags",
		Value:       "{\n  owner = \"platform\"\n}",
		Description: "Tags applied to every resource.",
		Category:    tfe.CategoryTerraform,
		HCL:         true,
		Workspace:   ref,
	}, {
		ID:          w.ids.id("var", seed+"/region"),
		Key:         "AWS_REGION",
		Value:       "us-east-2",
		Description: "The region the provider assumes into.",
		Category:    tfe.CategoryEnv,
		Workspace:   ref,
	}, {
		ID:          w.ids.id("var", seed+"/token"),
		Key:         "TF_VAR_datadog_api_key",
		Description: "Read back blank: sensitive values are write-only upstream.",
		Category:    tfe.CategoryEnv,
		Sensitive:   true,
		Workspace:   ref,
	}}
}

// newTagBindings builds a workspace's own tag bindings.
func (w *world) newTagBindings(spec workspaceSpec) []*tfe.TagBinding {
	return []*tfe.TagBinding{{
		ID:    w.ids.id("tb", spec.name+"/environment"),
		Key:   tagKeyEnvironment,
		Value: environmentOf(spec.name),
	}}
}

// newEffectiveTagBindings builds the tag bindings a workspace carries once its
// project's are inherited.
func (w *world) newEffectiveTagBindings(spec workspaceSpec) []*tfe.EffectiveTagBinding {
	return []*tfe.EffectiveTagBinding{{
		ID:    w.ids.id("etb", spec.name+"/environment"),
		Key:   tagKeyEnvironment,
		Value: environmentOf(spec.name),
	}, {
		ID:    w.ids.id("etb", spec.name+"/team"),
		Key:   "team",
		Value: "platform",
	}}
}

// newTeamAccess builds the team access one workspace grants.
func (w *world) newTeamAccess(ws *tfe.Workspace) []*tfe.TeamAccess {
	return []*tfe.TeamAccess{{
		ID:               w.ids.id("tws", ws.Name),
		Access:           tfe.AccessWrite,
		Runs:             tfe.RunsPermissionApply,
		Variables:        tfe.VariablesPermissionWrite,
		StateVersions:    tfe.StateVersionsPermissionWrite,
		SentinelMocks:    tfe.SentinelMocksPermissionRead,
		WorkspaceLocking: true,
		RunTasks:         true,
		Team:             &tfe.Team{ID: w.ids.team("platform-engineering")},
		Workspace:        &tfe.Workspace{ID: ws.ID},
	}}
}

// newWorkspaceRunTasks builds the run-task bindings one workspace carries.
func (w *world) newWorkspaceRunTasks(ws *tfe.Workspace) []*tfe.WorkspaceRunTask {
	return []*tfe.WorkspaceRunTask{{
		ID:               w.ids.id("wstask", ws.Name),
		EnforcementLevel: tfe.Advisory,
		Stage:            tfe.PostPlan,
		Stages:           []tfe.Stage{tfe.PostPlan},
		RunTask:          &tfe.RunTask{ID: w.ids.id("task", "conftest")},
		Workspace:        &tfe.Workspace{ID: ws.ID},
	}}
}

// newConfigVersion builds one configuration version and its ingress
// attributes, which the archiver splits into a file of their own.
func (w *world) newConfigVersion(cvID string, spec workspaceSpec, n int) *tfe.ConfigurationVersion {
	runSpec := runSpecs[n%len(runSpecs)]

	return &tfe.ConfigurationVersion{
		ID:            cvID,
		AutoQueueRuns: true,
		Source:        tfe.ConfigurationSourceGithub,
		Status:        tfe.ConfigurationUploaded,
		IngressAttributes: &tfe.IngressAttributes{
			ID:                w.ids.id("ia", cvID),
			Branch:            mainBranch,
			CommitSHA:         w.ids.commitSHA(cvID),
			CommitMessage:     runSpec.message,
			CommitURL:         "https://github.com/" + spec.repo + "/commit/" + w.ids.commitSHA(cvID),
			Identifier:        spec.repo,
			SenderUsername:    actorOf(runSpec),
			IsPullRequest:     runSpec.trigger == triggerVCS,
			PullRequestNumber: 218,
		},
	}
}

// newRun builds one run of a workspace's history, counting back from the
// newest, together with the artifacts its own endpoints serve.
//
// Run zero is reported still planning, so the archive keeps one run loose
// beside the sealed history the rest of the walk freezes; its children are
// never fetched, which is what a real in-flight run looks like mid-collection.
func (w *world) newRun(
	project string,
	spec workspaceSpec,
	ws *tfe.Workspace,
	cfgVer *configWorld,
	n int,
) *runWorld {
	rs := runSpecs[n%len(runSpecs)]
	rid := w.ids.run(project, spec.name, n)
	started := runStart(project, spec.name, n)
	live := n == 0

	if live {
		rs.status = tfe.RunPlanning
	}

	run := &tfe.Run{
		ID:               rid,
		CreatedAt:        started,
		HasChanges:       rs.status != tfe.RunPlannedAndFinished,
		IsDestroy:        rs.destroy,
		Message:          rs.message,
		Refresh:          true,
		Source:           rs.source,
		Status:           rs.status,
		TerraformVersion: spec.tfVersion,
		TriggerReason:    rs.trigger,
		Actions:          &tfe.RunActions{},
		Permissions:      &tfe.RunPermissions{CanApply: true, CanCancel: true, CanDiscard: true},
		StatusTimestamps: &tfe.RunStatusTimestamps{
			PlanQueueableAt: started,
			PlanQueuedAt:    started.Add(2 * time.Second),
			PlanningAt:      started.Add(6 * time.Second),
			PlannedAt:       started.Add(31 * time.Second),
			AppliedAt:       started.Add(88 * time.Second),
		},
		Variables:            []*tfe.RunVariableAttr{},
		ConfigurationVersion: cfgVer.cv,
		CreatedBy:            w.userNamed(actorOf(rs)),
		Plan:                 w.newPlan(rid, rs, live),
		Workspace:            &tfe.Workspace{ID: ws.ID},
	}

	if rs.status == tfe.RunApplied {
		run.Apply = w.newApply(rid)
		run.ConfirmedBy = w.users[0]
	}

	rw := &runWorld{
		run:      run,
		planLog:  []byte(planLog(&spec, rs)),
		planJSON: []byte(planJSON(&spec, rs)),
		comments: w.newComments(rid, rs),
		events:   w.newRunEvents(rid, rs),
		checks:   w.newPolicyChecks(rid, rs),
	}

	if run.Apply != nil {
		rw.applyLog = []byte(applyLog(&spec))
	}

	// The platform keeps a run's log only so long; the oldest run in every
	// workspace has outlived that window, so its log URL answers 404 and the
	// archive records a confirmed absence where the log used to be.
	rw.planLogExpired = n == w.runsPerWS-1

	return rw
}

// newPlan builds a run's plan record, whose log URL is absolute because a
// relative one would resolve against the API base path and answer 404.
func (w *world) newPlan(rid string, rs runSpec, live bool) *tfe.Plan {
	planID := w.ids.id("plan", rid)

	status := tfe.PlanFinished
	if live {
		status = tfe.PlanRunning
	}

	return &tfe.Plan{
		ID:                   planID,
		HasChanges:           rs.status != tfe.RunPlannedAndFinished,
		LogReadURL:           w.base + "/blobs/plans/" + planID + "/log",
		ResourceAdditions:    1,
		ResourceChanges:      1,
		ResourceDestructions: 0,
		Status:               status,
	}
}

// newApply builds a run's apply record, whose log URL is absolute for the same
// reason the plan's is.
func (w *world) newApply(rid string) *tfe.Apply {
	applyID := w.ids.id("apply", rid)

	return &tfe.Apply{
		ID:                   applyID,
		LogReadURL:           w.base + "/blobs/applies/" + applyID + "/log",
		ResourceAdditions:    1,
		ResourceChanges:      1,
		ResourceDestructions: 0,
		Status:               tfe.ApplyFinished,
	}
}

// newComments builds the run comments a review left, which only the runs that
// needed a human decision carry.
func (w *world) newComments(rid string, rs runSpec) []*tfe.Comment {
	if rs.status != tfe.RunPolicySoftFailed {
		return []*tfe.Comment{}
	}

	return []*tfe.Comment{{
		ID:   w.ids.id("comment", rid),
		Body: "Overriding: the wider CIDR is the migration plan agreed in RFC-114.",
	}}
}

// newRunEvents builds a run's actor-attributed timeline, ending on the action
// that settled it.
func (w *world) newRunEvents(rid string, rs runSpec) []*tfe.RunEvent {
	actions := []string{"created the run", "started the plan", "finished the plan"}

	switch rs.status {
	case tfe.RunApplied:
		actions = append(actions, "confirmed the run", "finished the apply")
	case tfe.RunErrored:
		actions = append(actions, "errored the plan")
	case tfe.RunDiscarded:
		actions = append(actions, "discarded the run")
	case tfe.RunPolicySoftFailed:
		actions = append(actions, "soft-failed a policy check")
	default:
		actions = append(actions, "finished the run")
	}

	events := make([]*tfe.RunEvent, 0, len(actions))

	for i, action := range actions {
		events = append(events, &tfe.RunEvent{
			ID:        w.ids.id("re", fmt.Sprintf("%s/%d", rid, i)),
			Action:    action,
			CreatedAt: epoch.Add(time.Duration(i) * 20 * time.Second),
			Actor:     w.userNamed(actorOf(rs)),
		})
	}

	return events
}

// newPolicyChecks builds the Sentinel results a run collected, which only a run
// the policy set stopped carries.
//
// The status is always settled: go-tfe polls a pending or queued check every
// half second, without bound, before it will read the check's log.
func (w *world) newPolicyChecks(rid string, rs runSpec) []*tfe.PolicyCheck {
	if rs.status != tfe.RunPolicySoftFailed {
		return nil
	}

	return []*tfe.PolicyCheck{{
		ID:     w.ids.id("polchk", rid),
		Scope:  tfe.PolicyScopeOrganization,
		Status: tfe.PolicySoftFailed,
		Result: &tfe.PolicyResult{
			Passed:      11,
			SoftFailed:  1,
			TotalFailed: 1,
		},
		Actions:     &tfe.PolicyActions{IsOverridable: true},
		Permissions: &tfe.PolicyPermissions{CanOverride: true},
		Run:         &tfe.Run{ID: rid},
	}}
}

// newStateVersion builds one state version of a workspace's history, counting
// back from the newest, together with both renderings of its state.
//
// Version zero is reported still pending with no download URLs, the shape a
// state version has between its creation and its upload finishing, so a walk
// records nothing for it and picks it up once it finalizes.
func (w *world) newStateVersion(project string, spec workspaceSpec, n int) *stateWorld {
	created := stateCreated(project, spec.name, n)
	svID := w.ids.stateVersion(project, spec.name, n)
	serial := int64(w.statesPerWS - n + 40)
	raw := rawState(&spec, serial)

	sv := &tfe.StateVersion{
		ID:                 svID,
		CreatedAt:          created,
		Status:             tfe.StateVersionFinalized,
		Serial:             serial,
		Size:               int64(len(raw)),
		ResourcesProcessed: true,
		StateVersion:       4,
		TerraformVersion:   spec.tfVersion,
		VCSCommitSHA:       w.ids.commitSHA(svID),
		DownloadURL:        w.base + "/blobs/state-versions/" + svID + "/tfstate",
		JSONDownloadURL:    w.base + "/blobs/state-versions/" + svID + "/json",
		Run:                &tfe.Run{ID: w.ids.run(project, spec.name, n)},
	}

	if n == 0 {
		sv.Status = tfe.StateVersionPending
		sv.ResourcesProcessed = false
		sv.DownloadURL = ""
		sv.JSONDownloadURL = ""
	}

	return &stateWorld{sv: sv, raw: []byte(raw), json: []byte(jsonState(&spec, serial))}
}

// buildRegistry builds the organization's private registry.
func (w *world) buildRegistry() {
	w.modules = w.newModules()
	w.gpgKeys = w.newGPGKeys()
}

// newModules builds the organization's private registry modules.
func (w *world) newModules() []*tfe.RegistryModule {
	published := epoch.AddDate(0, -2, 0).Format(time.RFC3339)

	built := make([]*tfe.RegistryModule, 0, 2)

	for _, name := range []string{"network", "warehouse"} {
		built = append(built, &tfe.RegistryModule{
			ID:                  w.ids.id("mod", name),
			Name:                name,
			Provider:            "aws",
			RegistryName:        tfe.PrivateRegistry,
			Namespace:           orgName,
			Status:              tfe.RegistryModuleStatusSetupComplete,
			PublishingMechanism: tfe.PublishingMechanismTag,
			Permissions:         &tfe.RegistryModulePermissions{CanDelete: true, CanResync: true, CanRetry: true},
			VCSRepo: &tfe.VCSRepo{
				Identifier:        orgName + "/terraform-aws-" + name,
				Branch:            mainBranch,
				RepositoryHTTPURL: "https://github.com/" + orgName + "/terraform-aws-" + name,
				ServiceProvider:   "github_app",
			},
			VersionStatuses: []tfe.RegistryModuleVersionStatuses{{
				Version: "2.4.0",
				Status:  tfe.RegistryModuleVersionStatusOk,
			}, {
				Version: "2.3.1",
				Status:  tfe.RegistryModuleVersionStatusOk,
			}},
			CreatedAt:    published,
			UpdatedAt:    published,
			Organization: orgRef(),
		})
	}

	return built
}

// newGPGKeys builds the signing keys the private registry publishes.
func (w *world) newGPGKeys() []*tfe.GPGKey {
	return []*tfe.GPGKey{{
		ID:         w.ids.id("gpg", orgName),
		KeyID:      strings.ToUpper(w.ids.commitSHA("gpg/" + orgName)[:16]),
		Namespace:  orgName,
		Source:     "",
		AsciiArmor: "-----BEGIN PGP PUBLIC KEY BLOCK-----\n\nmDMEZk...\n-----END PGP PUBLIC KEY BLOCK-----\n",
		CreatedAt:  epoch.AddDate(-1, 0, 0),
		UpdatedAt:  epoch.AddDate(-1, 0, 0),
	}}
}

// userNamed returns the user with the given username, or nil when the roster
// holds none.
func (w *world) userNamed(username string) *tfe.User {
	for i, p := range people {
		if p.username == username {
			return w.users[i]
		}
	}

	return nil
}

// policyCheckLog renders the Sentinel output a soft-failed policy check
// produced.
func policyCheckLog(spec *workspaceSpec) string {
	return fmt.Sprintf(`Sentinel Result: false

This result means that one or more Sentinel policies failed. More details are
shown below.

1 policies evaluated.

## Policy 1: mandatory-tags (soft-mandatory)

Result: false

FALSE - mandatory-tags.sentinel:18:1 - Rule "main"
  aws_cloudwatch_log_group.flow_logs in %s is missing the "owner" tag.
`, spec.name)
}
