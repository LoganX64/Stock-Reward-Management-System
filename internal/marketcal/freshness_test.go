package marketcal

import (
	"testing"
	"time"
)

func TestFreshnessDuringSession(t *testing.T) {
	policy := DefaultFreshnessPolicy(Default())

	tests := []struct {
		name string
		now  time.Time
		asOf time.Time
		want Freshness
	}{
		{
			name: "inside max stale",
			now:  istTime(2026, time.November, 2, 11, 30),
			asOf: istTime(2026, time.November, 2, 10, 30),
			want: FreshnessFresh,
		},
		{
			name: "older than max stale",
			now:  istTime(2026, time.November, 2, 11, 30),
			asOf: istTime(2026, time.November, 2, 9, 30),
			want: FreshnessStale,
		},
		{
			name: "quote before session open",
			now:  istTime(2026, time.November, 2, 10, 0),
			asOf: istTime(2026, time.November, 2, 9, 14),
			want: FreshnessStale,
		},
		{
			name: "at session open",
			now:  istTime(2026, time.November, 2, 10, 0),
			asOf: istTime(2026, time.November, 2, 9, 15),
			want: FreshnessFresh,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := policy.Classify(tt.asOf, tt.now); got != tt.want {
				t.Errorf("Classify(%s, %s) = %s, want %s", tt.asOf, tt.now, got, tt.want)
			}
		})
	}
}

func TestFreshnessAfterClose(t *testing.T) {
	policy := DefaultFreshnessPolicy(Default())
	now := istTime(2026, time.November, 2, 16, 0)

	tests := []struct {
		name string
		asOf time.Time
		want Freshness
	}{
		{name: "last trade before close", asOf: istTime(2026, time.November, 2, 15, 29), want: FreshnessFresh},
		{name: "inside close grace", asOf: istTime(2026, time.November, 2, 15, 10), want: FreshnessFresh},
		{name: "outside close grace", asOf: istTime(2026, time.November, 2, 15, 9), want: FreshnessStale},
		{name: "long before close", asOf: istTime(2026, time.November, 2, 14, 0), want: FreshnessStale},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := policy.Classify(tt.asOf, now); got != tt.want {
				t.Errorf("Classify(%s, %s) = %s, want %s", tt.asOf, now, got, tt.want)
			}
		})
	}
}

func TestFreshnessWeekendAndPreOpenCarryOver(t *testing.T) {
	policy := DefaultFreshnessPolicy(Default())
	fridayClose := istTime(2026, time.October, 30, 15, 30)
	fridayEarly := istTime(2026, time.October, 30, 14, 0)

	tests := []struct {
		name string
		now  time.Time
		asOf time.Time
		want Freshness
	}{
		{
			name: "Monday before open",
			now:  istTime(2026, time.November, 2, 8, 0),
			asOf: fridayClose,
			want: FreshnessFresh,
		},
		{
			name: "Monday before open with old Friday quote",
			now:  istTime(2026, time.November, 2, 8, 0),
			asOf: fridayEarly,
			want: FreshnessStale,
		},
		{
			name: "Saturday",
			now:  istTime(2026, time.October, 31, 12, 0),
			asOf: fridayClose,
			want: FreshnessFresh,
		},
		{
			name: "Sunday night",
			now:  istTime(2026, time.November, 1, 23, 0),
			asOf: fridayClose,
			want: FreshnessFresh,
		},
		{
			name: "Monday after open with Friday quote",
			now:  istTime(2026, time.November, 2, 10, 0),
			asOf: fridayClose,
			want: FreshnessStale,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := policy.Classify(tt.asOf, tt.now); got != tt.want {
				t.Errorf("Classify(%s, %s) = %s, want %s", tt.asOf, tt.now, got, tt.want)
			}
		})
	}
}

func TestFreshnessHoliday(t *testing.T) {
	policy := DefaultFreshnessPolicy(Default())
	mondayClose := istTime(2026, time.October, 19, 15, 30)

	if got := policy.Classify(mondayClose, istTime(2026, time.October, 20, 10, 0)); got != FreshnessFresh {
		t.Errorf("expected holiday to carry Monday close as fresh, got %s", got)
	}

	if got := policy.Classify(istTime(2026, time.October, 19, 14, 0), istTime(2026, time.October, 20, 10, 0)); got != FreshnessStale {
		t.Errorf("expected old holiday-eve quote to be stale, got %s", got)
	}

	if got := policy.Classify(mondayClose, istTime(2026, time.October, 21, 10, 0)); got != FreshnessStale {
		t.Errorf("expected Monday close to be stale during Wednesday session, got %s", got)
	}
}

func TestFreshnessUnknown(t *testing.T) {
	policy := DefaultFreshnessPolicy(Default())
	now := istTime(2026, time.November, 2, 10, 0)

	if got := policy.Classify(time.Time{}, now); got != FreshnessUnknown {
		t.Errorf("Classify(zero asOf) = %s, want %s", got, FreshnessUnknown)
	}
	if got := policy.Classify(now, time.Time{}); got != FreshnessUnknown {
		t.Errorf("Classify(zero now) = %s, want %s", got, FreshnessUnknown)
	}
	if got := policy.ClassifySource(SourceLegacy, istTime(2026, time.November, 2, 9, 30), now); got != FreshnessUnknown {
		t.Errorf("ClassifySource(legacy) = %s, want %s", got, FreshnessUnknown)
	}
	if got := policy.ClassifySource("", istTime(2026, time.November, 2, 9, 30), now); got != FreshnessUnknown {
		t.Errorf("ClassifySource(empty) = %s, want %s", got, FreshnessUnknown)
	}
}

func TestFreshnessSourceClassification(t *testing.T) {
	policy := DefaultFreshnessPolicy(Default())
	fridayClose := istTime(2026, time.October, 30, 15, 30)
	saturday := istTime(2026, time.October, 31, 12, 0)

	for _, source := range []string{"yahoo", "nse_bhavcopy", "simulator"} {
		if got := policy.ClassifySource(source, fridayClose, saturday); got != FreshnessFresh {
			t.Errorf("ClassifySource(%q) = %s, want %s", source, got, FreshnessFresh)
		}
	}
}

func TestFreshnessFutureSkew(t *testing.T) {
	policy := DefaultFreshnessPolicy(Default())
	now := istTime(2026, time.November, 2, 10, 0)

	if got := policy.Classify(now.Add(4*time.Minute), now); got != FreshnessFresh {
		t.Errorf("quote within future skew = %s, want %s", got, FreshnessFresh)
	}
	if got := policy.Classify(now.Add(6*time.Minute), now); got != FreshnessStale {
		t.Errorf("quote beyond future skew = %s, want %s", got, FreshnessStale)
	}
}

func TestFreshnessConvertsNowToIST(t *testing.T) {
	policy := DefaultFreshnessPolicy(Default())
	nowUTC := time.Date(2026, time.November, 2, 4, 30, 0, 0, time.UTC) // 10:00 IST
	asOfUTC := time.Date(2026, time.November, 2, 4, 0, 0, 0, time.UTC) // 09:30 IST

	if got := policy.Classify(asOfUTC, nowUTC); got != FreshnessFresh {
		t.Errorf("Classify(UTC inputs) = %s, want %s", got, FreshnessFresh)
	}
}

func TestFreshnessCustomMaxStale(t *testing.T) {
	calendar := Default()
	policy := FreshnessPolicy{
		MaxStale:   10 * time.Minute,
		CloseGrace: 20 * time.Minute,
		FutureSkew: 5 * time.Minute,
		Calendar:   calendar,
	}
	now := istTime(2026, time.November, 2, 10, 0)

	if got := policy.Classify(istTime(2026, time.November, 2, 9, 45), now); got != FreshnessStale {
		t.Errorf("15-minute-old quote with 10m max stale = %s, want %s", got, FreshnessStale)
	}
	if got := policy.Classify(istTime(2026, time.November, 2, 9, 55), now); got != FreshnessFresh {
		t.Errorf("5-minute-old quote with 10m max stale = %s, want %s", got, FreshnessFresh)
	}
}
