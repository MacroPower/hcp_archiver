package view

import (
	"encoding/json"
	"fmt"
	"os"
	"path"
	"strings"

	"charm.land/bubbles/v2/list"

	tea "charm.land/bubbletea/v2"
)

// item is one list row: its display strings and, when it descends somewhere,
// the constructor of the screen it opens.
type item struct {
	open  func() (screen, error)
	title string
	desc  string
}

// Title returns the row's first line.
func (i item) Title() string { return i.title }

// Description returns the row's second line.
func (i item) Description() string { return i.desc }

// FilterValue exposes both lines to the list's / filter.
func (i item) FilterValue() string { return i.title + " " + i.desc }

// listScreen is a screen backed by a filterable list.
//
// Create instances with [newListScreen].
type listScreen struct {
	list list.Model
	name string
}

// newListScreen creates a new [listScreen] named name (its breadcrumb segment)
// over rows.
func newListScreen(name string, rows []item) *listScreen {
	entries := make([]list.Item, len(rows))
	for i := range rows {
		entries[i] = rows[i]
	}

	l := list.New(entries, list.NewDefaultDelegate(), 0, 0)
	l.SetShowTitle(false)
	l.DisableQuitKeybindings()

	return &listScreen{name: name, list: l}
}

// update handles navigation keys itself and forwards everything else to the
// list. While the filter prompt is being typed every key belongs to the list,
// so q and esc stay typable; with a filter applied, esc falls through to the
// list to clear it before a second esc pops the screen.
func (s *listScreen) update(msg tea.Msg) tea.Cmd {
	if key, ok := msg.(tea.KeyPressMsg); ok && s.list.FilterState() != list.Filtering {
		switch key.String() {
		case "enter":
			if it, ok := s.list.SelectedItem().(item); ok && it.open != nil {
				return push(it.open)
			}

			return nil

		case "esc", "backspace":
			if s.list.FilterState() == list.Unfiltered {
				return pop()
			}

		case "q":
			return tea.Quit
		}
	}

	var cmd tea.Cmd

	s.list, cmd = s.list.Update(msg)

	return cmd
}

// view renders the list.
func (s *listScreen) view() string { return s.list.View() }

// crumb names the screen's breadcrumb segment.
func (s *listScreen) crumb() string { return s.name }

// setSize sizes the list to the screen body.
func (s *listScreen) setSize(width, height int) {
	s.list.SetWidth(width)
	s.list.SetHeight(height)
}

// newOrgsScreen lists the archive's organizations.
func newOrgsScreen(orgs []*Org) screen {
	rows := make([]item, 0, len(orgs))

	for _, org := range orgs {
		rows = append(rows, item{
			title: org.Name,
			desc:  "organization",
			open: func() (screen, error) {
				return newOrgScreen(org), nil
			},
		})
	}

	return newListScreen("organizations", rows)
}

// newOrgScreen is one organization's home: its sections, mirroring the HCP
// sidebar.
func newOrgScreen(org *Org) screen {
	rows := []item{
		{
			title: "Projects",
			desc:  "projects and the workspaces they own",
			open: func() (screen, error) {
				return newProjectsScreen(org)
			},
		},
		{
			title: "Workspaces",
			desc:  "every workspace across all projects",
			open: func() (screen, error) {
				return newAllWorkspacesScreen(org)
			},
		},
		{
			title: "Organization",
			desc:  "organization metadata (org.json)",
			open: func() (screen, error) {
				return newFileViewer(org, "org.json", nil)
			},
		},
		{
			title: "Files",
			desc:  "browse the raw archive tree",
			open: func() (screen, error) {
				return newFilesScreen(org, "")
			},
		},
	}

	return newListScreen(org.Name, rows)
}

// newProjectsScreen lists an organization's projects.
func newProjectsScreen(org *Org) (screen, error) {
	projects, err := org.Projects()
	if err != nil {
		return nil, err
	}

	rows := make([]item, 0, len(projects))

	for _, project := range projects {
		workspaces, wsErr := org.Workspaces(project)
		if wsErr != nil {
			return nil, wsErr
		}

		rows = append(rows, item{
			title: project,
			desc:  countNoun(len(workspaces), "workspace", "workspaces"),
			open: func() (screen, error) {
				return newWorkspacesScreen(org, project, workspaces)
			},
		})
	}

	return newListScreen("projects", rows), nil
}

// newWorkspacesScreen lists one project's workspaces, and its stacks when it
// has any. When workspaces is non-nil the caller has already listed them (the
// projects screen reads them to size its count label); a nil listing is read
// here so the screen also stands alone.
func newWorkspacesScreen(org *Org, project string, workspaces []string) (screen, error) {
	if workspaces == nil {
		var err error

		workspaces, err = org.Workspaces(project)
		if err != nil {
			return nil, err
		}
	}

	stacks, err := org.Stacks(project)
	if err != nil {
		return nil, err
	}

	rows := make([]item, 0, len(workspaces)+len(stacks))

	for _, name := range workspaces {
		ws := org.Workspace(project, name)

		rows = append(rows, item{
			title: name,
			desc:  "workspace",
			open: func() (screen, error) {
				return newWorkspaceScreen(ws)
			},
		})
	}

	for _, name := range stacks {
		dir := path.Join("projects", project, "stacks", name)

		rows = append(rows, item{
			title: name,
			desc:  "stack",
			open: func() (screen, error) {
				return newFilesScreen(org, dir)
			},
		})
	}

	return newListScreen(project, rows), nil
}

// newAllWorkspacesScreen lists every workspace in the organization, each
// described by its owning project.
func newAllWorkspacesScreen(org *Org) (screen, error) {
	projects, err := org.Projects()
	if err != nil {
		return nil, err
	}

	var rows []item

	for _, project := range projects {
		workspaces, wsErr := org.Workspaces(project)
		if wsErr != nil {
			return nil, wsErr
		}

		for _, name := range workspaces {
			ws := org.Workspace(project, name)

			rows = append(rows, item{
				title: name,
				desc:  "project: " + project,
				open: func() (screen, error) {
					return newWorkspaceScreen(ws)
				},
			})
		}
	}

	return newListScreen("workspaces", rows), nil
}

// newWorkspaceScreen is one workspace's home: its sections, mirroring the HCP
// workspace tabs.
func newWorkspaceScreen(ws *Workspace) (screen, error) {
	runCount, err := subdirNames(ws.org.AbsPath(path.Join(ws.Dir(), "runs")))
	if err != nil {
		return nil, err
	}

	stateNames, err := ws.StateVersionNames()
	if err != nil {
		return nil, err
	}

	rows := []item{
		{
			title: "Overview",
			desc:  "workspace settings and metadata",
			open: func() (screen, error) {
				return newOverviewScreen(ws)
			},
		},
		{
			title: "Runs",
			desc:  countNoun(len(runCount), "run", "runs"),
			open: func() (screen, error) {
				return newRunsScreen(ws)
			},
		},
		{
			title: "States",
			desc:  countNoun(len(stateNames), "state version", "state versions"),
			open: func() (screen, error) {
				return newStatesScreen(ws)
			},
		},
		{
			title: "Variables",
			desc:  "workspace variables (sensitive values redacted)",
			open: func() (screen, error) {
				return newVariablesScreen(ws)
			},
		},
		{
			title: "Files",
			desc:  "browse the workspace's archived files",
			open: func() (screen, error) {
				return newFilesScreen(ws.org, ws.Dir())
			},
		},
	}

	return newListScreen(ws.Name, rows), nil
}

// newOverviewScreen renders a workspace's key settings above its full
// workspace.json.
func newOverviewScreen(ws *Workspace) (screen, error) {
	data, err := ws.Open(ws.File("workspace.json"))
	if err != nil {
		return nil, err
	}

	resources, err := DecodeResources(data)
	if err != nil || len(resources) != 1 {
		return newViewerScreen("overview", string(data)), nil //nolint:nilerr // The raw document still displays.
	}

	r := &resources[0]

	var b strings.Builder

	writeField := func(label, value string) {
		if value != "" {
			fmt.Fprintf(&b, "%-20s %s\n", label, value)
		}
	}

	writeField("workspace", ws.Name)
	writeField("id", r.ID)
	writeField("description", r.String("description"))
	writeField("terraform version", r.String("terraform-version"))
	writeField("execution mode", r.String("execution-mode"))

	if v, ok := r.BoolOK("auto-apply"); ok {
		writeField("auto apply", fmt.Sprintf("%t", v))
	}

	if v, ok := r.IntOK("resource-count"); ok {
		writeField("resource count", fmt.Sprintf("%d", v))
	}

	writeField("created at", r.String("created-at"))

	if repo, ok := r.Attributes["vcs-repo"].(map[string]any); ok {
		if id, ok := repo["identifier"].(string); ok {
			writeField("vcs repo", id)
		}
	}

	b.WriteString("\n─── workspace.json ───\n\n")
	b.Write(data)

	return newViewerScreen("overview", b.String()), nil
}

// newRunsScreen lists a workspace's runs, newest first, badged like the HCP run
// list.
func newRunsScreen(ws *Workspace) (screen, error) {
	runs, err := ws.Runs()
	if err != nil {
		return nil, err
	}

	rows := make([]item, 0, len(runs))

	for _, run := range runs {
		title := runGlyph(run.Status) + " " + run.ID
		if run.IsDestroy {
			title += " (destroy)"
		}

		var parts []string

		if run.Status != "" {
			parts = append(parts, run.Status)
		}

		if !run.CreatedAt.IsZero() {
			parts = append(parts, run.CreatedAt.Format("2006-01-02 15:04"))
		}

		if run.Message != "" {
			parts = append(parts, firstLine(run.Message))
		}

		rows = append(rows, item{
			title: title,
			desc:  strings.Join(parts, " · "),
			open: func() (screen, error) {
				return newRunScreen(ws, run)
			},
		})
	}

	return newListScreen("runs", rows), nil
}

// binaryExts marks archived blobs the text viewer cannot usefully show.
var binaryExts = map[string]struct{}{
	".zip": {},
	".gz":  {},
}

// newRunScreen is one run's detail: its summary and each archived artifact.
func newRunScreen(ws *Workspace, run Run) (screen, error) {
	artifacts, err := ws.RunArtifacts(run.ID)
	if err != nil {
		return nil, err
	}

	rows := []item{{
		title: "run.json",
		desc:  runSummary(run),
		open: func() (screen, error) {
			return newFileViewer(ws.org, path.Join(ws.Dir(), "runs", run.ID, "run.json"), ws)
		},
	}}

	for _, relPath := range artifacts {
		name := path.Base(relPath)

		desc, ok := artifactDesc[name]
		if !ok && strings.HasPrefix(name, "policy-check-") {
			desc = "policy check log"
		}

		// A binary artifact has no useful text rendering, so list it without an
		// open func rather than dumping raw bytes into the viewer, matching fileRow.
		if _, bin := binaryExts[path.Ext(name)]; bin {
			note := "binary (not viewable)"
			if desc != "" {
				note = desc + " · " + note
			}

			rows = append(rows, item{title: name, desc: note})

			continue
		}

		rows = append(rows, item{
			title: name,
			desc:  desc,
			open: func() (screen, error) {
				return newFileViewer(ws.org, relPath, ws)
			},
		})
	}

	return newListScreen(run.ID, rows), nil
}

// runSummary renders a run's one-line description for its detail screen.
func runSummary(run Run) string {
	parts := []string{}

	if run.Status != "" {
		parts = append(parts, run.Status)
	}

	if run.Source != "" {
		parts = append(parts, run.Source)
	}

	if run.TerraformVersion != "" {
		parts = append(parts, "terraform "+run.TerraformVersion)
	}

	if run.HasChanges {
		parts = append(parts, "has changes")
	}

	if len(parts) == 0 {
		return "run summary"
	}

	return strings.Join(parts, " · ")
}

// newStatesScreen lists a workspace's state versions, newest first.
func newStatesScreen(ws *Workspace) (screen, error) {
	versions, err := ws.StateVersions()
	if err != nil {
		return nil, err
	}

	rows := make([]item, 0, len(versions))

	for _, sv := range versions {
		title := fmt.Sprintf("serial %d", sv.Serial)
		if !sv.CreatedAt.IsZero() {
			title += " — " + sv.CreatedAt.Format("2006-01-02 15:04")
		}

		parts := []string{}
		if sv.ID != "" {
			parts = append(parts, sv.ID)
		}

		if sv.Size > 0 {
			parts = append(parts, humanBytes(sv.Size))
		}

		if sv.Status != "" {
			parts = append(parts, sv.Status)
		}

		rows = append(rows, item{
			title: title,
			desc:  strings.Join(parts, " · "),
			open: func() (screen, error) {
				return newStateScreen(ws, sv)
			},
		})
	}

	return newListScreen("states", rows), nil
}

// newStateScreen is one state version's detail: its metadata sidecar and
// whichever state blobs were archived.
func newStateScreen(ws *Workspace, sv StateVersion) (screen, error) {
	rows := []item{{
		title: "meta.json",
		desc:  "state version metadata",
		open: func() (screen, error) {
			return newFileViewer(ws.org, ws.StateMetaPath(&sv), ws)
		},
	}}

	if sv.HasRaw {
		rows = append(rows, item{
			title: "raw state",
			desc:  "the .tfstate document (sensitive values in cleartext)",
			open: func() (screen, error) {
				return newFileViewer(ws.org, ws.RawStatePath(&sv), ws)
			},
		})
	}

	if sv.HasJSON {
		rows = append(rows, item{
			title: "JSON-format state",
			desc:  "the machine-readable state rendering",
			open: func() (screen, error) {
				return newFileViewer(ws.org, ws.JSONStatePath(&sv), ws)
			},
		})
	}

	crumb := sv.ID
	if crumb == "" {
		crumb = sv.Stem
	}

	return newListScreen(crumb, rows), nil
}

// newVariablesScreen lists a workspace's variables the way the HCP variables
// tab does: key, value (sensitive ones archived redacted), and category.
func newVariablesScreen(ws *Workspace) (screen, error) {
	data, err := ws.Open(ws.File("variables.json"))
	if err != nil {
		return nil, err
	}

	resources, err := DecodeResources(data)
	if err != nil {
		// A malformed-but-readable variables.json still shows its raw bytes,
		// matching newOverviewScreen, rather than dead-ending the tab on a
		// decode error the operator cannot see past.
		return newViewerScreen("variables", string(data)), nil //nolint:nilerr // The raw document still displays.
	}

	rows := make([]item, 0, len(resources))

	for _, r := range resources {
		parts := []string{r.String("category")}

		if r.Bool("sensitive") {
			parts = append(parts, "sensitive")
		}

		if r.Bool("hcl") {
			parts = append(parts, "hcl")
		}

		if v := firstLine(r.String("value")); v != "" {
			parts = append(parts, "= "+v)
		}

		rows = append(rows, item{
			title: r.String("key"),
			desc:  strings.Join(parts, " · "),
			open: func() (screen, error) {
				return newResourceViewer(r)
			},
		})
	}

	return newListScreen("variables", rows), nil
}

// newResourceViewer renders one decoded resource as indented JSON.
func newResourceViewer(r Resource) (screen, error) {
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrDecode, err)
	}

	name := r.String("key")
	if name == "" {
		name = r.ID
	}

	return newViewerScreen(name, string(data)), nil
}

// newFilesScreen browses the loose archive tree at an archive-relative
// directory; sealed objects surface through the workspace screens instead.
func newFilesScreen(org *Org, dir string) (screen, error) {
	entries, err := os.ReadDir(org.AbsPath(dir))
	if err != nil {
		return nil, fmt.Errorf("read %q: %w", dir, err)
	}

	var rows []item

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}

		sub := path.Join(dir, e.Name())

		rows = append(rows, item{
			title: e.Name() + "/",
			desc:  "directory",
			open: func() (screen, error) {
				return newFilesScreen(org, sub)
			},
		})
	}

	for _, e := range entries {
		if e.IsDir() {
			continue
		}

		rows = append(rows, fileRow(org, dir, e))
	}

	crumb := path.Base(dir)
	if dir == "" {
		crumb = "files"
	}

	return newListScreen(crumb, rows), nil
}

// fileRow builds one loose file's browser row; binary blobs list without
// opening.
func fileRow(org *Org, dir string, e os.DirEntry) item {
	name := e.Name()
	relPath := path.Join(dir, name)

	desc := "file"

	info, err := e.Info()
	if err == nil {
		desc = humanBytes(info.Size())
	}

	if _, ok := binaryExts[path.Ext(name)]; ok {
		return item{title: name, desc: desc + " · binary (not viewable)"}
	}

	return item{
		title: name,
		desc:  desc,
		open: func() (screen, error) {
			return newFileViewer(org, relPath, nil)
		},
	}
}

// newFileViewer opens the object at an archive-relative path in the viewer,
// through the workspace's sealed-form lookup when one is given and as a loose
// file otherwise.
func newFileViewer(org *Org, relPath string, ws *Workspace) (screen, error) {
	var (
		data []byte
		err  error
	)

	if ws != nil {
		data, err = ws.Open(relPath)
	} else {
		data, err = org.ReadFile(relPath)
	}

	if err != nil {
		return nil, err
	}

	return newViewerScreen(path.Base(relPath), string(data)), nil
}

// countNoun renders "N singular" or "N plural".
func countNoun(n int, singular, plural string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, singular)
	}

	return fmt.Sprintf("%d %s", n, plural)
}

// firstLine returns the first line of s, truncated to a list-friendly width.
func firstLine(s string) string {
	s, _, _ = strings.Cut(s, "\n")

	const maxLen = 60

	// Truncate on a rune boundary: a byte-count cut would split a multi-byte
	// rune whose bytes straddle maxLen and emit invalid UTF-8 into the list.
	if r := []rune(s); len(r) > maxLen {
		return string(r[:maxLen]) + "…"
	}

	return s
}
