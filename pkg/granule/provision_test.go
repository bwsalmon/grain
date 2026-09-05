package granule_test

import (
	"archive/tar"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/bwsalmon/grain/pkg/granule"
)

// mountedTree writes a container-side /grain the way a backend would,
// and returns its root.
func mountedTree(t *testing.T, files map[string]string, modes map[string]os.FileMode) string {
	t.Helper()
	root := t.TempDir()
	for name, body := range files {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("creating %s: %v", filepath.Dir(p), err)
		}
		mode := os.FileMode(0o600)
		if m, ok := modes[name]; ok {
			mode = m
		}
		if err := os.WriteFile(p, []byte(body), mode); err != nil {
			t.Fatalf("writing %s: %v", p, err)
		}
	}
	return root
}

// entries reads a provisioning tar back as a path -> mode map, which is
// the assertion that matters: what lands in a sandbox, and how readable
// it is.
func entries(t *testing.T, blob []byte) (map[string]int64, map[string]string) {
	t.Helper()
	modes := map[string]int64{}
	bodies := map[string]string{}
	tr := tar.NewReader(bytes.NewReader(blob))
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("reading the tar: %v", err)
		}
		if h.Typeflag == tar.TypeDir {
			continue
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("reading %s: %v", h.Name, err)
		}
		modes["/"+h.Name] = h.Mode
		bodies["/"+h.Name] = string(body)
	}
	return modes, bodies
}

// The tree is the mapping: a placement mounted at
// <root>/placements/home/agent/.netrc has to land at /home/agent/.netrc
// in the guest, with the mode it was mounted with and no other.
func TestPlacementsLandAtTheirGuestPathsWithTheirModes(t *testing.T) {
	root := mountedTree(t, map[string]string{
		"placements/home/agent/.netrc": "machine github.com",
		"placements/etc/pki/ca.crt":    "-----BEGIN-----",
		"setup":                        "#!/bin/sh\ngit clone\n",
	}, map[string]os.FileMode{
		"placements/etc/pki/ca.crt": 0o644,
		"setup":                     0o755,
	})

	plan, err := granule.PlanProvision(root, "")
	if err != nil {
		t.Fatalf("PlanProvision: %v", err)
	}
	blob, err := plan.Tar()
	if err != nil {
		t.Fatalf("Tar: %v", err)
	}
	modes, bodies := entries(t, blob)

	if got := bodies["/home/agent/.netrc"]; got != "machine github.com" {
		t.Errorf("/home/agent/.netrc = %q", got)
	}
	// The default is the tight one, and a placement that did not choose
	// keeps it: this is a credential's path.
	if got := modes["/home/agent/.netrc"]; got != 0o600 {
		t.Errorf("/home/agent/.netrc mode = %o, want 600", got)
	}
	if got := modes["/etc/pki/ca.crt"]; got != 0o644 {
		t.Errorf("/etc/pki/ca.crt mode = %o, want 644", got)
	}
	if _, ok := bodies[granule.GuestSetupPath]; !ok {
		t.Errorf("the setup script did not land at %s: %v", granule.GuestSetupPath, bodies)
	}
	if !plan.Setup {
		t.Error("plan does not report a setup script")
	}
}

// The client is installed by granule from the container layer, not
// placed by the controller, and it has to be executable or the agent's
// escape hatches are a file it cannot run.
func TestTheClientIsInstalledExecutable(t *testing.T) {
	root := mountedTree(t, map[string]string{"prompt": "do the thing"}, nil)
	bin := filepath.Join(t.TempDir(), "grain")
	if err := os.WriteFile(bin, []byte("ELF"), 0o755); err != nil {
		t.Fatalf("writing the client: %v", err)
	}

	plan, err := granule.PlanProvision(root, bin)
	if err != nil {
		t.Fatalf("PlanProvision: %v", err)
	}
	blob, err := plan.Tar()
	if err != nil {
		t.Fatalf("Tar: %v", err)
	}
	modes, bodies := entries(t, blob)
	if bodies[granule.GuestClientPath] != "ELF" {
		t.Fatalf("the client did not land at %s: %v", granule.GuestClientPath, bodies)
	}
	if modes[granule.GuestClientPath] != 0o755 {
		t.Errorf("client mode = %o, want 755", modes[granule.GuestClientPath])
	}
	// The prompt is the container's own and the agent reads it there, so
	// it must not be copied into the sandbox.
	for p := range bodies {
		if p == "/prompt" || p == "/grain/prompt" {
			t.Errorf("the prompt was copied into the guest at %s", p)
		}
	}
}

// A symlink in the mounted tree points wherever whoever mounted it
// aimed it. Following one would copy a file the controller never placed
// -- the container's own credential, for instance -- into the sandbox.
func TestASymlinkedPlacementIsRefusedRatherThanFollowed(t *testing.T) {
	root := mountedTree(t, map[string]string{"credential": "sk-secret"}, nil)
	dir := filepath.Join(root, "placements", "home", "agent")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("creating %s: %v", dir, err)
	}
	if err := os.Symlink(filepath.Join(root, "credential"), filepath.Join(dir, "stolen")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	_, err := granule.PlanProvision(root, "")
	if err == nil {
		t.Fatal("PlanProvision accepted a symlinked placement")
	}
	if !bytes.Contains([]byte(err.Error()), []byte("/home/agent/stolen")) {
		t.Errorf("error does not name the placement: %v", err)
	}
}

// An empty tree is ordinary -- a grain with no placements and no setup
// -- and must not be an error.
func TestAnEmptyTreeProvisionsNothing(t *testing.T) {
	plan, err := granule.PlanProvision(t.TempDir(), "")
	if err != nil {
		t.Fatalf("PlanProvision: %v", err)
	}
	if len(plan.Files) != 0 || plan.Setup {
		t.Fatalf("plan = %+v, want empty", plan)
	}
}
