package guard

import (
	"encoding/json"
	"strings"
)

const overloadMessage = "our servers are currently overloaded. please try again later."

func overload(text string) bool {
	if text == "" {
		return false
	}
	if strings.Contains(strings.ToLower(strings.Join(strings.Fields(text), " ")), overloadMessage) {
		return true
	}
	// Error strings can wrap JSON. Recognize the structured code, never a bare keyword or HTTP 503.
	for i, ch := range text {
		if ch != '{' {
			continue
		}
		var value any
		decoder := json.NewDecoder(strings.NewReader(text[i:]))
		if decoder.Decode(&value) == nil && overloadCode(value) {
			return true
		}
	}
	return false
}

func overloadCode(value any) bool {
	object, ok := value.(map[string]any)
	if !ok {
		return false
	}
	for _, name := range []string{"code", "type"} {
		if v, ok := object[name].(string); ok && v == "server_is_overloaded" {
			return true
		}
	}
	if child, ok := object["error"]; ok {
		return overloadCode(child)
	}
	return false
}
