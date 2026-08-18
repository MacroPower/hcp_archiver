package config

import (
	"context"
	"errors"
	"fmt"

	"go.jacobcolvin.com/niceyaml"
	"go.jacobcolvin.com/niceyaml/paths"
	"go.jacobcolvin.com/niceyaml/schema"
	"go.jacobcolvin.com/x/jsonschema"

	_ "embed"
)

//go:generate go run ../../internal/schemagen -o config.schema.json

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
// organization, project, and workspace filters, the opt-in include toggles, the
// archive, extract, and export directories, and the export templates. Per-run
// and secret
// settings (the progress mode, the retry-absent toggle, and the API token)
// are supplied by flags and the environment instead, so a configuration file
// never carries a credential; any path it names resolves relative to the file
// itself so the two travel together.
//
// Every field is optional; an absent field takes the package default. Create
// instances with [LoadFile], or [DefaultFile] for the defaults alone.
//
//nolint:govet // Field order sets the generated schema's property order.
type File struct {
	// Address is the HCP Terraform API address. It defaults to [DefaultAddress].
	Address string `json:"address,omitempty" jsonschema:"title=Address,format=uri,default=https://app.terraform.io,examples=https://app.terraform.io|https://tfe.example.com"`
	// RateLimit is the ceiling on how fast requests launch, in requests per
	// second; omitted, the client's default of 30 (HCP Terraform's documented
	// limit) applies. The client adapts downward from the server's rate-limit
	// feedback on its own, so set this only for an organization whose granted
	// limit sits well below the default and the run should not probe past it.
	RateLimit float64 `json:"rateLimit,omitempty" jsonschema:"title=Rate Limit,exclusiveMinimum=0,default=30"`
	// Organizations limits the run to the named organizations. Names match
	// exactly; there are no glob patterns. An empty list archives every
	// organization the token can see.
	Organizations []string `json:"organizations,omitempty" jsonschema:"title=Organizations,uniqueItems=true"`
	// Projects limits the run to the named projects within each archived
	// organization. Names match exactly; there are no glob patterns. An empty
	// list archives every project. With both projects and workspaces set, a
	// workspace must satisfy both filters.
	Projects []string `json:"projects,omitempty" jsonschema:"title=Projects,uniqueItems=true"`
	// Workspaces limits the run to the named workspaces within each archived
	// organization. Names match exactly; there are no glob patterns. An empty
	// list archives every workspace. With both projects and workspaces set, a
	// workspace must satisfy both filters.
	Workspaces []string `json:"workspaces,omitempty" jsonschema:"title=Workspaces,uniqueItems=true"`
	// Include selects the heavy or optional surfaces to archive, each off by
	// default.
	Include FileInclude `json:"include,omitzero" jsonschema:"title=Include"`
	// RunHistory bounds how much of each workspace's run history a run
	// fetches; unset fetches every run.
	RunHistory FileRunHistory `json:"runHistory,omitzero" jsonschema:"title=Run History"`
	// Archive locates the archive on local disk; unset leaves the location to
	// the --archive-path flag.
	Archive FileArchive `json:"archive,omitzero" jsonschema:"title=Archive"`
	// Remote mirrors the archive to a remote object store, evicting sealed
	// cold bundles and settled tarballs and syncing everything else; unset
	// keeps the whole archive on local disk.
	Remote FileRemote `json:"remote,omitzero" jsonschema:"title=Remote"`
	// Extract configures where the extract command writes; unset leaves the
	// location to the --extract-path flag.
	Extract FileExtract `json:"extract,omitzero" jsonschema:"title=Extract"`
	// Export configures the export command: where it writes and the page
	// templates it renders with. Unset leaves the location to the
	// --export-path flag and keeps the built-in templates.
	Export FileExport `json:"export,omitzero" jsonschema:"title=Export"`
}

// FileArchive locates the archive on local disk.
type FileArchive struct {
	// Path is the archive root directory: the default for the root command's
	// --archive-path flag and for the read commands' [archive-path]
	// positional. A relative path resolves against this file's directory. An
	// explicit flag or positional always wins.
	Path string `json:"path,omitempty" jsonschema:"title=Path"`
}

// FileExtract configures where the extract command writes.
type FileExtract struct {
	// Path is the default --extract-path directory the extract command
	// recovers files into. A relative path resolves against this file's
	// directory. An explicit --extract-path always wins.
	Path string `json:"path,omitempty" jsonschema:"title=Path"`
}

// FileExport configures the export command: the directory it writes into and
// the page templates it renders with.
type FileExport struct {
	// Path is the default --export-path directory the export command writes
	// its markdown tree into. A relative path resolves against this file's
	// directory. An explicit --export-path always wins.
	Path string `json:"path,omitempty" jsonschema:"title=Path"`
	// Templates locates the page templates the export renders with; unset
	// keeps the built-in templates.
	Templates FileExportTemplates `json:"templates,omitzero" jsonschema:"title=Templates"`
}

// FileExportTemplates locates the page templates the export command renders
// with.
type FileExportTemplates struct {
	// Path is a directory of *.md.tmpl files overriding the export's
	// built-in page templates by filename: archive, org, projects, project,
	// workspace, stack, teams, variable-sets, and policy-sets, each with the
	// .md.tmpl suffix. Pages without an override keep their default. A
	// relative path resolves against this file's directory.
	Path string `json:"path,omitempty" jsonschema:"title=Path"`
}

// FileRunHistory bounds how much of each workspace's run history a run
// fetches; unset fetches every run.
type FileRunHistory struct {
	// Fetch bounds what the run walk fetches; unset fetches every run.
	Fetch FileRunHistoryFetch `json:"fetch,omitzero" jsonschema:"title=Fetch"`
}

// FileRunHistoryFetch bounds how much of each workspace's run history a run
// fetches. Each bound is a guarantee of inclusion, optional and off at its
// zero value; when both are set the run walk fetches whichever admits more
// history, so a run is archived while it sits among the newest count runs
// or was created within the age window. With neither bound set every run is
// fetched. The bounds limit what a run fetches; they never remove runs
// already archived.
type FileRunHistoryFetch struct {
	// Count fetches the newest count runs of each workspace; zero means
	// unbounded.
	Count int `json:"count,omitempty" jsonschema:"title=Count,minimum=0"`
	// Age fetches each workspace's runs created within this window before
	// the archive runs, as a duration string in Go syntax extended with a
	// day unit, such as 90d or 2160h; zero means unbounded.
	Age Duration `json:"age,omitempty" jsonschema:"title=Age"`
}

// FileInclude holds the opt-in toggles for the heavy or most
// organization-specific surfaces, each archived only when a configuration
// turns it on.
type FileInclude struct {
	// Stacks enables archiving of Stacks.
	Stacks bool `json:"stacks,omitempty" jsonschema:"title=Stacks,default=false"`
	// HYOK enables archiving of hold-your-own-key configurations.
	HYOK bool `json:"hyok,omitempty" jsonschema:"title=HYOK,default=false"`
	// RegistryDetail enables the deeper registry version, platform, and binary
	// detail.
	RegistryDetail bool `json:"registryDetail,omitempty" jsonschema:"title=Registry Detail,default=false"`
	// AuditTrail enables archiving of the audit trail.
	AuditTrail bool `json:"auditTrail,omitempty" jsonschema:"title=Audit Trail,default=false"`
}

// FileRemote mirrors the archive to a remote object store. With the section
// set, the store holds a complete copy: sealed cold bundles and settled
// configuration-version tarballs are evicted to it (uploaded, verified, then
// removed locally), and every other archive file is synced to it
// incrementally at each organization run's close, with local disk staying
// the canonical searchable copy. Setting any field requires URL; leaving the
// whole section unset keeps the archive entirely local. Credentials are
// never configured here: the URL's scheme selects the backend, and each
// backend authenticates through its provider's default chain (the AWS SDK
// chain for s3://, Azure's environment variables or DefaultAzureCredential
// for azblob://).
type FileRemote struct {
	// URL locates the bucket in gocloud.dev form, selecting the backend by
	// scheme: "s3://bucket?region=us-east-1" (AWS S3, or a compatible store
	// such as MinIO, R2, or Ceph RGW via endpoint and use_path_style query
	// parameters), "azblob://container" (Azure Blob Storage), or
	// "file:///path" (a local directory tree). Required when any other
	// remote field is set.
	URL string `json:"url,omitempty" jsonschema:"title=URL,minLength=1,examples=s3://bucket?region=us-east-1|azblob://container|file:///mnt/archive-mirror"`
	// Prefix is an optional key prefix objects are stored under: keys compose
	// as <prefix>/<org>/<path>, and leading or trailing slashes in the prefix
	// are normalized away.
	Prefix string `json:"prefix,omitempty" jsonschema:"title=Prefix"`
	// Upload tunes the multipart uploads that carry large bodies to the
	// store; unset takes each backend's defaults.
	Upload FileRemoteUpload `json:"upload,omitzero" jsonschema:"title=Upload"`
}

// FileRemoteUpload tunes the multipart uploads that carry large bodies to the
// remote store. Each knob is optional; omitted, the backend's default
// applies.
type FileRemoteUpload struct {
	// PartSize is the upload part size for backends that split a large body
	// into parts, as a plain byte count or a suffixed size such as 64MiB;
	// omitted, the backend's default applies.
	PartSize ByteSize `json:"partSize,omitempty" jsonschema:"title=Part Size"`
	// Concurrency is the number of upload parts in flight per bundle, at
	// least one; omitted, the backend's default applies.
	Concurrency int `json:"concurrency,omitempty" jsonschema:"title=Concurrency,minimum=1"`
}

// IsZero reports whether the whole remote section was left unset, which
// disables offloading.
func (fr FileRemote) IsZero() bool {
	return fr == FileRemote{}
}

// RemoteConfig resolves the section into the [RemoteConfig] passed to
// [WithRemote].
func (fr FileRemote) RemoteConfig() RemoteConfig {
	return RemoteConfig{
		URL:         fr.URL,
		Prefix:      fr.Prefix,
		PartSize:    int64(fr.Upload.PartSize),
		Concurrency: fr.Upload.Concurrency,
	}
}

// DefaultFile returns a [*File] populated with the package defaults, used when
// no configuration file is supplied.
func DefaultFile() *File {
	return &File{
		Address: DefaultAddress,
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
		// A nil body is a truly empty document; decoding one panics, so guard it.
		if doc.Document().Body == nil {
			continue
		}

		// A document that carries no value (blank, comments only such as a lone
		// schema directive, or an explicit null) decodes to nil and leaves the
		// defaults in place. Such a document has a non-nil null or comment body
		// under go-yaml, so probe the decoded value rather than the node type.
		var probe any

		err = doc.Decode(&probe)
		if err != nil {
			//nolint:wrapcheck // WrapError returns a source-annotated niceyaml.Error.
			return nil, source.WrapError(err)
		}

		if probe == nil {
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
func (f File) ValidateSchema(ctx context.Context, data any) error {
	//nolint:wrapcheck // ValidateSchema returns a niceyaml.Error with path info.
	return configValidator.ValidateSchema(ctx, data)
}

// Validate reports whether the [File] is internally consistent after decoding.
// It implements [niceyaml.Validator] so [LoadFile] runs it automatically. The
// per-entry filter constraints live in the JSON schema, which validates before
// decoding; these checks cover what the schema cannot express: the remote
// cross-field invariant, and the address URL shape (the schema's URI format is
// an annotation draft 2020-12 validators do not assert). An empty address is
// fine here, meaning the default; the resolved [Config] requires a usable one.
func (f File) Validate() error {
	if f.Address != "" {
		err := ValidateAddress(f.Address)
		if err != nil {
			return niceyaml.NewError(err.Error(),
				niceyaml.WithPath(paths.Root().Child("address").Value()),
			)
		}
	}

	if !f.Remote.IsZero() && f.Remote.URL == "" {
		return niceyaml.NewError("remote url is required when any remote field is set",
			niceyaml.WithPath(paths.Root().Child("remote").Value()),
		)
	}

	return nil
}
