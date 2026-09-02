package installer

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// deployment is one real setup.sh run, and everything it left behind.
//
// Built once for the package and torn down by TestMain: the run is the
// expensive part and every test here asks a different question of the same
// result, the way an operator would look over one finished deploy rather
// than doing five.
type deployment struct {
	setup   result
	root    string
	data    string
	sandbox string
	src     string
	user    string
	port    int
	image   string
}

var deployOnce struct {
	sync.Once
	d   *deployment
	err error
}

// deployed is the fixture: the finished deploy, stood up on first use.
func deployed(t *testing.T) *deployment {
	t.Helper()
	deployOnce.Do(func() { deployOnce.d, deployOnce.err = install() })
	if deployOnce.err != nil {
		t.Fatalf("running the installer: %v", deployOnce.err)
	}
	return deployOnce.d
}

func install() (*deployment, error) {
	root, err := os.MkdirTemp("", "grain-installer-e2e-")
	if err != nil {
		return nil, err
	}
	// setup.sh's own units read these paths as $GRAIN_USER; MkdirTemp
	// makes the directory this account's alone.
	if err := os.Chmod(root, 0o755); err != nil {
		return nil, err
	}

	port, err := freePort()
	if err != nil {
		return nil, err
	}

	d := &deployment{
		root:    root,
		data:    filepath.Join(root, "data"),
		sandbox: filepath.Join(root, "sandbox"),
		src:     filepath.Join(root, "src"),
		user:    "grain-e2e",
		port:    port,
		image:   image,
	}

	checkout, err := repoRootFromCaller()
	if err != nil {
		return nil, err
	}

	// The script under test, put on this host the way a deploy puts it
	// there -- and nothing else, because nothing else is read from
	// beside it any more. setup.sh keeps no checkout of its own: it
	// needs nothing on a host but docker and systemd, and the source
	// its kontur step wants comes out of the deployment image. A copy
	// rather than a clone for the same reason: there is no repository
	// here for it to be part of.
	script, err := os.ReadFile(filepath.Join(checkout, "scripts", "setup.sh"))
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(d.src, "scripts"), 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(d.src, "scripts", "setup.sh"), script, 0o755); err != nil {
		return nil, err
	}

	// A stand-in for the GCP minter credential push-secrets.sh pushes. Its
	// *contents* never matter -- nothing here authenticates to GCP -- but
	// its presence is what makes setup.sh stage it for the containerised
	// CLI, which is the step that broke.
	minter := filepath.Join(root, "minter-key.json")
	if err := os.WriteFile(minter,
		[]byte(`{"type": "service_account", "project_id": "fake"}`), 0o644); err != nil {
		return nil, err
	}

	repo, tag, ok := splitImage(image)
	if !ok {
		return nil, fmt.Errorf("GRAIN_TEST_IMAGE=%q names no tag to deploy", image)
	}

	env := [][2]string{
		{"GRAIN_DATA_DIR", d.data},
		{"GRAIN_SANDBOX_DIR", d.sandbox},
		{"GRAIN_USER", d.user},
		{"GRAIN_IMAGE", repo},
		{"GRAIN_IMAGE_TAG", tag},
		{"GRAIN_UI_ADDR", fmt.Sprintf("127.0.0.1:%d", d.port)},
		// Off: the only part of a deploy needing nested virtualisation and
		// a debootstrap, and not where this file's failures live.
		{"GRAIN_KONTUR_ENABLE", "0"},
		// On: it is the default, and it is what mounts the docker socket
		// and wires the control units.
		{"GRAIN_ENABLE_UI_UPGRADE", "1"},
		{"GRAIN_GCP_SERVICE_ACCOUNT_KEY_FILE", minter},
		// Deliberately empty: a project set would send
		// mint_gemini_operating_key at the real GCP API.
		{"GRAIN_GCP_PROJECT", ""},
		{"GRAIN_GITHUB_TOKEN", "ghp_fake_token_for_the_installer_e2e"},
		// Empty: format_target_repo_if_empty would otherwise `git
		// ls-remote` (in the deployment image, but against the real
		// GitHub) with that fake token.
		{"GRAIN_TARGET_REPO", ""},
		{"PATH", pathOrDefault()},
	}

	argv := []string{"env"}
	for _, pair := range env {
		argv = append(argv, pair[0]+"="+pair[1])
	}
	argv = append(argv, "bash", filepath.Join(d.src, "scripts", "setup.sh"))

	d.setup = sudo(argv...)
	return d, nil
}

// teardown puts the host back: every unit this deploy installed, the
// container it started, the wrappers it wrote, and the account it created.
func (d *deployment) teardown() {
	for _, unit := range []string{
		"grain-daemon.service", "grain-reboot.path", "grain-restart.path",
		"grain-reboot.service", "grain-restart.service",
	} {
		sudo("systemctl", "disable", "--now", unit)
		sudo("rm", "-f", "/etc/systemd/system/"+unit)
	}
	sudo("systemctl", "daemon-reload")
	sudo("docker", "rm", "--force", "grain-daemon")
	sudo("rm", "-rf", "/usr/local/lib/grain", "/usr/local/bin/grain",
		"/usr/local/bin/konturctl", "/etc/profile.d/grain.sh")
	sudo("userdel", d.user)
	sudo("rm", "-rf", d.root)
}

func TestMain(m *testing.M) {
	code := m.Run()
	if deployOnce.d != nil {
		deployOnce.d.teardown()
	}
	os.Exit(code)
}

// The whole point: it has to *finish*.
//
// Every failure this file exists for looked like a partial deploy -- the
// script exiting mid-way under `set -e`, leaving a host with some of a
// deployment on it and no service. The exit code is the assertion; the
// output is what makes a failure readable without a re-run.
func TestSetupRunsToCompletion(t *testing.T) {
	requireInstaller(t)
	d := deployed(t)

	if d.setup.exitCode != 0 {
		t.Fatalf("setup.sh exited %d\n--- last 4000 chars of stdout ---\n%s\n"+
			"--- last 2000 chars of stderr ---\n%s",
			d.setup.exitCode, tail(d.setup.stdout, 4000), tail(d.setup.stderr, 2000))
	}
}

// `systemctl is-active grain-daemon.service` -- the missing piece.
//
// This is the observation that started it: a host with the config-sync
// service, the image, and no daemon unit. Asked of systemd rather than of
// the filesystem, because a unit file that exists and will not start is
// the same outcome for an operator.
func TestTheDaemonServiceExistsAndIsRunning(t *testing.T) {
	requireInstaller(t)
	deployed(t)

	state := strings.TrimSpace(sudo("systemctl", "is-active", "grain-daemon.service").stdout)
	if state != "active" {
		journal := sudo("journalctl", "-u", "grain-daemon.service", "-n", "50", "--no-pager").stdout
		t.Fatalf("grain-daemon.service is %q:\n%s", state, journal)
	}
}

// A running unit is not the same as a working deployment.
//
// The container has to have come up with mounts it can actually use -- the
// data directory it writes its store into, a HOME that exists -- and to be
// reachable on the address the unit gave it.
func TestTheDeploymentServesItsAPI(t *testing.T) {
	requireInstaller(t)
	d := deployed(t)

	base := fmt.Sprintf("http://127.0.0.1:%d", d.port)
	var actor string
	waitFor(t, "the deployment to serve "+base+"/api/config", 90*time.Second, func() error {
		response, err := (&http.Client{Timeout: 5 * time.Second}).Get(base + "/api/config")
		if err != nil {
			return err
		}
		defer response.Body.Close()

		var config struct {
			Actor string `json:"actor"`
		}
		if err := json.NewDecoder(response.Body).Decode(&config); err != nil {
			return err
		}
		if config.Actor == "" {
			return fmt.Errorf("the deployment reports no actor")
		}
		actor = config.Actor
		return nil
	})
	t.Logf("the deployment files work as %q", actor)
}

// The exact failure, asserted on its result rather than its shape.
//
// setup.sh stages this credential under the data directory and hands the
// path to `grain secrets set`, which runs as $GRAIN_USER inside a
// container. Staged as root at 0600 -- the obvious thing for a script
// running as root to do -- that CLI cannot read it, the command fails, and
// `set -e` ends the deploy before the unit is ever written.
//
// `grain secrets list` naming it is the proof the whole path worked:
// staged readably, read inside the container, and written into a store
// that is itself owned correctly.
func TestTheMinterCredentialReachedTheSecretsDatabase(t *testing.T) {
	requireInstaller(t)
	d := deployed(t)

	listed := sudo("/usr/local/bin/grain", "secrets", "-data-dir", d.data, "list")
	if !strings.Contains(listed.stdout, "gcp-key-minter") {
		t.Fatalf("the minter credential never landed:\n%v", listed)
	}
	// And the staging copy does not outlive the command that needed it.
	staged := filepath.Join(d.data, "secrets", ".minter-key.staged.json")
	if sudoTest("-e", staged) {
		t.Errorf("%s outlived the command that needed it", staged)
	}
}

// Root ran the installer; the daemon is not root.
//
// Every path the container writes through has to come out owned by
// $GRAIN_USER, or the next thing to touch it -- the daemon itself, an
// operator's `grain` -- fails on permissions. This is the general form of
// the minter-key bug above.
func TestWhatTheDeploymentWritesIsOwnedByTheServiceAccount(t *testing.T) {
	requireInstaller(t)
	d := deployed(t)

	account, err := user.Lookup(d.user)
	if err != nil {
		t.Fatalf("setup.sh did not create %s: %v", d.user, err)
	}
	uid, err := strconv.Atoi(account.Uid)
	if err != nil {
		t.Fatalf("%s has uid %q: %v", d.user, account.Uid, err)
	}

	for _, path := range []string{
		d.data,
		filepath.Join(d.data, "secrets"),
		filepath.Join(d.data, "home"),
		filepath.Join(d.data, "image.env"),
	} {
		if !sudoTest("-e", path) {
			t.Errorf("%s was never created", path)
			continue
		}
		if got := sudoUID(t, path); got != uid {
			t.Errorf("%s is owned by uid %d, not %s (%d)", path, got, d.user, uid)
		}
	}

	// The flip side, and the reason every check above needs sudo: the data
	// directory is not readable by an arbitrary account on the host.
	//
	// Only worth asking as an ordinary account. Root bypasses file
	// permissions by definition, so under `sudo go test` this would be
	// asserting something about root rather than about the deployment --
	// and failing for a reason that says nothing about setup.sh.
	if os.Geteuid() == 0 {
		t.Log("running as root, so the unreadability of the data directory says nothing here")
		return
	}
	if _, err := os.ReadDir(d.data); err == nil {
		t.Errorf("%s is readable by this account -- the store and the secrets "+
			"database are supposed to be the service account's alone", d.data)
	}
}

// The image ref file is the indirection an upgrade rewrites.
//
// The unit reads it as an EnvironmentFile, so what it names *is* what the
// deployment runs -- and setup.sh writing it is what makes a re-run with a
// different GRAIN_IMAGE_TAG a rollback.
func TestTheUnitRunsTheImageThisDeployWasGiven(t *testing.T) {
	requireInstaller(t)
	d := deployed(t)

	ref := strings.TrimSpace(sudoRead(filepath.Join(d.data, "image.env")))
	if want := "GRAIN_IMAGE=" + d.image; ref != want {
		t.Fatalf("the ref file reads %q, want %q", ref, want)
	}

	unit := sudoRead("/etc/systemd/system/grain-daemon.service")
	if !strings.Contains(unit, "docker") || !strings.Contains(unit, "run --name grain-daemon") {
		t.Errorf("the unit does not run the image:\n%s", unit)
	}
	if want := "EnvironmentFile=" + filepath.Join(d.data, "image.env"); !strings.Contains(unit, want) {
		t.Errorf("the unit does not read %s:\n%s", want, unit)
	}
}

// The daemon's only way to reach the host it runs on.
//
// The reboot button and the Upgrade button's restart are both a touch of a
// file under the data directory; these are the units that turn that into a
// command. Enabled *and* active: a .path unit that exists but is not
// running is watching nothing.
func TestTheHostControlUnitsAreInstalledAndWatching(t *testing.T) {
	requireInstaller(t)
	d := deployed(t)

	for _, unit := range []string{"grain-reboot.path", "grain-restart.path"} {
		state := strings.TrimSpace(sudo("systemctl", "is-active", unit).stdout)
		if state != "active" {
			t.Errorf("%s is %q, so nothing is watching for requests", unit, state)
		}
	}
	control := filepath.Join(d.data, "control")
	if !sudoTest("-d", control) {
		t.Errorf("%s was never created", control)
	}
}

// `grain list` on the host, against the daemon on the host.
//
// The wrapper bakes in GRAIN_SERVER from the unit's own -ui-addr, so this
// exercises the one thing an operator does first -- and would catch a
// wrapper pointed at cmd/grain's 8420 default instead.
func TestTheCLIWrapperTalksToTheDeploymentItInstalled(t *testing.T) {
	requireInstaller(t)
	deployed(t)

	listed := sudo("/usr/local/bin/grain", "-json", "list")
	if listed.exitCode != 0 {
		t.Fatalf("the wrapper does not reach the deployment:\n%v", listed)
	}

	body := strings.TrimSpace(listed.stdout)
	if body == "" {
		body = "[]"
	}
	var tasks []json.RawMessage
	if err := json.Unmarshal([]byte(body), &tasks); err != nil {
		t.Fatalf("`grain -json list` printed %q: %v", body, err)
	}
	if len(tasks) != 0 {
		t.Fatalf("a fresh deployment already has %d tasks: %s", len(tasks), body)
	}
}

// --- small helpers -------------------------------------------------------

// splitImage separates the repository from the tag, which setup.sh takes
// as two variables rather than one reference.
func splitImage(ref string) (repo, tag string, ok bool) {
	at := strings.LastIndex(ref, ":")
	if at <= 0 || strings.Contains(ref[at+1:], "/") {
		return "", "", false
	}
	return ref[:at], ref[at+1:], true
}

// tail is the last n characters of s, which is how a failing deploy's
// output is quoted: the end of it is where the error is.
func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

func pathOrDefault() string {
	if path := os.Getenv("PATH"); path != "" {
		return path
	}
	return "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
}
