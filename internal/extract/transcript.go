// Package extract implements the memory-extraction pipeline: transcript
// delta reading, prompt building, the headless claude spawn, and defensive
// parsing of its output.
package extract

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"strings"
)

type Turn struct {
	Role string // "user" or "assistant"
	Text string
	// Tools summarizes the assistant's tool calls, one line each: a file
	// path, a Bash description, a tool name — never a command or other
	// input, which may carry secrets.
	Tools []string
}

// envelope mirrors only the transcript JSONL fields we act on; anything else
// is ignored. Verified against real ~/.claude/projects transcripts.
type envelope struct {
	Type              string          `json:"type"`
	IsSidechain       bool            `json:"isSidechain"`
	IsMeta            bool            `json:"isMeta"`
	IsAPIErrorMessage bool            `json:"isApiErrorMessage"`
	Message           json.RawMessage `json:"message"`
}

type message struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type contentBlock struct {
	Type  string          `json:"type"`
	Text  string          `json:"text"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

const maxToolsPerTurn = 8

const perMessageClip = 2000

// ReadDelta parses transcript lines [fromLine, EOF) into conversational
// turns and reports the file's total line count (the next cursor value).
// Non-message line types, sidechains, meta lines and tool plumbing are
// skipped. Turns are clipped per message, then trimmed from the OLDEST side
// until the total fits maxChars, so the newest turns always survive.
func ReadDelta(path string, fromLine, maxChars int) (turns []Turn, totalLines int, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()

	r := bufio.NewReaderSize(f, 256*1024)
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			if totalLines >= fromLine {
				if t, ok := parseLine(line); ok {
					turns = append(turns, t)
				}
			}
			totalLines++
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, 0, err
		}
	}

	// Budget: keep newest turns.
	total := 0
	cut := len(turns)
	for i := len(turns) - 1; i >= 0; i-- {
		total += len(turns[i].Text) + len(strings.Join(turns[i].Tools, "; "))
		if maxChars > 0 && total > maxChars {
			break
		}
		cut = i
	}
	return turns[cut:], totalLines, nil
}

func parseLine(line []byte) (Turn, bool) {
	var env envelope
	if err := json.Unmarshal(line, &env); err != nil {
		return Turn{}, false
	}
	if env.Type != "user" && env.Type != "assistant" {
		return Turn{}, false
	}
	if env.IsSidechain || env.IsMeta || env.IsAPIErrorMessage || len(env.Message) == 0 {
		return Turn{}, false
	}
	var msg message
	if err := json.Unmarshal(env.Message, &msg); err != nil {
		return Turn{}, false
	}

	text := extractText(msg.Content)
	text = strings.TrimSpace(text)
	var tools []string
	if env.Type == "assistant" {
		tools = toolSummaries(msg.Content)
	}
	if (text == "" && len(tools) == 0) || isPlumbing(text) {
		return Turn{}, false
	}
	if len(text) > perMessageClip {
		text = text[:perMessageClip]
	}
	return Turn{Role: env.Type, Text: text, Tools: tools}, true
}

// toolSummaries renders tool_use blocks as one safe line each. Only fields
// known to be harmless are read: file paths, Bash's human description, a
// sub-agent's description. Everything else is reduced to the tool name.
func toolSummaries(raw json.RawMessage) []string {
	var blocks []contentBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil
	}
	var out []string
	for _, b := range blocks {
		if b.Type != "tool_use" || b.Name == "" || len(out) == maxToolsPerTurn {
			continue
		}
		var in struct {
			FilePath     string `json:"file_path"`
			NotebookPath string `json:"notebook_path"`
			Description  string `json:"description"`
		}
		_ = json.Unmarshal(b.Input, &in)
		line := b.Name
		switch b.Name {
		case "Read", "Edit", "Write", "MultiEdit", "NotebookEdit":
			if p := firstNonEmpty(in.FilePath, in.NotebookPath); p != "" {
				line += " " + p
			}
		case "Bash", "Agent", "Task":
			if d := strings.TrimSpace(in.Description); d != "" {
				line += ": " + clip(d, 100)
			}
		}
		out = append(out, line)
	}
	return out
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

// extractText handles both content shapes: plain string (typed prompts) and
// block arrays, keeping only "text" blocks (thinking/tool_use/tool_result
// are noise for memory purposes).
func extractText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var blocks []contentBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return ""
	}
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" && strings.TrimSpace(b.Text) != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// isPlumbing filters slash-command wrappers and local-command caveats that
// appear as user messages but are not conversation.
func isPlumbing(text string) bool {
	return strings.HasPrefix(text, "<command-") ||
		strings.HasPrefix(text, "<local-command") ||
		strings.HasPrefix(text, "Caveat: The messages below were generated")
}

// TotalChars sums turn text lengths (spawn-skip threshold input).
func TotalChars(turns []Turn) int {
	n := 0
	for _, t := range turns {
		n += len(t.Text)
	}
	return n
}
