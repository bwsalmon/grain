package mcp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// NewSandboxTools returns the four tools v1 runs over SSH against a real
// assigned sandbox VM (run_command, read_file, edit_file, write_file),
// ported field-for-field: same names, same input schemas, same output
// shapes. Most callers here still have no host adapter to give these a
// real remote VM to run against, so root stands in for the sandbox --
// every tool here is confined to it. That confinement is v1's own boundary
// substitute, not something v1 needed: its actual isolation is the
// sandbox VM itself. NewSSHSandboxTools (ssh_tools.go) is the same four
// tools with that boundary lifted, run against a real remote host instead
// -- what a kontur-managed sandbox VM needs (bwsalmon/agents#256).
func NewSandboxTools(root string) []Tool {
	return []Tool{
		runCommandTool(root),
		readFileTool(root),
		editFileTool(root),
		writeFileTool(root),
	}
}

// resolvePath maps a tool-supplied path onto one inside root, rejecting
// anything -- absolute or relative-with-.. -- that would land outside it.
func resolvePath(root, p string) (string, error) {
	if p == "" {
		return "", errors.New("file_path is required")
	}
	var full string
	if filepath.IsAbs(p) {
		full = filepath.Clean(p)
	} else {
		full = filepath.Clean(filepath.Join(root, p))
	}
	rootClean := filepath.Clean(root)
	if full != rootClean && !strings.HasPrefix(full, rootClean+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q escapes the sandbox root", p)
	}
	return full, nil
}

// The four schemas below are shared with ssh_tools.go: NewSSHSandboxTools'
// tools run their work over SSH instead of locally, but advertise the
// exact same name/description/inputSchema shape a caller of this package
// already sees from NewSandboxTools, so which one a given mcpserver
// process was started with is invisible to the model calling them.
var runCommandInputSchema = map[string]any{
	"type":                 "object",
	"additionalProperties": false,
	"properties": map[string]any{
		"command": map[string]any{"type": "string"},
		"timeout": map[string]any{
			"type":        "number",
			"description": "Timeout in milliseconds, max 600000",
		},
		"description": map[string]any{
			"type":        "string",
			"description": "Short description of what this command does",
		},
	},
	"required": []string{"command"},
}

var readFileInputSchema = map[string]any{
	"type":                 "object",
	"additionalProperties": false,
	"properties": map[string]any{
		"file_path": map[string]any{"type": "string"},
		"offset":    map[string]any{"type": "integer", "description": "Line number to start from"},
		"limit":     map[string]any{"type": "integer", "description": "Number of lines to read"},
	},
	"required": []string{"file_path"},
}

var writeFileInputSchema = map[string]any{
	"type":                 "object",
	"additionalProperties": false,
	"properties": map[string]any{
		"file_path": map[string]any{"type": "string"},
		"content":   map[string]any{"type": "string"},
	},
	"required": []string{"file_path", "content"},
}

var editFileInputSchema = map[string]any{
	"type":                 "object",
	"additionalProperties": false,
	"properties": map[string]any{
		"file_path":   map[string]any{"type": "string"},
		"old_string":  map[string]any{"type": "string"},
		"new_string":  map[string]any{"type": "string"},
		"replace_all": map[string]any{"type": "boolean", "default": false},
	},
	"required": []string{"file_path", "old_string", "new_string"},
}

func runCommandTool(root string) Tool {
	return Tool{
		Name:        "run_command",
		Description: "Run a shell command in your assigned sandbox workspace.",
		InputSchema: runCommandInputSchema,
		Handler: func(args map[string]any) Result {
			command, ok := argString(args, "command")
			if !ok || command == "" {
				return Result{Text: "command is required", IsError: true}
			}

			ctx := context.Background()
			if ms, ok := argFloat(args, "timeout"); ok {
				seconds := int(ms / 1000)
				if seconds < 1 {
					seconds = 1
				}
				if seconds > 600 {
					seconds = 600
				}
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, time.Duration(seconds)*time.Second)
				defer cancel()
			}

			cmd := exec.CommandContext(ctx, "bash", "-c", command)
			cmd.Dir = root
			// HOME=root, not whatever this process's own HOME is, so that
			// anything a command reads or writes there -- notably
			// ~/.gitconfig and ~/.git-credentials (see
			// ConfigureGitCredentials) -- is confined to this sandbox
			// stand-in the same way its working directory already is.
			cmd.Env = append(os.Environ(), "HOME="+root)
			var stdout, stderr bytes.Buffer
			cmd.Stdout = &stdout
			cmd.Stderr = &stderr
			err := cmd.Run()

			exitCode := 0
			if err != nil {
				var exitErr *exec.ExitError
				if errors.As(err, &exitErr) {
					exitCode = exitErr.ExitCode()
				} else {
					exitCode = -1
					stderr.WriteString(err.Error())
				}
			}
			text := fmt.Sprintf("exit=%d\nstdout:\n%s\nstderr:\n%s", exitCode, stdout.String(), stderr.String())
			return Result{Text: text, IsError: exitCode != 0}
		},
	}
}

// linesFromContent splits content the way Python's splitlines() does: a
// trailing newline never produces a trailing empty "line", which the
// cat -n-style numbering numberedRange does depends on to avoid an
// off-by-one phantom last line. Shared with ssh_tools.go's read_file,
// which has no os.ReadFile of its own to call -- content there is
// whatever `cat` on the remote host returned.
func linesFromContent(content string) []string {
	lines := strings.Split(content, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" && strings.HasSuffix(content, "\n") {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// numberedRange renders lines[offset:offset+limit] (args' "offset"/"limit",
// clamped into range) in the same cat -n-style numbering read_file's model-
// facing output always uses.
func numberedRange(lines []string, args map[string]any) string {
	start := 0
	if v, ok := argFloat(args, "offset"); ok {
		start = int(v)
	}
	if start < 0 {
		start = 0
	}
	if start > len(lines) {
		start = len(lines)
	}
	end := len(lines)
	if v, ok := argFloat(args, "limit"); ok {
		end = start + int(v)
	}
	if end > len(lines) {
		end = len(lines)
	}

	var b strings.Builder
	for i := start; i < end; i++ {
		if i > start {
			b.WriteByte('\n')
		}
		fmt.Fprintf(&b, "%6d\t%s", i+1, lines[i])
	}
	return b.String()
}

func readFileTool(root string) Tool {
	return Tool{
		Name:        "read_file",
		Description: "Read a file from your assigned sandbox workspace.",
		InputSchema: readFileInputSchema,
		Handler: func(args map[string]any) Result {
			fp, _ := argString(args, "file_path")
			full, err := resolvePath(root, fp)
			if err != nil {
				return Result{Text: err.Error(), IsError: true}
			}
			data, err := os.ReadFile(full)
			if err != nil {
				return Result{Text: fmt.Sprintf("Error reading %s: %v", fp, err), IsError: true}
			}
			lines := linesFromContent(string(data))
			return Result{Text: numberedRange(lines, args)}
		},
	}
}

func writeFileTool(root string) Tool {
	return Tool{
		Name:        "write_file",
		Description: "Write (create or overwrite) a file in your assigned sandbox workspace.",
		InputSchema: writeFileInputSchema,
		Handler: func(args map[string]any) Result {
			fp, _ := argString(args, "file_path")
			content, ok := argString(args, "content")
			if !ok {
				return Result{Text: "content is required", IsError: true}
			}
			full, err := resolvePath(root, fp)
			if err != nil {
				return Result{Text: err.Error(), IsError: true}
			}
			if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
				return Result{Text: fmt.Sprintf("Error creating %s: %v", filepath.Dir(fp), err), IsError: true}
			}
			if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
				return Result{Text: fmt.Sprintf("Error writing %s: %v", fp, err), IsError: true}
			}
			return Result{Text: fmt.Sprintf("Wrote %s", fp)}
		},
	}
}

func editFileTool(root string) Tool {
	return Tool{
		Name:        "edit_file",
		Description: "Replace an exact string in a file in your assigned sandbox workspace.",
		InputSchema: editFileInputSchema,
		Handler: func(args map[string]any) Result {
			fp, _ := argString(args, "file_path")
			oldStr, hasOld := argString(args, "old_string")
			newStr, hasNew := argString(args, "new_string")
			if !hasOld || !hasNew {
				return Result{Text: "old_string and new_string are required", IsError: true}
			}
			replaceAll, _ := argBool(args, "replace_all")

			full, err := resolvePath(root, fp)
			if err != nil {
				return Result{Text: err.Error(), IsError: true}
			}
			data, err := os.ReadFile(full)
			if err != nil {
				return Result{Text: fmt.Sprintf("Error reading %s: %v", fp, err), IsError: true}
			}
			content := string(data)
			count := strings.Count(content, oldStr)
			if count == 0 {
				return Result{Text: fmt.Sprintf("String not found in file: %q", oldStr), IsError: true}
			}
			if count > 1 && !replaceAll {
				return Result{
					Text: fmt.Sprintf(
						"String appears %d times in the file. Use replace_all: true to replace "+
							"every occurrence, or provide more surrounding context to uniquely "+
							"identify the instance you mean.", count),
					IsError: true,
				}
			}
			n := 1
			if replaceAll {
				n = -1
			}
			newContent := strings.Replace(content, oldStr, newStr, n)
			if err := os.WriteFile(full, []byte(newContent), 0o644); err != nil {
				return Result{Text: fmt.Sprintf("Error writing %s: %v", fp, err), IsError: true}
			}
			return Result{Text: fmt.Sprintf("Wrote %s", fp)}
		},
	}
}
