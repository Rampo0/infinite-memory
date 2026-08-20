// Package hookio decodes Claude Code hook stdin payloads and emits
// hookSpecificOutput JSON for context injection.
package hookio

import (
	"encoding/json"
	"io"
)

// Input covers the fields we use across UserPromptSubmit, Stop and
// SessionEnd. Unknown fields are ignored.
type Input struct {
	SessionID      string `json:"session_id"`
	TranscriptPath string `json:"transcript_path"`
	CWD            string `json:"cwd"`
	HookEventName  string `json:"hook_event_name"`
	UserPrompt     string `json:"user_prompt"`
	// Prompt is a fallback for older Claude Code builds that used "prompt".
	Prompt         string `json:"prompt"`
	StopHookActive bool   `json:"stop_hook_active"`
}

func Read(r io.Reader) (Input, error) {
	var in Input
	err := json.NewDecoder(r).Decode(&in)
	return in, err
}

func (in Input) PromptText() string {
	if in.UserPrompt != "" {
		return in.UserPrompt
	}
	return in.Prompt
}

type hookSpecificOutput struct {
	HookEventName     string `json:"hookEventName"`
	AdditionalContext string `json:"additionalContext"`
}

type contextOutput struct {
	HookSpecificOutput hookSpecificOutput `json:"hookSpecificOutput"`
}

// EmitContext writes the additionalContext JSON envelope for the given event.
func EmitContext(w io.Writer, event, context string) error {
	return json.NewEncoder(w).Encode(contextOutput{
		HookSpecificOutput: hookSpecificOutput{
			HookEventName:     event,
			AdditionalContext: context,
		},
	})
}
