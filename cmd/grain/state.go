// state.go is `grain state`, the operator's own handle on the state
// repository (pkg/staterepo) -- the git repository holding the database
// this deployment runs on.
//
// Like `grain secrets`, it edits files under -data-dir directly rather
// than talking to a daemon, so it only works on the host the server runs
// on. Unlike it, most of what it does is meant to be done while the
// daemon is stopped: adopting a different repository replaces every row
// in the database, and doing that underneath a running reconcile loop is
// not something this command tries to make safe.
//
// The three commands are the three answers to "where does this
// installation's state live", which is the same set the UI's bootstrap
// offers: `status` says which one is in force, `local` chooses no remote
// at all (and needs no input from anybody), and `adopt` points the
// installation at a repository -- an existing one, whose contents then
// become the database, or an empty one, which grain seeds from what it
// already has.
package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/bwsalmon/grain/pkg/model"
	"github.com/bwsalmon/grain/pkg/model/sqlite"
	"github.com/bwsalmon/grain/pkg/secrets"
	"github.com/bwsalmon/grain/pkg/staterepo"
)

const stateUsage = `usage: grain state -data-dir DIR <command> [args]

-data-dir must name the same root a colocated ` + "`grain daemon`" + ` was
started with. This edits files on disk, not a running daemon; stop the
daemon before adopting or importing, since both replace every row in the
database.

Commands:
  status                    where this installation's state lives, and whether it is committed
  local                     use a local-only repository: no remote, no credential, no input
  adopt -remote URL [-branch B] [-token-file F] [-secrets-key-file F]
                            point this installation at a repository. An existing
                            one's contents replace the database; an empty one is
                            seeded from it
  sync                      export the database, commit and push, now
  check DIR                 load a state repository's files into a throwaway
                            database and report what breaks. Needs no -data-dir
                            and touches nothing: this is the CI step a state
                            repository runs against a proposed change
  key show                  print this installation's secrets public key
  key path                  print where the private key is read from
  key import [-key-file F]  install a secrets private key this operator holds,
                            reading it from -key-file or stdin
`

func stateCmd(args []string) {
	if err := runState(args); err != nil {
		fmt.Fprintln(os.Stderr, "grain state: "+err.Error())
		os.Exit(1)
	}
}

func runState(args []string) error {
	fs := flag.NewFlagSet("grain state", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, stateUsage) }
	dataDir := fs.String("data-dir", "", "root directory a colocated `grain daemon` was started with (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	rest := fs.Args()
	if len(rest) == 0 {
		fs.Usage()
		return errors.New("a command is required")
	}
	ctx := context.Background()
	// check is the one command here that is not about this host's own
	// installation: it reads a directory somebody hands it, in a CI
	// runner that has no data directory, no store and no deployment. So
	// it is dispatched before -data-dir is insisted on rather than being
	// made to invent one.
	if rest[0] == "check" {
		return stateCheck(ctx, rest[1:])
	}
	if *dataDir == "" {
		fs.Usage()
		return errors.New("-data-dir is required")
	}
	switch cmd, cmdArgs := rest[0], rest[1:]; cmd {
	case "status":
		return stateStatus(ctx, *dataDir)
	case "local":
		return stateLocal(ctx, *dataDir)
	case "adopt":
		return stateAdopt(ctx, *dataDir, cmdArgs)
	case "sync":
		return stateSync(ctx, *dataDir)
	case "key":
		return stateKey(*dataDir, cmdArgs)
	default:
		fs.Usage()
		return fmt.Errorf("unknown command %q", cmd)
	}
}

func stateStatus(ctx context.Context, dataDir string) error {
	settings, err := staterepo.LoadSettings(dataDir)
	if err != nil {
		return err
	}
	repo, err := openStateRepo(ctx, dataDir)
	if err != nil {
		return err
	}
	where := "local only (no remote)"
	if settings.Remote != "" {
		where = settings.Remote
	}
	head, _ := repo.Head(ctx)
	if head == "" {
		head = "(nothing committed yet)"
	}
	version, err := staterepo.ReadSchemaVersion(repo.Dir())
	if err != nil {
		return err
	}
	fmt.Printf("repository: %s\n", where)
	fmt.Printf("branch:     %s\n", repo.Branch())
	fmt.Printf("working tree: %s\n", repo.Dir())
	fmt.Printf("head:       %s\n", head)
	fmt.Printf("schema:     %d in the repository, %d in this build\n", version, model.SchemaVersion)
	fmt.Printf("secrets:    %s (key: %s)\n",
		secretsConfig(dataDir).File, secretsConfig(dataDir).KeyFile)
	// Whether this host can actually open that file, which is a separate
	// question from whether it is there: a data directory restored from
	// another installation arrives sealed to that installation's key, and
	// an operator should learn that here rather than from the first run
	// that cannot resolve a credential.
	store := secrets.Open(secretsConfig(dataDir))
	if err := store.Check(); err != nil {
		fmt.Printf("            unreadable: %v\n", err)
		if want, err := store.FileRecipient(); err == nil && want != "" {
			fmt.Printf("            encrypted to %s -- install that key with `grain state key import`\n", want)
		}
	}
	// Whether a task can be dispatched at this repository, which is not
	// something an operator can tell by looking: it turns on whether
	// grain's encrypted secrets file appears anywhere in the history, and
	// a repository that has one looks exactly like one that has not until
	// the first push through the proxy is refused.
	held, err := repo.HasSecrets(ctx)
	switch {
	case err != nil:
		fmt.Printf("dispatch:   unknown -- could not read this repository's history (%v), "+
			"so the git proxy refuses it to every sandbox\n", err)
	case settings.Remote == "":
		fmt.Printf("dispatch:   not applicable -- a local-only repository is not reachable " +
			"through the git proxy at all\n")
	case held:
		fmt.Printf("dispatch:   refused -- %s appears in this repository, so the git proxy "+
			"refuses it to every sandbox. Removing it does not undo that: a clone reads "+
			"history. Adopt a repository that has never held one to file settings tasks "+
			"against it\n", staterepo.SecretsFile)
	default:
		fmt.Printf("dispatch:   allowed -- a task may be filed against this repository " +
			"(`grain create -repo <owner>/<name> ...`)\n")
	}
	return nil
}

// stateLocal is the bootstrap's third answer: run with no external
// repository. It is not a "reset" -- the working tree and every commit
// in it stay exactly as they are, and only the remote is dropped -- so
// an installation that was pushing somewhere and no longer wants to
// keeps all of its history locally.
func stateLocal(ctx context.Context, dataDir string) error {
	settings, err := staterepo.LoadSettings(dataDir)
	if err != nil {
		return err
	}
	settings.Remote = ""
	settings.TokenFile = ""
	if err := staterepo.SaveSettings(dataDir, settings); err != nil {
		return err
	}
	repo, err := openStateRepo(ctx, dataDir)
	if err != nil {
		return err
	}
	if err := repo.SetRemote(ctx, ""); err != nil {
		return err
	}
	fmt.Printf("state stays local, in %s\n", repo.Dir())
	return nil
}

// stateAdopt points this installation at a repository.
//
// Destructive by design, and in one direction: if the repository already
// holds a dump, that dump replaces the database wholesale, because the
// repository is the source of truth and adopting one means taking its
// answer rather than merging it with whatever this host happened to have
// (bwsalmon/grain#174's own "it is ok to assume this is a destructive
// operation"). An empty repository is the other case, and there grain's
// current database is what seeds it.
func stateAdopt(ctx context.Context, dataDir string, args []string) error {
	fs := flag.NewFlagSet("grain state adopt", flag.ContinueOnError)
	remote := fs.String("remote", "", "git URL of the repository holding this installation's state (required)")
	branch := fs.String("branch", staterepo.DefaultBranch, "branch state lives on")
	tokenFile := fs.String("token-file", "", "file holding the credential to push with; "+
		"defaults to the GitHub credential ladder under <data-dir>/secrets/github")
	keyFile := fs.String("secrets-key-file", "", "file holding the secrets private key "+
		"<data-dir>/secrets/secrets.enc is encrypted to; required to read the secrets of an "+
		"installation this host has not run before")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *remote == "" {
		fs.Usage()
		return errors.New("-remote is required")
	}
	// Read before anything is moved: a key that does not parse should
	// fail here, with the installation still exactly as it was, rather
	// than halfway through adopting.
	var secretsKey secrets.Key
	if *keyFile != "" {
		var err error
		if secretsKey, err = secrets.ReadKeyFile(*keyFile); err != nil {
			return err
		}
	}
	// The working tree is a clone of whatever was there before, and a
	// clone of one repository cannot become a clone of another by
	// changing a URL: its history is the wrong history. Moving it aside
	// rather than deleting it means an operator who adopts the wrong
	// repository has not lost anything.
	archived, err := archiveStateRepo(dataDir)
	if err != nil {
		return err
	}
	if archived != "" {
		fmt.Printf("moved the previous state repository to %s\n", archived)
	}
	if err := staterepo.SaveSettings(dataDir, staterepo.Settings{
		Remote: *remote, Branch: *branch, TokenFile: *tokenFile,
	}); err != nil {
		return err
	}
	repo, err := openStateRepo(ctx, dataDir)
	if err != nil {
		return err
	}
	if *keyFile != "" {
		if err := secrets.Open(secretsConfig(dataDir)).ImportKey(secretsKey); err != nil {
			return err
		}
		fmt.Printf("installed the secrets key from %s\n", *keyFile)
	}
	_, db, err := openStore(dataDir)
	if err != nil {
		return err
	}
	defer db.Close()
	if err := staterepo.Load(ctx, repo, db, model.SchemaVersion); err != nil {
		return err
	}
	head, _ := repo.Head(ctx)
	fmt.Printf("adopted %s at %s\n", *remote, head)
	return nil
}

// archiveStateRepo moves an existing working tree aside, if there is
// one, keeping a dated copy rather than removing it. It returns where it
// went, or "" when there was nothing there, so that the caller says so
// in its own voice -- a terminal for `grain state adopt`, the daemon's
// journal for the pane's own button.
//
// Nothing has to be carried out of the archived tree any more: the
// encrypted secrets file lives under <data-dir>/secrets (secretsConfig)
// rather than in the repository, so adopting no longer moves the one
// unregenerable thing this deployment has out from under itself.
func archiveStateRepo(dataDir string) (string, error) {
	dir := stateRepoDir(dataDir)
	if _, err := os.Stat(dir); err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	moved := dir + ".replaced-" + timestampSuffix()
	if err := os.Rename(dir, moved); err != nil {
		return "", fmt.Errorf("moving the existing state repository aside: %w", err)
	}
	return moved, nil
}

// timestampSuffix names an archived directory by when it was archived,
// in a form that sorts and that a shell does not have to be quoted for.
func timestampSuffix() string { return time.Now().UTC().Format("20060102-150405") }

func stateSync(ctx context.Context, dataDir string) error {
	repo, err := openStateRepo(ctx, dataDir)
	if err != nil {
		return err
	}
	_, db, err := openStore(dataDir)
	if err != nil {
		return err
	}
	defer db.Close()
	changed, err := staterepo.Sync(ctx, repo, db, model.SchemaVersion)
	if err != nil {
		return err
	}
	if !changed {
		fmt.Println("already up to date, nothing to commit")
		return nil
	}
	head, _ := repo.Head(ctx)
	fmt.Printf("committed %s\n", head)
	return nil
}

// stateCheck loads a directory of state files into a database it throws
// away, and says what broke.
//
// The point is where it runs: not here, on the deployment, but in the
// state repository's own CI, against a pull request nobody has merged
// yet. staterepo.Import is already the validator -- one transaction,
// foreign keys deferred, rolled back whole on any inconsistency -- and
// until this command existed the only thing that ever ran it was a
// daemon starting up. A dump that was malformed, or a row missing a NOT
// NULL column, therefore failed at import time on the next start: after
// the merge, in the deployment, to whoever was on call rather than to
// whoever wrote the diff.
//
// It needs no -data-dir and reads nothing but the directory named, so a
// CI runner needs this binary and a checkout and nothing else:
//
//	grain state check .
func stateCheck(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("grain state check", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, "usage: grain state check DIR\n\n"+
			"DIR is the root of a state repository -- the directory holding tables/ and\n"+
			"schema-version, which for a CI step run at the top of the checkout is \".\".\n")
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	dir := "."
	switch fs.NArg() {
	case 0:
	case 1:
		dir = fs.Arg(0)
	default:
		fs.Usage()
		return errors.New("check takes one directory")
	}

	db, cleanup, err := throwawayStore()
	if err != nil {
		return err
	}
	defer cleanup()

	report, err := staterepo.Check(ctx, db, dir, model.SchemaVersion)
	if err != nil {
		// Whatever the import objected to, printed before the error, so a
		// CI log says which schema the dump claimed as well as why it did
		// not load.
		for _, w := range report.Warnings {
			fmt.Printf("warning: %s\n", w)
		}
		return err
	}
	for _, w := range report.Warnings {
		fmt.Printf("warning: %s\n", w)
	}
	fmt.Printf("schema:  %d, which is what this build of grain knows\n", report.SchemaVersion)
	fmt.Printf("loaded:  %d rows across %d tables\n", report.Total(), len(report.Rows))
	for _, t := range report.Tables() {
		fmt.Printf("  %-28s %d\n", t, report.Rows[t])
	}
	fmt.Println("ok: grain can load this")
	return nil
}

// throwawayStore opens an empty database in a temporary directory, at
// this build's schema, for a check to import into and nothing else.
//
// Temporary and its own, because staterepo.Import replaces every row in
// whatever database it is given: a check that ran against the store
// under -data-dir would be a check that cost the deployment its state.
// Check tightens the connection pool and the pragmas on it from there.
func throwawayStore() (*sql.DB, func(), error) {
	dir, err := os.MkdirTemp("", "grain-state-check-")
	if err != nil {
		return nil, nil, err
	}
	db, err := sqlite.Open(sqlite.DefaultConfig(dir))
	if err != nil {
		os.RemoveAll(dir)
		return nil, nil, err
	}
	cleanup := func() {
		db.Close()
		os.RemoveAll(dir)
	}
	if err := model.New(db).Init(context.Background()); err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("applying schema: %w", err)
	}
	return db, cleanup, nil
}

// stateKey is the operator's view of the secrets key: the public half,
// which is safe to print, and where the private half is read from, which
// is what they have to back up. It never prints the private key --
// reading it is `cat` on a file they already own, and a command that
// prints one invites it into a terminal's scrollback.
func stateKey(dataDir string, args []string) error {
	store := openSecrets(dataDir)
	if len(args) == 0 {
		return errors.New("usage: grain state key show|path|import")
	}
	switch args[0] {
	case "show":
		pub, err := store.PublicKey()
		if err != nil {
			return err
		}
		fmt.Println(pub)
		return nil
	case "path":
		fmt.Println(store.KeyFile())
		return nil
	case "import":
		return stateKeyImport(store, args[1:])
	default:
		return fmt.Errorf("unknown key command %q", args[0])
	}
}

// stateKeyImport installs a key the operator already holds, which is
// what makes "put this deployment on a new machine" a restore rather
// than a fresh install with an unreadable file in it.
//
// The key is read from a file or from stdin and never from a flag: a
// private key on a command line is readable by anyone else on the host
// through `ps` and lands in the shell's history besides. That is the same
// reason `grain secrets set` takes no value on its own command line.
func stateKeyImport(store *secrets.Store, args []string) error {
	fs := flag.NewFlagSet("grain state key import", flag.ContinueOnError)
	keyFile := fs.String("key-file", "", "file holding the private key; defaults to reading stdin")
	if err := fs.Parse(args); err != nil {
		return err
	}
	var data []byte
	var err error
	if *keyFile != "" {
		data, err = os.ReadFile(*keyFile)
	} else {
		data, err = io.ReadAll(os.Stdin)
	}
	if err != nil {
		return err
	}
	key, err := secrets.ParseKey(string(data))
	if err != nil {
		return err
	}
	if err := store.ImportKey(key); err != nil {
		return err
	}
	fmt.Printf("installed %s at %s\n", key.Public(), store.KeyFile())
	return nil
}
