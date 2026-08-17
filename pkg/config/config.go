package config

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

// Default settings applied when an [Option] leaves a field unset.
const (
	// DefaultAddress is the HCP Terraform API address used when none is given.
	DefaultAddress = "https://app.terraform.io"
	// DefaultProgressMode is the progress mode used when none is given.
	DefaultProgressMode = ProgressModeAuto
	// DefaultProgressInterval is the reporting cadence used when none is given.
	DefaultProgressInterval = 5 * time.Second
)

// Environment variables consulted for the API token, in precedence order.
const (
	// EnvToken is the primary token variable, checked before [EnvTokenTFC] and
	// [EnvTokenFallback].
	EnvToken = "HCP_TOKEN"
	// EnvTokenTFC is the secondary token variable, retained for compatibility
	// with the tool's former name.
	EnvTokenTFC = "TFC_TOKEN"
	// EnvTokenFallback is the tertiary token variable.
	EnvTokenFallback = "TFE_TOKEN"
)

// Sentinel errors reported by [New] and [Config.Validate].
var (
	// ErrMissingToken indicates that no API token was supplied or found in the
	// environment.
	ErrMissingToken = errors.New("api token is required (set HCP_TOKEN, TFC_TOKEN, or TFE_TOKEN)")
	// ErrMissingOutputDir indicates that no output directory was supplied.
	ErrMissingOutputDir = errors.New(
		"output directory is required (set --archive-dir or archiveDir in the configuration file)",
	)
	// ErrInvalidOrganization indicates an organization name that is not a single
	// path segment, and so cannot root an organization's archive.
	ErrInvalidOrganization = errors.New("organization name must be a single path segment")
	// ErrInvalidProgressMode indicates an unrecognized progress mode.
	ErrInvalidProgressMode = errors.New("invalid progress mode")
	// ErrInvalidProgressInterval indicates a progress interval of zero or less.
	ErrInvalidProgressInterval = errors.New("progress interval must be greater than zero")
	// ErrInvalidRunHistoryCount indicates a negative run-history count.
	ErrInvalidRunHistoryCount = errors.New("run history count must not be negative")
	// ErrInvalidRunHistoryAge indicates a negative run-history age.
	ErrInvalidRunHistoryAge = errors.New("run history age must not be negative")
	// ErrMissingRemoteURL indicates a remote configuration naming no bucket URL.
	ErrMissingRemoteURL = errors.New("remote url is required")
	// ErrInvalidRemotePartSize indicates a negative remote part size.
	ErrInvalidRemotePartSize = errors.New("remote part size must not be negative")
	// ErrInvalidRemoteConcurrency indicates a negative remote upload concurrency.
	ErrInvalidRemoteConcurrency = errors.New("remote concurrency must not be negative")
)

// Config holds the already-resolved settings that govern a single archive run.
//
// Create instances with [New]. All fields are plain values; a Config performs
// no I/O beyond reading the environment during construction.
type Config struct {
	// Remote mirrors the archive to a remote object store, evicting sealed
	// cold bundles and settled tarballs and syncing everything else; nil
	// keeps the whole archive on local disk.
	Remote *RemoteConfig
	// Token is the HCP Terraform API token used to authenticate.
	Token string
	// Address is the HCP Terraform API address.
	Address string
	// OutputDir is the archive root; pointing at an existing archive makes the
	// run a resume rather than a fresh start.
	OutputDir string
	// ProgressMode selects how the run reports live progress.
	ProgressMode ProgressMode
	// Organizations limits the run to the named organizations; an empty list
	// means every organization the token can see.
	Organizations []string
	// Projects limits the run to the named projects within each archived
	// organization; an empty list means every project.
	Projects []string
	// Workspaces limits the run to the named workspaces within each archived
	// organization; an empty list means every workspace.
	Workspaces []string
	// ProgressInterval is the cadence at which progress is reported.
	ProgressInterval time.Duration
	// RateLimit is the ceiling of the client's adaptive rate governor, in
	// requests per second; zero keeps the client's default. HCP Terraform's
	// documented limit is 30 requests per second, but some organizations are
	// provisioned lower, so this is the knob to turn a run down to what the
	// server actually grants.
	RateLimit float64
	// RunHistoryAge bounds each workspace's archived run history to runs
	// created within this window before the archive runs; zero leaves the age
	// unbounded. When RunHistoryCount is also set, whichever bound admits more
	// history wins.
	RunHistoryAge time.Duration
	// RunHistoryCount bounds each workspace's archived run history to its
	// newest count runs; zero leaves the count unbounded. When RunHistoryAge is
	// also set, whichever bound admits more history wins.
	RunHistoryCount int
	// RetryAbsent forces re-probing of objects previously recorded as absent.
	RetryAbsent bool
	// Stacks enables archiving of Stacks.
	Stacks bool
	// HYOK enables archiving of hold-your-own-key configurations.
	HYOK bool
	// RegistryDetail enables the deeper registry version, platform, and binary
	// detail.
	RegistryDetail bool
	// AuditTrail enables archiving of the audit trail.
	AuditTrail bool
}

// RemoteConfig holds the resolved settings for mirroring the archive to a
// remote object store: sealed cold bundles and settled tarballs are evicted
// to it, and the rest of the archive is synced to it at each organization
// run's close. It mirrors the remote package's client configuration field
// for field so this package needs no storage-SDK dependency; the archiver
// performs the mapping. Credentials are never part of it: each backend
// authenticates through its provider's default chain.
type RemoteConfig struct {
	// URL locates the bucket in gocloud.dev form, selecting the backend by
	// scheme: s3://, azblob://, or file://.
	URL string
	// Prefix is an optional key prefix objects are stored under.
	Prefix string
	// PartSize is the upload part size in bytes for backends that split a
	// large body into parts; zero takes the backend's default.
	PartSize int64
	// Concurrency is the number of upload parts in flight per bundle; zero
	// takes the backend's default.
	Concurrency int
}

// Option configures a [Config] passed to [New].
//
// The available options are:
//   - [WithToken]
//   - [WithAddress]
//   - [WithOrganizations]
//   - [WithProjects]
//   - [WithWorkspaces]
//   - [WithOutputDir]
//   - [WithProgressMode]
//   - [WithProgressInterval]
//   - [WithRunHistoryCount]
//   - [WithRunHistoryAge]
//   - [WithRateLimit]
//   - [WithRetryAbsent]
//   - [WithStacks]
//   - [WithHYOK]
//   - [WithRegistryDetail]
//   - [WithAuditTrail]
//   - [WithRemote]
type Option func(*Config)

// WithToken sets the API token, taking precedence over the environment.
// It returns an [Option].
func WithToken(token string) Option {
	return func(c *Config) {
		c.Token = token
	}
}

// WithAddress sets the HCP Terraform API address. An empty address is ignored so
// it does not clobber [DefaultAddress], matching the client's own guard. It
// returns an [Option].
func WithAddress(address string) Option {
	return func(c *Config) {
		if address != "" {
			c.Address = address
		}
	}
}

// WithOrganizations limits the run to the named organizations. An empty list
// archives every organization the token can see. It returns an [Option].
func WithOrganizations(orgs []string) Option {
	return func(c *Config) {
		c.Organizations = orgs
	}
}

// WithProjects limits the run to the named projects within each archived
// organization. An empty list archives every project. It returns an [Option].
func WithProjects(projects []string) Option {
	return func(c *Config) {
		c.Projects = projects
	}
}

// WithWorkspaces limits the run to the named workspaces within each archived
// organization. An empty list archives every workspace. It returns an
// [Option].
func WithWorkspaces(workspaces []string) Option {
	return func(c *Config) {
		c.Workspaces = workspaces
	}
}

// WithOutputDir sets the archive root directory. It returns an [Option].
func WithOutputDir(dir string) Option {
	return func(c *Config) {
		c.OutputDir = dir
	}
}

// WithProgressMode sets the progress mode. It returns an [Option].
func WithProgressMode(mode ProgressMode) Option {
	return func(c *Config) {
		c.ProgressMode = mode
	}
}

// WithProgressInterval sets the progress reporting cadence. It returns an
// [Option].
func WithProgressInterval(interval time.Duration) Option {
	return func(c *Config) {
		c.ProgressInterval = interval
	}
}

// WithRunHistoryCount bounds each workspace's archived run history to its
// newest n runs; zero leaves the count unbounded. It returns an [Option].
func WithRunHistoryCount(n int) Option {
	return func(c *Config) {
		c.RunHistoryCount = n
	}
}

// WithRunHistoryAge bounds each workspace's archived run history to runs
// created within age before the archive runs; zero leaves the age unbounded.
// It returns an [Option].
func WithRunHistoryAge(age time.Duration) Option {
	return func(c *Config) {
		c.RunHistoryAge = age
	}
}

// WithRateLimit sets the ceiling of the client's adaptive rate governor, in
// requests per second, for organizations whose server-side limit sits below
// the client's default. A non-positive value is ignored so it does not clobber
// that default, matching the client's own guard. It returns an [Option].
func WithRateLimit(perSecond float64) Option {
	return func(c *Config) {
		if perSecond > 0 {
			c.RateLimit = perSecond
		}
	}
}

// WithRetryAbsent toggles re-probing of objects previously recorded as
// absent. It returns an [Option].
func WithRetryAbsent(retry bool) Option {
	return func(c *Config) {
		c.RetryAbsent = retry
	}
}

// WithStacks toggles archiving of Stacks. It returns an [Option].
func WithStacks(enabled bool) Option {
	return func(c *Config) {
		c.Stacks = enabled
	}
}

// WithHYOK toggles archiving of hold-your-own-key configurations. It returns an
// [Option].
func WithHYOK(enabled bool) Option {
	return func(c *Config) {
		c.HYOK = enabled
	}
}

// WithRegistryDetail toggles the deeper registry detail. It returns an
// [Option].
func WithRegistryDetail(enabled bool) Option {
	return func(c *Config) {
		c.RegistryDetail = enabled
	}
}

// WithAuditTrail toggles archiving of the audit trail. It returns an [Option].
func WithAuditTrail(enabled bool) Option {
	return func(c *Config) {
		c.AuditTrail = enabled
	}
}

// WithRemote enables mirroring the archive to the object store rc
// describes: cold surfaces evict there and everything else syncs there. It
// returns an [Option].
func WithRemote(rc RemoteConfig) Option {
	return func(c *Config) {
		c.Remote = &rc
	}
}

// New creates a new [Config].
//
// It starts from the package defaults, applies each [Option] in order, resolves
// the API token from [EnvToken], then [EnvTokenTFC], then [EnvTokenFallback]
// when no token was set, and validates the result. It reads environment
// variables but performs no other I/O.
func New(opts ...Option) (*Config, error) {
	c := &Config{
		Address:          DefaultAddress,
		ProgressMode:     DefaultProgressMode,
		ProgressInterval: DefaultProgressInterval,
	}

	for _, opt := range opts {
		opt(c)
	}

	if c.Token == "" {
		c.Token = tokenFromEnv()
	}

	err := c.Validate()
	if err != nil {
		return nil, err
	}

	return c, nil
}

// ValidateOrganizationName reports whether name is usable as the single
// directory segment an organization's archive is rooted at.
//
// The archiver joins the name onto the output directory to root the
// organization's store, and onto the remote key prefix to root its mirror.
// Both joins collapse ".." lexically, so a name carrying path separators, dot
// entries, or control characters would place the whole organization (its
// store, its manifest lock, and every mirrored object) outside the archive
// root and outside the configured prefix. Organization names arrive from the
// server's listing as readily as from an operator, so this is a boundary the
// archiver enforces on every organization it archives rather than a check on
// operator input alone. It returns [ErrInvalidOrganization] wrapped with the
// name.
func ValidateOrganizationName(name string) error {
	control := func(r rune) bool { return r < 0x20 || r == 0x7f }

	if name == "" || name == "." || name == ".." ||
		strings.ContainsAny(name, `/\`) || strings.ContainsFunc(name, control) {
		return fmt.Errorf("%w: %q", ErrInvalidOrganization, name)
	}

	return nil
}

// Validate reports whether the [Config] is internally consistent.
//
// It returns [ErrMissingToken], [ErrMissingOutputDir],
// [ErrInvalidOrganization], [ErrInvalidRunHistoryCount],
// [ErrInvalidRunHistoryAge], [ErrInvalidProgressMode],
// [ErrInvalidProgressInterval], or, when a remote section is configured,
// [ErrMissingRemoteURL], [ErrInvalidRemotePartSize], or
// [ErrInvalidRemoteConcurrency], wrapped with context on the first problem
// found.
func (c *Config) Validate() error {
	if c.Token == "" {
		return ErrMissingToken
	}

	if c.OutputDir == "" {
		return ErrMissingOutputDir
	}

	// Named organizations are rejected here so an operator's typo fails before
	// the run builds a client; the archiver re-checks every name it actually
	// archives, since auto-discovery never passes through this list.
	for _, org := range c.Organizations {
		err := ValidateOrganizationName(org)
		if err != nil {
			return err
		}
	}

	if c.RunHistoryCount < 0 {
		return fmt.Errorf("%w: %d", ErrInvalidRunHistoryCount, c.RunHistoryCount)
	}

	if c.RunHistoryAge < 0 {
		return fmt.Errorf("%w: %s", ErrInvalidRunHistoryAge, c.RunHistoryAge)
	}

	if !c.ProgressMode.valid() {
		return fmt.Errorf("%w: %q", ErrInvalidProgressMode, c.ProgressMode)
	}

	if c.ProgressInterval <= 0 {
		return fmt.Errorf("%w: %s", ErrInvalidProgressInterval, c.ProgressInterval)
	}

	return c.validateRemote()
}

// validateRemote checks the remote section when one is configured: a bucket
// URL is required, and the tuning knobs must not be negative.
func (c *Config) validateRemote() error {
	if c.Remote == nil {
		return nil
	}

	if c.Remote.URL == "" {
		return ErrMissingRemoteURL
	}

	if c.Remote.PartSize < 0 {
		return fmt.Errorf("%w: %d", ErrInvalidRemotePartSize, c.Remote.PartSize)
	}

	if c.Remote.Concurrency < 0 {
		return fmt.Errorf("%w: %d", ErrInvalidRemoteConcurrency, c.Remote.Concurrency)
	}

	return nil
}

// tokenFromEnv resolves the token from the environment, preferring [EnvToken]
// over [EnvTokenTFC] over [EnvTokenFallback] and treating an empty value as
// unset.
func tokenFromEnv() string {
	for _, env := range []string{EnvToken, EnvTokenTFC, EnvTokenFallback} {
		if v, ok := os.LookupEnv(env); ok && v != "" {
			return v
		}
	}

	return ""
}
