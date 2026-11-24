package metube

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

const historyBody = `{
  "done": [
    {"url": "https://www.youtube.com/watch?v=failed1", "status": "error", "msg": "Sign in to confirm you're not a bot"},
    {"url": "https://www.youtube.com/watch?v=ok1", "status": "finished", "filename": "Song.mp3"},
    {"url": "https://www.youtube.com/watch?v=failed2", "status": "error", "msg": "", "error": "Video unavailable"}
  ],
  "queue": [{"url": "https://www.youtube.com/watch?v=busy1", "status": "downloading"}],
  "pending": [{"url": "https://www.youtube.com/watch?v=wait1", "status": "pending"}]
}`

func historyServer(t *testing.T, status int, body string) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/history" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return New(srv.URL, "best", "mp3", "", 5*time.Second)
}

func TestHistoryFind(t *testing.T) {
	h, err := historyServer(t, http.StatusOK, historyBody).History(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		video   string
		where   Where
		message string
		file    string
	}{
		{"failed1", Errored, "Sign in to confirm you're not a bot", ""},
		{"failed2", Errored, "Video unavailable", ""}, // falls back to the error field
		{"ok1", Finished, "", "Song.mp3"},
		{"busy1", InQueue, "", ""},
		{"wait1", InQueue, "", ""},
	}
	for _, tc := range tests {
		got, ok := h.Find(tc.video)
		if !ok || got.Where != tc.where || got.Message != tc.message || got.Filename != tc.file {
			t.Errorf("Find(%q) = %+v, %v; want %s %q %q", tc.video, got, ok, tc.where, tc.message, tc.file)
		}
	}

	if _, ok := h.Find("unknown"); ok {
		t.Error("Find(unknown) found an entry")
	}
}

func TestFindPrefersSuccessOverEarlierFailure(t *testing.T) {
	h := History{
		{URL: "https://www.youtube.com/watch?v=x", Where: Errored, Message: "boom"},
		{URL: "https://www.youtube.com/watch?v=x", Where: Finished, Filename: "x.mp3"},
	}
	if got, _ := h.Find("x"); got.Where != Finished {
		t.Fatalf("Find() = %+v, want the finished retry to win", got)
	}
}

func TestErrorWithoutMessageStillSaysSomething(t *testing.T) {
	body := `{"done":[{"url":"https://www.youtube.com/watch?v=z","status":"error"}],"queue":[],"pending":[]}`
	h, err := historyServer(t, http.StatusOK, body).History(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := h.Find("z"); got.Message == "" {
		t.Fatal("errored entry has an empty message")
	}
}

func TestHistoryErrors(t *testing.T) {
	if _, err := historyServer(t, http.StatusInternalServerError, "").History(context.Background()); err == nil {
		t.Error("History() with a 500 returned no error")
	}
	if _, err := historyServer(t, http.StatusOK, "not json").History(context.Background()); err == nil {
		t.Error("History() with a bad body returned no error")
	}
}
