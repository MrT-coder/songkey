package youtube

import "testing"

// TestExtractIDAcceptsEveryLinkShape covers the URL forms a phone share menu
// actually produces.
func TestExtractIDAcceptsEveryLinkShape(t *testing.T) {
	const want = "kSx7OL5-w2E"

	cases := []struct {
		name string
		url  string
	}{
		{"share link with tracking", "https://youtu.be/kSx7OL5-w2E?si=XxrwWxDW3ZUBev5n"},
		{"watch link", "https://www.youtube.com/watch?v=kSx7OL5-w2E"},
		{"watch link with extra params", "https://www.youtube.com/watch?v=kSx7OL5-w2E&t=42s"},
		{"youtube music", "https://music.youtube.com/watch?v=kSx7OL5-w2E"},
		{"shorts", "https://www.youtube.com/shorts/kSx7OL5-w2E"},
		{"embed", "https://www.youtube.com/embed/kSx7OL5-w2E"},
		{"bare id", "kSx7OL5-w2E"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := ExtractID(c.url)
			if !ok || got != want {
				t.Errorf("ExtractID(%q) = (%q, %v), want (%q, true)", c.url, got, ok, want)
			}
		})
	}
}

// TestExtractIDOnVideoInsidePlaylist documents deliberate behaviour: sharing a
// song while a playlist is open produces a link carrying both ids, and only the
// song is wanted. The playlist is dropped.
func TestExtractIDOnVideoInsidePlaylist(t *testing.T) {
	url := "https://www.youtube.com/watch?v=kSx7OL5-w2E&list=PLBCF2DAC6FFB574DE&index=3"

	got, ok := ExtractID(url)
	if !ok {
		t.Fatal("a video shared from inside a playlist must still resolve")
	}
	if got != "kSx7OL5-w2E" {
		t.Errorf("ExtractID() = %q, want the video id, not the playlist id", got)
	}
}

// TestExtractPlaylistIDSeparatesIntentions guards the branch that decides
// between downloading one song and downloading a whole playlist.
//
// Getting this wrong in either direction is expensive: a whole album queued
// when one song was wanted, or one song queued when an album was.
func TestExtractPlaylistIDSeparatesIntentions(t *testing.T) {
	t.Run("bare playlist means the whole playlist", func(t *testing.T) {
		id, ok := ExtractPlaylistID("https://www.youtube.com/playlist?list=PLBCF2DAC6FFB574DE")
		if !ok || id != "PLBCF2DAC6FFB574DE" {
			t.Errorf("ExtractPlaylistID() = (%q, %v), want the playlist id", id, ok)
		}
	})

	t.Run("song shared from an open playlist means that song", func(t *testing.T) {
		url := "https://www.youtube.com/watch?v=kSx7OL5-w2E&list=PLBCF2DAC6FFB574DE&index=3"
		if id, ok := ExtractPlaylistID(url); ok {
			t.Errorf("ExtractPlaylistID(%q) = %q, but a link naming a video must be treated as one song", url, id)
		}
		if id, ok := ExtractID(url); !ok || id != "kSx7OL5-w2E" {
			t.Errorf("ExtractID() = (%q, %v), want the video id", id, ok)
		}
	})

	t.Run("album playlist on youtube music", func(t *testing.T) {
		id, ok := ExtractPlaylistID("https://music.youtube.com/playlist?list=OLAK5uy_kBUCbfCFT3rBpxwsDbcMHbYRDvL6BZfPQ")
		if !ok || id != "OLAK5uy_kBUCbfCFT3rBpxwsDbcMHbYRDvL6BZfPQ" {
			t.Errorf("ExtractPlaylistID() = (%q, %v), want the album id", id, ok)
		}
	})

	// A real link produced by the YouTube share sheet, tracking parameter and
	// all. This is the shape that actually arrives from a phone.
	t.Run("shared album link with tracking", func(t *testing.T) {
		url := "https://youtube.com/playlist?list=OLAK5uy_nSfjYPAVpid5DkvObz5Ff9GRr-3fLQq8w&si=iVBCElIxrqTAd3Z8"
		id, ok := ExtractPlaylistID(url)
		if !ok || id != "OLAK5uy_nSfjYPAVpid5DkvObz5Ff9GRr-3fLQq8w" {
			t.Errorf("ExtractPlaylistID() = (%q, %v), want the album id", id, ok)
		}
	})

	t.Run("plain song is not a playlist", func(t *testing.T) {
		if id, ok := ExtractPlaylistID("https://youtu.be/kSx7OL5-w2E?si=XxrwWxDW3ZUBev5n"); ok {
			t.Errorf("ExtractPlaylistID() = %q, want no playlist", id)
		}
	})
}

// TestExtractIDRejectsPlaylistOnly records what happens with a bare playlist
// link: there is no single video to check for duplicates, so it is refused
// rather than silently downloading something unexpected.
func TestExtractIDRejectsPlaylistOnly(t *testing.T) {
	urls := []string{
		"https://www.youtube.com/playlist?list=PLBCF2DAC6FFB574DE",
		"https://music.youtube.com/playlist?list=OLAK5uy_kBUCbfCFT3rBpxwsDbcMHbYRDvL6BZfPQ",
		"https://www.youtube.com/@SomeArtist",
		"https://www.youtube.com/@SomeArtist/videos",
	}

	for _, url := range urls {
		if got, ok := ExtractID(url); ok {
			t.Errorf("ExtractID(%q) = %q, but no single video id should be found", url, got)
		}
	}
}
