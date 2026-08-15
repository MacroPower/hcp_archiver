package export

// The types in this file are the data each page template renders. They are
// pure data: plain strings, numbers, and slices, with every sensitive value
// already redacted while the archive was read, so no template, default or
// user-supplied, can reach content the export withholds. Their fields carry
// raw (unescaped) text; templates neutralize markdown through the escape and
// link helpers. Changes to these types are additive only.

// KV is one labeled value in a page's definition list. Builders drop rows
// whose value is empty, so an absent attribute leaves no dangling label.
type KV struct {
	// Label is the row's display label, composed by the export.
	Label string
	// Value is the row's raw value.
	Value string
}

// Variable is one variable-like row (a workspace variable, a variable-set
// variable, or a policy-set parameter).
type Variable struct {
	// Key is the variable's key.
	Key string
	// Category is the variable's category attribute.
	Category string
	// HCL renders the hcl attribute: "true", "false", or empty when absent.
	HCL string
	// Value is the variable's value, replaced by "(sensitive)" for a
	// sensitive variable, whose stored value is never read.
	Value string
}

// ArchivePage is the data archive.md.tmpl renders: the site home.
type ArchivePage struct {
	// Orgs lists the archived organizations.
	Orgs []ArchiveOrg
}

// ArchiveOrg is one organization row on the site home.
type ArchiveOrg struct {
	// Name is the organization's name, also its directory in the site tree.
	Name string
}

// OrgPage is the data org.md.tmpl renders: one organization's index.
type OrgPage struct {
	// Title is the page title, the organization's name.
	Title string
	// Info holds the organization's allowlisted settings.
	Info []KV
	// Contents links into the organization's section pages.
	Contents []ContentEntry
	// Inventory counts the archived categories that get no page of their own.
	Inventory []InventoryEntry
}

// ContentEntry is one link on an organization index into a section page.
type ContentEntry struct {
	// Title is the link text.
	Title string
	// Dir is the section's directory beneath the organization's.
	Dir string
	// Noun and Nouns are the singular and plural count nouns.
	Noun, Nouns string
	// Count is how many objects the section holds.
	Count int
}

// InventoryEntry is one count-only category row on an organization index.
type InventoryEntry struct {
	// Label names the category.
	Label string
	// Count is how many objects the backup holds in it.
	Count int
}

// TeamsPage is the data teams.md.tmpl renders.
type TeamsPage struct {
	// Teams lists the organization's teams.
	Teams []Team
}

// Team is one team row. A team whose metadata leaf is damaged carries only
// its ID.
type Team struct {
	// Name is the team's name.
	Name string
	// ID is the team's ID, also its directory in the archive.
	ID string
	// Visibility is the team's visibility attribute.
	Visibility string
	// Users renders the team's member count, empty when unknown.
	Users string
}

// VariableSetsPage is the data variable-sets.md.tmpl renders.
type VariableSetsPage struct {
	// Sets lists the organization's variable sets.
	Sets []VariableSet
}

// VariableSet is one variable set's section.
type VariableSet struct {
	// Title is the set's name, falling back to its ID.
	Title string
	// Info holds the set's allowlisted settings.
	Info []KV
	// Variables lists the set's variables, values gated on sensitivity.
	Variables []Variable
}

// PolicySetsPage is the data policy-sets.md.tmpl renders.
type PolicySetsPage struct {
	// Sets lists the organization's policy sets.
	Sets []PolicySet
}

// PolicySet is one policy set's section.
type PolicySet struct {
	// Title is the set's name, falling back to its ID.
	Title string
	// Info holds the set's allowlisted settings.
	Info []KV
	// Parameters lists the set's parameters, values gated on sensitivity.
	Parameters []Variable
}

// ProjectsPage is the data projects.md.tmpl renders: an organization's
// project list.
type ProjectsPage struct {
	// Projects lists the organization's projects.
	Projects []ProjectEntry
}

// ProjectEntry is one project row on the projects index.
type ProjectEntry struct {
	// Name is the project's name, also its directory in the site tree.
	Name string
	// Workspaces and Stacks count the project's children.
	Workspaces, Stacks int
}

// ProjectPage is the data project.md.tmpl renders: one project's index.
type ProjectPage struct {
	// Title is the page title, the project's name.
	Title string
	// Info holds the project's allowlisted settings.
	Info []KV
	// Workspaces and Stacks name the project's children, each rendered as a
	// link into its page.
	Workspaces, Stacks []string
}

// WorkspacePage is the data workspace.md.tmpl renders.
type WorkspacePage struct {
	// Runs is the run-history section, nil when the workspace has none.
	Runs *RunsSection
	// States is the state-version section, nil when the workspace has none.
	States *StatesSection
	// Title is the page title, the workspace's name.
	Title string
	// Info holds the workspace's allowlisted settings.
	Info []KV
	// Variables lists the workspace's variables, values gated on sensitivity.
	Variables []Variable
	// HasReadme reports whether the workspace's readme was copied beside the
	// page.
	HasReadme bool
}

// RunsSection is a workspace page's run history.
type RunsSection struct {
	// Example is the org-prefixed archive path of one artifact the retrieval
	// snippet shows, empty when no run carries any.
	Example string
	// Dir is the org-prefixed archive path of the runs directory.
	Dir string
	// Rows lists the runs, newest first.
	Rows []RunRow
	// Count is how many runs are archived.
	Count int
}

// RunRow is one run's table row.
type RunRow struct {
	// ID is the run's ID.
	ID string
	// Created renders the run's creation time, empty when unknown.
	Created string
	// Status is the run's final status.
	Status string
	// Message is the run's message.
	Message string
	// Source names what triggered the run.
	Source string
	// TerraformVersion is the run's Terraform version.
	TerraformVersion string
	// Artifacts names the run's archived artifact files. The artifacts
	// themselves are withheld; only their names render.
	Artifacts []string
	// IsDestroy reports whether the run was a destroy.
	IsDestroy bool
	// HasChanges reports whether the run carried changes.
	HasChanges bool
}

// StatesSection is a workspace page's state-version history.
type StatesSection struct {
	// Example is the org-prefixed archive path of one state blob the
	// retrieval snippet shows, empty when none is archived.
	Example string
	// Dir is the org-prefixed archive path of the state-versions directory.
	Dir string
	// Rows lists the state versions, newest first.
	Rows []StateRow
	// Count is how many state versions are archived.
	Count int
}

// StateRow is one state version's table row. The state content itself is
// withheld; only metadata renders.
type StateRow struct {
	// ID is the state version's ID.
	ID string
	// Created renders the version's creation time, empty when unknown.
	Created string
	// Status is the version's status.
	Status string
	// Size renders the archived blob's size.
	Size string
	// Forms names which blob forms the backup holds: "raw", "json", or both.
	Forms string
	// Serial is the state serial.
	Serial int64
}

// StackPage is the data stack.md.tmpl renders.
type StackPage struct {
	// Title is the page title, the stack's name.
	Title string
	// Info holds the stack's allowlisted settings.
	Info []KV
	// Deployments lists the stack's deployments.
	Deployments []Deployment
}

// Deployment is one stack deployment's table row.
type Deployment struct {
	// Name is the deployment's name, its directory in the archive.
	Name string
	// Created renders the deployment's creation time, empty when unknown.
	Created string
}

// addKV appends a labeled value when it is non-empty, keeping the definition
// lists free of dangling labels the way the renderers always have.
func addKV(rows []KV, label, value string) []KV {
	if value == "" {
		return rows
	}

	return append(rows, KV{Label: label, Value: value})
}
