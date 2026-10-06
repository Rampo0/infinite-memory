// Package eval measures retrieval against hand-labelled cases, so scoring
// changes are judged on numbers instead of on how one prompt felt.
//
// Two modes mirror the two ways memory reaches a model:
//   - hook: what UserPromptSubmit injects, scored on what the model can
//     actually SEE (Claude Code inlines additionalContext up to 10,000
//     chars and shows a 2KB preview of anything larger);
//   - search: what `imem search` returns — the ai-review and on-call agents
//     read exactly these top-10 results.
package eval

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// Claude Code's hook-output limits (docs: hooks reference, additionalContext).
const (
	InlineCap    = 10000
	PreviewChars = 2048
)

// Case is one labelled query. Expect holds case-insensitive substrings of the
// titles that a good retrieval must surface.
type Case struct {
	Name    string   `json:"name"`
	Mode    string   `json:"mode"` // "hook" or "search"
	Prompt  string   `json:"prompt"`
	Context string   `json:"context,omitempty"`
	CWD     string   `json:"cwd"`
	Expect  []string `json:"expect"`
}

// CaseScore is how one result list fared against its expectations. Rank is
// 1-based and 0 on a miss.
type CaseScore struct {
	Hit    bool    `json:"hit"`
	Rank   int     `json:"rank"`
	Recall float64 `json:"recall"`
}

// Result is one executed case.
type Result struct {
	Name   string    `json:"name"`
	Mode   string    `json:"mode"`
	Score  CaseScore `json:"score"`
	Titles []string  `json:"titles"` // what was visible / returned, in order
	Chars  int       `json:"chars"`  // injected block size (hook mode)
	MS     int64     `json:"ms"`
	Err    string    `json:"error,omitempty"`
}

// ModeSummary aggregates one mode's results.
type ModeSummary struct {
	N          int     `json:"n"`
	HitRate    float64 `json:"hit_rate"`
	MRR        float64 `json:"mrr"`
	MeanRecall float64 `json:"mean_recall"`
	MaxChars   int     `json:"max_chars"`
	P50MS      int64   `json:"p50_ms"`
	P95MS      int64   `json:"p95_ms"`
	MaxMS      int64   `json:"max_ms"`
}

// ParseCases reads JSONL cases; blank lines and #-comments are skipped. An
// unknown mode is an error rather than a silently dropped case.
func ParseCases(r io.Reader) ([]Case, error) {
	var out []Case
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		var c Case
		if err := json.Unmarshal([]byte(line), &c); err != nil {
			return nil, fmt.Errorf("line %d: %w", n, err)
		}
		if c.Mode != "hook" && c.Mode != "search" {
			return nil, fmt.Errorf("line %d: mode %q is neither hook nor search", n, c.Mode)
		}
		out = append(out, c)
	}
	return out, sc.Err()
}

// VisibleText is the part of an injected block the model really receives.
func VisibleText(block string) string {
	if len(block) <= InlineCap {
		return block
	}
	return block[:PreviewChars]
}

// TitlesFromBlock lists the memory titles of a context block in order,
// stopping at the standing-rules section (rules are not retrieval results).
func TitlesFromBlock(block string) []string {
	var titles []string
	for _, line := range strings.Split(block, "\n") {
		if strings.HasPrefix(line, "Standing rules") {
			break
		}
		if !strings.HasPrefix(line, "- [") {
			continue
		}
		rest := line[strings.Index(line, "]")+1:]
		title, _, _ := strings.Cut(rest, " — ")
		titles = append(titles, strings.TrimSpace(title))
	}
	return titles
}

// Score matches expectations against titles, case-insensitively.
func Score(expect, titles []string) CaseScore {
	var s CaseScore
	if len(expect) == 0 {
		return s
	}
	found := make([]bool, len(expect))
	for i, t := range titles {
		lt := strings.ToLower(t)
		for j, e := range expect {
			if strings.Contains(lt, strings.ToLower(e)) {
				if !s.Hit {
					s.Hit, s.Rank = true, i+1
				}
				found[j] = true
			}
		}
	}
	n := 0
	for _, f := range found {
		if f {
			n++
		}
	}
	s.Recall = float64(n) / float64(len(expect))
	return s
}

// Summarize aggregates results per mode.
func Summarize(results []Result) map[string]ModeSummary {
	byMode := map[string][]Result{}
	for _, r := range results {
		byMode[r.Mode] = append(byMode[r.Mode], r)
	}
	out := map[string]ModeSummary{}
	for mode, rs := range byMode {
		var s ModeSummary
		var lat []int64
		for _, r := range rs {
			s.N++
			if r.Score.Hit {
				s.HitRate++
				s.MRR += 1 / float64(r.Score.Rank)
			}
			s.MeanRecall += r.Score.Recall
			if r.Chars > s.MaxChars {
				s.MaxChars = r.Chars
			}
			lat = append(lat, r.MS)
		}
		n := float64(s.N)
		s.HitRate, s.MRR, s.MeanRecall = s.HitRate/n, s.MRR/n, s.MeanRecall/n
		sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
		s.P50MS = lat[(len(lat)-1)/2]
		s.P95MS = lat[int(float64(len(lat)-1)*0.95)]
		s.MaxMS = lat[len(lat)-1]
		out[mode] = s
	}
	return out
}

// Runner executes cases against a daemon.
type Runner struct {
	BaseURL string
	Timeout time.Duration
}

// Run executes one case. Errors are recorded on the result, never fatal: a
// down endpoint should read as a miss in the report, not abort the run.
func (rn Runner) Run(c Case) Result {
	res := Result{Name: c.Name, Mode: c.Mode}
	start := time.Now()
	var err error
	if c.Mode == "hook" {
		var block string
		block, err = rn.hookBlock(c)
		res.Chars = len(block)
		res.Titles = TitlesFromBlock(VisibleText(block))
	} else {
		res.Titles, err = rn.searchTitles(c)
	}
	res.MS = time.Since(start).Milliseconds()
	if err != nil {
		res.Err = err.Error()
	}
	res.Score = Score(c.Expect, res.Titles)
	return res
}

// hookBlock asks for the block UserPromptSubmit would inject. It prefers the
// read-only preview endpoint; a daemon that predates it gets the real
// endpoint with a session id no hook ever uses, so no user's save report is
// drained by the measurement.
func (rn Runner) hookBlock(c Case) (string, error) {
	body, _ := json.Marshal(map[string]string{"cwd": c.CWD, "prompt": c.Prompt, "context": c.Context, "session_id": "imem-eval"})
	var out struct {
		Context string `json:"context"`
	}
	status, err := rn.post("/v1/retrieve/preview", body, &out)
	if err == nil && status == http.StatusNotFound {
		status, err = rn.post("/v1/retrieve", body, &out)
	}
	if err != nil {
		return "", err
	}
	if status >= 300 {
		return "", fmt.Errorf("status %d", status)
	}
	return out.Context, nil
}

func (rn Runner) searchTitles(c Case) ([]string, error) {
	u := rn.BaseURL + "/v1/memories?" + url.Values{"cwd": {c.CWD}, "q": {c.Prompt}, "limit": {"10"}}.Encode()
	client := http.Client{Timeout: rn.Timeout}
	resp, err := client.Get(u)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	// Same decoding as `imem search`, so the measurement sees what agents see.
	var out struct {
		Memories []struct {
			Title string `json:"Title"`
		} `json:"memories"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	titles := make([]string, 0, len(out.Memories))
	for _, m := range out.Memories {
		titles = append(titles, m.Title)
	}
	return titles, nil
}

func (rn Runner) post(path string, body []byte, out any) (int, error) {
	client := http.Client{Timeout: rn.Timeout}
	resp, err := client.Post(rn.BaseURL+path, "application/json", bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode >= 300 {
		return resp.StatusCode, nil
	}
	return resp.StatusCode, json.NewDecoder(resp.Body).Decode(out)
}
