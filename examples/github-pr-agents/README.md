# GitHub PR webhook that launches code agents

Every pull request that is opened or updated gets its own code agent: it
copies the change into a sandboxed workspace, reads and checks the code with
shell commands, and posts a review comment on the pull request.

| | |
| --- | --- |
| Channel | `ghhook` (GitHub webhook, HMAC-verified) |
| Loops | [`loops/pr_dispatcher.lua`](loops/pr_dispatcher.lua), [`loops/code_reviewer.lua`](loops/code_reviewer.lua) |
| Runs code in | one-off gVisor sandbox jobs (Linux), no network |
| Needs | a GitHub token, a public HTTPS URL for the webhook, an OpenAI-compatible model |

## How it works

```
GitHub ──pull_request──▶ ghhook ──▶ pr_dispatcher ──agent.spawn──▶ code_reviewer (one per PR)
                                                                   │  GitHub API: changed files → project "pr-<repo>-<n>"
                                                                   │  lua:run tool → shell.run in the sandbox
                                                                   └─ GitHub API: review comment, then exit
```

- **Dispatcher.** Every GitHub delivery reaches one session. For
  `pull_request` events with action `opened`, `synchronize`, `reopened` or
  `ready_for_review` (drafts are skipped) it spawns a `code_reviewer` from the
  `profiles.agent` template and sends it the job. Reviews run side by side.
- **Reviewer.** It writes the changed files at the head commit into its own
  files-store project, with `PR.md` and `PR.diff` at the root. The model gets
  one tool, `lua:run`, which runs a command on that project in a gVisor
  sandbox: no network, a read-only toolchain, and only the project's files
  survive. When it is done it posts the review and the session exits.
- **Secrets.** The GitHub token is the engine-wide credential `github`. Only
  the reviewer template may read it (`credentials: [github]`), every read is
  audited, and the loop only ever puts it in a request header.

## Run it

1. **Sandbox toolchain.** Reviews run on the `python` environment, a root
   filesystem at `examples/github-pr-agents/envs/python`. One way to make it:

   ```bash
   mkdir -p examples/github-pr-agents/envs/python
   docker export "$(docker create python:3.12-slim)" | tar -x -C examples/github-pr-agents/envs/python
   ```

   Or replace `rootfs:` with `image: docker.io/library/python:3.12-slim@sha256:<digest>`
   (the digest from `docker buildx imagetools inspect python:3.12-slim`).
   `agentflow doctor -config examples/github-pr-agents/agentflow.yaml` checks
   that the sandbox can run on this host.
2. **Secrets.** A fine-grained token with *Pull requests: read and write* and
   *Contents: read* on the repositories:

   ```bash
   export CREDENTIALS_MASTER_KEY=$(openssl rand -base64 32)   # keep it
   printf %s "$GITHUB_TOKEN" | agentflow cred set github -config examples/github-pr-agents/agentflow.yaml
   export OPENAI_API_KEY=sk-...  GITHUB_WEBHOOK_SECRET=$(openssl rand -hex 20)
   ```

3. **Start it** behind a TLS reverse proxy (GitHub needs a public HTTPS URL):

   ```bash
   agentflow -config examples/github-pr-agents/agentflow.yaml
   ```

4. **Webhook.** In the repository: *Settings → Webhooks → Add webhook*.
   Payload URL `https://<your host>/webhook/github/change-me-3b9d71c4/` (change
   the path in `agentflow.yaml` first), content type `application/json`,
   secret `$GITHUB_WEBHOOK_SECRET`, events: *Pull requests*. The ping shows
   up as `GitHub webhook connected` in the log.

Open a pull request; a review comment follows in a minute or two.

## Make it yours

- **What it checks** — [`prompts/reviewer.md`](prompts/reviewer.md), and the
  `python` environment's toolchain (add linters to the root filesystem).
- **Other events** — the dispatcher's `REVIEW_ON` table; `issue_comment` can
  trigger a review on demand.
- **Fix, not just review** — the reviewer can write files with `lua:run`;
  `files.commit` and a branch push through the GitHub API turn it into a
  fixing agent.
- **Cost** — `budget.tokens_per_day` on the template caps every reviewer
  together.
