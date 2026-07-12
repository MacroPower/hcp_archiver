package serialize_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"go.jacobcolvin.com/hcp_archiver/serialize"
)

var (
	// The secretShaped pattern matches field names that plausibly carry secret
	// material. It is deliberately broader than the sanitizer's redaction list:
	// the sanitizer must stay an exact-name allowlist (a suffix rule would zero
	// relation fields like an OAuthToken pointer), so this wider net plus a
	// reviewed exemption list is what catches a field the SDK adds or renames.
	secretShaped = regexp.MustCompile(`(?i)(token|secret|password|passphrase|privatekey|credential|hmac|apikey)`)

	// The reviewedNonSecrets map lists go-tfe fields whose names look
	// secret-shaped but were human-reviewed as carrying no secret material.
	// Keyed "Type.Field"; every entry documents its reason. A new SDK version
	// that adds a secret-shaped field fails TestGoTFESecretFieldsAreRedacted
	// until it is either added to the sanitizer's redaction list or reviewed
	// into this map.
	reviewedNonSecrets = map[string]string{}
)

// TestGoTFESecretFieldsAreRedacted scans the go-tfe module source for struct
// fields that could carry secret text and requires each one to be covered:
// either redacted by the safety pass or explicitly reviewed as not a secret.
//
// The safety pass redacts by field name over an SDK it does not control, so
// its denylist silently rots as the SDK evolves: a renamed or newly added
// secret field would serialize in cleartext with no failing test. This test
// pins the denylist to the SDK version in go.mod — upgrading go-tfe re-runs
// the scan against the new source, so the gap surfaces as a test failure at
// upgrade time instead of as a leak in an archive.
func TestGoTFESecretFieldsAreRedacted(t *testing.T) {
	t.Parallel()

	redacted := serialize.RedactedFieldNames()

	var missing []string

	for _, st := range parseGoTFEStructs(t) {
		// Request-payload types (create/update/list options) are never fed to
		// Marshal: the archiver serializes listed and read response models only.
		if strings.HasSuffix(st.name, "Options") {
			continue
		}

		for _, f := range st.fields {
			if !f.stringish || !secretShaped.MatchString(f.name) {
				continue
			}

			// A *TokenID names a reference to a token object, not its material.
			if strings.HasSuffix(f.name, "ID") {
				continue
			}

			if redacted[f.name] {
				continue
			}

			key := st.name + "." + f.name
			if _, ok := reviewedNonSecrets[key]; ok {
				continue
			}

			missing = append(missing, key)
		}
	}

	sort.Strings(missing)
	require.Emptyf(t, missing,
		"go-tfe fields that look like secrets but are neither redacted by the "+
			"safety pass nor reviewed as non-secret; add each to "+
			"redactedFieldNames (serialize.go) or, with justification, to "+
			"reviewedNonSecrets (this file): %v", missing)
}

// tfeStruct is one exported struct parsed from the go-tfe source.
type tfeStruct struct {
	name   string
	fields []tfeField
}

// tfeField is one named field with whether its type can carry secret text.
type tfeField struct {
	name      string
	stringish bool
}

// parseGoTFEStructs parses every non-test file of the go-tfe module in the
// local module cache and returns its exported structs.
func parseGoTFEStructs(t *testing.T) []tfeStruct {
	t.Helper()

	out, err := exec.CommandContext(t.Context(),
		"go", "list", "-m", "-f", "{{.Dir}}", "github.com/hashicorp/go-tfe").Output()
	require.NoError(t, err, "locate the go-tfe module directory")

	dir := strings.TrimSpace(string(out))
	require.NotEmpty(t, dir)

	files, err := filepath.Glob(filepath.Join(dir, "*.go"))
	require.NoError(t, err)

	var structs []tfeStruct

	fset := token.NewFileSet()

	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}

		parsed, perr := parser.ParseFile(fset, file, nil, parser.SkipObjectResolution)
		require.NoError(t, perr)

		ast.Inspect(parsed, func(n ast.Node) bool {
			spec, ok := n.(*ast.TypeSpec)
			if !ok || !spec.Name.IsExported() {
				return true
			}

			st, ok := spec.Type.(*ast.StructType)
			if !ok {
				return true
			}

			ts := tfeStruct{name: spec.Name.Name}

			for _, field := range st.Fields.List {
				for _, ident := range field.Names {
					// An unexported field never reaches the archive: both encoders
					// serialize exported fields only, and the reflection pass
					// cannot set it anyway.
					if !ident.IsExported() {
						continue
					}

					ts.fields = append(ts.fields, tfeField{
						name:      ident.Name,
						stringish: stringish(field.Type),
					})
				}
			}

			structs = append(structs, ts)

			return true
		})
	}

	require.NotEmpty(t, structs, "the go-tfe scan found no structs; the guard is not running")

	return structs
}

// stringish reports whether a field of this parsed type can carry secret text:
// a string, a pointer to string, a slice of strings, a map with string values,
// or an interface value. Booleans, numbers, times, and typed relations cannot,
// which keeps flag fields like UserTokensEnabled out of the scan.
func stringish(expr ast.Expr) bool {
	switch e := expr.(type) {
	case *ast.Ident:
		return e.Name == "string" || e.Name == "any"
	case *ast.StarExpr:
		return stringish(e.X)
	case *ast.ArrayType:
		return stringish(e.Elt)
	case *ast.MapType:
		return stringish(e.Value)
	case *ast.InterfaceType:
		return true
	default:
		return false
	}
}
