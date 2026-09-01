package orchestrator

import (
	"context"
	"fmt"
	"strings"

	"github.com/bwsalmon/grain/v2/pkg/mcp"
	"github.com/bwsalmon/grain/v2/pkg/model"
)

// AttachmentsDir is where a dispatched run finds every file its task or
// its conversation carries (bwsalmon/agents#522), relative to the
// sandbox's own working directory -- named apart from CheckoutDir
// ("work") so a repo clone never collides with it, and fixed rather than
// configurable so BuildPrompt can tell an agent exactly where to look.
const AttachmentsDir = "attachments"

// writeFileTool picks the write_file handler out of the tools a slot's
// sandbox exposes -- runCommandTool's own counterpart (checkout.go), and
// for the same reason: it is the one call already right for both a local
// HostSandboxes directory and a KonturSandboxes VM reached over SSH
// (mcp.NewSandboxTools and mcp.NewSSHSandboxTools both expose it under
// this name). A capability's own SideSandbox placement (applyPlacements)
// has no such reach: it only ever lands where a rootedSandbox
// hands out a local directory to write into directly (cycle.go's own
// runOne refuses a task's capability grants against anything else). An
// attachment inherits no such restriction, which is the whole reason to
// place it this way instead.
func writeFileTool(tools []mcp.Tool) (func(context.Context, map[string]any) mcp.Result, bool) {
	for _, t := range tools {
		if t.Name == "write_file" && t.Handler != nil {
			return t.Handler, true
		}
	}
	return nil, false
}

// attachmentPath is where one attachment lands inside AttachmentsDir --
// its own id first, so two attachments can never collide even when a
// human (or an earlier attempt relaying its own upload) reused a
// filename, followed by the filename itself so an agent reading a
// directory listing still sees a name that means something.
func attachmentPath(a model.Attachment) string {
	return fmt.Sprintf("%s/%d-%s", AttachmentsDir, a.ID, a.Filename)
}

// placeAttachments writes every one of attachments (a task's own body's,
// plus every comment's -- Store.Attachments already returns both) into
// the sandbox via the slot's own write_file tool, and returns a prompt
// section naming where each one landed -- "" if there are none. Content
// crosses this call as a plain Go string, byte for byte: unlike an
// attachment's own trip through the JSON API (where it must be base64,
// since a JSON string has to be valid UTF-8), this is an ordinary
// in-process function call with no encoding boundary of its own to cross,
// the same reasoning that lets applyPlacements write a capability's own
// Placement.Content with a bare []byte(content) conversion.
func placeAttachments(ctx context.Context, tools []mcp.Tool, attachments []model.Attachment) (string, error) {
	if len(attachments) == 0 {
		return "", nil
	}
	write, ok := writeFileTool(tools)
	if !ok {
		return "", fmt.Errorf("orchestrator: slot's sandbox exposes no write_file tool to place attachments with")
	}

	var b strings.Builder
	b.WriteString("\n\nFiles attached to this task are available in the sandbox:\n")
	for _, a := range attachments {
		p := attachmentPath(a)
		if result := write(ctx, map[string]any{"file_path": p, "content": string(a.Content)}); result.IsError {
			return "", fmt.Errorf("orchestrator: writing attachment %d (%s): %s", a.ID, a.Filename, strings.TrimSpace(result.Text))
		}
		origin := "the task description"
		if a.CommentID != nil {
			origin = fmt.Sprintf("comment #%d", *a.CommentID)
		}
		fmt.Fprintf(&b, "- ./%s (from %s)\n", p, origin)
	}
	return b.String(), nil
}
