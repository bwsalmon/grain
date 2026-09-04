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

func TestSpecWireFormat(t *testing.T) {
	spec := grain.Spec{
		Contract:  grain.Contract,
		ID:        "task-311-2",
		Framework: "claude",
		Shape:     grain.Shape{CPUs: 2, MemoryMB: 8192, DiskGB: 30},
		Setup:     "git clone http://10.0.2.1:8080/bwsalmon/grain.git /w && cd /w && ./scripts/setup.sh",
		Placements: []grain.Placement{
			{Dest: grain.DestContainer, Path: "/root/.claude/.credentials.json", Content: "{...}", Mode: "0600"},
			{Dest: grain.DestGuest, Path: "/home/agent/.git-credentials", Content: "https://x:sbx_9f3c1a@10.0.2.1:8080", Mode: "0600"},
		},
		MaxRuntime: grain.Duration(2 * time.Hour),
	}

	want := `{
  "contract": 1,
  "id": "task-311-2",
  "framework": "claude",
  "shape": {
    "cpus": 2,
    "memoryMB": 8192,
    "diskGB": 30
  },
  "setup": "git clone http://10.0.2.1:8080/bwsalmon/grain.git /w \u0026\u0026 cd /w \u0026\u0026 ./scripts/setup.sh",
  "placements": [
    {
      "dest": "container",
      "path": "/root/.claude/.credentials.json",
      "content": "{...}",
      "mode": "0600"
    },
    {
      "dest": "guest",
      "path": "/home/agent/.git-credentials",
      "content": "https://x:sbx_9f3c1a@10.0.2.1:8080",
      "mode": "0600"
    }
  ],
  "maxRuntime": "2h0m0s"
}`
	if got := marshal(t, spec); got != want {
		t.Fatalf("Spec wire format changed.\n got:\n%s\nwant:\n%s", got, want)
	}

	var back grain.Spec
	if err := json.Unmarshal([]byte(want), &back); err != nil {
		t.Fatalf("unmarshalling: %v", err)
	}
	if time.Duration(back.MaxRuntime) != 2*time.Hour {
		t.Errorf("MaxRuntime round-tripped as %s, want 2h0m0s", back.MaxRuntime)
	}
	if back.Placements[1].Dest != grain.DestGuest {
		t.Errorf("Dest round-tripped as %q, want %q", back.Placements[1].Dest, grain.DestGuest)
	}
}

// The Spec deliberately carries no task, repository, branch, git
// credential or capability model: a grain runs an agent in a sandbox and
// knows nothing about why. Everything task-shaped reaches it as a prompt,
// as the setup script, or as a placement.
//
// Asserted on the marshalled document rather than the type, because what
// matters is that none of it is on the wire between two separately
// released artifacts -- and because a field added back would round-trip
// perfectly and still put grain's task model into the sandbox image's
// contract.
func TestSpecCarriesNoTaskModel(t *testing.T) {
	full := marshal(t, grain.Spec{
		Contract: grain.Contract, ID: "task-311-2", Framework: "claude",
		Shape: grain.Shape{CPUs: 2}, Setup: "true",
		Placements: []grain.Placement{{Dest: grain.DestGuest, Path: "/p", Mode: "0600"}},
		MaxRuntime: grain.Duration(time.Hour),
	})
	for _, absent := range []string{
		"\"task\"", "\"repo\"", "\"branch\"", "\"base\"", "\"target\"",
		"\"gitToken\"", "\"grants\"", "\"proxyBase\"", "\"attempt\"", "\"maxTurns\"",
	} {
		if strings.Contains(full, absent) {
			t.Errorf("Spec carries %s; a grain has no use for it "+
				"(see the Spec doc comment for where it belongs instead)", absent)
		}
	}
}

func TestStatusWireFormat(t *testing.T) {
	st := grain.Status{
		Contract: grain.Contract,
		ID:       "task-311-2",
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
  "contract": 1,
  "id": "task-311-2",
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
		V: grain.Contract, Seq: 42,
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
	want := `{"v":1,"seq":42,"t":"2026-09-04T19:55:03Z","src":"console","data":"[    0.512] EXT4-fs (vda): mounted"}`
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
