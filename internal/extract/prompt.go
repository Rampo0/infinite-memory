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
Known entities (reuse these exact names when the same thing is meant): %s

A memory is knowledge that will still matter in future sessions:
- fact: something true about this project/system ("the daemon listens on 127.0.0.1:7690")
- decision: a choice that was made and why ("chose keyword retrieval because Memgraph text search is experimental")
- preference: how the user wants things done generally ("user prefers stdlib over frameworks")
- rule: a standing convention or standard the user stated or enforced — coding
  constitution (function LOC limits like "functions under 60 lines", "max 4 args",
  clean-code norms), preferred pattern per repo, code style, naming, MR/commit
  message templates, review norms. When the user corrects style or states "always/never
  do X", that is a rule. Rules are re-injected into every future session, so make them
  precise and self-contained.
- reference: a pointer worth keeping (file path, URL, command)

Entities are the retrieval index — include the business/domain TOPIC a memory belongs
to as an entity whenever one exists (e.g. "personal amend request", "amend bank",
"BCA API", "jago whitelist", "RDN creation"), because retrieval works across
repositories by topic, not by directory.

Do NOT extract: transient task state, small talk, generic programming knowledge,
anything already implied by an existing entity name alone, or restatements of the
user's current request.

Rules for output:
- 0 to 5 memories. An empty list is a good answer for chit-chat or pure execution turns.
- title: max 10 words, specific. content: 1-3 sentences, self-contained (readable
  without the conversation).
- entities: short canonical noun phrases (lowercase unless a proper name); 1-6 per memory.
- relations: only between entities listed in the same memory; relation verb is one
  lowercase word (uses, contains, replaces, requires, prefers, runs-on).

Reply with ONLY this JSON object, no markdown fences, no commentary:
{"memories":[{"title":"...","content":"...","type":"fact|decision|preference|rule|reference","entities":[{"name":"...","type":"technology|component|person|concept|topic|file|other"}],"relations":[["a","rel","b"]]}]}

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
