package youtube

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func previewProber(handler http.HandlerFunc) (*Prober, func()) {
	srv := httptest.NewServer(handler)
	p := NewProber("yt-dlp", time.Second)
	p.oembedURL = srv.URL
	return p, srv.Close
}

func TestPreviewReturnsTitleForVideoAndPlaylist(t *testing.T) {
	var asked []string
	p, closeSrv := previewProber(func(w http.ResponseWriter, r *http.Request) {
		target := r.URL.Query().Get("url")
		asked = append(asked, target)
		if target == "https://www.youtube.com/playlist?list=PL123" {
			w.Write([]byte(`{"title":"Un Verano Sin Ti","author_name":"Bad Bunny"}`))
			return
		}
		w.Write([]byte(`{"title":"Titi Me Pregunto","author_name":"Bad Bunny"}`))
	})
	defer closeSrv()

	got, err := p.Preview(context.Background(), "https://www.youtube.com/watch?v=abc")
	if err != nil || got != "Titi Me Pregunto" {
		t.Fatalf("video: Preview() = %q, %v", got, err)
	}
	got, err = p.Preview(context.Background(), "https://www.youtube.com/playlist?list=PL123")
	if err != nil || got != "Un Verano Sin Ti" {
		t.Fatalf("playlist: Preview() = %q, %v", got, err)
	}
	if len(asked) != 2 {
		t.Fatalf("oEmbed asked %d times, want 2: %v", len(asked), asked)
	}
}

func TestPreviewFailsInsteadOfWaiting(t *testing.T) {
	p, closeSrv := previewProber(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(previewTimeout * 3):
		}
	})
	defer closeSrv()

	start := time.Now()
	if _, err := p.Preview(context.Background(), "https://www.youtube.com/watch?v=abc"); err == nil {
		t.Fatal("Preview() returned no error for a server that never answers")
	}
	if took := time.Since(start); took > previewTimeout+2*time.Second {
		t.Fatalf("Preview() waited %v, want it bounded by %v", took, previewTimeout)
	}
}

func TestPreviewErrors(t *testing.T) {
	for name, handler := range map[string]http.HandlerFunc{
		"not found":   func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) },
		"bad json":    func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("nope")) },
		"empty title": func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte(`{"title":""}`)) },
	} {
		t.Run(name, func(t *testing.T) {
			p, closeSrv := previewProber(handler)
			defer closeSrv()
			if got, err := p.Preview(context.Background(), "https://www.youtube.com/watch?v=abc"); err == nil {
				t.Fatalf("Preview() = %q, want an error", got)
			}
		})
	}
}

// The fallback probe shares the same code path; it must keep working.
func TestProbeOEmbedStillWorks(t *testing.T) {
	p, closeSrv := previewProber(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"title":"Song","author_name":"Band"}`))
	})
	defer closeSrv()

	meta, err := p.probeOEmbed(context.Background(), "https://www.youtube.com/watch?v=kSx7OL5-w2E")
	if err != nil || meta.Title != "Song" || meta.Artist != "Band" || !meta.Degraded || meta.ID != "kSx7OL5-w2E" {
		t.Fatalf("probeOEmbed() = %+v, %v", meta, err)
	}
}
