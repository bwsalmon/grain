package agent

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

// passwdPath and groupPath are variables so the tests can point them at
// fixtures; nothing else reassigns them.
var (
	passwdPath = "/etc/passwd"
	groupPath  = "/etc/group"
)

// lookupAccount resolves name against the guest's own /etc/passwd and
// /etc/group.
//
// These are read directly rather than through os/user because os/user
// answers neither of the two things that actually matter here: the login
// shell (its User struct has no field for it) and the supplementary
// groups (its GroupIds needs cgo to be reliable, and this binary is
// built CGO_ENABLED=0 so that one artifact runs on both the Debian and
// the Alpine guest). Both files are the plain colon-separated format on
// either guest.
//
// An empty name means root, matching guestexec's own default.
func lookupAccount(name string) (*account, error) {
	if name == "" {
		name = "root"
	}

	a, err := readPasswd(name)
	if err != nil {
		return nil, err
	}
	if a.Shell == "" {
		a.Shell = "/bin/sh"
	}
	if a.Home == "" {
		a.Home = "/"
	}
	// A home directory named in /etc/passwd but not present would make
	// every command fail on chdir, which reads as the command being
	// broken rather than the account being half-created. Root of the
	// filesystem is a worse working directory and a much better error.
	if fi, err := os.Stat(a.Home); err != nil || !fi.IsDir() {
		a.Home = "/"
	}

	a.self = a.UID == os.Getuid()
	if !a.self {
		a.Groups = readGroups(a.Name, a.GID)
	}
	return a, nil
}

func readPasswd(name string) (*account, error) {
	f, err := os.Open(passwdPath)
	if err != nil {
		return nil, fmt.Errorf("reading the guest's accounts: %w", err)
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		// name:passwd:uid:gid:gecos:home:shell
		fields := strings.Split(sc.Text(), ":")
		if len(fields) < 7 || fields[0] != name {
			continue
		}
		return &account{
			Name:  fields[0],
			UID:   atoi(fields[2]),
			GID:   atoi(fields[3]),
			Home:  fields[5],
			Shell: fields[6],
		}, nil
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("reading the guest's accounts: %w", err)
	}
	return nil, fmt.Errorf("no account %q on this guest", name)
}

// readGroups returns the supplementary groups name belongs to, always
// including its own primary gid.
//
// Failure here is deliberately not fatal: a guest with no readable
// /etc/group is one where the primary group alone is the best available
// answer, and refusing to run anything at all would be a worse one.
func readGroups(name string, gid int) []uint32 {
	groups := []uint32{uint32(gid)}
	seen := map[uint32]bool{uint32(gid): true}

	f, err := os.Open(groupPath)
	if err != nil {
		return groups
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		// name:passwd:gid:member,member,...
		fields := strings.Split(sc.Text(), ":")
		if len(fields) < 4 {
			continue
		}
		for _, member := range strings.Split(fields[3], ",") {
			if member != name {
				continue
			}
			g := uint32(atoi(fields[2]))
			if !seen[g] {
				seen[g] = true
				groups = append(groups, g)
			}
		}
	}
	return groups
}
