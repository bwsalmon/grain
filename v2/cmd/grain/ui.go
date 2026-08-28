// ui.go implements `grain ui`, formerly its own cmd/ui binary before
// bwsalmon/agents#313 combined every mode into one: grain's standalone
// task UI, serving pkg/ui's JSON API and static frontend on localhost and
// opening it in the system's default browser, with no install step
// beyond building this one binary. See v2/README.md's ui/ section and
// pkg/ui's own package doc comment for why it is shaped this way -- a
// local web server rather than an Electron/Tauri/native app, so the same
// binary that runs on a developer's Mac or Linux box today is exactly the
// API surface a future iOS/Android client would speak to, with nothing
// about the server itself to rebuild.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"runtime"

	"github.com/bwsalmon/grain/v2/pkg/model"
	"github.com/bwsalmon/grain/v2/pkg/model/dolt"
	"github.com/bwsalmon/grain/v2/pkg/ui"
)

func uiServe(args []string) {
	fs := flag.NewFlagSet("grain ui", flag.ExitOnError)
	addr := fs.String("addr", "127.0.0.1:8420", "address to serve the UI on")
	storeAddr := fs.String("store-addr", "", "host:port of a Dolt SQL server holding the task store -- the multi-writer deployment (graind plus this plus a CLI)")
	storeDatabase := fs.String("store-database", "grain", "database name on -store-addr")
	storeUser := fs.String("store-user", "root", "user to connect to -store-addr as")
	storePasswordFile := fs.String("store-password-file", "", "file holding the password for -store-user")
	dataDir := fs.String("data-dir", "", "root directory of an embedded store, used when -store-addr is unset -- single writer, so nothing else may be running against it")
	actor := fs.String("as", "", "principal to attribute tasks created here to (defaults to the OS user)")
	defaultTargetRepo := fs.String("default-target-repo", "",
		"owner/name a task with no repo of its own targets")
	open := fs.Bool("open", true, "open the UI in the system's default browser once it's listening")
	demo := fs.Bool("demo", false,
		"seed a throwaway store with fake tasks instead of opening a real one -- "+
			"for trying out the UI with no orchestrator, sandbox or repo behind it")
	fs.Parse(args)

	if *demo && (*storeAddr != "" || *dataDir != "") {
		fmt.Fprintln(os.Stderr, "grain ui: -demo seeds its own throwaway store; -store-addr/-data-dir must be unset")
		os.Exit(2)
	}
	if *demo {
		dir, err := os.MkdirTemp("", "grain-ui-demo-")
		if err != nil {
			log.Fatalf("grain ui: creating a throwaway store for -demo: %v", err)
		}
		*dataDir = dir
	}

	server, err := serverConfig(*storeAddr, *storeDatabase, *storeUser, *storePasswordFile)
	if err != nil {
		log.Fatalf("grain ui: %v", err)
	}
	db, err := dolt.OpenOrConnect(*dataDir, server)
	if err != nil {
		log.Fatalf("grain ui: opening the task store: %v", err)
	}
	defer db.Close()

	store := model.New(db)
	if err := store.Init(context.Background()); err != nil {
		log.Fatalf("grain ui: applying the schema: %v", err)
	}

	cfg := ui.Config{
		Actor:        ui.DefaultActor(actorID(*actor)),
		Capabilities: ui.DefaultCapabilities(),
	}
	if *defaultTargetRepo != "" {
		repo, err := model.ParseRepo(*defaultTargetRepo)
		if err != nil {
			fmt.Fprintf(os.Stderr, "grain ui: -default-target-repo: %v\n", err)
			os.Exit(2)
		}
		cfg.DefaultTarget = &repo
	}
	if *demo {
		if cfg.DefaultTarget == nil {
			repo := model.RepoRef{Owner: "acme", Name: "widgets"}
			cfg.DefaultTarget = &repo
		}
		if err := seedDemo(context.Background(), store, cfg); err != nil {
			log.Fatalf("grain ui: seeding -demo data: %v", err)
		}
		log.Printf("grain ui: -demo mode -- serving fake tasks from a throwaway store at %s, nothing here is real", *dataDir)
	}
	srv := ui.NewServer(cfg, store)

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatalf("grain ui: listening on %s: %v", *addr, err)
	}
	url := "http://" + ln.Addr().String()
	log.Printf("grain ui: serving the task store on %s as %s", url, cfg.Actor.ID)
	if *open {
		openBrowser(url)
	}
	log.Fatal(http.Serve(ln, srv))
}

// openBrowser best-effort launches url in the system's default browser --
// failing to do so (headless box, unknown OS) is never fatal, since the
// server is up and the URL is printed regardless.
func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "linux":
		cmd = exec.Command("xdg-open", url)
	default:
		return
	}
	if err := cmd.Start(); err != nil {
		log.Printf("grain ui: opening browser: %v", err)
	}
}
