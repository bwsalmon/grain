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
		Task:      grain.TaskRef{ID: "task-311", Title: "Port the staleness check", Attempt: 2},
		Framework: "claude",
		Repo: grain.RepoSpec{
			Target: "bwsalmon/grain", Base: "main", Branch: "grain/task-311",
			Reads: []string{"bwsalmon/kontur"}, ProxyBase: "http://10.0.2.1:8080",
		},
		Shape:  grain.Shape{CPUs: 2, MemoryMB: 8192, DiskGB: 30},
		Limits: grain.Limits{MaxRuntime: grain.Duration(2 * time.Hour), MaxRebuilds: 3},
		Setup:  "./scripts/setup.sh",
		Grants: []string{"self-debug"},
		Placements: []grain.Placement{
			{Dest: grain.DestContainer, Path: "/root/.claude/.credentials.json", Content: "{...}", Mode: "0600"},
			{Dest: grain.DestGuest, Path: "/home/agent/key.json", Content: "{...}", Mode: "0600"},
		},
		GitToken: "sbx_9f3c1a",
	}

	want := `{
  "contract": 1,
  "id": "task-311-2",
  "task": {
    "id": "task-311",
    "title": "Port the staleness check",
    "attempt": 2
  },
  "framework": "claude",
  "repo": {
    "target": "bwsalmon/grain",
    "base": "main",
    "branch": "grain/task-311",
    "reads": [
      "bwsalmon/kontur"
    ],
    "proxyBase": "http://10.0.2.1:8080"
  },
  "shape": {
    "cpus": 2,
    "memoryMB": 8192,
    "diskGB": 30
  },
  "limits": {
    "maxRuntime": "2h0m0s",
    "maxRebuilds": 3
  },
  "setup": "./scripts/setup.sh",
  "grants": [
    "self-debug"
  ],
  "placements": [
    {
      "dest": "container",
      "path": "/root/.claude/.credentials.json",
      "content": "{...}",
      "mode": "0600"
    },
    {
      "dest": "guest",
      "path": "/home/agent/key.json",
      "content": "{...}",
      "mode": "0600"
    }
  ],
  "gitToken": "sbx_9f3c1a"
}`
	if got := marshal(t, spec); got != want {
		t.Fatalf("Spec wire format changed.\n got:\n%s\nwant:\n%s", got, want)
	}

	var back grain.Spec
	if err := json.Unmarshal([]byte(want), &back); err != nil {
		t.Fatalf("unmarshalling: %v", err)
	}
	if time.Duration(back.Limits.MaxRuntime) != 2*time.Hour {
		t.Errorf("MaxRuntime round-tripped as %s, want 2h0m0s", back.Limits.MaxRuntime)
	}
	if back.Placements[1].Dest != grain.DestGuest {
		t.Errorf("Dest round-tripped as %q, want %q", back.Placements[1].Dest, grain.DestGuest)
	}
}

func TestStatusWireFormat(t *testing.T) {
	st := grain.Status{
		Contract: grain.Contract,
		ID:       "task-311-2",
		Ref:      grain.TaskRef{ID: "task-311", Attempt: 2},
		Phase:    grain.PhaseBlocked,
		Since:    time.Date(2026, 9, 4, 19, 41, 12, 0, time.UTC),
		Activity: "waiting for CI",
		Rebuilds: 1,
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
  "ref": {
    "id": "task-311",
    "attempt": 2
  },
  "phase": "blocked",
  "since": "2026-09-04T19:41:12Z",
  "activity": "waiting for CI",
  "rebuilds": 1,
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
