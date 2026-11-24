// af-prelude-version: 2
// plugin:recency — default memory.recall handler.
//
// Query.kind selects the access pattern:
//   "all"    -> all records from the "dialogue" store (default)
//   "prefix" -> prefix scan on the store named by query.store
//   "text"   -> full-text search on query.store or default store
//   "time"   -> time range on the store named by query.store

interface RecencyQuery {
  store?: string;
  kind?: string;
  prefix?: string;
  text?: string;
  k?: number;
  from?: string;
  to?: string;
  [k: string]: unknown;
}

function memory_recall_handler(query?: RecencyQuery | null, _opts?: Record<string, unknown>): unknown[] {
  const qIn = query || {};
  const info = agent.info();
  const stores = (info && info.memory && info.memory.stores) || {};

  let storeName = qIn.store;
  if (!storeName) {
    if (stores.dialogue) {
      storeName = "dialogue";
    } else {
      const keys = Object.keys(stores);
      if (keys.length > 0) {
        storeName = keys[0];
      }
    }
  }
  if (!storeName) {
    return [];
  }

  const q: Record<string, unknown> = {
    kind: qIn.kind || "all",
    table: storeName,
  };
  if (qIn.kind === "prefix") {
    q.prefix = qIn.prefix;
  } else if (qIn.kind === "text") {
    q.text = qIn.text;
    q.k = qIn.k !== undefined && qIn.k !== null ? qIn.k : 10;
  } else if (qIn.kind === "time") {
    // Backends switch on "time_range" with RFC3339 timestamps.
    q.kind = "time_range";
    q.from = qIn.from;
    q.to = qIn.to;
  }

  const result = store.query(storeName, q);
  return (result && result.records) || [];
}
