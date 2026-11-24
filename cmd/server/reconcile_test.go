package main

import (
	"testing"

	"github.com/MrT-coder/songkey/internal/navidrome"
)

func songs(ids ...string) []navidrome.Song {
	out := make([]navidrome.Song, len(ids))
	for i, id := range ids {
		out[i] = navidrome.Song{ID: id, Title: "title-" + id}
	}
	return out
}

func ids(list []navidrome.Song) []string {
	out := make([]string, len(list))
	for i, s := range list {
		out[i] = s.ID
	}
	return out
}

func TestMissingFromPlaylist(t *testing.T) {
	tests := []struct {
		name     string
		library  []navidrome.Song
		playlist []navidrome.Song
		want     []string
	}{
		{"nothing missing", songs("a", "b"), songs("a", "b"), nil},
		{"some missing, library order kept", songs("a", "b", "c", "d"), songs("b"), []string{"a", "c", "d"}},
		{"empty playlist", songs("a", "b"), nil, []string{"a", "b"}},
		{"empty library", nil, songs("a"), nil},
		{"playlist holds songs the library lacks", songs("a"), songs("a", "ghost"), nil},
		{"a song listed twice is added once", songs("a", "a", "b"), songs("b"), []string{"a"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ids(missingFromPlaylist(tc.library, tc.playlist))
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("got %v, want %v", got, tc.want)
				}
			}
		})
	}
}

func TestFindByFileMatchesLocalizedDownload(t *testing.T) {
	library := []navidrome.Song{
		{ID: "x", Title: "Other", Path: "nsqk/other.mp3"},
		// The tags and the file carry the Spanish title YouTube's oEmbed never reports.
		{ID: "ok", Title: "Atrapado y Libre", Path: "nsqk/nsqk - Atrapado y Libre (Visualizer).mp3"},
		{ID: "fw", Title: "A | B", Path: "a/Artist - A ｜ B？.mp3"},
	}
	tests := []struct {
		name, file, want string
	}{
		{"same file, different folder", "nsqk - Atrapado y Libre (Visualizer).mp3", "ok"},
		{"full-width characters on one side", "Artist - A | B?.mp3", "fw"},
		{"extension differs", "nsqk - Atrapado y Libre (Visualizer).opus", "ok"},
		{"not in the library", "nsqk - Nada.mp3", ""},
		{"empty filename never matches", "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := findByFile(library, tc.file)
			if ok != (tc.want != "") || got != tc.want {
				t.Fatalf("findByFile(%q) = %q, %v; want %q", tc.file, got, ok, tc.want)
			}
		})
	}
}
