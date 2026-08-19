// Command schemagen generates the JSON schema for the hcp_archiver YAML
// configuration file from [config.File] and writes it to disk.
//
// It runs via go generate; see the directive in the config package. The
// generated schema is embedded by the config package for validation and is
// referenced by the yaml-language-server directive in example configurations
// for editor integration, so everything it emits is user-facing: doc comments
// are rewritten into plain hover text, Go type names are replaced with the
// configuration file's own section names, and the schema carries the $id it
// is published under.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"go.jacobcolvin.com/x/jsonschema"

	"go.jacobcolvin.com/hcp_archiver/pkg/config"
)

const (
	// The URL the release pipeline publishes the schema at, emitted as $id.
	schemaID = "https://jacobcolvin.com/hcp_archiver/config.schema.json"

	// The root description, replacing the root type's Go doc comment, which
	// speaks of constructors and packages rather than the file an operator
	// edits.
	rootDescription = "The hcp_archiver YAML configuration: what to archive and how. " +
		"archive.path is required; every other key is optional, and an absent key takes its " +
		"default. Per-run settings and the API token are supplied by flags and the environment " +
		"instead, so this file never carries a credential, and any relative path resolves " +
		"against the file's own directory."

	// The archive section's key in the published schema, named in several
	// mutations below.
	archiveDef = "archive"

	// The organization-name pattern, mirroring config.ValidateOrganizationName
	// for editor feedback: no path separators, no control characters, and at
	// least one character that is not a dot, rejecting the empty name and
	// all-dot names such as "." and "..". The runtime check stays
	// authoritative; RE2-safe so the embedded validator compiles it.
	orgNamePattern = `^[^\x00-\x1f\x7f/\\]*[^\x00-\x1f\x7f/\\.][^\x00-\x1f\x7f/\\]*$`
)

var (
	// The path the generated schema is written to.
	outFile = flag.String("o", "config.schema.json", "output file for the generated schema")

	// Go type names mapped onto the $defs keys the published schema uses,
	// matching the configuration file's own section names.
	defNames = map[string]string{
		"FileArchive":         archiveDef,
		"FileExport":          "export",
		"FileExportTemplates": "templates",
		"FileExtract":         "extract",
		"FileInclude":         "include",
		"FileRemote":          "remote",
		"FileRemoteUpload":    "upload",
		"FileRunHistory":      "runHistory",
		"FileRunHistoryFetch": "fetch",
		"ByteSize":            "byteSize",
		"Duration":            "duration",
	}

	// Uniform hover titles for the extracted definitions.
	defTitles = map[string]string{
		archiveDef:   "Archive",
		"export":     "Export",
		"templates":  "Templates",
		"extract":    "Extract",
		"include":    "Include",
		"remote":     "Remote",
		"upload":     "Upload",
		"runHistory": "Run History",
		"fetch":      "Fetch",
	}

	// Go doc-link syntax: [Name], [*Name], and [pkg.Name]. A bracketed token
	// that does not begin with a letter is prose, not a doc link, and is left
	// alone.
	docLink = regexp.MustCompile(`\[\*?([A-Za-z][A-Za-z0-9_]*(\.[A-Za-z][A-Za-z0-9_]*)?)\]`)
)

func main() {
	flag.Parse()

	js, err := jsonschema.GenerateFor[config.File](
		context.Background(),
		jsonschema.WithDescriptionProvider(&cleanProvider{
			inner: jsonschema.NewGoCommentProvider(),
		}),
		jsonschema.WithNamer(jsonschema.NamerFunc(func(tc jsonschema.TypeContext) string {
			return defNames[tc.Type.Name()]
		})),
	)
	if err != nil {
		log.Fatalf("generate JSON schema: %v", err)
	}

	js.ID = schemaID
	js.Title = "hcp_archiver configuration"
	js.Description = rootDescription

	for def, title := range defTitles {
		schema, ok := js.Defs[def]
		if !ok {
			log.Fatalf("missing definition %q", def)
		}

		schema.Title = title

		// A section key left bare in the file ("include:" with its children
		// commented out) decodes to null, which the decoder treats as unset,
		// so the schema admits it. Nullability lives on the definition because
		// the property site references it with $ref, whose siblings combine
		// with allOf semantics.
		schema.Type = ""
		schema.Types = []string{"object", "null"}
	}

	// A few descriptions read Go-only even after cleaning; replace them with
	// text written for the configuration file's audience. The address doc
	// comment names the default's Go constant, and the byte-size and duration
	// doc comments close with sentences about the Go types (the generator
	// overwrites a provider-returned description with the type's doc comment,
	// so the overrides land here rather than in the JSONSchema methods).
	js.Properties["address"].Description = "The HCP Terraform API address. " +
		"Defaults to https://app.terraform.io."
	js.Defs["byteSize"].Description = "A byte count: a plain integer number of bytes, " +
		"or a string with a decimal (KB, MB, GB, TB) or binary (KiB, MiB, GiB, TiB) " +
		"suffix, such as 64MiB."
	js.Defs["duration"].Description = "A duration: a string in Go duration syntax " +
		"(ns, us, ms, s, m, h) extended with a day unit, such as 90d, 36h, or 1d12h, " +
		"or 0 for the zero duration."

	// The tag DSL has no required keyword and no per-item constraints, so the
	// required keys and the filter lists' non-empty names are applied here.
	// The archive section is the one the file cannot omit: the root requires
	// the section and the section requires its path, and the nullability the
	// loop above granted is taken back so a bare "archive:" key is refused
	// rather than admitted as unset.
	js.Required = []string{archiveDef}
	js.Defs[archiveDef].Required = []string{"path"}
	js.Defs[archiveDef].Types = []string{"object"}
	js.Defs["remote"].Required = []string{"url"}

	minNameLength := 1
	for _, name := range []string{"organizations", "projects", "workspaces"} {
		js.Properties[name].Items.MinLength = &minNameLength
	}

	// Only organization names carry the path-safety pattern: the org name is
	// joined directly onto the archive root (and the remote key prefix) to
	// root the organization's store, before any sanitization exists. Project
	// and workspace names always pass through the store's per-segment
	// sanitization when paths are composed, so their filters need only be
	// non-empty.
	js.Properties["organizations"].Items.Pattern = orgNamePattern

	data, err := json.MarshalIndent(js, "", "  ")
	if err != nil {
		log.Fatalf("marshal JSON schema: %v", err)
	}

	err = os.WriteFile(*outFile, append(data, '\n'), 0o600)
	if err != nil {
		log.Fatalf("write schema file: %v", err)
	}
}

// cleanProvider wraps the Go comment provider and rewrites each doc comment
// into schema hover text: doc links lose their brackets, and the leading Go
// identifier convention ("Name is ...", "Name holds ...") is stripped so the
// description opens with the prose itself.
type cleanProvider struct {
	inner *jsonschema.GoCommentProvider
}

// TypeDescription cleans the type's doc comment.
func (p *cleanProvider) TypeDescription(ctx context.Context, tc jsonschema.TypeContext) (string, error) {
	s, err := p.inner.TypeDescription(ctx, tc)
	if err != nil {
		return "", fmt.Errorf("type description: %w", err)
	}

	return clean(s, tc.Type.Name()), nil
}

// FieldDescription cleans the field's doc comment.
func (p *cleanProvider) FieldDescription(ctx context.Context, fc jsonschema.FieldContext) (string, error) {
	s, err := p.inner.FieldDescription(ctx, fc)
	if err != nil {
		return "", fmt.Errorf("field description: %w", err)
	}

	return clean(s, fc.StructField.Name), nil
}

// clean rewrites the doc comment for name into user-facing hover text.
func clean(s, name string) string {
	if s == "" {
		return ""
	}

	s = unwrap(s)
	s = docLink.ReplaceAllString(s, "$1")

	if rest, ok := strings.CutPrefix(s, name+" "); ok {
		s = rest

		if r, ok := strings.CutPrefix(s, "is "); ok {
			s = r
		} else if r, ok := strings.CutPrefix(s, "are "); ok {
			s = r
		}

		r, size := utf8.DecodeRuneInString(s)
		s = string(unicode.ToUpper(r)) + s[size:]
	}

	return s
}

// unwrap removes the doc comment's source-line wrapping: single newlines
// become spaces so each paragraph is one line, and blank lines survive as
// paragraph breaks.
func unwrap(s string) string {
	paragraphs := strings.Split(s, "\n\n")
	for i, p := range paragraphs {
		paragraphs[i] = strings.Join(strings.Fields(p), " ")
	}

	return strings.Join(paragraphs, "\n\n")
}
