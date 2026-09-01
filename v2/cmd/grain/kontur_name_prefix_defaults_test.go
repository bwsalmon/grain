package main

import (
	"os"
	"regexp"
	"testing"

	"github.com/bwsalmon/grain/v2/pkg/orchestrator"
)

// The VM-name budget is spent by a value nothing in Go owns: the prefix a
// deployment is actually started with comes from v2/scripts/setup.sh and,
// above it, terraform/gcp-v2's own variable default. Both were "kontur-"
// while a VM was named after a slot ("kontur-1"), and both stayed that way
// when a VM started being named after its run -- so run() refused to start
// at all, on the default deploy path, with kontur sandboxing on by default.
//
// KonturSandboxes.CheckNamePrefix has its own unit tests, but they pass a
// prefix the test itself chose, which is precisely what cannot catch this:
// the question is not whether the check works, it is whether the value
// shipped satisfies it. These read the shipped defaults and ask the real
// check about them.
var (
	setupShDefault     = regexp.MustCompile(`GRAIN_KONTUR_VM_NAME_PREFIX="\$\{GRAIN_KONTUR_VM_NAME_PREFIX:-([^}]*)\}"`)
	terraformDefault   = regexp.MustCompile(`(?s)variable "kontur_vm_name_prefix".*?default\s*=\s*"([^"]*)"`)
	shippedPrefixFiles = []struct {
		name string
		path string
		re   *regexp.Regexp
	}{
		{"v2/scripts/setup.sh", "../../scripts/setup.sh", setupShDefault},
		{"terraform/gcp-v2/variables.tf", "../../../terraform/gcp-v2/variables.tf", terraformDefault},
	}
)

func TestShippedKonturVMNamePrefixDefaultsFitTheVMNameBudget(t *testing.T) {
	for _, f := range shippedPrefixFiles {
		data, err := os.ReadFile(f.path)
		if err != nil {
			t.Fatalf("reading %s: %v", f.name, err)
		}
		m := f.re.FindSubmatch(data)
		if m == nil {
			t.Fatalf("could not find the kontur VM name prefix default in %s -- if it moved, "+
				"point this test at it rather than dropping the check", f.name)
		}
		prefix := string(m[1])
		k := orchestrator.NewKonturSandboxes(orchestrator.KonturConfig{NamePrefix: prefix})
		if err := k.CheckNamePrefix(); err != nil {
			t.Errorf("%s ships -kontur-vm-name-prefix %q, which grain daemon refuses at startup: %v",
				f.name, prefix, err)
		}
	}
}

// The two have to agree as well as fit: terraform passes its own default
// through to setup.sh, so a deployment that goes through terraform and one
// that runs setup.sh directly should name their VMs the same way -- which
// is also what makes ReapOrphans' prefix match mean the same thing on both.
func TestShippedKonturVMNamePrefixDefaultsAgree(t *testing.T) {
	seen := map[string]string{}
	for _, f := range shippedPrefixFiles {
		data, err := os.ReadFile(f.path)
		if err != nil {
			t.Fatalf("reading %s: %v", f.name, err)
		}
		m := f.re.FindSubmatch(data)
		if m == nil {
			t.Fatalf("could not find the kontur VM name prefix default in %s", f.name)
		}
		seen[f.name] = string(m[1])
	}
	var first, firstName string
	for name, prefix := range seen {
		if firstName == "" {
			first, firstName = prefix, name
			continue
		}
		if prefix != first {
			t.Errorf("%s defaults to %q but %s defaults to %q", firstName, first, name, prefix)
		}
	}
}
