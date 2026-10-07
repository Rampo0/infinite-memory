package extract

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"strings"
	"unicode/utf8"
)

func ReadTail(path string, maxBytes int64) ([]Turn, error) {
	lines, err := tailLines(path, maxBytes)
	if err != nil {
		return nil, err
	}
	var turns []Turn
	lp := newLineParser()
	for _, line := range lines {
		if t, ok := lp.parse(line); ok {
			turns = append(turns, t)
		}
	}
	return turns, nil
}

func tailLines(path string, maxBytes int64) ([][]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	start := max(fi.Size()-maxBytes, 0)
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return nil, err
	}
	r := bufio.NewReaderSize(f, 256*1024)
	if start > 0 {
		if _, err := r.ReadBytes('\n'); err != nil {
			return nil, nil
		}
	}
	var lines [][]byte
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			lines = append(lines, line)
		}
		if err != nil {
			return lines, nil
		}
	}
}

const finalReplyTail = 512 * 1024

func HasFinalReply(path, reply string) bool {
	want := squash(reply)
	if want == "" {
		return true
	}
	turns, err := ReadTail(path, finalReplyTail)
	if err != nil {
		return false
	}
	for i := len(turns) - 1; i >= 0; i-- {
		if turns[i].Role == "assistant" && turns[i].Text != "" {
			return samePrefix(squash(turns[i].Text), want, 200)
		}
	}
	return false
}

func squash(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func samePrefix(a, b string, n int) bool {
	return clipRunes(a, n) == clipRunes(b, n)
}

func LastExchange(turns []Turn, maxChars int) string {
	var user, assistant string
	for i := len(turns) - 1; i >= 0 && (user == "" || assistant == ""); i-- {
		t := turns[i]
		if t.Role == "user" && user == "" {
			user = t.Text
		}
		if t.Role == "assistant" && assistant == "" {
			assistant = t.Text
		}
	}
	return clipRunes(strings.TrimSpace(user+"\n"+assistant), maxChars)
}

func clipRunes(s string, n int) string {
	if n <= 0 || len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

const searchTool = "mcp__imem__imem_search"

func LastTurnSearch(path string) (string, bool) {
	lines, err := tailLines(path, finalReplyTail)
	if err != nil {
		return "", false
	}
	id, searched := "", false
	for _, line := range lines {
		var env envelope
		if json.Unmarshal(line, &env) != nil || env.IsSidechain || env.IsMeta || len(env.Message) == 0 {
			continue
		}
		var msg message
		if json.Unmarshal(env.Message, &msg) != nil {
			continue
		}
		switch {
		case env.Type == "user" && isRealPrompt(msg.Content):
			id, searched = env.UUID, false
		case env.Type == "assistant" && callsTool(msg.Content, searchTool):
			searched = true
		}
	}
	return id, searched
}

func isRealPrompt(raw json.RawMessage) bool {
	var blocks []contentBlock
	if json.Unmarshal(raw, &blocks) == nil {
		for _, b := range blocks {
			if b.Type == "tool_result" {
				return false
			}
		}
	}
	text := strings.TrimSpace(extractText(raw))
	return text != "" && !strings.HasPrefix(text, "<task-notification") && !strings.HasPrefix(text, "<local-command") &&
		!strings.HasPrefix(text, "Caveat:")
}

func callsTool(raw json.RawMessage, name string) bool {
	var blocks []contentBlock
	if json.Unmarshal(raw, &blocks) != nil {
		return false
	}
	for _, b := range blocks {
		if b.Type == "tool_use" && b.Name == name {
			return true
		}
	}
	return false
}
