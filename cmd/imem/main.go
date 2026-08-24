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
		resp, err := client.Retrieve(cfg.BaseURL(), client.RetrieveRequest{
			CWD: in.CWD, Prompt: prompt, SessionID: in.SessionID,
		}, 2*time.Second)
		if err != nil || strings.TrimSpace(resp.Context) == "" {
			return
		}
		_ = hookio.EmitContext(os.Stdout, "UserPromptSubmit", resp.Context)
	case "stop":
		if in.StopHookActive {
			return
		}
		_ = client.NotifyExtract(cfg.BaseURL(), client.ExtractRequest{
			SessionID: in.SessionID, TranscriptPath: in.TranscriptPath, CWD: in.CWD, Source: "stop",
		}, 500*time.Millisecond)
	case "session-end":
		_ = client.NotifyExtract(cfg.BaseURL(), client.ExtractRequest{
			SessionID: in.SessionID, TranscriptPath: in.TranscriptPath, CWD: in.CWD, Source: "session_end",
		}, 500*time.Millisecond)
	}
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
	if err := getJSON(u, &out); err != nil {
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
				"hooks": []any{map[string]any{"type": "command", "command": exe + " hook user-prompt", "timeout": 5}},
			}},
			"Stop": []any{map[string]any{
				"hooks": []any{map[string]any{"type": "command", "command": exe + " hook stop", "async": true}},
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
}

func getJSON(u string, out any) error {
	c := http.Client{Timeout: 3 * time.Second}
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
