package navidrome

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

// songsServer serves total fake songs through search3, honouring paging.
func songsServer(t *testing.T, total int) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		count, _ := strconv.Atoi(q.Get("songCount"))
		offset, _ := strconv.Atoi(q.Get("songOffset"))

		songs := ""
		for i := offset; i < offset+count && i < total; i++ {
			if songs != "" {
				songs += ","
			}
			songs += fmt.Sprintf(`{"id":"s%d","title":"t%d"}`, i, i)
		}
		fmt.Fprintf(w, `{"subsonic-response":{"status":"ok","searchResult3":{"song":[%s]}}}`, songs)
	}))
	t.Cleanup(srv.Close)
	return New(srv.URL, "u", "p", 5*time.Second)
}

func TestAllSongsReadsEveryPage(t *testing.T) {
	// 1212 mirrors the real library; 500 per page means three requests.
	for _, total := range []int{0, 1, 499, 500, 501, 1212} {
		songs, err := songsServer(t, total).AllSongs(context.Background(), 500)
		if err != nil {
			t.Fatalf("total=%d: %v", total, err)
		}
		if len(songs) != total {
			t.Errorf("total=%d: got %d songs", total, len(songs))
		}
		seen := map[string]bool{}
		for _, s := range songs {
			if seen[s.ID] {
				t.Fatalf("total=%d: song %s returned twice", total, s.ID)
			}
			seen[s.ID] = true
		}
	}
}

func TestAllSongsReportsServerErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"subsonic-response":{"status":"failed","error":{"code":40,"message":"wrong credentials"}}}`)
	}))
	defer srv.Close()

	if _, err := New(srv.URL, "u", "p", time.Second).AllSongs(context.Background(), 500); err == nil {
		t.Fatal("AllSongs() returned no error for a failed response")
	}
}
