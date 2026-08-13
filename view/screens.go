package view

import (
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/list"
	"charm.land/bubbles/v2/table"
	"charm.land/lipgloss/v2"

	tea "charm.land/bubbletea/v2"

	"go.jacobcolvin.com/hcp_archiver/theme"
)

// item is one list row: its display strings and, when it descends somewhere,
// the constructor of the screen it opens. A row for an unsealable scope (a
// project or a workspace) also carries the constructor of its unseal prompt.
type item struct {
	open   func() (screen, error)
	unseal func() (screen, error)
	title  string
	desc   string
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

	l := newThemedList(entries)
	l.SetShowTitle(false)
	l.DisableQuitKeybindings()

	s := &listScreen{name: name, list: l}

	// Whether the u key acts here is inferred from the rows themselves, so a
	// screen can never advertise a dead action key or leave a live one
	// shadowed by the stock paging binding.
	if slices.ContainsFunc(rows, func(it item) bool { return it.unseal != nil }) {
		s.enableUnsealHint()
	}

	return s
}

// update handles navigation keys itself and forwards everything else to the
// list. While the filter prompt is being typed every key belongs to the list,
// so q and esc stay typable; with a filter applied, either back key clears it
// before a second press pops the screen.
func (s *listScreen) update(msg tea.Msg) tea.Cmd {
	if press, ok := msg.(tea.KeyPressMsg); ok && s.list.FilterState() != list.Filtering {
		switch press.String() {
		case "enter":
			if it, ok := s.list.SelectedItem().(item); ok && it.open != nil {
				return push(it.open)
			}

			return nil

		case keyEsc, keyBackspace:
			if s.list.FilterState() == list.Unfiltered {
				return pop()
			}

			// With a filter applied, the list clears it on esc but binds nothing
			// to backspace; clear explicitly so both back keys share the
			// clear-then-pop two-step.
			s.list.ResetFilter()

			return nil

		case "q":
			return tea.Quit

		case "u":
			if it, ok := s.list.SelectedItem().(item); ok && it.unseal != nil {
				return push(it.unseal)
			}

			// A row without an unseal action leaves u to the list, whose stock
			// keymap binds it to paging back.
		}
	}

	var cmd tea.Cmd

	s.list, cmd = s.list.Update(msg)

	return cmd
}

// enableUnsealHint frees u for the unseal action: it drops u from the list's
// stock prev-page keys (left, h, pgup, and b remain) and advertises the key
// in the list's help. [newListScreen] calls it when any row carries the
// action.
func (s *listScreen) enableUnsealHint() {
	s.list.KeyMap.PrevPage = key.NewBinding(
		key.WithKeys("left", "h", "pgup", "b"),
		key.WithHelp("←/h/pgup", "prev page"),
	)

	unseal := key.NewBinding(key.WithKeys("u"), key.WithHelp("u", "unseal"))
	s.list.AdditionalShortHelpKeys = func() []key.Binding { return []key.Binding{unseal} }
	s.list.AdditionalFullHelpKeys = func() []key.Binding { return []key.Binding{unseal} }
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
			desc:  theme.CountNoun(len(workspaces), "workspace", "workspaces"),
			open: func() (screen, error) {
				return newWorkspacesScreen(org, project, workspaces)
			},
			unseal: projectUnsealPrompt(org, project),
		})
	}

	return newListScreen("projects", rows), nil
}

// unsealPrompt builds the unseal-prompt constructor a project or workspace
// row carries: confirming the target runs plan and pushes the progress screen
// over the planned jobs.
func unsealPrompt(org *Org, label string, plan func() ([]unsealJob, error)) func() (screen, error) {
	return func() (screen, error) {
		return newUnsealPromptScreen(label, org.remote != nil, defaultTarget(),
			func(target string) (screen, error) {
				jobs, err := plan()
				if err != nil {
					return nil, err
				}

				return newUnsealProgressScreen(org, target, jobs, label), nil
			}), nil
	}
}

// projectUnsealPrompt is [unsealPrompt] planning a whole project.
func projectUnsealPrompt(org *Org, project string) func() (screen, error) {
	return unsealPrompt(org, "project "+project, func() ([]unsealJob, error) {
		return org.planProjectUnseal(project)
	})
}

// workspaceUnsealPrompt is [unsealPrompt] planning one workspace.
func workspaceUnsealPrompt(org *Org, ws *Workspace) func() (screen, error) {
	return unsealPrompt(org, "workspace "+ws.Name, func() ([]unsealJob, error) {
		return org.planWorkspaceUnseal(ws)
	})
}

// defaultTarget is the pre-filled unseal destination: an "unsealed" directory
// under the working directory the browser was launched from.
func defaultTarget() string {
	cwd, err := os.Getwd()
	if err != nil {
		return "unsealed"
	}

	return filepath.Join(cwd, "unsealed")
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
			unseal: workspaceUnsealPrompt(org, ws),
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
				unseal: workspaceUnsealPrompt(org, ws),
			})
		}
	}

	return newListScreen("workspaces", rows), nil
}

// newWorkspaceScreen is one workspace's home: its sections, mirroring the HCP
// workspace tabs.
func newWorkspaceScreen(ws *Workspace) (screen, error) {
	runIDs, err := ws.runIDs()
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
			desc:  theme.CountNoun(len(runIDs), "run", "runs"),
			open: func() (screen, error) {
				return newRunsScreen(ws)
			},
		},
		{
			title: "States",
			desc:  theme.CountNoun(len(stateNames), "state version", "state versions"),
			open: func() (screen, error) {
				return newStatesScreen(ws)
			},
		},
		{
			title: "Variables",
			desc:  "workspace variables",
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
		return newYAMLViewerScreen("overview", string(data)), nil //nolint:nilerr // The raw document still displays.
	}

	r := &resources[0]

	var rows []table.Row

	addField := func(label, value string) {
		if value != "" {
			rows = append(rows, table.Row{label, value})
		}
	}

	addField("workspace", ws.Name)
	addField("id", r.ID)
	addField("description", r.String("description"))
	addField("terraform version", r.String("terraform-version"))
	addField("execution mode", r.String("execution-mode"))

	if v, ok := r.BoolOK("auto-apply"); ok {
		addField("auto apply", strconv.FormatBool(v))
	}

	if v, ok := r.IntOK("resource-count"); ok {
		addField("resource count", strconv.FormatInt(v, 10))
	}

	addField("created at", r.String("created-at"))

	if repo, ok := r.Attributes["vcs-repo"].(map[string]any); ok {
		if id, ok := repo["identifier"].(string); ok {
			addField("vcs repo", id)
		}
	}

	// The label column is sized to its longest label plus the inter-column gap
	// (the themed table's cells are padding-free); the value column flexes on
	// resize, so its width here is a placeholder. The columns are headerless:
	// the table's mandatory header row renders as a blank spacer line, which
	// reads cleaner over a key/value block than "field"/"value" titles would.
	labelWidth := 0
	for _, row := range rows {
		labelWidth = max(labelWidth, lipgloss.Width(row[0]))
	}

	cols := []table.Column{
		{Title: "", Width: labelWidth + 2},
		{Title: "", Width: 1},
	}

	return newTableViewerScreen("overview", cols, rows, string(data)), nil
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
			parts = append(parts, run.CreatedAt.Format(theme.TimeLayout))
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
			title += " — " + sv.CreatedAt.Format(theme.TimeLayout)
		}

		parts := []string{}
		if sv.ID != "" {
			parts = append(parts, sv.ID)
		}

		if sv.Size > 0 {
			parts = append(parts, theme.HumanBytes(sv.Size))
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
// tab does: key, value (stored as the API returned it), and category.
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
		return newYAMLViewerScreen("variables", string(data)), nil //nolint:nilerr // The raw document still displays.
	}

	rows := make([]item, 0, len(resources))

	for _, r := range resources {
		var parts []string

		if c := r.String("category"); c != "" {
			parts = append(parts, c)
		}

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

	return newYAMLViewerScreen(name, string(data)), nil
}

// newFilesScreen browses the merged archive tree at an archive-relative
// directory, local files beside those only the organization's mirror holds;
// sealed objects surface through the workspace screens instead.
func newFilesScreen(org *Org, dir string) (screen, error) {
	entries, err := org.Entries(dir)
	if err != nil {
		return nil, err
	}

	var rows []item

	for _, e := range entries {
		if !e.Dir {
			continue
		}

		sub := path.Join(dir, e.Name)

		rows = append(rows, item{
			title: e.Name + "/",
			desc:  "directory",
			open: func() (screen, error) {
				return newFilesScreen(org, sub)
			},
		})
	}

	for _, e := range entries {
		if e.Dir {
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

// fileRow builds one merged file's browser row; binary blobs list without
// opening, and a file the mirror holds but the disk does not yet says so.
func fileRow(org *Org, dir string, e TreeEntry) item {
	name := e.Name
	relPath := path.Join(dir, name)

	desc := theme.HumanBytes(e.Size)
	if e.Remote {
		desc += " · remote"
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

	// JSON documents (metadata sidecars, structured plans, state blobs) go
	// through the highlighting viewport; logs and other plain text scroll raw.
	if path.Ext(relPath) == ".json" {
		return newYAMLViewerScreen(path.Base(relPath), string(data)), nil
	}

	return newViewerScreen(path.Base(relPath), string(data)), nil
}

// firstLine returns the first line of s, truncated to a list-friendly width.
func firstLine(s string) string {
	// Cut at the first CR or LF: archived text carries arbitrary user content
	// (commit messages, variable values) with CRLF or bare-CR endings, and a
	// stray \r in a list row moves the terminal cursor back to column 0,
	// garbling the rendered frame.
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}

	const maxLen = 60

	// Truncate on a rune boundary: a byte-count cut would split a multi-byte
	// rune whose bytes straddle maxLen and emit invalid UTF-8 into the list.
	if r := []rune(s); len(r) > maxLen {
		return string(r[:maxLen]) + "…"
	}

	return s
}
