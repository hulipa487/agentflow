-- shell_loop.lua — example loop that spawns a persistent Docker shell,
-- runs a command, and destroys the handle.
-- Requires Docker installed and a shell profile (or spawn opts) that use provider=docker.

function loop()
  while true do
    local msg = session.inbox()
    if msg.type == "user" and msg.text then
      local ok, h = pcall(shell.spawn, {
        provider = "docker",
        image = "alpine:3.20",
        workdir = "/work",
        network = "none",
      })
      if not ok then
        session.send("spawn failed: " .. tostring(h))
        return
      end

      local r = shell.exec(h.id, "echo hello from " .. msg.text)
      shell.destroy(h.id)
      session.send("exit=" .. r.exit_code .. " stdout=" .. (r.stdout or ""))
    end
  end
end
