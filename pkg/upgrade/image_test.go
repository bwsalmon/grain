package upgrade

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTagForBranch(t *testing.T) {
	cases := map[string]string{
		"main":                  "main",
		"grain/issue-645":       "grain-issue-645",
		"claude/a/b":            "claude-a-b",
		"feature.with_symbols-": "feature.with_symbols-",
	}
	for branch, want := range cases {
		if got := TagForBranch(branch); got != want {
			t.Errorf("TagForBranch(%q) = %q, want %q", branch, got, want)
		}
	}
}

// TestUpgraderImagePullsHealthChecksRecordsAndRestarts is the container
// path's counterpart to TestUpgraderStartRunsCheckoutBuildInstallAndRestart:
// no git fixture and no build at all -- just the four things an image
// upgrade does, each stubbed with a shell command that records what it
// was handed so the argv this package actually builds is asserted on
// rather than assumed.
func TestUpgraderImagePullsHealthChecksRecordsAndRestarts(t *testing.T) {
	dir := t.TempDir()
	pulled := filepath.Join(dir, "pulled")
	ran := filepath.Join(dir, "ran")
	refFile := filepath.Join(dir, "image.env")
	restartMarker := filepath.Join(dir, "restarted")

	u := New(Config{
		Image: &ImageConfig{
			Repository: "ghcr.io/bwsalmon/grain/grain",
			RefFile:    refFile,
			// Stand-ins for `docker pull` and `docker run --rm`: each
			// appends its own arguments to a file, so the test can read
			// back exactly what it was asked to pull and run.
			PullCmd: []string{"sh", "-c", `printf '%s' "$*" > ` + pulled + `; exit 0`, "sh"},
			RunCmd:  []string{"sh", "-c", `printf '%s' "$*" > ` + ran + `; exit 0`, "sh"},
		},
		HealthCheckArgs: []string{"schema-version"},
		RestartCmd:      []string{"sh", "-c", "touch " + restartMarker},
		StatusFile:      filepath.Join(dir, "upgrade-status.json"),
	})

	if err := u.Start("grain/issue-645"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	status := waitForPhase(t, u, PhaseOK)

	const wantRef = "ghcr.io/bwsalmon/grain/grain:grain-issue-645"
	if got := readFile(t, pulled); got != wantRef {
		t.Errorf("pulled %q, want %q", got, wantRef)
	}
	if got, want := readFile(t, ran), wantRef+" schema-version"; got != want {
		t.Errorf("health check ran %q, want %q", got, want)
	}
	if got, want := readFile(t, refFile), "GRAIN_IMAGE="+wantRef+"\n"; got != want {
		t.Errorf("ref file = %q, want %q", got, want)
	}
	if !strings.Contains(status.Detail, wantRef) {
		t.Errorf("status detail = %q, want it to name %q", status.Detail, wantRef)
	}
	waitForFile(t, restartMarker)
}

// TestUpgraderImagePullsTheSandboxImageTheNewBuildExpects covers the
// second half of a kontur deployment's upgrade (bwsalmon/agents#645):
// grain and the sandbox container each task's VM runs inside are built
// from one commit, so upgrading one without the other would leave the
// next dispatched task reaching for an image nothing fetched. The new
// image is asked which sandbox it expects, and that is pulled before
// anything cuts over.
func TestUpgraderImagePullsTheSandboxImageTheNewBuildExpects(t *testing.T) {
	dir := t.TempDir()
	pulled := filepath.Join(dir, "pulled")
	refFile := filepath.Join(dir, "image.env")

	u := New(Config{
		Image: &ImageConfig{
			Repository: "ghcr.io/bwsalmon/grain/grain",
			RefFile:    refFile,
			// Appends each pull to one file, so the test reads back both
			// pulls and the order they happened in.
			PullCmd: []string{"sh", "-c", `printf '%s\n' "$*" >> ` + pulled + `; exit 0`, "sh"},
			// Stands in for the image: `schema-version` (the health
			// check) prints a version, `sandbox-image` prints the
			// sandbox ref it was built against.
			RunCmd: []string{"sh", "-c",
				`case "$2" in sandbox-image) echo ghcr.io/bwsalmon/grain/kontur-sandbox:sha-abc1234;; *) echo 7;; esac`,
				"sh"},
			SandboxImageArgs: []string{"sandbox-image"},
		},
		HealthCheckArgs: []string{"schema-version"},
		StatusFile:      filepath.Join(dir, "upgrade-status.json"),
	})

	if err := u.Start("main"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	status := waitForPhase(t, u, PhaseOK)

	want := "ghcr.io/bwsalmon/grain/grain:main\nghcr.io/bwsalmon/grain/kontur-sandbox:sha-abc1234\n"
	if got := readFile(t, pulled); got != want {
		t.Errorf("pulled %q, want %q", got, want)
	}
	if !strings.Contains(status.Detail, "kontur-sandbox:sha-abc1234") {
		t.Errorf("status detail = %q, want it to name the sandbox image too", status.Detail)
	}
}

// TestUpgraderImageStopsWhenTheSandboxImageCannotBePulled: a sandbox
// image that cannot be fetched leaves the deployment where it was, the
// same as a failed health check. Cutting over anyway would produce a
// deployment that serves its UI perfectly well and fails every task it
// dispatches, which is a worse outcome than not upgrading.
func TestUpgraderImageStopsWhenTheSandboxImageCannotBePulled(t *testing.T) {
	dir := t.TempDir()
	refFile := filepath.Join(dir, "image.env")
	restartMarker := filepath.Join(dir, "restarted")

	const previous = "GRAIN_IMAGE=ghcr.io/bwsalmon/grain/grain:main\n"
	if err := os.WriteFile(refFile, []byte(previous), 0o644); err != nil {
		t.Fatal(err)
	}

	u := New(Config{
		Image: &ImageConfig{
			Repository: "ghcr.io/bwsalmon/grain/grain",
			RefFile:    refFile,
			// The grain image pulls; the sandbox image does not.
			PullCmd: []string{"sh", "-c",
				`case "$1" in *kontur-sandbox*) echo 'manifest unknown' >&2; exit 1;; esac`, "sh"},
			RunCmd: []string{"sh", "-c",
				`case "$2" in sandbox-image) echo ghcr.io/bwsalmon/grain/kontur-sandbox:sha-abc1234;; *) echo 7;; esac`,
				"sh"},
			SandboxImageArgs: []string{"sandbox-image"},
		},
		HealthCheckArgs: []string{"schema-version"},
		RestartCmd:      []string{"sh", "-c", "touch " + restartMarker},
		StatusFile:      filepath.Join(dir, "upgrade-status.json"),
	})

	if err := u.Start("feature"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	status := waitForPhase(t, u, PhaseFailed)
	if !strings.Contains(status.Detail, "sandbox image") {
		t.Errorf("status detail = %q, want it to name the sandbox image", status.Detail)
	}
	if got := readFile(t, refFile); got != previous {
		t.Errorf("ref file = %q, want it untouched at %q", got, previous)
	}
	if _, err := os.Stat(restartMarker); err == nil {
		t.Error("restart command ran despite the sandbox image never arriving")
	}
}

// TestUpgraderImageToleratesAnImageWithNoSandboxAnswer: an older grain,
// pulled by a rollback, predates the sandbox-image subcommand and answers
// with a usage error rather than a ref. That is "nothing to pull", not a
// reason to fail a rollback that is otherwise fine.
func TestUpgraderImageToleratesAnImageWithNoSandboxAnswer(t *testing.T) {
	dir := t.TempDir()
	refFile := filepath.Join(dir, "image.env")

	u := New(Config{
		Image: &ImageConfig{
			Repository: "ghcr.io/bwsalmon/grain/grain",
			RefFile:    refFile,
			PullCmd:    []string{"true"},
			RunCmd: []string{"sh", "-c",
				`case "$2" in sandbox-image) echo 'flag provided but not defined' >&2; exit 2;; *) echo 7;; esac`,
				"sh"},
			SandboxImageArgs: []string{"sandbox-image"},
		},
		HealthCheckArgs: []string{"schema-version"},
		StatusFile:      filepath.Join(dir, "upgrade-status.json"),
	})

	if err := u.Start("v-old"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitForPhase(t, u, PhaseOK)
	if got, want := readFile(t, refFile),
		"GRAIN_IMAGE=ghcr.io/bwsalmon/grain/grain:v-old\n"; got != want {
		t.Errorf("ref file = %q, want %q", got, want)
	}
}

// TestUpgraderImageLeavesRefFileAloneWhenHealthCheckFails is the
// image path's answer to TestUpgraderRollsBackWhenHealthCheckFails: it
// has no rollback because it never cuts over in the first place. A
// pulled image that cannot even run `schema-version` must leave the ref
// file exactly as it was -- so the restart that never happens would have
// brought up the same image the deployment already runs.
func TestUpgraderImageLeavesRefFileAloneWhenHealthCheckFails(t *testing.T) {
	dir := t.TempDir()
	refFile := filepath.Join(dir, "image.env")
	restartMarker := filepath.Join(dir, "restarted")

	const previous = "GRAIN_IMAGE=ghcr.io/bwsalmon/grain/grain:main\n"
	if err := os.WriteFile(refFile, []byte(previous), 0o644); err != nil {
		t.Fatal(err)
	}

	u := New(Config{
		Image: &ImageConfig{
			Repository: "ghcr.io/bwsalmon/grain/grain",
			RefFile:    refFile,
			PullCmd:    []string{"true"},
			// A "container" that starts and exits 1, standing in for an
			// image whose binary is broken outright.
			RunCmd: []string{"false"},
		},
		HealthCheckArgs: []string{"schema-version"},
		// Should never run: a failed health check stops this before the
		// deployment is ever pointed at the new image.
		RestartCmd: []string{"sh", "-c", "touch " + restartMarker},
		StatusFile: filepath.Join(dir, "upgrade-status.json"),
	})

	if err := u.Start("feature"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	status := waitForPhase(t, u, PhaseFailed)
	if !strings.Contains(status.Detail, "health check") {
		t.Errorf("status detail = %q, want it to name the health check", status.Detail)
	}
	if got := readFile(t, refFile); got != previous {
		t.Errorf("ref file = %q, want it untouched at %q", got, previous)
	}
	if _, err := os.Stat(restartMarker); err == nil {
		t.Error("restart command ran despite the failed health check")
	}
}

// TestUpgraderImageFailsWhenPullFails covers the other stop: a branch
// with no image published for it (a push whose build-artifacts run has
// not finished, or a typo) fails at the pull, before the health check
// and before the ref file.
func TestUpgraderImageFailsWhenPullFails(t *testing.T) {
	dir := t.TempDir()
	refFile := filepath.Join(dir, "image.env")

	u := New(Config{
		Image: &ImageConfig{
			Repository: "ghcr.io/bwsalmon/grain/grain",
			RefFile:    refFile,
			PullCmd:    []string{"sh", "-c", "echo 'manifest unknown' >&2; exit 1", "sh"},
		},
		HealthCheckArgs: []string{"schema-version"},
		StatusFile:      filepath.Join(dir, "upgrade-status.json"),
	})

	if err := u.Start("no-such-branch"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	status := waitForPhase(t, u, PhaseFailed)
	if !strings.Contains(status.Detail, "pull") || !strings.Contains(status.Detail, "manifest unknown") {
		t.Errorf("status detail = %q, want it to name the failed pull and its output", status.Detail)
	}
	if _, err := os.Stat(refFile); !os.IsNotExist(err) {
		t.Errorf("ref file exists after a failed pull (err=%v)", err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return string(data)
}
