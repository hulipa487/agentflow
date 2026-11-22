package config

import (
	"strings"
	"testing"
)

func TestValidateObservabilityTracing(t *testing.T) {
	baseYAML := `
runtime:
  persistence: "./data/af.db"
gateway:
  listen: 127.0.0.1:8080
models:
  default:
    provider: openai
    model: gpt-test
    api_key: test-key
`

	t.Run("valid otlp", func(t *testing.T) {
		cfg, err := loadTestConfigYAML(t, baseYAML+`
observability:
  tracing:
    enabled: true
    provider: otlp
    endpoint: https://otel.example.com
    sample_rate: 0.5
    flush_interval: 2s
    timeout: 5s
`)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg.Observability.Tracing.Sample() != 0.5 {
			t.Fatalf("sample rate = %v, want 0.5", cfg.Observability.Tracing.Sample())
		}
	})

	t.Run("langfuse requires credentials or auth header", func(t *testing.T) {
		_, err := loadTestConfigYAML(t, baseYAML+`
observability:
  tracing:
    enabled: true
    provider: langfuse
`)
		if err == nil || !strings.Contains(err.Error(), "public_key") {
			t.Fatalf("expected langfuse credential validation error, got %v", err)
		}
	})

	t.Run("otlp requires endpoint", func(t *testing.T) {
		_, err := loadTestConfigYAML(t, baseYAML+`
observability:
  tracing:
    enabled: true
    provider: otlp
`)
		if err == nil || !strings.Contains(err.Error(), "endpoint") {
			t.Fatalf("expected endpoint error, got %v", err)
		}
	})
}
