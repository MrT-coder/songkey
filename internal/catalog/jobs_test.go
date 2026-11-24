package catalog

import (
	"os"
	"path/filepath"
	"testing"
)

func newTestCatalog(t *testing.T) (*Catalog, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "catalog.json")
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return c, path
}

func TestBumpAttemptFailsWithReasonInsteadOfDropping(t *testing.T) {
	c, _ := newTestCatalog(t)
	c.AddPending(Pending{VideoID: "vid1", Title: "Song", Artist: "Band"})

	if _, dropped := c.BumpAttempt("vid1", 2, "never indexed"); dropped {
		t.Fatal("dropped on the first attempt, want it kept")
	}
	job, dropped := c.BumpAttempt("vid1", 2, "never indexed")
	if !dropped || job.State != StateFailed || job.Reason != "never indexed" {
		t.Fatalf("BumpAttempt() = %+v, %v; want failed with its reason", job, dropped)
	}

	if c.PendingCount() != 0 {
		t.Fatal("failed job is still active")
	}
	found, ok := c.FindJob("vid1")
	if !ok || found.State != StateFailed || found.Reason != "never indexed" {
		t.Fatalf("failed job not kept in history: %+v, %v", found, ok)
	}
}

func TestFinishMovesJobToHistory(t *testing.T) {
	c, _ := newTestCatalog(t)
	added := c.AddPending(Pending{VideoID: "vid1", Title: "Song", State: StateReceived})
	if added.ID == "" || added.State != StateReceived {
		t.Fatalf("AddPending() = %+v, want an id and the given state", added)
	}

	done, ok := c.Finish("vid1", StateDuplicate, "same as X")
	if !ok || done.State != StateDuplicate {
		t.Fatalf("Finish() = %+v, %v", done, ok)
	}
	if _, active := c.Active("vid1"); active {
		t.Fatal("job still active after Finish")
	}
	if got := c.History(10); len(got) != 1 || got[0].Reason != "same as X" {
		t.Fatalf("History() = %+v", got)
	}
}

func TestFindJobByIDOrVideo(t *testing.T) {
	c, _ := newTestCatalog(t)
	job := c.AddPending(Pending{VideoID: "vid1", Title: "Song"})

	for _, ref := range []string{job.ID, "vid1"} {
		if got, ok := c.FindJob(ref); !ok || got.VideoID != "vid1" {
			t.Errorf("FindJob(%q) = %+v, %v", ref, got, ok)
		}
	}
	if _, ok := c.FindJob("nope"); ok {
		t.Error("FindJob(nope) found something")
	}
}

func TestHistoryIsBoundedAndNewestFirst(t *testing.T) {
	c, _ := newTestCatalog(t)
	for i := 0; i < maxHistory+25; i++ {
		c.Record(Pending{VideoID: "v", Title: string(rune('a' + i%26))}, StateFailed, "r")
	}
	if got := c.History(maxHistory + 100); len(got) != maxHistory {
		t.Fatalf("history length = %d, want %d", len(got), maxHistory)
	}
}

func TestReceivedJobsAreNotMatchedAsPending(t *testing.T) {
	c, _ := newTestCatalog(t)
	c.AddPending(Pending{VideoID: "vid1", State: StateReceived})
	if id, ok := c.MatchPending("", "", 0); ok {
		t.Fatalf("MatchPending matched a job still being resolved: %q", id)
	}
}

func TestUnresolveRestoresJobAndRemovesTrack(t *testing.T) {
	c, _ := newTestCatalog(t)
	job := c.AddPending(Pending{VideoID: "vid1", Title: "Song", Artist: "Band", Duration: 200})
	before := c.Size()

	c.ResolvePending("vid1", "song-1")
	if c.Size() != before+1 || c.PendingCount() != 0 {
		t.Fatal("ResolvePending did not promote the track")
	}

	c.Unresolve(job)
	if c.Size() != before {
		t.Fatalf("track still indexed after Unresolve: size=%d", c.Size())
	}
	if _, ok := c.FindByVideo("vid1"); ok {
		t.Fatal("video still found in the index after Unresolve")
	}
	if _, ok := c.Active("vid1"); !ok {
		t.Fatal("job not active again after Unresolve")
	}
	for _, h := range c.History(10) {
		if h.State == StateFiled {
			t.Fatalf("filed entry left in history: %+v", h)
		}
	}
}

func TestLoadUpgradesOldPendingEntries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "catalog.json")
	// The shape written before jobs had ids, states or timestamps.
	old := `{"tracks":[],"pending":{"vid1":{"videoId":"vid1","title":"Song","artist":"Band","duration":100,"attempts":3}}}`
	if err := os.WriteFile(path, []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}

	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	job, ok := c.Active("vid1")
	if !ok || job.State != StateQueued || job.ID == "" || job.Attempts != 3 {
		t.Fatalf("old entry not upgraded: %+v, %v", job, ok)
	}
}

func TestJobsAndHistorySurviveSaveAndLoad(t *testing.T) {
	c, path := newTestCatalog(t)
	c.AddPending(Pending{VideoID: "active", Title: "A", State: StateDownloaded, Filename: "a.mp3"})
	c.Record(Pending{VideoID: "bad", Title: "B"}, StateFailed, "boom")
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}

	again, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if j, ok := again.Active("active"); !ok || j.State != StateDownloaded || j.Filename != "a.mp3" {
		t.Fatalf("active job lost: %+v, %v", j, ok)
	}
	if j, ok := again.FindJob("bad"); !ok || j.Reason != "boom" {
		t.Fatalf("history lost: %+v, %v", j, ok)
	}
}
