package audit_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	tfe "github.com/hashicorp/go-tfe"

	"github.com/MacroPower/tfc_archiver/collect/audit"
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
			want:  "00010101T000000Z-p0001.json",
		},
		"a cursor stamps in UTC without colons": {
			since: time.Date(2026, time.July, 8, 13, 4, 5, 0, time.UTC),
			page:  2,
			want:  "20260708T130405Z-p0002.json",
		},
		"a non-UTC cursor is normalized to UTC": {
			since: time.Date(2026, time.July, 8, 9, 4, 5, 0, time.FixedZone("EST", -4*60*60)),
			page:  3,
			want:  "20260708T130405Z-p0003.json",
		},
		"high page numbers keep sorting past four digits": {
			since: time.Time{},
			page:  12345,
			want:  "00010101T000000Z-p12345.json",
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
