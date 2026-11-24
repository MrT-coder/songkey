package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MrT-coder/songkey/internal/catalog"
	"github.com/MrT-coder/songkey/internal/notify"
)

// jsonServer is a server with an empty catalog and no backends, enough for the
// handler paths that never reach Navidrome, MeTube or YouTube.
func jsonServer(t *testing.T, token string) *server {
	t.Helper()
	cat, err := catalog.Load(filepath.Join(t.TempDir(), "catalog.json"))
	if err != nil {
		t.Fatal(err)
	}
	s := &server{
		cat:      cat,
		notifier: notify.New(""),
		guard:    newAuthGuard(3, time.Minute, 10*time.Minute),
	}
	s.cfg.apiToken = token
	s.cfg.playlistName = "songkey"
	s.playlistID = "pl-1"
	return s
}

func do(s *server, handler func(http.ResponseWriter, *http.Request), target string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, target, nil)
	w := httptest.NewRecorder()
	handler(w, r)
	return w
}

func decode(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("body is not JSON: %v\n%s", err, w.Body.String())
	}
	return out
}

func errorCode(t *testing.T, body map[string]any) string {
	t.Helper()
	e, ok := body["error"].(map[string]any)
	if !ok {
		t.Fatalf("no error object in %v", body)
	}
	if msg, _ := e["message"].(string); msg == "" {
		t.Errorf("error without message: %v", e)
	}
	code, _ := e["code"].(string)
	return code
}

func TestStatusJSONListsActiveAndRecentJobs(t *testing.T) {
	s := jsonServer(t, "")
	s.cat.Add(catalog.Track{VideoID: "old1", Title: "Old", Artist: "Band", Duration: 200})
	active := s.cat.AddPending(catalog.Pending{VideoID: "vid1", Title: "Song", Artist: "Band",
		Duration: 185, State: catalog.StateQueued})
	s.cat.AddPending(catalog.Pending{VideoID: "vid2", Title: "Gone", Artist: "Band"})
	s.cat.Finish("vid2", catalog.StateFailed, "MeTube rejected the download")

	w := do(s, s.handleStatus, "/status?format=json")
	if w.Code != http.StatusOK {
		t.Fatalf("status %d, want 200", w.Code)
	}
	body := decode(t, w)

	pl, _ := body["playlist"].(map[string]any)
	if pl["name"] != "songkey" || pl["id"] != "pl-1" {
		t.Errorf("playlist = %v", pl)
	}
	if body["indexed"] != float64(1) {
		t.Errorf("indexed = %v, want 1", body["indexed"])
	}
	act, _ := body["active"].([]any)
	if len(act) != 1 {
		t.Fatalf("active = %v, want one job", body["active"])
	}
	job := act[0].(map[string]any)
	if job["id"] != active.ID || job["state"] != "queued" || job["video_id"] != "vid1" ||
		job["title"] != "Song" || job["artist"] != "Band" || job["duration"] != float64(185) {
		t.Errorf("active job = %v", job)
	}
	recent, _ := body["recent"].([]any)
	if len(recent) != 1 {
		t.Fatalf("recent = %v, want one job", body["recent"])
	}
	failed := recent[0].(map[string]any)
	if failed["state"] != "failed" || failed["reason"] != "MeTube rejected the download" {
		t.Errorf("recent job = %v", failed)
	}
}

func TestStatusJSONEmptyListsAreArrays(t *testing.T) {
	s := jsonServer(t, "")
	body := decode(t, do(s, s.handleStatus, "/status?format=json"))
	if a, ok := body["active"].([]any); !ok || len(a) != 0 {
		t.Errorf("active = %#v, want []", body["active"])
	}
	if r, ok := body["recent"].([]any); !ok || len(r) != 0 {
		t.Errorf("recent = %#v, want []", body["recent"])
	}
}

func TestStatusJSONSingleJob(t *testing.T) {
	s := jsonServer(t, "")
	job := s.cat.AddPending(catalog.Pending{VideoID: "vid1", Title: "Song", Artist: "Band"})

	w := do(s, s.handleStatus, "/status?format=json&job="+job.ID)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d, want 200", w.Code)
	}
	got, _ := decode(t, w)["job"].(map[string]any)
	if got["id"] != job.ID || got["video_id"] != "vid1" {
		t.Errorf("job = %v", got)
	}
	for _, key := range []string{"created", "updated", "attempts"} {
		if _, ok := got[key]; !ok {
			t.Errorf("job lacks %q: %v", key, got)
		}
	}

	w = do(s, s.handleStatus, "/status?format=json&job=nope")
	if w.Code != http.StatusNotFound {
		t.Fatalf("unknown job: status %d, want 404", w.Code)
	}
	if code := errorCode(t, decode(t, w)); code != "job_not_found" {
		t.Errorf("code = %q, want job_not_found", code)
	}
}

func TestAddJSONErrors(t *testing.T) {
	s := jsonServer(t, "")
	tests := []struct {
		name, target string
		status       int
		code         string
	}{
		{"missing url", "/add?format=json", http.StatusBadRequest, "missing_url"},
		{"not youtube", "/add?format=json&url=https://example.com/x", http.StatusBadRequest, "invalid_url"},
		{"channel", "/add?format=json&url=https://www.youtube.com/@someone", http.StatusBadRequest, "channel_not_supported"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := do(s, s.handleAdd, tc.target)
			if w.Code != tc.status {
				t.Fatalf("status %d, want %d", w.Code, tc.status)
			}
			if code := errorCode(t, decode(t, w)); code != tc.code {
				t.Errorf("code = %q, want %q", code, tc.code)
			}
		})
	}
}

func TestAddJSONAlreadyHave(t *testing.T) {
	s := jsonServer(t, "")
	s.cat.Add(catalog.Track{VideoID: "dQw4w9WgXcQ", Title: "Song", Artist: "Band", Duration: 212})

	w := do(s, s.handleAdd, "/add?format=json&url=https://youtu.be/dQw4w9WgXcQ")
	if w.Code != http.StatusConflict {
		t.Fatalf("status %d, want 409", w.Code)
	}
	body := decode(t, w)
	if body["result"] != "already_have" {
		t.Errorf("result = %v, want already_have", body["result"])
	}
	track, _ := body["track"].(map[string]any)
	if track["artist"] != "Band" || track["title"] != "Song" || track["video_id"] != "dQw4w9WgXcQ" {
		t.Errorf("track = %v", track)
	}
	if msg, _ := body["message"].(string); !strings.Contains(msg, "ALREADY HAVE IT") {
		t.Errorf("message = %q", msg)
	}
}

func TestAddJSONAlreadyQueued(t *testing.T) {
	s := jsonServer(t, "")
	job := s.cat.AddPending(catalog.Pending{VideoID: "dQw4w9WgXcQ", Title: "Song",
		State: catalog.StateQueued})

	w := do(s, s.handleAdd, "/add?format=json&url=https://youtu.be/dQw4w9WgXcQ")
	if w.Code != http.StatusOK {
		t.Fatalf("status %d, want 200", w.Code)
	}
	body := decode(t, w)
	if body["result"] != "already_queued" {
		t.Errorf("result = %v, want already_queued", body["result"])
	}
	got, _ := body["job"].(map[string]any)
	if got["id"] != job.ID || got["state"] != "queued" {
		t.Errorf("job = %v", got)
	}
}

func TestQueuedResponseJSON(t *testing.T) {
	job := catalog.Pending{ID: "abc123", VideoID: "vid1", Title: "Song", State: catalog.StateReceived}
	w := httptest.NewRecorder()
	replyQueued(w, true, job, "Song", true, true)

	if w.Code != http.StatusAccepted {
		t.Fatalf("status %d, want 202", w.Code)
	}
	body := decode(t, w)
	if body["result"] != "queued" || body["notify"] != true {
		t.Errorf("body = %v", body)
	}
	if got, _ := body["job"].(map[string]any); got["id"] != "abc123" || got["state"] != "received" {
		t.Errorf("job = %v", body["job"])
	}
}

func TestQueuedResponseText(t *testing.T) {
	job := catalog.Pending{ID: "abc123"}
	tests := []struct {
		name          string
		named, notify bool
		want          string
	}{
		{"named, notifications on", true, true,
			"QUEUED\nSong\nJob abc123\n\nYou will get a notification with the result.\n"},
		{"unnamed, notifications off", false, false,
			"QUEUED\nJob abc123\n\nCheck /status?job=abc123 for the result.\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			replyQueued(w, false, job, "Song", tc.named, tc.notify)
			if w.Code != http.StatusAccepted || w.Body.String() != tc.want {
				t.Errorf("got %d %q, want 202 %q", w.Code, w.Body.String(), tc.want)
			}
		})
	}
}

func TestUnauthorizedJSON(t *testing.T) {
	s := jsonServer(t, "secret")
	w := do(s, s.handleStatus, "/status?format=json&token=nope")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status %d, want 401", w.Code)
	}
	if code := errorCode(t, decode(t, w)); code != "unauthorized" {
		t.Errorf("code = %q, want unauthorized", code)
	}
}

// The phone shortcut reads plain text; nothing about it may change.
func TestTextRepliesUnchanged(t *testing.T) {
	s := jsonServer(t, "secret")
	s.cat.Add(catalog.Track{VideoID: "dQw4w9WgXcQ", Title: "Song", Artist: "Band"})

	tests := []struct {
		name, target string
		handler      func(http.ResponseWriter, *http.Request)
		status       int
		body         string
	}{
		{"unauthorized", "/status?token=nope", s.handleStatus, 401, "unauthorized\n"},
		{"missing url", "/add?token=secret", s.handleAdd, 400, "missing url parameter\n"},
		{"already have", "/add?token=secret&url=https://youtu.be/dQw4w9WgXcQ", s.handleAdd, 409,
			"ALREADY HAVE IT\nBand - Song\n\nAdd &force=1 to download anyway.\n"},
		{"unknown job", "/status?token=secret&job=nope", s.handleStatus, 404, "no job with that id\n"},
		{"other format", "/add?token=secret&format=xml", s.handleAdd, 400, "missing url parameter\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := do(s, tc.handler, tc.target)
			if w.Code != tc.status || w.Body.String() != tc.body {
				t.Errorf("got %d %q, want %d %q", w.Code, w.Body.String(), tc.status, tc.body)
			}
			if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
				t.Errorf("Content-Type = %q, want text/plain", ct)
			}
		})
	}
}
