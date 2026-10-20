package main

import "testing"

func TestOpenAPIToolsGeneratesPathAndQuery(t *testing.T) {
	doc := map[string]any{
		"openapi": "3.0.0",
		"servers": []any{map[string]any{"url": "https://api.example.com"}},
		"paths": map[string]any{
			"/customers/{id}": map[string]any{
				"get": map[string]any{
					"operationId": "getCustomer",
					"summary":     "Get a customer",
					"parameters": []any{
						map[string]any{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
						map[string]any{"name": "expand", "in": "query", "schema": map[string]any{"type": "string"}},
					},
				},
			},
		},
	}
	defs, err := openAPITools(doc, "", "crm", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(defs) != 1 {
		t.Fatalf("defs = %d", len(defs))
	}
	def := defs[0]
	if def.Name != "getCustomer" || def.Method != "GET" {
		t.Fatalf("name/method = %q/%q", def.Name, def.Method)
	}
	if def.URL != "https://api.example.com/customers/{id}" {
		t.Fatalf("url = %q", def.URL)
	}
	if def.Query["expand"] != "{expand}" {
		t.Fatalf("query = %#v", def.Query)
	}
	if def.Auth == nil || def.Auth.Service != "crm" {
		t.Fatalf("auth = %#v", def.Auth)
	}
	if def.NeedsConfirm != nil {
		t.Fatal("GET should not require confirmation by default")
	}
}

func TestOpenAPIToolsBuildsJSONBody(t *testing.T) {
	doc := map[string]any{
		"openapi": "3.0.0",
		"servers": []any{map[string]any{"url": "https://api.example.com"}},
		"paths": map[string]any{
			"/customers": map[string]any{
				"post": map[string]any{
					"operationId": "createCustomer",
					"requestBody": map[string]any{
						"content": map[string]any{
							"application/json": map[string]any{
								"schema": map[string]any{
									"type":       "object",
									"required":   []any{"name"},
									"properties": map[string]any{"name": map[string]any{"type": "string"}, "tier": map[string]any{"type": "string"}},
								},
							},
						},
					},
				},
			},
		},
	}
	defs, err := openAPITools(doc, "", "", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(defs) != 1 {
		t.Fatalf("defs = %d", len(defs))
	}
	def := defs[0]
	if def.Body["name"] != "{name}" || def.Body["tier"] != "{tier}" {
		t.Fatalf("body = %#v", def.Body)
	}
	if def.NeedsConfirm == nil || !*def.NeedsConfirm {
		t.Fatal("POST should require confirmation by default")
	}
}
