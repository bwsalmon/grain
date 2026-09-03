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
	}, 0); err != nil {
		return fmt.Errorf("seeding a running task: %w", err)
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
	if err := store.PutTask(ctx, *completedTask); err != nil {
		return fmt.Errorf("seeding a completed task: %w", err)
	}
	completedAt := ago(29 * time.Hour)
	if err := store.ObserveField(ctx, completed.ID, completedAt, func(o *model.Observation) {
		o.CompletedAt = &completedAt
	}); err != nil {
		return fmt.Errorf("seeding a completed task: %w", err)
	}

	stacked, err := create(ago(50*time.Minute), ui.CreateTaskRequest{
		Title:       "Add pagination to the tasks API",
		Description: "GET /api/tasks returns everything at once; needs a page size and a cursor.",
		Approved:    true,
	})
	if err != nil {
		return err
	}
	stackedTask, err := store.GetTask(ctx, stacked.ID)
	if err != nil {
		return err
	}
	stackedTask.Links = append(stackedTask.Links,
		model.Link{Kind: model.LinkFixes, Target: "https://github.com/acme/widgets/pull/104"})
	if err := store.PutTask(ctx, *stackedTask); err != nil {
		return fmt.Errorf("seeding a task with a stacked fix: %w", err)
	}
	stackedCompletedAt := ago(40 * time.Minute)
	if err := store.ObserveField(ctx, stacked.ID, stackedCompletedAt, func(o *model.Observation) {
		o.CompletedAt = &stackedCompletedAt
	}); err != nil {
		return fmt.Errorf("seeding a task with a stacked fix: %w", err)
	}
	// The fix task itself, filed straight into the store already
	// approved the same way orchestrator.fileFixTask files a real one --
	// see model.LinkFixTask's doc comment -- so `grain demo` shows the
	// nested-under-its-parent card bwsalmon/agents#378 asked for without
	// needing a real merge queue cycle to produce one.
	fixID, err := store.NewTaskID(ctx)
	if err != nil {
		return fmt.Errorf("seeding a stacked fix task: %w", err)
	}
	queue := model.Principal{Kind: model.PrincipalAutomation, ID: "merge-queue"}
	fixCreatedAt := ago(35 * time.Minute)
	fixTask := model.Task{
		ID:     fixID,
		Intent: model.IntentImplement,
		Title:  "\U0001F916 grain: fix acme/widgets#104",
		Body:   "Task " + stacked.ID + " opened acme/widgets#104, but it has conflicts with `main`.",
		Origin: model.Origin{
			Attribution: model.Attribution{Actor: queue},
			Reason:      model.ReasonFix,
		},
		Approval:  &model.Attribution{Actor: queue},
		Target:    cfg.DefaultTarget,
		Binding:   model.BindingDirective,
		Base:      model.BranchName(stacked.ID),
		AutoMerge: true,
		Links:     []model.Link{{Kind: model.LinkProposedBy, Target: stacked.ID}},
		CreatedAt: &fixCreatedAt,
	}
	if err := store.PutTask(ctx, fixTask); err != nil {
		return fmt.Errorf("seeding a stacked fix task: %w", err)
	}
	if err := store.UpdateTask(ctx, stacked.ID, func(t *model.Task) error {
		t.Links = append(t.Links, model.Link{Kind: model.LinkFixTask, Target: fixID})
		return nil
	}); err != nil {
		return fmt.Errorf("seeding a stacked fix task: %w", err)
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
