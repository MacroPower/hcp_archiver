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
	// DefaultMaxConcurrency is the ceiling the worker count may scale up to
	// while no rate limiting is observed. Every run starts at one worker and
	// scales itself toward the ceiling.
	DefaultMaxConcurrency = 16
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
	// ErrInvalidMaxConcurrency indicates a concurrency ceiling below one.
	ErrInvalidMaxConcurrency = errors.New("max concurrency must be at least 1")
	// ErrInvalidProgressMode indicates an unrecognized progress mode.
	ErrInvalidProgressMode = errors.New("invalid progress mode")
	// ErrInvalidProgressInterval indicates a progress interval of zero or less.
	ErrInvalidProgressInterval = errors.New("progress interval must be greater than zero")
	// ErrInvalidRunHistoryCount indicates a negative run-history count.
	ErrInvalidRunHistoryCount = errors.New("run history count must not be negative")
	// ErrInvalidRunHistoryAge indicates a negative run-history age.
	ErrInvalidRunHistoryAge = errors.New("run history age must not be negative")
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
	// OutputDir is the archive root; pointing at an existing archive makes the
	// run a resume rather than a fresh start.
	OutputDir string
	// ProgressMode selects how the run reports live progress.
	ProgressMode ProgressMode
	// Organizations limits the run to the named organizations; an empty list
	// means every organization the token can see.
	Organizations []string
	// ProgressInterval is the cadence at which progress is reported.
	ProgressInterval time.Duration
	// MaxConcurrency is the ceiling the worker count may scale up to while no
	// rate limiting is observed. A worker is one in-flight API request, not one
	// workspace: workers pull whatever unit of work is ready, so several can
	// serve a single large workspace at once. Every run starts at one worker
	// and scales itself toward the ceiling.
	MaxConcurrency int
	// RunHistoryAge bounds each workspace's archived run history to runs
	// created within this window before the archive runs; zero leaves the age
	// unbounded. When RunHistoryCount is also set, whichever bound admits more
	// history wins.
	RunHistoryAge time.Duration
	// RunHistoryCount bounds each workspace's archived run history to its
	// newest count runs; zero leaves the count unbounded. When RunHistoryAge is
	// also set, whichever bound admits more history wins.
	RunHistoryCount int
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
//   - [WithOrganizations]
//   - [WithOutputDir]
//   - [WithProgressMode]
//   - [WithProgressInterval]
//   - [WithMaxConcurrency]
//   - [WithRunHistoryCount]
//   - [WithRunHistoryAge]
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

// WithMaxConcurrency sets the ceiling the worker count may scale up to while
// no rate limiting is observed. It returns an [Option].
func WithMaxConcurrency(n int) Option {
	return func(c *Config) {
		c.MaxConcurrency = n
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
		Address:          DefaultAddress,
		ProgressMode:     DefaultProgressMode,
		ProgressInterval: DefaultProgressInterval,
		MaxConcurrency:   DefaultMaxConcurrency,
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
// It returns [ErrMissingToken], [ErrMissingOutputDir],
// [ErrInvalidMaxConcurrency], [ErrInvalidRunHistoryCount],
// [ErrInvalidRunHistoryAge], [ErrInvalidProgressMode], or
// [ErrInvalidProgressInterval] wrapped with context on the first problem found.
func (c *Config) Validate() error {
	if c.Token == "" {
		return ErrMissingToken
	}

	if c.OutputDir == "" {
		return ErrMissingOutputDir
	}

	if c.MaxConcurrency < 1 {
		return fmt.Errorf("%w: %d", ErrInvalidMaxConcurrency, c.MaxConcurrency)
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
