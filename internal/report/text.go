package report

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"
	"time"
)

// WriteText writes the report as plain tables: a row per repo and a total,
// then runs by dispatch reason, checks by kind, and the changes that had
// follow-up runs.
func (r *Report) WriteText(w io.Writer) error {
	since := "all time"
	if r.Since != "" {
		since = "since " + r.Since
	}
	fmt.Fprintf(w, "Agent time %s, from %d session event log(s)\n\n", since, r.Sessions)
	if r.Overall.Runs == 0 && r.Overall.Consults == 0 && r.Overall.Reviews == 0 {
		fmt.Fprintln(w, "No runs, consults or reviews.")
		return nil
	}

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "REPO\tRUNS\tCHANGES\tFOLLOW-UPS\tSETUP\tAGENT\tFINALIZE\tCHECKS\tWAIT\tAPPROVAL\tCONSULT\tREVIEW\tCOST")
	row := func(name string, t Totals) {
		fmt.Fprintf(tw, "%s\t%d\t%d\t%d\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", name, t.Runs, t.Changes, t.FollowUps,
			dur(t.SetupMS), dur(t.AgentMS), dur(t.FinalizeMS), dur(checkMS(t)), dur(t.WaitMS),
			dur(t.ApprovalWaitMS), dur(t.ConsultMS), dur(t.ReviewMS), cost(t))
	}
	for _, t := range r.Repos {
		name := t.Repo
		if name == "" {
			name = "(unknown repo)"
		}
		row(name, t)
	}
	row("TOTAL", r.Overall)
	if err := tw.Flush(); err != nil {
		return err
	}

	if len(r.Overall.Reasons) > 0 {
		fmt.Fprintln(w)
		tw = tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "DISPATCH REASON\tRUNS\tFOLLOW-UPS")
		for _, k := range sortedKeys(r.Overall.Reasons) {
			fmt.Fprintf(tw, "%s\t%d\t%d\n", k, r.Overall.Reasons[k], r.Overall.FollowUpReasons[k])
		}
		if err := tw.Flush(); err != nil {
			return err
		}
	}

	if len(r.Overall.Checks) > 0 {
		fmt.Fprintln(w)
		tw = tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "CHECK\tCOUNT\tTIME\tFAILED")
		kinds := make([]string, 0, len(r.Overall.Checks))
		for k := range r.Overall.Checks {
			kinds = append(kinds, k)
		}
		sort.Strings(kinds)
		for _, k := range kinds {
			c := r.Overall.Checks[k]
			fmt.Fprintf(tw, "%s\t%d\t%s\t%d\n", k, c.Count, dur(c.MS), c.Failed)
		}
		if err := tw.Flush(); err != nil {
			return err
		}
	}

	var withFollowUps []Change
	for _, c := range r.Changes {
		if len(c.FollowUps) > 0 {
			withFollowUps = append(withFollowUps, c)
		}
	}
	if len(withFollowUps) > 0 {
		fmt.Fprintln(w)
		tw = tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "CHANGE\tRUNS\tAGENT\tWAIT\tFOLLOW-UP REASONS")
		for _, c := range withFollowUps {
			reasons := make([]string, len(c.FollowUps))
			for i, f := range c.FollowUps {
				reasons[i] = f.Reason
			}
			fmt.Fprintf(tw, "%s\t%d\t%s\t%s\t%s\n", c.Key, c.Runs, dur(c.AgentMS), dur(c.WaitMS), strings.Join(reasons, ", "))
		}
		if err := tw.Flush(); err != nil {
			return err
		}
	}

	fmt.Fprintln(w)
	fmt.Fprintln(w, "Checks run within the agent phase. WAIT is time a change had no run going,")
	fmt.Fprintln(w, "between its runs; APPROVAL is from a PR's last run to its first approval.")
	o := r.Overall
	fmt.Fprintf(w, "Cost is known for %d of %d runs.", o.CostRuns, o.Runs)
	if o.Untimed > 0 {
		fmt.Fprintf(w, " %d run(s) predate phase events: agent time is agent:started to their last event.", o.Untimed)
	}
	if o.Unfinished > 0 {
		fmt.Fprintf(w, " %d run(s) have not finished.", o.Unfinished)
	}
	if o.UnknownWaits > 0 {
		fmt.Fprintf(w, " %d follow-up(s) started after a run with no end on their change: their wait is unknown.", o.UnknownWaits)
	}
	fmt.Fprintln(w)
	return nil
}

func checkMS(t Totals) int64 {
	var ms int64
	for _, c := range t.Checks {
		ms += c.MS
	}
	return ms
}

func cost(t Totals) string {
	if t.CostRuns == 0 {
		return "-"
	}
	return fmt.Sprintf("$%.2f", t.CostUSD)
}

func sortedKeys(m map[string]int) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if m[keys[i]] != m[keys[j]] {
			return m[keys[i]] > m[keys[j]]
		}
		return keys[i] < keys[j]
	})
	return keys
}

// dur renders milliseconds compactly: 45s, 12m05s, 3h12m, 2d04h; "-" for none.
func dur(ms int64) string {
	if ms <= 0 {
		return "-"
	}
	d := time.Duration(ms) * time.Millisecond
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Round(time.Second).Seconds()))
	case d < time.Hour:
		d = d.Round(time.Second)
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	case d < 24*time.Hour:
		d = d.Round(time.Minute)
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
	d = d.Round(time.Hour)
	return fmt.Sprintf("%dd%02dh", int(d.Hours())/24, int(d.Hours())%24)
}

// ParseSince reads a --since duration: a Go duration (36h, 90m), or a whole
// number of days or weeks (7d, 2w). "0" means all time.
func ParseSince(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "0" {
		return 0, nil
	}
	for unit, d := range map[string]time.Duration{"d": 24 * time.Hour, "w": 7 * 24 * time.Hour} {
		if n, ok := strings.CutSuffix(s, unit); ok {
			var v int
			if _, err := fmt.Sscanf(n, "%d", &v); err == nil && v >= 0 && fmt.Sprint(v) == n {
				return time.Duration(v) * d, nil
			}
		}
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("--since %q: use a duration such as 7d, 2w, 36h or 90m, or 0 for all time", s)
	}
	return d, nil
}
