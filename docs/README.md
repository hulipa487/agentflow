# AgentFlow Documentation Site

A static documentation website for AgentFlow, built with plain HTML/CSS/JS — no build step required.

## Features

- **Light/dark mode** toggle (persists to localStorage, defaults to dark)
- **Sidebar navigation** with active-section highlighting and smooth scroll
- **Responsive** layout for desktop and mobile
- **Architecture diagrams** — ASCII layer + functional-flow views rendered in monospace

## Content

1. **Project Summary** — what AgentFlow is, design principles, tech stack, current status
2. **Architecture** — vertical layer diagram (channels → gateway/safety → supervisor → actor → capabilities/drivers → egress) and a functional flow for one user turn
3. **Quick Start** — build and run commands, connecting a real model
4. **Configuration Guide** — YAML philosophy, strict parsing, file skeleton
5. **Full Config Reference** — every config section: `runtime`, `models`, `memory.backends`, `profiles` (memory/safety/shell/agent), `gateway.channels`, `mcp.servers`, `tools.policy`, `agents`, `plugins`
6. **Lua Plugin Design** — the contract, writing loops, support chunks, shadowing, restrictions
7. **Hook Point Specs** — all hook tables: gateway, session, loop, memory, shell, safety (core-owned), core lifecycle
8. **Capability API** — every Lua-facing table: `llm`, `session`, `agent`, `scheduler`, `memory`/`store`, `tools`, `shell`, core utilities

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
