package daemon

import (
	"context"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Rampo0/infinite-memory/internal/expand"
	"github.com/Rampo0/infinite-memory/internal/extract"
)

// Vocabulary sizes handed to the expander. Same shape as the extraction
// prompt's canonicalization hints (see Worker.process), for the same reason:
// the model can only point at names the index actually holds.
const (
	vocabProject  = 30
	vocabGlobal   = 20
	vocabProjects = 25
	vocabTTL      = time.Minute
)

// vocabCache memoizes the index vocabulary. Expansion already costs seconds,
// so this is not about the graph round trip being slow — it is about three
// Stats() aggregations not running once per prompt.
type vocabCache struct {
	mu   sync.Mutex
	byPK map[string]vocabEntry
	now  func() time.Time // injectable for tests
}

type vocabEntry struct {
	v  expand.Vocab
	at time.Time
}

func newVocabCache() *vocabCache {
	return &vocabCache{byPK: map[string]vocabEntry{}, now: time.Now}
}

func (c *vocabCache) get(pk string) (expand.Vocab, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.byPK[pk]
	if !ok || c.now().Sub(e.at) > vocabTTL {
		return expand.Vocab{}, false
	}
	return e.v, true
}

func (c *vocabCache) put(pk string, v expand.Vocab) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.byPK[pk] = vocabEntry{v: v, at: c.now()}
}

// vocabFor reads the index's own vocabulary. Entity names are deduped by
// lowercase because Entity.key is project-namespaced: the same topic exists as
// one node per project, so "account-remisier" would otherwise appear five
// times and crowd out everything else.
func (s *server) vocabFor(ctx context.Context, pk string) expand.Vocab {
	if v, ok := s.vocab.get(pk); ok {
		return v
	}
	var v expand.Vocab
	seen := map[string]bool{}
	addNames := func(names []string) {
		for _, n := range names {
			lc := strings.ToLower(strings.TrimSpace(n))
			if lc == "" || seen[lc] {
				continue
			}
			seen[lc] = true
			v.Entities = append(v.Entities, n)
		}
	}
	if pk != "" {
		if names, err := s.store.EntityNames(ctx, pk, vocabProject); err == nil {
			addNames(names)
		}
	}
	if names, err := s.store.EntityNamesGlobal(ctx, vocabGlobal); err == nil {
		addNames(names)
	}
	// Project basenames, busiest first. The full keys are noise — a third of
	// them are .superset worktree paths — but the basenames are how the user
	// names the repo.
	if stats, err := s.store.Stats(ctx); err == nil {
		sort.Slice(stats, func(i, j int) bool { return stats[i].Memories > stats[j].Memories })
		pseen := map[string]bool{filepath.Base(pk): true}
		for _, st := range stats {
			if len(v.Projects) >= vocabProjects {
				break
			}
			base := filepath.Base(st.Key)
			if st.Memories == 0 || base == "" || base == "." || pseen[base] {
				continue
			}
			pseen[base] = true
			v.Projects = append(v.Projects, base)
		}
	}
	s.vocab.put(pk, v)
	return v
}

// newExpander wires the expander to the same headless-claude spawn extraction
// uses, in its isolated shape: no MCP servers, no plugins, no tool definitions
// and no default system prompt, which is both far cheaper and far safer for a
// call whose input is untrusted user text.
func (s *server) newExpander(runner *extract.Runner) *expand.Expander {
	return &expand.Expander{
		Run: func(ctx context.Context, prompt, schema, sys string) (string, error) {
			return runner.RunSchema(ctx, extract.Request{
				Prompt: prompt, Schema: schema, SystemPrompt: sys,
				Append: false, Model: s.cfg.ExpandModel, Isolated: true, Effort: "low",
			})
		},
		Vocab: s.vocabFor,
		Model: s.cfg.ExpandModel,
		Log:   s.log,
	}
}

// expandFor runs the expansion under its OWN deadline. It must be called
// before the graph budget starts: a spawn takes seconds where the graph takes
// milliseconds, so sharing one deadline would guarantee both fail.
func (s *server) expandFor(ctx context.Context, pk, prompt string) expand.Result {
	if !s.cfg.ExpandEnabled || s.expander == nil || strings.TrimSpace(prompt) == "" {
		return expand.Result{}
	}
	ctx, cancel := context.WithTimeout(ctx, s.cfg.ExpandBudget())
	defer cancel()
	return s.expander.Expand(ctx, pk, prompt)
}
