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
	maxAliasesPerMem   = 10
	maxAliasChars      = 40
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
	Aliases   []string    `json:"aliases"`
	Op        string      `json:"op"`
	TargetID  string      `json:"target_id"`
}

// Feedback grades one memory the assistant was shown during the excerpt.
type Feedback struct {
	ID      string
	Verdict string // used | wrong | outdated
}

var validVerdicts = map[string]bool{"used": true, "wrong": true, "outdated": true}

type rawEntity struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

type rawResult struct {
	Memories []rawMemory `json:"memories"`
	Feedback []struct {
		ID      string `json:"id"`
		Verdict string `json:"verdict"`
	} `json:"feedback"`
}

// ParseMemories defensively parses the model's output: markdown fences are
// stripped, everything outside the outermost braces is discarded, and each
// item is validated and clipped. LLM output is untrusted input.
func ParseMemories(raw string) ([]graph.MemoryIn, error) {
	mems, _, err := ParseExtraction(raw)
	return mems, err
}

// ParseExtraction is ParseMemories plus the feedback list. Ops are normalized
// (unknown -> add) but not yet checked against what the model was shown:
// that is ResolveOps, which knows the candidate ids.
func ParseExtraction(raw string) ([]graph.MemoryIn, []Feedback, error) {
	body, err := SliceJSON(raw)
	if err != nil {
		return nil, nil, err
	}
	var parsed rawResult
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		return nil, nil, fmt.Errorf("unmarshal memories: %w", err)
	}
	var fb []Feedback
	for _, f := range parsed.Feedback {
		v := strings.ToLower(strings.TrimSpace(f.Verdict))
		id := strings.TrimSpace(f.ID)
		if id != "" && validVerdicts[v] {
			fb = append(fb, Feedback{ID: id, Verdict: v})
		}
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
		mem.Aliases = CleanAliases(m.Aliases)
		mem.Op = strings.ToLower(strings.TrimSpace(m.Op))
		mem.TargetID = strings.TrimSpace(m.TargetID)
		if mem.Op != "update" && mem.Op != "noop" {
			mem.Op, mem.TargetID = "add", ""
		}
		out = append(out, mem)
	}
	return out, fb, nil
}

// ResolveOps checks update/noop targets against the ids the model was shown.
// An update naming anything else becomes a plain add; a noop naming anything
// else is dropped — it claimed the fact is already known but cannot say where.
func ResolveOps(mems []graph.MemoryIn, allowed map[string]bool) []graph.MemoryIn {
	out := mems[:0:0]
	for _, m := range mems {
		switch {
		case m.Op == "update" && !allowed[m.TargetID]:
			m.Op, m.TargetID = "add", ""
		case m.Op == "noop" && !allowed[m.TargetID]:
			continue
		case m.Op != "update" && m.Op != "noop":
			m.Op, m.TargetID = "add", ""
		}
		out = append(out, m)
	}
	return out
}

// CleanAliases trims, lowercases and dedupes aliases, dropping blanks and
// anything over maxAliasChars, and keeps at most maxAliasesPerMem: model
// output is untrusted, and a stuffed list would make one memory match all.
func CleanAliases(raw []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, a := range raw {
		a = strings.ToLower(strings.Join(strings.Fields(a), " "))
		if a == "" || len(a) > maxAliasChars || seen[a] {
			continue
		}
		seen[a] = true
		out = append(out, a)
		if len(out) == maxAliasesPerMem {
			break
		}
	}
	return out
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
