package config

import "testing"

func TestValidateHTTPTools(t *testing.T) {
	base := func() *Config {
		return &Config{Tools: Tools{HTTP: []HTTPTool{{
			Name: "ok", Method: "GET", URL: "https://api.example.com/x",
		}}}}
	}
	if err := validateHTTPTools("test", base()); err != nil {
		t.Fatalf("valid tool refused: %v", err)
	}

	cases := []struct {
		name string
		mut  func(*HTTPTool)
	}{
		{"missing name", func(t *HTTPTool) { t.Name = "" }},
		{"bad url", func(t *HTTPTool) { t.URL = "ftp://x" }},
		{"bad method", func(t *HTTPTool) { t.Method = "TRACE" }},
		{"bad timeout", func(t *HTTPTool) { t.Timeout = "nope" }},
		{"bad auth", func(t *HTTPTool) { t.Auth = &HTTPAuth{} }},
		{"bad host", func(t *HTTPTool) { t.Hosts = []string{"https://x"} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := base()
			tc.mut(&c.Tools.HTTP[0])
			if err := validateHTTPTools("test", c); err == nil {
				t.Fatal("expected refusal")
			}
		})
	}

	c := base()
	c.Tools.HTTP = append(c.Tools.HTTP, c.Tools.HTTP[0])
	if err := validateHTTPTools("test", c); err == nil {
		t.Fatal("duplicate names must be refused")
	}
}
