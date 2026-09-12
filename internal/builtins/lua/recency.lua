-- builtin:recency — default memory.recall handler.
--
-- Returns the most recent records from the agent's stores. Query.kind selects
-- the access pattern:
--   "all"      -> all records from the "dialogue" store (default)
--   "prefix"   -> prefix scan on the store named by query.store
--   "text"     -> full-text search on the "facts" store
--   "time"     -> time range on the store named by query.store
--
-- The result is a list of memory records: { key = ..., value = ..., meta = ... }.

function memory_recall_handler(query, opts)
  query = query or {}
  opts = opts or {}
  local info = agent.info()
  local stores = (info.memory and info.memory.stores) or {}

  local store_name = query.store
  if not store_name then
    if stores.dialogue then
      store_name = "dialogue"
    elseif next(stores) then
      for k in pairs(stores) do store_name = k; break end
    end
  end
  if not store_name then
    return {}
  end

  local q = { kind = query.kind or "all", table = store_name }
  if query.kind == "prefix" then
    q.prefix = query.prefix
  elseif query.kind == "text" then
    q.text = query.text
    q.k = query.k or 10
  elseif query.kind == "time" then
    q.from = query.from
    q.to = query.to
  end

  local result = store.query(store_name, q)
  return result.records or {}
end
