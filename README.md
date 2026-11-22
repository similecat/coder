# `coder` — High-Speed Native Coding Agent & Execution CLI

`coder` is a lightweight, low-latency software engineering agent written in Go. It combines native in-process file/search tooling, a prefix-cache-friendly prompt engine, full GitHub Copilot instruction/skill compatibility alongside native `.coder` extensibility, and pluggable LLM backends spanning **GitHub Copilot SDK**, **local inference servers** (`Ollama`, `llama.cpp`, `vLLM`, `LM Studio`), **OpenAI / OpenRouter**, and **Anthropic**.

---

## Why `coder` is Fast

- **Single Native Go Binary**: Zero Node.js/Python runtime startup overhead; sub-millisecond CLI initialization.
- **Zero-PowerShell In-Process File & Search Tools**: `read_file`, `edit_file` (CRLF/LF-preserving surgical replacements), `write_file` (atomic writes), `list_dir`, `grep_search` (`rg` fast path with pure-Go concurrent fallback), and `load_skill` run directly inside the Go process without spawning shells.
- **Automatic Parallel Read Batching**: Consecutive read-only tool calls (`read_file`, `list_dir`, `grep_search`, `load_skill`, and `read_only` custom tools) execute concurrently in goroutines while preserving deterministic message order; mutating/shell tools serialize safely.
- **2-Stage Lazy Skill Loading**: Only compact `<available_skills>` names and one-line descriptions are placed in the system prompt; full `SKILL.md` specifications and companion helper listings are loaded on demand via `load_skill`.
- **KV-Cache-Stable System Prompt**: Static `CoreSystemInstructions` always occupy the exact prefix of every system prompt, maximizing prompt-cache hit rates across Copilot, Anthropic, OpenAI, and local `llama.cpp`/`vLLM`/`Ollama` KV caches.
- **Warm Persistent Copilot SDK Session**: When using `copilot`, `coder` keeps a warm SDK client alive across turns with heavy CLI bloat disabled (`EnableConfigDiscovery: false`, `SkipCustomInstructions: true`, `EnableSkills: false`, `SkipEmbeddingRetrieval: true`, `EnableFileHooks: false`, `EnableSessionStore: false`) and routes tool invocations directly into `coder`'s in-process Go tools.
- **Cached MSVC `vcvars64.bat` Toolchain Environment**: On Windows, Visual Studio x64 C++ build variables (`PATH`, `INCLUDE`, `LIB`) are resolved via `vswhere.exe` + `vcvars64.bat` once per process (`sync.Once`) so every subsequent `cl.exe` / `nmake` / `link` invocation starts with **0ms** environment setup overhead.
- **Direct Pure-Go HTTP/2 SSE Clients**: Local and cloud HTTP providers stream Server-Sent Events directly over persistent Go `net/http` connections without heavyweight SDK wrappers.

---

## Quick Start

### Build from Source

```powershell
go build -o coder.exe .\cmd\coder
```

### Core Commands

```powershell
# Sign in to GitHub Copilot once (opens browser OAuth flow, saves token to credential store)
.\coder.exe login

# Launch interactive streaming coding REPL in the current repository
.\coder.exe

# Run a single headless engineering task (auto-approves workspace edits by default; add --full-auto for commands)
.\coder.exe exec --full-auto "Fix failing edge cases in scheduler.py and run python -m unittest"

# Run against a local Ollama or llama.cpp model
.\coder.exe -p ollama -m qwen2.5-coder:14b exec --full-auto "Add unit tests for expr_engine.py"

# Inspect active config, detected C++/Python toolchains, providers, instructions, and skills
.\coder.exe doctor

# List and run the built-in C++20 & Python benchmark suite
.\coder.exe bench --list
.\coder.exe bench -v
```

---

## Multi-Provider Configuration

`coder` resolves configuration in ascending precedence order:
1. Built-in defaults
2. User config: `~/.coder/config.json` (`%USERPROFILE%\.coder\config.json` on Windows)
3. Workspace config: `<repo>/.coder/config.json`
4. Environment variables (`CODER_PROVIDER`, `CODER_MODEL`, `CODER_BASE_URL`, `CODER_API_KEY`, `CODER_APPROVAL_POLICY`, `CODER_COPILOT_COMPAT`, `CODER_MAX_STEPS`, `CODER_COMMAND_TIMEOUT_SEC`, `CODER_STREAM`)
5. CLI flags (`-p/--provider`, `-m/--model`, `--base-url`, `--api-key`, `--full-auto`, `--safe`, `--no-copilot-compat`, etc.)

### Example `~/.coder/config.json`

```json
{
  "default_provider": "copilot",
  "default_model": "claude-sonnet-4.5",
  "approval_policy": "smart_auto",
  "max_steps": 40,
  "max_output_bytes": 32768,
  "command_timeout_sec": 120,
  "streaming": false,
  "compatibility": {
    "load_copilot_instructions": true,
    "load_copilot_skills": true,
    "load_agents_md": true
  },
  "providers": {
    "copilot": {
      "type": "copilot_sdk",
      "default_model": "claude-sonnet-4.5",
      "warm_session": true
    },
    "ollama": {
      "type": "openai_compat",
      "base_url": "http://127.0.0.1:11434/v1",
      "default_model": "qwen2.5-coder:14b"
    },
    "local": {
      "type": "openai_compat",
      "base_url": "http://127.0.0.1:8080/v1",
      "default_model": "qwen2.5-coder-32b-instruct"
    },
    "openai": {
      "type": "openai_compat",
      "base_url": "https://api.openai.com/v1",
      "api_key_env": "OPENAI_API_KEY",
      "default_model": "gpt-4.1"
    },
    "openrouter": {
      "type": "openai_compat",
      "base_url": "https://openrouter.ai/api/v1",
      "api_key_env": "OPENROUTER_API_KEY",
      "default_model": "anthropic/claude-sonnet-4"
    },
    "anthropic": {
      "type": "anthropic",
      "base_url": "https://api.anthropic.com",
      "api_key_env": "ANTHROPIC_API_KEY",
      "default_model": "claude-sonnet-4-20250514"
    }
  }
}
```

### Provider Quick Reference

| Profile | Backend Type | Typical Target | Auth / Notes |
| --- | --- | --- | --- |
| `copilot` | `copilot_sdk` | GitHub Copilot CLI / SDK | Uses existing Copilot login (`copilot` -> `/login`) or `COPILOT_GITHUB_TOKEN` / `GH_TOKEN` |
| `ollama` | `openai_compat` | `http://127.0.0.1:11434/v1` | Local Ollama server (no API key needed) |
| `lmstudio` | `openai_compat` | `http://127.0.0.1:1234/v1` | Local LM Studio server with auto-model discovery; pass `--reasoning-effort none` to disable `<think>` overhead on hybrid models |
| `local` | `openai_compat` | `http://127.0.0.1:8080/v1` | `llama-server` (`llama.cpp`) or `vLLM` |
| `openai` | `openai_compat` | `https://api.openai.com/v1` | `OPENAI_API_KEY` |
| `openrouter` | `openai_compat` | `https://openrouter.ai/api/v1` | `OPENROUTER_API_KEY` |
| `anthropic` | `anthropic` | `https://api.anthropic.com` | `ANTHROPIC_API_KEY` (native `/v1/messages` SSE with `cache_control` prompt caching) |

---

## GitHub Copilot Compatibility & Native `.coder` Extensibility

By default, `coder` seamlessly loads existing GitHub Copilot instructions, skills, and `AGENTS.md` files alongside its own `.coder/` configuration—and lets you disable any compatibility layer cleanly when you want strict `.coder`-only isolation.

### Compatibility Toggles

In `config.json`:
```json
"compatibility": {
  "load_copilot_instructions": true,
  "load_copilot_skills": true,
  "load_agents_md": true
}
```
Or per invocation via CLI / environment:
- `--no-copilot-compat` or `--copilot-compat=false` (disables Copilot instructions & skills)
- `CODER_COPILOT_COMPAT=false`, `CODER_LOAD_COPILOT_INSTRUCTIONS=false`, `CODER_LOAD_COPILOT_SKILLS=false`, `CODER_LOAD_AGENTS_MD=false`

### Supported Instruction, Rule & Skill Locations

| Ecosystem | Instructions & Scoped Rules | On-Demand Skills (`SKILL.md`) |
| --- | --- | --- |
| **Native `.coder` (Repo)** | `.coder/instructions.md`<br>`.coder/rules/*.md` | `.coder/skills/<name>/SKILL.md` |
| **Native `.coder` (User)** | `~/.coder/instructions.md`<br>`~/.coder/rules/*.md` | `~/.coder/skills/<name>/SKILL.md` |
| **GitHub Copilot (Repo)** | `.github/copilot-instructions.md`<br>`.github/instructions/*.instructions.md` | `.github/skills/<name>/SKILL.md`<br>`.agents/skills/<name>/SKILL.md` |
| **GitHub Copilot (User)** | `~/.copilot/copilot-instructions.md`<br>`~/.copilot/instructions/*.instructions.md` | `~/.copilot/skills/<name>/SKILL.md`<br>`~/.copilot/installed-plugins/**/skills/<name>/SKILL.md` |
| **Agent Markdown (Repo)** | `AGENTS.md`, `CLAUDE.md` | — |

On skill name collisions, native `.coder` skills take precedence over Copilot skills (`workspace .coder` > `user ~/.coder` > `workspace .github/.agents` > `user ~/.copilot`).

### Path-Scoped Rules (`applyTo` Frontmatter)

Both `.coder/rules/*.md` and `.github/instructions/*.instructions.md` support YAML frontmatter with comma-separated glob patterns matched against workspace files:

```markdown
---
description: Modern C++20 safety invariants
applyTo: "**/*.hpp, **/*.cpp"
---
- Compile cleanly under `/std:c++20 /W4` (MSVC) and `-std=c++20 -Wall -Wextra` (GCC/Clang).
- Use `int64_t` for accumulated graph weights and timestamps to prevent 32-bit signed overflow.
```

### Custom Declarative Tools (`.coder/tools/*.json`)

Drop JSON tool specifications into `<repo>/.coder/tools/*.json` or `~/.coder/tools/*.json` to expose project-specific scripts or binaries as first-class JSON Schema tools. Arguments are passed via `CODER_TOOL_ARGS` (JSON), `stdin`, and safe `{{argName}}` argv substitution:

```json
{
  "name": "run_ctest_filter",
  "description": "Run CTest cases matching a regex filter inside build/",
  "read_only": true,
  "timeout_sec": 60,
  "working_dir": "build",
  "command": "ctest",
  "args": ["--output-on-failure", "-R", "{{pattern}}"],
  "parameters": {
    "type": "object",
    "properties": {
      "pattern": {
        "type": "string",
        "description": "CTest regex filter"
      }
    },
    "required": ["pattern"]
  }
}
```

---

## Approval Policies & Interactive REPL

### Approval Modes

| Policy | Reads / Search / `load_skill` | Workspace File Edits | Shell Commands & External Paths |
| --- | --- | --- | --- |
| `smart_auto` *(default)* | Auto-approved | Auto-approved inside workspace | Prompts for confirmation |
| `full_auto` (`--full-auto`) | Auto-approved | Auto-approved | Auto-approved |
| `strict_ask` (`--safe`) | Auto-approved | Prompts for confirmation | Prompts for confirmation |

### REPL Slash Commands

| Command | Description |
| --- | --- |
| `/help` | Show available slash commands and keybindings |
| `/login [--host <url>]` | Sign in to GitHub Copilot via browser/device OAuth and reload session |
| `/provider [name] [model]` | Show active backend or switch provider (e.g. `/provider lmstudio`) |
| `/model [id]` | Show or switch the active model ID in place |
| `/mode [smart_auto\|full_auto\|strict_ask]` | View or change tool approval policy |
| `/skills` | List discovered `.coder` and Copilot skills |
| `/instructions` | List active global and `applyTo`-matched instruction files |
| `/compact` | Trim bulky older tool outputs in conversation history |
| `/clear` | Reset conversation context while keeping provider client warm |
| `/exit` / `/quit` | Exit the REPL cleanly |

---

## Automated Coding Benchmark Suite (`coder bench`)

`coder` ships with an isolated, tamper-proof coding benchmark runner (`internal/bench` + `testdata/problems`) covering C++20 data structures, graph bugfixing, safe expression parsing, concurrent DAG scheduling, and GitHub Copilot instruction/skill compatibility.

### How Isolation & Host Verification Work
1. Each problem's starter `workspace/` (including hidden `.github/` instructions and skills) is copied into a fresh temporary directory (`coder-bench-<id>-*`).
2. `coder` runs autonomously (`full_auto`) inside that sandbox—it never sees or has write access to `testdata/problems/<id>/verify/`.
3. After the agent finishes and closes its session, the host verifier compiles/runs the untouched `verify/` suite in a separate temporary verification directory (`MSVC cl.exe /std:c++20 /O2 /W4` or `g++` for C++20; `python -B` with `WORKSPACE_DIR` for Python) and emits an ASCII scorecard or JSON report.

### Included Benchmark Problems

| Problem ID | Lang | Focus |
| --- | --- | --- |
| `cpp_lru_ttl` | `cpp` | Header-only C++20 `TTLLRUCache<K, V>` with O(1) lookup, lazy/active TTL eviction before live LRU, and 50k-op `<500ms` scale check |
| `cpp_graph_bugfix` | `cpp` | Fix 32-bit cost overflow, stale PQ entries, hop-count + lexicographic path tie-breaking, and 0-weight cycles in `GraphRouter` |
| `py_expr_engine` | `python` | Safe `eval`/`exec`-free expression & lazy formula engine with right-associative `**`, diamond dependencies, and cycle recovery |
| `py_dag_scheduler_bugfix` | `python` | Fix transitive multi-level failure `SKIPPED` propagation, retry filtering, downstream cycle detection, and priority tie-breaking |
| `py_copilot_skill_compat` | `python` | Implement binary `telemetry.v2` packet codec + CRC-16/CCITT-FALSE by discovering `.github/copilot-instructions.md` and loading `.github/skills/telemetry-codec/SKILL.md` |

### Running Benchmarks

```powershell
# List all problems
.\coder.exe bench --list

# Run the entire suite with live streaming progress
.\coder.exe bench -v

# Run only C++20 problems on a specific provider/model
.\coder.exe bench --lang cpp -p copilot -m claude-sonnet-4.5 -v

# Run specific problems and emit machine-readable JSON
.\coder.exe bench --problems cpp_lru_ttl,py_copilot_skill_compat --json
```
