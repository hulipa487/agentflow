-- multimodal.lua — forwards inbound media attachments into llm.chat and
-- echoes a reply back. Works over any provider that accepts the part types
-- your channel ingests:
--   anthropic / gemini / openai-responses: image + pdf
--   openai (chat completions):            image + audio
--   gemini:                              also video
-- A part type a provider cannot take fails llm.chat with a clear error; the
-- loop below reports it honestly instead of crashing.

log.info("multimodal agent online")

function loop()
  while true do
    local msg = session.inbox()
    if msg.type ~= "user" then
      -- skip non-user turns (timers, agent messages, confirms)
    elseif not msg.attachments or #msg.attachments == 0 then
      session.send("Send me a photo or document and I'll describe it.")
    else
      local parts = {}
      table.insert(parts, { type = "text", text = msg.text or "Describe what this contains." })
      for _, att in ipairs(msg.attachments) do
        table.insert(parts, att) -- {type, mime, handle, name} — bytes stay in the blob store
      end

      local info = agent.info()
      local ok, reply = pcall(llm.chat,
        { { role = "user", parts = parts } },
        { model = info.model })

      if ok then
        session.send(reply.text or "[no text]")
      else
        session.send("⚠️ I couldn't process that media: " .. tostring(reply))
      end
    end
  end
end