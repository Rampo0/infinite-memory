package extract

import (
	"fmt"
	"strings"
)

// BuildPrompt renders the extraction prompt for one transcript delta.
func BuildPrompt(projectKey string, knownEntities []string, turns []Turn) string {
	known := "(none)"
	if len(knownEntities) > 0 {
		known = strings.Join(knownEntities, ", ")
	}

	var b strings.Builder
	fmt.Fprintf(&b, `Extract durable memories from this Claude Code conversation excerpt.

Project: %s
Known entities in this project (reuse these exact names when the same thing is meant): %s

A memory is knowledge that will still matter in future sessions:
- fact: something true about this project/system ("the daemon listens on 127.0.0.1:7690")
- decision: a choice that was made and why ("chose keyword retrieval because Memgraph text search is experimental")
- preference: how the user wants things done ("user prefers stdlib over frameworks")
- reference: a pointer worth keeping (file path, URL, command)

Do NOT extract: transient task state, small talk, generic programming knowledge,
anything already implied by an existing entity name alone, or restatements of the
user's current request.

Rules:
- 0 to 5 memories. An empty list is a good answer for chit-chat or pure execution turns.
- title: max 10 words, specific. content: 1-3 sentences, self-contained (readable
  without the conversation).
- entities: short canonical noun phrases (lowercase unless a proper name); 1-6 per memory.
- relations: only between entities listed in the same memory; relation verb is one
  lowercase word (uses, contains, replaces, requires, prefers, runs-on).

Reply with ONLY this JSON object, no markdown fences, no commentary:
{"memories":[{"title":"...","content":"...","type":"fact|decision|preference|reference","entities":[{"name":"...","type":"technology|component|person|concept|file|other"}],"relations":[["a","rel","b"]]}]}

Conversation excerpt (oldest first):
`, projectKey, known)

	for _, t := range turns {
		role := "USER"
		if t.Role == "assistant" {
			role = "ASSISTANT"
		}
		fmt.Fprintf(&b, "\n[%s] %s\n", role, t.Text)
	}
	return b.String()
}
