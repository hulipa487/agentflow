package caps

import (
	"context"
	"encoding/json"
	"fmt"

	"agentflow/internal/core/session"
)

// Gate restricts a handler map to agents that hold a capability.
//
// The capability lists — `agents.<name>.capabilities` and the global
// `plugins.allow_capabilities` ceiling — were validated at boot but never
// enforced at runtime for the ops behind these maps. Every agent was handed
// llm.chat, store.*, tools.*, shell.*, http.* and mail.* whatever it declared,
// so an agent that deliberately omitted net.http still had http.request, and
// one that omitted net.mail still had the mail ops. The declaration was
// documentation. This is what makes it binding.
//
// A withheld op is not removed from the map: it is replaced by a denial. The
// Lua prelude defines these functions unconditionally, so dropping the handler
// would turn a clear refusal into "attempt to call a nil value", which tells a
// loop author nothing about why. The denial names the op, the agent and the
// missing capability, matching the wording supervisor.Send/Request/Spawn
// already use for agent.send / agent.request / agent.spawn — those three were
// the only capabilities enforced anywhere, and this is the same rule applied to
// the rest.
//
// A nil granted map denies everything, which is the correct reading of "this
// agent holds no capabilities" and fails closed rather than open.
func Gate(hs map[string]session.OpHandler, capability, agent string, granted map[string]bool) map[string]session.OpHandler {
	if granted[capability] {
		return hs
	}
	out := make(map[string]session.OpHandler, len(hs))
	for name := range hs {
		op := name
		out[op] = func(context.Context, session.Op) (string, bool) {
			// JSON-encoded like every other failure from these handlers, so the
			// prelude's raise path surfaces it as an ordinary error string.
			b, _ := json.Marshal(fmt.Sprintf(
				"%s: agent %q lacks the %s capability", op, agent, capability))
			return string(b), false
		}
	}
	return out
}
