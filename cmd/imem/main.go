// imem is the single binary for infinite-memory: the daemon, the Claude Code
// hook client, and admin commands. The hook path stays light — it only ever
// reads config and speaks HTTP to the daemon.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Rampo0/infinite-memory/internal/backup"
	"github.com/Rampo0/infinite-memory/internal/client"
	"github.com/Rampo0/infinite-memory/internal/config"
	"github.com/Rampo0/infinite-memory/internal/daemon"
	"github.com/Rampo0/infinite-memory/internal/graph"
	"github.com/Rampo0/infinite-memory/internal/hookio"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "hook":
		hookMain(os.Args[2:])
	case "daemon":
		if err := daemon.Run(config.Load()); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	case "init":
		cmdInit()
	case "search":
		cmdSearch(os.Args[2:])
	case "expand":
		cmdExpand(os.Args[2:])
	case "entities":
		cmdEntities(os.Args[2:])
	case "entity":
		cmdEntity(os.Args[2:])
	case "backup":
		cmdBackup()
	case "backups":
		cmdBackups()
	case "restore":
		cmdRestore(os.Args[2:])
	case "status":
		cmdStatus()
	case "hooks-json":
		cmdHooksJSON()
	case "help", "-h", "--help":
		usage()
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `imem — infinite-memory for Claude Code

  imem daemon                          run the memory daemon (foreground)
  imem hook user-prompt|stop|session-end   hook entrypoints (stdin JSON from Claude Code)
  imem init                            ensure Memgraph schema, list indexes/constraints
  imem search "query" [--cwd path]     search memories via the daemon
  imem expand "prompt" [--cwd path]    show what the LLM expander would add (no search)
  imem entities [--project] [--limit N]    list entities by mention count (global by default)
  imem entity <name...>                one entity: relations + memories mentioning it
  imem backup                          dump the graph now to the backup dir
  imem backups                         list backups (newest first)
  imem restore <file> --yes            WIPE the graph and replay a backup
  imem status                          daemon + graph health and per-project counts
  imem hooks-json                      print the ~/.claude/settings.json hooks snippet
`)
}

// hookMain never exits non-zero and never writes to stdout except a valid
// hookSpecificOutput payload — a broken memory system must not break Claude.
func hookMain(args []string) {
	if os.Getenv("INFINITE_MEMORY_INTERNAL") == "1" {
		os.Exit(0)
	}
	defer func() {
		_ = recover()
		os.Exit(0)
	}()
	if len(args) < 1 {
		return
	}
	cfg := config.Load()
	in, err := hookio.Read(os.Stdin)
	if err != nil {
		return
	}

	switch args[0] {
	case "user-prompt":
		prompt := strings.TrimSpace(in.PromptText())
		if prompt == "" || strings.HasPrefix(prompt, "/") {
			return
		}
		start := time.Now()
		// The daemon spends the expansion budget before the graph budget, so
		// the client must outlast both or it cancels work already paid for.
		timeout := 2 * time.Second
		if cfg.ExpandEnabled {
			timeout = cfg.ExpandBudget() + cfg.RetrieveTO() + time.Second
		}
		resp, err := client.Retrieve(cfg.BaseURL(), client.RetrieveRequest{
			CWD: in.CWD, Prompt: prompt, SessionID: in.SessionID,
		}, timeout)
		ms := time.Since(start).Milliseconds()

		block := ""
		if err == nil {
			block = strings.TrimSpace(resp.Context)
		}
		// The user-facing line: what went in, or why nothing did. Silent
		// memory is indistinguishable from dead memory, so both the no-match
		// and daemon-down cases say so out loud.
		msg := ""
		if cfg.HookShowRetrieved {
			switch {
			case err != nil:
				msg = "imem: daemon unreachable — memory off"
			case resp.Memories == 0:
				// Say when expansion ran and still found nothing: otherwise a
				// slow, fruitless prompt looks identical to a fast one.
				msg = fmt.Sprintf("imem: no matches — 0 memories, %d rules (%dms)", resp.Rules, ms)
				if resp.Expanded != "" {
					msg += " · " + resp.Expanded
				}
			default:
				msg = fmt.Sprintf("imem: %d memories + %d rules (%dms)", resp.Memories, resp.Rules, ms)
				if resp.Expanded != "" {
					msg += " · " + resp.Expanded
				}
				if resp.Summary != "" {
					msg += "\n" + resp.Summary
				}
			}
		}
		// The save side: anything the extractor wrote since the last report,
		// including runs the Stop hook was too slow to see.
		if err == nil && cfg.HookShowSaved {
			if saved := savedMessage(resp.Saved); saved != "" {
				if msg != "" {
					msg += "\n"
				}
				msg += saved
			}
		}
		if block == "" && msg == "" {
			return
		}
		_ = hookio.EmitContext(os.Stdout, "UserPromptSubmit", block, msg)
	case "stop":
		if in.StopHookActive {
			return
		}
		req := client.ExtractRequest{
			SessionID: in.SessionID, TranscriptPath: in.TranscriptPath, CWD: in.CWD, Source: "stop",
		}
		if cfg.HookFlushOnStop {
			// Blocking: force extraction now so the line describes THIS turn.
			// The daemon caps its own wait at BudgetMS and answers "running"
			// past it, so the extra second here is slack, not a second wait.
			req.BudgetMS = cfg.StopFlushBudgetMS
			budget := time.Duration(cfg.StopFlushBudgetMS) * time.Millisecond
			if resp, err := client.Flush(cfg.BaseURL(), req, budget+3*time.Second); err == nil {
				if cfg.HookShowSaved {
					if msg := savedMessage(resp.Saved); msg != "" {
						// Empty context on purpose: Stop must never inject.
						_ = hookio.EmitContext(os.Stdout, "Stop", "", msg)
					}
				}
				return
			}
			// Daemon unreachable: fall through to the debounce path so the
			// turn is still saved, just later and silently.
			req.BudgetMS = 0
		}
		_, _ = client.NotifyExtract(cfg.BaseURL(), req, 500*time.Millisecond)
	case "session-end":
		// Still fire-and-forget: the session is over, so nothing would render.
		_, _ = client.NotifyExtract(cfg.BaseURL(), client.ExtractRequest{
			SessionID: in.SessionID, TranscriptPath: in.TranscriptPath, CWD: in.CWD, Source: "session_end",
		}, 500*time.Millisecond)
	}
}

// savedMessage renders the save-side counterpart of the retrieve line. Returns
// "" when there is nothing to say — a silent turn is correct when nothing was
// extracted and nothing is pending.
func savedMessage(p client.SavedPayload) string {
	var msg string
	switch {
	case p.Error != "":
		msg = "imem: save failed — " + p.Error
	case p.Count > 0:
		noun := "memories"
		if p.Count == 1 {
			noun = "memory"
		}
		msg = fmt.Sprintf("imem: saved %d %s (%.1fs)", p.Count, noun, float64(p.MS)/1000)
	case p.Status == "running":
		msg = "imem: extracting… (report at next prompt)"
	case p.Status == "skipped":
		msg = "imem: saved nothing — no new facts this turn"
	case p.DueInS > 0:
		msg = fmt.Sprintf("imem: extracting in %ds", p.DueInS)
	default:
		return ""
	}
	if p.Summary != "" {
		msg += "\n" + p.Summary
	}
	return msg
}

func cmdInit() {
	cfg := config.Load()
	store, err := graph.New(cfg.MemgraphURI, cfg.MemgraphUser, cfg.MemgraphPass)
	if err != nil {
		fmt.Fprintln(os.Stderr, "driver:", err)
		os.Exit(1)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := store.Ping(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "memgraph unreachable at %s: %v\n", cfg.MemgraphURI, err)
		os.Exit(1)
	}
	if err := store.EnsureSchema(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "schema:", err)
		os.Exit(1)
	}
	lines, err := store.ShowSchema(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "show schema:", err)
		os.Exit(1)
	}
	fmt.Println("schema ensured:")
	for _, l := range lines {
		fmt.Println(" ", l)
	}
}

func cmdSearch(args []string) {
	cfg := config.Load()
	query, cwd := "", ""
	for i := 0; i < len(args); i++ {
		if args[i] == "--cwd" && i+1 < len(args) {
			cwd = args[i+1]
			i++
			continue
		}
		if query == "" {
			query = args[i]
		}
	}
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	if query == "" {
		fmt.Fprintln(os.Stderr, `usage: imem search "query" [--cwd path]`)
		os.Exit(2)
	}

	u := cfg.BaseURL() + "/v1/memories?" + url.Values{
		"cwd": {cwd}, "q": {query}, "limit": {"10"},
	}.Encode()
	var out struct {
		Project  string `json:"project"`
		Memories []struct {
			Title   string  `json:"Title"`
			Content string  `json:"Content"`
			Kind    string  `json:"Kind"`
			Score   float64 `json:"score"`
		} `json:"memories"`
	}
	if err := getJSON(u, &out, cfg.ExpandBudget()+5*time.Second); err != nil {
		fmt.Fprintln(os.Stderr, "daemon unreachable:", err)
		os.Exit(1)
	}
	fmt.Printf("project: %s (%d hits)\n", out.Project, len(out.Memories))
	for _, m := range out.Memories {
		fmt.Printf("  [%s] %s — %s (score %.2f)\n", m.Kind, m.Title, m.Content, m.Score)
	}
}

func cmdEntities(args []string) {
	cfg := config.Load()
	limit, scoped := "50", false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--project":
			scoped = true
		case "--limit":
			if i+1 < len(args) {
				limit = args[i+1]
				i++
			}
		}
	}
	vals := url.Values{"limit": {limit}}
	scope := "all projects"
	if scoped {
		cwd, _ := os.Getwd()
		vals.Set("cwd", cwd)
		scope = "current project"
	}
	var out struct {
		Entities []graph.EntityInfo `json:"entities"`
	}
	if err := getJSON(cfg.BaseURL()+"/v1/entities?"+vals.Encode(), &out); err != nil {
		fmt.Fprintln(os.Stderr, "daemon unreachable:", err)
		os.Exit(1)
	}
	fmt.Printf("entities (%s, %d shown, by mention count):\n", scope, len(out.Entities))
	for _, e := range out.Entities {
		fmt.Printf("  %3dx  %-40s [%s]  %s\n", e.Mentions, e.Name, e.Etype, filepath.Base(e.ProjectKey))
	}
}

func cmdEntity(args []string) {
	name := strings.TrimSpace(strings.Join(args, " "))
	if name == "" {
		fmt.Fprintln(os.Stderr, "usage: imem entity <name...>")
		os.Exit(2)
	}
	cfg := config.Load()
	var d graph.EntityDetail
	u := cfg.BaseURL() + "/v1/entity?" + url.Values{"name": {name}}.Encode()
	if err := getJSON(u, &d); err != nil {
		fmt.Fprintln(os.Stderr, "daemon unreachable:", err)
		os.Exit(1)
	}
	if len(d.Nodes) == 0 {
		fmt.Printf("no entity matching %q\n", name)
		return
	}

	fmt.Printf("%s [%s] — appears in:", d.Nodes[0].Name, d.Nodes[0].Etype)
	for _, n := range d.Nodes {
		fmt.Printf(" %s(%dx)", filepath.Base(n.ProjectKey), n.Mentions)
	}
	fmt.Println()

	if len(d.Relations) > 0 {
		fmt.Printf("\nrelations (%d):\n", len(d.Relations))
		for _, r := range d.Relations {
			verb := r.Verb
			if verb == "" {
				verb = "related-to"
			}
			fmt.Printf("  %-12s → %-40s [%s] w%d  %s\n", verb, r.Name, r.Etype, r.Weight, filepath.Base(r.ProjectKey))
		}
	}

	if len(d.Memories) > 0 {
		fmt.Printf("\nmemories (%d):\n", len(d.Memories))
		now := time.Now().Unix()
		for _, m := range d.Memories {
			fmt.Printf("  [%s] %s — %s (%s, %s)\n", m.Kind, m.Title, m.Content, filepath.Base(m.ProjectKey), ago(now, m.LastSeen))
		}
	}
}

func ago(now, ts int64) string {
	d := time.Duration(now-ts) * time.Second
	switch {
	case d < 90*time.Second:
		return "just now"
	case d < 90*time.Minute:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 36*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

func cmdStatus() {
	cfg := config.Load()
	var health map[string]any
	if err := getJSON(cfg.BaseURL()+"/healthz", &health); err != nil {
		fmt.Printf("daemon: DOWN (%s): %v\n", cfg.HTTPAddr, err)
		os.Exit(1)
	}
	fmt.Printf("daemon: up (%s), memgraph: %v\n", cfg.HTTPAddr, health["memgraph"])
	var stats struct {
		Projects []graph.ProjectStats `json:"projects"`
	}
	if err := getJSON(cfg.BaseURL()+"/v1/stats", &stats); err == nil {
		for _, p := range stats.Projects {
			fmt.Printf("  %s: %d memories, %d entities, %d sessions\n", p.Key, p.Memories, p.Entities, p.Sessions)
		}
	}
	// Extraction health: the silent-failure case that is otherwise only
	// visible by tailing the daemon log.
	var sv struct {
		Sessions []struct {
			SessionID  string `json:"session_id"`
			At         int64  `json:"at"`
			DurationMS int64  `json:"duration_ms"`
			Memories   []struct {
				Title string `json:"title"`
			} `json:"memories"`
			Skipped string `json:"skipped"`
			Error   string `json:"error"`
		} `json:"sessions"`
	}
	if err := getJSON(cfg.BaseURL()+"/v1/saved", &sv); err == nil {
		if len(sv.Sessions) == 0 {
			fmt.Println("extraction: no runs since the daemon started")
		} else {
			fmt.Printf("extraction: %d recent session(s)\n", len(sv.Sessions))
			now := time.Now().Unix()
			for _, r := range sv.Sessions {
				sid := r.SessionID
				if len(sid) > 8 {
					sid = sid[:8]
				}
				switch {
				case r.Error != "":
					fmt.Printf("  %s  %-9s FAILED — %s\n", sid, ago(now, r.At), r.Error)
				case len(r.Memories) > 0:
					fmt.Printf("  %s  %-9s %d saved (%.1fs)\n", sid, ago(now, r.At), len(r.Memories), float64(r.DurationMS)/1000)
				default:
					fmt.Printf("  %s  %-9s nothing saved\n", sid, ago(now, r.At))
				}
			}
		}
	}
	var bk struct {
		IntervalHours int           `json:"interval_hours"`
		Keep          int           `json:"keep"`
		Backups       []backup.Info `json:"backups"`
		LastError     string        `json:"last_error"`
	}
	if err := getJSON(cfg.BaseURL()+"/v1/backups", &bk); err == nil {
		if len(bk.Backups) == 0 {
			fmt.Printf("backups: none yet (every %dh, keep %d)\n", bk.IntervalHours, bk.Keep)
		} else {
			b := bk.Backups[0]
			fmt.Printf("backups: %d kept, last %s (%s), every %dh\n",
				len(bk.Backups), ago(time.Now().Unix(), b.ModTime.Unix()), humanBytes(b.Size), bk.IntervalHours)
		}
		if bk.LastError != "" {
			fmt.Printf("  backup error: %s\n", bk.LastError)
		}
	}
}

func cmdBackup() {
	cfg := config.Load()
	var out struct {
		Path       string `json:"path"`
		Bytes      int64  `json:"bytes"`
		Statements int    `json:"statements"`
		Error      string `json:"error"`
	}
	if err := postJSON(cfg.BaseURL()+"/v1/backup", &out); err != nil {
		fmt.Fprintln(os.Stderr, "backup failed:", err)
		if out.Error != "" {
			fmt.Fprintln(os.Stderr, " ", out.Error)
		}
		os.Exit(1)
	}
	fmt.Printf("wrote %s (%s, %d statements)\n", out.Path, humanBytes(out.Bytes), out.Statements)
}

func cmdBackups() {
	cfg := config.Load()
	var out struct {
		Enabled       bool          `json:"enabled"`
		Dir           string        `json:"dir"`
		IntervalHours int           `json:"interval_hours"`
		Keep          int           `json:"keep"`
		Backups       []backup.Info `json:"backups"`
		LastError     string        `json:"last_error"`
	}
	if err := getJSON(cfg.BaseURL()+"/v1/backups", &out); err != nil {
		fmt.Fprintln(os.Stderr, "daemon unreachable:", err)
		os.Exit(1)
	}
	fmt.Printf("backups: %s (every %dh, keep %d, enabled %v)\n", out.Dir, out.IntervalHours, out.Keep, out.Enabled)
	if out.LastError != "" {
		fmt.Printf("  last error: %s\n", out.LastError)
	}
	if len(out.Backups) == 0 {
		fmt.Println("  none yet")
		return
	}
	now := time.Now().Unix()
	for _, b := range out.Backups {
		fmt.Printf("  %-9s %-10s %s\n", humanBytes(b.Size), ago(now, b.ModTime.Unix()), filepath.Base(b.Path))
	}
}

// cmdRestore talks to Bolt directly rather than through the daemon: a restore
// is exactly what you need when the daemon is down or crash-looping.
func cmdRestore(args []string) {
	path, yes := "", false
	for _, a := range args {
		if a == "--yes" || a == "-y" {
			yes = true
			continue
		}
		if path == "" {
			path = a
		}
	}
	if path == "" {
		fmt.Fprintln(os.Stderr, "usage: imem restore <file.cypherl.gz> --yes")
		os.Exit(2)
	}

	stmts, err := backup.Load(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "read backup:", err)
		os.Exit(1)
	}
	// A dump always carries the __mg_vertex__ scaffolding. Requiring it stops
	// a wrong or truncated file from wiping a healthy graph.
	if len(stmts) == 0 || !strings.Contains(strings.Join(stmts, "\n"), "__mg_vertex__") {
		fmt.Fprintf(os.Stderr, "%s does not look like an imem dump (%d statements, no __mg_vertex__)\n", path, len(stmts))
		os.Exit(1)
	}

	cfg := config.Load()
	if !yes {
		fmt.Fprintf(os.Stderr, `restore would DROP GRAPH on %s — deleting every node, edge,
index and constraint — then replay %d statements from
  %s

Nothing has been changed. Re-run with --yes to proceed.
`, cfg.MemgraphURI, len(stmts), path)
		os.Exit(2)
	}

	store, err := graph.New(cfg.MemgraphURI, cfg.MemgraphUser, cfg.MemgraphPass)
	if err != nil {
		fmt.Fprintln(os.Stderr, "driver:", err)
		os.Exit(1)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	if err := store.Ping(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "memgraph unreachable at %s: %v\n", cfg.MemgraphURI, err)
		os.Exit(1)
	}
	if err := store.Restore(ctx, stmts); err != nil {
		fmt.Fprintln(os.Stderr, "restore:", err)
		os.Exit(1)
	}
	fmt.Printf("restored %d statements from %s\n", len(stmts), path)
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1fMB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0fKB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%dB", n)
	}
}

func cmdExpand(args []string) {
	cfg := config.Load()
	prompt, cwd := "", ""
	for i := 0; i < len(args); i++ {
		if args[i] == "--cwd" && i+1 < len(args) {
			cwd = args[i+1]
			i++
			continue
		}
		if prompt == "" {
			prompt = args[i]
		}
	}
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	if prompt == "" {
		fmt.Fprintln(os.Stderr, `usage: imem expand "prompt" [--cwd path]`)
		os.Exit(2)
	}
	u := cfg.BaseURL() + "/v1/expand?" + url.Values{"cwd": {cwd}, "q": {prompt}}.Encode()
	var out struct {
		Project  string   `json:"project"`
		Tokens   []string `json:"tokens"`
		Intent   string   `json:"intent"`
		Expanded []string `json:"expanded"`
		MS       int64    `json:"ms"`
		Model    string   `json:"model"`
		Error    string   `json:"error"`
	}
	if err := getJSON(u, &out, cfg.ExpandBudget()+5*time.Second); err != nil {
		fmt.Fprintln(os.Stderr, "daemon unreachable:", err)
		os.Exit(1)
	}
	fmt.Printf("project:  %s\n", out.Project)
	fmt.Printf("prompt:   %s\n", prompt)
	fmt.Printf("tokens:   %s\n", orNone(out.Tokens))
	if out.Intent != "" {
		fmt.Printf("intent:   %s\n", out.Intent)
	}
	if out.Error != "" {
		// An expansion that failed and one that legitimately had nothing to
		// add both print no terms; only this line tells them apart.
		fmt.Printf("expanded: FAILED — %s\n", out.Error)
	} else {
		fmt.Printf("expanded: %s\n", orNone(out.Expanded))
	}
	fmt.Printf("%dms (%s)\n", out.MS, out.Model)
}

func orNone(items []string) string {
	if len(items) == 0 {
		return "(none)"
	}
	return strings.Join(items, ", ")
}

func cmdHooksJSON() {
	exe, err := os.Executable()
	if err == nil {
		if resolved, rerr := filepath.EvalSymlinks(exe); rerr == nil {
			exe = resolved
		}
	} else {
		exe = "imem"
	}
	snippet := map[string]any{
		"hooks": map[string]any{
			"UserPromptSubmit": []any{map[string]any{
				// Generous because expansion (expand_enabled) spends up to
				// expand_budget_ms inside this hook before the graph is touched.
				"hooks": []any{map[string]any{"type": "command", "command": exe + " hook user-prompt", "timeout": 30}},
			}},
			// Stop must be synchronous: an async hook's stdout is discarded,
			// which would silently drop the "saved" line. timeout covers the
			// blocking flush (stop_flush_budget_ms, default 90s) plus slack.
			"Stop": []any{map[string]any{
				"hooks": []any{map[string]any{"type": "command", "command": exe + " hook stop", "async": false, "timeout": 120}},
			}},
			"SessionEnd": []any{map[string]any{
				"hooks": []any{map[string]any{"type": "command", "command": exe + " hook session-end", "async": true}},
			}},
		},
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(snippet)
	fmt.Fprintln(os.Stderr, "\nMerge these entries as ADDITIONAL array elements into ~/.claude/settings.json (keep existing hooks).")
	fmt.Fprintln(os.Stderr, "Keep Stop synchronous with its timeout: async hooks have their stdout discarded, so the \"saved\" line would never print.")
}

// getJSON defaults to a short timeout; pass one explicitly for endpoints that
// may spawn claude, where 3s would cancel work the daemon is still doing.
func getJSON(u string, out any, timeout ...time.Duration) error {
	d := 3 * time.Second
	if len(timeout) > 0 {
		d = timeout[0]
	}
	c := http.Client{Timeout: d}
	resp, err := c.Get(u)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func postJSON(u string, out any) error {
	c := http.Client{Timeout: 90 * time.Second}
	resp, err := c.Post(u, "application/json", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		_ = json.NewDecoder(resp.Body).Decode(out)
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
