// Package metube enqueues downloads on a MeTube instance.
package metube

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Client posts download requests to MeTube's /add endpoint.
type Client struct {
	baseURL string
	quality string
	format  string
	folder  string
	http    *http.Client
}

// New builds a client. quality and format are passed straight through to
// MeTube; "best" and "mp3" are the usual choices for an audio library.
func New(baseURL, quality, format, folder string, timeout time.Duration) *Client {
	return &Client{
		baseURL: baseURL,
		quality: quality,
		format:  format,
		folder:  folder,
		http:    &http.Client{Timeout: timeout},
	}
}

type addRequest struct {
	URL       string `json:"url"`
	Quality   string `json:"quality"`
	Format    string `json:"format"`
	Folder    string `json:"folder"`
	AutoStart bool   `json:"auto_start"`
}

type addResponse struct {
	Status string `json:"status"`
	Msg    string `json:"msg"`
}

// Where says which of MeTube's lists an entry was found in.
type Where string

const (
	// InQueue: downloading, or waiting for its turn.
	InQueue Where = "queue"
	// Finished: MeTube completed the download.
	Finished Where = "finished"
	// Errored: MeTube gave up; Message holds why.
	Errored Where = "error"
)

// Entry is one download as MeTube reports it.
type Entry struct {
	URL      string
	Where    Where
	Message  string
	Filename string
}

// History is a snapshot of MeTube's queue and completed list.
type History []Entry

type historyItem struct {
	ID       string `json:"id"`
	URL      string `json:"url"`
	Status   string `json:"status"`
	Msg      string `json:"msg"`
	Error    string `json:"error"`
	Filename string `json:"filename"`
}

type historyResponse struct {
	Done    []historyItem `json:"done"`
	Queue   []historyItem `json:"queue"`
	Pending []historyItem `json:"pending"`
}

// History fetches MeTube's current queue and completed list in one request.
func (c *Client) History(ctx context.Context) (History, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/history", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("metube history: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("metube history: unexpected status %s", resp.Status)
	}

	var raw historyResponse
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, fmt.Errorf("metube history: decoding response: %w", err)
	}

	var out History
	for _, it := range raw.Queue {
		out = append(out, Entry{URL: it.URL, Where: InQueue})
	}
	for _, it := range raw.Pending {
		out = append(out, Entry{URL: it.URL, Where: InQueue})
	}
	for _, it := range raw.Done {
		e := Entry{URL: it.URL, Filename: it.Filename, Where: Finished}
		if it.Status == "error" {
			e.Where = Errored
			e.Message = it.Msg
			if e.Message == "" {
				e.Message = it.Error
			}
			if e.Message == "" {
				e.Message = "MeTube reported an error without saying why"
			}
		}
		out = append(out, e)
	}
	return out, nil
}

// Find returns what MeTube knows about a YouTube video. When several entries
// exist — a failure followed by a successful retry, say — a finished one wins,
// then one still in progress, then the failure.
func (h History) Find(videoID string) (Entry, bool) {
	var best Entry
	rank := 0
	for _, e := range h {
		if !strings.Contains(e.URL, videoID) {
			continue
		}
		r := map[Where]int{Errored: 1, InQueue: 2, Finished: 3}[e.Where]
		if r >= rank {
			best, rank = e, r
		}
	}
	return best, rank > 0
}

// Add enqueues a URL for download. It returns an error when MeTube rejects the
// request, so the caller never records a pending download that never started.
func (c *Client) Add(ctx context.Context, rawURL string) error {
	body, err := json.Marshal(addRequest{
		URL:       rawURL,
		Quality:   c.quality,
		Format:    c.format,
		Folder:    c.folder,
		AutoStart: true,
	})
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/add", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("metube add: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("metube add: unexpected status %s", resp.Status)
	}

	var out addResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return fmt.Errorf("metube add: decoding response: %w", err)
	}
	if out.Status != "ok" {
		return fmt.Errorf("metube rejected the download: %s", out.Msg)
	}
	return nil
}
