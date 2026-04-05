package webhook

// MergeEvent applies a partial webhook event to an existing state map.
// Only non-empty fields from the event overwrite existing values. This allows
// partial updates (e.g. a check_run only updates CI, not conflicts or review).
func MergeEvent(existing map[string]*Event, ev Event) {
	if ev.PRNumber == "" {
		return
	}
	cur, ok := existing[ev.PRNumber]
	if !ok {
		cp := ev
		existing[ev.PRNumber] = &cp
		return
	}
	if ev.CI != "" {
		cur.CI = ev.CI
	}
	if ev.State != "" {
		cur.State = ev.State
	}
	if ev.Conflicts != "" {
		cur.Conflicts = ev.Conflicts
	}
	if ev.ReviewDecision != "" {
		cur.ReviewDecision = ev.ReviewDecision
	}
	if ev.Repo != "" {
		cur.Repo = ev.Repo
	}
}
