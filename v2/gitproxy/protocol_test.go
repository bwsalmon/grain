package gitproxy

import (
	"strconv"
	"testing"
)

func TestParsePathParsesInfoRefs(t *testing.T) {
	req, ok := ParsePath("/owner/repo.git/info/refs")
	if !ok || req != (GitRequest{"owner", "repo", "info/refs"}) {
		t.Fatalf("got %+v, %v", req, ok)
	}
}

func TestParsePathWorksWithoutDotGitSuffix(t *testing.T) {
	req, ok := ParsePath("/owner/repo/git-upload-pack")
	if !ok || req != (GitRequest{"owner", "repo", "git-upload-pack"}) {
		t.Fatalf("got %+v, %v", req, ok)
	}
}

func TestParsePathParsesGitReceivePack(t *testing.T) {
	req, ok := ParsePath("/owner/repo.git/git-receive-pack")
	if !ok || req != (GitRequest{"owner", "repo", "git-receive-pack"}) {
		t.Fatalf("got %+v, %v", req, ok)
	}
}

func TestParsePathRejectsAnythingElse(t *testing.T) {
	for _, path := range []string{
		"/owner/repo.git/objects/info/packs",
		"/owner/repo.git/HEAD",
		"/",
		"/just-one-segment",
	} {
		if _, ok := ParsePath(path); ok {
			t.Errorf("ParsePath(%q) should not match", path)
		}
	}
}

func TestIsValidGitRequestNeedsAGitUserAgent(t *testing.T) {
	if IsValidGitRequest("Mozilla/5.0", "*/*", "info/refs") {
		t.Error("a browser user agent should be rejected")
	}
	if !IsValidGitRequest("git/2.39.2", "*/*", "info/refs") {
		t.Error("a git user agent should be accepted for info/refs")
	}
}

func TestIsValidGitRequestChecksAcceptForPackEndpoints(t *testing.T) {
	const ua = "git/2.39.2"
	if !IsValidGitRequest(ua, "application/x-git-upload-pack-result", "git-upload-pack") {
		t.Error("matching Accept header should be accepted")
	}
	if IsValidGitRequest(ua, "application/x-git-receive-pack-result", "git-upload-pack") {
		t.Error("mismatched Accept header should be rejected")
	}
	if IsValidGitRequest(ua, "*/*", "git-upload-pack") {
		t.Error("*/* should be rejected for a pack endpoint")
	}
}

func TestPktLineLengthPrefix(t *testing.T) {
	got := PktLine([]byte("hello\n"))
	want := "000ahello\n"
	if string(got) != want {
		t.Errorf("PktLine = %q, want %q", got, want)
	}
}

func TestErrPktIsAValidPktLine(t *testing.T) {
	out := ErrPkt("nope")
	length, err := strconv.ParseInt(string(out[:4]), 16, 64)
	if err != nil {
		t.Fatal(err)
	}
	if int(length) != len(out) {
		t.Errorf("length prefix %d, want %d", length, len(out))
	}
	if string(out[4:]) != "ERR nope\n" {
		t.Errorf("body = %q", out[4:])
	}
}
