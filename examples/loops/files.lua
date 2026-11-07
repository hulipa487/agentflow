-- af-prelude-version: 1
-- A note-taking loop over the user-scoped file store.
--
-- Needs: files. The project scope is resolved by the engine from the turn's
-- identity (user:<uuid> for a tenant turn, agent:<name> otherwise), so the
-- loop cannot name another user's project. Bytes never cross the bridge:
-- put() takes text, read() returns metadata plus a blob handle.

function loop()
  while true do
    local msg = session.inbox()
    if msg.type == "user" and msg.text then
      local stamp = os.date("%Y%m%d-%H%M%S")
      files.put("notes", "inbox/" .. stamp .. ".md", msg.text)

      local listing = files.list("notes", "inbox/")
      local count = #(listing.entries or {})

      -- Snapshot the tree. Commits are engine-native: a parent pointer, a
      -- message, and a named ref; checkout can restore the whole tree.
      files.commit("notes", { ref = "main", message = "note from " .. (msg.from or "?") })

      session.send("saved. " .. count .. " note(s) in the project.")
    elseif msg.type == "timer" then
      -- Maintenance turns have no channel; use state or a push.
      log.debug("file store heartbeat")
    end
  end
end
