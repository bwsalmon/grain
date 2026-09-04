package grain_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/bwsalmon/grain/pkg/grain"
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

func TestSpecEnv(t *testing.T) {
	spec := grain.Spec{
		Version:    grain.Version,
		Framework:  grain.FrameworkSpec{Name: "claude", Credential: "sk-ant-oat01-..."},
		Shape:      grain.Shape{CPUs: 2, MemoryMB: 8192, DiskGB: 30},
		Setup:      "git clone http://10.0.2.1:8080/bwsalmon/grain.git /w && ./scripts/setup.sh",
		Placements: []grain.Placement{{Path: "/home/agent/.git-credentials", Content: "https://x:tok@10.0.2.1:8080", Mode: "0600"}},
		MaxRuntime: grain.Duration(2 * time.Hour),
	}

	env, err := spec.Env()
	if err != nil {
		t.Fatalf("Env: %v", err)
	}
	want := map[string]string{
		"GRAIN_WIRE_VERSION": "v1",
		"GRAIN_FRAMEWORK":    "claude",
		"GRAIN_CREDENTIAL":   "sk-ant-oat01-...",
		"GRAIN_SETUP":        "git clone http://10.0.2.1:8080/bwsalmon/grain.git /w && ./scripts/setup.sh",
		"GRAIN_MAX_RUNTIME":  "2h0m0s",
		"GRAIN_PLACEMENTS":   `[{"path":"/home/agent/.git-credentials","content":"https://x:tok@10.0.2.1:8080","mode":"0600"}]`,
		// kontur's own, passed through and never read back.
		"CHV_CPUS":         "2",
		"CHV_MEMORY_MB":    "8192",
		"CHV_DISK_SIZE_MB": "30720",
	}
	if len(env) != len(want) {
		t.Fatalf("Env produced %d variables, want %d:\n%#v", len(env), len(want), env)
	}
	for k, v := range want {
		if env[k] != v {
			t.Errorf("%s = %q, want %q", k, env[k], v)
		}
	}

	back, err := grain.SpecFromEnv(func(k string) string { return env[k] })
	if err != nil {
		t.Fatalf("SpecFromEnv: %v", err)
	}
	if time.Duration(back.MaxRuntime) != 2*time.Hour {
		t.Errorf("MaxRuntime round-tripped as %s", back.MaxRuntime)
	}
	if back.Framework.Credential != spec.Framework.Credential {
		t.Errorf("credential round-tripped as %q", back.Framework.Credential)
	}
	if len(back.Placements) != 1 || back.Placements[0].Mode != "0600" {
		t.Errorf("placements round-tripped as %#v", back.Placements)
	}
	// Shape is kontur's to read, never grain's to read back.
	if !back.Shape.IsZero() {
		t.Errorf("SpecFromEnv read shape back as %#v; those variables are kontur's", back.Shape)
	}
}

// A shim that half-understands its environment starts an agent that does
// the wrong thing quietly. Refusing costs one run and names the
// disagreement.
func TestSpecFromEnvRefusesAnotherVersion(t *testing.T) {
	_, err := grain.SpecFromEnv(func(k string) string {
		if k == grain.EnvVersion {
			return "v2"
		}
		return ""
	})
	if err == nil {
		t.Fatal("SpecFromEnv accepted wire version v2")
	}
	if !strings.Contains(err.Error(), "v2") || !strings.Contains(err.Error(), grain.Version) {
		t.Errorf("error should name both versions, got: %v", err)
	}

	if _, err := grain.SpecFromEnv(func(string) string { return "" }); err == nil {
		t.Fatal("SpecFromEnv accepted an environment with no version at all")
	}
}

// A malformed placements blob is material; its parse error must not quote
// it.
func TestSpecFromEnvDoesNotEchoMaterial(t *testing.T) {
	_, err := grain.SpecFromEnv(func(k string) string {
		switch k {
		case grain.EnvVersion:
			return grain.Version
		case grain.EnvPlacements:
			return `[{"path":"/p","content":"sk-ant-secret-value"`
		}
		return ""
	})
	if err == nil {
		t.Fatal("SpecFromEnv accepted malformed placements")
	}
	if strings.Contains(err.Error(), "sk-ant-secret-value") {
		t.Errorf("parse error quotes material: %v", err)
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
	full := marshal(t, grain.Spec{
		Version:   grain.Version,
		Framework: grain.FrameworkSpec{Name: "claude", Credential: "sk-ant-oat01-..."},
		Shape:     grain.Shape{CPUs: 2}, Setup: "true",
		Placements: []grain.Placement{{Path: "/p", Mode: "0600"}},
		MaxRuntime: grain.Duration(time.Hour),
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
	st := grain.Status{
		Version:  grain.Version,
		ID:       "task-311-2", // set by the backend, never on the wire
		Phase:    grain.PhaseBlocked,
		Since:    time.Date(2026, 9, 4, 19, 41, 12, 0, time.UTC),
		Activity: "waiting for CI",
		Rebuilds: 1,
		Setup:    &grain.SetupResult{Output: "9f3c1a2\n"},
		Requests: []grain.Request{{
			ID: "r-7", Kind: grain.KindOpenPullRequest,
			Raised: time.Date(2026, 9, 4, 19, 40, 0, 0, time.UTC),
		}},
		Health: grain.Health{
			Container: grain.ContainerHealth{Running: true},
			Guest: grain.GuestHealth{
				Ready: true, LoadAverage: "0.41 0.30 0.22",
				ConntrackCount: 812, ConntrackMax: 262144,
			},
		},
		Seq:      4471,
		Consumed: []string{"sig-19"},
	}

	want := `{
  "version": "v1",
  "phase": "blocked",
  "since": "2026-09-04T19:41:12Z",
  "activity": "waiting for CI",
  "rebuilds": 1,
  "setup": {
    "exitCode": 0,
    "output": "9f3c1a2\n"
  },
  "requests": [
    {
      "id": "r-7",
      "kind": "open_pull_request",
      "raised": "2026-09-04T19:40:00Z"
    }
  ],
  "health": {
    "container": {
      "running": true
    },
    "guest": {
      "ready": true,
      "loadAverage": "0.41 0.30 0.22",
      "conntrackCount": 812,
      "conntrackMax": 262144
    }
  },
  "seq": 4471,
  "consumed": [
    "sig-19"
  ]
}`
	if got := marshal(t, st); got != want {
		t.Fatalf("Status wire format changed.\n got:\n%s\nwant:\n%s", got, want)
	}
}

// A record is one line, always: a log stream is line-oriented, and a
// value that spans lines cannot survive a reader joining mid-stream or a
// rotation taking the first half of it.
func TestRecordIsOneLine(t *testing.T) {
	rec := grain.Record{
		Version: grain.Version, Seq: 42,
		T:    time.Date(2026, 9, 4, 19, 55, 3, 0, time.UTC),
		Src:  grain.SrcConsole,
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
	var d grain.Duration
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
	spec := grain.Spec{
		Version:   grain.Version,
		Framework: grain.FrameworkSpec{Name: "claude", Credential: "sk-ant-oat01-secret"},
		Setup:     "git clone http://10.0.2.1:8080/bwsalmon/grain.git /w",
		Placements: []grain.Placement{
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
	if !strings.Contains(out, "git clone http://10.0.2.1:8080/bwsalmon/grain.git") {
		t.Error("Redacted blanked setup, which is diagnosis rather than material")
	}
	if strings.Contains(spec.Setup, "@") {
		t.Error("the setup script embeds credentials in a URL; they belong in a placement")
	}
}
