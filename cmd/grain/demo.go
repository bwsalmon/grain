// demo.go implements `grain demo`, formerly `grain ui -demo` before
// bwsalmon/agents#363 folded the real UI into `grain daemon` and left
// nothing standalone for the CLI's own store flag (-data-dir) to attach
// to. This mode never needed that anyway -- its whole point, unchanged,
// is a throwaway store nothing else is connected to (this file's own
// seedDemo doc comment) -- so it is its own small subcommand now rather
// than a flag on a mode that no longer exists: a real pkg/ui.Server, over
// a real embedded SQLite database in a fresh temp directory, seeded with
// fake tasks, for trying out the frontend with no daemon, no
// orchestrator, no sandbox, no Gemini key and no real git repo behind any
// of it.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/bwsalmon/grain/pkg/model"
	"github.com/bwsalmon/grain/pkg/model/sqlite"
	"github.com/bwsalmon/grain/pkg/orchestrator"
	"github.com/bwsalmon/grain/pkg/ui"
)

func demo(args []string) {
	fs := flag.NewFlagSet("grain demo", flag.ExitOnError)
	addr := fs.String("addr", "127.0.0.1:8420", "address to serve the demo UI on")
	actor := fs.String("as", "", "principal to attribute the fake tasks to (defaults to the OS user)")
	defaultTargetRepo := fs.String("default-target-repo", "acme/widgets", "owner/name the fake tasks target")
	open := fs.Bool("open", true, "open the UI in the system's default browser once it's listening")
	fs.Parse(args)

	repo, err := model.ParseRepo(*defaultTargetRepo)
	if err != nil {
		fmt.Fprintf(os.Stderr, "grain demo: -default-target-repo: %v\n", err)
		os.Exit(2)
	}
	cfg := ui.Config{
		Actor:         ui.DefaultActor(actorID(*actor)),
		DefaultTarget: &repo,
		Capabilities:  ui.OfferedCapabilities(),
	}

	dir, err := os.MkdirTemp("", "grain-demo-")
	if err != nil {
		log.Fatalf("grain demo: creating a throwaway store: %v", err)
	}
	db, err := sqlite.Open(sqlite.DefaultConfig(dir))
	if err != nil {
		log.Fatalf("grain demo: opening the throwaway store: %v", err)
	}
	defer db.Close()

	store := model.New(db)
	ctx := context.Background()
	if err := store.Init(ctx); err != nil {
		log.Fatalf("grain demo: applying the schema: %v", err)
	}
	if err := seedDemo(ctx, store, cfg); err != nil {
		log.Fatalf("grain demo: seeding fake tasks: %v", err)
	}

	srv := ui.NewServer(cfg, store)
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatalf("grain demo: listening on %s: %v", *addr, err)
	}
	url := "http://" + ln.Addr().String()
	log.Printf("grain demo: serving fake tasks from a throwaway store at %s -- nothing here is real", url)
	if *open {
		openBrowser(url)
	}
	log.Fatal(http.Serve(ln, srv))
}

// seedDemo populates a store with a fixed set of fake tasks for `grain
// demo`: one in each model.State a task can read, so a person working on
// pkg/ui/static's frontend can see every card style, badge and button the
// real states produce without an orchestrator, a sandbox, a Gemini key or
// a real git repo behind any of it. It goes through ui.Client and
// model.Store's own exported writes -- the same path a human clicking
// through the UI or the daemon's reconcile loop would take -- rather than
// inserting rows directly, so a demo task is exactly as well-formed as a
// real one and nothing here can drift from what those paths actually
// require.

func seedDemo(ctx context.Context, store *model.Store, cfg ui.Config) error {
	client := ui.NewClient(cfg, store)
	now := time.Now().UTC()
	ago := func(d time.Duration) time.Time { return now.Add(-d) }

	create := func(createdAt time.Time, req ui.CreateTaskRequest) (ui.Task, error) {
		client.Now = func() time.Time { return createdAt }
		task, err := client.CreateTask(ctx, req)
		if err != nil {
			return ui.Task{}, fmt.Errorf("seeding %q: %w", req.Title, err)
		}
		return task, nil
	}

	if _, err := create(ago(4*time.Hour), ui.CreateTaskRequest{
		Title:       "Add a dark mode toggle",
		Description: "Settings page needs a theme switch; several people have asked for it.",
	}); err != nil {
		return err
	}

	if _, err := create(ago(3*time.Hour), ui.CreateTaskRequest{
		Title:       "Fix flaky retry test",
		Description: "TestRetryBackoff fails about one run in twenty under -race.",
		Approved:    true,
	}); err != nil {
		return err
	}

	running, err := create(ago(90*time.Minute), ui.CreateTaskRequest{
		Title:        "Bump the Go toolchain to 1.24",
		Description:  "go.mod and the CI image both need the version bump.",
		Approved:     true,
		Capabilities: &[]string{"self-debug"},
	})
	if err != nil {
		return err
	}
	if err := store.StartRun(ctx, model.Run{
		ID:        "demo-run-" + running.ID,
		TaskID:    running.ID,
		Sandbox:   "demo-sandbox",
		Attempt:   1,
		StartedAt: ago(6 * time.Minute),
	}, model.Limits{}); err != nil {
		return fmt.Errorf("seeding a running task: %w", err)
	}
	// The prompt that run was "given", built by the same function a real
	// dispatch builds one with (orchestrator.BuildPrompt) rather than
	// written out here: what the UI's own prompt pane shows in the demo
	// is then the real thing, and stays the real thing as that function
	// changes, instead of a paraphrase of it that quietly goes stale.
	//
	// Read back through the store rather than built from the request
	// above: BuildPrompt takes the model.Task that was actually filed,
	// with the id and target ui.CreateTask resolved for it. A failure
	// here fails the seed, like every other write in this function -- a
	// demo whose prompt pane silently says "nothing has been dispatched"
	// is the one thing that pane must not say about a running task.
	//
	// The wall-clock budget is DefaultMaxRunRuntime for the same reason:
	// there is no orchestrator.Config behind a seeded run to read a
	// deployment's own MaxRunRuntime off, and the default is what a
	// deployment that has not set one really gives its runs.
	seeded, err := store.GetTask(ctx, running.ID)
	if err != nil {
		return fmt.Errorf("re-reading the running task: %w", err)
	}
	if seeded == nil {
		return fmt.Errorf("re-reading the running task: %s was not stored", running.ID)
	}
	if err := store.SetRunPrompt(ctx, "demo-run-"+running.ID,
		orchestrator.BuildPrompt(*seeded, orchestrator.CheckoutDir, true,
			orchestrator.DefaultMaxRunRuntime,
			// A first attempt: this seeded run has no earlier one to
			// carry an outcome, and nothing has been said on the task,
			// so orchestrator.History{} is what a real dispatch of it
			// would have assembled too.
			orchestrator.History{Attempt: 1}, nil)); err != nil {
		return fmt.Errorf("seeding a running task's prompt: %w", err)
	}

	awaiting, err := create(ago(2*time.Hour), ui.CreateTaskRequest{
		Title:       "Investigate the merge queue stall",
		Description: "Queue head hasn't moved since last night; find out why.",
		Approved:    true,
	})
	if err != nil {
		return err
	}
	questionAt := ago(20 * time.Minute)
	commentID, err := store.AddComment(ctx, model.Comment{
		TaskID: awaiting.ID,
		Author: model.Attribution{
			Actor:      model.Principal{Kind: model.PrincipalAutomation, ID: cfg.Actor.ID},
			OnBehalfOf: &model.Principal{Kind: model.PrincipalAgent, ID: "demo-run-" + awaiting.ID},
		},
		Body:      "Two PRs are both stuck failing the same lint check -- want me to fix both, or just unblock the queue?",
		CreatedAt: questionAt,
	})
	if err != nil {
		return fmt.Errorf("seeding an awaiting-reply task: %w", err)
	}
	if err := store.ObserveField(ctx, awaiting.ID, questionAt, func(o *model.Observation) {
		o.PendingQuestionCommentID = &commentID
	}); err != nil {
		return fmt.Errorf("seeding an awaiting-reply task: %w", err)
	}

	completed, err := create(ago(30*time.Hour), ui.CreateTaskRequest{
		Title:       "Rotate the Gemini signing key",
		Description: "Old key expires this week.",
		Approved:    true,
	})
	if err != nil {
		return err
	}
	completedTask, err := store.GetTask(ctx, completed.ID)
	if err != nil {
		return err
	}
	completedTask.Links = append(completedTask.Links,
		model.Link{Kind: model.LinkFixes, Target: "https://github.com/acme/widgets/pull/101"})
	// Submitted: model.StateAwaitingSubmit is what an unsubmitted pull
	// request reads as, so this is the field that makes this card the
	// "Queued for merge" one rather than a second copy of the card
	// seeded below it.
	completedTask.AutoMerge = true
	if err := store.PutTask(ctx, *completedTask); err != nil {
		return fmt.Errorf("seeding a completed task: %w", err)
	}
	completedAt := ago(29 * time.Hour)
	if err := store.ObserveField(ctx, completed.ID, completedAt, func(o *model.Observation) {
		o.CompletedAt = &completedAt
	}); err != nil {
		return fmt.Errorf("seeding a completed task: %w", err)
	}

	// The other post-run state: a run that finished and opened a pull
	// request nobody has submitted, which is where a task sits until a
	// human clicks Submit (model.StateAwaitingSubmit). Every seeded state
	// gets a card so the demo shows the whole vocabulary, and this one is
	// the state a deployment that does not auto-merge lives in.
	awaitingSubmit, err := create(ago(6*time.Hour), ui.CreateTaskRequest{
		Title:       "Cache the capability readiness probe",
		Description: "Settings' Capabilities tab re-probes every render; cache it for a minute.",
		Approved:    true,
	})
	if err != nil {
		return err
	}
	awaitingSubmitTask, err := store.GetTask(ctx, awaitingSubmit.ID)
	if err != nil {
		return err
	}
	awaitingSubmitTask.Links = append(awaitingSubmitTask.Links,
		model.Link{Kind: model.LinkFixes, Target: "https://github.com/acme/widgets/pull/102"})
	if err := store.PutTask(ctx, *awaitingSubmitTask); err != nil {
		return fmt.Errorf("seeding a task awaiting submit: %w", err)
	}
	awaitingSubmitAt := ago(5 * time.Hour)
	if err := store.ObserveField(ctx, awaitingSubmit.ID, awaitingSubmitAt, func(o *model.Observation) {
		o.CompletedAt = &awaitingSubmitAt
	}); err != nil {
		return fmt.Errorf("seeding a task awaiting submit: %w", err)
	}

	// A task the merge queue is repairing: its pull request went
	// conflicted, so the queue commented and sent the task itself back to
	// an agent on the very branch that pull request is open from
	// (orchestrator.requeueForRepair). There is no second task and no
	// second branch -- what a reader sees instead is the task running
	// again with its mark in green, which is the whole point of seeding
	// it here.
	repairing, err := create(ago(50*time.Minute), ui.CreateTaskRequest{
		Title:       "Add pagination to the tasks API",
		Description: "GET /api/tasks returns everything at once; needs a page size and a cursor.",
		Approved:    true,
	})
	if err != nil {
		return err
	}
	repairingTask, err := store.GetTask(ctx, repairing.ID)
	if err != nil {
		return err
	}
	repairingTask.Links = append(repairingTask.Links,
		model.Link{Kind: model.LinkFixes, Target: "https://github.com/acme/widgets/pull/104"})
	// Submitted, like the completed card above and for a stronger reason:
	// the merge queue only ever repairs one of its own members
	// (orchestrator.isQueueMember, which is AutoMerge), so a task being
	// repaired with no AutoMerge would be a shape no real deployment
	// produces.
	repairingTask.AutoMerge = true
	if err := store.PutTask(ctx, *repairingTask); err != nil {
		return fmt.Errorf("seeding a task under repair: %w", err)
	}
	repairAskedAt := ago(35 * time.Minute)
	if err := store.ObserveField(ctx, repairing.ID, repairAskedAt, func(o *model.Observation) {
		// Exactly what requeueForRepair writes: the record of the repair,
		// and no CompletedAt, which is what put the task back to work.
		o.MergeQueueRepairAt = &repairAskedAt
		o.CompletedAt = nil
		o.RetryRequestedAt = &repairAskedAt
	}); err != nil {
		return fmt.Errorf("seeding a task under repair: %w", err)
	}
	queue := model.Principal{Kind: model.PrincipalAutomation, ID: "merge-queue"}
	if _, err := store.AddComment(ctx, model.Comment{
		TaskID: repairing.ID,
		Author: model.Attribution{Actor: queue},
		Body: "acme/widgets#104 has conflicts with `main`, so the merge queue has sent " +
			"this task back to an agent to repair it. No approval needed, and no separate " +
			"task: the work happens on `" + model.BranchName(repairing.ID) + "` -- the " +
			"branch this task's pull request is already open from -- so the resolution and " +
			"the change share one pull request and one round of checks.",
		CreatedAt: repairAskedAt,
	}); err != nil {
		return fmt.Errorf("seeding a task under repair: %w", err)
	}
	if err := store.StartRun(ctx, model.Run{
		ID:        "demo-run-" + repairing.ID,
		TaskID:    repairing.ID,
		Sandbox:   "demo-sandbox-repair",
		Attempt:   2,
		StartedAt: ago(30 * time.Minute),
	}, model.Limits{}); err != nil {
		return fmt.Errorf("seeding a task under repair: %w", err)
	}

	// A task somebody put aside (model.StateDeferred): approved once, so
	// that picking it back up would put it straight onto the queue, and
	// hidden from the list and the board until the reader asks for it --
	// which is the whole of what the state does and worth seeing here.
	deferred, err := create(ago(150*time.Hour), ui.CreateTaskRequest{
		Title:       "Replace the settings pane with a form library",
		Description: "Worth doing, not worth doing this quarter.",
		Approved:    true,
	})
	if err != nil {
		return err
	}
	deferredAt := ago(140 * time.Hour)
	if err := store.SetDeferred(ctx, deferred.ID, &deferredAt); err != nil {
		return fmt.Errorf("seeding a deferred task: %w", err)
	}

	closed, err := create(ago(200*time.Hour), ui.CreateTaskRequest{
		Title:       "Spike: websocket transport",
		Description: "Explored replacing polling with a websocket; decided against it for now.",
		Approved:    true,
	})
	if err != nil {
		return err
	}
	closedAt := ago(190 * time.Hour)
	if err := store.ObserveField(ctx, closed.ID, closedAt, func(o *model.Observation) {
		o.ClosedAt = &closedAt
	}); err != nil {
		return fmt.Errorf("seeding a closed task: %w", err)
	}

	return nil
}
