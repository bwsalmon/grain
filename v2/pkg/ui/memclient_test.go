package ui

import (
	"sort"

	"github.com/bwsalmon/grain/v2/pkg/github"
)

// memClient is an in-memory github.Client stand-in for this package's own
// tests -- simpler to drive than github.FakeTransport for handler tests
// that need real state (labels changing under AddLabel/RemoveLabel,
// comments accumulating) rather than one scripted response per call.
type memClient struct {
	issues     map[int]*github.Issue
	comments   map[int][]github.Comment
	nextNumber int
}

func newMemClient() *memClient {
	return &memClient{issues: map[int]*github.Issue{}, comments: map[int][]github.Comment{}, nextNumber: 1}
}

// seed adds an issue directly, bypassing CreateIssue -- for tests that
// want to start from a task already in a particular state.
func (m *memClient) seed(number int, title, body string, labels ...string) {
	set := map[string]struct{}{}
	for _, l := range labels {
		set[l] = struct{}{}
	}
	m.issues[number] = &github.Issue{Number: number, Title: title, Body: body, State: "open", Labels: set, HTMLURL: "https://example.test/issues"}
	if number >= m.nextNumber {
		m.nextNumber = number + 1
	}
}

func (m *memClient) ListIssues(owner, repo, label string) ([]github.Issue, error) {
	var out []github.Issue
	for _, iss := range m.issues {
		if iss.State == "open" && iss.HasLabel(label) {
			out = append(out, *iss)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Number < out[j].Number })
	return out, nil
}

func (m *memClient) GetIssue(owner, repo string, number int) (github.Issue, error) {
	iss, ok := m.issues[number]
	if !ok {
		return github.Issue{}, &github.Error{Status: 404, Body: []byte("not found")}
	}
	return *iss, nil
}

func (m *memClient) AddLabel(owner, repo string, number int, label string) error {
	iss, ok := m.issues[number]
	if !ok {
		return &github.Error{Status: 404}
	}
	iss.Labels[label] = struct{}{}
	return nil
}

func (m *memClient) RemoveLabel(owner, repo string, number int, label string) error {
	iss, ok := m.issues[number]
	if !ok {
		return &github.Error{Status: 404}
	}
	delete(iss.Labels, label)
	return nil
}

func (m *memClient) CloseIssue(owner, repo string, number int) error {
	iss, ok := m.issues[number]
	if !ok {
		return &github.Error{Status: 404}
	}
	iss.State = "closed"
	return nil
}

func (m *memClient) ReopenIssue(owner, repo string, number int) error {
	iss, ok := m.issues[number]
	if !ok {
		return &github.Error{Status: 404}
	}
	iss.State = "open"
	return nil
}

func (m *memClient) CreateIssue(owner, repo, title, body string, labels []string) (github.Issue, error) {
	number := m.nextNumber
	m.nextNumber++
	set := map[string]struct{}{}
	for _, l := range labels {
		set[l] = struct{}{}
	}
	iss := github.Issue{Number: number, Title: title, Body: body, State: "open", Labels: set, HTMLURL: "https://example.test/issues", Author: "tester"}
	m.issues[number] = &iss
	return iss, nil
}

func (m *memClient) ListComments(owner, repo string, number int) ([]github.Comment, error) {
	return m.comments[number], nil
}

func (m *memClient) CreateComment(owner, repo string, number int, body string) (int, error) {
	id := len(m.comments[number]) + 1
	m.comments[number] = append(m.comments[number], github.Comment{ID: id, User: "tester", Body: body, AuthorAssociation: "OWNER"})
	return id, nil
}

// The rest of github.Client this package's handlers never call --
// stubbed to satisfy the interface only.

func (m *memClient) BranchExists(owner, repo, branch string) (bool, error) { return false, nil }
func (m *memClient) GetBranchHead(owner, repo, branch string) (*github.BranchHead, error) {
	return nil, nil
}
func (m *memClient) CreatePullRequest(owner, repo, head, base, title, body string) (github.PullRequest, error) {
	return github.PullRequest{}, nil
}
func (m *memClient) FindOpenPullRequestForBranch(owner, repo, branch string) (*github.PullRequest, error) {
	return nil, nil
}
func (m *memClient) MergePullRequest(owner, repo string, number int) error { return nil }
func (m *memClient) GetPullRequest(owner, repo string, number int) (github.PullRequestDetail, error) {
	return github.PullRequestDetail{}, nil
}
func (m *memClient) DefaultBranch(owner, repo string) (string, error) { return "main", nil }
func (m *memClient) ListReviewComments(owner, repo string, number int) ([]github.ReviewComment, error) {
	return nil, nil
}
func (m *memClient) ListCheckRuns(owner, repo, ref string) ([]github.CheckRun, error) {
	return nil, nil
}
func (m *memClient) CreateReview(owner, repo string, number int, body string, comments []github.NewReviewComment) (int, error) {
	return 0, nil
}

var _ github.Client = (*memClient)(nil)
