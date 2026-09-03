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
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/bwsalmon/grain/pkg/model"
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
  adopt -remote URL [-branch B] [-token-file F]
                            point this installation at a repository. An existing
                            one's contents replace the database; an empty one is
                            seeded from it
  sync                      export the database, commit and push, now
  key show                  print this installation's secrets public key
  key path                  print where the private key is read from
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
	if *dataDir == "" {
		fs.Usage()
		return errors.New("-data-dir is required")
	}
	rest := fs.Args()
	if len(rest) == 0 {
		fs.Usage()
		return errors.New("a command is required")
	}
	ctx := context.Background()
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
	reportSecretsKey(dataDir, out)
	return nil
}

// reportSecretsKey says where the private key is, whether it is there,
// and -- every time, not only when something is wrong -- that it is the
// operator's to back up.
//
// It is reported here because this is the command that describes what
// survives a rebuilt host, and the key is the one part of that which no
// repository holds: everything else in this output can be cloned back
// from a remote, and the file that decrypts the secrets in it cannot.
// A redeploy that mints a fresh key instead comes up unable to read its
// own repository's secrets file, which pkg/secrets refuses outright
// rather than papering over -- so the moment to notice is now, while the
// key still exists to be copied somewhere.
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
	fmt.Fprintf(out, "            rebuild: a host that mints a fresh key cannot read the secrets\n")
	fmt.Fprintf(out, "            its own repository still holds. Seed it back with GRAIN_SECRETS_KEY\n")
	fmt.Fprintf(out, "            (scripts/setup.sh), or copy it into place before starting grain.\n")
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
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *remote == "" {
		fs.Usage()
		return errors.New("-remote is required")
	}
	// The working tree is a clone of whatever was there before, and a
	// clone of one repository cannot become a clone of another by
	// changing a URL: its history is the wrong history. Moving it aside
	// rather than deleting it means an operator who adopts the wrong
	// repository has not lost anything.
	if err := archiveStateRepo(dataDir); err != nil {
		return err
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
// one, keeping a dated copy rather than removing it.
func archiveStateRepo(dataDir string) error {
	dir := stateRepoDir(dataDir)
	if _, err := os.Stat(dir); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	moved := dir + ".replaced-" + timestampSuffix()
	if err := os.Rename(dir, moved); err != nil {
		return fmt.Errorf("moving the existing state repository aside: %w", err)
	}
	fmt.Printf("moved the previous state repository to %s\n", moved)
	return nil
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

// stateKey is the operator's view of the secrets key: the public half,
// which is safe to print, and where the private half is read from, which
// is what they have to back up. It never prints the private key --
// reading it is `cat` on a file they already own, and a command that
// prints one invites it into a terminal's scrollback.
func stateKey(dataDir string, args []string) error {
	store := secrets.Open(secretsConfig(dataDir))
	if len(args) == 0 {
		return errors.New("usage: grain state key show|path")
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
	default:
		return fmt.Errorf("unknown key command %q", args[0])
	}
}
