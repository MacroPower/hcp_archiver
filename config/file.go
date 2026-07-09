package config

import (
	"errors"
	"fmt"

	"go.jacobcolvin.com/niceyaml"
	"go.jacobcolvin.com/niceyaml/paths"
	"go.jacobcolvin.com/niceyaml/schema"
	"go.jacobcolvin.com/x/jsonschema"

	_ "embed"
)

//go:generate go run ./schemagen -o config.schema.json

// EnvConfigPath names the environment variable consulted for the configuration
// file path when no path is given on the command line.
const EnvConfigPath = "HCP_ARCHIVER_CONFIG"

var (
	// ErrReadConfig indicates the configuration file could not be read.
	ErrReadConfig = errors.New("read config file")
	// ErrMultipleDocuments indicates the configuration file holds more than one
	// YAML document; a configuration is a single document.
	ErrMultipleDocuments = errors.New("config file must contain a single document")

	// Schema for [File], generated from the Go type by the schemagen command
	// and embedded for validation. Editors resolve the same schema through the
	// yaml-language-server directive in a configuration file.
	//go:embed config.schema.json
	schemaJSON []byte

	// Validator for decoded configuration data. The embedded schema is generated
	// from [File] and is valid at build time.
	configValidator = schema.NewValidator(jsonschema.MustCompileJSON(schemaJSON))
)

// File is the on-disk YAML configuration describing what to archive and how.
//
// It holds the settings that are stable across runs: the API address, the
// organizations to archive, the workspace concurrency, and the opt-in scope
// toggles. Per-run and secret settings (the output directory, the progress
// mode, the recheck toggle, and the API token) are supplied by flags and the
// environment instead, so a configuration file never carries a machine-specific
// path or a credential.
//
// Every field is optional; an absent field takes the package default. Create
// instances with [LoadFile], or [DefaultFile] for the defaults alone.
type File struct {
	// Address is the HCP Terraform API address. It defaults to [DefaultAddress].
	Address string `json:"address,omitempty" jsonschema:"title=Address,default=https://app.terraform.io"`
	// Organizations limits the run to the named organizations. An empty list
	// archives every organization the token can see.
	Organizations []string `json:"organizations,omitempty" jsonschema:"title=Organizations"`
	// Concurrency is the number of workspaces archived concurrently. It defaults
	// to [DefaultWorkspaceConcurrency].
	Concurrency int `json:"concurrency,omitempty" jsonschema:"title=Concurrency,minimum=1,default=4"`
	// Scope selects the heavy or optional surfaces to archive, each off by
	// default.
	Scope FileScope `json:"scope,omitzero" jsonschema:"title=Scope"`
}

// FileScope holds the opt-in toggles for the heavy or most organization-specific
// surfaces, each archived only when a configuration turns it on.
type FileScope struct {
	// Stacks enables archiving of Stacks.
	Stacks bool `json:"stacks,omitempty" jsonschema:"title=Stacks"`
	// HYOK enables archiving of hold-your-own-key configurations.
	HYOK bool `json:"hyok,omitempty" jsonschema:"title=HYOK"`
	// RegistryDetail enables the deeper registry version, platform, and binary
	// detail.
	RegistryDetail bool `json:"registryDetail,omitempty" jsonschema:"title=Registry Detail"`
	// AuditTrail enables archiving of the audit trail.
	AuditTrail bool `json:"auditTrail,omitempty" jsonschema:"title=Audit Trail"`
}

// DefaultFile returns a [*File] populated with the package defaults, used when
// no configuration file is supplied.
func DefaultFile() *File {
	return &File{
		Address:     DefaultAddress,
		Concurrency: DefaultWorkspaceConcurrency,
	}
}

// LoadFile reads and decodes the configuration file at path.
//
// It starts from [DefaultFile] so any field the document omits keeps its
// default, validates the document against the embedded JSON schema, decodes it,
// then runs [File.Validate]. A schema or validation failure is returned as a
// source-annotated error pointing at the offending line. LoadFile reports
// [ErrReadConfig] when the file cannot be read and [ErrMultipleDocuments] when
// it holds more than one document.
func LoadFile(path string) (*File, error) {
	source, err := niceyaml.NewSourceFromFile(path,
		niceyaml.WithErrorOptions(niceyaml.WithSourceLines(3)),
	)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrReadConfig, err)
	}

	decoder, err := source.Decoder()
	if err != nil {
		//nolint:wrapcheck // WrapError returns a source-annotated niceyaml.Error.
		return nil, source.WrapError(err)
	}

	file := DefaultFile()

	count := 0
	for _, doc := range decoder.Documents() {
		// An empty document (blank file or comments only, such as a lone schema
		// directive) has no body and leaves the defaults in place.
		if doc.Document().Body == nil {
			continue
		}

		count++
		if count > 1 {
			return nil, ErrMultipleDocuments
		}

		err = doc.Unmarshal(file)
		if err != nil {
			//nolint:wrapcheck // WrapError returns a source-annotated niceyaml.Error.
			return nil, source.WrapError(err)
		}
	}

	return file, nil
}

// ValidateSchema validates arbitrary decoded data against the configuration
// JSON schema. It implements [niceyaml.SchemaValidator] so [LoadFile] can
// report constraint violations with source annotations.
func (f File) ValidateSchema(data any) error {
	//nolint:wrapcheck // ValidateSchema returns a niceyaml.Error with path info.
	return configValidator.ValidateSchema(data)
}

// Validate reports whether the [File] is internally consistent after decoding.
// It implements [niceyaml.Validator] so [LoadFile] runs it automatically, and
// rejects an empty or duplicated organization name with an error pointing at
// the offending entry.
func (f File) Validate() error {
	seen := make(map[string]struct{}, len(f.Organizations))

	for i, org := range f.Organizations {
		if org == "" {
			return niceyaml.NewError("organization name must not be empty",
				niceyaml.WithPath(paths.Root().Child("organizations").Index(i).Value()),
			)
		}

		if _, dup := seen[org]; dup {
			return niceyaml.NewError(fmt.Sprintf("duplicate organization %q", org),
				niceyaml.WithPath(paths.Root().Child("organizations").Index(i).Value()),
			)
		}

		seen[org] = struct{}{}
	}

	return nil
}
