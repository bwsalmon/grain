// Command ui is grain's standalone task UI: one binary, no install step
// beyond `go build`, serving pkg/ui's JSON API and static frontend on
// localhost and opening it in the system's default browser. See
// v2/README.md's ui/ section and pkg/ui's own package doc comment for why
// it is shaped this way -- a local web server rather than an Electron/
// Tauri/native app, so the same binary that runs on a developer's Mac or
// Linux box today is exactly the API surface a future iOS/Android client
// would speak to, with nothing about the server itself to rebuild.
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
	"os/user"
	"runtime"
	"strings"

	"github.com/bwsalmon/grain/v2/pkg/model"
	"github.com/bwsalmon/grain/v2/pkg/model/dolt"
	"github.com/bwsalmon/grain/v2/pkg/ui"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8420", "address to serve the UI on")
	storeAddr := flag.String("store-addr", "", "host:port of a Dolt SQL server holding the task store -- the multi-writer deployment (graind plus this plus a CLI)")
	storeDatabase := flag.String("store-database", "grain", "database name on -store-addr")
	storeUser := flag.String("store-user", "root", "user to connect to -store-addr as")
	storePasswordFile := flag.String("store-password-file", "", "file holding the password for -store-user")
	dataDir := flag.String("data-dir", "", "root directory of an embedded store, used when -store-addr is unset -- single writer, so nothing else may be running against it")
	actor := flag.String("as", "", "principal to attribute tasks created here to (defaults to the OS user)")
	defaultTargetRepo := flag.String("default-target-repo", "",
		"owner/name a task with no repo of its own targets")
	open := flag.Bool("open", true, "open the UI in the system's default browser once it's listening")
	flag.Parse()

	server, err := serverConfig(*storeAddr, *storeDatabase, *storeUser, *storePasswordFile)
	if err != nil {
		log.Fatalf("ui: %v", err)
	}
	db, err := dolt.OpenOrConnect(*dataDir, server)
	if err != nil {
		log.Fatalf("ui: opening the task store: %v", err)
	}
	defer db.Close()

	store := model.New(db)
	if err := store.Init(context.Background()); err != nil {
		log.Fatalf("ui: applying the schema: %v", err)
	}

	cfg := ui.Config{
		Actor:        ui.DefaultActor(actorID(*actor)),
		Capabilities: ui.DefaultCapabilities(),
	}
	if *defaultTargetRepo != "" {
		repo, err := model.ParseRepo(*defaultTargetRepo)
		if err != nil {
			fmt.Fprintf(os.Stderr, "ui: -default-target-repo: %v\n", err)
			os.Exit(2)
		}
		cfg.DefaultTarget = &repo
	}
	srv := ui.NewServer(cfg, store)

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatalf("ui: listening on %s: %v", *addr, err)
	}
	url := "http://" + ln.Addr().String()
	log.Printf("ui: serving the task store on %s as %s", url, cfg.Actor.ID)
	if *open {
		openBrowser(url)
	}
	log.Fatal(http.Serve(ln, srv))
}

// serverConfig assembles a dolt.ServerConfig from the -store-* flags. An
// empty addr means "no server", which OpenOrConnect reads as "use the
// embedded database".
func serverConfig(addr, database, user, passwordFile string) (dolt.ServerConfig, error) {
	if addr == "" {
		return dolt.ServerConfig{}, nil
	}
	cfg := dolt.ServerConfig{Addr: addr, Database: database, User: user}
	if passwordFile != "" {
		data, err := os.ReadFile(passwordFile)
		if err != nil {
			return dolt.ServerConfig{}, fmt.Errorf("reading -store-password-file: %w", err)
		}
		cfg.Password = strings.TrimSpace(string(data))
	}
	return cfg, nil
}

// actorID resolves who this process acts as: the -as flag, else the OS
// user, else ui.DefaultActor's own fallback. A GitHub issue used to
// answer this with its own opening account; nothing files an issue now,
// so the deployment has to say.
func actorID(flagValue string) string {
	if flagValue != "" {
		return flagValue
	}
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	return ""
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
		log.Printf("ui: opening browser: %v", err)
	}
}
