package antigravity

import (
	"bufio"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// docs/agy-surface.md is a capture of what a real agy said about itself
// (scripts/agy-surface.sh, run by .github/workflows/agy-surface.yml), and
// the whole reason it is committed rather than left as a CI artifact is so
// that this package can be checked against it. Everything below is that
// check: no agy, no network and no credential, just the binary's own
// answers read back out of the tree.
//
// What this cannot do is notice that the capture is old. A file nobody has
// regenerated agrees with itself forever, and agy is installed unpinned,
// so a green run here means "this package matches the last agy anyone
// asked", not "this package matches the agy an image built today would
// carry". Dispatching agy-surface is what turns the second question into
// the first.

const surfaceCapturePath = "../../../docs/agy-surface.md"

// capturedRoster returns the native tool names in the capture's `init`
// event, and the count agy itself reported alongside them.
//
// It reads the section scripts/agy-surface.sh writes as a sorted list
// under a "native tools: N" line rather than parsing the raw JSON event,
// because that list is the one a human reads in the diff when a new
// capture lands -- a test agreeing with a different copy of the roster
// than the reviewer is looking at would be worth very little.
func capturedRoster(t *testing.T) (names []string, reported int) {
	t.Helper()

	f, err := os.Open(surfaceCapturePath)
	if err != nil {
		t.Fatalf("reading the committed agy capture: %v", err)
	}
	defer f.Close()

	const fence = "``````"
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 1<<20), 1<<20)

	inRoster := false
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "native tools: "):
			inRoster = true
			reported, err = strconv.Atoi(strings.TrimPrefix(line, "native tools: "))
			if err != nil {
				t.Fatalf("the capture's roster count is not a number: %q", line)
			}
		case inRoster && strings.HasPrefix(line, fence):
			inRoster = false
		case inRoster && line != "" && !strings.HasPrefix(line, "permission_mode:"):
			names = append(names, line)
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("reading the committed agy capture: %v", err)
	}

	if len(names) == 0 {
		t.Fatalf("%s holds no init-event roster. Either the capture is the placeholder that ships before the first dispatch, or agy stopped emitting the event -- and neither is something this package should be asserting against.", surfaceCapturePath)
	}
	if reported != len(names) {
		t.Errorf("the capture reports %d native tools and lists %d", reported, len(names))
	}
	return names, reported
}

// TestWithheldNativeToolsAreToolsAgyStillHas is the check that a denial
// still denies something.
//
// HookDecision matches a hook payload's tool name against
// withheldNativeTools exactly, so a name agy no longer uses -- renamed,
// dropped, or mistyped when it was added here -- is not a harmless extra
// entry. It is a line of this package's only working defence that can
// never match, and nothing else in the repository would say so: the unit
// tests assert that grain denies the names in this list, which stays true
// however wrong the names are.
func TestWithheldNativeToolsAreToolsAgyStillHas(t *testing.T) {
	roster, _ := capturedRoster(t)
	has := make(map[string]bool, len(roster))
	for _, name := range roster {
		has[name] = true
	}

	for _, withheld := range withheldNativeTools {
		if !has[withheld] {
			t.Errorf("withheldNativeTools denies %q, which the captured agy does not offer. Either agy renamed or dropped it -- in which case this list needs the new name, not just the old one removed -- or the name was never right.", withheld)
		}
	}
}

// TestCapturedRosterHoldsNoUntriagedTool is the check in the other
// direction, and the one with the shorter fuse: a tool agy has *added*
// since withheldNativeTools was written is a tool grain's hook does not
// deny.
//
// The list below is not a second copy of the roster for its own sake --
// the capture is the roster. It is the record of which of those names a
// human has looked at and decided about, which is a different fact and
// one no capture can carry. So a new agy bringing a new tool fails here
// rather than passing quietly, and the fix is to decide: deny it in
// withheldNativeTools if it reads, writes, executes or delegates, and
// otherwise add it here with the rest.
//
// Everything not in withheldNativeTools is here because it does not reach
// the controller's filesystem: agy's browser, its knowledge store, its
// task and inbox bookkeeping, the permission and question tools that ask
// rather than act, and `call_mcp_tool`, which reaches grain's own server
// and nothing else. The two exceptions worth naming are `manage_subagents`
// and `browser_subagent`: they are left alone because neither creates a
// subagent that could hold write tools (`define_subagent` does, and is
// denied), but they are the names to look at first if agy's subagent
// surface changes again.
var triagedNativeTools = []string{
	"ask_custom_permission", "ask_permission", "ask_question",
	"browser_click_element", "browser_drag_pixel_to_pixel",
	"browser_get_dom", "browser_get_network_request", "browser_input",
	"browser_list_network_requests", "browser_mouse_down",
	"browser_mouse_up", "browser_move_mouse", "browser_press_key",
	"browser_refresh_page", "browser_resize_window", "browser_scroll",
	"browser_scroll_dom", "browser_select_option", "browser_subagent",
	"call_mcp_tool", "capture_browser_console_logs",
	"capture_browser_screenshot", "click_browser_pixel",
	"delete_knowledge", "execute_browser_javascript", "finish",
	"generate_image", "list_browser_pages", "list_permissions",
	"list_resources", "manage_inbox", "manage_subagents", "manage_task",
	"open_browser_url", "read_browser_page", "read_resource",
	"read_url_content", "schedule", "search_web", "send_message",
	"wait", "wait_5_seconds",
}

func TestCapturedRosterHoldsNoUntriagedTool(t *testing.T) {
	roster, _ := capturedRoster(t)

	known := make(map[string]bool, len(triagedNativeTools)+len(withheldNativeTools))
	for _, name := range triagedNativeTools {
		known[name] = true
	}
	for _, name := range withheldNativeTools {
		known[name] = true
	}

	for _, name := range roster {
		if !known[name] {
			t.Errorf("the captured agy offers %q, which nothing in this package has an opinion about. Decide: add it to withheldNativeTools if it reads, writes, executes or delegates on the controller, and to triagedNativeTools if it does not.", name)
		}
	}

	// The other way round, so that a tool agy drops does not sit in the
	// triaged list forever pretending someone still checks it.
	offered := make(map[string]bool, len(roster))
	for _, name := range roster {
		offered[name] = true
	}
	for _, name := range triagedNativeTools {
		if !offered[name] {
			t.Errorf("triagedNativeTools names %q, which the captured agy no longer offers; drop it.", name)
		}
	}
}

// TestCapturedPathsAreThePathsThisPackageWritesTo checks the three
// filenames in this package against the capture's own candidate-path
// probes, which plant a marker in every plausible location and report
// which ones agy came back holding.
//
// This is the mistake that has actually happened here (see the README on
// ~/.gemini/settings.json): a config written one directory from where agy
// reads it produces no error at all, just a run with no MCP servers, no
// rules or no hook. Every test in this package that is not this one drives
// a stub, and a stub reads whichever file the test told it to.
func TestCapturedPathsAreThePathsThisPackageWritesTo(t *testing.T) {
	capture, err := os.ReadFile(surfaceCapturePath)
	if err != nil {
		t.Fatalf("reading the committed agy capture: %v", err)
	}
	text := string(capture)

	for _, path := range []string{mcpConfigRelPath, cliSettingsRelPath, hooksConfigRelPath} {
		want := "READ       ~/" + filepath.ToSlash(path)
		if !strings.Contains(text, want) {
			t.Errorf("this package writes ~/%s, and the capture does not report agy reading it. Look for a %q line in %s: if it says \"not listed\", agy is loading nothing this package writes there.",
				filepath.ToSlash(path), "~/"+filepath.ToSlash(path), surfaceCapturePath)
		}
	}
}
