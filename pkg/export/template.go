package export

import (
	"bytes"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"strings"
	"text/template"

	"go.jacobcolvin.com/hcp_archiver/pkg/theme"
)

var (
	// ErrUnknownTemplate indicates a template override directory holds a
	// *.md.tmpl file whose name matches no page template, most likely a typo
	// that would otherwise silently leave the default in place.
	ErrUnknownTemplate = errors.New("unrecognized template file name")

	// ErrTemplateInvalid indicates a template override does not parse.
	ErrTemplateInvalid = errors.New("template does not parse")

	// ErrTemplatesUnreadable indicates the template override directory could
	// not be listed.
	ErrTemplatesUnreadable = errors.New("templates directory is not readable")

	// The embedded built-in page templates, one per generated page kind;
	// their filenames are the override contract.
	//go:embed templates
	defaultTemplates embed.FS

	// The embedded set rooted so the template filenames are its top-level
	// entries. The embed directive guarantees the subtree exists.
	defaultTemplateFS = func() fs.FS {
		sub, err := fs.Sub(defaultTemplates, "templates")
		if err != nil {
			panic(err)
		}

		return sub
	}()
)

// templateSuffix is the file suffix every page template carries.
const templateSuffix = ".md.tmpl"

// DefaultTemplates returns the embedded default page templates as a file
// system whose top-level *.md.tmpl entries are the page template names, a
// starting point to copy into an override directory.
func DefaultTemplates() fs.FS {
	return defaultTemplateFS
}

// templateNames returns the page template filenames, sorted, read from the
// embedded set so the known-name list has a single source of truth.
func templateNames() []string {
	entries, err := fs.ReadDir(defaultTemplateFS, ".")
	if err != nil {
		panic(err)
	}

	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}

	return names
}

// templateFuncs returns the helper functions every page template can call.
func templateFuncs() template.FuncMap {
	return template.FuncMap{
		"escape":     escapeCell,
		"link":       mdLink,
		"countNoun":  theme.CountNoun,
		"join":       strings.Join,
		"shellQuote": shellQuote,
	}
}

// loadTemplates parses the embedded page templates and, when dir is
// non-empty, the *.md.tmpl overrides it holds, each replacing the same-named
// default. An override whose name matches no page template is refused with
// [ErrUnknownTemplate]; one that does not parse is refused with
// [ErrTemplateInvalid]. Other files and subdirectories are ignored.
//
// A default an override replaces is never parsed at all, because a template
// set keeps the definition it already holds when the replacement parses to an
// empty tree, and counts a body of comments or whitespace as empty. Parsing
// both would leave a deliberately blank override rendering the default it was
// written to suppress.
func loadTemplates(dir string) (*template.Template, error) {
	known := templateNames()

	overrides, err := overrideNames(dir, known)
	if err != nil {
		return nil, err
	}

	defaults := make([]string, 0, len(known))

	for _, name := range known {
		if !slices.Contains(overrides, name) {
			defaults = append(defaults, name)
		}
	}

	t := template.New("export").
		Option("missingkey=error").
		Funcs(templateFuncs())

	// ParseFS refuses an empty file list, and overriding every page leaves no
	// default to parse.
	if len(defaults) > 0 {
		t, err = t.ParseFS(defaultTemplateFS, defaults...)
		if err != nil {
			return nil, fmt.Errorf("parse embedded templates: %w", err)
		}
	}

	// The override names all come from the known set, so none carries glob
	// metacharacters.
	if len(overrides) > 0 {
		t, err = t.ParseFS(os.DirFS(dir), overrides...)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrTemplateInvalid, err)
		}
	}

	return t, nil
}

// overrideNames returns the page templates dir overrides, in the order the
// directory lists them. An empty dir names none. A *.md.tmpl file whose name
// is not in known is refused with [ErrUnknownTemplate]; other files and
// subdirectories are ignored.
func overrideNames(dir string, known []string) ([]string, error) {
	if dir == "" {
		return nil, nil
	}

	entries, err := fs.ReadDir(os.DirFS(dir), ".")
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrTemplatesUnreadable, err)
	}

	var overrides []string

	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, templateSuffix) {
			continue
		}

		if !slices.Contains(known, name) {
			return nil, fmt.Errorf("%w: %q (known: %s)",
				ErrUnknownTemplate, name, strings.Join(known, ", "))
		}

		overrides = append(overrides, name)
	}

	return overrides, nil
}

// renderPage executes the named page template over data, buffering the whole
// page so an execution error never reaches the target tree.
func (e *Exporter) renderPage(name string, data any) ([]byte, error) {
	var buf bytes.Buffer

	err := e.tmpl.ExecuteTemplate(&buf, name, data)
	if err != nil {
		return nil, fmt.Errorf("render %q: %w", name, err)
	}

	return buf.Bytes(), nil
}
