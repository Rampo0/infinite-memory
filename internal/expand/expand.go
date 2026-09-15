// Package expand turns a short or vague user prompt into extra retrieval terms
// by asking headless claude, the same way internal/extract asks it for
// memories. It sits between tokenization and the graph queries and adds only
// terms: the search itself, its Cypher and its scoring are untouched.
//
// Every failure path returns no terms, which reproduces unexpanded retrieval
// exactly. The caller can therefore treat expansion as best-effort.
package expand

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/Rampo0/infinite-memory/internal/textutil"
)

// Result is one expansion. Added is empty whenever the call was skipped or
// failed; Intent is the model's one-line reading of the prompt, shown to the
// user so a bad expansion is visible the turn it happens.
type Result struct {
	Added  []string
	Intent string
	MS     int64
	// Err records why an expansion produced nothing. A timeout and a model
	// that genuinely had nothing to add both yield zero terms, and telling
	// them apart is the difference between "working" and "quietly dead".
	Err error
}

// TimedOut reports whether the expansion ran out of budget rather than
// finishing. Callers show this differently: it means raise the budget, not
// that the prompt was unexpandable.
func (r Result) TimedOut() bool { return errors.Is(r.Err, context.DeadlineExceeded) }

// Expander holds the two things it cannot do itself. Both are func fields
// rather than interfaces: the repo has no interfaces, and this is the same
// injection shape as NewQueue's process func. Tests supply canned values and
// never spawn claude.
type Expander struct {
	// Run executes one headless claude call and returns its raw result text.
	Run func(ctx context.Context, prompt, schema, systemPrompt string) (string, error)
	// Vocab supplies the index's own vocabulary. The caller owns any caching,
	// so Expand does no I/O beyond the spawn.
	Vocab func(ctx context.Context, pk string) Vocab
	Model string // for logging only
	Log   *slog.Logger
}

// Expand asks the model for extra search terms. It never returns an error:
// a timeout, a spawn failure, garbage output or an empty answer all yield a
// zero Result, and the caller then searches exactly as it does today.
//
// The caller owns the context deadline, and it must NOT be the graph's
// retrieval budget — a spawn takes seconds where the graph takes milliseconds.
func (e *Expander) Expand(ctx context.Context, pk, prompt string) Result {
	if e == nil || e.Run == nil {
		return Result{}
	}
	start := time.Now()

	var vocab Vocab
	if e.Vocab != nil {
		vocab = e.Vocab(ctx, pk)
	}
	raw, err := e.Run(ctx, BuildPrompt(prompt, pk, vocab), ExpansionSchema, systemPrompt)
	ms := time.Since(start).Milliseconds()
	if err != nil {
		// A killed spawn reports as a signal, not as a deadline, so ask the
		// context what really happened before blaming the binary.
		if ctxErr := ctx.Err(); ctxErr != nil {
			err = fmt.Errorf("%w after %dms: %v", context.DeadlineExceeded, ms, err)
		}
		e.logf("expand failed", "err", err, "ms", ms)
		return Result{MS: ms, Err: err}
	}
	// Tokenized the same way the search side will tokenize it, so a "term"
	// the user already typed is recognised as such and dropped.
	intent, terms, err := Parse(raw, textutil.Tokenize(prompt, 24), vocab.nameSet())
	if err != nil {
		e.logf("expand unparseable", "err", err, "ms", ms)
		return Result{MS: ms, Err: err}
	}
	return Result{Added: terms, Intent: intent, MS: ms}
}

// Merge returns tokens plus added, without aliasing either input.
func Merge(tokens, added []string) []string {
	if len(added) == 0 {
		return tokens
	}
	out := make([]string, 0, len(tokens)+len(added))
	out = append(out, tokens...)
	return append(out, added...)
}

func (e *Expander) logf(msg string, args ...any) {
	if e.Log != nil {
		e.Log.Warn(msg, args...)
	}
}
