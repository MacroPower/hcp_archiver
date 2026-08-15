package export

import (
	"errors"
	"fmt"
	"path"
	"strconv"
	"strings"

	"go.jacobcolvin.com/hcp_archiver/theme"
	"go.jacobcolvin.com/hcp_archiver/view"
)

// readmeFile is the user-authored workspace readme leaf, archived and exported
// under the same name.
const readmeFile = "readme.md"

// cliName is the archiver binary the export's retrieval snippets invoke.
const cliName = "hcp_archiver"

// renderWorkspace writes one workspace's page beneath its org-prefixed archive
// path, which the export tree mirrors, and its readme copy beside it when the
// archive holds one.
func (e *Exporter) renderWorkspace(ws *view.Workspace, orgName string) error {
	outDir := path.Join(orgName, ws.Dir())

	p := &page{}

	overview := one(decodeTolerant(ws.Open(ws.File("workspace.json"))))
	p.h1(titleOr(overview, ws.Name))

	if overview != nil {
		p.kv(workspaceOverview(overview))
	}

	hasReadme, err := e.copyReadme(ws, outDir)
	if err != nil {
		return err
	}

	if hasReadme {
		p.para("See the workspace's " + mdLink("readme", readmeFile) + ".")
	}

	if rows := variableRows(decodeTolerant(ws.Open(ws.File("variables.json")))); len(rows) > 0 {
		p.h2("Variables")
		p.table(variableHeaders, rows)
	}

	err = workspaceRuns(p, ws, orgName)
	if err != nil {
		return err
	}

	err = workspaceStates(p, ws, orgName)
	if err != nil {
		return err
	}

	return e.write(path.Join(outDir, indexFile), p.bytes())
}

// workspaceOverview picks the allowlisted settings a workspace page shows,
// mirroring the interactive browser's overview fields.
func workspaceOverview(r *view.Resource) [][2]string {
	rows := [][2]string{
		{labelID, escapeCell(r.ID)},
		{labelDescription, escapeCell(r.String("description"))},
		{"Terraform version", escapeCell(r.String("terraform-version"))},
		{"Execution mode", escapeCell(r.String("execution-mode"))},
		{"Auto apply", boolCell(r, "auto-apply")},
	}

	if n, ok := r.IntOK("resource-count"); ok {
		rows = append(rows, [2]string{"Resource count", strconv.FormatInt(n, 10)})
	}

	rows = append(rows, [2]string{labelCreated, fmtTime(r.Time("created-at"))})

	if repo, ok := r.Attributes["vcs-repo"].(map[string]any); ok {
		if id, ok := repo["identifier"].(string); ok {
			rows = append(rows, [2]string{"VCS repo", escapeCell(id)})
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

// workspaceRuns renders the workspace's run history, newest first. Each row
// carries the run's metadata and the names of its archived artifacts; the
// artifacts themselves (plan and apply logs, cost estimates) can embed secret
// values and are represented by name alone, with a snippet naming the CLI
// commands that retrieve them.
func workspaceRuns(p *page, ws *view.Workspace, orgName string) error {
	runs, err := ws.Runs()
	if err != nil {
		return fmt.Errorf("list runs of %q: %w", ws.Name, err)
	}

	if len(runs) == 0 {
		return nil
	}

	var example string

	rows := make([][]string, 0, len(runs))

	for _, run := range runs {
		artifacts, artErr := ws.RunArtifacts(run.ID)
		if artErr != nil {
			return fmt.Errorf("list artifacts of run %q: %w", run.ID, artErr)
		}

		if example == "" && len(artifacts) > 0 {
			example = path.Join(orgName, artifacts[0])
		}

		names := make([]string, 0, len(artifacts))
		for _, rel := range artifacts {
			names = append(names, path.Base(rel))
		}

		rows = append(rows, []string{
			escapeCell(run.ID),
			fmtTime(run.CreatedAt),
			escapeCell(run.Status),
			escapeCell(run.Message),
			escapeCell(run.Source),
			escapeCell(run.TerraformVersion),
			strconv.FormatBool(run.IsDestroy),
			strconv.FormatBool(run.HasChanges),
			escapeCell(strings.Join(names, ", ")),
		})
	}

	p.h2("Runs")
	p.para(theme.CountNoun(len(runs), "archived run", "archived runs") +
		". Artifact contents are withheld; the backup holds them in full:")
	retrievalSnippet(p, example, path.Join(orgName, ws.Dir(), "runs"))
	p.table(
		[]string{
			labelID,
			labelCreated,
			"Status",
			"Message",
			"Source",
			"Terraform",
			"Destroy",
			"Changes",
			"Archived artifacts",
		},
		rows,
	)

	return nil
}

// workspaceStates renders the workspace's state-version history, newest
// first: metadata and which blob forms the backup holds, never the state
// itself, whose raw form embeds sensitive values in cleartext. A snippet
// names the CLI commands that retrieve the blobs.
func workspaceStates(p *page, ws *view.Workspace, orgName string) error {
	versions, err := ws.StateVersions()
	if err != nil {
		return fmt.Errorf("list state versions of %q: %w", ws.Name, err)
	}

	if len(versions) == 0 {
		return nil
	}

	var example string

	rows := make([][]string, 0, len(versions))

	for _, sv := range versions {
		if example == "" {
			switch {
			case sv.HasRaw:
				example = path.Join(orgName, ws.RawStatePath(&sv))
			case sv.HasJSON:
				example = path.Join(orgName, ws.JSONStatePath(&sv))
			}
		}

		rows = append(rows, []string{
			escapeCell(sv.ID),
			fmtTime(sv.CreatedAt),
			strconv.FormatInt(sv.Serial, 10),
			escapeCell(sv.Status),
			theme.HumanBytes(sv.Size),
			stateForms(sv),
		})
	}

	p.h2("State versions")
	p.para(theme.CountNoun(len(versions), "archived state version", "archived state versions") +
		". State contents are withheld; the backup holds them in full:")
	retrievalSnippet(p, example, path.Join(orgName, ws.Dir(), "state-versions"))
	p.table([]string{labelID, labelCreated, "Serial", "Status", "Size", "Archived blobs"}, rows)

	return nil
}

// retrievalSnippet writes the shell commands that retrieve a section's
// withheld content: a show of one example object when the section holds any,
// and an unseal of the whole section directory. Archive paths are
// single-quoted so a name with spaces stays one shell argument; the
// archive-dir and output-dir placeholders stay for the reader, who alone
// knows where the archive and the recovered files should live.
func retrievalSnippet(p *page, example, dir string) {
	lines := make([]string, 0, 2)

	if example != "" {
		lines = append(lines, fmt.Sprintf("%s show <archive-dir> '%s'", cliName, example))
	}

	lines = append(lines, fmt.Sprintf("%s unseal <archive-dir> '%s' --target <output-dir>", cliName, dir))

	p.code("sh", lines...)
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
