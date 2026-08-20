package extract

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/Rampo0/infinite-memory/internal/config"
)

// ExtractionSchema is passed via --json-schema so the CLI enforces the
// output structure; parse.go still validates defensively.
const ExtractionSchema = `{
  "type": "object",
  "properties": {
    "memories": {
      "type": "array",
      "maxItems": 8,
      "items": {
        "type": "object",
        "properties": {
          "title": {"type": "string"},
          "content": {"type": "string"},
          "type": {"type": "string", "enum": ["fact", "decision", "preference", "reference", "rule"]},
          "entities": {
            "type": "array",
            "items": {
              "type": "object",
              "properties": {
                "name": {"type": "string"},
                "type": {"type": "string"}
              },
              "required": ["name"]
            }
          },
          "relations": {
            "type": "array",
            "items": {
              "type": "array",
              "items": {"type": "string"},
              "minItems": 3,
              "maxItems": 3
            }
          }
        },
        "required": ["title", "content", "type"]
      }
    }
  },
  "required": ["memories"]
}`

const systemPrompt = "You are a memory-extraction function. Reply with a single JSON object and nothing else. Do not use any tools."

// Runner spawns headless claude for extraction. Recursion guards:
// disableAllHooks in --settings (the spawned claude runs no user hooks) and
// INFINITE_MEMORY_INTERNAL=1 in env (our own hooks exit immediately if a
// hook somehow still fires). --bare is NOT used: it disables subscription
// OAuth. SpawnDir is a neutral cwd so no project CLAUDE.md is auto-loaded.
type Runner struct {
	Bin      string
	Model    string
	SpawnDir string
}

func NewRunner(cfg config.Config) *Runner {
	return &Runner{Bin: cfg.ClaudeBin, Model: cfg.ExtractModel, SpawnDir: cfg.SpawnDir()}
}

func (r *Runner) resolveBin() (string, error) {
	if p, err := exec.LookPath(r.Bin); err == nil {
		return p, nil
	}
	fallback := config.ExpandHome("~/.claude/local/claude")
	if _, err := os.Stat(fallback); err == nil {
		return fallback, nil
	}
	return "", fmt.Errorf("claude binary not found (tried %q and %q)", r.Bin, fallback)
}

// Run executes one extraction and returns the model's raw result text.
// The caller owns the context deadline (120s recommended).
func (r *Runner) Run(ctx context.Context, prompt string) (string, error) {
	bin, err := r.resolveBin()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(r.SpawnDir, 0o755); err != nil {
		return "", err
	}

	cmd := exec.CommandContext(ctx, bin,
		"-p",
		"--settings", `{"disableAllHooks": true}`,
		"--output-format", "json",
		"--model", r.Model,
		"--no-session-persistence",
		// --json-schema is implemented via a StructuredOutput tool, so that
		// tool must be allowed; every other tool stays permission-denied in
		// -p mode (--disallowedTools '*' would break structured output).
		"--allowedTools", "StructuredOutput",
		"--json-schema", ExtractionSchema,
		"--append-system-prompt", systemPrompt,
	)
	cmd.Dir = r.SpawnDir
	cmd.Stdin = strings.NewReader(prompt)
	cmd.Env = append(os.Environ(), "INFINITE_MEMORY_INTERNAL=1")

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("claude spawn: %w (stderr: %s)", err, clip(stderr.String(), 500))
	}
	return parseEnvelope(stdout.Bytes())
}

// parseEnvelope unwraps the --output-format json envelope. The result may be
// a plain string or (with --json-schema) a structured value; both normalize
// to the JSON text parse.go consumes.
func parseEnvelope(out []byte) (string, error) {
	var env map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(out), &env); err != nil {
		return "", fmt.Errorf("claude output not JSON: %w (%s)", err, clip(string(out), 300))
	}
	if isErr, _ := env["is_error"].(bool); isErr {
		res, _ := env["result"].(string)
		return "", fmt.Errorf("claude returned error: %s", clip(res, 500))
	}
	for _, key := range []string{"structured_result", "structured_output", "result"} {
		v, ok := env[key]
		if !ok || v == nil {
			continue
		}
		if s, isStr := v.(string); isStr {
			if strings.TrimSpace(s) == "" {
				continue
			}
			return s, nil
		}
		data, err := json.Marshal(v)
		if err == nil {
			return string(data), nil
		}
	}
	return "", fmt.Errorf("claude envelope has no usable result: %s", clip(string(out), 300))
}

func clip(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
