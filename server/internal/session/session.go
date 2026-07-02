// Package session drives Claude Code in headless stream-json mode and tracks
// sessions as durable records (a session_id on disk tied to a directory), not
// live processes. This is the data path for the voice interface: each dictated
// turn shells out to `claude -p --output-format stream-json` and the clean
// `result` text is returned for text-to-speech. See docs/protocol.md and the
// "TUI capture" decision in CLAUDE.md.
package session

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os/exec"
)

// Session is a durable record. There is no long-lived process: the conversation
// state lives on disk under SessionID and is reattached via `claude --resume`.
type Session struct {
	Name      string `json:"name"`       // human/voice handle, e.g. "claude-claude"
	Dir       string `json:"dir"`        // working directory for the session
	SessionID string `json:"session_id"` // claude session uuid (generated at spawn)
	Started   bool   `json:"started"`    // false until the first turn has run
}

// Driver runs Claude Code turns. It holds no per-session state.
type Driver struct {
	// Bin is the claude binary (default "claude").
	Bin string
	// Bypass adds --dangerously-skip-permissions when true (project default).
	Bypass bool
}

// NewDriver returns a Driver with project defaults.
func NewDriver() *Driver { return &Driver{Bin: "claude", Bypass: true} }

// ToolUse describes a tool Claude invoked during a turn. FilePath is set for
// file-editing tools (Edit/Write/MultiEdit/NotebookEdit) so the caller can show
// which file changed.
type ToolUse struct {
	Name     string
	FilePath string
}

// Turn sends one user message to the session and returns the assistant's final
// prose (the stream-json `result` event). onTool, if non-nil, is called for each
// tool Claude uses, so the caller can show activity ("thinking…", "editing
// foo.go") separately from the answer.
//
// The first turn (s.Started == false) creates the session with --session-id;
// later turns reattach with --resume. Turn flips s.Started to true on success —
// the caller is responsible for persisting the updated record.
func (d *Driver) Turn(ctx context.Context, s *Session, prompt string, onTool func(ToolUse)) (string, error) {
	if s.SessionID == "" {
		return "", fmt.Errorf("session %q has no SessionID", s.Name)
	}
	args := []string{"-p", prompt, "--output-format", "stream-json", "--verbose"}
	if s.Started {
		args = append(args, "--resume", s.SessionID)
	} else {
		args = append(args, "--session-id", s.SessionID)
	}
	if d.Bypass {
		args = append(args, "--dangerously-skip-permissions")
	}

	cmd := exec.CommandContext(ctx, d.Bin, args...)
	cmd.Dir = s.Dir
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", err
	}
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("start claude: %w", err)
	}

	reply, perr := parseStream(stdout, onTool)
	if werr := cmd.Wait(); werr != nil {
		return "", fmt.Errorf("claude exited: %w", werr)
	}
	if perr != nil {
		return "", perr
	}
	s.Started = true
	return reply, nil
}

// streamEvent is the subset of the stream-json schema we consume. Unknown fields
// are ignored; non-JSON lines are skipped by the scanner loop. Tool use is read
// from the full `assistant` message content (always present), not stream_event
// deltas (which require --include-partial-messages).
type streamEvent struct {
	Type    string `json:"type"`    // "system" | "assistant" | "user" | "result" | ...
	Subtype string `json:"subtype"` // on result: "success" | "error_*"
	IsError bool   `json:"is_error"`
	Result  string `json:"result"`
	Message struct {
		Content []struct {
			Type  string `json:"type"` // "text" | "tool_use"
			Name  string `json:"name"` // tool name when Type=="tool_use"
			Input struct {
				FilePath     string `json:"file_path"`
				NotebookPath string `json:"notebook_path"`
			} `json:"input"`
		} `json:"content"`
	} `json:"message"`
}

// parseStream reads NDJSON until EOF, returning the final result text.
func parseStream(r interface{ Read([]byte) (int, error) }, onTool func(ToolUse)) (string, error) {
	sc := bufio.NewScanner(r)
	// Events (especially tool inputs) can exceed the default 64KB line cap.
	sc.Buffer(make([]byte, 0, 1<<20), 1<<24)

	var result string
	var gotResult, isError bool
	var subtype string
	for sc.Scan() {
		var ev streamEvent
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			continue // defensively skip anything that isn't a JSON event
		}
		switch ev.Type {
		case "assistant":
			if onTool != nil {
				for _, b := range ev.Message.Content {
					if b.Type == "tool_use" {
						path := b.Input.FilePath
						if path == "" {
							path = b.Input.NotebookPath
						}
						onTool(ToolUse{Name: b.Name, FilePath: path})
					}
				}
			}
		case "result":
			result, gotResult = ev.Result, true
			isError = ev.IsError || (ev.Subtype != "" && ev.Subtype != "success")
			subtype = ev.Subtype
		}
	}
	if err := sc.Err(); err != nil {
		return "", fmt.Errorf("read stream: %w", err)
	}
	if !gotResult {
		return "", fmt.Errorf("stream ended without a result event")
	}
	if isError {
		return "", fmt.Errorf("claude turn failed (%s): %s", subtype, result)
	}
	return result, nil
}

// NewSessionID returns a random RFC-4122 v4 UUID for use with --session-id.
func NewSessionID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}
