package marketcal

import (
	"testing"
	"time"
)

func istTime(year int, month time.Month, day, hour, minute int) time.Time {
	return time.Date(year, month, day, hour, minute, 0, 0, Location())
}

func TestCalendarTradingDays(t *testing.T) {
	calendar := Default()

	tests := []struct {
		name string
		when time.Time
		want bool
	}{
		{name: "regular Monday", when: istTime(2026, time.November, 2, 10, 0), want: true},
		{name: "regular Friday", when: istTime(2026, time.October, 30, 10, 0), want: true},
		{name: "Saturday", when: istTime(2026, time.October, 31, 10, 0), want: false},
		{name: "Sunday", when: istTime(2026, time.November, 1, 10, 0), want: false},
		{name: "Republic Day", when: istTime(2026, time.January, 26, 10, 0), want: false},
		{name: "Holi", when: istTime(2026, time.March, 3, 10, 0), want: false},
		{name: "Good Friday", when: istTime(2026, time.April, 3, 10, 0), want: false},
		{name: "Gandhi Jayanti", when: istTime(2026, time.October, 2, 10, 0), want: false},
		{name: "Diwali", when: istTime(2026, time.November, 10, 10, 0), want: false},
		{name: "Christmas", when: istTime(2026, time.December, 25, 10, 0), want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := calendar.IsTradingDay(tt.when); got != tt.want {
				t.Errorf("IsTradingDay(%s) = %v, want %v", tt.when.Format(time.RFC3339), got, tt.want)
			}
		})
	}
}

func TestCalendarSessionBoundaries(t *testing.T) {
	calendar := Default()
	day := time.Date(2026, time.November, 2, 0, 0, 0, 0, Location())

	tests := []struct {
		name string
		when time.Time
		want bool
	}{
		{name: "before open", when: day.Add(9*time.Hour + 14*time.Minute + 59*time.Second), want: false},
		{name: "at open", when: day.Add(9*time.Hour + 15*time.Minute), want: true},
		{name: "late session", when: day.Add(15*time.Hour + 29*time.Minute + 59*time.Second), want: true},
		{name: "at close", when: day.Add(15*time.Hour + 30*time.Minute), want: false},
		{name: "after close", when: day.Add(16 * time.Hour), want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := calendar.IsOpen(tt.when); got != tt.want {
				t.Errorf("IsOpen(%s) = %v, want %v", tt.when.Format(time.RFC3339), got, tt.want)
			}
		})
	}
}

func TestCalendarSessionHelpers(t *testing.T) {
	calendar := Default()
	monday := istTime(2026, time.November, 2, 10, 0)

	open := calendar.SessionOpen(monday)
	wantOpen := istTime(2026, time.November, 2, 9, 15)
	if !open.Equal(wantOpen) {
		t.Errorf("SessionOpen() = %s, want %s", open.Format(time.RFC3339), wantOpen.Format(time.RFC3339))
	}

	close := calendar.SessionClose(monday)
	wantClose := istTime(2026, time.November, 2, 15, 30)
	if !close.Equal(wantClose) {
		t.Errorf("SessionClose() = %s, want %s", close.Format(time.RFC3339), wantClose.Format(time.RFC3339))
	}

	fridayClose := istTime(2026, time.October, 30, 15, 30)
	previous := calendar.PreviousSessionClose(monday)
	if !previous.Equal(fridayClose) {
		t.Errorf("PreviousSessionClose() = %s, want %s", previous.Format(time.RFC3339), fridayClose.Format(time.RFC3339))
	}

	lastCompleted := calendar.LastCompletedSessionClose(monday)
	if !lastCompleted.Equal(fridayClose) {
		t.Errorf("LastCompletedSessionClose(during session) = %s, want %s", lastCompleted.Format(time.RFC3339), fridayClose.Format(time.RFC3339))
	}

	afterClose := istTime(2026, time.November, 2, 16, 0)
	lastCompleted = calendar.LastCompletedSessionClose(afterClose)
	if !lastCompleted.Equal(wantClose) {
		t.Errorf("LastCompletedSessionClose(after close) = %s, want %s", lastCompleted.Format(time.RFC3339), wantClose.Format(time.RFC3339))
	}

	atClose := istTime(2026, time.November, 2, 15, 30)
	lastCompleted = calendar.LastCompletedSessionClose(atClose)
	if !lastCompleted.Equal(wantClose) {
		t.Errorf("LastCompletedSessionClose(at close) = %s, want %s", lastCompleted.Format(time.RFC3339), wantClose.Format(time.RFC3339))
	}
}

func TestCalendarWeekendPreviousSession(t *testing.T) {
	calendar := Default()
	saturday := istTime(2026, time.October, 31, 12, 0)
	fridayClose := istTime(2026, time.October, 30, 15, 30)

	if got := calendar.PreviousSessionClose(saturday); !got.Equal(fridayClose) {
		t.Errorf("PreviousSessionClose(Saturday) = %s, want %s", got.Format(time.RFC3339), fridayClose.Format(time.RFC3339))
	}
	if got := calendar.LastCompletedSessionClose(saturday); !got.Equal(fridayClose) {
		t.Errorf("LastCompletedSessionClose(Saturday) = %s, want %s", got.Format(time.RFC3339), fridayClose.Format(time.RFC3339))
	}
}

func TestCalendarHolidayPreviousSession(t *testing.T) {
	calendar := Default()
	holiday := istTime(2026, time.October, 20, 10, 0)
	mondayClose := istTime(2026, time.October, 19, 15, 30)

	if calendar.IsTradingDay(holiday) {
		t.Fatal("expected Diwali to be a holiday")
	}
	if got := calendar.PreviousSessionClose(holiday); !got.Equal(mondayClose) {
		t.Errorf("PreviousSessionClose(holiday) = %s, want %s", got.Format(time.RFC3339), mondayClose.Format(time.RFC3339))
	}
	if got := calendar.LastCompletedSessionClose(holiday); !got.Equal(mondayClose) {
		t.Errorf("LastCompletedSessionClose(holiday) = %s, want %s", got.Format(time.RFC3339), mondayClose.Format(time.RFC3339))
	}
}

func TestCalendarOverrides(t *testing.T) {
	calendar, err := New([]string{"2026-11-02", "2026-11-02"})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	if calendar.IsTradingDay(istTime(2026, time.November, 2, 10, 0)) {
		t.Fatal("expected override to close Monday 2026-11-02")
	}

	// Monday is closed, so Tuesday's previous session is Friday 30 October.
	tuesday := istTime(2026, time.November, 3, 10, 0)
	want := istTime(2026, time.October, 30, 15, 30)
	if got := calendar.PreviousSessionClose(tuesday); !got.Equal(want) {
		t.Errorf("PreviousSessionClose(Tuesday) = %s, want %s", got.Format(time.RFC3339), want.Format(time.RFC3339))
	}
}

func TestCalendarInvalidHoliday(t *testing.T) {
	if _, err := New([]string{"2026-13-01"}); err == nil {
		t.Fatal("New() accepted an invalid holiday")
	}
}

func TestCalendarUsesISTForUTCInput(t *testing.T) {
	calendar := Default()

	// 20:00 UTC on Sunday 1 November is 01:30 IST on Monday 2 November.
	mondayEarly := time.Date(2026, time.November, 1, 20, 0, 0, 0, time.UTC)
	if !calendar.IsTradingDay(mondayEarly) {
		t.Error("expected 01:30 IST Monday to be a trading day")
	}

	// 18:00 UTC on Sunday is 23:30 IST on Sunday.
	sundayLate := time.Date(2026, time.November, 1, 18, 0, 0, 0, time.UTC)
	if calendar.IsTradingDay(sundayLate) {
		t.Error("expected 23:30 IST Sunday not to be a trading day")
	}
}
