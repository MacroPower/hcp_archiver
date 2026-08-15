package audit_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	tfe "github.com/hashicorp/go-tfe/v2"

	"go.jacobcolvin.com/hcp_archiver/collect/audit"
)

func TestPageName(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		since time.Time
		want  string
		page  int
	}{
		"zero cursor formats to the epoch stamp": {
			since: time.Time{},
			page:  1,
			want:  "00010101T000000.000000000Z-p000000001.json",
		},
		"a cursor stamps in UTC without colons": {
			since: time.Date(2026, time.July, 8, 13, 4, 5, 0, time.UTC),
			page:  2,
			want:  "20260708T130405.000000000Z-p000000002.json",
		},
		"a non-UTC cursor is normalized to UTC": {
			since: time.Date(2026, time.July, 8, 9, 4, 5, 0, time.FixedZone("EST", -4*60*60)),
			page:  3,
			want:  "20260708T130405.000000000Z-p000000003.json",
		},
		"sub-second cursors keep their fractional precision": {
			since: time.Date(2026, time.July, 8, 13, 4, 5, 987654321, time.UTC),
			page:  1,
			want:  "20260708T130405.987654321Z-p000000001.json",
		},
		"high page numbers keep sorting past four digits": {
			since: time.Time{},
			page:  12345,
			want:  "00010101T000000.000000000Z-p000012345.json",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tc.want, audit.PageName(tc.since, tc.page))
		})
	}
}

func TestPageNameOrdersByPage(t *testing.T) {
	t.Parallel()

	since := time.Date(2026, time.July, 8, 0, 0, 0, 0, time.UTC)

	assert.Less(t, audit.PageName(since, 1), audit.PageName(since, 2))
	assert.Less(t, audit.PageName(since, 2), audit.PageName(since, 10))
	// The padding must hold lexical order across a digit-width boundary.
	assert.Less(t, audit.PageName(since, 9999), audit.PageName(since, 10000))
}

func TestPageNameDistinguishesSameSecondCursors(t *testing.T) {
	t.Parallel()

	// Two watermarks in the same wall-clock second must not collide, or the
	// later run's page would settle under a name the earlier run already wrote.
	early := time.Date(2026, time.July, 8, 13, 4, 5, 500_000_000, time.UTC)
	late := time.Date(2026, time.July, 8, 13, 4, 5, 900_000_000, time.UTC)

	assert.NotEqual(t, audit.PageName(early, 1), audit.PageName(late, 1))
	assert.Less(t, audit.PageName(early, 1), audit.PageName(late, 1))
}

func TestEventsAfter(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, time.July, 8, 0, 0, 0, 0, time.UTC)

	ev := func(id string, ts time.Time) *tfe.AuditTrail {
		return &tfe.AuditTrail{ID: id, Timestamp: ts}
	}

	tests := map[string]struct {
		since    time.Time
		items    []*tfe.AuditTrail
		archived []*tfe.AuditTrail
		want     []string
	}{
		"nothing archived keeps every newer event in order": {
			items: []*tfe.AuditTrail{
				ev("ev-2", base.Add(2*time.Hour)),
				ev("ev-1", base.Add(time.Hour)),
			},
			since: base,
			want:  []string{"ev-2", "ev-1"},
		},
		"events at or before the cursor are dropped": {
			items: []*tfe.AuditTrail{
				ev("ev-new", base.Add(time.Nanosecond)),
				ev("ev-at-mark", base),
				ev("ev-old", base.Add(-time.Hour)),
			},
			since: base,
			want:  []string{"ev-new"},
		},
		"a shifted page drops the events its settled sibling holds": {
			items: []*tfe.AuditTrail{
				ev("ev-5", base.Add(5*time.Hour)),
				ev("ev-4", base.Add(4*time.Hour)),
				ev("ev-3", base.Add(3*time.Hour)),
			},
			archived: []*tfe.AuditTrail{
				ev("ev-5", base.Add(5*time.Hour)),
				ev("ev-4", base.Add(4*time.Hour)),
			},
			since: base,
			want:  []string{"ev-3"},
		},
		"a page holding only archived events keeps nothing": {
			items: []*tfe.AuditTrail{
				ev("ev-2", base.Add(2*time.Hour)),
				ev("ev-1", base.Add(time.Hour)),
			},
			archived: []*tfe.AuditTrail{
				ev("ev-1", base.Add(time.Hour)),
				ev("ev-2", base.Add(2*time.Hour)),
			},
			since: base,
			want:  []string{},
		},
		"nil entries are skipped": {
			items: []*tfe.AuditTrail{
				nil,
				ev("ev-1", base.Add(time.Hour)),
				nil,
			},
			since: base,
			want:  []string{"ev-1"},
		},
		"an id-less event is never taken for archived": {
			items: []*tfe.AuditTrail{
				ev("", base.Add(time.Hour)),
				ev("", base.Add(2*time.Hour)),
			},
			archived: []*tfe.AuditTrail{ev("", base.Add(time.Hour))},
			since:    base,
			want:     []string{"", ""},
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			fresh := audit.EventsAfter(tc.items, tc.since, tc.archived)

			ids := make([]string, len(fresh))
			for i, e := range fresh {
				ids[i] = e.ID
			}

			assert.Equal(t, tc.want, ids)
		})
	}
}

func TestNewestTimestamp(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, time.July, 8, 0, 0, 0, 0, time.UTC)

	tests := map[string]struct {
		want  time.Time
		items []*tfe.AuditTrail
	}{
		"empty list is the zero time": {
			items: nil,
			want:  time.Time{},
		},
		"single entry is its own timestamp": {
			items: []*tfe.AuditTrail{{Timestamp: base}},
			want:  base,
		},
		"newest wins regardless of position": {
			items: []*tfe.AuditTrail{
				{Timestamp: base.Add(time.Hour)},
				{Timestamp: base.Add(3 * time.Hour)},
				{Timestamp: base.Add(2 * time.Hour)},
			},
			want: base.Add(3 * time.Hour),
		},
		"nil entries are skipped": {
			items: []*tfe.AuditTrail{
				nil,
				{Timestamp: base.Add(time.Hour)},
				nil,
			},
			want: base.Add(time.Hour),
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tc.want, audit.NewestTimestamp(tc.items))
		})
	}
}
