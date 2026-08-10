-- builtin:semantic — embedding-based memory.recall handler.
--
-- Selected by `recall: builtin:semantic` on the agent's memory profile; the
-- prelude's memory.recall dispatches here. The pipeline:
--
--   embed query text (profile embed_model)
--   -> vector query on the target store (k, or k * oversample when reranking)
--   -> cross-encoder rerank (profile rerank_model, when set)
--   -> top-k records
--
-- Query fields: text (required), store, k. The store's backend must be
-- vector-capable (declare `requires: ["vector"]` on the store so a bad
-- binding fails at boot, not at recall time).
--
-- The result is a list of memory records: { key = ..., value = ..., meta = ... }.
-- Reranked records carry meta.rerank_score.
local function record_text(r)
  local v = r.value
  if type(v) == "table" then
    v = v.text or json.encode(v)
  end
  return tostring(v or "")
end

function memory_semantic_recall_handler(query, opts)
  query = query or {}
  opts = opts or {}
  local info = agent.info()
  local mem = info.memory or {}
  local stores = mem.stores or {}

  local text = query.text or query.q
  if not text or text == "" then
    -- Non-semantic query (e.g. a loop's kind="all" history fetch): defer to
    -- the default recency handler so mixed profiles keep working.
    return memory_recall_handler(query, opts)
  end

  local embed_model = opts.model or mem.embed_model
  if not embed_model or embed_model == "" then
    error("semantic recall requires embed_model on the memory profile (or opts.model)", 2)
  end

  local store_name = query.store
  if not store_name then
    if stores.semantic then
      store_name = "semantic"
    elseif stores.dialogue then
      store_name = "dialogue"
    elseif next(stores) then
      for k in pairs(stores) do store_name = k; break end
    end
  end
  if not store_name then
    return {}
  end

  local k = query.k or 10
  local rerank_model = opts.rerank_model or mem.rerank_model
  local have_rerank = rerank_model and rerank_model ~= ""

  local fetch = k
  if have_rerank then
    local oversample = mem.oversample
    if not oversample or oversample < 1 then oversample = 4 end
    fetch = k * oversample
  end

  local emb = llm.embed({ text }, { model = embed_model })
  local vector = emb and emb.vectors and emb.vectors[1]
  if not vector then
    error("semantic recall: embedding model " .. embed_model .. " returned no vector", 2)
  end

  local result = store.query(store_name, { kind = "vector", vector = vector, k = fetch })
  local records = result.records or {}

  if have_rerank and #records > 1 then
    local docs = {}
    for i, r in ipairs(records) do
      docs[i] = record_text(r)
    end
    local rr = llm.rerank(text, docs, { model = rerank_model, top_n = k })
    local ranked = {}
    for _, res in ipairs((rr and rr.results) or {}) do
      -- rerank indices are 0-based into docs
      local rec = records[res.index + 1]
      if rec then
        rec.meta = rec.meta or {}
        rec.meta.rerank_score = res.score
        ranked[#ranked + 1] = rec
      end
    end
    records = ranked
  end

  local out = {}
  for i = 1, math.min(k, #records) do
    out[i] = records[i]
  end
  return out
end
