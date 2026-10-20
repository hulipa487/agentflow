package studio

import "agentflow/internal/vm"

// Catalog is the builder's menu: what this deployment can offer an agent. It
// is assembled by main from the live registries (models, tools, config
// profiles), so the app never guesses at a name the runtime would refuse.
type Catalog struct {
	Models         []string   `json:"models"`
	Tools          []string   `json:"tools"`
	Capabilities   []string   `json:"capabilities"`
	DefaultCaps    []string   `json:"default_capabilities"`
	SafetyProfiles []string   `json:"safety_profiles"`
	MemoryProfiles []string   `json:"memory_profiles"`
	PromptKeys     []string   `json:"prompt_keys"`
	PreludeVersion int        `json:"prelude_version"`
	Templates      []Template `json:"templates"`
}

// CapabilityNames is every capability the gate families understand. The
// builder offers these as checkboxes; a deployment's plugins.allow_capabilities
// ceiling still decides which are accepted.
var CapabilityNames = []string{
	"llm.chat", "memory", "tools", "agent.send", "net.http", "net.mail",
	"shell.exec", "files", "session.state", "users", "channel.push",
}

// NewCatalog assembles the menu. Empty slices are normalized to [] so the app
// can render them without nil checks.
func NewCatalog(models, tools, safety, memory, prompts, defaultCaps []string) Catalog {
	c := Catalog{
		Models:         sorted(models),
		Tools:          sorted(tools),
		Capabilities:   CapabilityNames,
		DefaultCaps:    sorted(defaultCaps),
		SafetyProfiles: sorted(safety),
		MemoryProfiles: sorted(memory),
		PromptKeys:     sorted(prompts),
		PreludeVersion: vm.PreludeVersion,
		Templates:      Templates(),
	}
	return c
}

func sorted(in []string) []string {
	out := make([]string, 0, len(in))
	seen := map[string]bool{}
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	sortStrings(out)
	return out
}

// sortStrings is a tiny insertion sort: these lists are short and this keeps
// the package free of a sort import in the catalog path.
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
