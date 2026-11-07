// Package locality names where a memory backend's data lives relative to the
// sessions that read it.
//
// The label is derived, never configured: it follows from the provider and the
// deployment tier. A loop can read it (agent.info().memory.stores[*].locality)
// and the validator uses it to refuse durable conversation stores on backends
// that die with one instance.
//
// It is a leaf package on purpose: config needs the derivation for validation
// and memory needs it for the binding, and config cannot import memory (memory
// reaches config through session). No dependencies means no cycle.
package locality

// Locality is the reach of a backend's data.
type Locality string

const (
	// Deployment means every instance of the deployment can read it, including
	// across regions when the deployment is one Cell. PostgreSQL, pgvector,
	// Qdrant, and (in single) SQLite and Redis.
	Deployment Locality = "deployment"
	// Region means every instance in one region can read it: Redis in a
	// regional cell, where sessions may move between AZs but never between
	// cells.
	Region Locality = "region"
	// Instance means the data dies with the process: volatile memory, and
	// anything unknown.
	Instance Locality = "instance"
)

// For derives a provider's locality in the given topology. topology is the
// normalized tier ("single" or "regional"); anything else is treated as single,
// which is the backward-compatible reading.
func For(provider, topology string) Locality {
	switch provider {
	case "postgres", "pgvector", "qdrant":
		// Shared by construction: every instance reaches the same server.
		return Deployment
	case "sqlite":
		// In single the one instance is the deployment. A regional cell refuses
		// SQLite at validation, so reaching here with regional topology is a
		// caller that skipped validation; the conservative answer is still
		// deployment, because the validator is the gate that matters.
		return Deployment
	case "redis", "redisvector":
		if topology == "regional" {
			return Region
		}
		return Deployment
	case "volatile":
		return Instance
	default:
		// Unknown providers are treated as the narrowest reach. A caller that
		// reaches this branch has a provider the engine does not know, and
		// guessing wider would be guessing in the unsafe direction.
		return Instance
	}
}
