package runtime

import (
	"testing"
	"time"
)

func mustParse(t *testing.T, spec string) CronSchedule {
	t.Helper()
	c, err := ParseCronSpec(spec)
	if err != nil {
		t.Fatalf("ParseCronSpec(%q): %v", spec, err)
	}
	return c
}

func TestCronNextBasicFields(t *testing.T) {
	t.Parallel()
	// Reference: Wednesday 2026-08-12 10:17:00 UTC.
	ref := time.Date(2026, 8, 12, 10, 17, 0, 0, time.UTC)
	cases := []struct {
		spec string
		want time.Time
	}{
		// Every minute: the next minute boundary.
		{"* * * * *", time.Date(2026, 8, 12, 10, 18, 0, 0, time.UTC)},
		// Top of the next hour.
		{"0 * * * *", time.Date(2026, 8, 12, 11, 0, 0, 0, time.UTC)},
		// 09:30 daily has already passed today, so tomorrow.
		{"30 9 * * *", time.Date(2026, 8, 13, 9, 30, 0, 0, time.UTC)},
		// Specific minute later this hour.
		{"45 10 * * *", time.Date(2026, 8, 12, 10, 45, 0, 0, time.UTC)},
		// First of next month at midnight.
		{"0 0 1 * *", time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)},
	}
	for _, tc := range cases {
		got, ok := mustParse(t, tc.spec).Next(ref)
		if !ok {
			t.Fatalf("%q: Next returned ok=false", tc.spec)
		}
		if !got.Equal(tc.want) {
			t.Fatalf("%q: Next = %s, want %s", tc.spec, got, tc.want)
		}
	}
}

func TestCronNextListsRangesSteps(t *testing.T) {
	t.Parallel()
	ref := time.Date(2026, 8, 12, 10, 17, 0, 0, time.UTC)
	cases := []struct {
		spec string
		want time.Time
	}{
		// Step over minutes: next multiple of 15 after :17 is :30.
		{"*/15 * * * *", time.Date(2026, 8, 12, 10, 30, 0, 0, time.UTC)},
		// List of minutes.
		{"0,20,40 * * * *", time.Date(2026, 8, 12, 10, 20, 0, 0, time.UTC)},
		// Range of hours: business hours; :17 is inside hour 10, next minute 0
		// candidate is 11:00.
		{"0 9-17 * * *", time.Date(2026, 8, 12, 11, 0, 0, 0, time.UTC)},
		// Stepped range: minute 0 at hours {0,6,12}; next after 10:17 is 12:00.
		{"0 0-12/6 * * *", time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)},
	}
	for _, tc := range cases {
		got, ok := mustParse(t, tc.spec).Next(ref)
		if !ok {
			t.Fatalf("%q: Next returned ok=false", tc.spec)
		}
		if !got.Equal(tc.want) {
			t.Fatalf("%q: Next = %s, want %s", tc.spec, got, tc.want)
		}
	}
}

func TestCronNextDayOfWeek(t *testing.T) {
	t.Parallel()
	// Wednesday 2026-08-12. Monday is 2026-08-17.
	ref := time.Date(2026, 8, 12, 10, 17, 0, 0, time.UTC)
	got, ok := mustParse(t, "0 0 * * 1").Next(ref) // Mondays at midnight
	if !ok {
		t.Fatal("Next returned ok=false")
	}
	want := time.Date(2026, 8, 17, 0, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("Next = %s, want %s", got, want)
	}
	if got.Weekday() != time.Monday {
		t.Fatalf("Next weekday = %s, want Monday", got.Weekday())
	}
}

func TestCronNextEvery(t *testing.T) {
	t.Parallel()
	ref := time.Date(2026, 8, 12, 10, 17, 30, 0, time.UTC)
	got, ok := mustParse(t, "@every 30m").Next(ref)
	if !ok {
		t.Fatal("Next returned ok=false")
	}
	want := ref.Add(30 * time.Minute)
	if !got.Equal(want) {
		t.Fatalf("Next = %s, want %s", got, want)
	}
}

func TestCronNextMonthRollover(t *testing.T) {
	t.Parallel()
	// Reference late in the month; "0 0 1 * *" rolls to next month.
	ref := time.Date(2026, 12, 31, 23, 59, 0, 0, time.UTC)
	got, ok := mustParse(t, "0 0 1 * *").Next(ref)
	if !ok {
		t.Fatal("Next returned ok=false")
	}
	want := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("Next = %s, want %s", got, want)
	}
}

func TestCronNextUnsatisfiable(t *testing.T) {
	t.Parallel()
	// February 31st never occurs.
	ref := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if _, ok := mustParse(t, "0 0 31 2 *").Next(ref); ok {
		t.Fatal("Next returned ok=true for an unsatisfiable spec")
	}
}

func TestCronNextNotReturnedTwice(t *testing.T) {
	t.Parallel()
	// A reference exactly on a fire boundary must yield the following fire, not
	// the same instant.
	ref := time.Date(2026, 8, 12, 10, 0, 0, 0, time.UTC)
	got, ok := mustParse(t, "0 * * * *").Next(ref)
	if !ok {
		t.Fatal("Next returned ok=false")
	}
	want := time.Date(2026, 8, 12, 11, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("Next = %s, want %s", got, want)
	}
}

func TestParseCronSpecErrors(t *testing.T) {
	t.Parallel()
	bad := []string{
		"",
		"* * * *",       // 4 fields
		"* * * * * *",   // 6 fields
		"60 * * * *",    // minute out of range
		"* 24 * * *",    // hour out of range
		"* * 0 * *",     // dom below range
		"* * * 13 *",    // month out of range
		"* * * * 7",     // dow above range
		"*/0 * * * *",   // zero step
		"5-1 * * * *",   // reversed range
		"@every",        // missing duration
		"@every 0s",     // non-positive interval
		"@every banana", // invalid duration
	}
	for _, spec := range bad {
		if _, err := ParseCronSpec(spec); err == nil {
			t.Fatalf("ParseCronSpec(%q) = nil error, want error", spec)
		}
	}
}
