package demoapi

import (
	"net/http"
	"strings"

	"github.com/hashicorp/go-tfe"
)

// api serves one built organization. Every handler reads the world and writes;
// nothing is computed per request, so two identical requests answer with
// identical documents.
type api struct {
	world *world
}

// routes builds the endpoint table.
//
// The set is the one a default archive run reads: the optional stacks, HYOK,
// registry-detail, and audit-trail surfaces are not served, since nothing in
// the demo configuration turns them on. An unrouted path answers 404, which is
// deliberately loud: a listing the archiver expected and did not get is a
// dropped surface, and a dropped surface fails the run.
func (a *api) routes() *http.ServeMux {
	mux := http.NewServeMux()

	a.routeOrgScope(mux)
	a.routeProjects(mux)
	a.routeWorkspaces(mux)
	a.routeStateVersions(mux)
	a.routeRuns(mux)
	a.routeRegistry(mux)

	mux.HandleFunc("GET "+pingPath, a.ping)
	mux.HandleFunc("/", a.notFound)

	return mux
}

// ping answers the metadata probe go-tfe opens every client with. It reads the
// version headers off the response and ignores its status entirely.
func (a *api) ping(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("TFP-API-Version", "2.5")
	w.Header().Set("TFP-AppName", "HCP Terraform")
	w.WriteHeader(http.StatusNoContent)
}

// notFound answers a path the server does not serve.
func (a *api) notFound(w http.ResponseWriter, _ *http.Request) {
	writeError(w, http.StatusNotFound, "not found")
}

// routeOrgScope registers the endpoints the org-scope collector reads.
func (a *api) routeOrgScope(mux *http.ServeMux) {
	mux.HandleFunc("GET "+apiPrefix+"organizations", func(w http.ResponseWriter, r *http.Request) {
		writeList(w, r, a.world.orgs)
	})

	mux.HandleFunc("GET "+apiPrefix+"organizations/{org}", a.org(func(w http.ResponseWriter, _ *http.Request) {
		writeOne(w, a.world.org)
	}))

	mux.HandleFunc("GET "+apiPrefix+"organizations/{org}/teams", a.org(func(w http.ResponseWriter, r *http.Request) {
		writeList(w, r, a.world.teams)
	}))

	mux.HandleFunc("GET "+apiPrefix+"teams/{id}/notification-configurations",
		func(w http.ResponseWriter, r *http.Request) {
			writeList(w, r, []*tfe.NotificationConfiguration(nil))
		})

	mux.HandleFunc("GET "+apiPrefix+"organizations/{org}/organization-memberships",
		a.org(func(w http.ResponseWriter, r *http.Request) {
			writeList(w, r, a.world.memberships)
		}))

	mux.HandleFunc("GET "+apiPrefix+"organizations/{org}/oauth-clients",
		a.org(func(w http.ResponseWriter, r *http.Request) {
			writeList(w, r, []*tfe.OAuthClient(nil))
		}))

	mux.HandleFunc("GET "+apiPrefix+"github-app/installations", func(w http.ResponseWriter, r *http.Request) {
		writeList(w, r, []*tfe.GHAInstallation(nil))
	})

	a.routeGovernance(mux)
}

// routeGovernance registers the variable-set, policy, and organization-setting
// endpoints.
func (a *api) routeGovernance(mux *http.ServeMux) {
	mux.HandleFunc("GET "+apiPrefix+"organizations/{org}/varsets", a.org(func(w http.ResponseWriter, r *http.Request) {
		writeList(w, r, a.world.variableSets)
	}))

	mux.HandleFunc("GET "+apiPrefix+"varsets/{id}/relationships/vars", func(w http.ResponseWriter, r *http.Request) {
		writeList(w, r, a.world.variableSetVars[r.PathValue("id")])
	})

	mux.HandleFunc("GET "+apiPrefix+"organizations/{org}/policy-sets",
		a.org(func(w http.ResponseWriter, r *http.Request) {
			writeList(w, r, []*tfe.PolicySet(nil))
		}))

	mux.HandleFunc("GET "+apiPrefix+"policy-sets/{id}/parameters", func(w http.ResponseWriter, r *http.Request) {
		writeList(w, r, []*tfe.PolicySetParameter(nil))
	})

	mux.HandleFunc("GET "+apiPrefix+"organizations/{org}/policies", a.org(func(w http.ResponseWriter, r *http.Request) {
		writeList(w, r, a.world.policies)
	}))

	mux.HandleFunc("GET "+apiPrefix+"policies/{id}/download", func(w http.ResponseWriter, r *http.Request) {
		source, ok := a.world.policySources[r.PathValue("id")]
		if !ok {
			writeError(w, http.StatusNotFound, "policy not found")

			return
		}

		writeBlob(w, "text/plain; charset=utf-8", source)
	})

	mux.HandleFunc("GET "+apiPrefix+"organizations/{org}/tasks", a.org(func(w http.ResponseWriter, r *http.Request) {
		writeList(w, r, a.world.runTasks)
	}))

	mux.HandleFunc("GET "+apiPrefix+"organizations/{org}/agent-pools",
		a.org(func(w http.ResponseWriter, r *http.Request) {
			writeList(w, r, []*tfe.AgentPool(nil))
		}))

	mux.HandleFunc("GET "+apiPrefix+"organizations/{org}/token-ttl-policies",
		a.org(func(w http.ResponseWriter, r *http.Request) {
			writeList(w, r, []*tfe.OrganizationTokenTTLPolicy(nil))
		}))

	mux.HandleFunc("GET "+apiPrefix+"organizations/{org}/reserved-tag-keys",
		a.org(func(w http.ResponseWriter, r *http.Request) {
			writeList(w, r, a.world.reservedTagKeys)
		}))
}

// routeProjects registers the project endpoints.
func (a *api) routeProjects(mux *http.ServeMux) {
	mux.HandleFunc("GET "+apiPrefix+"organizations/{org}/projects", a.org(func(w http.ResponseWriter, r *http.Request) {
		writeList(w, r, a.world.projects)
	}))

	mux.HandleFunc("GET "+apiPrefix+"projects/{id}", func(w http.ResponseWriter, r *http.Request) {
		project, ok := a.world.projectByID[r.PathValue("id")]
		if !ok {
			writeError(w, http.StatusNotFound, "project not found")

			return
		}

		writeOne(w, project)
	})

	mux.HandleFunc("GET "+apiPrefix+"projects/{id}/notification-configurations",
		func(w http.ResponseWriter, r *http.Request) {
			writeList(w, r, []*tfe.NotificationConfiguration(nil))
		})

	mux.HandleFunc("GET "+apiPrefix+"team-projects", func(w http.ResponseWriter, r *http.Request) {
		writeList(w, r, a.world.projectAccess[r.URL.Query().Get("filter[project][id]")])
	})
}

// routeWorkspaces registers the workspace endpoints.
func (a *api) routeWorkspaces(mux *http.ServeMux) {
	mux.HandleFunc("GET "+apiPrefix+"organizations/{org}/workspaces",
		a.org(func(w http.ResponseWriter, r *http.Request) {
			writeList(w, r, a.world.workspaces)
		}))

	mux.HandleFunc("GET "+apiPrefix+"workspaces/{id}", a.workspace(a.readWorkspace))

	mux.HandleFunc("GET "+apiPrefix+"workspaces/{id}/all-vars",
		a.workspace(func(w http.ResponseWriter, r *http.Request, ww *workspaceWorld) {
			writeList(w, r, ww.variables)
		}))

	mux.HandleFunc("GET "+apiPrefix+"workspaces/{id}/tag-bindings",
		a.workspace(func(w http.ResponseWriter, r *http.Request, ww *workspaceWorld) {
			writeList(w, r, ww.tagBindings)
		}))

	mux.HandleFunc("GET "+apiPrefix+"workspaces/{id}/effective-tag-bindings",
		a.workspace(func(w http.ResponseWriter, r *http.Request, ww *workspaceWorld) {
			writeList(w, r, ww.effectiveTags)
		}))

	mux.HandleFunc("GET "+apiPrefix+"team-workspaces", func(w http.ResponseWriter, r *http.Request) {
		ww, ok := a.world.workspaceByID[r.URL.Query().Get("filter[workspace][id]")]
		if !ok {
			writeList(w, r, []*tfe.TeamAccess(nil))

			return
		}

		writeList(w, r, ww.teamAccess)
	})

	mux.HandleFunc("GET "+apiPrefix+"workspaces/{id}/notification-configurations",
		func(w http.ResponseWriter, r *http.Request) {
			writeList(w, r, []*tfe.NotificationConfiguration(nil))
		})

	mux.HandleFunc("GET "+apiPrefix+"workspaces/{id}/run-triggers", func(w http.ResponseWriter, r *http.Request) {
		writeList(w, r, []*tfe.RunTrigger(nil))
	})

	mux.HandleFunc("GET "+apiPrefix+"workspaces/{id}/tasks",
		a.workspace(func(w http.ResponseWriter, r *http.Request, ww *workspaceWorld) {
			writeList(w, r, ww.workspaceRuns)
		}))

	mux.HandleFunc("GET "+apiPrefix+"workspaces/{id}/relationships/remote-state-consumers",
		a.workspace(func(w http.ResponseWriter, r *http.Request, ww *workspaceWorld) {
			writeList(w, r, ww.consumers)
		}))
}

// readWorkspace answers the workspace read, which serves two different
// documents off one path: the settings record, and, when the readme is asked
// for, the raw markdown wrapped in a relation of its own. A workspace with no
// readme answers 404, which is how the platform reports one that was never
// written.
func (a *api) readWorkspace(w http.ResponseWriter, r *http.Request, ww *workspaceWorld) {
	if !strings.Contains(r.URL.Query().Get("include"), "readme") {
		writeOne(w, ww.ws)

		return
	}

	if ww.readme == "" {
		writeError(w, http.StatusNotFound, "workspace readme not found")

		return
	}

	writeOne(w, &workspaceReadme{
		ID: ww.ws.ID,
		Readme: &readmeDocument{
			ID:          ww.ws.ID,
			RawMarkdown: ww.readme,
		},
	})
}

// workspaceReadme is the workspace document the readme include answers with,
// mirroring the shape go-tfe decodes: the workspace's identifier and the readme
// relation, with the settings attributes left off.
type workspaceReadme struct {
	Readme *readmeDocument `jsonapi:"relation,readme"`
	ID     string          `jsonapi:"primary,workspaces"`
}

// readmeDocument is a workspace readme's raw markdown.
type readmeDocument struct {
	ID          string `jsonapi:"primary,workspace-readme"`
	RawMarkdown string `jsonapi:"attr,raw-markdown"`
}

// routeStateVersions registers the state-version endpoints, including the blob
// routes the version records advertise.
func (a *api) routeStateVersions(mux *http.ServeMux) {
	mux.HandleFunc("GET "+apiPrefix+"state-versions", func(w http.ResponseWriter, r *http.Request) {
		ww, ok := a.world.workspaceByName[r.URL.Query().Get("filter[workspace][name]")]
		if !ok {
			writeError(w, http.StatusNotFound, "workspace not found")

			return
		}

		writeList(w, r, ww.states)
	})

	mux.HandleFunc("GET "+apiPrefix+"state-versions/{id}",
		a.state(func(w http.ResponseWriter, _ *http.Request, sw *stateWorld) {
			writeOne(w, sw.sv)
		}))

	mux.HandleFunc("GET "+blobPrefix+"state-versions/{id}/tfstate",
		a.state(func(w http.ResponseWriter, _ *http.Request, sw *stateWorld) {
			writeBlob(w, "application/json", sw.raw)
		}))

	mux.HandleFunc("GET "+blobPrefix+"state-versions/{id}/json",
		a.state(func(w http.ResponseWriter, _ *http.Request, sw *stateWorld) {
			writeBlob(w, "application/json", sw.json)
		}))
}

// routeRuns registers the run endpoints and everything hanging off a run: its
// configuration version, its plan and apply with their logs, and the metadata
// listings the archiver reads once a run is terminal.
func (a *api) routeRuns(mux *http.ServeMux) {
	mux.HandleFunc("GET "+apiPrefix+"workspaces/{id}/runs",
		a.workspace(func(w http.ResponseWriter, r *http.Request, ww *workspaceWorld) {
			writeList(w, r, ww.runs)
		}))

	a.routeRunChildren(mux)
	a.routeConfigVersions(mux)
	a.routePlans(mux)
}

// routeRunChildren registers the listings that hang off one run.
func (a *api) routeRunChildren(mux *http.ServeMux) {
	mux.HandleFunc("GET "+apiPrefix+"runs/{id}", a.run(func(w http.ResponseWriter, _ *http.Request, rw *runWorld) {
		writeOne(w, rw.run)
	}))

	mux.HandleFunc("GET "+apiPrefix+"runs/{id}/comments",
		a.run(func(w http.ResponseWriter, r *http.Request, rw *runWorld) {
			writeList(w, r, rw.comments)
		}))

	mux.HandleFunc("GET "+apiPrefix+"runs/{id}/run-events",
		a.run(func(w http.ResponseWriter, r *http.Request, rw *runWorld) {
			writeList(w, r, rw.events)
		}))

	mux.HandleFunc("GET "+apiPrefix+"runs/{id}/policy-checks",
		a.run(func(w http.ResponseWriter, r *http.Request, rw *runWorld) {
			writeList(w, r, rw.checks)
		}))

	mux.HandleFunc("GET "+apiPrefix+"runs/{id}/task-stages",
		a.run(func(w http.ResponseWriter, r *http.Request, _ *runWorld) {
			writeList(w, r, []*tfe.TaskStage(nil))
		}))

	mux.HandleFunc("GET "+apiPrefix+"policy-checks/{id}", func(w http.ResponseWriter, r *http.Request) {
		check, ok := a.world.checkByID[r.PathValue("id")]
		if !ok {
			writeError(w, http.StatusNotFound, "policy check not found")

			return
		}

		writeOne(w, check.check)
	})

	mux.HandleFunc("GET "+apiPrefix+"policy-checks/{id}/output", func(w http.ResponseWriter, r *http.Request) {
		check, ok := a.world.checkByID[r.PathValue("id")]
		if !ok {
			writeError(w, http.StatusNotFound, "policy check not found")

			return
		}

		writeBlob(w, "text/plain; charset=utf-8", check.log)
	})
}

// routeConfigVersions registers the configuration-version record and its
// tarball.
func (a *api) routeConfigVersions(mux *http.ServeMux) {
	mux.HandleFunc("GET "+apiPrefix+"configuration-versions/{id}", func(w http.ResponseWriter, r *http.Request) {
		cw, ok := a.world.configByID[r.PathValue("id")]
		if !ok {
			writeError(w, http.StatusNotFound, "configuration version not found")

			return
		}

		writeOne(w, cw.cv)
	})

	mux.HandleFunc("GET "+apiPrefix+"configuration-versions/{id}/download",
		func(w http.ResponseWriter, r *http.Request) {
			cw, ok := a.world.configByID[r.PathValue("id")]
			if !ok {
				writeError(w, http.StatusNotFound, "configuration version not found")

				return
			}

			writeBlob(w, "application/octet-stream", cw.tarball)
		})
}

// routePlans registers the plan and apply records and the log and structured
// output they point at.
func (a *api) routePlans(mux *http.ServeMux) {
	mux.HandleFunc("GET "+apiPrefix+"plans/{id}", a.plan(func(w http.ResponseWriter, _ *http.Request, rw *runWorld) {
		writeOne(w, rw.run.Plan)
	}))

	mux.HandleFunc("GET "+apiPrefix+"plans/{id}/json-output",
		a.plan(func(w http.ResponseWriter, _ *http.Request, rw *runWorld) {
			writeBlob(w, "application/json", rw.planJSON)
		}))

	mux.HandleFunc("GET "+blobPrefix+"plans/{id}/log",
		a.plan(func(w http.ResponseWriter, _ *http.Request, rw *runWorld) {
			// The platform keeps a run's log only so long, and this one has
			// outlived that window; the archive records a confirmed absence in
			// its place.
			if rw.planLogExpired {
				writeError(w, http.StatusNotFound, "log expired")

				return
			}

			writeBlob(w, "text/plain; charset=utf-8", rw.planLog)
		}))

	mux.HandleFunc("GET "+apiPrefix+"applies/{id}", a.apply(func(w http.ResponseWriter, _ *http.Request, rw *runWorld) {
		writeOne(w, rw.run.Apply)
	}))

	mux.HandleFunc("GET "+blobPrefix+"applies/{id}/log",
		a.apply(func(w http.ResponseWriter, _ *http.Request, rw *runWorld) {
			writeBlob(w, "text/plain; charset=utf-8", rw.applyLog)
		}))
}

// routeRegistry registers the private registry endpoints. The GPG keys sit
// outside the API prefix, on the registry's own.
func (a *api) routeRegistry(mux *http.ServeMux) {
	mux.HandleFunc("GET "+apiPrefix+"organizations/{org}/registry-modules",
		a.org(func(w http.ResponseWriter, r *http.Request) {
			writeList(w, r, a.world.modules)
		}))

	mux.HandleFunc("GET "+apiPrefix+"organizations/{org}/registry-providers",
		a.org(func(w http.ResponseWriter, r *http.Request) {
			writeList(w, r, []*tfe.RegistryProvider(nil))
		}))

	mux.HandleFunc("GET "+registryPrefix+"gpg-keys", func(w http.ResponseWriter, r *http.Request) {
		writeList(w, r, a.world.gpgKeys)
	})
}

// org wraps a handler so a request naming an organization the server does not
// serve answers 404 rather than the served one's documents.
func (a *api) org(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("org") != orgName {
			writeError(w, http.StatusNotFound, "organization not found")

			return
		}

		next(w, r)
	}
}

// workspace resolves the workspace named by the request path, answering 404
// when the server serves none.
func (a *api) workspace(next func(http.ResponseWriter, *http.Request, *workspaceWorld)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ww, ok := a.world.workspaceByID[r.PathValue("id")]
		if !ok {
			writeError(w, http.StatusNotFound, "workspace not found")

			return
		}

		next(w, r, ww)
	}
}

// run resolves the run named by the request path.
func (a *api) run(next func(http.ResponseWriter, *http.Request, *runWorld)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rw, ok := a.world.runByID[r.PathValue("id")]
		if !ok {
			writeError(w, http.StatusNotFound, "run not found")

			return
		}

		next(w, r, rw)
	}
}

// plan resolves the run whose plan the request path names.
func (a *api) plan(next func(http.ResponseWriter, *http.Request, *runWorld)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rw, ok := a.world.planByID[r.PathValue("id")]
		if !ok {
			writeError(w, http.StatusNotFound, "plan not found")

			return
		}

		next(w, r, rw)
	}
}

// apply resolves the run whose apply the request path names.
func (a *api) apply(next func(http.ResponseWriter, *http.Request, *runWorld)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rw, ok := a.world.applyByID[r.PathValue("id")]
		if !ok {
			writeError(w, http.StatusNotFound, "apply not found")

			return
		}

		next(w, r, rw)
	}
}

// state resolves the state version named by the request path.
func (a *api) state(next func(http.ResponseWriter, *http.Request, *stateWorld)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sw, ok := a.world.stateByID[r.PathValue("id")]
		if !ok {
			writeError(w, http.StatusNotFound, "state version not found")

			return
		}

		next(w, r, sw)
	}
}
