package main

// OpenAPI import: turn an OpenAPI 3 document into tools.http entries. This is
// a one-way generator, not a runtime dependency: the operator reviews and
// commits the YAML, and the runtime never fetches a spec at boot.

import (
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"agentflow/internal/config"
)

func cmdTools(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "import-openapi" {
		fmt.Fprintln(stderr, "usage: agentflow tools import-openapi <spec.yaml|spec.json> [-out file] [-base-url url] [-auth service] [-prefix p]")
		return 2
	}
	fs := flag.NewFlagSet("tools import-openapi", flag.ContinueOnError)
	fs.SetOutput(stderr)
	out := fs.String("out", "", "write the generated tools block to this file instead of stdout")
	baseURL := fs.String("base-url", "", "override the server URL from the spec")
	auth := fs.String("auth", "", "credential service to reference as auth.service")
	prefix := fs.String("prefix", "", "prefix every generated tool name")
	readOnly := fs.Bool("read-only", false, "only import GET and HEAD operations")
	if err := fs.Parse(hoistFlagsWithBools(args[1:], map[string]bool{"-read-only": true})); err != nil {
		return 2
	}
	if fs.NArg() == 0 {
		fmt.Fprintln(stderr, "tools import-openapi: a spec file is required")
		return 2
	}
	if fs.NArg() > 1 {
		fmt.Fprintln(stderr, "tools import-openapi: unexpected argument:", fs.Arg(1))
		return 2
	}
	b, err := os.ReadFile(fs.Arg(0))
	if err != nil {
		fmt.Fprintln(stderr, "tools import-openapi:", err)
		return 1
	}
	var doc map[string]any
	if err := yaml.Unmarshal(b, &doc); err != nil {
		fmt.Fprintln(stderr, "tools import-openapi: parse:", err)
		return 1
	}
	defs, err := openAPITools(doc, *baseURL, *auth, *prefix, *readOnly)
	if err != nil {
		fmt.Fprintln(stderr, "tools import-openapi:", err)
		return 1
	}
	if len(defs) == 0 {
		fmt.Fprintln(stderr, "tools import-openapi: the spec produced no operations")
		return 1
	}
	block := map[string]any{"tools": map[string]any{"http": defs}}
	outBytes, err := yaml.Marshal(block)
	if err != nil {
		fmt.Fprintln(stderr, "tools import-openapi:", err)
		return 1
	}
	if *out == "" {
		_, _ = stdout.Write(outBytes)
		return 0
	}
	if err := os.WriteFile(*out, outBytes, 0o644); err != nil {
		fmt.Fprintln(stderr, "tools import-openapi:", err)
		return 1
	}
	fmt.Fprintf(stdout, "wrote %d tools to %s\n", len(defs), *out)
	return 0
}

func openAPITools(doc map[string]any, baseOverride, auth, prefix string, readOnly bool) ([]config.HTTPTool, error) {
	base := baseOverride
	if base == "" {
		base = openAPIServerURL(doc)
	}
	paths, _ := doc["paths"].(map[string]any)
	if paths == nil {
		return nil, fmt.Errorf("no paths object")
	}
	pathNames := make([]string, 0, len(paths))
	for p := range paths {
		pathNames = append(pathNames, p)
	}
	sort.Strings(pathNames)

	seen := map[string]bool{}
	var out []config.HTTPTool
	for _, path := range pathNames {
		item, _ := paths[path].(map[string]any)
		if item == nil {
			continue
		}
		for _, method := range []string{"get", "post", "put", "patch", "delete", "head", "options"} {
			op, _ := item[method].(map[string]any)
			if op == nil {
				continue
			}
			if readOnly && method != "get" && method != "head" {
				continue
			}
			name := operationName(op, method, path)
			if prefix != "" {
				name = prefix + "_" + name
			}
			name = sanitizeToolName(name)
			if name == "" {
				continue
			}
			for seen[name] {
				name += "_2"
			}
			seen[name] = true

			def := config.HTTPTool{
				Name:        name,
				Description: firstNonEmpty(openAPIString(op["summary"]), openAPIString(op["description"]), method+" "+path),
				Method:      strings.ToUpper(method),
				URL:         joinOpenAPIURL(base, path),
				Params:      map[string]any{"type": "object", "properties": map[string]any{}},
			}
			if auth != "" {
				def.Auth = &config.HTTPAuth{Service: auth}
			}
			if h := hostOf(def.URL); h != "" {
				def.Hosts = []string{h}
			}
			if method != "get" && method != "head" {
				t := true
				def.NeedsConfirm = &t
			}
			applyOpenAPIParameters(&def, item, op)
			applyOpenAPIRequestBody(&def, op)
			out = append(out, def)
		}
	}
	return out, nil
}

func openAPIServerURL(doc map[string]any) string {
	if servers, ok := doc["servers"].([]any); ok && len(servers) > 0 {
		if s, ok := servers[0].(map[string]any); ok {
			return openAPIString(s["url"])
		}
	}
	return ""
}

func operationName(op map[string]any, method, path string) string {
	if id := openAPIString(op["operationId"]); id != "" {
		return id
	}
	cleaned := strings.NewReplacer("/", "_", "{", "", "}", "", "-", "_").Replace(strings.Trim(path, "/"))
	if cleaned == "" {
		cleaned = "root"
	}
	return method + "_" + cleaned
}

func sanitizeToolName(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return strings.Trim(b.String(), "_")
}

func joinOpenAPIURL(base, path string) string {
	base = strings.TrimRight(base, "/")
	if base == "" {
		return path
	}
	if path == "" {
		return base
	}
	return base + "/" + strings.TrimLeft(path, "/")
}

func hostOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

func applyOpenAPIParameters(def *config.HTTPTool, item, op map[string]any) {
	props, _ := def.Params["properties"].(map[string]any)
	required := []string{}
	addParams := func(raw any) {
		arr, _ := raw.([]any)
		for _, p := range arr {
			pm, _ := p.(map[string]any)
			if pm == nil {
				continue
			}
			name := openAPIString(pm["name"])
			in := openAPIString(pm["in"])
			if name == "" || in == "header" || in == "cookie" {
				continue
			}
			schema, _ := pm["schema"].(map[string]any)
			if schema == nil {
				schema = map[string]any{"type": "string"}
			} else {
				schema = cloneMap(schema)
			}
			if d := openAPIString(pm["description"]); d != "" {
				schema["description"] = d
			}
			props[name] = schema
			if b, _ := pm["required"].(bool); b {
				required = append(required, name)
			}
			if in == "query" {
				if def.Query == nil {
					def.Query = map[string]string{}
				}
				def.Query[name] = "{" + name + "}"
			}
		}
	}
	addParams(item["parameters"])
	addParams(op["parameters"])
	def.Params["properties"] = props
	if len(required) > 0 {
		def.Params["required"] = required
	}
}

func applyOpenAPIRequestBody(def *config.HTTPTool, op map[string]any) {
	rb, _ := op["requestBody"].(map[string]any)
	if rb == nil {
		return
	}
	content, _ := rb["content"].(map[string]any)
	if content == nil {
		return
	}
	media, _ := content["application/json"].(map[string]any)
	if media == nil {
		// Try the first media type.
		for _, v := range content {
			if m, ok := v.(map[string]any); ok {
				media = m
				break
			}
		}
	}
	if media == nil {
		return
	}
	schema, _ := media["schema"].(map[string]any)
	if schema == nil {
		return
	}
	props, _ := schema["properties"].(map[string]any)
	if props == nil {
		return
	}
	body := map[string]any{}
	modelProps, _ := def.Params["properties"].(map[string]any)
	required := []string{}
	if req, ok := schema["required"].([]any); ok {
		for _, r := range req {
			if s, ok := r.(string); ok {
				required = append(required, s)
			}
		}
	}
	keys := make([]string, 0, len(props))
	for k := range props {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		body[k] = "{" + k + "}"
		if modelProps != nil {
			if s, ok := props[k].(map[string]any); ok {
				modelProps[k] = cloneMap(s)
			} else {
				modelProps[k] = map[string]any{"type": "string"}
			}
		}
	}
	if modelProps != nil {
		def.Params["properties"] = modelProps
	}
	if len(required) > 0 {
		def.Params["required"] = required
	}
	def.Body = body
}

func cloneMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func openAPIString(v any) string {
	s, _ := v.(string)
	return s
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
