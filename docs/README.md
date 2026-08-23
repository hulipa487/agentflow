# AgentFlow Documentation Site

A static documentation website for AgentFlow, built with plain HTML/CSS/JS — no build step required.

## Features

- **Light/dark mode** toggle (persists to localStorage, defaults to dark)
- **Sidebar navigation** with active-section highlighting and smooth scroll
- **Responsive** layout for desktop and mobile
- **Architecture diagrams** — ASCII layer + functional-flow views rendered in monospace

## Content

1. **Project Summary** — what AgentFlow is, design principles, tech stack, current status
2. **Install Guide** — per-OS setup (Windows, Linux, macOS), build, verify, troubleshooting
3. **Architecture** — vertical layer diagram and functional flow for one user turn
4. **Quick Start** — run the minimal example, connect a real model
5. **Configuration Guide** — YAML philosophy, strict parsing, file skeleton
6. **Full Config Reference** — every config section: `runtime` (incl. `identity`/`credentials`), `models` (incl. `gemini`, `rerank`, `server_tools`), `memory.backends`, `profiles` (memory/safety/shell/agent, incl. rolling-window budgets), `gateway` (one shared listener + `public_url`; webhook/`ghhook`/telegram polling·webhook·auto; per-channel `media` ingest policy), `mcp.servers`, `tools.policy`, `agents`, `plugins`
7. **Lua Plugin Development** — comprehensive guide to writing loops, support chunks, and using every built-in Lua API
8. **Hook Point Specs** — all hook tables: gateway, session, loop, memory, shell, safety (core-owned), core lifecycle
9. **Lua Plugin Development** covers the full built-in Lua API reference (llm, session, agent, scheduler, memory/store, tools, shell, http/os, mail, log/json/time)

## Serve locally

```bash
cd docs
python -m http.server 8888
# open http://localhost:8888
```

Or any static file server (`npx serve`, `caddy file-server`, etc.). On GitHub, publish via Settings → Pages → "Deploy from a branch" → `docs/` folder.

## Files

```
docs/
├── index.html          # all content sections
├── css/styles.css      # light/dark theme variables + layout + diagram styles
├── js/theme.js         # dark/light toggle
└── js/nav.js           # sidebar active-link + smooth scroll
```
