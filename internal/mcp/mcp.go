// Package mcp is a minimal Model Context Protocol server over stdio
// (newline-delimited JSON-RPC 2.0), so the session model can pull memory on
// demand instead of relying only on what the prompt hook pushed:
//
//   - imem_search: the model writes the query itself, with the whole
//     conversation as context — the job the per-prompt LLM expander used to
//     do in tens of seconds, now done for free and only when needed;
//   - imem_remember: an explicit save, for what the user asks to keep.
//
// Stdlib only, like the rest of the repo. Tool calls go to the daemon over
// HTTP; the server itself holds no state.
package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// LatestProtocol is answered to clients asking for a version this server
// does not list.
const LatestProtocol = "2025-06-18"

var supportedProtocols = map[string]bool{"2025-06-18": true, "2025-03-26": true, "2024-11-05": true}

// RememberInput is one explicit memory.
type RememberInput struct {
	Title    string   `json:"title"`
	Content  string   `json:"content"`
	Kind     string   `json:"kind"`
	Entities []string `json:"entities"`
}

var validKinds = map[string]bool{"fact": true, "decision": true, "preference": true, "rule": true, "reference": true}

// Server answers MCP requests. Search and Remember do the work (func fields,
// the repo's injection style); both return the text the model reads.
type Server struct {
	Search   func(ctx context.Context, query string, limit int) (string, error)
	Remember func(ctx context.Context, in RememberInput) (string, error)
	Version  string
}

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// Serve reads requests from r until EOF and writes one line per response to
// w. Notifications (no id) are never answered.
func (s *Server) Serve(ctx context.Context, r io.Reader, w io.Writer) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	enc := json.NewEncoder(w)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var req request
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			if err := enc.Encode(response{JSONRPC: "2.0", ID: json.RawMessage("null"),
				Error: &rpcError{Code: -32700, Message: "parse error"}}); err != nil {
				return err
			}
			continue
		}
		if len(req.ID) == 0 || string(req.ID) == "null" {
			continue // a notification: nothing to answer
		}
		result, rerr := s.handle(ctx, req)
		resp := response{JSONRPC: "2.0", ID: req.ID}
		if rerr != nil {
			resp.Error = rerr
		} else {
			resp.Result = result
		}
		if err := enc.Encode(resp); err != nil {
			return err
		}
	}
	return sc.Err()
}

func (s *Server) handle(ctx context.Context, req request) (any, *rpcError) {
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &p)
		version := LatestProtocol
		if supportedProtocols[p.ProtocolVersion] {
			version = p.ProtocolVersion
		}
		return map[string]any{
			"protocolVersion": version,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]any{"name": "imem", "version": s.Version},
			"instructions": "Long-term memory from past Claude Code sessions across every repo. " +
				"Relevant memories are pushed into each prompt as an <infinite-memory> block; " +
				"use imem_search to look further, and imem_remember when the user asks you to remember something.",
		}, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		return map[string]any{"tools": tools}, nil
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return nil, &rpcError{Code: -32602, Message: "invalid params"}
		}
		text, err := s.call(ctx, p.Name, p.Arguments)
		if err != nil {
			return toolResult(err.Error(), true), nil
		}
		return toolResult(text, false), nil
	default:
		return nil, &rpcError{Code: -32601, Message: "method not found: " + req.Method}
	}
}

func (s *Server) call(ctx context.Context, name string, args json.RawMessage) (string, error) {
	switch name {
	case "imem_search":
		var a struct {
			Query string `json:"query"`
			Limit int    `json:"limit"`
		}
		if err := json.Unmarshal(args, &a); err != nil || strings.TrimSpace(a.Query) == "" {
			return "", fmt.Errorf("imem_search needs a non-empty query")
		}
		switch {
		case a.Limit <= 0:
			a.Limit = 8
		case a.Limit > 20:
			a.Limit = 20
		}
		if s.Search == nil {
			return "", fmt.Errorf("search is not available")
		}
		return s.Search(ctx, a.Query, a.Limit)
	case "imem_remember":
		var a RememberInput
		if err := json.Unmarshal(args, &a); err != nil {
			return "", fmt.Errorf("invalid arguments: %v", err)
		}
		a.Title, a.Content, a.Kind = strings.TrimSpace(a.Title), strings.TrimSpace(a.Content), strings.ToLower(strings.TrimSpace(a.Kind))
		if a.Title == "" || a.Content == "" {
			return "", fmt.Errorf("imem_remember needs a title and content")
		}
		if !validKinds[a.Kind] {
			return "", fmt.Errorf("kind must be one of fact, decision, preference, rule, reference")
		}
		if s.Remember == nil {
			return "", fmt.Errorf("remember is not available")
		}
		return s.Remember(ctx, a)
	default:
		return "", fmt.Errorf("unknown tool %q", name)
	}
}

func toolResult(text string, isErr bool) map[string]any {
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": text}},
		"isError": isErr,
	}
}

var tools = []map[string]any{
	{
		"name": "imem_search",
		"description": "Search long-term memory from past Claude Code sessions across all of the user's repos: " +
			"decisions and their reasons, facts about systems, conventions, references, preferences. " +
			"Use it when the user refers to earlier work (\"yang kemarin\", \"like last time\"), when you need a " +
			"project's conventions or a past decision, or when the <infinite-memory> block does not cover the topic. " +
			"Query with specific nouns — repo or service names, features, error codes, identifiers; English " +
			"terms match best, Indonesian works too. Results are dated leads: verify before relying on them.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query": map[string]any{"type": "string", "description": "What to look for, e.g. \"jago syariah link 4000703\""},
				"limit": map[string]any{"type": "integer", "minimum": 1, "maximum": 20, "description": "Max results (default 8)"},
			},
			"required": []string{"query"},
		},
	},
	{
		"name": "imem_remember",
		"description": "Save one durable memory for future sessions. Use it when the user asks you to remember " +
			"something, or for an important correction or decision worth keeping verbatim. Kinds: fact (true about " +
			"a system), decision (a choice and why), preference / rule (only what the USER stated or enforced — " +
			"rules are re-injected into every future session), reference (a path, URL or command). Not for " +
			"transient task state: a background extractor already saves memories after each turn.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"title":    map[string]any{"type": "string", "description": "At most 10 words, specific"},
				"content":  map[string]any{"type": "string", "description": "1-3 self-contained sentences"},
				"kind":     map[string]any{"type": "string", "enum": []string{"fact", "decision", "preference", "rule", "reference"}},
				"entities": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "maxItems": 6, "description": "Topics it belongs to, e.g. \"jago whitelist\""},
			},
			"required": []string{"title", "content", "kind"},
		},
	},
}
