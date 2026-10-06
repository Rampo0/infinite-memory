package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

type reply struct {
	ID     any             `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code int `json:"code"`
	} `json:"error"`
}

func serve(t *testing.T, s *Server, lines ...string) []reply {
	t.Helper()
	var out bytes.Buffer
	if err := s.Serve(context.Background(), strings.NewReader(strings.Join(lines, "\n")+"\n"), &out); err != nil {
		t.Fatal(err)
	}
	var replies []reply
	sc := bufio.NewScanner(&out)
	for sc.Scan() {
		var r reply
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			t.Fatalf("server wrote a non-JSON line %q: %v", sc.Text(), err)
		}
		replies = append(replies, r)
	}
	return replies
}

func TestInitializeNegotiatesVersion(t *testing.T) {
	s := &Server{Version: "test"}
	got := serve(t, s,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"claude-code","version":"x"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"initialize","params":{"protocolVersion":"1999-01-01"}}`,
	)
	if len(got) != 2 {
		t.Fatalf("a notification gets no reply: want 2 replies, got %d", len(got))
	}
	var r1, r2 struct {
		ProtocolVersion string         `json:"protocolVersion"`
		Capabilities    map[string]any `json:"capabilities"`
		ServerInfo      struct{ Name string }
	}
	_ = json.Unmarshal(got[0].Result, &r1)
	_ = json.Unmarshal(got[1].Result, &r2)
	if r1.ProtocolVersion != "2025-03-26" || r1.Capabilities["tools"] == nil || r1.ServerInfo.Name != "imem" {
		t.Fatalf("supported version must be echoed with tools capability: %+v", r1)
	}
	if r2.ProtocolVersion != LatestProtocol {
		t.Fatalf("unsupported version -> latest, got %q", r2.ProtocolVersion)
	}
}

func TestToolsList(t *testing.T) {
	got := serve(t, &Server{}, `{"jsonrpc":"2.0","id":"a","method":"tools/list"}`)
	var res struct {
		Tools []struct {
			Name        string `json:"name"`
			Description string `json:"description"`
			InputSchema struct {
				Required []string `json:"required"`
			} `json:"inputSchema"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(got[0].Result, &res); err != nil {
		t.Fatal(err)
	}
	names := map[string][]string{}
	for _, tl := range res.Tools {
		names[tl.Name] = tl.InputSchema.Required
		if tl.Description == "" {
			t.Fatalf("%s needs a description: it is how the model decides to call it", tl.Name)
		}
	}
	if strings.Join(names["imem_search"], ",") != "query" || strings.Join(names["imem_remember"], ",") != "title,content,kind" {
		t.Fatalf("unexpected tools: %v", names)
	}
}

func call(name, args string) string {
	return `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"` + name + `","arguments":` + args + `}}`
}

func toolText(t *testing.T, r reply) (string, bool) {
	t.Helper()
	var res struct {
		Content []struct{ Type, Text string } `json:"content"`
		IsError bool                          `json:"isError"`
	}
	if err := json.Unmarshal(r.Result, &res); err != nil || len(res.Content) != 1 {
		t.Fatalf("bad tool result %s: %v", r.Result, err)
	}
	return res.Content[0].Text, res.IsError
}

func TestSearchToolDefaultsAndCapsLimit(t *testing.T) {
	var gotQ string
	var gotN []int
	s := &Server{Search: func(_ context.Context, q string, n int) (string, error) {
		gotQ = q
		gotN = append(gotN, n)
		return "- [fact] Jago Syariah binds — c", nil
	}}
	got := serve(t, s, call("imem_search", `{"query":"jago syariah"}`), call("imem_search", `{"query":"x","limit":99}`))
	text, isErr := toolText(t, got[0])
	if isErr || gotQ != "x" || !strings.Contains(text, "Jago Syariah") || gotN[0] != 8 || gotN[1] != 20 {
		t.Fatalf("want default 8 and cap 20, got %v (err %v) %q", gotN, isErr, text)
	}
}

// Tool failures are tool results with isError, not protocol errors: the
// model should read why and carry on.
func TestToolErrorsAreResults(t *testing.T) {
	s := &Server{
		Search: func(context.Context, string, int) (string, error) { return "", errors.New("daemon unreachable") },
		Remember: func(context.Context, RememberInput) (string, error) {
			t.Fatal("must not save an invalid memory")
			return "", nil
		},
	}
	got := serve(t, s, call("imem_search", `{"query":"x"}`), call("imem_remember", `{"title":"","content":"c","kind":"fact"}`),
		call("imem_remember", `{"title":"t","content":"c","kind":"gossip"}`))
	for i, r := range got {
		if r.Error != nil {
			t.Fatalf("reply %d is a protocol error, want a tool result", i)
		}
		if _, isErr := toolText(t, r); !isErr {
			t.Fatalf("reply %d should be an isError tool result", i)
		}
	}
}

func TestRememberPassesValidInput(t *testing.T) {
	var got RememberInput
	s := &Server{Remember: func(_ context.Context, in RememberInput) (string, error) { got = in; return "saved (new)", nil }}
	r := serve(t, s, call("imem_remember", `{"title":"Never use --bare","content":"It disables OAuth.","kind":"rule","entities":["claude cli"]}`))
	if text, isErr := toolText(t, r[0]); isErr || text != "saved (new)" {
		t.Fatalf("got %q err %v", text, isErr)
	}
	if got.Title != "Never use --bare" || got.Kind != "rule" || len(got.Entities) != 1 {
		t.Fatalf("input not passed through: %+v", got)
	}
}

func TestUnknownMethodAndParseError(t *testing.T) {
	got := serve(t, &Server{}, `{"jsonrpc":"2.0","id":3,"method":"resources/list"}`, `{not json`, `{"jsonrpc":"2.0","method":"notifications/whatever"}`)
	if len(got) != 2 || got[0].Error == nil || got[0].Error.Code != -32601 || got[1].Error == nil || got[1].Error.Code != -32700 {
		t.Fatalf("want method-not-found then parse error and silence for the notification, got %+v", got)
	}
}

func TestPingAnswersEmpty(t *testing.T) {
	got := serve(t, &Server{}, `{"jsonrpc":"2.0","id":9,"method":"ping"}`)
	if len(got) != 1 || got[0].Error != nil || string(got[0].Result) != "{}" {
		t.Fatalf("ping must answer {}, got %+v", got)
	}
}

func TestReadOnlyServesSearchOnly(t *testing.T) {
	s := &Server{ReadOnly: true, Remember: func(context.Context, RememberInput) (string, error) { return "saved", nil }}
	got := serve(t, s, `{"jsonrpc":"2.0","id":"a","method":"tools/list"}`, call("imem_remember", `{"title":"t","content":"c","kind":"fact"}`))
	var res struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(got[0].Result, &res); err != nil {
		t.Fatal(err)
	}
	if len(res.Tools) != 1 || res.Tools[0].Name != "imem_search" {
		t.Fatalf("read-only must list imem_search alone, got %+v", res.Tools)
	}
	if text, isErr := toolText(t, got[1]); !isErr || !strings.Contains(text, "unknown tool") {
		t.Fatalf("read-only must refuse imem_remember, got %q", text)
	}
}

func TestInstructionsAskForSearchEveryTurn(t *testing.T) {
	for _, s := range []*Server{{}, {ReadOnly: true}} {
		got := serve(t, s, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`)
		var res struct {
			Instructions string `json:"instructions"`
		}
		if err := json.Unmarshal(got[0].Result, &res); err != nil || !strings.Contains(res.Instructions, "call imem_search") {
			t.Fatalf("instructions must ask for imem_search (readOnly=%v): %q", s.ReadOnly, res.Instructions)
		}
	}
}
