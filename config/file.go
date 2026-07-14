package config

import (
	"errors"
	"fmt"
	"time"

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
// organization, project, and workspace filters, and the opt-in scope toggles.
// Per-run and secret
// settings (the output directory, the progress mode, the retry-absent toggle,
// and the API token) are supplied by flags and the environment instead, so a
// configuration file never carries a machine-specific path or a credential.
//
// Every field is optional; an absent field takes the package default. Create
// instances with [LoadFile], or [DefaultFile] for the defaults alone.
type File struct {
	// Address is the HCP Terraform API address. It defaults to [DefaultAddress].
	Address string `json:"address,omitempty" jsonschema:"title=Address,default=https://app.terraform.io"`
	// Organizations limits the run to the named organizations. An empty list
	// archives every organization the token can see.
	Organizations []string `json:"organizations,omitempty" jsonschema:"title=Organizations"`
	// Projects limits the run to the named projects within each archived
	// organization. An empty list archives every project.
	Projects []string `json:"projects,omitempty" jsonschema:"title=Projects"`
	// Workspaces limits the run to the named workspaces within each archived
	// organization. An empty list archives every workspace.
	Workspaces []string `json:"workspaces,omitempty" jsonschema:"title=Workspaces"`
	// Remote mirrors the archive to an S3-compatible object store, evicting
	// sealed cold bundles and settled tarballs and syncing everything else;
	// unset keeps the whole archive on local disk.
	Remote FileRemote `json:"remote,omitzero" jsonschema:"title=Remote"`
	// RunHistory bounds how much of each workspace's run history is archived;
	// unset archives every run.
	RunHistory FileRunHistory `json:"runHistory,omitzero" jsonschema:"title=Run History"`
	// RateLimit is the ceiling on how fast requests launch, in requests per
	// second; omitted, the client's default of 30 (HCP Terraform's documented
	// limit) applies. The client adapts downward from the server's rate-limit
	// feedback on its own, so set this only for an organization whose granted
	// limit sits well below the default and the run should not probe past it.
	RateLimit float64 `json:"rateLimit,omitempty" jsonschema:"title=Rate Limit,exclusiveMinimum=0,default=30"`
	// Scope selects the heavy or optional surfaces to archive, each off by
	// default.
	Scope FileScope `json:"scope,omitzero" jsonschema:"title=Scope"`
}

// FileRunHistory bounds how much of each workspace's run history is archived.
// Each bound is optional and off at its zero value; when both are set the run
// walk keeps whichever admits more history, so a run is archived while it sits
// among the newest count runs or was created within the age window. With
// neither bound set every run is archived.
type FileRunHistory struct {
	// Count keeps the newest count runs of each workspace; zero means
	// unbounded.
	Count int `json:"count,omitempty" jsonschema:"title=Count,minimum=0"`
	// Age keeps each workspace's runs created within this window before the
	// archive runs, as a Go duration string such as 2160h; zero means
	// unbounded.
	Age time.Duration `json:"age,omitempty" jsonschema:"title=Age,type=string,pattern=^([0-9]+(\\.[0-9]+)?(ns|us|µs|ms|s|m|h))+$"`
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

// FileRemote mirrors the archive to an S3-compatible object store. With the
// section set, the store holds a complete copy: sealed cold bundles and
// settled configuration-version tarballs are evicted to it (uploaded,
// verified, then removed locally), and every other archive file is synced to
// it incrementally at each organization run's close, with local disk staying
// the canonical searchable copy. Eviction takes StorageClass; the synced
// search layer takes SyncStorageClass. Setting any field requires Bucket;
// leaving the whole section unset keeps the archive entirely local.
// Credentials are never configured here: the client authenticates through
// the AWS SDK default chain (environment variables, shared configuration,
// an instance or task role).
type FileRemote struct {
	// Bucket names the bucket archive objects are uploaded to; required when
	// any other remote field is set.
	Bucket string `json:"bucket,omitempty" jsonschema:"title=Bucket"`
	// Prefix is an optional key prefix objects are stored under.
	Prefix string `json:"prefix,omitempty" jsonschema:"title=Prefix"`
	// Endpoint overrides the S3 endpoint URL for compatible stores (MinIO,
	// R2, Ceph RGW); empty resolves the AWS default for the region.
	Endpoint string `json:"endpoint,omitempty" jsonschema:"title=Endpoint"`
	// Region is the bucket's region; empty defers to the SDK default chain.
	Region string `json:"region,omitempty" jsonschema:"title=Region"`
	// Checksums toggles the flexible-checksum headers the server validates
	// uploads with, on by default. Turn them off only for a compatible store
	// that rejects them; without checksums the remote verify gate is
	// existence and size alone.
	Checksums *bool `json:"checksums,omitempty" jsonschema:"title=Checksums,default=true"`
	// StorageClass is the storage class evicted cold surfaces (bundles and
	// settled configuration-version tarballs) are written with; empty takes
	// the store's default. Any class the store accepts is allowed, so
	// compatible stores' custom classes work; the examples are the AWS ones.
	StorageClass string `json:"storageClass,omitempty" jsonschema:"title=Storage Class,examples=STANDARD|STANDARD_IA|GLACIER_IR|GLACIER|DEEP_ARCHIVE"`
	// SyncStorageClass is the storage class synced search-layer files are
	// written with; empty takes the store's default. Keep it a directly
	// readable class (not GLACIER or DEEP_ARCHIVE): synced files change and
	// re-upload as the archive evolves, and restoring means downloading
	// them.
	SyncStorageClass string `json:"syncStorageClass,omitempty" jsonschema:"title=Sync Storage Class,examples=STANDARD|STANDARD_IA"`
	// PartSize is the multipart upload part size in bytes; zero takes the
	// transfer manager's default.
	PartSize int64 `json:"partSize,omitempty" jsonschema:"title=Part Size,minimum=0"`
	// Concurrency is the number of upload parts in flight per bundle; zero
	// takes the transfer manager's default.
	Concurrency int `json:"concurrency,omitempty" jsonschema:"title=Concurrency,minimum=0"`
	// ForcePathStyle addresses the bucket as a path segment rather than a
	// virtual host, the shape MinIO and Ceph RGW expect.
	ForcePathStyle bool `json:"forcePathStyle,omitempty" jsonschema:"title=Force Path Style"`
}

// IsZero reports whether the whole remote section was left unset, which
// disables offloading.
func (fr FileRemote) IsZero() bool {
	return fr == FileRemote{}
}

// RemoteConfig resolves the section into the [RemoteConfig] passed to
// [WithRemote], defaulting checksums on when the field is omitted.
func (fr FileRemote) RemoteConfig() RemoteConfig {
	return RemoteConfig{
		Bucket:           fr.Bucket,
		Prefix:           fr.Prefix,
		Endpoint:         fr.Endpoint,
		Region:           fr.Region,
		StorageClass:     fr.StorageClass,
		SyncStorageClass: fr.SyncStorageClass,
		PartSize:         fr.PartSize,
		Concurrency:      fr.Concurrency,
		ForcePathStyle:   fr.ForcePathStyle,
		DisableChecksums: fr.Checksums != nil && !*fr.Checksums,
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
func (f File) ValidateSchema(data any) error {
	//nolint:wrapcheck // ValidateSchema returns a niceyaml.Error with path info.
	return configValidator.ValidateSchema(data)
}

// Validate reports whether the [File] is internally consistent after decoding.
// It implements [niceyaml.Validator] so [LoadFile] runs it automatically, and
// rejects an empty or duplicated name in the organization, project, or
// workspace filter with an error pointing at the offending entry.
func (f File) Validate() error {
	filters := []struct {
		field string
		noun  string
		names []string
	}{
		{field: "organizations", noun: "organization", names: f.Organizations},
		{field: "projects", noun: "project", names: f.Projects},
		{field: "workspaces", noun: "workspace", names: f.Workspaces},
	}

	for _, filter := range filters {
		err := validateNames(filter.field, filter.noun, filter.names)
		if err != nil {
			return err
		}
	}

	if !f.Remote.IsZero() && f.Remote.Bucket == "" {
		return niceyaml.NewError("remote bucket is required when any remote field is set",
			niceyaml.WithPath(paths.Root().Child("remote").Value()),
		)
	}

	return nil
}

// validateNames rejects an empty or duplicated name in the filter list under
// field with an error pointing at the offending entry, naming it by noun.
func validateNames(field, noun string, names []string) error {
	seen := make(map[string]struct{}, len(names))

	for i, name := range names {
		if name == "" {
			return niceyaml.NewError(fmt.Sprintf("%s name must not be empty", noun),
				niceyaml.WithPath(paths.Root().Child(field).Index(i).Value()),
			)
		}

		if _, dup := seen[name]; dup {
			return niceyaml.NewError(fmt.Sprintf("duplicate %s %q", noun, name),
				niceyaml.WithPath(paths.Root().Child(field).Index(i).Value()),
			)
		}

		seen[name] = struct{}{}
	}

	return nil
}
