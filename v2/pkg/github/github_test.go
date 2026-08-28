package github

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// --- RealTransport: exercised against a real local HTTP server rather
// than mocked, since the whole point is to check that net/http is driven
// correctly (method/path/headers/body out, status/headers/body back).
// UseTLS false is a real, documented configuration -- the mocked-GitHub
// live-test seam -- so this is not testing a code path production never
// takes.

func TestRealTransportSendsTheRequestAndParsesTheResponse(t *testing.T) {
	var gotMethod, gotPath, gotAccept string
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotAccept = r.Header.Get("Accept")
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("X-Reply", "yes")
		w.WriteHeader(200)
		w.Write([]byte("hello"))
	}))
	defer server.Close()

	host := strings.TrimPrefix(server.URL, "http://")
	transport := &RealTransport{Host: host, UseTLS: false}
	resp, err := transport.Request("POST", "/repos/acme/widgets/issues",
		map[string]string{"Accept": "application/vnd.github+json"}, []byte("payload"))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Status != 200 || string(resp.Body) != "hello" || resp.Headers["X-Reply"] != "yes" {
		t.Fatalf("unexpected response: %+v", resp)
	}
	if gotMethod != "POST" || gotPath != "/repos/acme/widgets/issues" {
		t.Fatalf("unexpected request: %s %s", gotMethod, gotPath)
	}
	if string(gotBody) != "payload" {
		t.Fatalf("unexpected body: %s", gotBody)
	}
	if gotAccept != "application/vnd.github+json" {
		t.Fatalf("unexpected accept header: %s", gotAccept)
	}
}

func issueJSONFixture(number int, isPR bool, labels ...string) map[string]any {
	if len(labels) == 0 {
		labels = []string{"grain-agent"}
	}
	labelObjs := make([]map[string]string, len(labels))
	for i, l := range labels {
		labelObjs[i] = map[string]string{"name": l}
	}
	item := map[string]any{
		"number": number, "title": "issue", "body": "do the thing",
		"html_url": "https://github.com/o/r/issues/1", "labels": labelObjs,
	}
	if isPR {
		item["pull_request"] = map[string]string{"url": "..."}
	}
	return item
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func strPtr(s string) *string { return &s }

func TestListIssuesReturnsOpenIssues(t *testing.T) {
	transport := NewFakeTransport(ApiResponse{Status: 200, Body: mustJSON(t, []any{issueJSONFixture(1, false)})})
	client := NewClient(transport, StaticToken{strPtr("t")})
	issues, err := client.ListIssues("o", "r", "grain-agent")
	if err != nil {
		t.Fatal(err)
	}
	if len(issues) != 1 || issues[0].Number != 1 {
		t.Fatalf("got %+v", issues)
	}
	if !issues[0].HasLabel("grain-agent") {
		t.Fatalf("expected grain-agent label, got %+v", issues[0].Labels)
	}
}

func TestListIssuesFiltersOutPullRequests(t *testing.T) {
	transport := NewFakeTransport(ApiResponse{
		Status: 200, Body: mustJSON(t, []any{issueJSONFixture(1, false), issueJSONFixture(2, true)}),
	})
	client := NewClient(transport, StaticToken{strPtr("t")})
	issues, err := client.ListIssues("o", "r", "grain-agent")
	if err != nil {
		t.Fatal(err)
	}
	if len(issues) != 1 || issues[0].Number != 1 {
		t.Fatalf("got %+v", issues)
	}
}

func TestListIssuesFollowsLinkHeaderPagination(t *testing.T) {
	transport := &FakeTransport{Responses: []ApiResponse{
		{Status: 200, Headers: map[string]string{"Link": `<https://api.github.com/repos/o/r/issues?page=2>; rel="next"`},
			Body: mustJSON(t, []any{issueJSONFixture(1, false)})},
		{Status: 200, Body: mustJSON(t, []any{issueJSONFixture(2, false)})},
	}}
	client := NewClient(transport, StaticToken{strPtr("t")})
	issues, err := client.ListIssues("o", "r", "grain-agent")
	if err != nil {
		t.Fatal(err)
	}
	if len(issues) != 2 || issues[0].Number != 1 || issues[1].Number != 2 {
		t.Fatalf("got %+v", issues)
	}
	if transport.Calls[1].Path != "/repos/o/r/issues?page=2" {
		t.Fatalf("unexpected second call path: %s", transport.Calls[1].Path)
	}
}

func TestNextPagePathReturnsEmptyWhenTheLinkHeaderHasNoNextRel(t *testing.T) {
	header := `<https://api.github.com/repos/o/r/issues?page=1>; rel="prev"`
	if got := nextPagePath(header); got != "" {
		t.Fatalf("expected empty, got %q", got)
	}
}

func TestNextPagePathSkipsEarlierSegmentsToFindNext(t *testing.T) {
	header := `<https://api.github.com/repos/o/r/issues?page=1>; rel="prev", ` +
		`<https://api.github.com/repos/o/r/issues?page=3>; rel="next"`
	if got := nextPagePath(header); got != "/repos/o/r/issues?page=3" {
		t.Fatalf("got %q", got)
	}
}

func TestListIssuesRaisesOnANon200(t *testing.T) {
	transport := NewFakeTransport(ApiResponse{Status: 403, Body: []byte("nope")})
	client := NewClient(transport, StaticToken{strPtr("t")})
	if _, err := client.ListIssues("o", "r", "grain-agent"); err == nil {
		t.Fatal("expected an error")
	}
}

func TestGetIssueReturnsTheIssue(t *testing.T) {
	transport := NewFakeTransport(ApiResponse{Status: 200, Body: mustJSON(t, issueJSONFixture(7, false))})
	client := NewClient(transport, StaticToken{strPtr("t")})
	issue, err := client.GetIssue("o", "r", 7)
	if err != nil {
		t.Fatal(err)
	}
	if issue.Number != 7 || !issue.HasLabel("grain-agent") {
		t.Fatalf("got %+v", issue)
	}
	if transport.Calls[0].Path != "/repos/o/r/issues/7" {
		t.Fatalf("got path %s", transport.Calls[0].Path)
	}
}

func TestGetIssueRaisesOnANon200(t *testing.T) {
	transport := NewFakeTransport(ApiResponse{Status: 404, Body: []byte("not found")})
	client := NewClient(transport, StaticToken{strPtr("t")})
	if _, err := client.GetIssue("o", "r", 7); err == nil {
		t.Fatal("expected an error")
	}
}

func TestAddLabelPostsTheLabelBody(t *testing.T) {
	transport := NewFakeTransport(ApiResponse{Status: 200, Body: []byte("[]")})
	client := NewClient(transport, StaticToken{strPtr("t")})
	if err := client.AddLabel("o", "r", 1, "grain-agent-in-progress"); err != nil {
		t.Fatal(err)
	}
	call := transport.Calls[0]
	if call.Method != "POST" {
		t.Fatalf("got method %s", call.Method)
	}
	var body struct {
		Labels []string `json:"labels"`
	}
	if err := json.Unmarshal(call.Body, &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Labels) != 1 || body.Labels[0] != "grain-agent-in-progress" {
		t.Fatalf("got %+v", body)
	}
}

func TestRemoveLabelToleratesA404TheLabelIsAlreadyGone(t *testing.T) {
	transport := NewFakeTransport(ApiResponse{Status: 404, Body: []byte("not found")})
	client := NewClient(transport, StaticToken{strPtr("t")})
	if err := client.RemoveLabel("o", "r", 1, "grain-agent"); err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
}

func TestRemoveLabelRaisesOnOtherErrors(t *testing.T) {
	transport := NewFakeTransport(ApiResponse{Status: 500, Body: []byte("boom")})
	client := NewClient(transport, StaticToken{strPtr("t")})
	if err := client.RemoveLabel("o", "r", 1, "grain-agent"); err == nil {
		t.Fatal("expected an error")
	}
}

func TestCloseIssuePatchesTheIssueClosed(t *testing.T) {
	transport := NewFakeTransport(ApiResponse{Status: 200, Body: []byte("{}")})
	client := NewClient(transport, StaticToken{strPtr("t")})
	if err := client.CloseIssue("o", "r", 1); err != nil {
		t.Fatal(err)
	}
	call := transport.Calls[0]
	if call.Method != "PATCH" || call.Path != "/repos/o/r/issues/1" {
		t.Fatalf("got %+v", call)
	}
	var body map[string]string
	json.Unmarshal(call.Body, &body)
	if body["state"] != "closed" {
		t.Fatalf("got %+v", body)
	}
}

func TestReopenIssuePatchesTheIssueOpen(t *testing.T) {
	transport := NewFakeTransport(ApiResponse{Status: 200, Body: []byte("{}")})
	client := NewClient(transport, StaticToken{strPtr("t")})
	if err := client.ReopenIssue("o", "r", 1); err != nil {
		t.Fatal(err)
	}
	var body map[string]string
	json.Unmarshal(transport.Calls[0].Body, &body)
	if body["state"] != "open" {
		t.Fatalf("got %+v", body)
	}
}

func TestUpdateIssuePatchesOnlyTheFieldsGiven(t *testing.T) {
	transport := NewFakeTransport(ApiResponse{Status: 200, Body: []byte("{}")})
	client := NewClient(transport, StaticToken{strPtr("t")})
	title := "new title"
	if err := client.UpdateIssue("o", "r", 1, &title, nil); err != nil {
		t.Fatal(err)
	}
	call := transport.Calls[0]
	if call.Method != "PATCH" || call.Path != "/repos/o/r/issues/1" {
		t.Fatalf("got %+v", call)
	}
	var body map[string]string
	if err := json.Unmarshal(call.Body, &body); err != nil {
		t.Fatal(err)
	}
	if body["title"] != "new title" {
		t.Fatalf("got %+v", body)
	}
	if _, ok := body["body"]; ok {
		t.Fatalf("body field should be omitted when nil, got %+v", body)
	}
}

func TestUpdateIssueRaisesOnANon200(t *testing.T) {
	transport := NewFakeTransport(ApiResponse{Status: 500, Body: []byte("boom")})
	client := NewClient(transport, StaticToken{strPtr("t")})
	body := "new body"
	if err := client.UpdateIssue("o", "r", 1, nil, &body); err == nil {
		t.Fatal("expected an error")
	}
}

func TestAnonymousClientSendsNoAuthorizationHeader(t *testing.T) {
	transport := NewFakeTransport(ApiResponse{Status: 200, Body: []byte("[]")})
	client := NewClient(transport, nil)
	if _, err := client.ListIssues("o", "r", "grain-agent"); err != nil {
		t.Fatal(err)
	}
	if _, ok := transport.Calls[0].Headers["Authorization"]; ok {
		t.Fatal("expected no Authorization header")
	}
}

func TestBranchExistsTrueOn200(t *testing.T) {
	transport := NewFakeTransport(ApiResponse{Status: 200, Body: []byte("{}")})
	client := NewClient(transport, StaticToken{strPtr("t")})
	exists, err := client.BranchExists("o", "r", "grain/issue-1")
	if err != nil || !exists {
		t.Fatalf("exists=%v err=%v", exists, err)
	}
}

func TestBranchExistsFalseOn404(t *testing.T) {
	transport := NewFakeTransport(ApiResponse{Status: 404, Body: []byte("not found")})
	client := NewClient(transport, StaticToken{strPtr("t")})
	exists, err := client.BranchExists("o", "r", "grain/issue-1")
	if err != nil || exists {
		t.Fatalf("exists=%v err=%v", exists, err)
	}
}

func TestBranchExistsPercentEncodesTheSlashInTheBranchName(t *testing.T) {
	transport := NewFakeTransport(ApiResponse{Status: 200, Body: []byte("{}")})
	client := NewClient(transport, StaticToken{strPtr("t")})
	if _, err := client.BranchExists("o", "r", "grain/issue-1"); err != nil {
		t.Fatal(err)
	}
	if transport.Calls[0].Path != "/repos/o/r/branches/grain%2Fissue-1" {
		t.Fatalf("got %s", transport.Calls[0].Path)
	}
}

func TestBranchExistsRaisesOnOtherErrors(t *testing.T) {
	transport := NewFakeTransport(ApiResponse{Status: 500, Body: []byte("boom")})
	client := NewClient(transport, StaticToken{strPtr("t")})
	if _, err := client.BranchExists("o", "r", "grain/issue-1"); err == nil {
		t.Fatal("expected an error")
	}
}

func TestGetBranchHeadReturnsTheShaAndMessageOn200(t *testing.T) {
	body := mustJSON(t, map[string]any{
		"commit": map[string]any{"sha": "abc123", "commit": map[string]any{"message": "Fix the thing\n\nDetails."}},
	})
	transport := NewFakeTransport(ApiResponse{Status: 200, Body: body})
	client := NewClient(transport, StaticToken{strPtr("t")})
	head, err := client.GetBranchHead("o", "r", "grain/issue-1")
	if err != nil {
		t.Fatal(err)
	}
	if head == nil || head.SHA != "abc123" || head.Message != "Fix the thing\n\nDetails." {
		t.Fatalf("got %+v", head)
	}
}

func TestGetBranchHeadReturnsNilOn404(t *testing.T) {
	transport := NewFakeTransport(ApiResponse{Status: 404, Body: []byte("not found")})
	client := NewClient(transport, StaticToken{strPtr("t")})
	head, err := client.GetBranchHead("o", "r", "grain/issue-1")
	if err != nil || head != nil {
		t.Fatalf("head=%+v err=%v", head, err)
	}
}

func TestGetBranchHeadRaisesOnOtherErrors(t *testing.T) {
	transport := NewFakeTransport(ApiResponse{Status: 500, Body: []byte("boom")})
	client := NewClient(transport, StaticToken{strPtr("t")})
	if _, err := client.GetBranchHead("o", "r", "grain/issue-1"); err == nil {
		t.Fatal("expected an error")
	}
}

func TestDryRunClientPassesGetBranchHeadThrough(t *testing.T) {
	body := mustJSON(t, map[string]any{
		"commit": map[string]any{"sha": "abc123", "commit": map[string]any{"message": "Fix the thing"}},
	})
	transport := NewFakeTransport(ApiResponse{Status: 200, Body: body})
	dry := DryRunClient{Inner: NewClient(transport, StaticToken{strPtr("t")})}
	head, err := dry.GetBranchHead("o", "r", "grain/issue-1")
	if err != nil || head == nil || head.SHA != "abc123" {
		t.Fatalf("head=%+v err=%v", head, err)
	}
}

func TestCreatePullRequestPostsHeadBaseAndTitle(t *testing.T) {
	body := mustJSON(t, map[string]any{"number": 42, "html_url": "https://github.com/o/r/pull/42"})
	transport := NewFakeTransport(ApiResponse{Status: 201, Body: body})
	client := NewClient(transport, StaticToken{strPtr("t")})
	pr, err := client.CreatePullRequest("o", "r", "grain/issue-1", "main", "grain: fix #1", "")
	if err != nil {
		t.Fatal(err)
	}
	if pr != (PullRequest{Number: 42, HTMLURL: "https://github.com/o/r/pull/42"}) {
		t.Fatalf("got %+v", pr)
	}
	call := transport.Calls[0]
	if call.Method != "POST" || call.Path != "/repos/o/r/pulls" {
		t.Fatalf("got %+v", call)
	}
	var sent map[string]string
	json.Unmarshal(call.Body, &sent)
	if sent["head"] != "grain/issue-1" || sent["base"] != "main" || sent["title"] != "grain: fix #1" {
		t.Fatalf("got %+v", sent)
	}
}

func TestCreatePullRequestRaisesOnANon201(t *testing.T) {
	transport := NewFakeTransport(ApiResponse{Status: 422, Body: []byte("already exists")})
	client := NewClient(transport, StaticToken{strPtr("t")})
	if _, err := client.CreatePullRequest("o", "r", "grain/issue-1", "main", "x", ""); err == nil {
		t.Fatal("expected an error")
	}
}

func TestFindingTheOpenPRForABranchFiltersByAQualifiedHead(t *testing.T) {
	body := mustJSON(t, []any{map[string]any{"number": 42, "html_url": "https://github.com/o/r/pull/42"}})
	transport := NewFakeTransport(ApiResponse{Status: 200, Body: body})
	client := NewClient(transport, StaticToken{strPtr("t")})
	pr, err := client.FindOpenPullRequestForBranch("o", "r", "grain/issue-1")
	if err != nil || pr == nil || pr.Number != 42 {
		t.Fatalf("pr=%+v err=%v", pr, err)
	}
	call := transport.Calls[0]
	if call.Method != "GET" || call.Path != "/repos/o/r/pulls?state=open&head=o%3Agrain%2Fissue-1" {
		t.Fatalf("got %+v", call)
	}
}

func TestFindingTheOpenPRForABranchReturnsNoneWhenThereIsNone(t *testing.T) {
	transport := NewFakeTransport(ApiResponse{Status: 200, Body: []byte("[]")})
	client := NewClient(transport, StaticToken{strPtr("t")})
	pr, err := client.FindOpenPullRequestForBranch("o", "r", "grain/issue-1")
	if err != nil || pr != nil {
		t.Fatalf("pr=%+v err=%v", pr, err)
	}
}

func TestFindingTheOpenPRForABranchRaisesOnANon200(t *testing.T) {
	transport := NewFakeTransport(ApiResponse{Status: 500, Body: []byte("boom")})
	client := NewClient(transport, StaticToken{strPtr("t")})
	if _, err := client.FindOpenPullRequestForBranch("o", "r", "grain/issue-1"); err == nil {
		t.Fatal("expected an error")
	}
}

func TestTheDryRunClientPassesTheOpenPRLookupThrough(t *testing.T) {
	body := mustJSON(t, []any{map[string]any{"number": 42, "html_url": "https://github.com/o/r/pull/42"}})
	transport := NewFakeTransport(ApiResponse{Status: 200, Body: body})
	dry := DryRunClient{Inner: NewClient(transport, StaticToken{strPtr("t")})}
	pr, err := dry.FindOpenPullRequestForBranch("o", "r", "grain/issue-1")
	if err != nil || pr == nil || pr.Number != 42 {
		t.Fatalf("pr=%+v err=%v", pr, err)
	}
	if len(transport.Calls) != 1 {
		t.Fatalf("expected exactly one call, got %d", len(transport.Calls))
	}
}

func TestCreateIssuePostsTitleBodyAndLabels(t *testing.T) {
	transport := NewFakeTransport(ApiResponse{Status: 201, Body: mustJSON(t, issueJSONFixture(9, false))})
	client := NewClient(transport, StaticToken{strPtr("t")})
	issue, err := client.CreateIssue("o", "r", "issue 9", "do the thing", []string{"grain-agent-needs-approval"})
	if err != nil {
		t.Fatal(err)
	}
	if issue.Number != 9 || !issue.HasLabel("grain-agent") {
		t.Fatalf("got %+v", issue)
	}
	call := transport.Calls[0]
	if call.Method != "POST" || call.Path != "/repos/o/r/issues" {
		t.Fatalf("got %+v", call)
	}
	var sent map[string]any
	json.Unmarshal(call.Body, &sent)
	if sent["title"] != "issue 9" || sent["body"] != "do the thing" {
		t.Fatalf("got %+v", sent)
	}
	labels, _ := sent["labels"].([]any)
	if len(labels) != 1 || labels[0] != "grain-agent-needs-approval" {
		t.Fatalf("got %+v", sent["labels"])
	}
}

func TestCreateIssueOmitsLabelsKeyWhenNoneGiven(t *testing.T) {
	transport := NewFakeTransport(ApiResponse{Status: 201, Body: mustJSON(t, issueJSONFixture(9, false))})
	client := NewClient(transport, StaticToken{strPtr("t")})
	if _, err := client.CreateIssue("o", "r", "issue 9", "", nil); err != nil {
		t.Fatal(err)
	}
	var sent map[string]any
	json.Unmarshal(transport.Calls[0].Body, &sent)
	if _, ok := sent["labels"]; ok {
		t.Fatalf("expected no labels key, got %+v", sent)
	}
}

func TestCreateIssueRaisesOnANon201(t *testing.T) {
	transport := NewFakeTransport(ApiResponse{Status: 422, Body: []byte("nope")})
	client := NewClient(transport, StaticToken{strPtr("t")})
	if _, err := client.CreateIssue("o", "r", "x", "", nil); err == nil {
		t.Fatal("expected an error")
	}
}

func TestMergePullRequestPutsToTheMergeEndpoint(t *testing.T) {
	transport := NewFakeTransport(ApiResponse{Status: 200, Body: []byte(`{"merged": true}`)})
	client := NewClient(transport, StaticToken{strPtr("t")})
	if err := client.MergePullRequest("o", "r", 5); err != nil {
		t.Fatal(err)
	}
	call := transport.Calls[0]
	if call.Method != "PUT" || call.Path != "/repos/o/r/pulls/5/merge" {
		t.Fatalf("got %+v", call)
	}
}

func TestMergePullRequestRaisesOnANon200(t *testing.T) {
	transport := NewFakeTransport(ApiResponse{Status: 405, Body: []byte("not mergeable")})
	client := NewClient(transport, StaticToken{strPtr("t")})
	err := client.MergePullRequest("o", "r", 5)
	if err == nil {
		t.Fatal("expected an error")
	}
	ghErr, ok := err.(*Error)
	if !ok || ghErr.Status != 405 {
		t.Fatalf("got %v", err)
	}
}

func checkRunsJSON(runs ...CheckRun) map[string]any {
	items := make([]map[string]any, len(runs))
	for i, r := range runs {
		items[i] = map[string]any{"name": r.Name, "status": r.Status, "conclusion": r.Conclusion}
	}
	return map[string]any{"total_count": len(runs), "check_runs": items}
}

func TestListCheckRunsReadsTheCheckRunShape(t *testing.T) {
	transport := NewFakeTransport(ApiResponse{Status: 200, Body: mustJSON(t, checkRunsJSON(
		CheckRun{Name: "build", Status: "completed", Conclusion: strPtr("failure")},
	))})
	client := NewClient(transport, StaticToken{strPtr("t")})
	runs, err := client.ListCheckRuns("o", "r", "feature-x")
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].Name != "build" || *runs[0].Conclusion != "failure" {
		t.Fatalf("got %+v", runs)
	}
	if transport.Calls[0].Path != "/repos/o/r/commits/feature-x/check-runs?per_page=100" {
		t.Fatalf("got %s", transport.Calls[0].Path)
	}
}

func TestListCheckRunsToleratesAStillRunningCheckWithNoConclusion(t *testing.T) {
	transport := NewFakeTransport(ApiResponse{Status: 200, Body: mustJSON(t, checkRunsJSON(
		CheckRun{Name: "build", Status: "in_progress", Conclusion: nil},
	))})
	client := NewClient(transport, StaticToken{strPtr("t")})
	runs, err := client.ListCheckRuns("o", "r", "feature-x")
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].Conclusion != nil {
		t.Fatalf("got %+v", runs)
	}
}

func TestListCheckRunsFollowsLinkHeaderPagination(t *testing.T) {
	transport := &FakeTransport{Responses: []ApiResponse{
		{Status: 200, Headers: map[string]string{"Link": `<https://api.github.com/repos/o/r/commits/feature-x/check-runs?page=2>; rel="next"`},
			Body: mustJSON(t, checkRunsJSON(CheckRun{Name: "a", Status: "completed", Conclusion: strPtr("success")}))},
		{Status: 200, Body: mustJSON(t, checkRunsJSON(CheckRun{Name: "b", Status: "completed", Conclusion: strPtr("success")}))},
	}}
	client := NewClient(transport, StaticToken{strPtr("t")})
	runs, err := client.ListCheckRuns("o", "r", "feature-x")
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 2 || runs[0].Name != "a" || runs[1].Name != "b" {
		t.Fatalf("got %+v", runs)
	}
}

func TestListCheckRunsRaisesOnANon200(t *testing.T) {
	transport := NewFakeTransport(ApiResponse{Status: 500, Body: []byte("boom")})
	client := NewClient(transport, StaticToken{strPtr("t")})
	if _, err := client.ListCheckRuns("o", "r", "feature-x"); err == nil {
		t.Fatal("expected an error")
	}
}

func prJSON(number int, headRef, baseRef string) map[string]any {
	return map[string]any{
		"number": number, "title": "pr", "body": "please review",
		"html_url": fmt.Sprintf("https://github.com/o/r/pull/%d", number),
		"head":     map[string]any{"ref": headRef, "sha": "abc123"},
		"base":     map[string]any{"ref": baseRef},
	}
}

func TestGetPullRequestReadsMergeable(t *testing.T) {
	body := prJSON(5, "feature-branch", "main")
	body["mergeable"] = false
	transport := NewFakeTransport(ApiResponse{Status: 200, Body: mustJSON(t, body)})
	client := NewClient(transport, StaticToken{strPtr("t")})
	pr, err := client.GetPullRequest("o", "r", 5)
	if err != nil {
		t.Fatal(err)
	}
	if pr.Mergeable == nil || *pr.Mergeable != false {
		t.Fatalf("got %+v", pr.Mergeable)
	}
}

func TestGetPullRequestDefaultsMergeableToNilWhenAbsent(t *testing.T) {
	transport := NewFakeTransport(ApiResponse{Status: 200, Body: mustJSON(t, prJSON(5, "feature-branch", "main"))})
	client := NewClient(transport, StaticToken{strPtr("t")})
	pr, err := client.GetPullRequest("o", "r", 5)
	if err != nil {
		t.Fatal(err)
	}
	if pr.Mergeable != nil {
		t.Fatalf("got %+v", pr.Mergeable)
	}
}

func TestGetPullRequestReadsHeadAndBaseRef(t *testing.T) {
	transport := NewFakeTransport(ApiResponse{Status: 200, Body: mustJSON(t, prJSON(5, "feature-branch", "main"))})
	client := NewClient(transport, StaticToken{strPtr("t")})
	pr, err := client.GetPullRequest("o", "r", 5)
	if err != nil {
		t.Fatal(err)
	}
	want := PullRequestDetail{
		Number: 5, Title: "pr", Body: "please review", HTMLURL: "https://github.com/o/r/pull/5",
		HeadRef: "feature-branch", BaseRef: "main", State: "open",
	}
	if pr != want {
		t.Fatalf("got %+v want %+v", pr, want)
	}
	if transport.Calls[0].Path != "/repos/o/r/pulls/5" {
		t.Fatalf("got %s", transport.Calls[0].Path)
	}
}

func TestGetPullRequestReadsAClosedState(t *testing.T) {
	body := prJSON(5, "feature-branch", "main")
	body["state"] = "closed"
	transport := NewFakeTransport(ApiResponse{Status: 200, Body: mustJSON(t, body)})
	client := NewClient(transport, StaticToken{strPtr("t")})
	pr, err := client.GetPullRequest("o", "r", 5)
	if err != nil {
		t.Fatal(err)
	}
	if pr.State != "closed" {
		t.Fatalf("got %s", pr.State)
	}
}

func TestGetPullRequestDefaultsStateToOpenWhenAbsent(t *testing.T) {
	transport := NewFakeTransport(ApiResponse{Status: 200, Body: mustJSON(t, prJSON(5, "feature-branch", "main"))})
	client := NewClient(transport, StaticToken{strPtr("t")})
	pr, err := client.GetPullRequest("o", "r", 5)
	if err != nil {
		t.Fatal(err)
	}
	if pr.State != "open" {
		t.Fatalf("got %s", pr.State)
	}
}

func TestGetPullRequestRaisesOnANon200(t *testing.T) {
	transport := NewFakeTransport(ApiResponse{Status: 404, Body: []byte("not found")})
	client := NewClient(transport, StaticToken{strPtr("t")})
	if _, err := client.GetPullRequest("o", "r", 5); err == nil {
		t.Fatal("expected an error")
	}
}

func TestDefaultBranchReadsTheReposOwnDefault(t *testing.T) {
	transport := NewFakeTransport(ApiResponse{Status: 200, Body: mustJSON(t, map[string]string{"default_branch": "trunk"})})
	client := NewClient(transport, StaticToken{strPtr("t")})
	branch, err := client.DefaultBranch("o", "r")
	if err != nil || branch != "trunk" {
		t.Fatalf("branch=%q err=%v", branch, err)
	}
	if transport.Calls[0].Path != "/repos/o/r" {
		t.Fatalf("got %s", transport.Calls[0].Path)
	}
}

func TestDefaultBranchRaisesOnANon200(t *testing.T) {
	transport := NewFakeTransport(ApiResponse{Status: 404, Body: []byte("not found")})
	client := NewClient(transport, StaticToken{strPtr("t")})
	if _, err := client.DefaultBranch("o", "r"); err == nil {
		t.Fatal("expected an error")
	}
}

// perRepoTokens resolves a credential per repo -- the shape a real
// per-repo credential ladder is, and what proves the token is resolved
// per call rather than fixed at construction.
type perRepoTokens map[string]string

func (p perRepoTokens) TokenFor(owner, repo string) *string {
	if t, ok := p[owner+"/"+repo]; ok {
		return &t
	}
	return nil
}

func TestATokenSourceResolvesACredentialPerRepo(t *testing.T) {
	transport := NewFakeTransport()
	client := NewClient(transport, perRepoTokens{"o/tasks": "task-token", "o/code": "code-token"})
	client.ListIssues("o", "tasks", "grain-agent")
	client.ListIssues("o", "code", "grain-agent")
	client.ListIssues("o", "unmapped", "grain-agent")
	if transport.Calls[0].Headers["Authorization"] != "token task-token" {
		t.Fatalf("got %+v", transport.Calls[0].Headers)
	}
	if transport.Calls[1].Headers["Authorization"] != "token code-token" {
		t.Fatalf("got %+v", transport.Calls[1].Headers)
	}
	if _, ok := transport.Calls[2].Headers["Authorization"]; ok {
		t.Fatal("expected no Authorization header for the unmapped repo")
	}
}

func TestABareTokenStillAppliesToEveryRepo(t *testing.T) {
	transport := NewFakeTransport()
	client := NewClient(transport, StaticToken{strPtr("t")})
	client.ListIssues("o", "one", "grain-agent")
	client.ListIssues("other", "two", "grain-agent")
	for _, call := range transport.Calls {
		if call.Headers["Authorization"] != "token t" {
			t.Fatalf("got %+v", call.Headers)
		}
	}
}

func reviewCommentJSON(id int, line *int) map[string]any {
	return map[string]any{
		"id": id, "user": map[string]string{"login": "reviewer"}, "body": "please fix this",
		"path": "src/thing.py", "line": line, "diff_hunk": "@@ -1,3 +1,3 @@",
	}
}

func TestListReviewCommentsReadsTheReviewCommentShape(t *testing.T) {
	line := 12
	transport := NewFakeTransport(ApiResponse{Status: 200, Body: mustJSON(t, []any{reviewCommentJSON(9, &line)})})
	client := NewClient(transport, StaticToken{strPtr("t")})
	comments, err := client.ListReviewComments("o", "r", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(comments) != 1 || comments[0].ID != 9 || comments[0].User != "reviewer" ||
		comments[0].Path != "src/thing.py" || comments[0].Line == nil || *comments[0].Line != 12 {
		t.Fatalf("got %+v", comments)
	}
	if transport.Calls[0].Path != "/repos/o/r/pulls/5/comments?per_page=100" {
		t.Fatalf("got %s", transport.Calls[0].Path)
	}
}

func TestListReviewCommentsToleratesANullLineOnAnOutdatedComment(t *testing.T) {
	transport := NewFakeTransport(ApiResponse{Status: 200, Body: mustJSON(t, []any{reviewCommentJSON(9, nil)})})
	client := NewClient(transport, StaticToken{strPtr("t")})
	comments, err := client.ListReviewComments("o", "r", 5)
	if err != nil {
		t.Fatal(err)
	}
	if comments[0].Line != nil {
		t.Fatalf("got %+v", comments[0].Line)
	}
}

func TestListReviewCommentsFollowsLinkHeaderPagination(t *testing.T) {
	one, two := 1, 2
	transport := &FakeTransport{Responses: []ApiResponse{
		{Status: 200, Headers: map[string]string{"Link": `<https://api.github.com/repos/o/r/pulls/5/comments?page=2>; rel="next"`},
			Body: mustJSON(t, []any{reviewCommentJSON(1, &one)})},
		{Status: 200, Body: mustJSON(t, []any{reviewCommentJSON(2, &two)})},
	}}
	client := NewClient(transport, StaticToken{strPtr("t")})
	comments, err := client.ListReviewComments("o", "r", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(comments) != 2 || comments[0].ID != 1 || comments[1].ID != 2 {
		t.Fatalf("got %+v", comments)
	}
}

func TestListReviewCommentsRaisesOnANon200(t *testing.T) {
	transport := NewFakeTransport(ApiResponse{Status: 500, Body: []byte("boom")})
	client := NewClient(transport, StaticToken{strPtr("t")})
	if _, err := client.ListReviewComments("o", "r", 5); err == nil {
		t.Fatal("expected an error")
	}
}

func commentJSON(id int, user, body string) map[string]any {
	return map[string]any{"id": id, "user": map[string]string{"login": user}, "body": body}
}

func TestListCommentsReadsThePlainCommentShape(t *testing.T) {
	transport := NewFakeTransport(ApiResponse{Status: 200, Body: mustJSON(t, []any{commentJSON(9, "human", "here's my answer")})})
	client := NewClient(transport, StaticToken{strPtr("t")})
	comments, err := client.ListComments("o", "r", 5)
	if err != nil {
		t.Fatal(err)
	}
	want := Comment{ID: 9, User: "human", Body: "here's my answer", AuthorAssociation: "NONE"}
	if len(comments) != 1 || comments[0] != want {
		t.Fatalf("got %+v want %+v", comments, want)
	}
	if transport.Calls[0].Path != "/repos/o/r/issues/5/comments?per_page=100" {
		t.Fatalf("got %s", transport.Calls[0].Path)
	}
}

func TestListCommentsFollowsLinkHeaderPagination(t *testing.T) {
	transport := &FakeTransport{Responses: []ApiResponse{
		{Status: 200, Headers: map[string]string{"Link": `<https://api.github.com/repos/o/r/issues/5/comments?page=2>; rel="next"`},
			Body: mustJSON(t, []any{commentJSON(1, "human", "a")})},
		{Status: 200, Body: mustJSON(t, []any{commentJSON(2, "human", "b")})},
	}}
	client := NewClient(transport, StaticToken{strPtr("t")})
	comments, err := client.ListComments("o", "r", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(comments) != 2 || comments[0].ID != 1 || comments[1].ID != 2 {
		t.Fatalf("got %+v", comments)
	}
}

func TestListCommentsRaisesOnANon200(t *testing.T) {
	transport := NewFakeTransport(ApiResponse{Status: 500, Body: []byte("boom")})
	client := NewClient(transport, StaticToken{strPtr("t")})
	if _, err := client.ListComments("o", "r", 5); err == nil {
		t.Fatal("expected an error")
	}
}

func TestCreateCommentPostsTheBodyAndReturnsTheNewId(t *testing.T) {
	transport := NewFakeTransport(ApiResponse{Status: 201, Body: mustJSON(t, map[string]int{"id": 999})})
	client := NewClient(transport, StaticToken{strPtr("t")})
	id, err := client.CreateComment("o", "r", 5, "a question for you")
	if err != nil || id != 999 {
		t.Fatalf("id=%d err=%v", id, err)
	}
	call := transport.Calls[0]
	if call.Method != "POST" || call.Path != "/repos/o/r/issues/5/comments" {
		t.Fatalf("got %+v", call)
	}
	var body map[string]string
	json.Unmarshal(call.Body, &body)
	if body["body"] != "a question for you" {
		t.Fatalf("got %+v", body)
	}
}

func TestCreateCommentRaisesOnANon201(t *testing.T) {
	transport := NewFakeTransport(ApiResponse{Status: 404, Body: []byte("not found")})
	client := NewClient(transport, StaticToken{strPtr("t")})
	if _, err := client.CreateComment("o", "r", 5, "a question"); err == nil {
		t.Fatal("expected an error")
	}
}

func TestCreateReviewPostsADraftWithNoEventKey(t *testing.T) {
	transport := NewFakeTransport(ApiResponse{Status: 200, Body: mustJSON(t, map[string]int{"id": 321})})
	client := NewClient(transport, StaticToken{strPtr("t")})
	id, err := client.CreateReview("o", "r", 5, "looks good overall",
		[]NewReviewComment{{Path: "src/thing.py", Line: 12, Body: "nit: typo"}})
	if err != nil || id != 321 {
		t.Fatalf("id=%d err=%v", id, err)
	}
	call := transport.Calls[0]
	if call.Method != "POST" || call.Path != "/repos/o/r/pulls/5/reviews" {
		t.Fatalf("got %+v", call)
	}
	var payload map[string]any
	json.Unmarshal(call.Body, &payload)
	if _, ok := payload["event"]; ok {
		t.Fatal("expected no event key")
	}
	if payload["body"] != "looks good overall" {
		t.Fatalf("got %+v", payload)
	}
	comments, _ := payload["comments"].([]any)
	if len(comments) != 1 {
		t.Fatalf("got %+v", payload["comments"])
	}
	comment := comments[0].(map[string]any)
	if comment["path"] != "src/thing.py" || comment["body"] != "nit: typo" {
		t.Fatalf("got %+v", comment)
	}
}

func TestCreateReviewWithNoInlineCommentsPostsAnEmptyList(t *testing.T) {
	transport := NewFakeTransport(ApiResponse{Status: 200, Body: mustJSON(t, map[string]int{"id": 1})})
	client := NewClient(transport, StaticToken{strPtr("t")})
	if _, err := client.CreateReview("o", "r", 5, "LGTM", nil); err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	json.Unmarshal(transport.Calls[0].Body, &payload)
	comments, ok := payload["comments"].([]any)
	if !ok || len(comments) != 0 {
		t.Fatalf("got %+v", payload["comments"])
	}
}

func TestCreateReviewRaisesOnANon200(t *testing.T) {
	transport := NewFakeTransport(ApiResponse{Status: 422, Body: []byte("unprocessable")})
	client := NewClient(transport, StaticToken{strPtr("t")})
	if _, err := client.CreateReview("o", "r", 5, "x", nil); err == nil {
		t.Fatal("expected an error")
	}
}

// captureStdout runs fn with os.Stdout replaced, returning what it wrote.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stdout
	os.Stdout = w
	fn()
	w.Close()
	os.Stdout = orig
	var buf bytes.Buffer
	io.Copy(&buf, r)
	return buf.String()
}

func TestDryRunClientPrintsTheReviewInsteadOfPostingIt(t *testing.T) {
	inner := NewClient(NewFakeTransport(), StaticToken{strPtr("t")})
	dry := DryRunClient{Inner: inner}
	var id int
	out := captureStdout(t, func() {
		var err error
		id, err = dry.CreateReview("o", "r", 5, "looks good", []NewReviewComment{{Path: "a.py", Line: 1, Body: "nit"}})
		if err != nil {
			t.Fatal(err)
		}
	})
	if id != 0 {
		t.Fatalf("got %d", id)
	}
	if !strings.Contains(out, "draft review on o/r#5") {
		t.Fatalf("got %q", out)
	}
}

func TestDryRunClientPassesPrReadsThrough(t *testing.T) {
	transport := NewFakeTransport(
		ApiResponse{Status: 200, Body: mustJSON(t, prJSON(1, "feature-branch", "main"))},
		ApiResponse{Status: 200, Body: mustJSON(t, []any{reviewCommentJSON(1, intPtr(1))})},
		ApiResponse{Status: 200, Body: mustJSON(t, map[string]string{"default_branch": "main"})},
	)
	dry := DryRunClient{Inner: NewClient(transport, StaticToken{strPtr("t")})}
	if _, err := dry.GetPullRequest("o", "r", 1); err != nil {
		t.Fatal(err)
	}
	comments, err := dry.ListReviewComments("o", "r", 1)
	if err != nil || len(comments) != 1 || comments[0].ID != 1 {
		t.Fatalf("comments=%+v err=%v", comments, err)
	}
	branch, err := dry.DefaultBranch("o", "r")
	if err != nil || branch != "main" {
		t.Fatalf("branch=%q err=%v", branch, err)
	}
	if len(transport.Calls) != 3 {
		t.Fatalf("expected 3 calls to reach the transport, got %d", len(transport.Calls))
	}
}

func intPtr(n int) *int { return &n }

func TestDryRunClientPassesReadsThroughButPrintsMutations(t *testing.T) {
	transport := NewFakeTransport(
		ApiResponse{Status: 200, Body: mustJSON(t, []any{issueJSONFixture(1, false)})},
		ApiResponse{Status: 200, Body: mustJSON(t, issueJSONFixture(1, false))},
		ApiResponse{Status: 200, Body: []byte("{}")},
	)
	real := NewClient(transport, StaticToken{strPtr("t")})
	dry := DryRunClient{Inner: real}

	out := captureStdout(t, func() {
		issues, err := dry.ListIssues("o", "r", "grain-agent")
		if err != nil || len(issues) != 1 {
			t.Fatalf("issues=%+v err=%v", issues, err)
		}
		issue, err := dry.GetIssue("o", "r", 1)
		if err != nil {
			t.Fatal(err)
		}
		_ = issue
		exists, err := dry.BranchExists("o", "r", "grain/issue-1")
		if err != nil || !exists {
			t.Fatalf("exists=%v err=%v", exists, err)
		}
		dry.AddLabel("o", "r", 1, "grain-agent-in-progress")
		dry.RemoveLabel("o", "r", 1, "grain-agent")
		dry.CloseIssue("o", "r", 1)
		dry.ReopenIssue("o", "r", 1)
		if _, err := dry.CreatePullRequest("o", "r", "grain/issue-1", "main", "x", ""); err != nil {
			t.Fatal(err)
		}
	})
	for _, want := range []string{"add label", "remove label", "close issue", "reopen issue", "open PR"} {
		if !strings.Contains(out, want) {
			t.Fatalf("expected output to contain %q, got %q", want, out)
		}
	}
	// Only the three reads (list_issues, get_issue, branch_exists) actually
	// reached the transport -- every mutation, including PR creation,
	// closing and reopening the issue, only printed.
	if len(transport.Calls) != 3 {
		t.Fatalf("expected 3 calls to reach the transport, got %d", len(transport.Calls))
	}
}

func TestDryRunClientPassesCheckRunsThroughButPrintsIssueAndMerge(t *testing.T) {
	transport := NewFakeTransport(ApiResponse{Status: 200, Body: mustJSON(t, checkRunsJSON(
		CheckRun{Name: "build", Status: "completed", Conclusion: strPtr("success")},
	))})
	dry := DryRunClient{Inner: NewClient(transport, StaticToken{strPtr("t")})}
	out := captureStdout(t, func() {
		runs, err := dry.ListCheckRuns("o", "r", "feature-x")
		if err != nil || len(runs) != 1 {
			t.Fatalf("runs=%+v err=%v", runs, err)
		}
		if _, err := dry.CreateIssue("o", "r", "fix it", "", []string{"grain-agent-needs-approval"}); err != nil {
			t.Fatal(err)
		}
		if err := dry.MergePullRequest("o", "r", 5); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, "file issue") || !strings.Contains(out, "merge PR") {
		t.Fatalf("got %q", out)
	}
	if len(transport.Calls) != 1 {
		t.Fatalf("expected 1 call to reach the transport, got %d", len(transport.Calls))
	}
}

func TestDryRunClientPassesListCommentsThroughButPrintsCreateComment(t *testing.T) {
	transport := NewFakeTransport(ApiResponse{Status: 200, Body: mustJSON(t, []any{commentJSON(1, "human", "hi")})})
	dry := DryRunClient{Inner: NewClient(transport, StaticToken{strPtr("t")})}
	out := captureStdout(t, func() {
		comments, err := dry.ListComments("o", "r", 1)
		if err != nil || len(comments) != 1 {
			t.Fatalf("comments=%+v err=%v", comments, err)
		}
		if _, err := dry.CreateComment("o", "r", 1, "a question for you"); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, "comment on") || !strings.Contains(out, "a question for you") {
		t.Fatalf("got %q", out)
	}
	if len(transport.Calls) != 1 {
		t.Fatalf("expected 1 call to reach the transport, got %d", len(transport.Calls))
	}
}

// TestFakeTransportDefaultAnswersOnceResponsesAreExhausted proves the
// convenience NewFakeTransport gives every test above: a call past the
// scripted responses gets Default rather than a panic, so a test only
// scripts the calls it cares about.
func TestFakeTransportDefaultAnswersOnceResponsesAreExhausted(t *testing.T) {
	transport := NewFakeTransport()
	resp, err := transport.Request("GET", "/anything", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Status != 200 || string(resp.Body) != "[]" {
		t.Fatalf("got %+v", resp)
	}
}
