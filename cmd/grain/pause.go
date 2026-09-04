// pause.go is "grain pause": the agent usage-limit gate
// (orchestrator.Pause, served as GET /api/pause) read at a terminal, and
// -lift is DELETE /api/pause -- the same thing the UI banner's "Resume
// now" button sends.
//
// grain/task-132 wired that gate to the browser and nothing else, so an
// operator ssh'd into a deployment, or driving one with `grain -server`,
// had two ways to find out why a queue of ready tasks was dispatching
// nothing: the daemon's journal, or the detail of an attempt they would
// have to know to open. `grain metrics` deliberately does not carry it
// (pkg/ui/pause.go's own doc comment on why a live gauge does not belong
// in a report computed over rows), which leaves this.
//
// It is spelled as a noun with a flag, like `grain settings`, rather
// than as a `grain resume` verb: every verb in this CLI acts on a task
// named by its argument (approve, retry, reopen), so a bare `grain
// resume` would read as one of those with its id left off. `grain pause`
// prints the reading; `grain pause -lift` changes it and prints what it
// left behind.
package main

import (
	"context"
	"flag"
	"fmt"
	"time"

	"github.com/bwsalmon/grain/pkg/ui"
)

func cmdPause(ctx context.Context, c *ui.HTTPClient, out *printer, args []string) error {
	fs := flag.NewFlagSet("grain pause", flag.ContinueOnError)
	lift := fs.Bool("lift", false, "lift the current pause instead of printing it: dispatch resumes on the next reconcile tick")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *lift {
		status, lifted, err := c.LiftAgentPause(ctx)
		if err != nil {
			return err
		}
		out.agentPause(status, true, &lifted)
		return nil
	}
	status, enabled, err := c.AgentPause(ctx)
	if err != nil {
		return err
	}
	out.agentPause(status, enabled, nil)
	return nil
}

// agentPauseView is what `grain pause -json` prints: the route's own
// body rather than a shape invented here, so a reading piped into
// something else and one fetched with `curl .../api/pause` are the same
// object. (pkg/ui's agentPauseResponse is unexported -- it is the
// server's private wire type -- so this restates it.)
type agentPauseView struct {
	Enabled bool                 `json:"enabled"`
	Pause   *ui.AgentPauseStatus `json:"pause,omitempty"`
	Lifted  *bool                `json:"lifted,omitempty"`
}

// agentPause renders one reading of the gate. lifted is nil for a plain
// read and non-nil for a -lift, where whether there was anything to lift
// is the answer the operator asked for.
//
// A deployment whose UI was handed no gate (enabled false) says so
// rather than printing "not paused": this CLI is pointed at whatever
// -server names, and "nothing is paused here" from a daemon that could
// not tell either way is the one wrong answer -- it is exactly what an
// operator would act on to conclude the limit is not the problem.
func (p *printer) agentPause(status ui.AgentPauseStatus, enabled bool, lifted *bool) {
	if p.json {
		view := agentPauseView{Enabled: enabled, Lifted: lifted}
		if enabled {
			view.Pause = &status
		}
		p.encode(view)
		return
	}
	if !enabled {
		fmt.Println("no agent pause reported: this deployment's UI is not wired to a reconcile loop" +
			"\nthat has one, so nothing here says whether a usage limit has stopped dispatch")
		return
	}
	if lifted != nil {
		if *lifted {
			fmt.Println("pause lifted: dispatch resumes on the next reconcile tick")
		} else {
			fmt.Println("nothing to lift: dispatch was not paused")
		}
	}
	if !status.Paused {
		if lifted == nil {
			fmt.Println("dispatch is running: no agent usage limit is in force")
		}
		return
	}
	// The provider's own sentence last and verbatim: it names the
	// framework and the window, which is the half of this an operator
	// can act on. The instant is what somebody deciding whether to wait
	// plans around; the duration beside it is what says whether this is
	// nearly over.
	fmt.Println("dispatch is paused: the agent has no budget left in this window")
	fmt.Printf("resumes at: %s (in %s)\n",
		status.Until.Format(time.RFC3339), seconds(status.SecondsRemaining))
	if status.Reason != "" {
		fmt.Printf("reason:     %s\n", status.Reason)
	}
	if lifted == nil {
		fmt.Println("\nlift it now with \"grain pause -lift\" -- worth doing only if you know something\n" +
			"this deployment cannot, such as that the plan behind the refusal has been topped\n" +
			"up or that the deployment has moved onto another agent framework")
	}
}
