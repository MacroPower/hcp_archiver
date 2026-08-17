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
		"Every key is optional; an absent key takes its default. Per-run settings and the " +
		"API token are supplied by flags and the environment instead, so this file never " +
		"carries a credential, and any relative path resolves against the file's own directory."

	// The organization-name pattern, mirroring config.ValidateOrganizationName
	// for editor feedback: no path separators, no control characters, and at
	// least one character that is not a dot, rejecting "", ".", and "..". The
	// runtime check stays authoritative; RE2-safe so the embedded validator
	// compiles it.
	orgNamePattern = `^[^\x00-\x1f\x7f/\\]*[^\x00-\x1f\x7f/\\.][^\x00-\x1f\x7f/\\]*$`
)

var (
	// The path the generated schema is written to.
	outFile = flag.String("o", "config.schema.json", "output file for the generated schema")

	// Go type names mapped onto the $defs keys the published schema uses,
	// matching the configuration file's own section names.
	defNames = map[string]string{
		"FileExport":     "export",
		"FileRemote":     "remote",
		"FileRunHistory": "runHistory",
		"FileScope":      "scope",
		"ByteSize":       "byteSize",
	}

	// Uniform hover titles for the extracted definitions.
	defTitles = map[string]string{
		"export":     "Export",
		"remote":     "Remote",
		"runHistory": "Run History",
		"scope":      "Scope",
	}

	// Go doc-link syntax: [Name], [*Name], and [pkg.Name]. It cannot match
	// prose brackets such as [archive-dir], which carry characters a Go
	// identifier never does.
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
	}

	// A few descriptions read Go-only even after cleaning; replace them with
	// text written for the configuration file's audience. The address doc
	// comment names the default's Go constant, and the byte-size doc comment
	// closes with a sentence about the Go type.
	js.Properties["address"].Description = "The HCP Terraform API address. " +
		"Defaults to https://app.terraform.io."
	js.Defs["byteSize"].Description = "A byte count: a plain integer number of bytes, " +
		"or a string with a decimal (KB, MB, GB, TB) or binary (KiB, MiB, GiB, TiB) " +
		"suffix, such as 64MiB."

	// The tag DSL has no required keyword and no per-item constraints, so the
	// remote section's mandatory bucket URL and the filter lists' non-empty
	// names are applied here.
	js.Defs["remote"].Required = []string{"url"}

	minNameLength := 1
	for _, name := range []string{"organizations", "projects", "workspaces"} {
		js.Properties[name].Items.MinLength = &minNameLength
	}

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
