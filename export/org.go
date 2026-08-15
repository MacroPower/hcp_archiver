package export

import (
	"context"
	"fmt"
	"path"
	"strconv"
	"strings"
	"time"

	"go.jacobcolvin.com/hcp_archiver/atomicfile"
	"go.jacobcolvin.com/hcp_archiver/store"
	"go.jacobcolvin.com/hcp_archiver/theme"
	"go.jacobcolvin.com/hcp_archiver/view"
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
// answering zero when it is absent. The archive's own bookkeeping is
// excluded: dotfiles (identity sidecars, ledgers), staging temp files, and
// eviction stubs, each stub folded onto the object it stands in for so an
// evicted object never counts beside the mirror's record of it.
func countEntries(org *view.Org, rel string) int {
	entries, err := org.Entries(rel)
	if err != nil {
		return 0
	}

	seen := make(map[string]struct{}, len(entries))

	for _, e := range entries {
		name := e.Name
		if strings.HasPrefix(name, ".") || atomicfile.IsTemp(name) {
			continue
		}

		if target, ok := store.RemoteStubTarget(name); ok {
			name = target
		}

		seen[name] = struct{}{}
	}

	return len(seen)
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

// variableRows renders variable-like resources (workspace variables,
// variable-set variables, policy-set parameters) into table rows, gating the
// value on the sensitive flag. A sensitive variable's stored value is never
// read, even though the API blanks it upstream; the export does not rely on
// that behavior.
func variableRows(resources []view.Resource) [][]string {
	rows := make([][]string, 0, len(resources))

	for i := range resources {
		r := &resources[i]

		value := sensitiveMarker
		if !r.Bool("sensitive") {
			value = escapeCell(r.String("value"))
		}

		rows = append(rows, []string{
			escapeCell(r.String("key")),
			escapeCell(r.String("category")),
			boolCell(r, "hcl"),
			value,
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
// organization's directory.
type orgSection struct {
	render func(org *view.Org, ids []string) *page
	title  string
	dir    string
	noun   string
	nouns  string
}

var (
	// The columns [variableRows] renders.
	variableHeaders = []string{"Key", "Category", "HCL", "Value"}

	// The categories that earn a page: those whose archive directories are
	// keyed by opaque ids, folded into one readable page each.
	orgSections = []orgSection{
		{title: "Teams", dir: "teams", noun: "team", nouns: "teams", render: teamsPage},
		{
			title: "Variable sets", dir: "variable-sets",
			noun: "variable set", nouns: "variable sets", render: variableSetsPage,
		},
		{
			title: "Policy sets", dir: "policy-sets",
			noun: "policy set", nouns: "policy sets", render: policySetsPage,
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

		err = e.write(path.Join(org.Name, section.dir, indexFile), section.render(org, sectionIDs[i]).bytes())
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
	p := &page{}

	r := one(decodeTolerant(org.ReadFile("org.json")))
	p.h1(titleOr(r, org.Name))

	if r != nil {
		p.kv([][2]string{
			{labelID, escapeCell(r.ID)},
			{"Email", escapeCell(r.String("email"))},
			{labelCreated, fmtTime(r.Time("created-at"))},
		})
	}

	lines := []string{"- " + mdLink("Projects", "projects", indexFile) + ": " +
		theme.CountNoun(projectCount, "project", "projects")}

	for i, section := range orgSections {
		if len(sectionIDs[i]) == 0 {
			continue
		}

		lines = append(lines, "- "+mdLink(section.title, section.dir, indexFile)+": "+
			theme.CountNoun(len(sectionIDs[i]), section.noun, section.nouns))
	}

	p.h2("Contents")
	p.para(strings.Join(lines, "\n"))

	var rows [][]string

	for _, cat := range orgInventoryDirs {
		if n := countEntries(org, cat.dir); n > 0 {
			rows = append(rows, []string{cat.label, strconv.Itoa(n)})
		}
	}

	for _, cat := range orgInventoryFiles {
		if n := len(decodeTolerant(org.ReadFile(cat.file))); n > 0 {
			rows = append(rows, []string{cat.label, strconv.Itoa(n)})
		}
	}

	if len(rows) > 0 {
		p.h2("Also archived")
		p.para("Categories the backup holds beyond the pages of this site.")
		p.table([]string{"Category", "Count"}, rows)
	}

	return e.write(path.Join(org.Name, indexFile), p.bytes())
}

// teamsPage renders every team as one table row; a team whose team.json is
// damaged still gets a row carrying only its id.
func teamsPage(org *view.Org, ids []string) *page {
	p := &page{}
	p.h1("Teams")

	rows := make([][]string, 0, len(ids))

	for _, id := range ids {
		row := []string{"", escapeCell(id), "", ""}

		if r := one(decodeTolerant(org.ReadFile(path.Join("teams", id, "team.json")))); r != nil {
			row[0] = escapeCell(r.String("name"))
			row[2] = escapeCell(r.String("visibility"))

			if n, ok := r.IntOK("users-count"); ok {
				row[3] = strconv.FormatInt(n, 10)
			}
		}

		rows = append(rows, row)
	}

	p.table([]string{"Name", "ID", "Visibility", "Users"}, rows)

	return p
}

// idSetsPage renders a category of id-keyed sets as one page: an h2 section
// per set carrying the allowlisted settings its kv builder names and the
// set's variables table, values gated by [variableRows]. A set whose metadata
// leaf is damaged still gets a section keyed by its id.
func idSetsPage(org *view.Org, ids []string, title, dir, setFile, varsFile string,
	kv func(r *view.Resource) [][2]string,
) *page {
	p := &page{}
	p.h1(title)

	for _, id := range ids {
		r := one(decodeTolerant(org.ReadFile(path.Join(dir, id, setFile))))
		p.h2(titleOr(r, id))

		if r != nil {
			p.kv(kv(r))
		}

		p.table(variableHeaders, variableRows(decodeTolerant(org.ReadFile(path.Join(dir, id, varsFile)))))
	}

	return p
}

// variableSetsPage renders every variable set through [idSetsPage].
func variableSetsPage(org *view.Org, ids []string) *page {
	return idSetsPage(org, ids, "Variable sets", "variable-sets", "variable-set.json", "variables.json",
		func(r *view.Resource) [][2]string {
			return [][2]string{
				{labelID, escapeCell(r.ID)},
				{labelDescription, escapeCell(r.String("description"))},
				{"Global", boolCell(r, "global")},
			}
		})
}

// policySetsPage renders every policy set through [idSetsPage], its
// parameters carrying the same sensitive gate as variables.
func policySetsPage(org *view.Org, ids []string) *page {
	return idSetsPage(org, ids, "Policy sets", "policy-sets", "policy-set.json", "parameters.json",
		func(r *view.Resource) [][2]string {
			kv := [][2]string{
				{labelID, escapeCell(r.ID)},
				{"Kind", escapeCell(r.String("kind"))},
				{"Global", boolCell(r, "global")},
			}

			if n, ok := r.IntOK("policy-count"); ok {
				kv = append(kv, [2]string{"Policies", strconv.FormatInt(n, 10)})
			}

			return kv
		})
}

// writeProjectsIndex renders the organization's project list.
func (e *Exporter) writeProjectsIndex(org *view.Org, listings []projectListing) error {
	p := &page{}
	p.h1("Projects")

	rows := make([][]string, 0, len(listings))

	for _, listing := range listings {
		rows = append(rows, []string{
			mdLink(listing.name, listing.name, indexFile),
			strconv.Itoa(len(listing.workspaces)),
			strconv.Itoa(len(listing.stacks)),
		})
	}

	p.table([]string{"Project", "Workspaces", "Stacks"}, rows)

	return e.write(path.Join(org.Name, "projects", indexFile), p.bytes())
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

// linkSection writes a heading and a single-column table of links into child
// pages under dir, writing nothing when names is empty.
func linkSection(p *page, heading, column, dir string, names []string) {
	if len(names) == 0 {
		return
	}

	p.h2(heading)

	rows := make([][]string, 0, len(names))
	for _, name := range names {
		rows = append(rows, []string{mdLink(name, dir, name, indexFile)})
	}

	p.table([]string{column}, rows)
}

// writeProjectIndex renders one project's page: its allowlisted settings and
// its workspace and stack lists.
func (e *Exporter) writeProjectIndex(org *view.Org, listing projectListing) error {
	p := &page{}

	r := one(decodeTolerant(org.ReadFile(path.Join("projects", listing.name, "project.json"))))
	p.h1(titleOr(r, listing.name))

	if r != nil {
		p.kv([][2]string{
			{labelID, escapeCell(r.ID)},
			{labelDescription, escapeCell(r.String("description"))},
			{labelCreated, fmtTime(r.Time("created-at"))},
		})
	}

	linkSection(p, "Workspaces", "Workspace", "workspaces", listing.workspaces)
	linkSection(p, "Stacks", "Stack", "stacks", listing.stacks)

	return e.write(path.Join(org.Name, "projects", listing.name, indexFile), p.bytes())
}
