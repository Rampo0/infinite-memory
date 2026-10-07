package setup

import (
	"bytes"
	"encoding/json"
	"fmt"
)

type field struct {
	key   string
	value json.RawMessage
}

func parseObject(data []byte) ([]field, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, nil
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil, fmt.Errorf("not a JSON object")
	}
	var out []field
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, _ := tok.(string)
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, err
		}
		out = append(out, field{key: key, value: raw})
	}
	return out, nil
}

func writeObject(fields []field) ([]byte, error) {
	var b bytes.Buffer
	b.WriteString("{\n")
	for i, f := range fields {
		key, err := marshal(f.key)
		if err != nil {
			return nil, err
		}
		var val bytes.Buffer
		if err := json.Indent(&val, f.value, "  ", "  "); err != nil {
			return nil, err
		}
		fmt.Fprintf(&b, "  %s: %s", key, val.Bytes())
		if i < len(fields)-1 {
			b.WriteString(",")
		}
		b.WriteString("\n")
	}
	b.WriteString("}\n")
	return b.Bytes(), nil
}

func get(fields []field, key string) json.RawMessage {
	for _, f := range fields {
		if f.key == key {
			return f.value
		}
	}
	return nil
}

func set(fields []field, key string, value json.RawMessage) []field {
	for i, f := range fields {
		if f.key == key {
			fields[i].value = value
			return fields
		}
	}
	return append(fields, field{key: key, value: value})
}

func marshal(v any) (json.RawMessage, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(b.Bytes(), "\n"), nil
}
