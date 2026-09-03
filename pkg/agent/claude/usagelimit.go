package claude

import (
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/bwsalmon/grain/pkg/agent"
)

// A run that ends because the Claude Code account has no budget left in
// its current window is not a run that failed on its own terms, and
// treating it as one costs the whole queue: dispatch retries the task,
// the retry hits the same wall, and every other task in the deployment
// follows it there. agent.UsageLimitError is what says so to a caller,
// and this file is what recognises the condition in what the CLI
// actually prints.
//
// Two forms are recognised, deliberately at different strictnesses.
//
// The strict one is the CLI's own machine-readable report of an
// OAuth-account limit: the exact text "Claude AI usage limit
// reached|<unix seconds>", which it emits as the run's terminal result
// -- sometimes as a failure, sometimes as an ordinary final answer with
// exit 0. Because that second shape reaches us through a *successful*
// parse, matching on it has to be tight enough that an agent quoting the
// phrase back at us (a run reading this very file, say) cannot trip it:
// the "|<epoch>" suffix is what makes it a report from the CLI rather
// than a sentence from the model.
//
// The loose one is every other way a provider says "not now" -- an API
// 429's rate_limit_error, a quota refusal -- and is only ever consulted
// for a run that has *already* failed, where the alternative reading is
// not "the agent said something" but "the run broke for reasons we
// cannot name".
const (
	// usageLimitFramework names this framework in the errors below, so a
	// deployment running both frameworks can tell whose credential is out
	// of budget from the run's own detail.
	usageLimitFramework = "claude"
)

// resetEpochPattern is the strict form: the CLI's own
// "...limit reached|<unix seconds>". The digit bound admits both a
// seconds epoch (10 digits until 2286) and a milliseconds one, which
// resetFromEpoch tells apart; it excludes a short number that happened
// to follow a pipe.
var resetEpochPattern = regexp.MustCompile(`(?i)usage limit reached\|(\d{9,13})`)

// looseUsageLimitMarkers are the phrases that mean "out of budget" in
// text that already belongs to a failed run. Lowercase, matched against
// a lowercased haystack.
//
// "429" is deliberately not one of them on its own: it is three digits
// that appear in file paths, diffs and test output, and the run whose
// text this reads is one whose sandbox has been echoing all three back
// for an hour. Every marker here is a phrase a provider writes and a
// repository does not.
var looseUsageLimitMarkers = []string{
	"usage limit reached",
	"usage limit exceeded",
	"rate_limit_error",
	"rate limit exceeded",
	"exceeded your rate limit",
}

// usageLimitFromResult reads the terminal result event's own text under
// the strict rule above -- the one detection that is allowed to turn a
// *successful* claude run into an error, because the CLI reports an
// account limit as the run's final answer as readily as it reports it as
// a failure. nil means this result says nothing about a limit, which is
// every ordinary run.
func usageLimitFromResult(resultText string) *agent.UsageLimitError {
	m := resetEpochPattern.FindStringSubmatch(resultText)
	if m == nil {
		return nil
	}
	return &agent.UsageLimitError{
		Framework: usageLimitFramework,
		Message:   agent.TrimLimitMessage(resultText),
		ResetAt:   resetFromEpoch(m[1]),
	}
}

// usageLimitFromFailure reads a failed run's own account of itself --
// the terminal result event's text and whatever the subprocess itself
// reported (a non-zero exit with the API's error body on stderr) --
// under the loose rule. nil means the run failed for some other reason,
// which is what a caller falls back to its ordinary diagnosis for.
func usageLimitFromFailure(resultText string, runErr error) *agent.UsageLimitError {
	text := resultText
	if runErr != nil {
		text = strings.TrimSpace(text + "\n" + runErr.Error())
	}
	if !mentionsUsageLimit(text) {
		return nil
	}
	limit := &agent.UsageLimitError{
		Framework: usageLimitFramework,
		Message:   agent.TrimLimitMessage(text),
		Err:       runErr,
	}
	if m := resetEpochPattern.FindStringSubmatch(text); m != nil {
		limit.ResetAt = resetFromEpoch(m[1])
	}
	return limit
}

// mentionsUsageLimit reports whether text carries any of the loose
// markers.
func mentionsUsageLimit(text string) bool {
	lower := strings.ToLower(text)
	for _, marker := range looseUsageLimitMarkers {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// resetFromEpoch turns the digits captured after the pipe into an
// instant, reading a 12-or-more-digit number as milliseconds and
// anything shorter as seconds -- the two spellings a Unix epoch arrives
// in, told apart by magnitude rather than by trusting one. A number that
// does not parse at all (longer than an int64) yields the zero time,
// which agent.UsageLimitError already reads as "the provider named no
// reset".
func resetFromEpoch(digits string) time.Time {
	n, err := strconv.ParseInt(digits, 10, 64)
	if err != nil {
		return time.Time{}
	}
	if len(digits) >= 12 {
		return time.UnixMilli(n).UTC()
	}
	return time.Unix(n, 0).UTC()
}
