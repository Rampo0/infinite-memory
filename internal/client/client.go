// Package client is the thin HTTP client hooks use to reach the daemon.
// Every call has a hard timeout and callers treat all errors as "no daemon" —
// hooks always fail open.
package client

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

type RetrieveRequest struct {
	CWD       string `json:"cwd"`
	Prompt    string `json:"prompt"`
	SessionID string `json:"session_id"`
}

type RetrieveResponse struct {
	Context string `json:"context"`
	Count   int    `json:"count"`
	// Summary is the compact per-memory listing the hook shows the user;
	// Memories/Rules split Count so the hook can label both sections.
	Summary  string `json:"summary"`
	Memories int    `json:"memories"`
	Rules    int    `json:"rules"`
	// Expanded is the pre-rendered query-expansion line, empty when the
	// expander did not run or added nothing.
	Expanded string `json:"expanded"`
	// Saved carries whatever the extractor wrote since the last report.
	Saved SavedPayload `json:"saved"`
}

// SavedPayload mirrors the daemon's wire shape. Duplicated rather than
// imported so the hook binary never pulls in the daemon package.
type SavedPayload struct {
	Summary string `json:"summary"`
	Count   int    `json:"count"`
	New     int    `json:"new"`
	Batches int    `json:"batches"`
	MS      int64  `json:"ms"`
	Status  string `json:"status"` // "" | "running" | "skipped"
	Error   string `json:"error"`
	DueInS  int    `json:"due_in_s"`
}

type ExtractRequest struct {
	SessionID      string `json:"session_id"`
	TranscriptPath string `json:"transcript_path"`
	CWD            string `json:"cwd"`
	Source         string `json:"source"`
	// BudgetMS applies to Flush only: how long the daemon may hold the
	// response before answering "running".
	BudgetMS int `json:"budget_ms,omitempty"`
}

type ExtractResponse struct {
	Queued bool         `json:"queued"`
	Saved  SavedPayload `json:"saved"`
}

type FlushResponse struct {
	OK    bool         `json:"ok"`
	Saved SavedPayload `json:"saved"`
}

func post(baseURL, path string, body any, timeout time.Duration, out any) error {
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	c := http.Client{Timeout: timeout}
	resp, err := c.Post(baseURL+path, "application/json", bytes.NewReader(data))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("%s: status %d", path, resp.StatusCode)
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

func Retrieve(baseURL string, req RetrieveRequest, timeout time.Duration) (RetrieveResponse, error) {
	var out RetrieveResponse
	err := post(baseURL, "/v1/retrieve", req, timeout, &out)
	return out, err
}

func NotifyExtract(baseURL string, req ExtractRequest, timeout time.Duration) (ExtractResponse, error) {
	var out ExtractResponse
	err := post(baseURL, "/v1/extract", req, timeout, &out)
	return out, err
}

// Flush runs extraction synchronously in the daemon and returns what it wrote.
// The caller's timeout must exceed req.BudgetMS, or the daemon's "running"
// answer is lost to a client-side cancel.
func Flush(baseURL string, req ExtractRequest, timeout time.Duration) (FlushResponse, error) {
	var out FlushResponse
	err := post(baseURL, "/v1/flush", req, timeout, &out)
	return out, err
}

type SessionStartRequest struct {
	SessionID string `json:"session_id"`
	CWD       string `json:"cwd"`
	// Source is Claude Code's SessionStart source: startup, resume, clear or
	// compact. The daemon forgets what the session was shown unless resume.
	Source string `json:"source"`
}

type SessionStartResponse struct {
	Context     string `json:"context"`
	Rules       int    `json:"rules"`
	Preferences int    `json:"preferences"`
	Omitted     int    `json:"omitted"`
}

func SessionStart(baseURL string, req SessionStartRequest, timeout time.Duration) (SessionStartResponse, error) {
	var out SessionStartResponse
	err := post(baseURL, "/v1/session-start", req, timeout, &out)
	return out, err
}
