// Package export renders an archive's metadata as a tree of markdown files, a
// browsable site source any static site generator with directory-based
// navigation (mkdocs and its kin) can build without configuration.
//
// The tree mirrors the archive's logical hierarchy: an index page per
// organization links into teams, variable sets, policy sets, and projects,
// each project into its workspaces and stacks, and each workspace page
// summarizes its settings, variables, runs, and state versions. Every
// directory carries an index.md, so generators that treat one as a section
// index navigate the tree as-is.
//
// # Sensitivity
//
// The archive stores everything the API returned, including raw state blobs
// and execution logs that embed secret values in cleartext. The export is the
// opposite surface: pages carry metadata only, and every renderer names the
// exact attributes it prints, so a field the allowlist does not know stays out
// of the output. Content that can embed secrets, such as state blobs, plan and
// apply logs, and cost estimates, is represented by presence alone: names,
// sizes, and timestamps, never bytes, alongside a snippet of the archiver CLI
// commands (show, extract) that retrieve the full content from the backup. A
// variable whose sensitive flag is set
// shows its key with a "(sensitive)" marker and its stored value is never
// read, regardless of what the archive holds for it.
//
// # Templates
//
// Every page renders through Go's text/template. The defaults ship embedded,
// one template per page kind, named by file ([DefaultTemplates] serves them
// as a starting point to copy). [WithTemplatesDir] names a directory whose
// *.md.tmpl files override the same-named defaults; pages without an override
// keep theirs, and a file whose name matches no page template is refused
// ([ErrUnknownTemplate]) rather than silently ignored. Overrides parse before
// the target directory is touched, so a broken template
// ([ErrTemplateInvalid]) never costs a cleared site.
//
// Each template renders a typed context (its doc comment names its template
// file) built from the archive with the sensitivity rules above already
// applied: the contexts are plain data, redacted before any template runs, so
// a user-supplied template can restyle the pages but cannot reach withheld
// content. Context fields carry raw text; templates neutralize markdown
// through the helper functions every template can call: escape (markdown
// cell escaping), link (an escaped link with URL-escaped path segments),
// countNoun (count with a singular or plural noun), and join
// (strings.Join).
//
// # Usage
//
// Open the archive with [view.OpenArchive], wrap the organizations in a
// [view.Archive], and run an [Exporter] over it:
//
//	orgs, _ := view.OpenArchive(dir)
//	summary, err := export.New(view.NewArchive(orgs), target).Run(ctx)
//
// The target directory must be disjoint from the archive
// ([ErrTargetOverlapsArchive]); it is created when absent and refused when it
// is a file ([ErrTargetNotDir]) or non-empty ([ErrTargetNotEmpty]), with
// [WithForce] replacing a non-empty target's contents instead.
package export
