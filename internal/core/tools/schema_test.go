package tools

import (
	"fmt"
	"reflect"
	"testing"
)

// TestNormalizeSchemaRecursesThroughCompositeKeywords: the function exists to
// drop a `required` the Lua bridge has mangled into an object (or an empty
// array), because strict providers reject that with a 400. It descended only
// into properties and items, so a mangled required under oneOf, allOf, $defs or
// additionalProperties reached the provider untouched — and both places a schema
// comes from, a Lua tool.def and an MCP server's inputSchema, are authored
// outside this repo.
//
// Recursion can only ever remove an invalid `required`, never add one, so
// descending further cannot introduce a 400 — only prevent one.
func TestNormalizeSchemaRecursesThroughCompositeKeywords(t *testing.T) {
	mangled := map[string]any{} // `required: {}` as the bridge delivers it
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"a": map[string]any{"type": "string", "required": mangled},
		},
		"oneOf": []any{
			map[string]any{"type": "object", "required": mangled},
			map[string]any{"type": "string"},
		},
		"allOf":                []any{map[string]any{"required": mangled}},
		"anyOf":                []any{map[string]any{"required": mangled}},
		"$defs":                map[string]any{"Deep": map[string]any{"required": mangled}},
		"definitions":          map[string]any{"Old": map[string]any{"required": mangled}},
		"patternProperties":    map[string]any{"^x": map[string]any{"required": mangled}},
		"dependentSchemas":     map[string]any{"d": map[string]any{"required": mangled}},
		"additionalProperties": map[string]any{"required": mangled},
		"not":                  map[string]any{"required": mangled},
		"if":                   map[string]any{"required": mangled},
		"then":                 map[string]any{"required": mangled},
		"else":                 map[string]any{"required": mangled},
		"contains":             map[string]any{"required": mangled},
		"items":                []any{map[string]any{"required": mangled}},
		"prefixItems":          []any{map[string]any{"required": mangled}},
	}
	out := NormalizeSchema(schema)

	var check func(path string, v any)
	check = func(path string, v any) {
		switch tv := v.(type) {
		case map[string]any:
			if _, bad := tv["required"]; bad {
				t.Errorf("%s still carries a mangled required", path)
			}
			for k, cv := range tv {
				check(path+"."+k, cv)
			}
		case []any:
			for i, cv := range tv {
				check(fmt.Sprintf("%s[%d]", path, i), cv)
			}
		}
	}
	check("schema", out)

	// A real required list survives untouched.
	got := NormalizeSchema(map[string]any{"required": []any{"a", "b"}})
	if !reflect.DeepEqual(got["required"], []any{"a", "b"}) {
		t.Fatalf("a non-empty required was altered: %#v", got["required"])
	}
	// And a boolean sub-schema passes through: JSON Schema allows it, and there
	// is nothing inside to normalize.
	got = NormalizeSchema(map[string]any{"additionalProperties": false})
	if v, ok := got["additionalProperties"]; !ok || v != false {
		t.Fatalf("a boolean sub-schema was altered: %#v", got)
	}
}
