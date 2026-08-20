package export

import (
	"errors"
	"fmt"
	"path"
	"strconv"
	"strings"

	"go.jacobcolvin.com/hcp_archiver/pkg/theme"
	"go.jacobcolvin.com/hcp_archiver/pkg/view"
)

// readmeFile is the user-authored workspace readme leaf, archived and exported
// under the same name.
const readmeFile = "readme.md"

// renderWorkspace writes one workspace's page beneath its org-prefixed archive
// path, which the export tree mirrors, and its readme copy beside it when the
// archive holds one.
func (e *Exporter) renderWorkspace(ws *view.Workspace, orgName string) error {
	outDir := path.Join(orgName, ws.Dir())

	overview := one(decodeTolerant(ws.Open(ws.File("workspace.json"))))

	pageCtx := WorkspacePage{Title: titleOr(overview, ws.Name)}

	if overview != nil {
		pageCtx.Info = workspaceOverview(overview)
	}

	hasReadme, err := e.copyReadme(ws, outDir)
	if err != nil {
		return err
	}

	pageCtx.HasReadme = hasReadme
	pageCtx.Variables = buildVariables(decodeTolerant(ws.Open(ws.File("variables.json"))))

	pageCtx.Runs, err = workspaceRuns(ws, orgName)
	if err != nil {
		return err
	}

	pageCtx.States, err = workspaceStates(ws, orgName)
	if err != nil {
		return err
	}

	data, err := e.renderPage("workspace.md.tmpl", pageCtx)
	if err != nil {
		return err
	}

	return e.write(path.Join(outDir, indexFile), data)
}

// workspaceOverview picks the allowlisted settings a workspace page shows,
// mirroring the interactive browser's overview fields.
func workspaceOverview(r *view.Resource) []KV {
	rows := addKV(nil, labelID, r.ID)
	rows = addKV(rows, labelDescription, r.String("description"))
	rows = addKV(rows, "Terraform version", r.String("terraform-version"))
	rows = addKV(rows, "Execution mode", r.String("execution-mode"))
	rows = addKV(rows, "Auto apply", boolCell(r, "auto-apply"))

	if n, ok := r.IntOK("resource-count"); ok {
		rows = addKV(rows, "Resource count", strconv.FormatInt(n, 10))
	}

	rows = addKV(rows, labelCreated, fmtTime(r.Time("created-at")))

	if repo, ok := r.Attributes["vcs-repo"].(map[string]any); ok {
		if id, ok := repo["identifier"].(string); ok {
			rows = addKV(rows, "VCS repo", id)
		}
	}

	return rows
}

// copyReadme copies the workspace's archived readme verbatim beneath outDir,
// reporting whether one exists. The readme is user-authored markdown: inlined
// into the generated page its own headings would corrupt the layout, while as
// a sibling file it renders as its own page and needs no escaping.
func (e *Exporter) copyReadme(ws *view.Workspace, outDir string) (bool, error) {
	data, err := ws.Open(ws.File(readmeFile))

	switch {
	case errors.Is(err, view.ErrObjectNotFound):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("read readme of %q: %w", ws.Name, err)
	}

	return true, e.write(path.Join(outDir, readmeFile), data)
}

// workspaceRuns builds the workspace's run history, newest first, nil when it
// has none. Each row carries the run's metadata and the names of its archived
// artifacts; the artifacts themselves (plan and apply logs, cost estimates)
// can embed secret values and are represented by name alone, with the page's
// snippet naming the CLI commands that retrieve them.
func workspaceRuns(ws *view.Workspace, orgName string) (*RunsSection, error) {
	runs, err := ws.Runs()
	if err != nil {
		return nil, fmt.Errorf("list runs of %q: %w", ws.Name, err)
	}

	if len(runs) == 0 {
		return nil, nil //nolint:nilnil // No runs means no section, not an error.
	}

	// One listing for the whole history: a per-run call re-lists the runs
	// directory for every run, and asks the filesystem about the runs sealing
	// has already emptied. The trades are the error message, which names the
	// workspace rather than the run whose listing failed, and a run that
	// appears between this listing and the one above, which renders with no
	// artifacts until the next export.
	byRun, err := ws.AllRunArtifacts()
	if err != nil {
		return nil, fmt.Errorf("list run artifacts of %q: %w", ws.Name, err)
	}

	section := &RunsSection{
		Count: len(runs),
		Dir:   path.Join(orgName, ws.Dir(), "runs"),
		Rows:  make([]RunRow, 0, len(runs)),
	}

	for _, run := range runs {
		artifacts := byRun[run.ID]

		if section.Example == "" && len(artifacts) > 0 {
			section.Example = path.Join(orgName, artifacts[0])
		}

		names := make([]string, 0, len(artifacts))
		for _, rel := range artifacts {
			names = append(names, path.Base(rel))
		}

		section.Rows = append(section.Rows, RunRow{
			ID:               run.ID,
			Created:          fmtTime(run.CreatedAt),
			Status:           run.Status,
			Message:          run.Message,
			Source:           run.Source,
			TerraformVersion: run.TerraformVersion,
			IsDestroy:        run.IsDestroy,
			HasChanges:       run.HasChanges,
			Artifacts:        names,
		})
	}

	return section, nil
}

// workspaceStates builds the workspace's state-version history, newest first,
// nil when it has none: metadata and which blob forms the backup holds, never
// the state itself, whose raw form embeds sensitive values in cleartext. The
// page's snippet names the CLI commands that retrieve the blobs.
func workspaceStates(ws *view.Workspace, orgName string) (*StatesSection, error) {
	versions, err := ws.StateVersions()
	if err != nil {
		return nil, fmt.Errorf("list state versions of %q: %w", ws.Name, err)
	}

	if len(versions) == 0 {
		return nil, nil //nolint:nilnil // No versions means no section, not an error.
	}

	section := &StatesSection{
		Count: len(versions),
		Dir:   path.Join(orgName, ws.Dir(), "state-versions"),
		Rows:  make([]StateRow, 0, len(versions)),
	}

	for _, sv := range versions {
		if section.Example == "" {
			switch {
			case sv.HasRaw:
				section.Example = path.Join(orgName, ws.RawStatePath(&sv))
			case sv.HasJSON:
				section.Example = path.Join(orgName, ws.JSONStatePath(&sv))
			}
		}

		section.Rows = append(section.Rows, StateRow{
			ID:      sv.ID,
			Created: fmtTime(sv.CreatedAt),
			Serial:  sv.Serial,
			Status:  sv.Status,
			Size:    theme.HumanBytes(sv.Size),
			Forms:   stateForms(sv),
		})
	}

	return section, nil
}

// stateForms names which state blob forms the backup holds for a version.
func stateForms(sv view.StateVersion) string {
	var forms []string

	if sv.HasRaw {
		forms = append(forms, "raw")
	}

	if sv.HasJSON {
		forms = append(forms, "json")
	}

	return strings.Join(forms, ", ")
}
