// af-prelude-version: 2
// plugin:routing_table — default memory.write filter chain.
//
// Default: route "turn" records to the "dialogue" store and "fact" records
// to the "facts" store.

interface RoutingTargetSpec {
  store: string;
  key: string;
  value: unknown;
  ttl?: number;
}

interface RoutingMemoryRecord {
  type?: string;
  id?: string;
  key?: string;
  ts?: string | number;
  ttl?: number;
  targets?: RoutingTargetSpec[];
  [k: string]: unknown;
}

function memory_routing_table(record?: RoutingMemoryRecord | null): RoutingTargetSpec[] {
  if (!record) return [];

  const info = agent.info();
  const stores = (info && info.memory && info.memory.stores) || {};
  const targets: RoutingTargetSpec[] = [];

  if (record.type === "turn") {
    if (stores.dialogue) {
      const key = record.id !== undefined && record.id !== null ? record.id : record.ts;
      targets.push({
        store: "dialogue",
        key: "turn:" + String(key !== undefined && key !== null ? key : "unknown"),
        value: record,
        ttl: record.ttl,
      });
    }
  } else if (record.type === "fact") {
    if (stores.facts) {
      const rawKey = record.key !== undefined && record.key !== null ? record.key : record.id;
      targets.push({
        store: "facts",
        key: String(rawKey !== undefined && rawKey !== null ? rawKey : "unknown"),
        value: record,
        ttl: record.ttl,
      });
    }
  } else if (Array.isArray(record.targets)) {
    for (const t of record.targets) {
      targets.push(t);
    }
  }

  return targets;
}
