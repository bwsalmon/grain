// Package container holds the end-to-end tests against the real
// deployment container.
//
// tests/deploy checks that the files describing the container deployment
// agree with each other. This one actually runs it: it starts the
// published image the way scripts/setup.sh's own unit starts it -- host
// networking, an unprivileged --user, the data and sandbox directories
// bind-mounted at the paths they have on the host -- and then drives the
// daemon inside through its own REST API, its CLI, and the two
// host-facing mechanisms (the reboot/restart control files and the image
// upgrade) that only exist because it is in a container at all.
//
// These are the claims a container makes that no unit test can check,
// because each one is about the boundary rather than about any code on
// either side of it: that the store survives the container it was written
// from, that files come out owned by the host account rather than by
// root, that a non-root process reaches port 80 through a file
// capability, that a request written into a mounted directory reaches a
// systemd unit outside, and that an upgrade pulls a real tag out of a
// real registry and repoints a real deployment at it.
//
// Gated, and skipped rather than failed when the gate is closed -- the
// same shape the live suites elsewhere in this repository already use for
// /dev/kvm and a GEMINI_API_KEY:
//
//	GRAIN_TEST_IMAGE  the image to test, e.g. `grain-e2e:test`. Unset
//	                  skips every test here, so `go test ./...` on a
//	                  developer's laptop stays a unit run.
//	docker            must answer `docker info`.
//
// .github/workflows/build-artifacts.yml builds the image and runs this
// against it before publishing anything, so a commit whose container does
// not come up cannot become a tag a deployment might pull.
package container

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// image is the deployment image under test. Empty means every test here
// skips.
var image = os.Getenv("GRAIN_TEST_IMAGE")

var dockerOnce struct {
	sync.Once
	works bool
}

// requireImage is the gate, asked at the top of every test rather than
// once for the package: a skip has to name which test skipped for the
// reason to be readable in a CI log.
func requireImage(t *testing.T) {
	t.Helper()
	if image == "" {
		t.Skip("GRAIN_TEST_IMAGE is unset; this suite needs a built image to drive")
	}
	dockerOnce.Do(func() {
		if _, err := exec.LookPath("docker"); err != nil {
			return
		}
		dockerOnce.works = exec.Command("docker", "info").Run() == nil
	})
	if !dockerOnce.works {
		t.Skip("docker does not answer `docker info`")
	}
}

// --- plumbing ----------------------------------------------------------

// docker runs a docker command and returns its stdout, failing the test
// with both streams when it exits non-zero.
func docker(t *testing.T, args ...string) string {
	t.Helper()
	out, err := tryDocker(args...)
	if err != nil {
		t.Fatalf("docker %s: %v", strings.Join(args, " "), err)
	}
	return out
}

// tryDocker is docker for the calls whose failure is not itself a test
// failure -- inspecting a container that may have exited, or removing one
// in a cleanup that must not mask the real error.
func tryDocker(args ...string) (string, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.Command("docker", args...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String(), fmt.Errorf("%w\n%s%s", err, stdout.String(), stderr.String())
	}
	return stdout.String(), nil
}

// freePort is a port nothing is listening on, for a daemon on the host
// network.
func freePort(t *testing.T) int {
	t.Helper()
	port, err := unusedPort()
	if err != nil {
		t.Fatalf("finding a free port: %v", err)
	}
	return port
}

// unusedPort is freePort for the registry, which is stood up once for the
// package and so has no *testing.T of its own to fail.
func unusedPort() (int, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port, nil
}

// call is one REST call against a running daemon: its status and its
// undecoded body. A transport error is returned as an error, the way the
// callers that poll for a daemon that is not up yet need it.
func call(base, method, path string, body any, timeout time.Duration) (int, []byte, error) {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		reader = bytes.NewReader(encoded)
	}

	request, err := http.NewRequest(method, base+path, reader)
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}

	response, err := (&http.Client{Timeout: timeout}).Do(request)
	if err != nil {
		return 0, nil, err
	}
	defer response.Body.Close()

	raw, err := io.ReadAll(response.Body)
	if err != nil {
		return response.StatusCode, nil, err
	}
	return response.StatusCode, raw, nil
}

// waitFor polls until check returns nil, or fails saying what never
// happened and what the last attempt said.
func waitFor(t *testing.T, what string, timeout time.Duration, check func() error) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last error
	for time.Now().Before(deadline) {
		last = check()
		if last == nil {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s: %v", timeout, what, last)
}

// --- the daemon under test ---------------------------------------------

// Daemon is a `grain daemon` running in a container, the way the unit
// runs it.
//
// The argument list mirrors scripts/setup.sh's own docker_run_args and
// write_systemd_units rather than being the smallest thing that would
// work: host networking, --user with this process's own uid:gid (the host
// account, exactly as the unit passes $GRAIN_USER's), and the
// data/sandbox directories mounted at the paths they have out here.
// Testing a container started some other way would be testing something
// no deployment runs.
type Daemon struct {
	Name    string
	Root    string
	Data    string
	Sandbox string
	Port    int
	Base    string

	uiAddr  string
	network []string
	runArgs []string
	flags   []string
}

// options are the handful of ways a test needs a daemon to differ from
// the one the unit starts.
type options struct {
	// bindPort, when set, is the port the container binds *inside*,
	// published on Daemon.Port out here -- the privileged-port case.
	bindPort int
	network  []string
	runArgs  []string
	flags    []string
}

func newDaemon(t *testing.T, root string, opts options) *Daemon {
	t.Helper()

	d := &Daemon{
		Name:    "grain-e2e-" + randomSuffix(t, 10),
		Root:    root,
		Data:    filepath.Join(root, "data"),
		Sandbox: filepath.Join(root, "sandbox"),
		Port:    freePort(t),
		runArgs: opts.runArgs,
		flags:   opts.flags,
	}
	d.Base = fmt.Sprintf("http://127.0.0.1:%d", d.Port)

	// The layout setup_data_dir lays out on a real host: a HOME that
	// exists (the unit exports one, and $GRAIN_USER has no home of its
	// own) and the secrets directory the daemon's own flags name.
	for _, dir := range []string{
		filepath.Join(d.Data, "home"),
		filepath.Join(d.Data, "secrets"),
		d.Sandbox,
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("laying out the data directory: %v", err)
		}
	}

	d.network = opts.network
	if d.network == nil {
		d.network = []string{"--network", "host"}
	}
	if opts.bindPort != 0 {
		d.uiAddr = fmt.Sprintf("0.0.0.0:%d", opts.bindPort)
		d.network = append(d.network,
			"--publish", fmt.Sprintf("127.0.0.1:%d:%d", d.Port, opts.bindPort))
	} else {
		d.uiAddr = fmt.Sprintf("127.0.0.1:%d", d.Port)
	}
	return d
}

// start brings the container up and waits for it to serve. The cleanup is
// registered before the container is started, so a daemon that never
// serves is still removed.
func start(t *testing.T, root string, opts options) *Daemon {
	t.Helper()
	d := newDaemon(t, root, opts)
	d.run(t)
	d.awaitReady(t)
	return d
}

// run starts the container without waiting for it to serve.
func (d *Daemon) run(t *testing.T) {
	t.Helper()
	t.Cleanup(d.stop)

	args := []string{"run", "--detach", "--name", d.Name}
	args = append(args, d.network...)
	args = append(args,
		"--user", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()),
		"--env", "HOME="+filepath.Join(d.Data, "home"),
		"--volume", d.Data+":"+d.Data,
		"--volume", d.Sandbox+":"+d.Sandbox,
	)
	args = append(args, d.runArgs...)
	args = append(args, image, "daemon",
		"-data-dir", d.Data,
		"-sandbox-dir", d.Sandbox,
		"-gemini-api-key-file", filepath.Join(d.Data, "secrets", "gemini-api-key"),
		"-ui-addr", d.uiAddr,
		// An hour, so the reconciler never wakes during a test. These
		// tests are about the container boundary, not about dispatch: a
		// task reaching a real agent run would need a credential and a
		// repo neither of which exists here.
		"-poll-interval", "1h",
		// A task has to name a repo or fall back to the deployment's
		// default; a deployment with neither rejects every create.
		// Nothing here ever reaches this repo -- no task is approved, so
		// none dispatches -- it just has to be a well-formed owner/name
		// the way a real deployment's own is.
		"-default-target-repo", "grain-e2e/tasks",
	)
	args = append(args, d.flags...)

	docker(t, args...)
}

// awaitReady blocks until the daemon answers, or says why it never will.
//
// A container that exited is reported immediately, with its logs, rather
// than polled for the full timeout: a daemon that refuses to start says
// so in one line, and waiting a minute to print it turns every such
// failure into a slow one.
func (d *Daemon) awaitReady(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if !d.running() {
			t.Fatalf("%s exited before serving %s:\n%s", d.Name, d.Base, d.logs())
		}
		status, _, err := call(d.Base, http.MethodGet, "/api/config", nil, 5*time.Second)
		if err == nil && status == http.StatusOK {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("%s never served %s:\n%s", d.Name, d.Base, d.logs())
}

func (d *Daemon) running() bool {
	out, _ := tryDocker("inspect", "-f", "{{.State.Running}}", d.Name)
	return strings.TrimSpace(out) == "true"
}

func (d *Daemon) logs() string {
	out, _ := tryDocker("logs", d.Name)
	if strings.TrimSpace(out) == "" {
		return "<no output>"
	}
	return out
}

func (d *Daemon) stop() {
	_, _ = tryDocker("rm", "--force", d.Name)
}

// api is one call against this daemon, failing the test on a transport
// error -- which for a daemon awaitReady has already cleared means the
// container died mid-test.
func (d *Daemon) api(t *testing.T, method, path string, body any) (int, []byte) {
	t.Helper()
	status, raw, err := call(d.Base, method, path, body, 30*time.Second)
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", method, path, err, d.logs())
	}
	return status, raw
}

// decode is the JSON half of a call, kept separate so a test that only
// cares about the status never has to name a type.
func decode(t *testing.T, raw []byte, into any) {
	t.Helper()
	if err := json.Unmarshal(raw, into); err != nil {
		t.Fatalf("decoding %s: %v", raw, err)
	}
}

// --- the shapes these tests read ----------------------------------------

type config struct {
	Actor         string            `json:"actor"`
	Capabilities  []json.RawMessage `json:"capabilities"`
	RebootEnabled bool              `json:"rebootEnabled"`
}

type task struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	State    string `json:"state"`
	Comments []struct {
		Body string `json:"body"`
	} `json:"comments"`
}

type upgradeStatus struct {
	Enabled bool   `json:"enabled"`
	Phase   string `json:"phase"`
	Detail  string `json:"detail"`
}

// createTask files a task through the API and returns it.
//
// Left unapproved (the default), so it sits in `proposed` and no
// reconciler cycle ever tries to dispatch it.
func createTask(t *testing.T, d *Daemon, title string) task {
	t.Helper()
	status, raw := d.api(t, http.MethodPost, "/api/tasks", map[string]string{
		"title":       title,
		"description": "filed by the container e2e suite",
	})
	if status != http.StatusCreated {
		t.Fatalf("creating a task: %d %s", status, raw)
	}
	var created task
	decode(t, raw, &created)
	return created
}

// randomSuffix names a container uniquely enough that a re-run over a
// half-cleaned machine does not collide.
func randomSuffix(t *testing.T, n int) string {
	t.Helper()
	suffix, err := randomHex(n)
	if err != nil {
		t.Fatalf("naming a container: %v", err)
	}
	return suffix
}

// randomHex is randomSuffix for the registry, for the same reason
// unusedPort exists.
func randomHex(n int) (string, error) {
	b := make([]byte, (n+1)/2)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b)[:n], nil
}

// errNothingYet is what a waitFor check returns while the thing it is
// waiting for simply has not happened yet -- distinct from the errors
// that describe something going wrong.
var errNothingYet = errors.New("not yet")

// filesUnder is every regular file in the tree at root, which is empty
// (and not an error) until the daemon has written its first one.
func filesUnder(root string) ([]string, error) {
	var found []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// The store directory itself does not exist until the daemon
			// creates it, which is one of the things waitFor is waiting for.
			if os.IsNotExist(err) {
				return fs.SkipAll
			}
			return err
		}
		if !d.IsDir() {
			found = append(found, path)
		}
		return nil
	})
	return found, err
}

// ownedByUs answers the question --user exists to make true: that what
// came out of the container belongs to the host account, not to root.
func ownedByUs(path string) (bool, error) {
	info, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return false, fmt.Errorf("%s: no ownership information on this platform", path)
	}
	return int(stat.Uid) == os.Getuid(), nil
}

// requireSystemd is the second gate on the host half of the control
// channel: a .path unit needs a system manager to install it into, which
// a container-based CI step may not have.
func requireSystemd(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("systemctl"); err != nil {
		t.Skip("no systemctl, so there is no .path unit to install")
	}
	// The state word rather than the exit code: `is-system-running` exits 1
	// for "degraded" -- an ordinary state for a CI runner, and still a
	// systemd that runs units -- but also for "offline", which is what a
	// machine with no systemd as PID 1 says, and there this test would
	// install a unit nothing would ever act on and then time out.
	out, _ := exec.Command("systemctl", "is-system-running").Output()
	if state := strings.TrimSpace(string(out)); !systemdStates[state] {
		t.Skipf("systemd is %q, so no .path unit here would ever fire", state)
	}
}

// systemdStates are the states a systemd that actually runs units reports
// itself in.
var systemdStates = map[string]bool{
	"running": true, "degraded": true, "maintenance": true, "starting": true,
}

// writeUnit installs one unit file, staged in a temporary file this
// account can write and copied into place as root.
func writeUnit(t *testing.T, name, body string) {
	t.Helper()
	staged := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(staged, []byte(body), 0o644); err != nil {
		t.Fatalf("staging %s: %v", name, err)
	}
	sudo(t, "cp", staged, "/etc/systemd/system/"+name)
}

// sudo runs one command as root, failing the test with its output.
func sudo(t *testing.T, args ...string) {
	t.Helper()
	out, err := exec.Command("sudo", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("sudo %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

// touch creates the file if it is missing and updates its mtime if it is
// not -- the exact thing -reboot-cmd asks for, and what PathModified
// watches for.
func touch(t *testing.T, path string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("touching %s: %v", path, err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("touching %s: %v", path, err)
	}
	now := time.Now()
	if err := os.Chtimes(path, now, now); err != nil {
		t.Fatalf("touching %s: %v", path, err)
	}
}

// countLines is how many lines of text are exactly want, which is how the
// control-channel service records each time it acted.
func countLines(text, want string) int {
	var n int
	for _, line := range strings.Split(text, "\n") {
		if strings.TrimSpace(line) == want {
			n++
		}
	}
	return n
}

// dockerGroupArgs is what lets the container's non-root uid use the
// mounted socket.
//
// setup.sh's own docker_run_args does the same lookup, for the same
// reason: /var/run/docker.sock is root:docker 0660, so a container running
// as an ordinary uid needs the group rather than the file.
func dockerGroupArgs() []string {
	group, err := user.LookupGroup("docker")
	if err != nil {
		return nil
	}
	return []string{"--group-add", group.Gid}
}
