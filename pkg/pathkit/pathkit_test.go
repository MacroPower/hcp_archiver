package pathkit_test

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"

	"go.jacobcolvin.com/hcp_archiver/pkg/pathkit"
)

// abs joins slash-separated segments into an absolute path in the host's
// separators, so the tables read naturally on every platform.
func abs(p string) string {
	return filepath.FromSlash(p)
}

func TestContains(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		parent string
		child  string
		want   bool
	}{
		"equal paths contain": {
			parent: "/data/archive", child: "/data/archive", want: true,
		},
		"direct child is inside": {
			parent: "/data/archive", child: "/data/archive/org", want: true,
		},
		"deep descendant is inside": {
			parent: "/data/archive", child: "/data/archive/org/projects/p", want: true,
		},
		"parent is not inside its child": {
			parent: "/data/archive/org", child: "/data/archive", want: false,
		},
		"sibling sharing a name prefix is outside": {
			parent: "/data/archive", child: "/data/archive-backup", want: false,
		},
		"unrelated tree is outside": {
			parent: "/data/archive", child: "/tmp/out", want: false,
		},
		"the root contains everything": {
			parent: "/", child: "/data/archive", want: true,
		},
		"relative against absolute is outside": {
			// The two cannot be related by filepath.Rel, which reads as
			// outside rather than an error the guard must handle; on Windows
			// the same shape covers two volumes.
			parent: "/data/archive", child: "data/archive", want: false,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tc.want, pathkit.Contains(abs(tc.parent), abs(tc.child)))
		})
	}
}

func TestOverlaps(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		a    string
		b    string
		want bool
	}{
		"same directory overlaps": {
			a: "/data/archive", b: "/data/archive", want: true,
		},
		"child overlaps its parent": {
			a: "/data/archive/org", b: "/data/archive", want: true,
		},
		"parent overlaps its child": {
			a: "/data/archive", b: "/data/archive/org", want: true,
		},
		"ancestor reach-back overlaps": {
			// The extract reach-back case: a target that is an ancestor of the
			// archive writes "<org>/<path>" straight back into it. Here a is
			// what joining the organization name "mini-org" under the target
			// "/data" produces, landing exactly on the archive directory b.
			a: "/data/mini-org", b: "/data/mini-org", want: true,
		},
		"siblings do not overlap": {
			a: "/data/archive", b: "/data/out", want: false,
		},
		"name-prefix siblings do not overlap": {
			a: "/data/archive", b: "/data/archive-backup", want: false,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tc.want, pathkit.Overlaps(abs(tc.a), abs(tc.b)))
		})
	}
}

func TestUnderPrefix(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		relPath string
		prefix  string
		want    bool
	}{
		"empty prefix names everything": {
			relPath: "runs/run-1/plan.log", prefix: "", want: true,
		},
		"the prefix itself matches": {
			relPath: "runs/run-1", prefix: "runs/run-1", want: true,
		},
		"a descendant matches": {
			relPath: "runs/run-1/plan.log", prefix: "runs/run-1", want: true,
		},
		"a name sharing the prefix's leaf as a prefix does not": {
			relPath: "runs/run-10", prefix: "runs/run-1", want: false,
		},
		"an unrelated path does not": {
			relPath: "state-versions/sv-1.json", prefix: "runs", want: false,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tc.want, pathkit.UnderPrefix(tc.relPath, tc.prefix))
		})
	}
}

func TestConfineSlash(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		relPath string
		want    string
	}{
		"clean path is unchanged": {
			relPath: "projects/p/workspaces/w", want: "projects/p/workspaces/w",
		},
		"leading dot-dot collapses at the root": {
			relPath: "../../etc/passwd", want: "etc/passwd",
		},
		"interior dot-dot resolves": {
			relPath: "a/../b", want: "b",
		},
		"leading separator strips": {
			relPath: "/rooted", want: "rooted",
		},
		"empty path confines to the root": {
			relPath: "", want: "",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tc.want, pathkit.ConfineSlash(tc.relPath))
		})
	}
}

func TestConfineJoin(t *testing.T) {
	t.Parallel()

	root := abs("/data/archive")

	tests := map[string]struct {
		relPath string
		want    string
	}{
		"clean path joins beneath the root": {
			relPath: "org/org.json", want: "/data/archive/org/org.json",
		},
		"escape attempt stays beneath the root": {
			relPath: "../../etc/passwd", want: "/data/archive/etc/passwd",
		},
		"native separators are accepted": {
			relPath: filepath.FromSlash("org/nested/file"), want: "/data/archive/org/nested/file",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, abs(tc.want), pathkit.ConfineJoin(root, tc.relPath))
		})
	}
}
