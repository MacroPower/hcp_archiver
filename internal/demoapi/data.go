package demoapi

import (
	"time"

	"github.com/hashicorp/go-tfe"
)

// projectSpec describes one served project and the workspaces it owns.
type projectSpec struct {
	name        string
	description string
	workspaces  []workspaceSpec
}

// workspaceSpec describes one served workspace: the settings its overview
// screen shows and the shape of the run history generated beneath it.
type workspaceSpec struct {
	name        string
	description string
	repo        string
	tfVersion   string
	dir         string
	resources   int
	autoApply   bool
	// The globalRemoteState setting decides whether the archiver asks for the
	// workspace's remote-state consumers at all, so the demo organization
	// answers both ways.
	globalRemoteState bool
	// The noReadme setting drops the workspace's readme, which the API reports
	// by answering the readme-included workspace read with a 404. It is the one
	// always-absent object the demo organization carries, so an archive shows a
	// settled gap and a run shows the confirming re-probe that precedes one.
	noReadme bool
}

// runSpec describes one generated run: what it did and how it ended.
type runSpec struct {
	status  tfe.RunStatus
	source  tfe.RunSource
	message string
	trigger string
	destroy bool
}

const (
	// The fictional organization every served document belongs to.
	orgName = "jacobcolvin-com"

	// The source the platform records for a run the Terraform CLI queued, which
	// go-tfe carries no constant for.
	runSourceCLI tfe.RunSource = "terraform+cloud"

	// The values the demo organization repeats across its workspaces and runs:
	// the Terraform version most of them pin, the working directory the
	// production workspaces build from, the execution mode every workspace runs
	// in, and the two reasons a run is triggered.
	currentTF       = "1.12.2"
	prodDir         = "envs/prod"
	remoteExecution = "remote"
	triggerVCS      = "vcs"
	triggerManual   = "manual"

	// The tag key every workspace carries its environment under, and the branch
	// every repository in the organization publishes from.
	tagKeyEnvironment = "environment"
	mainBranch        = "main"
)

var (
	// The demo organization's "now": every timestamp is an offset back from it,
	// so the recordings show stable dates rather than drifting with the clock.
	epoch = time.Date(2026, time.August, 10, 17, 4, 0, 0, time.UTC)

	// The demo organization's whole tree, ordered as the API lists it.
	projects = []projectSpec{{
		name:        "platform",
		description: "Shared network, identity, and cluster foundations.",
		workspaces: []workspaceSpec{{
			name:        "network-prod",
			description: "Production VPCs, transit gateway, and flow logs.",
			repo:        "jacobcolvin-com/terraform-network",
			dir:         prodDir,
			tfVersion:   currentTF,
			resources:   187,
		}, {
			name:              "network-staging",
			description:       "Staging mirror of the production network.",
			repo:              "jacobcolvin-com/terraform-network",
			dir:               "envs/staging",
			tfVersion:         currentTF,
			resources:         164,
			autoApply:         true,
			globalRemoteState: true,
		}, {
			name:              "kubernetes-prod",
			description:       "Production EKS clusters and their node groups.",
			repo:              "jacobcolvin-com/terraform-kubernetes",
			dir:               prodDir,
			tfVersion:         "1.11.4",
			resources:         243,
			globalRemoteState: true,
		}},
	}, {
		name:        "data-services",
		description: "Warehouse, streaming, and the pipelines between them.",
		workspaces: []workspaceSpec{{
			name:        "warehouse-prod",
			description: "Snowflake warehouses, roles, and grants.",
			repo:        "jacobcolvin-com/terraform-warehouse",
			dir:         prodDir,
			tfVersion:   currentTF,
			resources:   96,
		}, {
			name:              "streaming-prod",
			description:       "Kafka topics, consumer groups, and connectors.",
			repo:              "jacobcolvin-com/terraform-streaming",
			dir:               prodDir,
			tfVersion:         "1.10.5",
			resources:         58,
			globalRemoteState: true,
		}},
	}, {
		name:        "edge",
		description: "Everything that answers a request from the internet.",
		workspaces: []workspaceSpec{{
			name:        "cdn-prod",
			description: "CDN distributions, certificates, and cache policies.",
			repo:        "jacobcolvin-com/terraform-edge",
			dir:         "cdn",
			tfVersion:   currentTF,
			resources:   41,
			noReadme:    true,
		}, {
			name:              "dns",
			description:       "Public zones and their delegation records.",
			repo:              "jacobcolvin-com/terraform-edge",
			dir:               "dns",
			tfVersion:         currentTF,
			resources:         312,
			autoApply:         true,
			globalRemoteState: true,
		}},
	}}

	// The rotation each workspace's history is drawn from, newest first, so
	// every list screen shows a plausible spread of outcomes rather than a
	// column of identical rows.
	runSpecs = []runSpec{{
		status:  tfe.RunApplied,
		source:  tfe.RunSourceUI,
		message: "Merge pull request #218 from jacobcolvin-com/flow-log-retention",
		trigger: triggerVCS,
	}, {
		status:  tfe.RunPlannedAndFinished,
		source:  tfe.RunSourceAPI,
		message: "Speculative plan for pull request #219",
		trigger: triggerVCS,
	}, {
		status:  tfe.RunErrored,
		source:  tfe.RunSourceUI,
		message: "Merge pull request #217 from jacobcolvin-com/bump-provider-aws",
		trigger: triggerVCS,
	}, {
		status:  tfe.RunApplied,
		source:  runSourceCLI,
		message: "Triggered via CLI",
		trigger: triggerManual,
	}, {
		status:  tfe.RunPolicySoftFailed,
		source:  tfe.RunSourceUI,
		message: "Merge pull request #214 from jacobcolvin-com/widen-cidr",
		trigger: triggerVCS,
	}, {
		status:  tfe.RunApplied,
		source:  tfe.RunSourceAPI,
		message: "Scheduled drift correction",
		trigger: triggerManual,
	}, {
		status:  tfe.RunDiscarded,
		source:  tfe.RunSourceUI,
		message: "Retire the decommissioned peering links",
		trigger: triggerManual,
		destroy: true,
	}, {
		status:  tfe.RunApplied,
		source:  runSourceCLI,
		message: "Triggered via CLI",
		trigger: triggerManual,
	}}

	// The users the demo organization's runs, comments, and memberships are
	// attributed to.
	people = []struct {
		username string
		email    string
		name     string
	}{
		{"rmoreno", "rmoreno@example.com", "R. Moreno"},
		{"ptakahashi", "ptakahashi@example.com", "P. Takahashi"},
		{"dokafor", "dokafor@example.com", "D. Okafor"},
		{"terraform-ci", "ci@example.com", "Terraform CI"},
	}

	// The demo organization's teams and the access each one carries.
	teams = []struct {
		name       string
		visibility string
		members    int
	}{
		{"owners", "secret", 1},
		{"platform-engineering", "organization", 3},
		{"data-engineering", "organization", 2},
	}
)

// runStart returns when a workspace's run number n began, counting back from
// [epoch] so run 0 is the newest.
func runStart(project, ws string, n int) time.Time {
	offset := time.Duration(n)*37*time.Hour + time.Duration(len(project)+len(ws))*time.Minute

	return epoch.Add(-offset)
}

// stateCreated returns when a workspace's state version number n was created:
// just after the run that produced it finished, so the two histories line up.
func stateCreated(project, ws string, n int) time.Time {
	return runStart(project, ws, n).Add(90 * time.Second)
}

// environmentOf reads a workspace's environment out of its name, which is how
// the demo organization tags them.
func environmentOf(ws string) string {
	if len(ws) > len("-staging") && ws[len(ws)-len("-staging"):] == "-staging" {
		return "staging"
	}

	return "production"
}

// actorOf names the user a run is attributed to: the CI service account for
// anything a merge triggered, a person for anything queued by hand.
func actorOf(spec runSpec) string {
	if spec.trigger == triggerVCS {
		return people[3].username
	}

	return people[1].username
}
