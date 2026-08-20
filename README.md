# infinite-memory

Persistent memory for Claude Code, backed by Memgraph. Every user prompt gets relevant
memories from past sessions injected as context; every assistant response gets memories
extracted in the background. No embeddings, no API key — extraction runs headless
`claude -p` on your existing subscription login; retrieval is keyword + graph traversal.

```
UserPromptSubmit ── imem hook user-prompt ── POST /v1/retrieve ─┐  fail-open, <50ms
Stop / SessionEnd ─ imem hook stop|session-end ─ POST /v1/extract ┤  fire-and-forget
                                                                 ▼
                                     imem daemon (127.0.0.1:7690)
                                       ├─ retrieve: tokenize → 3 Cypher reads → score → top-K
                                       └─ extract queue: 45s debounce → transcript delta →
                                          claude -p (haiku, hooks disabled, structured output)
                                                                 ▼
                                     Memgraph (bolt://127.0.0.1:7687, docker compose)
```

## Setup

```sh
make up        # memgraph + memgraph-lab (http://localhost:3000)
make install   # builds ~/.local/bin/imem
make init      # schema: 8 indexes, 4 constraints
make run       # daemon in foreground (or use launchd/com.ammar.imemd.plist)
make hooks-json  # prints the snippet to merge into ~/.claude/settings.json
```

Hooks are registered as additional array entries in `~/.claude/settings.json`
(`UserPromptSubmit` → retrieve, `Stop`/`SessionEnd` → extract). New Claude Code sessions
pick them up automatically.

## How it works

- **Memories** are `fact | decision | preference | rule | reference` nodes tagged with
  the project they came from (git root of the session cwd), linked to `Entity` nodes
  (`MENTIONS`) with co-occurrence `RELATED` edges. Extraction is told to always tag the
  business/domain topic ("personal amend request", "jago whitelist", "BCA API") as an
  entity — topics are the retrieval index.
- **Retrieval is GLOBAL, topic-based — not cwd-partitioned** (no embeddings): shared
  tokenizer on save + query; candidates from direct keyword hits, entity-name hits
  (2x weight), and 1-hop `RELATED` expansion across ALL projects; scored with recency
  decay (`2·e^(-age/14d)`), a `seen_count` bonus, and a same-project boost
  (`same_project_boost`, default 1.0) so local context wins ties without hiding other
  repos. Foreign memories carry a `from <project>` marker.
- **Standing rules**: `rule` memories (coding constitution — LOC limits, max args,
  per-repo patterns, code style, MR templates) are ALWAYS injected, current-project
  first, capped by `rules_k` (default 3) — independent of keyword match.
- **Extraction**: Stop events debounce 45s per session; the worker reads the transcript
  delta since the stored cursor, prompts `claude -p --model claude-haiku-4-5-20251001`
  with `--json-schema` structured output, and MERGEs results into the graph
  (content-hash dedup; same-title memories supersede older versions).
- **Recursion guards**: spawned claude runs with `--settings '{"disableAllHooks":true}'`
  and `INFINITE_MEMORY_INTERNAL=1`; every hook subcommand exits instantly when that env
  var is set. `--bare` is deliberately NOT used — it disables subscription OAuth.
- **Fail-open everywhere**: daemon or Memgraph down → hooks exit 0 silently; the cursor
  model means missed extractions catch up on the next Stop.

## Ops

```sh
imem status                  # daemon + memgraph health, per-project counts
imem search "query"          # search memories (global, boosted for current project)
imem entities [--project] [--limit N]   # entities by mention count (global by default)
imem entity <name...>        # one entity: relations (verb/weight) + memories mentioning it
tail -f ~/.local/state/infinite-memory/imemd.log
make cypher                  # mgconsole inside the container
make test                    # unit tests
make itest                   # integration tests (needs make up)
```

Config: `~/.config/infinite-memory/config.json` (see `config.example.json`); all fields
optional.
