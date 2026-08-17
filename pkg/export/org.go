package export

import (
	"context"
	"fmt"
	"path"
	"strconv"
	"time"

	"go.jacobcolvin.com/hcp_archiver/pkg/theme"
	"go.jacobcolvin.com/hcp_archiver/pkg/view"
)

// decodeTolerant decodes an archived JSON:API document from a reader's (data,
// error) result, answering nil when the file is absent, unreadable, or
// malformed: a damaged leaf degrades its rows rather than failing the export,
// matching how the archive browser tolerates one. It wraps any read directly:
// decodeTolerant(org.ReadFile(rel)) or decodeTolerant(ws.Open(rel)).
func decodeTolerant(data []byte, err error) []view.Resource {
	if err != nil {
		return nil
	}

	resources, err := view.DecodeResources(data)
	if err != nil {
		return nil
	}

	return resources
}

// one returns the document's resource when it holds exactly one, nil
// otherwise, so a page can guard its allowlisted fields on a single pointer.
func one(resources []view.Resource) *view.Resource {
	if len(resources) != 1 {
		return nil
	}

	return &resources[0]
}

// titleOr returns the resource's name attribute as a page title, falling back
// to the on-disk directory name when the resource is nil or unnamed.
func titleOr(r *view.Resource, fallback string) string {
	if r != nil {
		if name := r.String("name"); name != "" {
			return name
		}
	}

	return fallback
}

// subdirNames returns the subdirectory names under an org-relative directory,
// answering nil when it is absent.
func subdirNames(org *view.Org, rel string) []string {
	entries, err := org.Entries(rel)
	if err != nil {
		return nil
	}

	var names []string

	for _, e := range entries {
		if e.Dir {
			names = append(names, e.Name)
		}
	}

	return names
}

// countEntries counts the archived objects under an org-relative directory,
// answering zero when it is absent. [*view.Org.Entries] already hides the
// archive's bookkeeping and folds each eviction stub onto the object it
// stands in for, so every child counts once.
func countEntries(org *view.Org, rel string) int {
	entries, err := org.Entries(rel)
	if err != nil {
		return 0
	}

	return len(entries)
}

// fmtTime renders a timestamp in the archive's shared layout, empty when
// zero so [page.kv] skips the row.
func fmtTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}

	return t.Format(theme.TimeLayout)
}

// boolCell renders a resource's bool attribute for a key/value row: absent
// stays empty (the row is skipped), present renders explicitly so a false
// still shows.
func boolCell(r *view.Resource, key string) string {
	v, ok := r.BoolOK(key)
	if !ok {
		return ""
	}

	return strconv.FormatBool(v)
}

// sensitiveMarker is the value cell a sensitive variable shows in place of
// its stored value.
const sensitiveMarker = "(sensitive)"

// buildVariables builds variable-like resources (workspace variables,
// variable-set variables, policy-set parameters) into [Variable] rows, gating
// the value on the sensitive flag. A sensitive variable's stored value is
// never read, even though the API blanks it upstream; the export does not
// rely on that behavior, and the redaction happens here, before any template
// runs, so no override can reach the value either.
func buildVariables(resources []view.Resource) []Variable {
	rows := make([]Variable, 0, len(resources))

	for i := range resources {
		r := &resources[i]

		value := sensitiveMarker
		if !r.Bool("sensitive") {
			value = r.String("value")
		}

		rows = append(rows, Variable{
			Key:      r.String("key"),
			Category: r.String("category"),
			HCL:      boolCell(r, "hcl"),
			Value:    value,
		})
	}

	return rows
}

// Key/value labels shared across pages.
const (
	labelCreated     = "Created"
	labelDescription = "Description"
	labelID          = "ID"
)

// orgSection is one org-scope category rendered as its own page beneath the
// organization's directory: a template name and the builder of the data it
// renders.
type orgSection struct {
	build func(org *view.Org, ids []string) any
	tmpl  string
	title string
	dir   string
	noun  string
	nouns string
}

var (
	// The categories that earn a page: those whose archive directories are
	// keyed by opaque ids, folded into one readable page each.
	orgSections = []orgSection{
		{
			title: "Teams", dir: "teams", noun: "team", nouns: "teams",
			tmpl: "teams.md.tmpl", build: buildTeamsPage,
		},
		{
			title: "Variable sets", dir: "variable-sets",
			noun: "variable set", nouns: "variable sets",
			tmpl: "variable-sets.md.tmpl", build: buildVariableSetsPage,
		},
		{
			title: "Policy sets", dir: "policy-sets",
			noun: "policy set", nouns: "policy sets",
			tmpl: "policy-sets.md.tmpl", build: buildPolicySetsPage,
		},
	}

	// The count-only categories the org index reports: the per-object
	// directories counted by entry, then the single-list files counted by
	// decoded resource. Audit trails (configuration plus page files) carry no
	// meaningful entry count and are omitted.
	orgInventoryDirs = []struct{ label, dir string }{
		{"Users", "users"},
		{"Agent pools", "agent-pools"},
		{"OAuth clients", "oauth-clients"},
		{"Configuration versions", "config-versions"},
		{"Registry modules", "registry/modules"},
		{"Registry no-code modules", "registry/no-code-modules"},
		{"Registry providers", "registry/providers"},
		{"Registry GPG keys", "registry/gpg-keys"},
	}

	orgInventoryFiles = []struct{ label, file string }{
		{"Memberships", "memberships.json"},
		{"Run tasks", "run-tasks.json"},
	}
)

// projectListing carries one project's directory listings, gathered once per
// run and shared by every page that lists or counts them.
type projectListing struct {
	name       string
	workspaces []string
	stacks     []string
}

// projectListings gathers each project's workspace and stack names.
func projectListings(org *view.Org, projects []string) ([]projectListing, error) {
	listings := make([]projectListing, 0, len(projects))

	for _, project := range projects {
		workspaces, err := org.Workspaces(project)
		if err != nil {
			return nil, fmt.Errorf("list workspaces of %q: %w", project, err)
		}

		stacks, err := org.Stacks(project)
		if err != nil {
			return nil, fmt.Errorf("list stacks of %q: %w", project, err)
		}

		listings = append(listings, projectListing{name: project, workspaces: workspaces, stacks: stacks})
	}

	return listings, nil
}

// renderOrg writes one organization's subtree: its index, its org-scope
// section pages, and its projects. Every directory is listed once here and
// the results shared, so a category is never scanned again for a later page.
//
//nolint:contextcheck // Mirror read-throughs run under the archive's stored browse context (see view.WithContext).
func (e *Exporter) renderOrg(ctx context.Context, org *view.Org) error {
	projects, err := org.Projects()
	if err != nil {
		return fmt.Errorf("list projects of %q: %w", org.Name, err)
	}

	listings, err := projectListings(org, projects)
	if err != nil {
		return err
	}

	sectionIDs := make([][]string, len(orgSections))
	for i, section := range orgSections {
		sectionIDs[i] = subdirNames(org, section.dir)
	}

	err = e.writeOrgIndex(org, len(projects), sectionIDs)
	if err != nil {
		return err
	}

	for i, section := range orgSections {
		if len(sectionIDs[i]) == 0 {
			continue
		}

		data, renderErr := e.renderPage(section.tmpl, section.build(org, sectionIDs[i]))
		if renderErr != nil {
			return renderErr
		}

		err = e.write(path.Join(org.Name, section.dir, indexFile), data)
		if err != nil {
			return err
		}
	}

	err = e.writeProjectsIndex(org, listings)
	if err != nil {
		return err
	}

	for _, listing := range listings {
		err = e.renderProject(ctx, org, listing)
		if err != nil {
			return err
		}
	}

	return nil
}

// writeOrgIndex renders the organization's index: its allowlisted settings,
// links into its sections, and the inventory of count-only categories.
func (e *Exporter) writeOrgIndex(org *view.Org, projectCount int, sectionIDs [][]string) error {
	r := one(decodeTolerant(org.ReadFile("org.json")))

	pageCtx := OrgPage{
		Title: titleOr(r, org.Name),
		Contents: []ContentEntry{
			{Title: "Projects", Dir: "projects", Noun: "project", Nouns: "projects", Count: projectCount},
		},
	}

	if r != nil {
		pageCtx.Info = addKV(pageCtx.Info, labelID, r.ID)
		pageCtx.Info = addKV(pageCtx.Info, "Email", r.String("email"))
		pageCtx.Info = addKV(pageCtx.Info, labelCreated, fmtTime(r.Time("created-at")))
	}

	for i, section := range orgSections {
		if len(sectionIDs[i]) == 0 {
			continue
		}

		pageCtx.Contents = append(pageCtx.Contents, ContentEntry{
			Title: section.title, Dir: section.dir,
			Noun: section.noun, Nouns: section.nouns,
			Count: len(sectionIDs[i]),
		})
	}

	for _, cat := range orgInventoryDirs {
		if n := countEntries(org, cat.dir); n > 0 {
			pageCtx.Inventory = append(pageCtx.Inventory, InventoryEntry{Label: cat.label, Count: n})
		}
	}

	for _, cat := range orgInventoryFiles {
		if n := len(decodeTolerant(org.ReadFile(cat.file))); n > 0 {
			pageCtx.Inventory = append(pageCtx.Inventory, InventoryEntry{Label: cat.label, Count: n})
		}
	}

	data, err := e.renderPage("org.md.tmpl", pageCtx)
	if err != nil {
		return err
	}

	return e.write(path.Join(org.Name, indexFile), data)
}

// buildTeamsPage builds every team into one table row; a team whose
// team.json is damaged still gets a row carrying only its id.
func buildTeamsPage(org *view.Org, ids []string) any {
	pageCtx := TeamsPage{Teams: make([]Team, 0, len(ids))}

	for _, id := range ids {
		team := Team{ID: id}

		if r := one(decodeTolerant(org.ReadFile(path.Join("teams", id, "team.json")))); r != nil {
			team.Name = r.String("name")
			team.Visibility = r.String("visibility")

			if n, ok := r.IntOK("users-count"); ok {
				team.Users = strconv.FormatInt(n, 10)
			}
		}

		pageCtx.Teams = append(pageCtx.Teams, team)
	}

	return pageCtx
}

// setEntry builds one id-keyed set's section: its title, the allowlisted
// settings its kv builder names, and its variables, values gated by
// [buildVariables]. A set whose metadata leaf is damaged still gets a section
// keyed by its id.
func setEntry(org *view.Org, id, dir, setFile, varsFile string,
	kv func(r *view.Resource) []KV,
) (string, []KV, []Variable) {
	r := one(decodeTolerant(org.ReadFile(path.Join(dir, id, setFile))))

	var info []KV

	if r != nil {
		info = kv(r)
	}

	vars := buildVariables(decodeTolerant(org.ReadFile(path.Join(dir, id, varsFile))))

	return titleOr(r, id), info, vars
}

// buildVariableSetsPage builds every variable set's section through
// [setEntry].
func buildVariableSetsPage(org *view.Org, ids []string) any {
	pageCtx := VariableSetsPage{Sets: make([]VariableSet, 0, len(ids))}

	for _, id := range ids {
		title, info, vars := setEntry(org, id, "variable-sets", "variable-set.json", "variables.json",
			func(r *view.Resource) []KV {
				info := addKV(nil, labelID, r.ID)
				info = addKV(info, labelDescription, r.String("description"))

				return addKV(info, "Global", boolCell(r, "global"))
			})

		pageCtx.Sets = append(pageCtx.Sets, VariableSet{Title: title, Info: info, Variables: vars})
	}

	return pageCtx
}

// buildPolicySetsPage builds every policy set's section through [setEntry],
// its parameters carrying the same sensitive gate as variables.
func buildPolicySetsPage(org *view.Org, ids []string) any {
	pageCtx := PolicySetsPage{Sets: make([]PolicySet, 0, len(ids))}

	for _, id := range ids {
		title, info, params := setEntry(org, id, "policy-sets", "policy-set.json", "parameters.json",
			func(r *view.Resource) []KV {
				info := addKV(nil, labelID, r.ID)
				info = addKV(info, "Kind", r.String("kind"))
				info = addKV(info, "Global", boolCell(r, "global"))

				if n, ok := r.IntOK("policy-count"); ok {
					info = addKV(info, "Policies", strconv.FormatInt(n, 10))
				}

				return info
			})

		pageCtx.Sets = append(pageCtx.Sets, PolicySet{Title: title, Info: info, Parameters: params})
	}

	return pageCtx
}

// writeProjectsIndex renders the organization's project list.
func (e *Exporter) writeProjectsIndex(org *view.Org, listings []projectListing) error {
	pageCtx := ProjectsPage{Projects: make([]ProjectEntry, 0, len(listings))}

	for _, listing := range listings {
		pageCtx.Projects = append(pageCtx.Projects, ProjectEntry{
			Name:       listing.name,
			Workspaces: len(listing.workspaces),
			Stacks:     len(listing.stacks),
		})
	}

	data, err := e.renderPage("projects.md.tmpl", pageCtx)
	if err != nil {
		return err
	}

	return e.write(path.Join(org.Name, "projects", indexFile), data)
}

// renderProject writes one project's subtree: its index, its workspace pages,
// and its stack pages.
//
//nolint:contextcheck // Mirror read-throughs run under the archive's stored browse context (see view.WithContext).
func (e *Exporter) renderProject(ctx context.Context, org *view.Org, listing projectListing) error {
	err := e.writeProjectIndex(org, listing)
	if err != nil {
		return err
	}

	for _, name := range listing.workspaces {
		ctxErr := ctx.Err()
		if ctxErr != nil {
			return fmt.Errorf("export stopped: %w", ctxErr)
		}

		err = e.renderWorkspace(org.Workspace(listing.name, name), org.Name)
		if err != nil {
			return err
		}
	}

	for _, name := range listing.stacks {
		err = e.renderStack(org, listing.name, name)
		if err != nil {
			return err
		}
	}

	return nil
}

// writeProjectIndex renders one project's page: its allowlisted settings and
// its workspace and stack lists.
func (e *Exporter) writeProjectIndex(org *view.Org, listing projectListing) error {
	r := one(decodeTolerant(org.ReadFile(path.Join("projects", listing.name, "project.json"))))

	pageCtx := ProjectPage{
		Title:      titleOr(r, listing.name),
		Workspaces: listing.workspaces,
		Stacks:     listing.stacks,
	}

	if r != nil {
		pageCtx.Info = addKV(pageCtx.Info, labelID, r.ID)
		pageCtx.Info = addKV(pageCtx.Info, labelDescription, r.String("description"))
		pageCtx.Info = addKV(pageCtx.Info, labelCreated, fmtTime(r.Time("created-at")))
	}

	data, err := e.renderPage("project.md.tmpl", pageCtx)
	if err != nil {
		return err
	}

	return e.write(path.Join(org.Name, "projects", listing.name, indexFile), data)
}
