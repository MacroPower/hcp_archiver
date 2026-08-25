package demoapi

import (
	"fmt"

	"github.com/hashicorp/go-tfe"
)

// world is the whole organization the server answers from, built once by
// [newWorld] and read-only from then on.
//
// Every collection is an ordered slice, and the maps beside them exist only to
// find an element by its identifier. No handler ranges a map or computes a
// document, so two identical requests answer with byte-identical documents,
// which is what keeps a re-run of the archiver from rewriting files whose
// content did not change.
//
// The fields are grouped by memory layout rather than by subject, which is what
// the alignment check asks for: the maps that find one object by its identifier
// first, then the address the artifact links resolve against, then the ordered
// collections the listings answer from.
type world struct {
	ids *ids
	org *tfe.Organization

	variableSetVars map[string][]*tfe.VariableSetVariable
	policySources   map[string][]byte
	projectByID     map[string]*tfe.Project
	projectAccess   map[string][]*tfe.TeamProjectAccess
	workspaceByID   map[string]*workspaceWorld
	workspaceByName map[string]*workspaceWorld
	runByID         map[string]*runWorld
	planByID        map[string]*runWorld
	applyByID       map[string]*runWorld
	checkByID       map[string]*policyCheckWorld
	configByID      map[string]*configWorld
	stateByID       map[string]*stateWorld

	base string

	orgs            []*tfe.Organization
	users           []*tfe.User
	memberships     []*tfe.OrganizationMembership
	teams           []*tfe.Team
	variableSets    []*tfe.VariableSet
	policies        []*tfe.Policy
	runTasks        []*tfe.RunTask
	reservedTagKeys []*tfe.ReservedTagKey
	modules         []*tfe.RegistryModule
	gpgKeys         []*tfe.GPGKey
	projects        []*tfe.Project
	workspaces      []*tfe.Workspace

	runsPerWS   int
	statesPerWS int
}

// workspaceWorld is one workspace and everything the endpoints beneath it
// serve.
type workspaceWorld struct {
	ws            *tfe.Workspace
	readme        string
	variables     []*tfe.Variable
	tagBindings   []*tfe.TagBinding
	effectiveTags []*tfe.EffectiveTagBinding
	teamAccess    []*tfe.TeamAccess
	workspaceRuns []*tfe.WorkspaceRunTask
	consumers     []*tfe.Workspace
	runs          []*tfe.Run
	states        []*tfe.StateVersion
}

// runWorld is one run and the artifacts its own endpoints serve.
type runWorld struct {
	run      *tfe.Run
	planLog  []byte
	planJSON []byte
	applyLog []byte
	comments []*tfe.Comment
	events   []*tfe.RunEvent
	checks   []*tfe.PolicyCheck
	// The planLogExpired flag marks a run old enough that the platform has
	// dropped its log, so the log URL answers 404 rather than bytes.
	planLogExpired bool
}

// policyCheckWorld is one policy check and the Sentinel output it serves.
type policyCheckWorld struct {
	check *tfe.PolicyCheck
	log   []byte
}

// configWorld is one configuration version and the tarball it serves.
type configWorld struct {
	cv      *tfe.ConfigurationVersion
	tarball []byte
}

// stateWorld is one state version and both renderings of its state.
type stateWorld struct {
	sv   *tfe.StateVersion
	raw  []byte
	json []byte
}

// newWorld builds the served organization at base, the absolute URL the
// server's own artifact links resolve against.
//
// It returns an error only when a generated artifact cannot be rendered, which
// is a bug in the generator rather than a runtime condition.
func newWorld(cfg config, base string) (*world, error) {
	w := &world{
		ids:             newIDs(cfg.seed),
		base:            base,
		runsPerWS:       cfg.runs,
		statesPerWS:     cfg.states,
		variableSetVars: map[string][]*tfe.VariableSetVariable{},
		policySources:   map[string][]byte{},
		projectByID:     map[string]*tfe.Project{},
		projectAccess:   map[string][]*tfe.TeamProjectAccess{},
		workspaceByID:   map[string]*workspaceWorld{},
		workspaceByName: map[string]*workspaceWorld{},
		runByID:         map[string]*runWorld{},
		planByID:        map[string]*runWorld{},
		applyByID:       map[string]*runWorld{},
		checkByID:       map[string]*policyCheckWorld{},
		configByID:      map[string]*configWorld{},
		stateByID:       map[string]*stateWorld{},
	}

	w.buildOrgScope()

	err := w.buildProjects()
	if err != nil {
		return nil, err
	}

	w.buildRegistry()

	return w, nil
}

// buildOrgScope builds the organization record and everything it owns directly.
func (w *world) buildOrgScope() {
	w.users = w.newUsers()
	w.memberships = w.newMemberships()
	w.teams = w.newTeams()
	w.org = w.newOrganization()
	w.orgs = []*tfe.Organization{w.org}
	w.variableSets = w.newVariableSets()
	w.runTasks = w.newRunTasks()
	w.reservedTagKeys = w.newReservedTagKeys()
	w.policies = w.newPolicies()

	for _, set := range w.variableSets {
		w.variableSetVars[set.ID] = w.newVariableSetVariables(set)
	}

	for _, policy := range w.policies {
		w.policySources[policy.ID] = []byte(policySource(policy.Name))
	}
}

// buildProjects builds every project, its workspaces, and the run and state
// history beneath each of them.
func (w *world) buildProjects() error {
	for _, spec := range projects {
		project := w.newProject(spec)

		w.projects = append(w.projects, project)
		w.projectByID[project.ID] = project
		w.projectAccess[project.ID] = w.newProjectAccess(project)

		for _, wsSpec := range spec.workspaces {
			err := w.buildWorkspace(spec.name, wsSpec, project)
			if err != nil {
				return err
			}
		}
	}

	w.linkRemoteStateConsumers()

	return nil
}

// buildWorkspace builds one workspace, its adjacent metadata, and its run and
// state history.
func (w *world) buildWorkspace(project string, spec workspaceSpec, model *tfe.Project) error {
	ws := w.newWorkspace(project, spec, model)

	ww := &workspaceWorld{
		ws:            ws,
		variables:     w.newVariables(project, spec),
		tagBindings:   w.newTagBindings(spec),
		effectiveTags: w.newEffectiveTagBindings(spec),
		teamAccess:    w.newTeamAccess(ws),
		workspaceRuns: w.newWorkspaceRunTasks(ws),
	}

	if !spec.noReadme {
		ww.readme = readme(&spec)
	}

	err := w.buildRuns(project, spec, ws, ww)
	if err != nil {
		return err
	}

	w.buildStates(project, spec, ww)

	w.workspaces = append(w.workspaces, ws)
	w.workspaceByID[ws.ID] = ww
	w.workspaceByName[ws.Name] = ww

	return nil
}

// buildRuns builds the workspace's run history, newest first, and the
// configuration versions the runs were built from.
func (w *world) buildRuns(project string, spec workspaceSpec, ws *tfe.Workspace, ww *workspaceWorld) error {
	for n := range w.runsPerWS {
		cfgVer, err := w.configVersionFor(project, spec, n)
		if err != nil {
			return err
		}

		rw := w.newRun(project, spec, ws, cfgVer, n)

		ww.runs = append(ww.runs, rw.run)
		w.runByID[rw.run.ID] = rw
		w.planByID[rw.run.Plan.ID] = rw

		if rw.run.Apply != nil {
			w.applyByID[rw.run.Apply.ID] = rw
		}

		for _, check := range rw.checks {
			w.checkByID[check.ID] = &policyCheckWorld{check: check, log: []byte(policyCheckLog(&spec))}
		}
	}

	return nil
}

// configVersionFor returns the configuration version a workspace's run number n
// was built from, building it (and its tarball) on first use.
func (w *world) configVersionFor(project string, spec workspaceSpec, n int) (*configWorld, error) {
	cvID := w.ids.configVersion(project, spec.name, n)

	existing, ok := w.configByID[cvID]
	if ok {
		return existing, nil
	}

	tarball, err := configTarball(&spec)
	if err != nil {
		return nil, fmt.Errorf("render configuration tarball %s: %w", cvID, err)
	}

	cw := &configWorld{cv: w.newConfigVersion(cvID, spec, n), tarball: tarball}
	w.configByID[cvID] = cw

	return cw, nil
}

// buildStates builds the workspace's state history, newest first.
func (w *world) buildStates(project string, spec workspaceSpec, ww *workspaceWorld) {
	for n := range w.statesPerWS {
		sw := w.newStateVersion(project, spec, n)

		ww.states = append(ww.states, sw.sv)
		w.stateByID[sw.sv.ID] = sw
	}
}

// linkRemoteStateConsumers wires each workspace that does not share its state
// globally to the workspaces that read it, which is the only shape in which the
// consumers listing answers with anything.
func (w *world) linkRemoteStateConsumers() {
	for _, ws := range w.workspaces {
		if ws.GlobalRemoteState {
			continue
		}

		owner := w.workspaceByID[ws.ID]

		for _, other := range w.workspaces {
			if other.ID != ws.ID && other.Project.ID == ws.Project.ID {
				owner.consumers = append(owner.consumers, other)
			}
		}
	}
}

// chaosTargets names the paths the failure injector treats specially, each
// picked because the archiver fetches it exactly once per run: a path a
// collector re-lists has no stable attempt count for a one-shot rule to key on.
//
// All three sit in the first workspace, which every demo configuration
// archives, so a narrowed run meets the same failures a whole-organization one
// does.
func (w *world) chaosTargets() map[string]profile {
	targets := map[string]profile{}

	if len(w.workspaces) == 0 {
		return targets
	}

	ww := w.workspaceByID[w.workspaces[0].ID]

	// One state-version read is rate limited, which halves the general
	// governor's rate once and pauses its launches for the advertised second.
	if idx := min(2, len(ww.states)-1); idx > 0 {
		targets[apiPrefix+"state-versions/"+ww.states[idx].ID] = profileRateLimit
	}

	// One plan log arrives truncated. It is the second-newest run's, since the
	// newest is still planning and its log is never fetched.
	if len(ww.runs) > 1 {
		targets[blobPrefix+"plans/"+ww.runs[1].Plan.ID+"/log"] = profileTruncate
	}

	// One configuration version answers 404 before it answers bytes, the
	// eventual-consistency blip every archive primitive re-probes.
	if len(ww.runs) > 2 {
		targets[apiPrefix+"configuration-versions/"+ww.runs[2].ConfigurationVersion.ID] = profileVanish
	}

	return targets
}
