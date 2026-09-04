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
	"path/filepath"
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
  format [DIR] [-image I] [-force]
                            lay out a new, empty state repository: its README,
                            its .gitignore and the CI step above, written into
                            a clone you commit and push yourself. Needs no
                            -data-dir
  ci [DIR] [-image I] [-force]
                            write just that CI step, into a repository a
                            deployment is already using. Needs no -data-dir
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
	// Three commands here are not about this host's own installation:
	// they read, or write, a directory somebody hands them -- a CI runner
	// with a checkout and nothing else, or an operator's own clone of a
	// repository no deployment has been pointed at yet. Neither has a
	// data directory, a store or a daemon, so all three are dispatched
	// before -data-dir is insisted on rather than being made to invent
	// one.
	switch rest[0] {
	case "check":
		return stateCheck(ctx, rest[1:])
	case "format":
		return stateFormat(rest[1:])
	case "ci":
		return stateCI(rest[1:])
	}
	if *dataDir == "" {
		fs.Usage()
		return errors.New("-data-dir is required")
	}
	switch cmd, cmdArgs := rest[0], rest[1:]; cmd {
	case "status":
		return stateStatus(ctx, *dataDir, os.Stdout)
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

func stateStatus(ctx context.Context, dataDir string, out io.Writer) error {
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
	fmt.Fprintf(out, "repository: %s\n", where)
	fmt.Fprintf(out, "branch:     %s\n", repo.Branch())
	fmt.Fprintf(out, "working tree: %s\n", repo.Dir())
	fmt.Fprintf(out, "head:       %s\n", head)
	fmt.Fprintf(out, "schema:     %d in the repository, %d in this build\n", version, model.SchemaVersion)
	fmt.Fprintf(out, "secrets:    %s\n", secretsConfig(dataDir).File)
	// Whether this host can actually open that file, which is a separate
	// question from whether it is there: a data directory restored from
	// another installation arrives sealed to that installation's key, and
	// an operator should learn that here rather than from the first run
	// that cannot resolve a credential.
	//
	// Only asked when there is a file to open, because secrets.Open mints
	// a key for a data directory that has neither -- and a command that
	// reports on a key must not be the thing that creates one. An install
	// with nothing stored yet has nothing to fail to read anyway; where
	// its key stands is reportSecretsKey's answer, below.
	if _, statErr := os.Stat(secretsConfig(dataDir).File); statErr == nil {
		store := secrets.Open(secretsConfig(dataDir))
		if err := store.Check(); err != nil {
			fmt.Fprintf(out, "            unreadable: %v\n", err)
			if want, err := store.FileRecipient(); err == nil && want != "" {
				fmt.Fprintf(out, "            encrypted to %s -- install that key with `grain state key import`\n", want)
			}
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
		fmt.Fprintf(out, "dispatch:   unknown -- could not read this repository's history (%v), "+
			"so the git proxy refuses it to every sandbox\n", err)
	case settings.Remote == "":
		fmt.Fprintf(out, "dispatch:   not applicable -- a local-only repository is not reachable "+
			"through the git proxy at all\n")
	case held:
		fmt.Fprintf(out, "dispatch:   refused -- %s appears in this repository, so the git proxy "+
			"refuses it to every sandbox. Removing it does not undo that: a clone reads "+
			"history. Adopt a repository that has never held one to file settings tasks "+
			"against it\n", staterepo.SecretsFile)
	default:
		fmt.Fprintf(out, "dispatch:   allowed -- a task may be filed against this repository "+
			"(`grain create -repo <owner>/<name> ...`)\n")
	}
	reportSecretsKey(dataDir, out)
	return nil
}

// reportSecretsKey says where the private key is, whether it is there,
// and -- every time, not only when something is wrong -- that it is the
// operator's to back up.
//
// It is reported here because this is the command that describes what
// survives a rebuilt host, and the key is the one part of that which
// nothing else can stand in for: the rest of this output can be cloned
// back from a remote, and the encrypted file beside this key travels
// nowhere at all (secretsConfig -- it is deliberately out of the
// repository a sandbox may clone). A host restored onto a fresh data
// directory therefore needs both halves handed back, and a key nobody
// copied anywhere is a key one disk away from taking every secret with
// it. pkg/secrets refuses a secrets file whose key is missing rather
// than papering over it, so the moment to notice is now, while the key
// still exists to be copied somewhere.
//
// Read straight off disk rather than through secrets.Open, which would
// *mint* a key on a data directory that has none -- a reporting command
// must not create the thing it reports on.
func reportSecretsKey(dataDir string, out io.Writer) {
	path := secretsConfig(dataDir).KeyFile
	key, err := secrets.ReadKeyFile(path)
	switch {
	case errors.Is(err, secrets.ErrNoKey):
		fmt.Fprintf(out, "secrets key: %s (none yet -- grain mints one the first time it opens the store)\n", path)
		return
	case err != nil:
		fmt.Fprintf(out, "secrets key: %s (unreadable: %v)\n", path, err)
		return
	}
	fmt.Fprintf(out, "secrets key: %s\n", path)
	fmt.Fprintf(out, "            public key %s\n", key.Public())
	fmt.Fprintf(out, "            back this file up. It is the one thing here a redeploy cannot\n")
	fmt.Fprintf(out, "            rebuild: a host that mints a fresh key cannot read a secrets\n")
	fmt.Fprintf(out, "            file restored beside it. Seed it back with GRAIN_SECRETS_KEY\n")
	fmt.Fprintf(out, "            (scripts/setup.sh) on a deploy, or install one you are holding\n")
	fmt.Fprintf(out, "            with `grain state key import`.\n")
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
	// SyncAll, not Sync: a human typing this is asking for the whole
	// database to be written out now, not for the state tier alone with
	// grain's own churn left to its slower clock (pkg/staterepo/tier.go).
	changed, err := staterepo.SyncAll(ctx, repo, db, model.SchemaVersion)
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

// stateFormat lays out a state repository in a directory that does not
// hold one yet: the README, the .gitignore and the workflow that runs
// `grain state check` on every pull request against it.
//
// It is the step before `grain state adopt`, and it runs somewhere else
// -- in a clone the operator made, not in the deployment's own working
// tree. That is not squeamishness about touching the tree. A push that
// adds a file under .github/workflows is refused unless the credential
// making it may write workflows, and grain's own installation token need
// not be able to: a commit sitting in the deployment's tree that its
// sync loop can never push would wedge every later sync behind it, so
// the one file that carries that risk is committed by a human with a
// credential of their own. Nothing here commits anything.
//
// It writes no dump, and that is what keeps the bootstrap's two cases
// apart. A repository with a dump in it is one `adopt` imports over the
// database; a formatted one still has none, so adopting it is still the
// "empty repository grain seeds from what it has" case -- see
// staterepo/format.go.
func stateFormat(args []string) error {
	fs := flag.NewFlagSet("grain state format", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, "usage: grain state format [-image IMAGE] [-force] [DIR]\n\n"+
			"DIR is a clone of the repository this installation's state will live in,\n"+
			"and defaults to \".\". It must not hold a dump already: use `grain state ci`\n"+
			"to add the validation step to a repository a deployment is already using.\n")
	}
	image := fs.String("image", staterepo.DefaultCheckImage,
		"container image the CI step runs `grain state check` from")
	force := fs.Bool("force", false, "replace a workflow file that is already there")
	dir, err := stateDirArg(fs, args, "format")
	if err != nil {
		return err
	}
	if staterepo.HasDump(dir) {
		return fmt.Errorf("%s already holds a dump (there are table files in %s/), so it is a "+
			"state repository already; `grain state ci %s` adds the validation step to it "+
			"without formatting anything", dir, staterepo.TablesDir, dir)
	}
	formatted, err := staterepo.Format(dir, *image, *force)
	if err != nil {
		return err
	}
	fmt.Printf("formatted %s as a grain state repository\n", dir)
	for _, path := range formatted.Wrote {
		fmt.Printf("  wrote   %s\n", path)
	}
	for _, path := range formatted.Left {
		fmt.Printf("  left    %s alone; it is already there (-force replaces it)\n", path)
	}
	fmt.Printf("\nCommit and push these yourself: a push that adds a file under\n"+
		".github/workflows needs a credential that may write workflows, and grain's\n"+
		"own pushes deliberately never carry one.\n\n"+
		"  git -C %s add .\n"+
		"  git -C %s commit -m 'Format this repository for grain'\n"+
		"  git -C %s push\n\n"+
		"Then, on the host grain runs on:\n\n"+
		"  grain state adopt -remote <this repository's URL>\n\n"+
		"which seeds it from that deployment's database. There is deliberately no dump\n"+
		"here, so adopting still means \"grain writes its state out\" rather than \"this\n"+
		"repository replaces grain's state\".\n", dir, dir, dir)
	return nil
}

// stateCI writes the validation workflow into a repository that already
// has one of everything else -- the case `format` cannot cover, because
// a deployment has been pushing to this repository since before the
// workflow existed and formatting it would mean nothing.
//
// The same warning applies to the commit as it does above, and for the
// same reason, which is why this writes the file and stops rather than
// committing into a deployment's working tree.
func stateCI(args []string) error {
	fs := flag.NewFlagSet("grain state ci", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, "usage: grain state ci [-image IMAGE] [-force] [DIR]\n\n"+
			"DIR is a clone of a state repository, and defaults to \".\". Writes the\n"+
			"workflow that runs `grain state check` on every pull request against it.\n")
	}
	image := fs.String("image", staterepo.DefaultCheckImage,
		"container image the CI step runs `grain state check` from")
	force := fs.Bool("force", false, "replace a workflow file that is already there")
	dir, err := stateDirArg(fs, args, "ci")
	if err != nil {
		return err
	}
	wrote, err := staterepo.EnsureWorkflow(dir, *image, *force)
	if err != nil {
		return err
	}
	if !wrote {
		fmt.Printf("%s is already there; -force replaces it\n",
			filepath.Join(dir, filepath.FromSlash(staterepo.WorkflowFile)))
		return nil
	}
	fmt.Printf("wrote %s\n", filepath.Join(dir, filepath.FromSlash(staterepo.WorkflowFile)))
	fmt.Printf("\nCommit and push it yourself: a push that adds a file under\n" +
		".github/workflows needs a credential that may write workflows, and grain's\n" +
		"own pushes deliberately never carry one.\n")
	return nil
}

// stateDirArg parses the flags of a command whose only argument is the
// directory it works on, defaulting to the working directory -- which is
// what these commands look like when they are run at the top of a
// checkout, in CI or in a terminal sitting in one.
func stateDirArg(fs *flag.FlagSet, args []string, name string) (string, error) {
	if err := fs.Parse(args); err != nil {
		return "", err
	}
	switch fs.NArg() {
	case 0:
		return ".", nil
	case 1:
		return fs.Arg(0), nil
	default:
		fs.Usage()
		return "", fmt.Errorf("%s takes one directory", name)
	}
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
