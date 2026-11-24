// af-prelude-version: 2
// plugin:semantic — embedding-based memory.recall handler.
//
// Selected by `recall: plugin:semantic` on the agent's memory profile.
// Pipeline: embed query -> vector query -> optional cross-encoder rerank -> top-k.

interface SemanticQuery {
  store?: string;
  text?: string;
  q?: string;
  k?: number;
  [k: string]: unknown;
}

interface SemanticRecallOpts {
  model?: string;
  rerank_model?: string;
  [k: string]: unknown;
}

interface SemanticRecord {
  value?: unknown;
  meta?: Record<string, unknown>;
  [k: string]: unknown;
}

function memory_semantic_recall_handler(query?: SemanticQuery | null, opts?: SemanticRecallOpts | null): unknown[] {
  const qIn = query || {};
  const oIn = opts || {};
  const info = agent.info();
  const mem = (info && info.memory) || {};
  const stores = mem.stores || {};

  const text = qIn.text || qIn.q;
  if (!text) {
    return memory_recall_handler(qIn, oIn);
  }

  const embedModel = oIn.model || mem.embed_model;
  if (!embedModel) {
    throw new Error("semantic recall requires embed_model on the memory profile (or opts.model)");
  }

  let storeName = qIn.store;
  if (!storeName) {
    if (stores.semantic) {
      storeName = "semantic";
    } else if (stores.dialogue) {
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

  const k = qIn.k !== undefined && qIn.k !== null ? qIn.k : 10;
  const rerankModel = oIn.rerank_model || mem.rerank_model;
  const haveRerank = Boolean(rerankModel);

  let fetchCount = k;
  if (haveRerank) {
    const oversample = mem.oversample && mem.oversample >= 1 ? mem.oversample : 4;
    fetchCount = k * oversample;
  }

  const emb = llm.embed([text], { model: embedModel });
  const vector = emb && emb.vectors && emb.vectors[0];
  if (!vector) {
    throw new Error(`semantic recall: embedding model ${embedModel} returned no vector`);
  }

  const result = store.query(storeName, { kind: "vector", vector: vector, k: fetchCount });
  let records = ((result && result.records) || []) as SemanticRecord[];

  const recordText = (r: SemanticRecord): string => {
    const v = r.value;
    if (v && typeof v === "object") {
      const obj = v as Record<string, unknown>;
      if (obj.text !== undefined && obj.text !== null) {
        return String(obj.text);
      }
      return JSON.stringify(obj);
    }
    return String(v !== undefined && v !== null ? v : "");
  };

  if (haveRerank && records.length > 1) {
    const docs = records.map(recordText);
    const rr = llm.rerank(text, docs, { model: rerankModel, top_n: k });
    const ranked: SemanticRecord[] = [];
    for (const res of (rr && rr.results) || []) {
      const rec = records[res.index];
      if (rec) {
        rec.meta = rec.meta || {};
        rec.meta.rerank_score = res.score;
        ranked.push(rec);
      }
    }
    records = ranked;
  }

  return records.slice(0, Math.max(0, k));
}
