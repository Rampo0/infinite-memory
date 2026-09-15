package extract

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Rampo0/infinite-memory/internal/graph"
)

const (
	maxMemories        = 8
	maxContentChars    = 1500
	maxEntitiesPerMem  = 10
	maxRelationsPerMem = 12
)

var validKinds = map[string]bool{
	"fact": true, "decision": true, "preference": true, "reference": true, "rule": true,
}

type rawMemory struct {
	Title     string      `json:"title"`
	Content   string      `json:"content"`
	Type      string      `json:"type"`
	Entities  []rawEntity `json:"entities"`
	Relations [][]string  `json:"relations"`
}

type rawEntity struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

type rawResult struct {
	Memories []rawMemory `json:"memories"`
}

// ParseMemories defensively parses the model's output: markdown fences are
// stripped, everything outside the outermost braces is discarded, and each
// item is validated and clipped. LLM output is untrusted input.
func ParseMemories(raw string) ([]graph.MemoryIn, error) {
	body, err := SliceJSON(raw)
	if err != nil {
		return nil, err
	}
	var parsed rawResult
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		return nil, fmt.Errorf("unmarshal memories: %w", err)
	}

	var out []graph.MemoryIn
	for _, m := range parsed.Memories {
		if len(out) >= maxMemories {
			break
		}
		title := strings.TrimSpace(m.Title)
		content := strings.TrimSpace(m.Content)
		if title == "" || content == "" {
			continue
		}
		if len(content) > maxContentChars {
			content = content[:maxContentChars]
		}
		kind := strings.ToLower(strings.TrimSpace(m.Type))
		if !validKinds[kind] {
			kind = "fact"
		}
		mem := graph.MemoryIn{Title: title, Content: content, Kind: kind}
		for _, e := range m.Entities {
			if len(mem.Entities) >= maxEntitiesPerMem {
				break
			}
			if strings.TrimSpace(e.Name) == "" {
				continue
			}
			mem.Entities = append(mem.Entities, graph.EntityIn{Name: e.Name, Type: e.Type})
		}
		for _, r := range m.Relations {
			if len(mem.Relations) >= maxRelationsPerMem {
				break
			}
			if len(r) != 3 || r[0] == "" || r[1] == "" || r[2] == "" {
				continue
			}
			mem.Relations = append(mem.Relations, [3]string{r[0], r[1], r[2]})
		}
		out = append(out, mem)
	}
	return out, nil
}

// SliceJSON strips markdown fences and returns the outermost {...} span of a
// model response. LLM output is untrusted input: it fences even under a
// --json-schema, and wraps the object in prose often enough to matter.
func SliceJSON(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	s = strings.TrimPrefix(s, "```json")
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSuffix(s, "```")
	start := strings.Index(s, "{")
	end := strings.LastIndex(s, "}")
	if start < 0 || end <= start {
		return "", fmt.Errorf("no JSON object in output: %s", clip(raw, 200))
	}
	return s[start : end+1], nil
}
