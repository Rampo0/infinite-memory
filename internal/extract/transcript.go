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
	"unicode/utf8"
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
	UUID              string          `json:"uuid"`
	IsSidechain       bool            `json:"isSidechain"`
	IsMeta            bool            `json:"isMeta"`
	IsAPIErrorMessage bool            `json:"isApiErrorMessage"`
	Message           json.RawMessage `json:"message"`
	Attachment        json.RawMessage `json:"attachment"`
}

type message struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type contentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Name      string          `json:"name"`
	ID        string          `json:"id"`
	ToolUseID string          `json:"tool_use_id"`
	Input     json.RawMessage `json:"input"`
	Content   json.RawMessage `json:"content"`
}

const maxToolsPerTurn = 8

const (
	messageClip = 6000
	reportClip  = 8000
)

type Chunk struct {
	Turns []Turn
	Next  int
	Total int
}

type lineTurn struct {
	turn Turn
	line int
}

func ReadChunk(path string, fromLine, maxChars int) (Chunk, error) {
	f, err := os.Open(path)
	if err != nil {
		return Chunk{}, err
	}
	defer f.Close()
	lts, total, err := readTurns(bufio.NewReaderSize(f, 256*1024), fromLine)
	if err != nil {
		return Chunk{}, err
	}
	return budgetChunk(lts, total, maxChars), nil
}

func readTurns(r *bufio.Reader, fromLine int) ([]lineTurn, int, error) {
	p := newLineParser()
	var out []lineTurn
	total := 0
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			if total >= fromLine {
				if t, ok := p.parse(line); ok {
					out = append(out, lineTurn{turn: t, line: total})
				}
			}
			total++
		}
		if err == io.EOF {
			return out, total, nil
		}
		if err != nil {
			return nil, 0, err
		}
	}
}

func budgetChunk(lts []lineTurn, total, maxChars int) Chunk {
	c := Chunk{Next: total, Total: total}
	size := 0
	for i, lt := range lts {
		size += len(lt.turn.Text) + len(strings.Join(lt.turn.Tools, "; "))
		if maxChars > 0 && size > maxChars && i > 0 {
			c.Next = lt.line
			return c
		}
		c.Turns = append(c.Turns, lt.turn)
	}
	return c
}

type lineParser struct {
	agentCalls map[string]bool
}

func newLineParser() *lineParser {
	return &lineParser{agentCalls: map[string]bool{}}
}

func (p *lineParser) parse(line []byte) (Turn, bool) {
	var env envelope
	if err := json.Unmarshal(line, &env); err != nil {
		return Turn{}, false
	}
	if env.Type == "attachment" && !env.IsSidechain {
		return queuedPrompt(env.Attachment)
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
	if env.Type == "assistant" {
		p.noteAgentCalls(msg.Content)
	} else if t, ok := p.subagentReport(msg.Content); ok {
		return t, true
	}
	return conversationTurn(env.Type, msg.Content)
}

func conversationTurn(role string, content json.RawMessage) (Turn, bool) {
	text := strings.TrimSpace(extractText(content))
	if isPlumbing(text) {
		text = slashCommand(text)
	}
	var tools []string
	if role == "assistant" {
		tools = toolSummaries(content)
	}
	if text == "" && len(tools) == 0 {
		return Turn{}, false
	}
	return Turn{Role: role, Text: clipMessage(text), Tools: tools}, true
}

func (p *lineParser) noteAgentCalls(raw json.RawMessage) {
	var blocks []contentBlock
	if json.Unmarshal(raw, &blocks) != nil {
		return
	}
	for _, b := range blocks {
		if b.Type == "tool_use" && (b.Name == "Agent" || b.Name == "Task") && b.ID != "" {
			p.agentCalls[b.ID] = true
		}
	}
}

func (p *lineParser) subagentReport(raw json.RawMessage) (Turn, bool) {
	var blocks []contentBlock
	if json.Unmarshal(raw, &blocks) != nil {
		return Turn{}, false
	}
	var parts []string
	for _, b := range blocks {
		if b.Type != "tool_result" || !p.agentCalls[b.ToolUseID] {
			continue
		}
		text := strings.TrimSpace(extractText(b.Content))
		if text != "" && !strings.HasPrefix(text, "Async agent launched") {
			parts = append(parts, text)
		}
	}
	if len(parts) == 0 {
		return Turn{}, false
	}
	return Turn{Role: "assistant", Text: clipMessage("Subagent report: " + strings.Join(parts, "\n\n"))}, true
}

func queuedPrompt(raw json.RawMessage) (Turn, bool) {
	var a struct {
		Type   string          `json:"type"`
		Prompt json.RawMessage `json:"prompt"`
	}
	if json.Unmarshal(raw, &a) != nil || a.Type != "queued_command" {
		return Turn{}, false
	}
	text := strings.TrimSpace(extractText(a.Prompt))
	if text == "" || isPlumbing(text) {
		return Turn{}, false
	}
	return Turn{Role: "user", Text: clipMessage(text)}, true
}

func slashCommand(text string) string {
	args := strings.TrimSpace(between(text, "<command-args>", "</command-args>"))
	name := strings.TrimSpace(between(text, "<command-name>", "</command-name>"))
	if args == "" || name == "" {
		return ""
	}
	return name + " " + args
}

func between(s, open, closing string) string {
	i := strings.Index(s, open)
	if i < 0 {
		return ""
	}
	rest := s[i+len(open):]
	j := strings.Index(rest, closing)
	if j < 0 {
		return ""
	}
	return rest[:j]
}

func clipMessage(text string) string {
	limit := messageClip
	if strings.HasPrefix(text, "<task-notification>") || strings.HasPrefix(text, "Subagent report: ") {
		limit = reportClip
	}
	return clipMiddle(text, limit)
}

func clipMiddle(s string, n int) string {
	if len(s) <= n {
		return s
	}
	head := clipRunes(s, n*2/3)
	tail := len(s) - (n - len(head))
	for tail < len(s) && !utf8.RuneStart(s[tail]) {
		tail++
	}
	return head + "\n…\n" + s[tail:]
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
