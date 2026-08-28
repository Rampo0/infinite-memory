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
}

type ExtractRequest struct {
	SessionID      string `json:"session_id"`
	TranscriptPath string `json:"transcript_path"`
	CWD            string `json:"cwd"`
	Source         string `json:"source"`
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

func NotifyExtract(baseURL string, req ExtractRequest, timeout time.Duration) error {
	return post(baseURL, "/v1/extract", req, timeout, nil)
}
