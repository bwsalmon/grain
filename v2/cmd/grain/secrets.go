// secrets.go implements `grain secrets`, the CLI's own answer to
// bwsalmon/agents#357: given -data-dir naming the same root a colocated
// `grain daemon` was started with (daemon.go's own -data-dir, "root
// directory for the store, secrets, and sandbox roots"), it edits the
// files under <data-dir>/secrets directly -- no daemon RPC of any kind,
// since pkg/secrets.Store already reads fresh off disk on every Resolve.
// That also means it only ever works when run on the same host as the
// server: -data-dir is a local filesystem path, so pointing it at the
// daemon's own root is only possible from where that root actually
// lives.
//
// It never opens the task store: a secret has nothing to do with a task,
// so main's dispatch routes "secrets" here the same way it already does
// for daemon/ui/mcpserver, ahead of runCLI's own store-connecting setup.
//
// list prints which secrets exist and which keys each one holds, never
// a value -- the same "tell what's set, not what it is" restriction
// pkg/secrets.Store.List and pkg/ui's own secrets endpoints hold to. set
// is the only way a value moves through this command at all, and it
// deliberately never takes one as an argv flag -- anything on a command
// line is readable back by anyone else on this host with a `ps` --
// reading it instead from -value-file or, left unset, from stdin.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/bwsalmon/grain/v2/pkg/secrets"
)

const secretsUsage = `usage: grain secrets -data-dir DIR <command> [args]

-data-dir must name the same root a colocated ` + "`grain daemon`" + ` was
started with -- secrets live at <data-dir>/secrets, the same path daemon.go
itself resolves. This edits files on disk, not a running daemon, so it only
works when run on the same host as the server (bwsalmon/agents#357).

Commands:
  list                          list every secret and the keys it holds (never values)
  set [-value-file F] S K       write key K's value in secret S, from -value-file or stdin
  delete <secret> [key]         delete one key, or (with no key given) the whole secret
`

func secretsCmd(args []string) {
	if err := runSecrets(args); err != nil {
		fmt.Fprintln(os.Stderr, "grain secrets: "+err.Error())
		os.Exit(1)
	}
}

func runSecrets(args []string) error {
	fs := flag.NewFlagSet("grain secrets", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, secretsUsage) }
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

	store := secrets.New(filepath.Join(*dataDir, "secrets"))
	cmd, cmdArgs := rest[0], rest[1:]
	switch cmd {
	case "list":
		return secretsList(store, cmdArgs)
	case "set":
		return secretsSet(store, cmdArgs)
	case "delete":
		return secretsDelete(store, cmdArgs)
	default:
		fs.Usage()
		return fmt.Errorf("unknown command %q", cmd)
	}
}

func secretsList(store *secrets.Store, args []string) error {
	fs := flag.NewFlagSet("grain secrets list", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	list, err := store.List()
	if err != nil {
		return err
	}
	if len(list) == 0 {
		fmt.Println("no secrets set")
		return nil
	}
	for _, s := range list {
		fmt.Printf("%s: %s\n", s.Name, strings.Join(s.Keys, ", "))
	}
	return nil
}

func secretsSet(store *secrets.Store, args []string) error {
	fs := flag.NewFlagSet("grain secrets set", flag.ContinueOnError)
	valueFile := fs.String("value-file", "", "file holding the exact bytes to write as this key's value; defaults to reading stdin")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 2 {
		fs.Usage()
		return errors.New("usage: grain secrets set [-value-file FILE] <secret> <key>")
	}
	secretName, key := fs.Arg(0), fs.Arg(1)

	var value []byte
	var err error
	if *valueFile != "" {
		value, err = os.ReadFile(*valueFile)
	} else {
		value, err = io.ReadAll(os.Stdin)
	}
	if err != nil {
		return fmt.Errorf("reading the value: %w", err)
	}
	if err := store.Set(secretName, key, value); err != nil {
		return err
	}
	fmt.Printf("set %s/%s\n", secretName, key)
	return nil
}

func secretsDelete(store *secrets.Store, args []string) error {
	fs := flag.NewFlagSet("grain secrets delete", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	switch fs.NArg() {
	case 1:
		secretName := fs.Arg(0)
		if err := store.DeleteSecret(secretName); err != nil {
			return err
		}
		fmt.Printf("deleted %s\n", secretName)
	case 2:
		secretName, key := fs.Arg(0), fs.Arg(1)
		if err := store.DeleteKey(secretName, key); err != nil {
			return err
		}
		fmt.Printf("deleted %s/%s\n", secretName, key)
	default:
		fs.Usage()
		return errors.New("usage: grain secrets delete <secret> [key]")
	}
	return nil
}
