-- af-prelude-version: 1
-- A code agent that reviews one pull request, comments, and exits.
--
-- Spawned by pr_dispatcher with the job as its first message. It:
--   1. lists the pull request's changed files through the GitHub API and
--      writes them, at the head commit, into a files-store project of its
--      own, with PR.md (title and description) and PR.diff (the patches);
--   2. works through the change with one tool, lua:run, which runs a shell
--      command on that project in a gVisor sandbox: cat, grep, a compile or
--      lint check — no network, and nothing outside the project survives;
--   3. posts its review as a comment on the pull request.
--
-- The GitHub token is the engine-wide credential "github", allowed for this
-- profile by credentials: [github]. It is read once per job and only ever
-- placed in a request header.

local MAX_FILES = 40        -- changed files copied into the sandbox project
local MAX_CHANGES = 2000    -- skip files with more changed lines than this
local MAX_ROUNDS = 8        -- tool rounds before the review must be written
local OUTPUT_CHARS = 6000   -- command output handed back to the model

local project = nil         -- the current job's sandbox project
local token = nil

local function clip(s, n)
  s = tostring(s or "")
  if #s > n then
    return s:sub(1, n) .. "\n…(truncated)"
  end
  return s
end

tool.def({
  name = "lua:run",
  description = "Run a shell command in the pull request's checkout: the changed files at the head commit, plus PR.md and PR.diff at the root. There is no network. Use it to read files (cat, sed -n), search (grep -rn) and run quick checks (python -m py_compile, etc.).",
  params = {
    type = "object",
    properties = { command = { type = "string", description = "The shell command to run." } },
    required = { "command" },
  },
  handler = function(args)
    if not project then
      return { ok = false, error = "no pull request is checked out" }
    end
    local ok, r = pcall(shell.run, tostring(args.command or ""), { project = project, timeout = 60 })
    if not ok then
      return { ok = false, error = tostring(r) }
    end
    return {
      ok = true,
      exit_code = r.exit_code,
      timed_out = r.timed_out,
      stdout = clip(r.stdout, OUTPUT_CHARS),
      stderr = clip(r.stderr, 2000),
    }
  end,
})

local function escape_path(path)
  return (path:gsub("[^%w%-%._~/]", function(c)
    return string.format("%%%02X", string.byte(c))
  end))
end

local function github(method, path, opts)
  opts = opts or {}
  return http.request({
    method = method,
    url = "https://api.github.com" .. path,
    headers = {
      ["Accept"] = opts.accept or "application/vnd.github+json",
      ["Authorization"] = "Bearer " .. token,
      ["X-GitHub-Api-Version"] = "2022-11-28",
      ["User-Agent"] = "agentflow-pr-reviewer",
    },
    json = opts.json,
    timeout = 30,
  })
end

-- checkout copies the pull request into the job's project.
local function checkout(job)
  local r = github("GET", "/repos/" .. job.repo .. "/pulls/" .. job.number .. "/files?per_page=100")
  if r.status ~= 200 then
    error("listing the pull request's files: HTTP " .. tostring(r.status))
  end
  local changed = json.decode(r.body) or {}
  local diff, copied = {}, 0
  for _, f in ipairs(changed) do
    if f.patch then
      table.insert(diff, "--- " .. f.filename .. " (" .. tostring(f.status) .. ")\n" .. f.patch)
    end
    if f.status ~= "removed" and copied < MAX_FILES and (f.changes or 0) <= MAX_CHANGES then
      local c = github("GET", "/repos/" .. job.repo .. "/contents/" .. escape_path(f.filename) .. "?ref=" .. tostring(job.head_sha),
        { accept = "application/vnd.github.raw" })
      if c.status == 200 and not c.truncated then
        -- Binary files are not utf-8 text and are left out.
        if pcall(files.put, project, f.filename, c.body) then
          copied = copied + 1
        end
      end
    end
  end
  files.put(project, "PR.md", "# " .. job.title .. "\n\nby @" .. tostring(job.author) .. "\n\n" .. job.body)
  files.put(project, "PR.diff", table.concat(diff, "\n\n"))
  return #changed, copied
end

-- review runs the tool loop and returns the review text.
local function review(job, total, copied)
  local info = agent.info()
  local run_tool = {}
  for _, t in ipairs(tools.list()) do
    if t["function"] and t["function"].name == "lua:run" then
      table.insert(run_tool, t)
    end
  end
  local messages = {
    { role = "system", content = info.instructions or "You review pull requests." },
    { role = "user", content = string.format(
      "Review pull request #%d in %s: %s\n%d file(s) changed; %d are in your checkout. Start with `cat PR.md PR.diff`.",
      job.number, job.repo, job.title, total, copied) },
  }
  for _ = 1, MAX_ROUNDS do
    local reply = llm.chat(messages, { tools = run_tool, tool_choice = "auto" })
    local calls = reply.tool_calls or {}
    if #calls == 0 then
      return reply.text or ""
    end
    table.insert(messages, { role = "assistant", content = reply.text or "", tool_calls = calls, thinking_blocks = reply.thinking_blocks })
    for _, call in ipairs(calls) do
      local ok, result = pcall(tools.run, call.name, call.args or {})
      if not ok then
        result = { ok = false, error = tostring(result) }
      end
      table.insert(messages, { role = "tool", tool_call_id = call.id, content = json.encode(result) })
    end
  end
  -- Out of rounds: ask for the review with what has been seen so far.
  table.insert(messages, { role = "user", content = "Write the review now, from what you have seen." })
  return llm.chat(messages).text or ""
end

local function run_job(job)
  local cred = credential.get("github")
  if not cred or not cred.value or cred.value == "" then
    error("no GitHub token: store one with `agentflow cred set github`")
  end
  token = cred.value
  project = "pr-" .. job.repo:lower():gsub("[^%w]+", "-") .. "-" .. tostring(job.number)
  local total, copied = checkout(job)
  local text = review(job, total, copied)
  local r = github("POST", "/repos/" .. job.repo .. "/issues/" .. job.number .. "/comments",
    { json = { body = text .. "\n\n<sub>Reviewed by an AgentFlow code agent at " .. tostring(job.head_sha):sub(1, 7) .. ".</sub>" } })
  if r.status ~= 201 then
    error("posting the review: HTTP " .. tostring(r.status))
  end
  log.info("reviewed " .. job.repo .. "#" .. job.number)
end

function loop()
  local msg = session.inbox()
  local job = msg.payload or {}
  if job.repo and job.number then
    local ok, err = pcall(run_job, job)
    if not ok then
      log.error("review of " .. tostring(job.repo) .. "#" .. tostring(job.number) .. " failed: " .. tostring(err))
    end
  end
  token = nil
  session.exit()
end
