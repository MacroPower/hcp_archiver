package pathkit

import (
	"path"
	"path/filepath"
	"strings"
)

// Contains reports whether absChild sits at or beneath absParent. The
// comparison is segment-wise over absolute paths, so a sibling sharing a name
// prefix ("archive-backup" beside "archive") is outside; unrelatable paths (a
// different volume) are outside by definition. Symlinks are not resolved:
// this is a lexical check, and physical identity belongs to the fsid package.
func Contains(absParent, absChild string) bool {
	rel, err := filepath.Rel(absParent, absChild)
	if err != nil {
		return false
	}

	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// Overlaps reports whether two absolute paths name the same directory or one
// sits inside the other, so a write under either could land in both. Like
// [Contains] it is lexical and segment-wise.
func Overlaps(absA, absB string) bool {
	return Contains(absA, absB) || Contains(absB, absA)
}

// UnderPrefix reports whether a slash-separated archive-relative path sits at
// or beneath prefix, matching whole segments so "runs/run-10" is not under
// "runs/run-1". An empty prefix names everything.
func UnderPrefix(relPath, prefix string) bool {
	return prefix == "" || relPath == prefix || strings.HasPrefix(relPath, prefix+"/")
}

// ConfineSlash confines a slash-separated relative path beneath an implicit
// root: the path is cleaned as if rooted at "/", so any leading ".." collapses
// at the root rather than escaping it, and the leading separator is stripped
// again. A clean relative path is unchanged.
func ConfineSlash(relPath string) string {
	return strings.TrimPrefix(path.Clean("/"+relPath), "/")
}

// ConfineJoin joins a relative path (in either separator style) beneath the
// absolute root absRoot, confined via [ConfineSlash] so no ".." escapes the
// root.
func ConfineJoin(absRoot, relPath string) string {
	clean := ConfineSlash(filepath.ToSlash(relPath))

	return filepath.Join(absRoot, filepath.FromSlash(clean))
}
