package antigravity

import (
	"regexp"
	"strings"
	"time"

	"github.com/bwsalmon/grain/pkg/agent"
)

// agent/claude's usagelimit.go has the reasoning this file shares: a run
// that ends because the credential it runs as has no budget left in its
// current window is not a run that failed on its own terms, and
// retrying it -- or dispatching the next task -- spends the whole queue
// on the same refusal. agent.UsageLimitError is how that is said to
// orchestrator.RunDispatch, which pauses dispatch until the window
// resets instead.
//
// What differs is the vocabulary. agy talks to the Gemini API, whose
// refusal is a RESOURCE_EXHAUSTED status with a quota message, and which
// -- unlike Claude Code's absolute epoch -- reports when to come back as
// a delay ("retryDelay":"56s"). Both spellings end up in the same
// agent.UsageLimitError; see its ResetAt/RetryAfter fields.
const usageLimitFramework = "antigravity"

// usageLimitMarkers are the phrases that mean "out of quota" in agy's
// own report of a failed run. Lowercase, matched against a lowercased
// haystack.
//
// A bare "429" is deliberately absent, for the same reason it is in
// agent/claude: it is three digits that a repository's own files and
// test output are full of, and some of that text passes through this
// same stream.
var usageLimitMarkers = []string{
	"resource_exhausted",
	"quota exceeded",
	"exceeded your current quota",
	"rate limit exceeded",
	"rate_limit_exceeded",
	"ratelimitexceeded",
	"usage limit reached",
}

// retryDelayPattern is the Gemini API's own "come back in": a
// RetryInfo detail rendered into the error body as
// "retryDelay": "56s". Seconds are the only unit it uses in practice,
// but the pattern admits a fractional value ("1.5s") since the field is
// a protobuf Duration.
var retryDelayPattern = regexp.MustCompile(`(?i)"?retry_?delay"?\s*[:=]\s*"?(\d+(?:\.\d+)?)s`)

// usageLimitFailure reads a failed run's own account of itself -- the
// terminal result event's text and whatever the subprocess reported --
// and returns the limit it describes, or nil when it describes something
// else entirely (which is every ordinary failure).
//
// Unlike agent/claude there is no second, stricter reading for a
// *successful* run: agy reports a quota refusal as a failed terminal
// status, never as the run's final answer, so nothing here has to tell
// the model quoting the phrase apart from the CLI reporting it.
func usageLimitFailure(resultText string, runErr error) *agent.UsageLimitError {
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
	if d, ok := retryDelay(text); ok {
		limit.RetryAfter = d
	}
	return limit
}

// mentionsUsageLimit reports whether text carries any of the markers.
func mentionsUsageLimit(text string) bool {
	lower := strings.ToLower(text)
	for _, marker := range usageLimitMarkers {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// retryDelay reads the API's own "retryDelay" out of text, when it named
// one. A delay of zero reads as "none given" rather than "come back
// immediately": the caller's own floor is the safer reading of a
// provider that has just refused a request (agent.UsageLimitError.
// ResumeAt makes the same argument for a reset already in the past).
func retryDelay(text string) (time.Duration, bool) {
	m := retryDelayPattern.FindStringSubmatch(text)
	if m == nil {
		return 0, false
	}
	d, err := time.ParseDuration(m[1] + "s")
	if err != nil || d <= 0 {
		return 0, false
	}
	return d, true
}
