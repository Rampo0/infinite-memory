# infinite-memory

Persistent memory for Claude Code, backed by Memgraph. Every session and every subagent
starts with the standing rules (pinned core rules first); every prompt gets the past
memories that match it — a vague follow-up ("lanjut", "fix that") is matched with the
last exchange — plus a protocol line telling the model to call `imem_search` with its own
keywords before answering; turns get new memories extracted, reconciled against what is
already known. Headless `claude -p` sessions and the bots in `~/scratch` get the same
loop at agent trust (rules demoted to facts, never grading your memories). No embeddings,
no API key: extraction runs headless `claude -p` on your subscription login; retrieval is
idf-weighted keyword + entity matching over the graph, and runs in milliseconds.

```
SessionStart ───── imem hook session-start ─ POST /v1/session-start ┐  standing rules, once
SubagentStart ──── imem hook subagent-start POST /v1/session-start ┤  same rules, per subagent
UserPromptSubmit ── imem hook user-prompt ── POST /v1/retrieve ─────┤  ≤6K chars, ~50ms, + protocol
Stop ───────────── imem hook stop ───────── POST /v1/flush ─────────┤  blocking (headless: /v1/extract)
SessionEnd ─────── imem hook session-end ── POST /v1/extract ───────┤  fire-and-forget
imem mcp (stdio) ─ imem_search / imem_remember ── /v1/memories, /v1/remember
                                                                     ▼
                                     imem daemon (127.0.0.1:7690)
                                       ├─ retrieve: tokenize → 3 Cypher reads → idf score
                                       │  → relevance floor → char budget → per-session dedup
                                       ├─ extract queue: transcript delta (+ tool summaries)
                                       │  + similar existing memories + what was shown →
                                       │  claude -p → add / update / noop + feedback
                                       └─ save log: what each run wrote, drained once by
                                          whichever hook reports it first
                                                                     ▼
                                     Memgraph (bolt://127.0.0.1:7687, docker compose)
```

## Setup

```sh
make up        # memgraph + memgraph-lab (http://localhost:3000)
make install   # builds ~/.local/bin/imem (atomically: build to .tmp, then rename)
make init      # schema: indexes + constraints
make run       # daemon in foreground (or install via launchd — section below)
make hooks-json  # prints the snippet to merge into ~/.claude/settings.json
claude mcp add --scope user imem -- ~/.local/bin/imem mcp   # imem_search / imem_remember
```

Hooks are registered as additional array entries in `~/.claude/settings.json`
(`SessionStart` → standing rules, `UserPromptSubmit` → retrieve, `Stop`/`SessionEnd` →
extract). New Claude Code sessions pick them up automatically. Allowlisting
`mcp__imem__imem_search` is safe (read-only); leave `imem_remember` behind its
permission prompt — a model that read a malicious file could otherwise plant a rule.

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
  the project they came from, linked to `Entity` nodes (`MENTIONS`) with co-occurrence
  `RELATED` edges. Extraction is told to always tag the business/domain topic
  ("personal amend request", "jago whitelist", "BCA API") as an entity — topics are the
  retrieval index. Each memory also carries 3-10 **aliases**: the words a future prompt
  would use for it that its text does not contain — Indonesian terms, synonyms, joined
  and split identifiers (`bca rdn` ↔ `bcardn`) — indexed as keywords. This is query
  expansion done once per memory at save time instead of once per prompt.
- **Project = repository.** A session's project is the git root of its cwd, and a linked
  worktree (`.superset/worktrees/<id>/<branch>`) resolves to its main repository, so
  every checkout of a repo shares one project. Older worktree-keyed memories carry a
  `repo_key` (see Maintenance).
- **Retrieval is GLOBAL, topic-based**: shared tokenizer on save + query (English and
  Indonesian stopwords, 32 prompt tokens); candidates from keyword hits, entity hits and
  1-hop `RELATED` neighbours across ALL projects. Scoring:
  - every matched term weighs its BM25 **idf** — `go` (in 550 memories) counts ~1.3, a
    term in one memory ~7 — so common words stop drowning rare ones;
  - an exact multi-part entity name (`opening-account`, `imem.save`) counts double its
    own idf; a single-word entity or a term that only hit a token of a longer name counts
    once, like a keyword;
  - `RELATED` neighbours only re-rank direct matches (≤ +1.0) and only over edges seen at
    least `related_min_weight` times — on their own they once pulled 1,020 memories into
    one prompt;
  - the **relevance floor** `min_match` drops weak matches before any boost is added;
  - boosts: recency `2·e^(-age/14d)` (age from last seen *or last used*), `seen_count`,
    `used_count`, a penalty per dispute, and `same_project_boost`.
- **Injection budget.** Claude Code inlines `additionalContext` only up to 10,000 chars
  and shows a 2KB preview of anything larger, so the block is capped at
  `retrieve_max_chars` (6,000), best-scored first, ending with "… N more matched —
  imem_search finds more". Memories already injected earlier in the session are skipped
  (they are still in context); `/clear` and compaction reset that.
- **Standing rules arrive once, at SessionStart** (also after `/clear`, resume and
  compaction): this project's rules — a parent workspace counts as this project — then
  its preferences, then other projects' rules and preferences, within `rules_max_chars`.
  Per prompt, rules still surface like any memory when their topic matches.
- **Extraction**: the worker reads the transcript from the stored cursor — the text of each
  turn, the final report of every Agent/Task subagent, prompts typed while Claude was busy,
  slash-command arguments, plus one-line tool summaries (`Edit <path>`, `Bash: <description>`;
  never a command, which may carry secrets) — oldest first, up to `max_transcript_chars`, and
  prompts `claude -p` (`extract_model`, `extract_effort`) with `--json-schema`. Messages over
  6 KB (8 KB for agent reports) keep their head and tail. A longer delta is extracted in
  chunks: the cursor stops after the last turn read and the rest is queued at once. Every
  spawn is isolated (`--safe-mode --setting-sources "" --tools ""`): no CLAUDE.md, rules,
  plugins or MCP servers, and no inherited effort. Two more things go into the prompt for
  your own sessions:
  - **reconcile** — the 12 existing memories most like the excerpt; each new memory says
    `add`, `update` (it replaces that memory, which is superseded) or `noop` (that
    memory already says it; it is re-observed, `seen_count` grows);
  - **feedback** — the memories the session was shown since the last extraction; the
    extractor grades the ones the conversation gives evidence about as `used`, `wrong`
    or `outdated`. `used` raises `used_count` and freshness; disputes lower the score.
    Retiring a memory still takes an `update` naming its replacement.

  Targets and grades are whitelisted to the ids the model was shown. By default the Stop
  hook first waits (up to 5s) until Claude Code has written the turn's final reply
  (`last_assistant_message`) to the transcript, then **blocks** on `/v1/flush` so it can
  print what the turn saved. A delta under 200 chars waits, cursor unmoved, for the next
  turn; a session end or the sweep extracts it anyway. Past `stop_flush_budget_ms`
  (default 90s) the daemon answers `running` and its report prints at the next prompt.
- **Nothing is dropped**: a failed extraction keeps the cursor and retries a chunk half the
  size; after 3 failures the session pauses (30 min, doubling up to 24h) and the hook says
  so. Above `extract_max_usage` (default 0.85 of the 5-hour window, probed every 10 min)
  extraction is deferred, not skipped. Every 10 minutes the daemon **sweeps** sessions seen
  in the last 7 days whose transcript has lines past the cursor and has been idle for 3
  minutes, and extracts them — a missed Stop, a daemon restart or a session that never
  sent SessionEnd still gets saved.
- **Agent transcripts**: a transcript outside `~/.claude/projects` is only accepted from
  a configured `agent_roots` directory (symlinks resolved), is extracted in isolated mode
  (no MCP servers, settings or tools), never saves a `rule` or `preference` (those become
  `fact`), and stays add-only: no reconcile, no feedback — a bot's text may add facts,
  never retire, reinforce or grade yours.
- **Ignored directories** (`ignore_cwds`, default `~/.claude/double-shot-latte`): sessions
  there get no retrieve and no extraction. double-shot-latte's continuation judge runs
  `claude -p` there with a copy of the conversation; it was 38% of all retrieves.
  Background-task notifications are not retrieved for either.
- **Recursion guards**: spawned claude runs with `--settings '{"disableAllHooks":true}'`,
  `--strict-mcp-config` and `INFINITE_MEMORY_INTERNAL=1`; every hook subcommand and
  `imem mcp` exit instantly when that env var is set. `--bare` is deliberately NOT used —
  it disables subscription OAuth.
- **Visible in the CLI, both directions**: the hooks emit a `systemMessage`, so every
  prompt prints what memory *read* and every turn prints what it *wrote*. The no-match,
  daemon-down and extraction-failed cases print too, because silent memory and dead
  memory otherwise look identical — this project shipped two weeks of completely failed
  extraction before the save side was visible.

  Standing rules, at `SessionStart`:

  ```
  imem: 18 standing rules, 3 preferences (17ms) · 29 more over budget
  ```

  Retrieve, at `UserPromptSubmit` — one line per injected memory (kind, clipped title,
  age, score, `↖source-project` when foreign), then what the budget left out:

  ```
  imem: 6 memories + 0 rules (12ms)
    [fact]       Recursion guard via env sentinel       7d ago    8.4
    [decision]   Field masks modeled as a domain enum   3h ago    7.1  ↖opening-account
    … 4 more
    3 more matched, over the 6000-char budget

  imem: no matches — 0 memories, 0 rules (9ms)
  imem: daemon unreachable — memory off
  ```

  Save, at `Stop` — same two columns, with new/seen-again where retrieve shows age and
  score. `seen 4x` means the content hash already existed and only `seen_count` moved:

  ```
  imem: saved 5 memories (32.7s)
    [fact]       account repo migrated to urfave/cli v3            new
    [decision]   Expansion moved to index time                     replaces older
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

## Pull: the MCP tools

`imem mcp` is a stdio MCP server (stdlib JSON-RPC; registered at user scope). The session
model writes its own queries with the whole conversation as context — the job the old
per-prompt LLM expander did in tens of seconds, now free. Every prompt's injection ends with
an `<imem-protocol>` line (also when nothing matched or the daemon is down) asking the model
to call `imem_search` at least once per turn with keywords it derives itself; the MCP
instructions and tool description say the same. Searches carry `CLAUDE_CODE_SESSION_ID`, so
their results join the session's injection log: later prompts don't repeat them, and the
extractor grades them used / wrong like injected ones.

`imem mcp --agent --cwd <project>` is the bots' read-only variant: `imem_search` only, results
prefixed as untrusted reference data, scoped to `--cwd`, never recorded against a session.

| tool | does |
|---|---|
| `imem_search(query, limit≤20)` | `/v1/memories` (same ranking, no floor): one line per memory with kind, title, content, age and source repo |
| `imem_remember(title, content, kind, entities?)` | `/v1/remember` → `SaveBatch` under the session's project, in a per-day `mcp-` session (dedup and same-title supersede apply) |

The hook block's "… N more matched — imem_search finds more" line points the model at it.

## Ops

```sh
imem status                  # daemon + memgraph health, per-project counts,
                             # and recent extraction runs (including failures)
imem search "query"          # search memories (global, boosted for current project)
imem rules [--cwd p] [--limit N]   # every live rule, this project first, one per line
imem entities [--project] [--limit N]   # entities by mention count (global by default)
imem entity <name...>        # one entity: relations (verb/weight) + memories mentioning it
imem eval [--mode hook|search] [--json]   # score retrieval on labelled cases (Evaluation)
imem backups                 # list host-side graph dumps (see Backups)
imem backup                  # dump the graph now
tail -f ~/.local/state/infinite-memory/imemd.log
make cypher                  # mgconsole inside the container
make test                    # unit tests (incl. the agent contract tests)
make itest                   # integration tests on a fresh throwaway Memgraph (7688)
make itest-down              # stop that test instance
```

`make itest` never touches the live graph: it recreates the `memgraph-test` compose
service (127.0.0.1:7688, no volume) and the tests refuse `memgraph_uri` unless
`IMEM_TEST_ALLOW_LIVE=1`.

## Maintenance

All of these are dry runs until told otherwise, and none deletes anything.

```sh
imem backfill-aliases [--limit N] [--batch 10] [--workers 6] [--max-usage 0.5] [--yes]
imem migrate-repo-keys [--map old=new]... [--yes]
imem consolidate [--plan | --apply] [--limit N] [--min-jaccard 0.4] [--max-usage 0.5]
imem consolidate --archive [--apply]
imem reindex [--yes]          # recompute keywords / alias-only tokens with today's tokenizer
imem quota [--resets]         # 5-hour / 7-day subscription usage, via one tiny probe
```

**Quota.** Bulk jobs spawn `claude -p` on the same subscription the ai-review and on-call
agents use, and those agents pause their queues at 60% of the 5-hour window. Run bulk jobs
with `--max-usage 0.5`: they probe usage at most once a minute, stop before the next spawn
once the cap is reached (exit code 3; the rest stays pending), and stop if the usage cannot
be read.

**Alias-only tokens.** Each memory also stores `alias_only`: the alias tokens its own text
lacks. Retrieval weighs those at half their idf — aliases are the extractor's guesses, and
while few memories have them their words look artificially rare (a generic "wajib" on one
memory once outranked a memory whose own title said "invalid parameter").

- **backfill-aliases** gives memories saved before aliases existed (`aliases IS NULL`)
  their aliases: one isolated `expand_model` spawn per batch, batches partitioned across
  workers, resumable (a memory the model skipped gets an empty list and is done).
- **migrate-repo-keys** sets `repo_key` on memories saved under a worktree path: from the
  worktree itself when it still exists, else from a live sibling under the same
  `.superset/worktrees/<id>/`, else from a repository named like `<id>`, else `--map`.
  Additive: `project_key` and hashes stay; undo is `REMOVE m.repo_key`.
- **consolidate** clusters near-duplicates (a shared entity and keyword Jaccard ≥ 0.4),
  asks `extract_model` which really say the same thing (`--plan` prints its merges),
  and with `--apply` saves one canonical memory per merge and supersedes the members.
  A merge never escalates a kind (facts never become a rule). `--archive` lists memories
  injected 20+ times, never used, unseen for 60+ days; `--apply` sets `archived` (out of
  retrieval; undo `REMOVE m.archived`). `consolidate_enabled` runs the merge daily from
  the daemon — off by default; review a `--plan` first.

## Evaluation

`imem eval` runs the labelled cases in `~/.config/infinite-memory/eval.jsonl` (one JSON
per line: `name`, `mode`, `prompt`, `cwd`, `expect` = title substrings) and reports hit
rate, MRR, recall, block size and latency per mode:

- `hook` scores what the model can actually SEE — the whole block up to 10,000 chars, the
  2KB preview beyond that — through the side-effect-free `POST /v1/retrieve/preview`;
- `search` scores `imem search`'s top 10, which is what the ai-review and on-call agents
  read.

`--json` prints everything for diffing a baseline. On 24 cases (12 interactive prompts, 12
real agent queries) the move from live LLM expansion to idf scoring went: hook hit 58% →
92% (MRR 0.38 → 0.66, block 719KB → 5KB, p50 41s → 26ms); search hit 75% → 92% (MRR 0.37 →
0.57, p50 40s → 24ms).

## Agent contract (ai-review, on-call)

The bots in `~/scratch` use imem from deterministic code and parse some of its output.
`cmd/imem/contract_test.go` and `internal/daemon/server_test.go` pin each of these; change
the agents before changing any of them:

| caller | uses | contract |
|---|---|---|
| on-call `preflight.imem_status`, `doctor` | `imem status` | exit 0, first line has `daemon: up` and `memgraph: true` (gates every full run) |
| `agentkit.imem.entities` / `entity_matches` | `imem entities --limit 500` | lines match `\s*(\d+)x\s+(.+?)\s{2,}\[` |
| on-call prep, ai-review `run_imem` | `imem entity "<name>"`, `imem search "<q>" --cwd ~/accountworkspace` | flags and line format unchanged, search capped at 10, no relevance floor |
| ai-review `prep.newest_rules` | `imem rules --cwd ~/accountworkspace` | bare `- [rule] …` lines, non-zero exit when the daemon is down |
| `agentkit.imem.save` / `retry_pending` | `POST /v1/extract` (`session_end`, transcript under an `agent_roots` dir) | 202; extracted isolated, add-only, rules demoted |
| `agentkit.imem.recall` | `POST /v1/retrieve/preview`, `GET /v1/rules?pinned=1` | `context` block as the hook would inject it; pinned rule lines |
| bots' per-run MCP config | `imem mcp --agent --cwd ~/accountworkspace` | tools/list has `imem_search` only; results start with `Untrusted reference data from imem` |

The agents run `claude -p --restricted --strict-mcp-config` with explicit MCP lists, so
imem's hooks never fire in them; they get recall from deterministic code and the read-only
`--agent` server in their stage's MCP list.

Any other headless `claude -p` (a plugin's judge, a script, a future tool) keeps the user's
hooks, so it gets rules, recall, the protocol and extraction automatically. The hook marks it
headless (`CLAUDE_CODE_SESSION_ATTENDED=0`, an `sdk-*` `CLAUDE_CODE_ENTRYPOINT`, or
`IMEM_AGENT=<name>`): its Stop never blocks on a flush, and its extraction runs at agent trust
(isolated, add-only, rules and preferences demoted) even though its transcript sits under
`~/.claude/projects`. `ignore_cwds` still drops spawns that are not conversations at all.

### All rules in every session: the rules file

Claude Code inlines at most 10,000 chars per hook output, and the live rules run to ~110K, so
SessionStart alone can't carry them all. With `"rules_file": "~/.claude/imem-rules.md"` the
daemon writes every live rule there (pinned first, then by project, no ages, so the text stays
stable and cacheable) at startup and at every session start, and the SessionStart / SubagentStart
block then carries only preferences and the protocol. Import it from `~/.claude/CLAUDE.md` with a
line `@imem-rules.md`: Claude Code loads imports into the system prompt of every session,
headless `claude -p` and subagent alike, with no hook cap. A session reads the file as the
previous session start left it. Without the import line, set no `rules_file`: the rules would
otherwise reach no session at all.

### Pinned rules

`imem pin <id or title words>` marks a rule or preference as core: it leads the SessionStart
and SubagentStart block in every repo, survives the `rules_k` cap and the char budget ahead
of local rules, and is listed first by `imem rules` (`--pinned` lists only those). Keep the
pinned set small — the whole block must stay under Claude Code's 10,000-char inline cap.

## Coexistence with other memory systems

| system | role here |
|---|---|
| imem | automatic, cross-repo long-term memory; pushed per prompt, pulled via MCP |
| Claude Code auto-memory (`MEMORY.md`) | per-project notes the session model writes itself; overlaps with `imem_remember` |
| episodic-memory plugin | full-text search over raw past transcripts; complementary |
| claude-mem plugin | installed but disabled since 2026-04; its `<claude-mem-context>` blocks in `~/.claude/CLAUDE.md` and `~/play/CLAUDE.md` are stale yet still loaded every session |

## Config

`~/.config/infinite-memory/config.json` (see `config.example.json`); all fields
optional. Retrieval knobs:

| key | default | meaning |
|---|---|---|
| `extract_effort` | high | `--effort` of every extraction spawn |
| `extract_max_usage` | 0.85 | defer extraction above this share of the 5-hour window (-1 = never) |
| `retrieve_k` | 6 | max memories per prompt before the char budget |
| `retrieve_max_chars` | 6000 | per-prompt block budget (Claude Code inlines ≤10,000) |
| `min_match` | 2.0 | relevance floor on the hook path (0 disables) |
| `related_min_weight` | 2 | minimum `RELATED` edge weight the 1-hop query follows |
| `rules_on_session_start` | true | standing rules once per session instead of per prompt |
| `rules_max_chars` | 8000 | SessionStart block budget |
| `rules_k` | 50 | standing rules considered before the budget (0 = off) |
| `ignore_cwds` | `["~/.claude/double-shot-latte"]` | sessions memory never touches |
| `consolidate_enabled` | false | daily consolidation in the daemon |
| `consolidate_interval_hours` / `consolidate_min_jaccard` | 24 / 0.4 | its schedule and cluster threshold |

The capped knobs share one convention:

| value | `retrieve_k`, `retrieve_max_chars`, `rules_max_chars` | `rules_k` | `hook_summary_lines`, `hook_saved_lines` |
|---|---|---|---|
| `-1` (any negative) | no cap | no cap — every rule | no cap — one line per memory |
| `0` / absent | compiled default | **rules off** | compiled default (6) |
| `n > 0` | n | best n rules | first n lines, rest as "… N more" |

## Legacy: live LLM query expansion (off)

`expand_enabled: true` still asks headless `claude` for extra search terms on every prompt
before the graph is touched. It is off by default and should stay off: measured over the
last 300 prompts it cost **32s median, 59s p90, up to 5 minutes** (a timeout) per prompt
for a median of two extra terms, and Claude Code renders nothing while a
`UserPromptSubmit` hook runs. Its job is now done by index-time aliases and by the session
model querying `imem_search` itself. `imem expand "prompt"` still shows what it would add.
