-- builtin:fact_extractor — cheap after_turn tap that extracts facts.
--
-- Called as fact_extractor_turn(turn) after the assistant reply is sent.
-- Returns a list of memory records to write, or nil if nothing to remember.
--
-- Minimal heuristic: only records simple declarative sentences that look
-- like facts ("I am ...", "My name is ...", "The capital of ..."). A
-- production chain would run a small model here.

local function looks_like_fact(text)
  text = text or ""
  local patterns = {
    "^[iI] am ",
    "^[mM]y name is ",
    "^[tT]he capital of ",
    "^[oO]ur ",
    "^[tT]his is ",
  }
  for _, p in ipairs(patterns) do
    if text:match(p) then return true end
  end
  return false
end

function fact_extractor_turn(turn)
  local facts = {}
  if turn and turn.assistant and looks_like_fact(turn.assistant) then
    facts[#facts + 1] = {
      type = "fact",
      key = "fact:" .. tostring(turn.id or turn.ts or "unknown"),
      value = {
        text = turn.assistant,
        source = "after_turn",
      },
    }
  end
  if #facts == 0 then return nil end
  return facts
end
