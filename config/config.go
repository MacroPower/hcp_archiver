package config

import (
	"errors"
	"fmt"
	"os"
	"time"
)

// Default settings applied when an [Option] leaves a field unset.
const (
	// DefaultAddress is the HCP Terraform API address used when none is given.
	DefaultAddress = "https://app.terraform.io"
	// DefaultWorkspaceConcurrency is the number of workspaces archived at once.
	DefaultWorkspaceConcurrency = 4
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
	ErrMissingOutputDir = errors.New("output directory is required")
	// ErrInvalidConcurrency indicates a workspace concurrency below one.
	ErrInvalidConcurrency = errors.New("workspace concurrency must be at least 1")
	// ErrInvalidProgressMode indicates an unrecognized progress mode.
	ErrInvalidProgressMode = errors.New("invalid progress mode")
	// ErrInvalidProgressInterval indicates a progress interval of zero or less.
	ErrInvalidProgressInterval = errors.New("progress interval must be greater than zero")
)

// Config holds the already-resolved settings that govern a single archive run.
//
// Create instances with [New]. All fields are plain values; a Config performs
// no I/O beyond reading the environment during construction.
type Config struct {
	// Token is the HCP Terraform API token used to authenticate.
	Token string
	// Address is the HCP Terraform API address.
	Address string
	// Organization limits the run to a single organization; empty means every
	// organization the token can see.
	Organization string
	// OutputDir is the archive root; pointing at an existing archive makes the
	// run a resume rather than a fresh start.
	OutputDir string
	// ProgressMode selects how the run reports live progress.
	ProgressMode ProgressMode
	// ProgressInterval is the cadence at which progress is reported.
	ProgressInterval time.Duration
	// WorkspaceConcurrency is the number of workspaces archived concurrently.
	WorkspaceConcurrency int
	// RecheckAbsent forces re-probing of objects previously recorded as
	// permanently gone.
	RecheckAbsent bool
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

// Option configures a [Config] passed to [New].
//
// The available options are:
//   - [WithToken]
//   - [WithAddress]
//   - [WithOrganization]
//   - [WithOutputDir]
//   - [WithProgressMode]
//   - [WithProgressInterval]
//   - [WithWorkspaceConcurrency]
//   - [WithRecheckAbsent]
//   - [WithStacks]
//   - [WithHYOK]
//   - [WithRegistryDetail]
//   - [WithAuditTrail]
type Option func(*Config)

// WithToken sets the API token, taking precedence over the environment.
// It returns an [Option].
func WithToken(token string) Option {
	return func(c *Config) {
		c.Token = token
	}
}

// WithAddress sets the HCP Terraform API address. It returns an [Option].
func WithAddress(address string) Option {
	return func(c *Config) {
		c.Address = address
	}
}

// WithOrganization limits the run to a single organization. It returns an
// [Option].
func WithOrganization(org string) Option {
	return func(c *Config) {
		c.Organization = org
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

// WithWorkspaceConcurrency sets the number of workspaces archived concurrently.
// It returns an [Option].
func WithWorkspaceConcurrency(n int) Option {
	return func(c *Config) {
		c.WorkspaceConcurrency = n
	}
}

// WithRecheckAbsent toggles re-probing of permanently-absent objects. It
// returns an [Option].
func WithRecheckAbsent(recheck bool) Option {
	return func(c *Config) {
		c.RecheckAbsent = recheck
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

// New creates a new [Config].
//
// It starts from the package defaults, applies each [Option] in order, resolves
// the API token from [EnvToken], then [EnvTokenTFC], then [EnvTokenFallback]
// when no token was set, and validates the result. It reads environment
// variables but performs no other I/O.
func New(opts ...Option) (*Config, error) {
	c := &Config{
		Address:              DefaultAddress,
		ProgressMode:         DefaultProgressMode,
		ProgressInterval:     DefaultProgressInterval,
		WorkspaceConcurrency: DefaultWorkspaceConcurrency,
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

// Validate reports whether the [Config] is internally consistent.
//
// It returns [ErrMissingToken], [ErrMissingOutputDir], [ErrInvalidConcurrency],
// or [ErrInvalidProgressMode] wrapped with context on the first problem found.
func (c *Config) Validate() error {
	if c.Token == "" {
		return ErrMissingToken
	}

	if c.OutputDir == "" {
		return ErrMissingOutputDir
	}

	if c.WorkspaceConcurrency < 1 {
		return fmt.Errorf("%w: %d", ErrInvalidConcurrency, c.WorkspaceConcurrency)
	}

	if !c.ProgressMode.valid() {
		return fmt.Errorf("%w: %q", ErrInvalidProgressMode, c.ProgressMode)
	}

	if c.ProgressInterval <= 0 {
		return fmt.Errorf("%w: %s", ErrInvalidProgressInterval, c.ProgressInterval)
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
