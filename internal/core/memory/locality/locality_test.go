package locality

import "testing"

// TestFor pins the derivation table: it is the single answer both the validator
// and agent.info use, so a change here is a change to the documented model.
func TestFor(t *testing.T) {
	cases := []struct {
		provider string
		topology string
		want     Locality
	}{
		{"postgres", "single", Deployment},
		{"postgres", "regional", Deployment},
		{"pgvector", "regional", Deployment},
		{"qdrant", "regional", Deployment},
		{"sqlite", "single", Deployment},
		// Redis is one region-wide cluster inside a regional cell; in single
		// the instance is the deployment.
		{"redis", "single", Deployment},
		{"redis", "regional", Region},
		{"redisvector", "single", Deployment},
		{"redisvector", "regional", Region},
		{"volatile", "single", Instance},
		{"volatile", "regional", Instance},
		{"unknown-provider", "regional", Instance},
	}
	for _, c := range cases {
		if got := For(c.provider, c.topology); got != c.want {
			t.Errorf("For(%q, %q) = %q, want %q", c.provider, c.topology, got, c.want)
		}
	}
}
