package config

import "fmt"

// ProgressMode selects how an archive run reports its live progress.
//
// The zero value is not valid; use [ParseProgressMode] to turn a flag value
// into a mode, and [ProgressMode.String] for the reverse.
type ProgressMode string

const (
	// ProgressModeAuto picks human output on a TTY and quiet output off one.
	ProgressModeAuto ProgressMode = "auto"
	// ProgressModeHuman renders periodic human-readable progress.
	ProgressModeHuman ProgressMode = "human"
	// ProgressModeJSON emits one machine-readable JSON object per update.
	ProgressModeJSON ProgressMode = "json"
	// ProgressModeQuiet suppresses progress output entirely.
	ProgressModeQuiet ProgressMode = "quiet"
)

// String returns the flag-facing spelling of the mode.
func (m ProgressMode) String() string {
	return string(m)
}

// valid reports whether the mode is one of the recognized values.
func (m ProgressMode) valid() bool {
	switch m {
	case ProgressModeAuto, ProgressModeHuman, ProgressModeJSON, ProgressModeQuiet:
		return true
	default:
		return false
	}
}

// ParseProgressMode converts a flag value into a [ProgressMode].
//
// It returns [ErrInvalidProgressMode] wrapped with the offending value when the
// value is not one of auto, human, json, or quiet.
func ParseProgressMode(s string) (ProgressMode, error) {
	m := ProgressMode(s)
	if !m.valid() {
		return "", fmt.Errorf("%w: %q", ErrInvalidProgressMode, s)
	}

	return m, nil
}
