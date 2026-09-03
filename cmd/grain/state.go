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
	"path/filepath"
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
  adopt -remote URL [-branch B] [-token-file F] [-secrets-key-file F]
                            point this installation at a repository. An existing
                            one's contents replace the database; an empty one is
                            seeded from it
  sync                      export the database, commit and push, now
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
	// question from whether it is there: a repository adopted from
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
	keyFile := fs.String("secrets-key-file", "", "file holding the secrets private key this repository's "+
		"secrets.enc is encrypted to; required to read the secrets of an installation this host has not run before")
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
	carried, err := carrySecretsFile(archived, repo.Dir())
	if err != nil {
		return err
	}
	if carried {
		fmt.Printf("carried this installation's secrets across into %s\n", repo.Dir())
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
// went, or "" when there was nothing there -- both because the caller
// says so in its own voice (a terminal or the daemon's journal) and
// because carrySecretsFile has to go looking in it.
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

// carrySecretsFile brings the encrypted secrets file across from the
// working tree an adopt moved aside.
//
// Everything else in the old tree is regenerable -- it is a dump of a
// database that is still right here -- and the secrets file is not: it
// is the one thing in the repository that is not a materialisation of
// something else. Adopting an empty repository, which is the bootstrap's
// "start from scratch", would otherwise leave every secret this
// deployment holds behind in a directory nothing looks at again, and
// silently: the store would come back up reporting no secrets set rather
// than reporting anything wrong.
//
// A repository that already carries its own secrets file keeps it. That
// file is the adopted installation's, sealed to the adopted
// installation's key, and it is the source of truth for the same reason
// its tables are; the key to read it is what ImportKey is for.
func carrySecretsFile(fromDir, toDir string) (bool, error) {
	if fromDir == "" {
		return false, nil
	}
	src := filepath.Join(fromDir, secrets.DefaultFileName)
	data, err := os.ReadFile(src)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("reading %s: %w", src, err)
	}
	dst := filepath.Join(toDir, secrets.DefaultFileName)
	switch _, err := os.Stat(dst); {
	case err == nil:
		return false, nil
	case !os.IsNotExist(err):
		return false, fmt.Errorf("inspecting %s: %w", dst, err)
	}
	if err := os.WriteFile(dst, data, 0o600); err != nil {
		return false, fmt.Errorf("writing %s: %w", dst, err)
	}
	return true, nil
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
// what makes "clone the repository onto a new machine" a restore rather
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
