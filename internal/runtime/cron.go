package runtime

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// CronSchedule is a compiled recurring schedule. It is produced by
// ParseCronSpec and reports the next fire time after a given instant. All
// computation is in UTC; callers that need a different zone must convert the
// results themselves. The zero value is not usable; construct one with
// ParseCronSpec.
//
// Two spec forms are supported:
//
//   - A standard five-field cron expression "minute hour dom month dow", where
//     each field accepts "*", a single value, a comma list ("1,15"), an
//     inclusive range ("9-17"), or a step over "*" or a range ("*/5",
//     "0-30/10"). Day-of-week is 0-6 with Sunday as 0. When both day-of-month
//     and day-of-week are restricted (neither is "*"), a day matches if it
//     satisfies either field, following the common cron convention.
//   - A fixed interval "@every <duration>", where <duration> is any positive
//     value accepted by time.ParseDuration (for example "@every 30m"). The next
//     fire is computed relative to the reference passed to Next, not to a wall
//     clock boundary.
type CronSchedule struct {
	spec    string
	every   time.Duration // non-zero iff this is an "@every" schedule
	minute  uint64        // bitmask over 0-59
	hour    uint64        // bitmask over 0-23
	dom     uint64        // bitmask over 1-31
	month   uint64        // bitmask over 1-12
	dow     uint64        // bitmask over 0-6 (Sunday = 0)
	domStar bool          // true when the day-of-month field was "*"
	dowStar bool          // true when the day-of-week field was "*"
}

// cronSearchLimit bounds the forward scan Next performs before giving up on an
// unsatisfiable expression (for example "0 0 31 2 *", February 31st). Four
// years covers a full leap cycle, so any satisfiable calendar expression fires
// within it.
const cronSearchLimit = 4 * 366 * 24 * time.Hour

// Spec returns the original schedule expression the schedule was parsed from.
func (c CronSchedule) Spec() string { return c.spec }

// ParseCronSpec compiles a schedule expression into a CronSchedule. It returns
// an error for a malformed expression, an out-of-range field, or a
// non-positive "@every" duration.
func ParseCronSpec(spec string) (CronSchedule, error) {
	trimmed := strings.TrimSpace(spec)
	if trimmed == "" {
		return CronSchedule{}, fmt.Errorf("lebro: cron spec is empty")
	}
	if rest, ok := strings.CutPrefix(trimmed, "@every "); ok {
		d, err := time.ParseDuration(strings.TrimSpace(rest))
		if err != nil {
			return CronSchedule{}, fmt.Errorf("lebro: cron spec %q: invalid @every duration: %w", spec, err)
		}
		if d <= 0 {
			return CronSchedule{}, fmt.Errorf("lebro: cron spec %q: @every duration must be positive", spec)
		}
		return CronSchedule{spec: trimmed, every: d}, nil
	}

	fields := strings.Fields(trimmed)
	if len(fields) != 5 {
		return CronSchedule{}, fmt.Errorf("lebro: cron spec %q: expected 5 fields, got %d", spec, len(fields))
	}
	c := CronSchedule{spec: trimmed}
	var err error
	if c.minute, _, err = parseCronField(fields[0], 0, 59); err != nil {
		return CronSchedule{}, fmt.Errorf("lebro: cron spec %q minute: %w", spec, err)
	}
	if c.hour, _, err = parseCronField(fields[1], 0, 23); err != nil {
		return CronSchedule{}, fmt.Errorf("lebro: cron spec %q hour: %w", spec, err)
	}
	if c.dom, c.domStar, err = parseCronField(fields[2], 1, 31); err != nil {
		return CronSchedule{}, fmt.Errorf("lebro: cron spec %q day-of-month: %w", spec, err)
	}
	if c.month, _, err = parseCronField(fields[3], 1, 12); err != nil {
		return CronSchedule{}, fmt.Errorf("lebro: cron spec %q month: %w", spec, err)
	}
	if c.dow, c.dowStar, err = parseCronField(fields[4], 0, 6); err != nil {
		return CronSchedule{}, fmt.Errorf("lebro: cron spec %q day-of-week: %w", spec, err)
	}
	return c, nil
}

// parseCronField parses one cron field into a bitmask over [min, max]. It
// reports whether the field was the "*" wildcard so day matching can apply the
// dom/dow either-or convention. It rejects values outside the range and
// non-positive steps.
func parseCronField(field string, min, max int) (mask uint64, star bool, err error) {
	if field == "*" {
		return rangeMask(min, max), true, nil
	}
	for _, part := range strings.Split(field, ",") {
		spec, stepStr, hasStep := strings.Cut(part, "/")
		lo, hi := min, max
		if spec != "*" {
			startStr, endStr, isRange := strings.Cut(spec, "-")
			lo, err = cronAtoi(startStr, min, max)
			if err != nil {
				return 0, false, err
			}
			if isRange {
				hi, err = cronAtoi(endStr, min, max)
				if err != nil {
					return 0, false, err
				}
			} else {
				hi = lo
			}
		}
		if lo > hi {
			return 0, false, fmt.Errorf("range start %d is after end %d", lo, hi)
		}
		step := 1
		if hasStep {
			step, err = strconv.Atoi(stepStr)
			if err != nil {
				return 0, false, fmt.Errorf("invalid step %q", stepStr)
			}
			if step <= 0 {
				return 0, false, fmt.Errorf("step must be positive, got %d", step)
			}
		}
		// Stop before incrementing when the remaining range is smaller than the
		// step so a very large step cannot overflow v and loop unboundedly.
		for v := lo; v <= hi; {
			mask |= 1 << uint(v)
			if step > hi-v {
				break
			}
			v += step
		}
	}
	return mask, false, nil
}

func cronAtoi(s string, min, max int) (int, error) {
	v, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("invalid value %q", s)
	}
	if v < min || v > max {
		return 0, fmt.Errorf("value %d out of range [%d, %d]", v, min, max)
	}
	return v, nil
}

func rangeMask(min, max int) uint64 {
	var mask uint64
	for v := min; v <= max; v++ {
		mask |= 1 << uint(v)
	}
	return mask
}

// Next returns the earliest fire time strictly after the reference instant and
// true, or the zero time and false when the schedule can never fire again
// within cronSearchLimit (an unsatisfiable calendar expression). The reference
// is interpreted in UTC.
//
// For an "@every" schedule the result is after.Add(interval). For a cron
// schedule the search advances a minute at a time from the start of the next
// minute, so a match at the same minute as after is not returned twice.
func (c CronSchedule) Next(after time.Time) (time.Time, bool) {
	if c.every > 0 {
		return after.Add(c.every), true
	}
	// Start at the top of the next minute in UTC; cron precision is one minute
	// and a fire at the reference minute has already happened.
	t := after.UTC().Truncate(time.Minute).Add(time.Minute)
	limit := t.Add(cronSearchLimit)
	for t.Before(limit) {
		if c.month&(1<<uint(t.Month())) == 0 {
			// Skip to the first day of the next month rather than scanning
			// every minute of a non-matching month.
			t = time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, 1, 0)
			continue
		}
		if !c.dayMatches(t) {
			t = time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, 1)
			continue
		}
		if c.hour&(1<<uint(t.Hour())) == 0 {
			t = time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), 0, 0, 0, time.UTC).Add(time.Hour)
			continue
		}
		if c.minute&(1<<uint(t.Minute())) == 0 {
			t = t.Add(time.Minute)
			continue
		}
		return t, true
	}
	return time.Time{}, false
}

// dayMatches applies the standard cron day convention: when both day-of-month
// and day-of-week are restricted, a day matches if it satisfies either field;
// otherwise it must satisfy the restricted field (or matches unconditionally
// when both are "*").
func (c CronSchedule) dayMatches(t time.Time) bool {
	domHit := c.dom&(1<<uint(t.Day())) != 0
	dowHit := c.dow&(1<<uint(t.Weekday())) != 0
	switch {
	case c.domStar && c.dowStar:
		return true
	case c.domStar:
		return dowHit
	case c.dowStar:
		return domHit
	default:
		return domHit || dowHit
	}
}
