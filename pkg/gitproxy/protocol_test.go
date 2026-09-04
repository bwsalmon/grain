package gitproxy

import "testing"

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

// What a refusal looks like on the wire. git prints the body of a
// non-200 response verbatim, as "remote: " lines, and only when it is
// told the body is text -- so a length prefix in front of the message
// is noise an operator reads, and a missing Content-Type is a message
// they never see at all.
func TestDenialIsPlainTextGitWillPrint(t *testing.T) {
	resp := Denial(500, "no credential configured for owner/repo")
	if resp.Status != 500 {
		t.Errorf("status = %d, want 500", resp.Status)
	}
	if got := string(resp.Body); got != "no credential configured for owner/repo\n" {
		t.Errorf("body = %q, want the bare message and a newline", got)
	}
	if got := resp.Headers["Content-Type"]; got != "text/plain; charset=utf-8" {
		t.Errorf("Content-Type = %q, want plain text", got)
	}
}
