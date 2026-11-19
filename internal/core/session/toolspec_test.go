package session

import (
	"encoding/json"
	"testing"
)

// tools.list hands a loop definitions wrapped as { type, function = {...} },
// and the examples pass them straight to llm.chat. Read as the flat shape
// only, every tool reached the model with an empty name.
func TestToolSpecAcceptsBothShapes(t *testing.T) {
	for name, src := range map[string]string{
		"flat":    `{"name":"web_search","description":"Search","parameters":{"type":"object"}}`,
		"wrapped": `{"type":"function","function":{"name":"web_search","description":"Search","parameters":{"type":"object"}}}`,
	} {
		var spec ToolSpec
		if err := json.Unmarshal([]byte(src), &spec); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if spec.Name != "web_search" || spec.Description != "Search" || spec.Parameters["type"] != "object" {
			t.Errorf("%s decoded as %+v", name, spec)
		}
	}
}
