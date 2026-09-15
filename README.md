# infinite-memory

Persistent memory for Claude Code, backed by Memgraph. Every user prompt gets relevant
memories from past sessions injected as context; every assistant response gets memories
extracted in the background. No embeddings, no API key — extraction runs headless
`claude -p` on your existing subscription login; retrieval is keyword + graph traversal.

```
UserPromptSubmit ── imem hook user-prompt ── POST /v1/retrieve ─┐  fail-open, <50ms
Stop ───────────── imem hook stop ───────────── POST /v1/flush ─┤  blocking, budget-capped
SessionEnd ─────── imem hook session-end ────── POST /v1/extract ┤  fire-and-forget
                                                                 ▼
                                     imem daemon (127.0.0.1:7690)
                                       ├─ retrieve: tokenize → 3 Cypher reads → score → top-K
                                       ├─ extract queue: 45s debounce → transcript delta →
                                       │  claude -p (haiku, hooks disabled, structured output)
                                       └─ save log: what each run wrote, drained once by
                                          whichever hook reports it first
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
- **Extraction**: the worker reads the transcript delta since the stored cursor,
  prompts `claude -p --model claude-haiku-4-5-20251001` with `--json-schema` structured
  output, and MERGEs results into the graph (content-hash dedup; same-title memories
  supersede older versions). By default the Stop hook **blocks** on `/v1/flush` so it
  can print what the turn actually saved; a delta under 200 chars skips the spawn and
  returns in milliseconds, so only substantive turns pay. Past `stop_flush_budget_ms`
  (default 90s) the daemon answers `running`, the extraction finishes in the background,
  and its report prints at the next prompt. `hook_flush_on_stop: false` restores the old
  fire-and-forget behaviour, where `SessionEnd` and the 45s debounce do the saving.
- **Recursion guards**: spawned claude runs with `--settings '{"disableAllHooks":true}'`
  and `INFINITE_MEMORY_INTERNAL=1`; every hook subcommand exits instantly when that env
  var is set. `--bare` is deliberately NOT used — it disables subscription OAuth.
- **Visible in the CLI, both directions**: the hooks emit a `systemMessage`, so every
  prompt prints what memory *read* and every turn prints what it *wrote*. The no-match,
  daemon-down and extraction-failed cases print too, because silent memory and dead
  memory otherwise look identical — this project shipped two weeks of completely failed
  extraction before the save side was visible.

  Retrieve, at `UserPromptSubmit` — one line per injected memory (kind, clipped title,
  age, score, `↖source-project` when foreign), rules collapsed to a count:

  ```
  imem: 6 memories + 50 rules (12ms)
    [fact]       Recursion guard via env sentinel       7d ago    8.4
    [decision]   Field masks modeled as a domain enum   3h ago    7.1  ↖opening-account
    … 4 more · 50 standing rules

  imem: no matches — 0 memories, 50 rules (9ms)
  imem: daemon unreachable — memory off
  ```

  Save, at `Stop` — same two columns, with new/seen-again where retrieve shows age and
  score. `seen 4x` means the content hash already existed and only `seen_count` moved:

  ```
  imem: saved 5 memories (32.7s)
    [fact]       account repo migrated to urfave/cli v3            new
    [reference]  Single-binary CLI verification checklist for ac…  new
    [rule]       Formatter mirrors FormatSummary columns           seen 4x

  imem: saved nothing — no new facts this turn
  imem: extracting… (report at next prompt)
  imem: save failed — claude: claude binary not found (tried "claude", …)
  ```

  A report is drained once, by whichever hook reports it first, so nothing prints twice.
  `hook_show_retrieved: false` / `hook_show_saved: false` restore the old silent
  behavior; `hook_summary_lines` and `hook_saved_lines` (default 6, `-1` for no cap) cap
  the per-memory lines. The two `*_lines` caps are read daemon-side, so changing them
  needs a daemon restart; the two `*_show_*` switches are read by the hook binary and
  take effect immediately.
- **Fail-open everywhere**: daemon or Memgraph down → hooks exit 0 silently apart from
  that one warning line; the cursor model means missed extractions catch up on the
  next Stop. If the blocking flush cannot reach the daemon at all, the Stop hook falls
  back to the fire-and-forget `/v1/extract` call, so the turn is still saved — just
  later, and without a line.

## Ops

```sh
imem status                  # daemon + memgraph health, per-project counts,
                             # and recent extraction runs (including failures)
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

| value | `retrieve_k` | `rules_k` | `hook_summary_lines` | `hook_saved_lines` |
|---|---|---|---|---|
| `-1` (any negative) | no cap — every match | no cap — every rule | no cap — one line per memory | no cap — one line per memory |
| `0` / absent | compiled default (6) | **rules section off** | compiled default (6) | compiled default (6) |
| `n > 0` | top n | best n rules | first n lines, rest as "… N more" | first n lines, rest as "… N more" |

`rules_k` is the odd one out: it is the only section you can switch off, so `0` means
disabled there rather than "use the default".

## LLM query expansion (off by default)

The tokenizer drops generic dev vocabulary — `issue`, `problem`, `error`, `fix`, `use`,
`file`, `code` are all stopwords — so a prompt like `solve this issue` tokenizes to
`["solve"]` and matches almost nothing. Expansion asks headless `claude` for extra search
terms in the index's own vocabulary, then appends them to the token list. Nothing else
changes: the Cypher, the scoring and the graph schema are untouched.

```json
{ "expand_enabled": true, "expand_model": "claude-haiku-4-5-20251001", "expand_budget_ms": 30000 }
```

| key | default | meaning |
|---|---|---|
| `expand_enabled` | `false` | the kill switch. Note the polarity: every other bool here defaults `true`, this one defaults `false`, because expansion costs real seconds on every prompt. |
| `expand_model` | `claude-haiku-4-5-20251001` | deliberately separate from `extract_model` — extraction runs a big model off the critical path, expansion runs a small one while you wait. |
| `expand_budget_ms` | `30000` | spent *before* the `retrieve_timeout_ms` graph budget, never inside it. On expiry you get exactly the unexpanded result. |

Nothing else is tunable. How many terms to return is the model's decision; the structural
bounds live in the JSON schema the CLI enforces.

**The cost is real and it is not small.** Measured on this machine with `claude-haiku-4-5`,
the isolated spawn (`--safe-mode --strict-mcp-config --setting-sources "" --tools ""
--effort low`, which removes a ~30K-token preamble):

| | |
|---|---|
| fastest observed | 6.5s |
| typical | 14-19s |
| slowest observed | 43s |
| cost per prompt | ~$0.01 |

Almost all of it is thinking tokens (1,000-5,000 per call), and the spread is wide and
unpredictable — the same prompt can take 7s or time out. Claude Code renders nothing while
a `UserPromptSubmit` hook runs, so that shows up as a hang. Try it on the CLI first:

```
imem expand "solve this issue"       # what would be added, without searching
```

`imem expand` ignores `expand_enabled` on purpose, so you can judge expansion before you
turn it on.

**Set `retrieve_k` to a real number before enabling.** With `retrieve_k: -1` a broad
expansion injects every match into the context block; 30 generic tokens match 442 of 607
memories in this graph, and the hook's injection has already been silently truncated at
102.9KB once.
