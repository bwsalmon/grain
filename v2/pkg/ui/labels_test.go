package ui

import "testing"

func TestDeriveStatePrecedence(t *testing.T) {
	l := DefaultLabels()
	set := func(names ...string) map[string]struct{} {
		s := make(map[string]struct{}, len(names))
		for _, n := range names {
			s[n] = struct{}{}
		}
		return s
	}

	cases := []struct {
		name   string
		labels map[string]struct{}
		want   State
	}{
		{"nothing", set(), StateUntracked},
		{"trigger only", set(l.Trigger), StateQueued},
		{"needs approval", set(l.NeedsApproval), StateNeedsApproval},
		{"in progress", set(l.InProgress), StateRunning},
		{"awaiting reply", set(l.AwaitingReply), StateAwaitingReply},
		{"completed", set(l.Completed), StateCompleted},
		// A stale in-progress label left on a completed issue still reads
		// completed -- completed is the label a poll only ever adds last.
		{"completed wins over in-progress", set(l.Completed, l.InProgress), StateCompleted},
		{"awaiting reply wins over in-progress", set(l.AwaitingReply, l.InProgress), StateAwaitingReply},
		{"trigger and a capability label", set(l.Trigger, "grain-gemini-key"), StateQueued},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := deriveState(c.labels, l); got != c.want {
				t.Errorf("deriveState(%v) = %q, want %q", c.labels, got, c.want)
			}
		})
	}
}
