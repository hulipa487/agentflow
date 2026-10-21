<div align="center">
<h1>
🤖 AgentFlow
<p>
</h1>
<i> Configure the ordinary, code the exceptionals. <p></p></i>
<img src="https://img.shields.io/github/v/release/hulipa487/agentflow">
<img src="https://img.shields.io/github/stars/hulipa487/agentflow">
<img src="https://img.shields.io/github/forks/hulipa487/agentflow"><p></p>
<img width="1444" height="691" alt="image" src="https://github.com/user-attachments/assets/0817ad4c-da1e-4502-9aaf-3bf15dca0394" />
</div>


## What is AgentFlow

> TL;DR - A LLM Runtime with Lua programmable, hot reloadable loops

AgentFlow is a open-source, single-binary Go runtime for LLM agents.

Configure all the routines - API keys, LLM Upstreams, Tools, with simple YAML while you enjoy the fun of coding agentic loops in simple Lua, and without sacrificing controls too! You name it, we've got it. 
- [x] Direct r/w control over vectordb?
- [x] User prompts accesible to Lua?
- [x] Custom ReAct loops?
- [ ] Flying pigs? (working on it)

Interact with your agents with Telegram and Webhooks, or through our first-class desktop tool agentflow-studio.

## Getting Started

Run the following auto-install script.

```bash
curl -fsSL agentflow.hulipa.my | sh
```

OR

Build from binary:

```bash
git clone --recurse-submodules https://github.com/hulipa487/agentflow.git
cd agentflow
make
./agentflow -config agentflow.example.yaml
```

## Why exactly AgentFlow?
- **Full Transparency**: All model parameters are within your control.
- **Ease of Development**: High level Lua language with unified calls.
- **Cross Platform**: Windows, MacOS, Linux, we've got you covered.


## Contributors
<a href="https://github.com/hulipa487/agentflow/graphs/contributors">
  <img src="https://contrib.rocks/image?repo=hulipa487/agentflow" />
</a>

**LICENSE**: MIT

Made with Vibe and ❤️ by hulipa and her team.