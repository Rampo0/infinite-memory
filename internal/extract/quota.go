package extract

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// Quota is the subscription usage Claude Code reports: utilization 0..1 of
// the 5-hour and 7-day windows. The ai-review and on-call agents pause their
// queues at 60% of the 5-hour window, so bulk imem jobs stop well before.
type Quota struct {
	FiveHour       float64
	FiveHourResets int64 // unix seconds
	SevenDay       float64
}

// ParseQuota reads the rate_limit_event of a stream-json transcript.
func ParseQuota(stream []byte) (Quota, error) {
	sc := bufio.NewScanner(bytes.NewReader(stream))
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		var ev struct {
			Type string `json:"type"`
			Info struct {
				Windows map[string]struct {
					Utilization float64 `json:"utilization"`
					ResetsAt    int64   `json:"resetsAt"`
				} `json:"unifiedWindows"`
			} `json:"rate_limit_info"`
		}
		if json.Unmarshal(sc.Bytes(), &ev) != nil || !strings.Contains(ev.Type, "rate_limit") {
			continue
		}
		five, ok := ev.Info.Windows["five_hour"]
		if !ok {
			continue
		}
		return Quota{FiveHour: five.Utilization, FiveHourResets: five.ResetsAt,
			SevenDay: ev.Info.Windows["seven_day"].Utilization}, nil
	}
	return Quota{}, fmt.Errorf("no rate_limit_event in the stream")
}

// Quota probes the current usage with one tiny isolated call (a few tokens
// of the cheapest model) and reads its rate_limit_event.
func (r *Runner) Quota(ctx context.Context, model string) (Quota, error) {
	bin, err := r.resolveBin()
	if err != nil {
		return Quota{}, err
	}
	if err := os.MkdirAll(r.SpawnDir, 0o755); err != nil {
		return Quota{}, err
	}
	cmd := exec.CommandContext(ctx, bin, "-p", "--output-format", "stream-json", "--verbose",
		"--model", model, "--settings", `{"disableAllHooks": true}`, "--no-session-persistence",
		"--safe-mode", "--strict-mcp-config", "--setting-sources", "", "--tools", "")
	cmd.Dir = r.SpawnDir
	cmd.Stdin = strings.NewReader("Reply with the single word ok.")
	cmd.Env = append(os.Environ(), "INFINITE_MEMORY_INTERNAL=1")
	out, err := cmd.Output()
	if err != nil {
		return Quota{}, fmt.Errorf("quota probe: %w", err)
	}
	return ParseQuota(out)
}
