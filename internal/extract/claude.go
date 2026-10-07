package extract

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
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
          },
          "aliases": {
            "type": "array",
            "maxItems": 10,
            "items": {"type": "string", "maxLength": 40}
          },
          "op": {"type": "string", "enum": ["add", "update", "noop"]},
          "target_id": {"type": "string"}
        },
        "required": ["title", "content", "type"]
      }
    },
    "feedback": {
      "type": "array",
      "items": {
        "type": "object",
        "properties": {
          "id": {"type": "string"},
          "verdict": {"type": "string", "enum": ["used", "wrong", "outdated"]}
        },
        "required": ["id", "verdict"]
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
	Effort   string
}

func NewRunner(cfg config.Config) *Runner {
	return &Runner{Bin: cfg.ClaudeBin, Model: cfg.ExtractModel, SpawnDir: cfg.SpawnDir(), Effort: cfg.ExtractEffort}
}

// binFallbacks are tried in order when claude is not on PATH. The daemon
// inherits launchd's PATH, not your shell's, so a bare "claude" often fails to
// resolve even though the CLI works fine in a terminal.
var binFallbacks = []string{
	"~/.local/bin/claude",    // current installer
	"~/.claude/local/claude", // legacy installer
	"/opt/homebrew/bin/claude",
	"/usr/local/bin/claude",
}

func (r *Runner) ResolveBin() (string, error) { return r.resolveBin() }

func (r *Runner) resolveBin() (string, error) {
	if p, err := exec.LookPath(r.Bin); err == nil {
		return p, nil
	}
	tried := []string{strconv.Quote(r.Bin)}
	for _, fb := range binFallbacks {
		path := config.ExpandHome(fb)
		if _, err := os.Stat(path); err == nil {
			return path, nil
		}
		tried = append(tried, strconv.Quote(path))
	}
	return "", fmt.Errorf("claude binary not found (tried %s) — set claude_bin to an absolute path in config.json",
		strings.Join(tried, ", "))
}

// Request is one headless-claude call. A struct rather than positional args
// so the spawn shape stays readable as callers with different needs appear.
type Request struct {
	Prompt       string // stdin — never argv
	Schema       string // --json-schema; "" disables structured output
	SystemPrompt string
	// Append adds to Claude Code's default system prompt instead of replacing
	// it. Extraction appends (historical behaviour); small single-purpose
	// calls should replace, since the default prompt is dead weight.
	Append bool
	Model  string // "" means r.Model
	// Isolated adds --safe-mode --strict-mcp-config --setting-sources ""
	// --tools "". Measured on this machine: input preamble 30K tokens -> 0,
	// wall 14.4s -> 9.3s, cost $0.016 -> $0.006. It is also a security
	// control — it removes every configured MCP server from a spawn that
	// processes untrusted user text.
	Isolated bool
	// Effort maps to --effort. Measured on the expansion prompt: "low" cuts
	// thinking tokens ~25% and wall time ~20%, with no loss in term quality.
	// "" leaves the account default.
	Effort string
}

// Run executes one extraction and returns the model's raw result text.
// The caller owns the context deadline (120s recommended).
func (r *Runner) Run(ctx context.Context, prompt string) (string, error) {
	return r.RunSchema(ctx, Request{
		Prompt: prompt, Schema: ExtractionSchema, SystemPrompt: systemPrompt, Append: true,
		Isolated: true, Effort: r.Effort,
	})
}

// RunSchema executes one headless claude call and returns its raw result text.
// Every spawn-hardening decision lives here and nowhere else. The caller owns
// the context deadline.
func (r *Runner) RunSchema(ctx context.Context, req Request) (string, error) {
	bin, err := r.resolveBin()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(r.SpawnDir, 0o755); err != nil {
		return "", err
	}
	model := req.Model
	if model == "" {
		model = r.Model
	}

	args := []string{
		"-p",
		"--settings", `{"disableAllHooks": true}`,
		"--output-format", "json",
		"--model", model,
		"--no-session-persistence",
	}
	if req.Isolated {
		// --tools "" strips the tool definitions themselves, which is what
		// actually removes the preamble; --json-schema keeps working without
		// them (the result then arrives under "result" rather than
		// "structured_result", which parseEnvelope already handles).
		args = append(args, "--safe-mode", "--strict-mcp-config", "--setting-sources", "", "--tools", "")
	} else {
		// --json-schema is implemented via a StructuredOutput tool, so that
		// tool must be allowed; every other tool stays permission-denied in
		// -p mode (--disallowedTools '*' would break structured output).
		// --strict-mcp-config with no --mcp-config loads no MCP servers: the
		// user-scope imem server and the rest would boot for nothing.
		args = append(args, "--allowedTools", "StructuredOutput", "--strict-mcp-config")
	}
	if req.Effort != "" {
		args = append(args, "--effort", req.Effort)
	}
	if req.Schema != "" {
		args = append(args, "--json-schema", req.Schema)
	}
	if req.SystemPrompt != "" {
		flag := "--system-prompt"
		if req.Append {
			flag = "--append-system-prompt"
		}
		args = append(args, flag, req.SystemPrompt)
	}

	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = r.SpawnDir
	cmd.Stdin = strings.NewReader(req.Prompt)
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
