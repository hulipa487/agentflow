package supervisor

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"agentflow/internal/builtins"
	"agentflow/internal/core/metrics"
	"agentflow/internal/core/session"
)

// SpawnResult is returned to the parent actor. The address is the runtime ID
// the parent uses to reach the child; the child's session ID is opaque.
type SpawnResult = session.SpawnResult

// Spawn creates an ephemeral child from a spawn profile. Grants are attenuated
// to the intersection of the parent's effective grants, the profile, and the
// global ceiling already baked into the template. The parent and child receive
// an implicit mutual contact grant for the child's lifetime only.
func (s *Supervisor) Spawn(ctx context.Context, parent session.Identity, profile string, spec map[string]any) (session.SpawnResult, error) {
	if !parent.Capabilities["agent.spawn"] {
		return session.SpawnResult{}, fmt.Errorf("agent %q lacks agent.spawn capability", parent.Agent)
	}
	tmpl, ok := s.templates[profile]
	if !ok {
		return session.SpawnResult{}, fmt.Errorf("unknown spawn profile %q", profile)
	}

	// Attenuate capabilities: child ⊆ parent ∩ template.
	childCaps := intersectCaps(parent.Capabilities, tmpl.Capabilities)
	if len(childCaps) == 0 {
		return session.SpawnResult{}, fmt.Errorf("spawn profile %q has no capabilities remaining after attenuation", profile)
	}

	// Contact ACL: parent↔child implicit, plus the template's own contacts
	// intersected with the parent's contacts (a child cannot reach an agent the
	// parent cannot reach).
	childContact := map[string]bool{parent.Agent: true}
	for target := range tmpl.CanContact {
		if parent.CanContact[target] {
			childContact[target] = true
		}
	}

	childID := "spawn-" + uuid.NewString()[:8]
	agentName := "spawn:" + profile
	skey := agentName + "|" + childID

	// Clone the template Info so per-session state does not leak across spawns.
	info := cloneInfo(tmpl.Memory)
	if info == nil {
		info = &session.Info{Name: agentName, Model: tmpl.Model}
	}
	info.Name = agentName
	info.Model = tmpl.Model
	info.Shell = tmpl.Shell
	info.Skills = tmpl.Skills
	info.Capabilities = keysOf(childCaps)

	identity := session.Identity{
		SessionID:    skey,
		Agent:        agentName,
		ParentID:     parent.SessionID,
		CanContact:   childContact,
		Capabilities: childCaps,
	}

	// Support chunks are read and version-gated before the child exists at all.
	// A refusal has to refuse the spawn: the child has no earlier version of
	// them to keep — it has never run — so the choice is between a child that
	// runs them and one that does not, and running a session without the chunks
	// its loop was written against is the silent downgrade the gate exists to
	// prevent. The error reaches the parent's spawn op, which is where a spawn
	// profile that cannot be served belongs.
	support, err := builtins.SupportChunks()
	if err != nil {
		return session.SpawnResult{}, fmt.Errorf("spawn profile %q: %w", profile, err)
	}

	// The child is a session like any other, so the per-tenant cap (E18)
	// applies here too — the spawn boundary is the other place actors are
	// created, and the child acts for the spawning turn's tenant.
	tenant := session.PersonalUUIDFromCtx(ctx)
	if tenant != "" && s.sessionCap > 0 {
		s.mu.Lock()
		atCap := s.countTenantLocked(tenant) >= s.sessionCap
		s.mu.Unlock()
		if atCap {
			return session.SpawnResult{}, fmt.Errorf("tenant %s is at the live-session cap (%d); refusing to spawn", tenant, s.sessionCap)
		}
	}

	a := session.New(skey, identity, info, s.gw, s, s.sched, s.users, tmpl.Safety, tmpl.Handlers, s.pool, s.log)
	a.LoopFile = tmpl.LoopFile
	a.LoopSrc = tmpl.LoopSrc
	a.SupportSrcs = support
	a.OnExit = s.onActorExit
	a.Journal = s.EgressJournal
	a.MemBudget = s.vmMemory

	actorCtx, cancel := context.WithCancel(s.ctx)

	s.mu.Lock()
	// Grant the parent a contact entry for the child agent name so the parent
	// can address session:<skey> or agent:<agentName> going forward.
	if parentActor, ok := s.sessions[parent.SessionID]; ok {
		if parentActor.Identity.CanContact == nil {
			parentActor.Identity.CanContact = map[string]bool{}
		}
		parentActor.Identity.CanContact[agentName] = true
	}
	s.sessions[skey] = a
	s.cancels[skey] = cancel
	if tenant != "" {
		s.tenants[skey] = tenant
	}
	s.mu.Unlock()

	go a.Run(actorCtx)
	metrics.Inc("agentflow_children_spawned")
	s.log.Info("child spawned", "child", skey, "parent", parent.SessionID, "profile", profile)

	return session.SpawnResult{
		Address:   "session:" + skey,
		SessionID: skey,
		Agent:     agentName,
	}, nil
}

func intersectCaps(parent, template map[string]bool) map[string]bool {
	out := map[string]bool{}
	for cap := range template {
		if parent[cap] {
			out[cap] = true
		}
	}
	return out
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// cloneInfo copies a spawn template's Info so per-session state does not leak
// across spawns. The copy is field-wise: map-typed config carried on the
// template (Extras, Shell, and the Credentials allow-list) is shared by
// reference, which is deliberate — it is read-only deployment data that
// agent.config() only reads (and renders secrets from as opaque markers), so
// every child of a profile sees its own extras without a per-spawn copy.
func cloneInfo(src *session.Info) *session.Info {
	if src == nil {
		return nil
	}
	cp := *src
	return &cp
}
