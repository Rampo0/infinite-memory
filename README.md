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
make run       # daemon in foreground (or install via launchd — section below)
make hooks-json  # prints the snippet to merge into ~/.claude/settings.json
```

Hooks are registered as additional array entries in `~/.claude/settings.json`
(`UserPromptSubmit` → retrieve, `Stop`/`SessionEnd` → extract). New Claude Code sessions
pick them up automatically.

## launchd (macOS): run the daemon at login

Install — the plist keeps the daemon alive and starts it at login:

```sh
pkill -f 'imem daemon' || true     # kill any manually started daemon first (port 7690 clash)
mkdir -p ~/Library/LaunchAgents
cp launchd/com.ammar.imemd.plist ~/Library/LaunchAgents/
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.ammar.imemd.plist
```

Restart — needed after every `make install` (launchd keeps running the old binary)
or after changing `~/.config/infinite-memory/config.json`:

```sh
launchctl kickstart -k gui/$(id -u)/com.ammar.imemd
```

Check / logs:

```sh
launchctl print gui/$(id -u)/com.ammar.imemd | head -20   # state, pid, last exit code
curl -s localhost:7690/healthz                             # {"ok":true,"memgraph":true}
tail -f ~/.local/state/infinite-memory/imemd.log
```

Uninstall:

```sh
launchctl bootout gui/$(id -u)/com.ammar.imemd
rm ~/Library/LaunchAgents/com.ammar.imemd.plist
```

Notes:
- Older macOS syntax (still works): `launchctl load|unload ~/Library/LaunchAgents/com.ammar.imemd.plist`.
- The plist has `KeepAlive.SuccessfulExit=false`: a crash restarts the daemon, but if
  port 7690 is already taken by a manual daemon it will retry-loop — always `pkill` the
  manual one before bootstrapping.
- launchd only manages the daemon. Memgraph runs separately via Docker: after a Docker
  restart run `docker compose up -d` in this repo (healthz shows `"memgraph":false`
  until you do; hooks stay silent/fail-open, nothing breaks).

## Backups

The graph lives in the Docker volume `infinite-memory_mg_lib`. Memgraph snapshots itself
every 5 min, but *into that same volume* — `docker compose down -v` or a Docker Desktop
reset takes the data and every copy of it at once. So the daemon also dumps the graph to
the host, outside Docker:

```sh
~/.local/state/infinite-memory/backups/imem-20260824-140000.cypherl.gz   # ~77 KB
```

Every 4 hours, keeping the 2 newest (`backup_interval_hours`, `backup_keep`,
`backup_dir`, `backup_enabled` in config). The artifact is `DUMP DATABASE` output —
plain Cypher, so it survives a `memgraph-mage:latest` version bump that a binary
snapshot might not. Two details worth knowing: the loop compares wall-clock times
instead of counting ticks, so a laptop that slept through the window backs up on wake;
and a dump carrying no nodes is never written, so a blank Memgraph cannot age out both
good backups (it does not dump *zero* statements — the schema is recreated at boot — so
the check is for node data, not for length).

```sh
imem backups                 # list: size, age, newest first
imem backup                  # dump now (also rotates)
```

Restore wipes the graph and replays the dump. It talks to Bolt directly, not via the
daemon — you need it precisely when the daemon is down. It refuses a file that is not an
imem dump, and without `--yes` it only prints what it would do:

```sh
imem restore ~/.local/state/infinite-memory/backups/imem-20260824-140000.cypherl.gz --yes
imem init                    # verify indexes + constraints came back
imem status                  # verify counts
```

Fallback if the binary is unavailable — wipe first, since the dump recreates the unique
constraints (`DROP GRAPH` is rejected in the default `IN_MEMORY_TRANSACTIONAL` mode):

```sh
printf 'MATCH (n) DETACH DELETE n;\nDROP ALL CONSTRAINTS;\nDROP ALL INDEXES;\n' \
  | docker compose exec -T memgraph mgconsole
gunzip -c ~/.local/state/infinite-memory/backups/imem-20260824-033013.cypherl.gz \
  | docker compose exec -T memgraph mgconsole
```

Memgraph's own `DUMP DATABASE` emits the NUL byte inside `Entity.key` raw, which its
parser then rejects — `imem backup` escapes control bytes to `\uXXXX` on the way out, so
the stored file is replayable by either route. `make itest` guards this by `EXPLAIN`-ing
every escaped statement.

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
  repos. Foreign memories carry a `from <project>` marker. Top matches are capped by
  `retrieve_k` (default 6, `-1` for no cap).
- **Standing rules**: `rule` memories (coding constitution — LOC limits, max args,
  per-repo patterns, code style, MR templates) are ALWAYS injected, current-project
  first, capped by `rules_k` (default 50) — independent of keyword match. The whole
  rule pool is fetched and prioritized in Go before the cap, so the cap keeps the
  *best* rules, not an arbitrary slice.
- **Extraction**: Stop events debounce 45s per session; the worker reads the transcript
  delta since the stored cursor, prompts `claude -p --model claude-haiku-4-5-20251001`
  with `--json-schema` structured output, and MERGEs results into the graph
  (content-hash dedup; same-title memories supersede older versions).
- **Recursion guards**: spawned claude runs with `--settings '{"disableAllHooks":true}'`
  and `INFINITE_MEMORY_INTERNAL=1`; every hook subcommand exits instantly when that env
  var is set. `--bare` is deliberately NOT used — it disables subscription OAuth.
- **Visible in the CLI**: the `UserPromptSubmit` hook also emits a `systemMessage`, so
  every prompt prints what memory actually did — one line per injected memory (kind,
  clipped title, age, score, `↖source-project` when foreign), rules collapsed to a
  count. The no-match and daemon-down cases print too, because silent memory and dead
  memory otherwise look identical:

  ```
  imem: 6 memories + 50 rules (12ms)
    [fact]       Recursion guard via env sentinel       7d ago    8.4
    [decision]   Field masks modeled as a domain enum   3h ago    7.1  ↖opening-account
    … 4 more · 50 standing rules

  imem: no matches — 0 memories, 50 rules (9ms)
  imem: daemon unreachable — memory off
  ```

  `hook_show_retrieved: false` restores the old silent behavior;
  `hook_summary_lines` (default 6, `-1` for no cap) caps the per-memory lines.
- **Fail-open everywhere**: daemon or Memgraph down → hooks exit 0 silently apart from
  that one warning line; the cursor model means missed extractions catch up on the
  next Stop.

## Ops

```sh
imem status                  # daemon + memgraph health, per-project counts
imem search "query"          # search memories (global, boosted for current project)
imem entities [--project] [--limit N]   # entities by mention count (global by default)
imem entity <name...>        # one entity: relations (verb/weight) + memories mentioning it
imem backups                 # list host-side graph dumps (see Backups)
imem backup                  # dump the graph now
tail -f ~/.local/state/infinite-memory/imemd.log
make cypher                  # mgconsole inside the container
make test                    # unit tests
make itest                   # integration tests (needs make up)
```

Config: `~/.config/infinite-memory/config.json` (see `config.example.json`); all fields
optional.

The three capped knobs share one convention:

| value | `retrieve_k` | `rules_k` | `hook_summary_lines` |
|---|---|---|---|
| `-1` (any negative) | no cap — every match | no cap — every rule | no cap — one line per memory |
| `0` / absent | compiled default (6) | **rules section off** | compiled default (6) |
| `n > 0` | top n | best n rules | first n lines, rest as "… N more" |

`rules_k` is the odd one out: it is the only section you can switch off, so `0` means
disabled there rather than "use the default".
