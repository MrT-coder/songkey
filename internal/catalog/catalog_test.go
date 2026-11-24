package catalog

import "testing"

// TestKeyCollapsesYouTubeTitleNoise checks the core premise of the whole
// service: the same recording uploaded under different YouTube titles must
// reduce to the same key.
func TestKeyCollapsesYouTubeTitleNoise(t *testing.T) {
	want := Key("Bad Bunny", "Tití Me Preguntó")

	variants := []struct {
		artist string
		title  string
	}{
		{"Bad Bunny", "Tití Me Preguntó (Official Video)"},
		{"Bad Bunny - Topic", "Titi Me Pregunto"},
		{"BadBunnyPR", "Bad Bunny - Tití Me Preguntó [HD] Lyrics"},
		{"Bad Bunny", "TITÍ ME PREGUNTÓ (Video Oficial) 4K"},
		{"Bad Bunny", "Tití Me Preguntó (Remastered 2023)"},
	}

	for _, v := range variants {
		if got := Key(v.artist, v.title); got != want {
			t.Errorf("Key(%q, %q) = %q, want %q", v.artist, v.title, got, want)
		}
	}
}

// TestFindDuplicateDetectsRenamedUpload verifies a differently titled upload of
// a track already in the library is reported as a duplicate.
func TestFindDuplicateDetectsRenamedUpload(t *testing.T) {
	c, err := Load(t.TempDir() + "/catalog.json")
	if err != nil {
		t.Fatal(err)
	}
	c.Add(Track{Title: "Tití Me Preguntó", Artist: "Bad Bunny", Duration: 243})

	match, dup := c.FindDuplicate("BadBunnyPR", "Bad Bunny - Titi Me Pregunto (Official Video) HD", 245)
	if !dup {
		t.Fatal("expected a renamed upload of the same song to be flagged")
	}
	if match.Track.Title != "Tití Me Preguntó" {
		t.Errorf("matched the wrong track: %q", match.Track.Title)
	}
}

// TestFindDuplicateAllowsDifferentSong guards the other direction: a genuinely
// different track must not be swallowed by the fuzzy match.
func TestFindDuplicateAllowsDifferentSong(t *testing.T) {
	c, err := Load(t.TempDir() + "/catalog.json")
	if err != nil {
		t.Fatal(err)
	}
	c.Add(Track{Title: "Tití Me Preguntó", Artist: "Bad Bunny", Duration: 243})

	if _, dup := c.FindDuplicate("Bad Bunny", "Moscow Mule", 245); dup {
		t.Error("a different song by the same artist must not be treated as a duplicate")
	}
}

// TestFindDuplicateRespectsDuration protects covers and live versions: same
// title, very different runtime, so it is not the same recording.
func TestFindDuplicateRespectsDuration(t *testing.T) {
	c, err := Load(t.TempDir() + "/catalog.json")
	if err != nil {
		t.Fatal(err)
	}
	c.Add(Track{Title: "Tití Me Preguntó", Artist: "Bad Bunny", Duration: 243})

	if _, dup := c.FindDuplicate("Bad Bunny", "Tití Me Preguntó (En Vivo)", 402); dup {
		t.Error("a version 2.5 minutes longer must not be treated as the same recording")
	}
}

// TestFindByVideoIsExact confirms the cheap path works, which is what keeps the
// common case free of any network or yt-dlp cost.
func TestFindByVideoIsExact(t *testing.T) {
	c, err := Load(t.TempDir() + "/catalog.json")
	if err != nil {
		t.Fatal(err)
	}
	c.Add(Track{VideoID: "Cr8K88UcO0s", Title: "Tití Me Preguntó", Artist: "Bad Bunny", Duration: 243})

	if _, found := c.FindByVideo("Cr8K88UcO0s"); !found {
		t.Error("a previously downloaded video id must be found")
	}
	if _, found := c.FindByVideo("aaaaaaaaaaa"); found {
		t.Error("an unknown video id must not match")
	}
}

// TestMatchPendingFindsFiledDownload covers the worker path that moves a
// finished download into the playlist.
func TestMatchPendingFindsFiledDownload(t *testing.T) {
	c, err := Load(t.TempDir() + "/catalog.json")
	if err != nil {
		t.Fatal(err)
	}
	c.AddPending(Pending{VideoID: "Cr8K88UcO0s", Title: "Tití Me Preguntó", Artist: "Bad Bunny", Duration: 243})

	// Navidrome reports the track from the file's tags, which may differ.
	id, ok := c.MatchPending("Bad Bunny", "Titi Me Pregunto", 243)
	if !ok || id != "Cr8K88UcO0s" {
		t.Fatalf("MatchPending() = (%q, %v), want the pending video id", id, ok)
	}

	c.ResolvePending(id, "song-42")
	if c.PendingCount() != 0 {
		t.Error("resolving must clear the pending entry")
	}
	if _, found := c.FindByVideo("Cr8K88UcO0s"); !found {
		t.Error("a resolved download must enter the index so it is never downloaded twice")
	}
}

// TestPersistenceSurvivesReload is what protects the index from being lost when
// the container is recreated.
func TestPersistenceSurvivesReload(t *testing.T) {
	path := t.TempDir() + "/catalog.json"

	first, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	first.Add(Track{VideoID: "Cr8K88UcO0s", Title: "Tití Me Preguntó", Artist: "Bad Bunny", Duration: 243})
	if err := first.Save(); err != nil {
		t.Fatal(err)
	}

	second, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if second.Size() != 1 {
		t.Fatalf("reloaded catalog has %d tracks, want 1", second.Size())
	}
	if _, found := second.FindByVideo("Cr8K88UcO0s"); !found {
		t.Error("video id lookup must survive a reload")
	}
}
