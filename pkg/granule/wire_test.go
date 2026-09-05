package granule_test

import (
	"encoding/json"
	"path"
	"strings"
	"testing"
	"time"

	"github.com/bwsalmon/grain/pkg/granule"
)

// The wire format is a contract between two separately released
// artifacts -- the daemon binary and the sandbox image -- so it is worth
// pinning as literal JSON rather than only round-tripping. A field
// renamed by a careless refactor round-trips perfectly and still breaks
// every deployment whose image is a version behind.

func marshal(t *testing.T, v any) string {
	t.Helper()
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatalf("marshalling: %v", err)
	}
	return string(b)
}

func TestSpecEnvAndFiles(t *testing.T) {
	spec := granule.Spec{
		Version:    granule.Version,
		Framework:  granule.FrameworkSpec{Name: "claude", Credential: "sk-ant-oat01-..."},
		Shape:      granule.Shape{CPUs: 2, MemoryMB: 8192, DiskGB: 30},
		Prompt:     "You are working on task-311...\n",
		Setup:      "#!/bin/sh\nset -eu\ngit clone http://10.0.2.1:8080/bwsalmon/granule.git /w\ncd /w && ./scripts/setup.sh\ngit rev-parse HEAD\n",
		Placements: []granule.Placement{{Path: "/home/agent/.git-credentials", Content: "https://x:tok@10.0.2.1:8080"}},
		MaxRuntime: granule.Duration(2 * time.Hour),
	}

	// Scalars only: no material in the environment at all.
	wantEnv := map[string]string{
		"GRAIN_WIRE_VERSION": "v1",
		"GRAIN_FRAMEWORK":    "claude",
		"GRAIN_MAX_RUNTIME":  "2h0m0s",
		// kontur's own, passed through and never read back.
		"CHV_CPUS":         "2",
		"CHV_MEMORY_MB":    "8192",
		"CHV_DISK_SIZE_MB": "30720",
	}
	env := spec.Env()
	if len(env) != len(wantEnv) {
		t.Fatalf("Env produced %d variables, want %d:\n%#v", len(env), len(wantEnv), env)
	}
	for k, v := range wantEnv {
		if env[k] != v {
			t.Errorf("%s = %q, want %q", k, env[k], v)
		}
	}
	for k, v := range env {
		if strings.Contains(v, "sk-ant") || strings.Contains(v, "tok@") {
			t.Errorf("%s carries material: the environment is scalars only", k)
		}
	}

	files, err := spec.Files()
	if err != nil {
		t.Fatalf("Files: %v", err)
	}
	wantFiles := map[string]granule.File{
		"/grain/credential": {Content: "sk-ant-oat01-...", Mode: "0600"},
		"/grain/prompt":     {Content: spec.Prompt, Mode: "0644"},
		"/grain/setup":      {Content: spec.Setup, Mode: "0755"},
		"/grain/placements/home/agent/.git-credentials": {
			Content: "https://x:tok@10.0.2.1:8080", Mode: "0600",
		},
	}
	if len(files) != len(wantFiles) {
		t.Fatalf("Files produced %d entries, want %d:\n%#v", len(files), len(wantFiles), files)
	}
	for at, want := range wantFiles {
		got, ok := files[at]
		if !ok {
			t.Errorf("no file at %s", at)
			continue
		}
		if got != want {
			t.Errorf("%s = %+v, want %+v", at, got, want)
		}
	}

	back, err := granule.SpecFromEnv(func(k string) string { return env[k] })
	if err != nil {
		t.Fatalf("SpecFromEnv: %v", err)
	}
	if time.Duration(back.MaxRuntime) != 2*time.Hour || back.Framework.Name != "claude" {
		t.Errorf("scalars round-tripped as %+v", back)
	}
	// The material half is files, so it is never recovered from env --
	// which is also why an error from there can never quote it.
	if back.Framework.Credential != "" || back.Setup != "" || len(back.Placements) != 0 {
		t.Errorf("SpecFromEnv recovered material it should never see: %+v", back.Redacted())
	}
	if !back.Shape.IsZero() {
		t.Errorf("SpecFromEnv read shape back as %#v; those variables are kontur's", back.Shape)
	}
}

// The placements tree *is* the mapping from container path to guest path,
// so it has to round-trip exactly -- and it has to refuse anything that
// would escape the tree, since that is the one part of this contract that
// names a location.
func TestPlacementPathContainment(t *testing.T) {
	for _, guest := range []string{"/home/agent/.netrc", "/etc/pip.conf", "/a"} {
		at, err := granule.PlacementPath(guest)
		if err != nil {
			t.Fatalf("PlacementPath(%q): %v", guest, err)
		}
		if !strings.HasPrefix(at, granule.DirPlacements+"/") {
			t.Errorf("PlacementPath(%q) = %q, outside %s", guest, at, granule.DirPlacements)
		}
		back, err := granule.GuestPath(at)
		if err != nil {
			t.Fatalf("GuestPath(%q): %v", at, err)
		}
		if back != guest {
			t.Errorf("round trip: %q -> %q -> %q", guest, at, back)
		}
	}

	for _, bad := range []string{
		"",                    // no path at all
		"home/agent/.netrc",   // relative
		"/a/../../etc/shadow", // escapes the tree
		"/a/./b",              // not in simplest form
		"/a//b",               // ditto
	} {
		if at, err := granule.PlacementPath(bad); err == nil {
			t.Errorf("PlacementPath(%q) = %q, want an error", bad, at)
		}
	}

	// A stray file the shim finds under the tree is refused on the way
	// out too: it walks a directory somebody else mounted.
	if _, err := granule.GuestPath("/grain/placements/a/../../../etc/shadow"); err == nil {
		t.Error("GuestPath accepted a path escaping the placements tree")
	}
	if _, err := granule.GuestPath("/somewhere/else"); err == nil {
		t.Error("GuestPath accepted a path outside the placements tree")
	}
}

// Two placements landing at one path means the controller composed
// something wrong; writing one of them silently is worse than writing
// neither and saying so.
func TestFilesRefusesClashingPlacements(t *testing.T) {
	_, err := granule.Spec{
		Version: granule.Version,
		Placements: []granule.Placement{
			{Path: "/home/agent/.netrc", Content: "a"},
			{Path: "/home/agent/.netrc", Content: "b"},
		},
	}.Files()
	if err == nil {
		t.Fatal("Files accepted two placements at one path")
	}
}

// A shim that half-understands its environment starts an agent that does
// the wrong thing quietly. Refusing costs one run and names the
// disagreement.
func TestSpecFromEnvRefusesAnotherVersion(t *testing.T) {
	// Deliberately not "v2": tests/deploy's own
	// TestNoSourceFileStillRefersToTheV2Subdirectory forbids that
	// segment anywhere in the tree, because v1's removal moved the Go
	// tree out of v2/ and a survivor is a file-not-found at run time
	// rather than a compile error. A fake version has no business
	// spending that word.
	const other = "v7"
	_, err := granule.SpecFromEnv(func(k string) string {
		if k == granule.EnvVersion {
			return other
		}
		return ""
	})
	if err == nil {
		t.Fatalf("SpecFromEnv accepted wire version %s", other)
	}
	if !strings.Contains(err.Error(), other) || !strings.Contains(err.Error(), granule.Version) {
		t.Errorf("error should name both versions, got: %v", err)
	}

	if _, err := granule.SpecFromEnv(func(string) string { return "" }); err == nil {
		t.Fatal("SpecFromEnv accepted an environment with no version at all")
	}
}

// The Spec deliberately carries no task, repository, branch, git
// credential or capability model: a grain runs an agent in a sandbox and
// knows nothing about why. Everything task-shaped reaches it as a prompt,
// as the setup script, or as a placement.
//
// Asserted on the rendered environment rather than the type, because
// what matters is that none of it crosses between two separately
// released artifacts -- and because a field added back would round-trip
// perfectly and still put grain's task model into the sandbox image's
// contract.
func TestGrainEnvCarriesNoTaskModel(t *testing.T) {
	full := marshal(t, granule.Spec{
		Version:   granule.Version,
		Framework: granule.FrameworkSpec{Name: "claude", Credential: "sk-ant-oat01-..."},
		Shape:     granule.Shape{CPUs: 2}, Setup: "true",
		Placements: []granule.Placement{{Path: "/p", Mode: "0600"}},
		MaxRuntime: granule.Duration(time.Hour),
	})
	for _, absent := range []string{
		"\"task\"", "\"repo\"", "\"branch\"", "\"base\"", "\"target\"",
		"\"gitToken\"", "\"grants\"", "\"proxyBase\"", "\"attempt\"", "\"maxTurns\"",
		// The container is the identity; a grain is never told a name it
		// makes no use of.
		"\"id\"",
		// Every capability grain has places into the sandbox, and
		// model.SideController has never had a producer, so a placement
		// needs no side to land on.
		"\"dest\"",
	} {
		if strings.Contains(full, absent) {
			t.Errorf("Spec carries %s; a grain has no use for it "+
				"(see the Spec doc comment for where it belongs instead)", absent)
		}
	}
}

func TestStatusWireFormat(t *testing.T) {
	st := granule.Status{
		Version:  granule.Version,
		ID:       "task-311-2", // set by the backend, never on the wire
		Phase:    granule.PhaseRunning,
		Since:    time.Date(2026, 9, 4, 19, 41, 12, 0, time.UTC),
		Activity: "waiting for CI",
		Rebuilds: 1,
		Setup:    &granule.SetupResult{Output: "9f3c1a2\n"},
		Health: granule.Health{
			Container: granule.ContainerHealth{Running: true},
			Guest: granule.GuestHealth{
				Ready: true, LoadAverage: "0.41 0.30 0.22",
				ConntrackCount: 812, ConntrackMax: 262144,
			},
		},
		Seq: 4471,
	}

	want := `{
  "version": "v1",
  "phase": "running",
  "since": "2026-09-04T19:41:12Z",
  "activity": "waiting for CI",
  "rebuilds": 1,
  "setup": {
    "exitCode": 0,
    "output": "9f3c1a2\n"
  },
  "health": {
    "guest": {
      "ready": true,
      "loadAverage": "0.41 0.30 0.22",
      "conntrackCount": 812,
      "conntrackMax": 262144
    }
  },
  "seq": 4471
}`
	if got := marshal(t, st); got != want {
		t.Fatalf("Status wire format changed.\n got:\n%s\nwant:\n%s", got, want)
	}
}

// A record is one line, always: a log stream is line-oriented, and a
// value that spans lines cannot survive a reader joining mid-stream or a
// rotation taking the first half of it.
func TestRecordIsOneLine(t *testing.T) {
	rec := granule.Record{
		Version: granule.Version, Seq: 42,
		T:    time.Date(2026, 9, 4, 19, 55, 3, 0, time.UTC),
		Src:  granule.SrcConsole,
		Data: json.RawMessage(`"[    0.512] EXT4-fs (vda): mounted"`),
	}
	b, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("marshalling: %v", err)
	}
	if strings.ContainsAny(string(b), "\n\r") {
		t.Fatalf("record spans lines: %s", b)
	}
	want := `{"version":"v1","seq":42,"t":"2026-09-04T19:55:03Z","src":"console","data":"[    0.512] EXT4-fs (vda): mounted"}`
	if string(b) != want {
		t.Fatalf("Record wire format changed.\n got: %s\nwant: %s", b, want)
	}
}

// A duration that cannot be parsed must say so rather than silently
// becoming zero, which would read downstream as "no limit".
func TestDurationRejectsNonsense(t *testing.T) {
	var d granule.Duration
	if err := json.Unmarshal([]byte(`"two hours"`), &d); err == nil {
		t.Fatal("parsing \"two hours\" succeeded, want an error")
	}
	if err := json.Unmarshal([]byte(`0`), &d); err != nil {
		t.Fatalf("a bare 0 should be accepted as no limit: %v", err)
	}
}

// A Spec exists to move credentials, so "never log it" cannot be a rule
// everyone remembers. Redacted is the enforceable version, and this is
// what stops a later field from quietly escaping it.
func TestRedactedCarriesNoMaterial(t *testing.T) {
	spec := granule.Spec{
		Version:   granule.Version,
		Framework: granule.FrameworkSpec{Name: "claude", Credential: "sk-ant-oat01-secret"},
		Setup:     "git clone http://10.0.2.1:8080/bwsalmon/granule.git /w",
		Placements: []granule.Placement{
			{Path: "/home/agent/.git-credentials", Content: "https://x:sbx_tok@10.0.2.1:8080", Mode: "0600"},
		},
	}
	out := marshal(t, spec.Redacted())

	for _, secret := range []string{"sk-ant-oat01-secret", "https://x:sbx_tok@10.0.2.1:8080"} {
		if strings.Contains(out, secret) {
			t.Errorf("Redacted still carries %q", secret)
		}
	}
	// Lengths survive, because "arrived empty" is the common failure and
	// a blank string cannot say so.
	if !strings.Contains(out, "[redacted, 19 bytes]") {
		t.Errorf("Redacted dropped the credential's length:\n%s", out)
	}
	// The original is untouched -- a redaction that mutated its receiver
	// would blank the spec about to be sent.
	if spec.Framework.Credential != "sk-ant-oat01-secret" {
		t.Error("Redacted mutated the spec it was called on")
	}

	// Setup is deliberately NOT redacted, and that is only safe because a
	// setup script must never embed a credential -- the clone URL above
	// carries none, and git finds the token in the placement beside it.
	// This is the rule that makes git's credential a placement rather
	// than something interpolated into the script: a secret in Setup
	// would survive Redacted, and Setup is exactly the field you need
	// unblanked to diagnose a failed run.
	if !strings.Contains(out, "git clone http://10.0.2.1:8080/bwsalmon/granule.git") {
		t.Error("Redacted blanked setup, which is diagnosis rather than material")
	}
	if strings.Contains(spec.Setup, "@") {
		t.Error("the setup script embeds credentials in a URL; they belong in a placement")
	}
}

// A status record carries a whole Status, because that is what lets a
// controller read a grain off the log stream it already tails. Pinned as
// literal JSON for the same reason every other document is: the reader
// and the writer ship in different artifacts.
func TestStatusRidesTheRecordStream(t *testing.T) {
	st := granule.Status{
		Version: granule.Version, Phase: granule.PhaseRunning,
		Since:    time.Date(2026, 9, 4, 19, 41, 12, 0, time.UTC),
		Activity: "running the test suite", Seq: 4471,
	}
	body, err := json.Marshal(st)
	if err != nil {
		t.Fatalf("marshalling status: %v", err)
	}
	rec := granule.Record{
		Version: granule.Version, Seq: st.Seq,
		T:    time.Date(2026, 9, 4, 19, 41, 12, 0, time.UTC),
		Src:  granule.SrcShim,
		Kind: granule.KindStatus,
		Data: body,
	}
	line, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("marshalling record: %v", err)
	}
	if strings.ContainsAny(string(line), "\n\r") {
		t.Fatalf("a status record spans lines: %s", line)
	}

	// A reader tailing the stream gets the Status back whole -- it is a
	// snapshot, not a delta, which is what keeps Reconcile level-triggered
	// when it is fed from here instead of from an exec.
	var back granule.Record
	if err := json.Unmarshal(line, &back); err != nil {
		t.Fatalf("reading the record back: %v", err)
	}
	if back.Kind != granule.KindStatus || back.Src != granule.SrcShim {
		t.Fatalf("record round-tripped as src=%q kind=%q", back.Src, back.Kind)
	}
	var got granule.Status
	if err := json.Unmarshal(back.Data, &got); err != nil {
		t.Fatalf("reading the status back: %v", err)
	}
	if got.Phase != granule.PhaseRunning || got.Activity != st.Activity || got.Seq != st.Seq {
		t.Errorf("status round-tripped as %+v", got)
	}
}

func keys(m map[string]granule.File) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// Nothing outside the sandbox is a tool. A deployment's escape hatches --
// opening a pull request, asking a human -- are a CLI in the guest
// and a credential in a placement, run with run_command like anything
// else, so this package holds no vocabulary for any of them and no idea
// that a controller exists.
//
// Asserted on the rendered environment and files together, because the
// failure this guards against is a later change quietly reintroducing a
// controller address or a controller credential on the grain's own wire.
func TestAGrainCannotReachAControllerAtAll(t *testing.T) {
	spec := granule.Spec{
		Version:   granule.Version,
		Framework: granule.FrameworkSpec{Name: "claude", Credential: "sk-ant-oat01-..."},
		Setup:     "true",
		Placements: []granule.Placement{
			// A run's controller credential is one of these, exactly as
			// its git credential is -- and grain cannot tell them apart,
			// which is the property.
			{Path: "/etc/grain/controller", Content: "url=… token=…", Mode: "0600"},
		},
	}
	env := spec.Env()
	for k := range env {
		if strings.Contains(k, "CONTROLLER") || strings.Contains(k, "TOKEN") || strings.Contains(k, "URL") {
			t.Errorf("the grain environment carries %s: a grain has no controller to reach", k)
		}
	}
	files, err := spec.Files()
	if err != nil {
		t.Fatalf("Files: %v", err)
	}
	for at := range files {
		if at == "/grain/token" {
			t.Error("a controller token is being placed container-side; it belongs in the guest, as a placement, like git's")
		}
	}
	// The one it does place is the agent's own, which lives beside the
	// agent because the agent is what uses it.
	if _, ok := files[granule.FileCredential]; !ok {
		t.Error("the model credential is missing; that one is the grain's own business")
	}
}

// A grain cannot report the two things only the outside knows: which
// grain it is, and whether it can be reached. Both are filled in by the
// backend while it merges the runtime's listing with the log stream
// (Grains.List), and both must stay off the wire -- a grain that answered
// "container: running" would be stating the one fact its answering
// already proved, and a grain that could not answer could not correct it.
//
// This is what removing the status exec rests on: with no call into a
// shim there is no second opinion to reconcile, so the split between what
// a grain says and what the runtime says has to be exact.
func TestAGrainReportsNeitherItsNameNorItsReachability(t *testing.T) {
	st := granule.Status{
		Version: granule.Version, Phase: granule.PhaseRunning,
		Since: time.Date(2026, 9, 4, 19, 41, 12, 0, time.UTC),
		ID:    "task-311-1",
		Health: granule.Health{
			Container: granule.ContainerHealth{Running: true, Err: "unreachable"},
			Guest:     granule.GuestHealth{Ready: true},
		},
	}
	body, err := json.Marshal(st)
	if err != nil {
		t.Fatalf("marshalling status: %v", err)
	}
	for _, leaked := range []string{"task-311-1", "unreachable"} {
		if strings.Contains(string(body), leaked) {
			t.Errorf("a status carries %q on the wire: %s", leaked, body)
		}
	}
	// Asserted on the shape and not only on the values: an empty
	// "container": {} would leak no string while still putting the
	// question on the wire for something to start answering.
	var shape struct {
		Health map[string]json.RawMessage `json:"health"`
	}
	if err := json.Unmarshal(body, &shape); err != nil {
		t.Fatalf("reading the health object back: %v", err)
	}
	if _, ok := shape.Health["container"]; ok {
		t.Errorf("health carries a container object: %s", body)
	}
	if len(shape.Health) != 1 {
		t.Errorf("health has %d members, want just guest: %s", len(shape.Health), body)
	}

	var back granule.Status
	if err := json.Unmarshal(body, &back); err != nil {
		t.Fatalf("reading the status back: %v", err)
	}
	if back.ID != "" || back.Health.Container != (granule.ContainerHealth{}) {
		t.Errorf("id or container health survived a round trip: %+v", back)
	}
	// The half that is the grain's own still crosses, or the record would
	// carry nothing worth reading.
	if !back.Health.Guest.Ready || back.Phase != granule.PhaseRunning {
		t.Errorf("the grain's own half did not survive: %+v", back)
	}
}

// The guest writes one path and the container mounts another, and they
// must not overlap. Everything under Root arrives from outside and is
// grain's own material; GuestActivityFile is written from inside the
// sandbox by the work itself. A guest-writable path under Root would let
// the sandbox appear to have authored a prompt, a credential or a setup
// script.
func TestTheGuestActivityPathIsOutsideTheMountedTree(t *testing.T) {
	if strings.HasPrefix(granule.GuestActivityFile, granule.Root+"/") || granule.GuestActivityFile == granule.Root {
		t.Fatalf("%s is inside the mounted tree %s", granule.GuestActivityFile, granule.Root)
	}
	if !path.IsAbs(granule.GuestActivityFile) || path.Clean(granule.GuestActivityFile) != granule.GuestActivityFile {
		t.Errorf("%s is not an absolute, cleaned path", granule.GuestActivityFile)
	}
	// It is a guest path, so it is not something Files renders: a Spec
	// that could write it would be the controller setting an activity on
	// the grain's behalf, which is backwards.
	spec := granule.Spec{Version: granule.Version, Prompt: "p", Setup: "s"}
	files, err := spec.Files()
	if err != nil {
		t.Fatalf("rendering files: %v", err)
	}
	if _, ok := files[granule.GuestActivityFile]; ok {
		t.Errorf("Files renders the guest activity path")
	}
}
