package container

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

var digits = regexp.MustCompile(`^\d+$`)

// `docker run <image> schema-version` is not an arbitrary smoke test.
//
// It is the exact command pkg/upgrade's image health check runs against a
// freshly pulled image before pointing a deployment at it
// (healthCheckImage), and the one setup.sh's
// reformat_store_if_schema_changed asks through its own wrapper. If the
// entrypoint, the binary or its libc coupling were wrong, this is where
// every one of those would fail.
func TestTheImageRunsTheCLIAndReportsASchemaVersion(t *testing.T) {
	requireImage(t)
	out := strings.TrimSpace(docker(t, "run", "--rm", image, "schema-version"))
	if !digits.MatchString(out) {
		t.Fatalf("schema-version printed %q", out)
	}
}

// Dockerfile's header lists these and says why each is there.
//
// A missing one does not fail a build or a startup -- it fails the first
// dispatch, the first kontur VM, or the first look at the Logs pane, which
// is exactly the class of failure putting them in an image was meant to
// end.
//
// Both agent CLIs are in that list on purpose: which framework a run uses
// is a live per-task choice, so an image carrying one of them is an image
// that fails every run choosing the other.
func TestTheImageCarriesEveryBinaryTheDaemonShellsOutTo(t *testing.T) {
	requireImage(t)
	script := `for b in git bash konturctl docker claude agy journalctl ssh curl; do ` +
		`command -v "$b" >/dev/null || { echo "MISSING $b"; exit 1; }; ` +
		`done; echo all-present`

	out := docker(t, "run", "--rm", "--entrypoint", "sh", image, "-c", script)
	if !strings.Contains(out, "all-present") {
		t.Fatalf("a binary the daemon shells out to is missing: %s", out)
	}
}

// A kontur deployment runs two images, and is told about neither.
//
// `grain sandbox-image` prints the sandbox container reference stamped
// into this build at build time (cmd/grain/sandboximage.go). That is what
// scripts/setup.sh pulls when GRAIN_KONTUR_OCI_IMAGE names nothing, and
// what an upgrade asks a newly pulled grain for so it can pull the
// matching sandbox alongside it -- so an image that cannot answer is a
// deployment that has to be configured by hand, and one that answers with
// the wrong thing is a deployment running two commits' worth of itself.
func TestTheImageNamesTheSandboxContainerItExpects(t *testing.T) {
	requireImage(t)
	out := strings.TrimSpace(docker(t, "run", "--rm", image, "sandbox-image"))

	if !strings.Contains(out, "/kontur-sandbox:") {
		t.Fatalf("sandbox-image printed %q", out)
	}
	// A bare `make image` leaves the source default (the tag CI keeps
	// pointed at main); CI stamps the immutable tag of the sandbox built
	// from the same commit. Either is a real reference -- what must never
	// happen is an empty or malformed one.
	if strings.HasSuffix(out, ":") || strings.Contains(out, " ") {
		t.Fatalf("sandbox-image printed a malformed reference: %q", out)
	}
}

// The image carries the source it was built from.
//
// tests/deploy pins that the Dockerfile says so and that cmd/grain looks
// there; this is the half that can only be asked of a real image -- that
// the tree actually arrived, that it is grain's own and not some other
// checkout, and that the three things deliberately left behind stayed
// behind. .git is the one worth naming: it is 6MB of history no reader
// wants (selfdebug is os.ReadFile and os.ReadDir), and a `cp -a` that
// stopped excluding it would be invisible except in the image's size.
func TestTheImageCarriesItsOwnSource(t *testing.T) {
	requireImage(t)

	const root = "/usr/local/share/grain/src"
	script := `set -e
test -f ` + root + `/go.mod
grep -q "^module github.com/bwsalmon/grain$" ` + root + `/go.mod
test -d ` + root + `/cmd/grain
test -d ` + root + `/pkg/capability/selfdebug
for excluded in .git bin pkg/ui/frontend/node_modules; do
  if [ -e ` + root + `/$excluded ]; then echo "LEAKED $excluded"; exit 1; fi
done
echo source-present`

	out := docker(t, "run", "--rm", "--entrypoint", "sh", image, "-c", script)
	if !strings.Contains(out, "source-present") {
		t.Fatalf("the image does not carry a usable copy of its own source: %s", out)
	}
}

// The unit always passes --user with the host's own grain account.
//
// An image that only worked as root would leave every file it wrote into
// the mounted data directory owned by root -- see
// TestTheStoreComesOutOwnedByTheHostAccount below for the consequence this
// is the precondition of.
func TestTheImageRunsAsAnUnprivilegedUID(t *testing.T) {
	requireImage(t)
	out := strings.TrimSpace(docker(t,
		"run", "--rm", "--user", "1234:1234", "--entrypoint", "id", image, "-u"))
	if out != "1234" {
		t.Fatalf("id -u printed %q, want 1234", out)
	}
}

// --- the daemon, in the container --------------------------------------

func TestTheDaemonServesItsAPIFromTheContainer(t *testing.T) {
	requireImage(t)
	d := start(t, t.TempDir(), options{})

	status, raw := d.api(t, http.MethodGet, "/api/config", nil)
	if status != http.StatusOK {
		t.Fatalf("GET /api/config: %d %s", status, raw)
	}

	// Real content, not just a 200: the actor the daemon files work as,
	// and the capability list the UI builds its panes from.
	var got config
	decode(t, raw, &got)
	if got.Actor == "" {
		t.Error("the daemon reports no actor")
	}
	if got.Capabilities == nil {
		t.Error("the daemon reports no capability list at all")
	}
}

func TestTasksCreatedThroughTheAPIComeBackOutOfTheStore(t *testing.T) {
	requireImage(t)
	d := start(t, t.TempDir(), options{})

	created := createTask(t, d, "a task filed inside a container")
	if created.State != "proposed" {
		t.Fatalf("a freshly filed task is in %q, want proposed", created.State)
	}

	status, raw := d.api(t, http.MethodGet, "/api/tasks", nil)
	if status != http.StatusOK {
		t.Fatalf("GET /api/tasks: %d %s", status, raw)
	}
	var listed []task
	decode(t, raw, &listed)
	if !slicesContainsID(listed, created.ID) {
		t.Fatalf("%s is not in the list: %s", created.ID, raw)
	}

	status, raw = d.api(t, http.MethodGet, "/api/tasks/"+created.ID, nil)
	if status != http.StatusOK {
		t.Fatalf("GET /api/tasks/%s: %d %s", created.ID, status, raw)
	}
	var fetched task
	decode(t, raw, &fetched)
	if fetched.Title != "a task filed inside a container" {
		t.Fatalf("fetched title %q", fetched.Title)
	}

	// A write that is not a create: the conversation half of a task, which
	// lands in a different table of the same store.
	status, raw = d.api(t, http.MethodPost, "/api/tasks/"+created.ID+"/comments",
		map[string]string{"body": "a comment from the e2e suite"})
	if status != http.StatusOK && status != http.StatusCreated {
		t.Fatalf("commenting: %d %s", status, raw)
	}

	_, raw = d.api(t, http.MethodGet, "/api/tasks/"+created.ID, nil)
	decode(t, raw, &fetched)
	var found bool
	for _, comment := range fetched.Comments {
		if strings.Contains(comment.Body, "e2e suite") {
			found = true
		}
	}
	if !found {
		t.Fatalf("the comment did not come back out of the store: %s", raw)
	}
}

// The whole reason the data directory is a bind mount.
//
// A container is disposable; the store, the secrets database and the
// deployment's own configuration are not. Replacing the container -- which
// is what every deploy and every upgrade does -- must leave all of it
// exactly where it was.
func TestATaskSurvivesTheContainerThatCreatedIt(t *testing.T) {
	requireImage(t)
	root := t.TempDir()

	first := start(t, root, options{})
	created := createTask(t, first, "a task that outlives its container")
	first.stop()

	// A genuinely new container over the same directories, the way a
	// `systemctl restart grain-daemon.service` brings one up.
	second := start(t, root, options{})
	if second.Name == first.Name {
		t.Fatal("the second container is the first one")
	}

	status, raw := second.api(t, http.MethodGet, "/api/tasks/"+created.ID, nil)
	if status != http.StatusOK {
		t.Fatalf("GET /api/tasks/%s from a new container: %d %s", created.ID, status, raw)
	}
	var fetched task
	decode(t, raw, &fetched)
	if fetched.Title != "a task that outlives its container" {
		t.Fatalf("fetched title %q", fetched.Title)
	}
}

// `docker run --user` is what keeps this true.
//
// Without it the daemon would run as root inside the container and every
// file it wrote into the mounted data directory would come out root-owned
// on the host -- unreadable by the account an operator's own `grain` CLI
// runs as, and a permission failure the moment anything outside the
// container touched it.
func TestTheStoreComesOutOwnedByTheHostAccount(t *testing.T) {
	requireImage(t)
	d := start(t, t.TempDir(), options{})
	createTask(t, d, "a task whose store file we stat")

	store := filepath.Join(d.Data, "store")
	var written []string
	waitFor(t, "a store file under "+store, 60*time.Second, func() error {
		var err error
		written, err = filesUnder(store)
		if err != nil {
			return err
		}
		if len(written) == 0 {
			return errNothingYet
		}
		return nil
	})

	for _, path := range written {
		owned, err := ownedByUs(path)
		if err != nil {
			t.Fatalf("stat %s: %v", path, err)
		}
		if !owned {
			t.Errorf("%s is not ours", path)
		}
	}
}

// /usr/local/bin/grain is a wrapper around this exact invocation.
//
// install_cli_wrappers writes a script that runs the deployment image on
// the host network with GRAIN_SERVER pointed at the daemon's own port --
// so an operator's `grain list` and the daemon are the same build, talking
// over the loopback address they share.
func TestTheCLIReachesTheDaemonFromASecondContainer(t *testing.T) {
	requireImage(t)
	d := start(t, t.TempDir(), options{})
	createTask(t, d, "a task the CLI should list")

	out := docker(t, "run", "--rm", "--network", "host",
		"--user", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()),
		"--env", "GRAIN_SERVER="+d.Base,
		image, "-json", "list")

	var listed []task
	decode(t, []byte(out), &listed)
	for _, listedTask := range listed {
		if listedTask.Title == "a task the CLI should list" {
			return
		}
	}
	t.Fatalf("the CLI did not list the task: %s", out)
}

func slicesContainsID(tasks []task, id string) bool {
	for _, candidate := range tasks {
		if candidate.ID == id {
			return true
		}
	}
	return false
}
