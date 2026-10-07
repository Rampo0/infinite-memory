package setup

import (
	"bytes"
	"encoding/json"
	"strings"
)

const SearchPermission = "mcp__imem__imem_search"

func RulesPermissions(exe string) []string {
	return []string{"Bash(imem rules:*)", "Bash(" + exe + " rules:*)"}
}

var hookEvents = []string{"SessionStart", "UserPromptSubmit", "SubagentStart", "Stop", "SessionEnd"}

func HookGroup(event, exe string, promptTimeout int) map[string]any {
	item := map[string]any{"type": "command", "command": exe + " hook " + hookArg(event), "timeout": 10}
	switch event {
	case "UserPromptSubmit":
		item["timeout"] = promptTimeout
	case "Stop":
		item["async"], item["timeout"] = false, 120
	case "SessionEnd":
		delete(item, "timeout")
		item["async"] = true
	}
	return map[string]any{"hooks": []any{item}}
}

func hookArg(event string) string {
	return map[string]string{"SessionStart": "session-start", "UserPromptSubmit": "user-prompt",
		"SubagentStart": "subagent-start", "Stop": "stop", "SessionEnd": "session-end"}[event]
}

func MergeSettings(data []byte, exe string) ([]byte, bool, error) {
	fields, err := parseObject(data)
	if err != nil {
		return nil, false, err
	}
	before, err := writeObject(fields)
	if err != nil {
		return nil, false, err
	}
	hooks, err := mergeHooks(get(fields, "hooks"), exe)
	if err != nil {
		return nil, false, err
	}
	fields = set(fields, "hooks", hooks)
	perms := get(fields, "permissions")
	for _, p := range append([]string{SearchPermission}, RulesPermissions(exe)...) {
		if perms, err = mergeAllow(perms, p); err != nil {
			return nil, false, err
		}
	}
	fields = set(fields, "permissions", perms)
	after, err := writeObject(fields)
	if err != nil {
		return nil, false, err
	}
	return after, len(data) == 0 || !bytes.Equal(before, after), nil
}

func mergeHooks(raw json.RawMessage, exe string) (json.RawMessage, error) {
	events, err := parseObject(raw)
	if err != nil {
		return nil, err
	}
	for _, ev := range hookEvents {
		val, err := mergeEvent(get(events, ev), HookGroup(ev, exe, 10))
		if err != nil {
			return nil, err
		}
		events = set(events, ev, val)
	}
	return compact(events)
}

func mergeEvent(cur json.RawMessage, canonical map[string]any) (json.RawMessage, error) {
	var groups []json.RawMessage
	if cur != nil {
		if err := json.Unmarshal(cur, &groups); err != nil {
			return nil, err
		}
	}
	want, err := marshal(canonical)
	if err != nil {
		return nil, err
	}
	placed := false
	var out []json.RawMessage
	for _, g := range groups {
		kept, hadImem, err := dropImem(g)
		if err != nil {
			return nil, err
		}
		if hadImem && !placed {
			placed = true
			if sameJSON(g, want) {
				out = append(out, g)
				continue
			}
			out = append(out, want)
		}
		if kept != nil {
			out = append(out, kept)
		}
	}
	if !placed {
		out = append(out, want)
	}
	return marshal(out)
}

func dropImem(g json.RawMessage) (json.RawMessage, bool, error) {
	var group map[string]any
	if err := json.Unmarshal(g, &group); err != nil {
		return g, false, nil
	}
	items, _ := group["hooks"].([]any)
	var keep []any
	for _, it := range items {
		m, _ := it.(map[string]any)
		if cmd, _ := m["command"].(string); !strings.Contains(cmd, "imem hook ") {
			keep = append(keep, it)
		}
	}
	if len(keep) == len(items) {
		return g, false, nil
	}
	if len(keep) == 0 {
		return nil, true, nil
	}
	group["hooks"] = keep
	out, err := marshal(group)
	return out, true, err
}

func sameJSON(a, b json.RawMessage) bool {
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	xa, _ := marshal(x)
	ya, _ := marshal(y)
	return bytes.Equal(xa, ya)
}

func mergeAllow(raw json.RawMessage, perm string) (json.RawMessage, error) {
	fields, err := parseObject(raw)
	if err != nil {
		return nil, err
	}
	var allow []any
	if cur := get(fields, "allow"); cur != nil {
		if err := json.Unmarshal(cur, &allow); err != nil {
			return nil, err
		}
	}
	for _, a := range allow {
		if a == perm {
			return compact(fields)
		}
	}
	val, err := marshal(append(allow, perm))
	if err != nil {
		return nil, err
	}
	return compact(set(fields, "allow", val))
}

func compact(fields []field) (json.RawMessage, error) {
	out, err := writeObject(fields)
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	if err := json.Compact(&b, out); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

func EnsureLine(data []byte, line string) ([]byte, bool) {
	for _, l := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(l) == line {
			return data, false
		}
	}
	s := string(data)
	if s != "" && !strings.HasSuffix(s, "\n") {
		s += "\n"
	}
	return []byte(s + line + "\n"), true
}
