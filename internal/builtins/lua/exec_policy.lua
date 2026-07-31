-- builtin:exec_policy — shell.before_exec filter.
--
-- Inspects a command before it is sent to a shell handle. Returns "allow",
-- "deny", or a rewritten command string.
--
-- Configurable from agent YAML shell.before_exec config; this is the safe
-- default (deny obviously destructive patterns, allow everything else).

shell_before_exec_policy = shell_before_exec_policy or {}

function shell_before_exec_policy.check(cmd)
  if type(cmd) ~= "string" then return "deny" end

  local deny_patterns = {
    "rm%s+%-rf%s+/",
    "mkfs%.",
    "dd%s+if=",
    ":%(%)%s*{%s*:%s*|",
    ">%s*/dev/sd",
    "chmod%s+%-R%s+777",
  }
  for _, pat in ipairs(deny_patterns) do
    if cmd:find(pat) then
      return "deny"
    end
  end
  return "allow"
end
