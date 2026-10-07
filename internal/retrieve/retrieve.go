// Package retrieve turns a user prompt into a ranked context block:
// tokenize -> global graph candidate queries -> score in Go (with a
// same-project boost) -> top-K -> plus standing rules -> format.
package retrieve

import (
	"context"
	"fmt"
	"math"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Rampo0/infinite-memory/internal/graph"
	"github.com/Rampo0/infinite-memory/internal/textutil"
)

type Scored struct {
	graph.Candidate
	Score float64 `json:"score"`
	Q1    int64   `json:"q1_hits"`
	Q2    int64   `json:"q2_hits"`
	Q3    int64   `json:"q3_hits"`
}

type Retriever struct {
	Store *graph.Store
	// K caps the scored memories per prompt; K <= 0 means no cap.
	K               int
	MaxContentChars int
	// SameProjectBoost is added to the score of memories from the current
	// project so local context wins ties without hiding other projects.
	SameProjectBoost float64
	// RulesK caps the always-on standing-rules section: 0 disables the
	// section, negative means no cap.
	RulesK int
	// SummaryLines caps the per-memory lines in the CLI summary; the rest
	// collapse into a "… N more" line. Negative means no cap.
	SummaryLines int
	// Expand appends LLM-derived terms to the prompt's own tokens. Nil means
	// no expansion — today's behaviour exactly. It takes no context because
	// the LLM call has already happened by the time Query runs: expansion
	// spends its own budget in the daemon, before the graph deadline starts.
	Expand func(tokens []string) []string
	// Corpus returns the live memory count N for idf weights; nil or <= 0
	// weighs every hit 1 (the pre-idf behaviour).
	Corpus func(ctx context.Context) int
	// MinMatch drops memories whose relevance (idf-weighted hits, before
	// recency, seen and project boosts) is below it. 0 keeps everything —
	// the right call for explicit searches, where the caller caps the list.
	MinMatch float64
	// RelatedMinWeight is the minimum RELATED edge weight the 1-hop query
	// follows; <= 1 follows every edge.
	RelatedMinWeight int
	// MaxChars caps the injected block (Claude Code inlines additionalContext
	// only up to 10,000 chars and shows a 2KB preview of anything larger).
	// <= 0 means no cap.
	MaxChars int
	// Exclude drops memories already injected earlier in the session: they
	// are still in the model's context, so re-sending them wastes budget.
	Exclude map[string]bool
}

// PromptTokenCap bounds the tokens taken from a prompt.
const PromptTokenCap = 32

// Result is one retrieval: the block injected into the model's context, plus
// the compact summary and counts shown to the user in the CLI.
type Result struct {
	Block    string `json:"block"`
	Summary  string `json:"summary"`
	Memories int    `json:"memories"`
	Rules    int    `json:"rules"`
	// Omitted counts matches (memories and rules) left out by MaxChars.
	Omitted int `json:"omitted"`
	// Injected are the memories that made it into Block, in order.
	Injected []Scored `json:"-"`
}

// Query returns the top-K scored memories for a prompt. Matching is global;
// pk only drives the same-project boost.
func (r *Retriever) Query(ctx context.Context, pk, prompt string, now int64) ([]Scored, error) {
	return r.QueryTokens(ctx, pk, r.expandTokens(prompt), now)
}

// QueryTokens is Query over ready-made tokens — the extractor's "existing
// memories like this excerpt" lookup has a transcript, not a prompt.
func (r *Retriever) QueryTokens(ctx context.Context, pk string, tokens []string, now int64) ([]Scored, error) {
	if len(tokens) == 0 {
		return nil, nil
	}
	q1, q2, q3, err := r.Store.Candidates(ctx, tokens, r.RelatedMinWeight)
	if err != nil {
		return nil, err
	}
	opts := ScoreOpts{SameProjectBoost: r.SameProjectBoost, MinMatch: r.MinMatch}
	if r.Corpus != nil {
		opts.Corpus = r.Corpus(ctx)
	}
	return capK(withoutIDs(MergeAndScore(q1, q2, q3, now, pk, opts), r.Exclude), r.K), nil
}

func capK(scored []Scored, k int) []Scored {
	if k > 0 && len(scored) > k {
		return scored[:k]
	}
	return scored
}

// Retrieve returns the context block plus its user-facing summary. Block is ""
// when nothing matched and no rules exist.
func (r *Retriever) Retrieve(ctx context.Context, pk, prompt string, now int64) (Result, error) {
	all := *r
	all.K = 0
	matched, err := all.Query(ctx, pk, prompt, now)
	if err != nil {
		return Result{}, err
	}
	scored := capK(matched, r.K)

	var rules []graph.Candidate
	if r.RulesK != 0 {
		// Fetch the whole rule pool: the current-project-first ordering and
		// the RulesK cap both happen in SortRules, so a Cypher-side LIMIT
		// would truncate before anything is prioritized.
		all, err := r.Store.Rules(ctx, -1)
		if err != nil {
			return Result{}, err
		}
		seen := make(map[string]bool, len(scored))
		for _, s := range scored {
			seen[s.ID] = true
		}
		rules = SortRules(all, pk, r.RulesK, seen)
	}

	if len(scored) == 0 && len(rules) == 0 {
		return Result{}, nil
	}
	block, shown, shownRules := BuildBlock(BlockInput{PK: pk, Mems: scored, Rules: rules, Matched: len(matched),
		MaxContentChars: r.MaxContentChars, MaxChars: r.MaxChars, Now: now})
	omitted := len(matched) - len(shown) + len(rules) - len(shownRules)
	summary := FormatSummary(pk, shown, shownRules, r.SummaryLines, now)
	if omitted > 0 {
		summary += fmt.Sprintf("\n  %d more matched, over the %d-char budget", omitted, r.MaxChars)
	}
	return Result{
		Block:    block,
		Summary:  summary,
		Memories: len(shown),
		Rules:    len(shownRules),
		Omitted:  omitted,
		Injected: shown,
	}, nil
}

// uniq drops repeated strings, keeping first occurrences in order.
func uniq(ss []string) []string {
	seen := make(map[string]bool, len(ss))
	out := ss[:0]
	for _, s := range ss {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// withoutIDs drops memories whose id is in exclude, keeping order.
func withoutIDs(scored []Scored, exclude map[string]bool) []Scored {
	if len(exclude) == 0 {
		return scored
	}
	out := scored[:0:0]
	for _, s := range scored {
		if !exclude[s.ID] {
			out = append(out, s)
		}
	}
	return out
}

// expandTokens is the prompt's own tokens plus whatever the expander added.
// Expansion runs AFTER tokenizing, not before: a prompt made entirely of
// stopwords tokenizes to nothing, and rescuing exactly that case is the point.
func (r *Retriever) expandTokens(prompt string) []string {
	tokens := textutil.Tokenize(prompt, PromptTokenCap)
	if r.Expand == nil {
		return tokens
	}
	return r.Expand(tokens)
}

// aliasWeight scales a term matched only through an alias.
const aliasWeight = 0.5

// ScoreOpts configures MergeAndScore.
type ScoreOpts struct {
	// SameProjectBoost is added to memories of the current project.
	SameProjectBoost float64
	// Corpus is the live memory count N; <= 0 weighs every hit 1.
	Corpus int
	// MinMatch is the relevance floor, applied before any boost; 0 = none.
	MinMatch float64
}

// idf is the BM25 inverse document frequency: a term in one memory of 2000
// weighs ~7.2, one in a quarter of them ~1.4. Always positive.
func idf(n, df int) float64 {
	if df < 1 {
		df = 1
	}
	return math.Log(1 + (float64(n)-float64(df)+0.5)/(float64(df)+0.5))
}

// MergeAndScore merges the three candidate lists by memory id and ranks:
//
//	direct = Σ idf(keyword, ½ when only an alias has it) + 2·Σ idf(specific exact entity)
//	         + Σ idf(single-word entity or partial term, once each)
//	         (zero direct match discarded)
//	match  = direct + 0.25·min(related, 4)       (dropped below opts.MinMatch)
//	score  = match + 2.0·exp(-ageDays/14) + 0.3·ln(1+seen_count)
//	       + 0.4·ln(1+used_count) - 1.0·min(disputed_count, 3)
//	       + opts.SameProjectBoost when the memory belongs to pk
//
// ageDays counts from the later of last seen and last used. Feedback only
// re-ranks: the relevance floor is applied to match, before any of it.
//
// Document frequencies come from the candidate rows themselves: the keyword
// and entity queries return every memory matching any term, so counting rows
// per term is exact. RELATED hits only re-rank direct matches — on their own
// they once pulled 1020 memories into a single prompt. Without a corpus size
// or matched lists, each hit weighs 1 (the pre-idf formula).
func MergeAndScore(q1, q2, q3 []graph.Candidate, now int64, pk string, o ScoreOpts) []Scored {
	byID := map[string]*Scored{}
	kwTerms := map[string][]string{}
	entTerms, partTerms := map[string][]string{}, map[string][]string{}
	dfKW, dfEnt, dfPart := map[string]int{}, map[string]int{}, map[string]int{}
	absorb := func(cands []graph.Candidate, set func(s *Scored, c graph.Candidate)) {
		for _, c := range cands {
			s := byID[c.ID]
			if s == nil {
				s = &Scored{Candidate: c}
				s.Matched, s.Partial, s.ViaAlias = nil, nil, nil
				byID[c.ID] = s
			}
			set(s, c)
		}
	}
	viaAlias := map[string]map[string]bool{}
	absorb(q1, func(s *Scored, c graph.Candidate) {
		s.Q1 = c.Hits
		kwTerms[c.ID] = c.Matched
		if len(c.ViaAlias) > 0 {
			viaAlias[c.ID] = make(map[string]bool, len(c.ViaAlias))
			for _, t := range c.ViaAlias {
				viaAlias[c.ID][t] = true
			}
		}
		for _, t := range c.Matched {
			dfKW[t]++
		}
	})
	absorb(q2, func(s *Scored, c graph.Candidate) {
		s.Q2 = c.Hits
		entTerms[c.ID] = c.Matched
		partTerms[c.ID] = c.Partial
		for _, e := range c.Matched {
			dfEnt[e]++
		}
		for _, t := range c.Partial {
			dfPart[t]++
		}
	})
	absorb(q3, func(s *Scored, c graph.Candidate) { s.Q3 = c.Hits })

	// A keyword term weighs its idf — half when it matched only through an
	// alias: aliases are the extractor's guesses, and while few memories have
	// them their words look artificially rare.
	weighKeywords := func(id string, hits int64) float64 {
		terms := kwTerms[id]
		if o.Corpus <= 0 || len(terms) == 0 {
			return float64(hits)
		}
		sum := 0.0
		for _, t := range terms {
			w := idf(o.Corpus, dfKW[t])
			if viaAlias[id][t] {
				w *= aliasWeight
			}
			sum += w
		}
		return sum
	}

	// A specific entity name (multi-part: "opening-account", "imem.save")
	// matched exactly weighs double its own idf. A single-word name repeats
	// a keyword's evidence ("account" is tagged on 40 memories but written in
	// 459), so it counts once, like a term — as does a term that only hit a
	// token of a longer name. Each term counts at most once per memory.
	entityWeight := func(id string, hits int64) float64 {
		exact, part := entTerms[id], partTerms[id]
		if o.Corpus <= 0 || (len(exact) == 0 && len(part) == 0) {
			return 2.0 * float64(hits)
		}
		w := 0.0
		counted := make(map[string]bool, len(kwTerms[id]))
		for _, t := range kwTerms[id] {
			counted[t] = true
		}
		asTerm := func(t string, entDF int) {
			if counted[t] {
				return
			}
			counted[t] = true
			df := max(dfKW[t], dfPart[t], entDF)
			w += idf(o.Corpus, df)
		}
		for _, e := range exact {
			if strings.ContainsAny(e, " _./-") {
				w += 2.0 * idf(o.Corpus, dfEnt[e])
				continue
			}
			asTerm(e, dfEnt[e])
		}
		for _, t := range part {
			asTerm(t, 0)
		}
		return w
	}

	out := make([]Scored, 0, len(byID))
	for id, s := range byID {
		direct := weighKeywords(id, s.Q1) + entityWeight(id, s.Q2)
		if direct == 0 {
			continue
		}
		// RELATED co-occurrence is weak evidence: at most +1.0.
		match := direct + 0.25*math.Min(float64(s.Q3), 4)
		if o.MinMatch > 0 && match < o.MinMatch {
			continue
		}
		// Fresh is last seen OR last used: a memory the assistant relied on
		// yesterday is as current as one re-extracted yesterday.
		ageDays := float64(now-max(s.LastSeen, s.LastUsed)) / 86400.0
		if ageDays < 0 {
			ageDays = 0
		}
		s.Score = match + 2.0*math.Exp(-ageDays/14.0) + 0.3*math.Log(1+float64(s.SeenCount)) +
			0.4*math.Log(1+float64(s.UsedCount)) - 1.0*math.Min(float64(s.DisputedCount), 3)
		if pk != "" && s.ProjectKey == pk {
			s.Score += o.SameProjectBoost
		}
		s.Matched = uniq(append(append(append([]string(nil), kwTerms[id]...), entTerms[id]...), partTerms[id]...))
		s.Partial = nil
		out = append(out, *s)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// SortRules orders standing rules pinned-first, then current-project-first,
// then by how often they were re-observed and how recently, caps at k (k <= 0
// means no cap) and drops ids already shown in the scored section.
func SortRules(rules []graph.Candidate, pk string, k int, exclude map[string]bool) []graph.Candidate {
	var out []graph.Candidate
	for _, r := range rules {
		if !exclude[r.ID] {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Pinned != out[j].Pinned {
			return out[i].Pinned
		}
		li, lj := out[i].ProjectKey == pk, out[j].ProjectKey == pk
		if li != lj {
			return li
		}
		if out[i].SeenCount != out[j].SeenCount {
			return out[i].SeenCount > out[j].SeenCount
		}
		if out[i].LastSeen != out[j].LastSeen {
			return out[i].LastSeen > out[j].LastSeen
		}
		return out[i].ID < out[j].ID
	})
	if k > 0 && len(out) > k {
		out = out[:k]
	}
	return out
}

// SessionRules is the standing context injected once per session, in fill
// order: pinned rules and preferences (the core set, e.g. the code
// constitution, which must survive the budget in every repo), this project's
// rules (a parent workspace counts as this project: ~/ws rules apply in
// ~/ws/repo), its preferences, then every other project's rules and
// preferences. Within a group: most re-observed, then most recent. The
// caller's char budget decides how far down the list makes it in.
func SessionRules(rules, prefs []graph.Candidate, pk string) []graph.Candidate {
	var groups [5][]graph.Candidate
	place := func(c graph.Candidate, localGroup, otherGroup int) {
		switch {
		case c.Pinned:
			groups[0] = append(groups[0], c)
		case isLocal(c, pk):
			groups[localGroup] = append(groups[localGroup], c)
		default:
			groups[otherGroup] = append(groups[otherGroup], c)
		}
	}
	for _, r := range rules {
		place(r, 1, 3)
	}
	for _, p := range prefs {
		place(p, 2, 4)
	}
	var out []graph.Candidate
	for _, g := range groups {
		sort.SliceStable(g, func(i, j int) bool {
			if g[i].SeenCount != g[j].SeenCount {
				return g[i].SeenCount > g[j].SeenCount
			}
			if g[i].LastSeen != g[j].LastSeen {
				return g[i].LastSeen > g[j].LastSeen
			}
			return g[i].ID < g[j].ID
		})
		out = append(out, g...)
	}
	return out
}

func isLocal(c graph.Candidate, pk string) bool {
	return c.ProjectKey != "" && (c.ProjectKey == pk || strings.HasPrefix(pk, c.ProjectKey+"/"))
}

func isHome(c graph.Candidate, home string) bool {
	return home != "" && (c.ProjectKey == home || c.ProjectKey == filepath.Join(home, ".claude"))
}

func HereSet(rules, prefs []graph.Candidate, pk, home string) (kept, skipped []graph.Candidate) {
	for _, c := range SessionRules(rules, prefs, pk) {
		if c.Pinned || isLocal(c, pk) || isHome(c, home) {
			kept = append(kept, c)
		} else {
			skipped = append(skipped, c)
		}
	}
	return kept, skipped
}

func StandingLines(items []graph.Candidate) []string {
	out := make([]string, len(items))
	for i, c := range items {
		out[i] = standingLine(c.Kind, c)
	}
	return out
}

const rulesFileHeader = `# imem standing rules (generated by imem, do not edit by hand)

The user's standing rules and preferences live in imem (infinite-memory); only pinned ones are inlined here.
Before you write or change code, review code, commit or push, open or edit an MR, or draft Slack or GitLab text,
run ` + "`%s`" + ` once in this session (again after /clear or compaction) and follow what it prints: pinned
items, this repo's and its workspace's rules and preferences, and the user's global ones. A repo's own written
rules win where they conflict. To change a rule, correct it in conversation or use imem; this file is rewritten
at every session start.
`

func RulesFileText(rules, prefs []graph.Candidate, fetch string) string {
	var b strings.Builder
	fmt.Fprintf(&b, rulesFileHeader, fetch)
	pinnedRules, pinnedPrefs := pinnedItems(rules), pinnedItems(prefs)
	if len(pinnedRules)+len(pinnedPrefs) > 0 {
		b.WriteString("\n## Pinned\n\n")
	}
	writeStanding(&b, "rule", pinnedRules)
	writeStanding(&b, "preference", pinnedPrefs)
	return b.String()
}

func pinnedItems(cs []graph.Candidate) []graph.Candidate {
	var out []graph.Candidate
	for _, c := range cs {
		if c.Pinned {
			out = append(out, c)
		}
	}
	return out
}

func writeStanding(b *strings.Builder, kind string, items []graph.Candidate) {
	sorted := append([]graph.Candidate(nil), items...)
	sort.SliceStable(sorted, func(i, j int) bool {
		a, c := sorted[i], sorted[j]
		if a.Pinned != c.Pinned {
			return a.Pinned
		}
		if a.ProjectKey != c.ProjectKey {
			return a.ProjectKey < c.ProjectKey
		}
		if a.SeenCount != c.SeenCount {
			return a.SeenCount > c.SeenCount
		}
		return a.ID < c.ID
	})
	for _, r := range sorted {
		b.WriteString(standingLine(kind, r) + "\n")
	}
}

func standingLine(kind string, c graph.Candidate) string {
	from := ""
	if c.ProjectKey != "" {
		from = " (from " + filepath.Base(c.ProjectKey) + ")"
	}
	return fmt.Sprintf("- [%s] %s — %s%s", kind, c.Title, strings.TrimSpace(c.Content), from)
}

// RuleLines renders rules one per line, byte-identical to their lines in a
// context block clipped the same way (ai-review's prep keeps the lines
// starting with "- [rule]" and used to scrape them from hook output).
func RuleLines(pk string, rules []graph.Candidate, maxContentChars int, now int64) []string {
	out := make([]string, len(rules))
	for i, r := range rules {
		out[i] = strings.TrimSuffix(renderLine(r, pk, maxContentChars, now), "\n")
	}
	return out
}

// FormatBlock renders the whole context block, with no size budget.
func FormatBlock(pk string, mems []Scored, rules []graph.Candidate, maxContentChars int, now int64) string {
	block, _, _ := BuildBlock(BlockInput{PK: pk, Mems: mems, Rules: rules, MaxContentChars: maxContentChars, Now: now})
	return block
}

type BlockInput struct {
	PK              string
	Mems            []Scored
	Rules           []graph.Candidate
	Matched         int
	MaxContentChars int
	MaxChars        int
	Now             int64
}

// footerReserve keeps room for the "… N more" line and the closing tag.
const footerReserve = 120

// BuildBlock renders the context block injected via additionalContext within
// maxChars (<= 0: no cap): memories in score order first, then rules with
// whatever room is left, and one line counting what did not fit. It returns
// the memories and rules actually written. Memories from other projects carry
// a "from <project>" marker.
func BuildBlock(in BlockInput) (string, []Scored, []graph.Candidate) {
	var b strings.Builder
	fmt.Fprintf(&b, "<infinite-memory project=%q>\n", in.PK)
	fits := func(line string) bool {
		return in.MaxChars <= 0 || b.Len()+len(line)+footerReserve <= in.MaxChars
	}
	shown := writeSection(&b, fits, "Long-term memories from previous sessions (background knowledge; verify before relying on it):\n",
		scoredLines(in), len(in.Mems))
	shownRules := writeSection(&b, fits, "Standing rules and conventions (follow these unless the user says otherwise):\n",
		ruleLines(in), len(in.Rules))
	writeFooter(&b, max(in.Matched, len(in.Mems))-shown, len(in.Rules)-shownRules)
	b.WriteString("</infinite-memory>")
	return b.String(), in.Mems[:shown], in.Rules[:shownRules]
}

func scoredLines(in BlockInput) []string {
	out := make([]string, len(in.Mems))
	for i, m := range in.Mems {
		out[i] = renderLine(m.Candidate, in.PK, in.MaxContentChars, in.Now)
	}
	return out
}

func ruleLines(in BlockInput) []string {
	out := make([]string, len(in.Rules))
	for i, r := range in.Rules {
		out[i] = renderLine(r, in.PK, in.MaxContentChars, in.Now)
	}
	return out
}

func writeSection(b *strings.Builder, fits func(string) bool, header string, lines []string, n int) int {
	if n == 0 || !fits(header) {
		return 0
	}
	start := b.Len()
	b.WriteString(header)
	shown := 0
	for _, line := range lines {
		if !fits(line) {
			break
		}
		b.WriteString(line)
		shown++
	}
	if shown == 0 {
		s := b.String()[:start]
		b.Reset()
		b.WriteString(s)
	}
	return shown
}

func writeFooter(b *strings.Builder, mems, rules int) {
	var more []string
	if mems > 0 {
		more = append(more, fmt.Sprintf("%d more matched memories", mems))
	}
	if rules > 0 {
		more = append(more, fmt.Sprintf("%d more standing rules", rules))
	}
	if len(more) > 0 {
		fmt.Fprintf(b, "… %s not shown — call imem_search with offset to page through them\n", strings.Join(more, " and "))
	}
}

func renderLine(m graph.Candidate, pk string, maxContentChars int, now int64) string {
	content := strings.TrimSpace(m.Content)
	if maxContentChars > 0 && len(content) > maxContentChars {
		content = content[:maxContentChars] + "…"
	}
	meta := relAge(now, m.LastSeen)
	if m.ProjectKey != "" && m.ProjectKey != pk {
		meta += ", from " + filepath.Base(m.ProjectKey)
	}
	return fmt.Sprintf("- [%s] %s — %s (%s)\n", m.Kind, m.Title, content, meta)
}

// summaryTitleChars caps the title column of the CLI summary.
const summaryTitleChars = 48

// FormatSummary renders the compact body shown to the user in the Claude Code
// CLI: one line per scored memory, standing rules only counted. maxLines caps
// the memory lines (negative means no cap) and everything left over collapses
// into a trailing "… N more · M standing rules" line. Returns "" when there
// is nothing at all.
func FormatSummary(pk string, mems []Scored, rules []graph.Candidate, maxLines int, now int64) string {
	if len(mems) == 0 && len(rules) == 0 {
		return ""
	}
	shown := mems
	if maxLines >= 0 && len(shown) > maxLines {
		shown = shown[:maxLines]
	}

	var lines []string
	for _, m := range shown {
		from := ""
		if m.ProjectKey != "" && m.ProjectKey != pk {
			from = "  ↖" + filepath.Base(m.ProjectKey)
		}
		line := fmt.Sprintf("  %-12s %-*s %8s  %5.1f%s",
			"["+m.Kind+"]", summaryTitleChars, truncRunes(m.Title, summaryTitleChars),
			relAge(now, m.LastSeen), m.Score, from)
		lines = append(lines, strings.TrimRight(line, " "))
	}

	var tail []string
	if rest := len(mems) - len(shown); rest > 0 {
		tail = append(tail, fmt.Sprintf("… %d more", rest))
	}
	if len(rules) > 0 {
		tail = append(tail, fmt.Sprintf("%d standing rules", len(rules)))
	}
	if len(tail) > 0 {
		lines = append(lines, "  "+strings.Join(tail, " · "))
	}
	return strings.Join(lines, "\n")
}

// truncRunes shortens s to max runes (never mid-rune), ellipsis included.
func truncRunes(s string, max int) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) <= max || max <= 0 {
		return s
	}
	if max == 1 {
		return "…"
	}
	return strings.TrimRight(string(r[:max-1]), " ") + "…"
}

func relAge(now, ts int64) string {
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

// SavedLine is one memory the extractor wrote. Deliberately minimal: the save
// side has no score and no age, but the columns line up with FormatSummary so
// the two blocks stack cleanly in the CLI.
type SavedLine struct {
	Title string
	Kind  string
	New   bool
	Seen  int64 // seen_count after the write; shown only when !New
	// Updated: the memory replaced an older one it was reconciled against.
	Updated bool
}

// FormatSaved renders the save-side counterpart of FormatSummary: one line per
// written memory, with new/seen-again where retrieve shows age and score.
// maxLines caps the lines (negative means no cap) and the remainder collapses
// into a trailing "… N more". Returns "" when there is nothing.
func FormatSaved(lines []SavedLine, maxLines int) string {
	if len(lines) == 0 {
		return ""
	}
	shown := lines
	if maxLines >= 0 && len(shown) > maxLines {
		shown = shown[:maxLines]
	}

	var out []string
	for _, l := range shown {
		state := "new"
		switch {
		case l.Updated:
			state = "replaces older"
		case !l.New:
			state = fmt.Sprintf("seen %dx", l.Seen)
		}
		line := fmt.Sprintf("  %-12s %-*s  %s",
			"["+l.Kind+"]", summaryTitleChars, truncRunes(l.Title, summaryTitleChars), state)
		out = append(out, strings.TrimRight(line, " "))
	}
	if rest := len(lines) - len(shown); rest > 0 {
		out = append(out, fmt.Sprintf("  … %d more", rest))
	}
	return strings.Join(out, "\n")
}
