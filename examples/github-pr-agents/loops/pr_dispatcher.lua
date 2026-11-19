-- af-prelude-version: 1
-- GitHub pull request dispatcher.
--
-- Every GitHub delivery reaches this one session (the ghhook channel signs
-- and forwards events as msg.payload = { source = "github", event, data }).
-- For a pull request that was opened, updated, reopened or marked ready, it
-- spawns a code_reviewer from the profiles.agent template and hands it the
-- job. Each reviewer is its own session with its own sandbox project, works
-- the pull request through, comments on it, and exits — so reviews run side
-- by side and one slow review never holds up the next event.

local REVIEW_ON = { opened = true, synchronize = true, reopened = true, ready_for_review = true }

local function job_from(data)
  local pr = data.pull_request or {}
  local repo = (data.repository or {}).full_name
  if not repo or not pr.number or pr.draft then
    return nil
  end
  return {
    repo = repo,
    number = pr.number,
    head_sha = (pr.head or {}).sha,
    title = pr.title or "",
    body = pr.body or "",
    url = pr.html_url,
    author = (pr.user or {}).login,
  }
end

function loop()
  while true do
    local msg = session.inbox()
    local p = msg.payload or {}
    local data = p.data or {}
    if p.source == "github" and p.event == "pull_request" and REVIEW_ON[data.action] then
      local job = job_from(data)
      if job then
        local ok, child = pcall(agent.spawn, "code_reviewer")
        if ok and child and child.address then
          agent.send(child.address, job)
          log.info("review started: " .. job.repo .. "#" .. job.number .. " -> " .. child.address)
        else
          log.error("could not start a reviewer for " .. job.repo .. "#" .. job.number .. ": " .. tostring(child))
        end
      end
    elseif p.event == "ping" then
      log.info("GitHub webhook connected")
    end
  end
end
