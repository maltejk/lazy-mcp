package hierarchy

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// coerceArguments repairs common argument-encoding mistakes LLMs make when
// calling execute_tool, using the downstream tool's JSON inputSchema:
//
//   - "true"/"false" strings for boolean properties
//   - numeric strings for integer/number properties
//   - arrays sent as a bare scalar, a JSON-encoded string, or an XML-style
//     wrapper object such as {"item": "a"} / {"item": ["a","b"]}
//   - objects sent as a JSON-encoded string
//
// Strict downstream servers (e.g. zod-validated ones) reject these outright.
// Values that already match the schema, or that can't be repaired, are left
// untouched. It never mutates args and returns the list of repairs made
// (as "path: from -> to" notes) for logging.
func coerceArguments(args map[string]interface{}, schema map[string]interface{}) (map[string]interface{}, []string) {
	if len(args) == 0 || schema == nil {
		return args, nil
	}
	var notes []string
	out := coerceObject(args, schema, "", &notes)
	return out, notes
}

func coerceObject(obj map[string]interface{}, schema map[string]interface{}, path string, notes *[]string) map[string]interface{} {
	props, _ := schema["properties"].(map[string]interface{})
	if props == nil {
		return obj
	}
	out := make(map[string]interface{}, len(obj))
	for k, v := range obj {
		propSchema, _ := props[k].(map[string]interface{})
		if propSchema == nil {
			out[k] = v
			continue
		}
		out[k] = coerceValue(v, propSchema, joinPath(path, k), notes)
	}
	return out
}

func joinPath(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

func coerceValue(v interface{}, schema map[string]interface{}, path string, notes *[]string) interface{} {
	typ, _ := schema["type"].(string)
	switch typ {
	case "boolean":
		if s, ok := v.(string); ok {
			switch strings.ToLower(strings.TrimSpace(s)) {
			case "true":
				return note(notes, path, v, true)
			case "false":
				return note(notes, path, v, false)
			}
		}
	case "integer":
		if s, ok := v.(string); ok {
			if n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64); err == nil {
				return note(notes, path, v, n)
			}
		}
	case "number":
		if s, ok := v.(string); ok {
			if n, err := strconv.ParseFloat(strings.TrimSpace(s), 64); err == nil {
				return note(notes, path, v, n)
			}
		}
	case "array":
		return coerceArray(v, schema, path, notes)
	case "object":
		if s, ok := v.(string); ok {
			var parsed map[string]interface{}
			if json.Unmarshal([]byte(s), &parsed) == nil {
				v = note(notes, path, v, parsed)
			}
		}
		if m, ok := v.(map[string]interface{}); ok {
			return coerceObject(m, schema, path, notes)
		}
	}
	return v
}

func coerceArray(v interface{}, schema map[string]interface{}, path string, notes *[]string) interface{} {
	items, _ := schema["items"].(map[string]interface{})
	var arr []interface{}
	switch x := v.(type) {
	case []interface{}:
		arr = x
	case string:
		var parsed []interface{}
		if t := strings.TrimSpace(x); strings.HasPrefix(t, "[") && json.Unmarshal([]byte(t), &parsed) == nil {
			arr = parsed
		} else {
			arr = []interface{}{x}
		}
		note(notes, path, v, arr)
	case map[string]interface{}:
		// XML-style wrapper: {"item": ...} (any single key) -> the inner value.
		if len(x) != 1 {
			return v
		}
		for _, inner := range x {
			if a, ok := inner.([]interface{}); ok {
				arr = a
			} else {
				arr = []interface{}{inner}
			}
		}
		note(notes, path, v, arr)
	default:
		// Bare scalar (number/bool) where an array is expected.
		arr = []interface{}{v}
		note(notes, path, v, arr)
	}
	if items == nil {
		return arr
	}
	out := make([]interface{}, len(arr))
	for i, e := range arr {
		out[i] = coerceValue(e, items, fmt.Sprintf("%s[%d]", path, i), notes)
	}
	return out
}

func note(notes *[]string, path string, from, to interface{}) interface{} {
	fb, _ := json.Marshal(from)
	tb, _ := json.Marshal(to)
	*notes = append(*notes, fmt.Sprintf("%s: %s -> %s", path, fb, tb))
	return to
}
