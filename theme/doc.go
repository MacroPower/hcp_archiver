// Package theme is the terminal design system shared by every surface the
// tool draws: the live progress panel, the closing summary block, and the
// interactive archive browser.
//
// Two different packages render those surfaces, and before this package each
// carried its own copy of the palette, the status glyphs, and the byte
// formatter. Copies drift: a color tweaked on one surface silently unmatched
// the other, and the browser printed "1.5 KB" where the panel printed
// "1.5 KiB" for the same count. Centralizing the visual vocabulary makes the
// surfaces read as one tool by construction rather than by discipline, and a
// lint rule (see forbidigo in .golangci.yaml) keeps raw colors from creeping
// back in elsewhere.
//
// The palette names semantic roles, not hues: a consumer asks for the success
// tone or the muted tone and never for green or gray, so a retune of the
// palette lands everywhere at once. Colors are truecolor values; lipgloss
// renders them fully saturated and Bubble Tea downsamples the composed frame
// to the terminal's actual profile, so rendering stays deterministic. The
// shared styles cover the roles every surface uses; packages compose their
// own surface-specific styles (borders, padding, faintness) by deriving from
// them — lipgloss styles are values, so a derivation never mutates the shared
// definition.
//
// The glyph vocabulary carries status at a glance (per-status counts on the
// panel, run badges in the browser) plus the small punctuation marks the
// chrome shares, so the same outcome always wears the same mark on every
// surface.
package theme
