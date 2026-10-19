package engine

import (
	"fmt"
	"slices"
	"time"
)

// WindowMode selects the stage's allow/deny rule (R23, D-033).
type WindowMode string

// Window modes: a covering deny always wins over an allow.
const (
	WindowAllow WindowMode = "allow"
	WindowDeny  WindowMode = "deny"
)

// Window is a platform schedule. Exactly one schedule is set (§12.2).
type Window struct {
	Name      string
	Mode      WindowMode
	Recurring *Recurring
	Once      *OneOff
}

// Recurring starts on Days, at Start and End wall-clock minutes in Loc.
// Time zones are resolved by the caller, never by the engine (D-033).
type Recurring struct {
	Days       []time.Weekday
	Start, End int
	TimeZone   string
	Loc        *time.Location
}

// OneOff is an absolute half-open interval.
type OneOff struct{ From, To time.Time }

// WindowHorizon bounds the search for the next stage opening (§12.2).
const WindowHorizon = 400 * 24 * time.Hour

// Validate checks the data contract without loading time zones.
func (w Window) Validate() error {
	if w.Mode != WindowAllow && w.Mode != WindowDeny {
		return fmt.Errorf("mode must be allow or deny")
	}
	if (w.Recurring == nil) == (w.Once == nil) {
		return fmt.Errorf("exactly one of recurring and once is required")
	}
	if w.Once != nil {
		if !w.Once.To.After(w.Once.From) {
			return fmt.Errorf("once.to must be after from")
		}
		return nil
	}
	r := w.Recurring
	if r.Loc == nil || r.TimeZone == "" {
		return fmt.Errorf("recurring requires a resolved location and timeZone")
	}
	if r.Start < 0 || r.Start >= 1440 || r.End < 0 || r.End > 1440 || r.Start == r.End {
		return fmt.Errorf("recurring start/end minutes are invalid")
	}
	if len(r.Days) == 0 {
		return fmt.Errorf("recurring.days must not be empty")
	}
	seen := map[time.Weekday]bool{}
	for _, d := range r.Days {
		if d < time.Sunday || d > time.Saturday || seen[d] {
			return fmt.Errorf("recurring.days contains invalid or duplicate day")
		}
		seen[d] = true
	}
	return nil
}

type windowInterval struct{ start, end time.Time }

// wallInstant implements §12.1: earliest occurrence in overlaps, first instant
// after a gap. ZoneBounds lets us examine actual offsets, including half-hour
// transitions, without assuming the offset chosen by time.Date.
func wallInstant(day time.Time, minutes int, loc *time.Location) time.Time {
	wall := day.Add(time.Duration(minutes) * time.Minute)
	var first time.Time
	limit := wall.Add(48 * time.Hour)
	// The scan spans 96 hours. TZif bounds have whole-second precision, so
	// each advancing bound consumes at least one second; fallback consumes a day.
	const maxSteps = int(96*time.Hour/time.Second) + 1
	steps := 0
	for cursor := wall.Add(-48 * time.Hour); cursor.Before(limit); {
		steps++
		if steps > maxSteps {
			panic("window zone scan exceeded its 96-hour bound")
		}
		local := cursor.In(loc)
		_, offset := local.Zone()
		_, end := local.ZoneBounds()
		candidate := wall.Add(-time.Duration(offset) * time.Second)
		// Validate the actual offset, not ZoneBounds: Go's rule-based bounds
		// can end at or before cursor on December 31 of a leap year.
		_, candidateOffset := candidate.In(loc).Zone()
		if candidateOffset == offset {
			if first.IsZero() || candidate.Before(first) {
				first = candidate
			}
		}
		if end.IsZero() || !end.Before(limit) {
			break
		}
		if !end.After(cursor) {
			cursor = cursor.Add(24 * time.Hour)
			continue
		}
		_, nextOffset := end.In(loc).Zone()
		if nextOffset > offset {
			before := end.Add(time.Duration(offset) * time.Second)
			after := end.Add(time.Duration(nextOffset) * time.Second)
			if !wall.Before(before) && wall.Before(after) {
				return end
			}
		}
		cursor = end
	}
	return first
}

func (w Window) intervals(t time.Time, daysAhead int) []windowInterval {
	if w.Once != nil {
		return []windowInterval{{w.Once.From, w.Once.To}}
	}
	if w.Recurring == nil || w.Recurring.Loc == nil {
		return nil
	}
	r := w.Recurring
	y, m, d := t.In(r.Loc).Date()
	day := time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
	var out []windowInterval
	for i := -1; i <= daysAhead; i++ {
		date := day.AddDate(0, 0, i)
		if !slices.Contains(r.Days, date.Weekday()) {
			continue
		}
		end := r.End
		if end <= r.Start {
			end += 1440
		}
		a, b := wallInstant(date, r.Start, r.Loc), wallInstant(date, end, r.Loc)
		if b.After(a) {
			out = append(out, windowInterval{a, b})
		}
	}
	return out
}

// Covers reports half-open interval membership (§12.1).
func (w Window) Covers(t time.Time) bool {
	for _, i := range w.intervals(t, 0) {
		if !t.Before(i.start) && t.Before(i.end) {
			return true
		}
	}
	return false
}

// Next returns the earliest strict future change in coverage, skipping adjacent
// or overlapping intervals whose shared edges do not change membership.
func (w Window) Next(t time.Time) (time.Time, bool) {
	// §12.1: a weekly occurrence may be empty inside a DST gap. Include
	// two following weeks (plus a boundary day) to reach the next occurrence.
	intervals := w.intervals(t, 15)
	slices.SortFunc(intervals, func(a, b windowInterval) int { return a.start.Compare(b.start) })
	// Merge adjacent intervals so their shared edges do not change coverage.
	var merged []windowInterval
	for _, interval := range intervals {
		if len(merged) == 0 || interval.start.After(merged[len(merged)-1].end) {
			merged = append(merged, interval)
		} else if interval.end.After(merged[len(merged)-1].end) {
			merged[len(merged)-1].end = interval.end
		}
	}
	for _, interval := range merged {
		if interval.start.After(t) {
			return interval.start, true
		}
		if interval.end.After(t) && !w.Covers(interval.end) {
			return interval.end, true
		}
	}
	return time.Time{}, false
}

// WindowsOpen applies the stage rule: deny beats the union of allows (R23).
func WindowsOpen(ws []Window, t time.Time) bool { return len(WindowClosers(ws, t)) == 0 }

// WindowClosers returns covering denies and, when no allow covers, every allow.
func WindowClosers(ws []Window, t time.Time) []Window {
	var deny, allow []Window
	allowed := false
	for _, w := range ws {
		covers := w.Covers(t)
		if w.Mode == WindowDeny && covers {
			deny = append(deny, w)
		}
		if w.Mode == WindowAllow {
			allow = append(allow, w)
			allowed = allowed || covers
		}
	}
	if !allowed {
		deny = append(deny, allow...)
	}
	return deny
}

// WindowsNextChange returns the next individual window edge (§3.8).
func WindowsNextChange(ws []Window, t time.Time) (time.Time, bool) {
	var next time.Time
	found := false
	for _, w := range ws {
		if u, ok := w.Next(t); ok && (!found || u.Before(next)) {
			next, found = u, true
		}
	}
	return next, found
}

// WindowsNextOpen walks edges up to WindowHorizon, including t when already open.
func WindowsNextOpen(ws []Window, t time.Time) (time.Time, bool) {
	if WindowsOpen(ws, t) {
		return t, true
	}
	// Cache each window's edge and coverage. Only windows reaching an edge
	// need their next edge computed again (§12.2).
	type cursor struct {
		window     Window
		next       time.Time
		ok, covers bool
	}
	limit := t.Add(WindowHorizon)
	cursors := make([]cursor, len(ws))
	allows, coveringAllows, coveringDenies := 0, 0, 0
	for i, w := range ws {
		cursors[i].window = w
		cursors[i].covers = w.Covers(t)
		cursors[i].next, cursors[i].ok = w.Next(t)
		// A covering deny with no edge inside the horizon prevents every opening.
		if w.Mode == WindowDeny && cursors[i].covers && (!cursors[i].ok || cursors[i].next.After(limit)) {
			return time.Time{}, false
		}
		if w.Mode == WindowAllow {
			allows++
			if cursors[i].covers {
				coveringAllows++
			}
		} else if cursors[i].covers {
			coveringDenies++
		}
	}
	// Each recurring interval has at most two edges per local date. The
	// horizon plus four dates includes both offset margins and overnight ends;
	// one-off windows have only two edges. Slice loops elsewhere are finite.
	maxSteps := 2 * (int(WindowHorizon/(24*time.Hour)) + 4) * len(ws)
	previous := t
	for step := 0; ; step++ {
		if step > maxSteps {
			panic("window opening search exceeded its edge bound")
		}
		var next time.Time
		found := false
		for _, c := range cursors {
			if c.ok && (!found || c.next.Before(next)) {
				next, found = c.next, true
			}
		}
		if !found || next.After(limit) {
			return time.Time{}, false
		}
		if !next.After(previous) {
			panic("window opening search did not advance")
		}
		previous = next
		// Apply every simultaneous edge before deciding whether the stage opens.
		for i := range cursors {
			c := &cursors[i]
			if !c.ok || !c.next.Equal(next) {
				continue
			}
			c.covers = !c.covers
			delta := -1
			if c.covers {
				delta = 1
			}
			if c.window.Mode == WindowAllow {
				coveringAllows += delta
			} else {
				coveringDenies += delta
			}
		}
		if coveringDenies == 0 && (allows == 0 || coveringAllows > 0) {
			return next, true
		}
		for i := range cursors {
			c := &cursors[i]
			if c.ok && c.next.Equal(next) {
				c.next, c.ok = c.window.Next(next)
			}
		}
	}
}
