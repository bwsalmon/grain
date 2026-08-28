package orchestrator

import (
	"testing"

	"github.com/bwsalmon/grain/v2/pkg/github"
	"github.com/bwsalmon/grain/v2/pkg/model"
)

func TestHealthFromClosedStateReadsClosedRegardlessOfChecks(t *testing.T) {
	failure := "failure"
	got := healthFrom(github.PullRequestDetail{State: "closed"}, []github.CheckRun{
		{Status: "completed", Conclusion: &failure},
	})
	if got != model.PrClosed {
		t.Fatalf("got %q, want closed", got)
	}
}

func TestHealthFromUnknownMergeabilityWithNoFailingChecksIsUnknown(t *testing.T) {
	got := healthFrom(github.PullRequestDetail{State: "open"}, nil)
	if got != model.PrUnknown {
		t.Fatalf("got %q, want unknown", got)
	}
}

func TestHealthFromNotMergeableIsConflicted(t *testing.T) {
	no := false
	got := healthFrom(github.PullRequestDetail{State: "open", Mergeable: &no}, nil)
	if got != model.PrConflicted {
		t.Fatalf("got %q, want conflicted", got)
	}
}

func TestHealthFromAFailedCompletedCheckIsFailing(t *testing.T) {
	yes := true
	failure := "failure"
	got := healthFrom(github.PullRequestDetail{State: "open", Mergeable: &yes}, []github.CheckRun{
		{Name: "build", Status: "completed", Conclusion: &failure},
	})
	if got != model.PrFailing {
		t.Fatalf("got %q, want failing", got)
	}
}

func TestHealthFromAnInProgressCheckIsNotYetFailing(t *testing.T) {
	yes := true
	got := healthFrom(github.PullRequestDetail{State: "open", Mergeable: &yes}, []github.CheckRun{
		{Name: "build", Status: "in_progress"},
	})
	if got != model.PrClean {
		t.Fatalf("got %q, want clean (an in-progress check is not a failure)", got)
	}
}

func TestHealthFromMergeableWithNoFailingChecksIsClean(t *testing.T) {
	yes := true
	got := healthFrom(github.PullRequestDetail{State: "open", Mergeable: &yes}, []github.CheckRun{
		{Name: "build", Status: "completed", Conclusion: strPtr("success")},
	})
	if got != model.PrClean {
		t.Fatalf("got %q, want clean", got)
	}
}

func strPtr(s string) *string { return &s }
