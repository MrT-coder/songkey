// Package catalog keeps the in-memory index of every track already present in
// the library and answers duplicate questions about incoming candidates.
//
// The index is small by design: a library of a few thousand tracks costs well
// under a megabyte, so it lives in memory and is persisted to a single JSON
// file. No database engine is involved, which keeps the resident footprint low
// enough for very modest hardware.
package catalog

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
)

// Track is one song already known to the library.
type Track struct {
	// VideoID is the YouTube id it came from, when known. Older entries
	// imported from the playlist have an empty VideoID.
	VideoID string `json:"videoId,omitempty"`
	// SongID is the Navidrome/Subsonic id, when the track has been indexed.
	SongID   string `json:"songId,omitempty"`
	Title    string `json:"title"`
	Artist   string `json:"artist"`
	Duration int    `json:"duration"` // seconds
	// Key is the normalized "artist|title" form used for fuzzy matching.
	Key string `json:"key"`
}

// JobState is where a download request currently stands.
//
// Every request ends in exactly one terminal state, and every terminal state
// carries a reason, so "where did my song go?" always has an answer.
type JobState string

const (
	// StateReceived: accepted from the phone, metadata and MeTube not done yet.
	StateReceived JobState = "received"
	// StateQueued: MeTube accepted it and is (or will be) downloading it.
	StateQueued JobState = "queued"
	// StateDownloaded: MeTube finished; waiting for Navidrome to index the file.
	StateDownloaded JobState = "downloaded"
	// StateFiled: terminal — the song is in the playlist.
	StateFiled JobState = "filed"
	// StateFailed: terminal — something broke; Reason says what.
	StateFailed JobState = "failed"
	// StateDuplicate: terminal — skipped on purpose; Reason says why.
	StateDuplicate JobState = "duplicate"
)

// Terminal reports whether the state is final.
func (s JobState) Terminal() bool {
	return s == StateFiled || s == StateFailed || s == StateDuplicate
}

// maxHistory bounds how many finished jobs are remembered.
const maxHistory = 300

// Pending is one download request. While it is active it lives in the pending
// map; once it reaches a terminal state it moves to the bounded history.
//
// The name is historical: it began as "a download waiting to be filed" and grew
// into the full record of a request.
type Pending struct {
	// ID is a short handle the phone can quote back, e.g. in /status?job=.
	ID       string `json:"id,omitempty"`
	VideoID  string `json:"videoId"`
	URL      string `json:"url,omitempty"`
	Title    string `json:"title"`
	Artist   string `json:"artist"`
	Duration int    `json:"duration"`

	State  JobState `json:"state,omitempty"`
	Reason string   `json:"reason,omitempty"`
	// Force remembers a force=1 request, so a job resumed after a restart
	// behaves the way the original request asked.
	Force bool `json:"force,omitempty"`
	// Filename is what MeTube reported once the download finished.
	Filename string `json:"filename,omitempty"`

	// Attempts counts how many worker passes have looked for this track.
	Attempts int       `json:"attempts"`
	Created  time.Time `json:"created"`
	Updated  time.Time `json:"updated"`
}

// NewID returns a short random job handle.
func NewID() string {
	var b [3]byte
	if _, err := rand.Read(b[:]); err != nil {
		return strconv.FormatInt(time.Now().UnixNano()%0xffffff, 16)
	}
	return hex.EncodeToString(b[:])
}

// Match describes why a candidate was considered a duplicate.
type Match struct {
	Track  Track
	Reason string
	// Exact is true when the candidate shares a YouTube id with a known track.
	Exact bool
}

// Catalog is the concurrency-safe index. The zero value is not usable; call Load.
type Catalog struct {
	mu      sync.RWMutex
	path    string
	byVideo map[string]*Track
	byKey   map[string][]*Track
	tracks  []*Track
	pending map[string]*Pending
	history []*Pending
	dirty   bool
}

type snapshot struct {
	Tracks  []*Track            `json:"tracks"`
	Pending map[string]*Pending `json:"pending"`
	History []*Pending          `json:"history,omitempty"`
}

// Load reads the catalog from path, returning an empty catalog if the file
// does not exist yet.
func Load(path string) (*Catalog, error) {
	c := &Catalog{
		path:    path,
		byVideo: make(map[string]*Track),
		byKey:   make(map[string][]*Track),
		pending: make(map[string]*Pending),
	}

	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return c, nil
	}
	if err != nil {
		return nil, err
	}

	var snap snapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return nil, err
	}
	if snap.Pending != nil {
		c.pending = snap.Pending
	}
	// Entries written before jobs had states are plain "sent to MeTube" records.
	now := time.Now()
	for _, p := range c.pending {
		if p.State == "" {
			p.State = StateQueued
		}
		if p.ID == "" {
			p.ID = NewID()
		}
		if p.Created.IsZero() {
			p.Created, p.Updated = now, now
		}
	}
	c.history = snap.History
	for _, t := range snap.Tracks {
		c.insert(t)
	}
	return c, nil
}

// Save writes the catalog to disk atomically. It is a no-op when nothing changed.
func (c *Catalog) Save() error {
	c.mu.Lock()
	if !c.dirty {
		c.mu.Unlock()
		return nil
	}
	// Marshal while still holding the lock: the maps and records are mutated
	// in place by other goroutines, and encoding them unlocked would race.
	data, err := json.Marshal(snapshot{Tracks: c.tracks, Pending: c.pending, History: c.history})
	c.dirty = false
	c.mu.Unlock()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(c.path), 0o755); err != nil {
		return err
	}
	tmp := c.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, c.path)
}

// insert adds a track to every lookup structure. The caller holds the lock.
func (c *Catalog) insert(t *Track) {
	if t.Key == "" {
		t.Key = Key(t.Artist, t.Title)
	}
	c.tracks = append(c.tracks, t)
	if t.VideoID != "" {
		c.byVideo[t.VideoID] = t
	}
	c.byKey[t.Key] = append(c.byKey[t.Key], t)
}

// Add records a track as present in the library.
func (c *Catalog) Add(t Track) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.insert(&t)
	c.dirty = true
}

// Replace swaps the whole track set, keeping pending downloads untouched. It is
// used by the bootstrap and reindex paths.
func (c *Catalog) Replace(tracks []Track) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.tracks = nil
	c.byVideo = make(map[string]*Track, len(tracks))
	c.byKey = make(map[string][]*Track, len(tracks))
	for i := range tracks {
		t := tracks[i]
		c.insert(&t)
	}
	c.dirty = true
}

// Size reports how many tracks are indexed.
func (c *Catalog) Size() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.tracks)
}

// SongIDs returns the Navidrome ids of every indexed track.
func (c *Catalog) SongIDs() map[string]struct{} {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make(map[string]struct{}, len(c.tracks))
	for _, t := range c.tracks {
		if t.SongID != "" {
			out[t.SongID] = struct{}{}
		}
	}
	return out
}

// FindByVideo returns the track downloaded from the given YouTube id.
// This is the cheapest check available: no network, no metadata lookup.
func (c *Catalog) FindByVideo(videoID string) (Track, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if t, ok := c.byVideo[videoID]; ok {
		return *t, true
	}
	return Track{}, false
}

// durationSlack is how far apart two runtimes may be and still be considered
// the same recording. YouTube rips routinely differ by a second or two of
// silence at either end.
const durationSlack = 4

// similarityFloor is the token overlap ratio above which two titles are
// reported as a likely duplicate.
const similarityFloor = 0.82

// FindDuplicate looks for an existing track that plausibly is the same
// recording as the candidate. Duration is used as a guard so that covers and
// remixes of very different length are not collapsed together.
func (c *Catalog) FindDuplicate(artist, title string, duration int) (Match, bool) {
	key := Key(artist, title)

	c.mu.RLock()
	defer c.mu.RUnlock()

	// Exact normalized key wins immediately when the runtime agrees.
	for _, t := range c.byKey[key] {
		if durationClose(t.Duration, duration) {
			return Match{Track: *t, Reason: "same normalized title and duration"}, true
		}
	}

	candidate := tokenize(key)
	if len(candidate) == 0 {
		return Match{}, false
	}

	var best *Track
	bestScore := 0.0
	for _, t := range c.tracks {
		if !durationClose(t.Duration, duration) {
			continue
		}
		score := similarity(candidate, tokenize(t.Key))
		if score > bestScore {
			bestScore, best = score, t
		}
	}
	if best != nil && bestScore >= similarityFloor {
		return Match{Track: *best, Reason: "very similar title and matching duration"}, true
	}
	return Match{}, false
}

// durationClose reports whether two runtimes are near enough to be the same
// recording. A zero on either side means "unknown", which never blocks.
func durationClose(a, b int) bool {
	if a == 0 || b == 0 {
		return true
	}
	d := a - b
	if d < 0 {
		d = -d
	}
	return d <= durationSlack
}

// --- pending downloads -------------------------------------------------------

// AddPending records an active download request and returns it with its
// defaults filled in (id, state, timestamps).
func (c *Catalog) AddPending(p Pending) Pending {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	if p.ID == "" {
		p.ID = NewID()
	}
	if p.State == "" {
		p.State = StateQueued
	}
	if p.Created.IsZero() {
		p.Created = now
	}
	p.Updated = now
	c.pending[p.VideoID] = &p
	c.dirty = true
	return p
}

// Update changes an active job in place. It reports false when the job is no
// longer active.
func (c *Catalog) Update(videoID string, fn func(*Pending)) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	p, ok := c.pending[videoID]
	if !ok {
		return false
	}
	fn(p)
	p.Updated = time.Now()
	c.dirty = true
	return true
}

// Active returns a copy of one active job.
func (c *Catalog) Active(videoID string) (Pending, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if p, ok := c.pending[videoID]; ok {
		return *p, true
	}
	return Pending{}, false
}

// Finish moves an active job into the history with a terminal state and the
// reason it ended that way. It returns the finished record.
func (c *Catalog) Finish(videoID string, state JobState, reason string) (Pending, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	p, ok := c.pending[videoID]
	if !ok {
		return Pending{}, false
	}
	delete(c.pending, videoID)
	p.State, p.Reason, p.Updated = state, reason, time.Now()
	c.remember(p)
	return *p, true
}

// Record stores a request that ended without ever becoming active, such as a
// duplicate or a link whose metadata could not be read.
func (c *Catalog) Record(p Pending, state JobState, reason string) Pending {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	if p.ID == "" {
		p.ID = NewID()
	}
	if p.Created.IsZero() {
		p.Created = now
	}
	p.State, p.Reason, p.Updated = state, reason, now
	c.remember(&p)
	return p
}

// remember appends to the bounded history. The caller holds the lock.
func (c *Catalog) remember(p *Pending) {
	c.history = append(c.history, p)
	if over := len(c.history) - maxHistory; over > 0 {
		c.history = c.history[over:]
	}
	c.dirty = true
}

// History returns up to limit finished jobs, newest first.
func (c *Catalog) History(limit int) []Pending {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]Pending, 0, limit)
	for i := len(c.history) - 1; i >= 0 && len(out) < limit; i-- {
		out = append(out, *c.history[i])
	}
	return out
}

// FindJob looks a job up by its id or by its YouTube id, preferring active
// jobs and then the most recent finished one.
func (c *Catalog) FindJob(ref string) (Pending, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	for _, p := range c.pending {
		if p.ID == ref || p.VideoID == ref {
			return *p, true
		}
	}
	for i := len(c.history) - 1; i >= 0; i-- {
		if p := c.history[i]; p.ID == ref || p.VideoID == ref {
			return *p, true
		}
	}
	return Pending{}, false
}

// Pending returns a copy of every active job, oldest first so output is stable.
func (c *Catalog) Pending() []Pending {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]Pending, 0, len(c.pending))
	for _, p := range c.pending {
		out = append(out, *p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.Before(out[j].Created) })
	return out
}

// PendingCount reports how many downloads are still in flight. The worker uses
// this to stay completely idle when there is nothing to do.
func (c *Catalog) PendingCount() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.pending)
}

// ResolvePending marks a download as filed and promotes it into the index.
func (c *Catalog) ResolvePending(videoID, songID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	p, ok := c.pending[videoID]
	if !ok {
		return
	}
	delete(c.pending, videoID)
	c.insert(&Track{
		VideoID:  p.VideoID,
		SongID:   songID,
		Title:    p.Title,
		Artist:   p.Artist,
		Duration: p.Duration,
	})
	p.State, p.Reason, p.Updated = StateFiled, "", time.Now()
	c.remember(p)
}

// Unresolve undoes ResolvePending for a job whose playlist update failed: the
// track leaves the index and the job returns to the active set, so the next
// worker pass retries it instead of believing it was filed.
func (c *Catalog) Unresolve(p Pending) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if t, ok := c.byVideo[p.VideoID]; ok {
		delete(c.byVideo, p.VideoID)
		for i, x := range c.tracks {
			if x == t {
				c.tracks = append(c.tracks[:i], c.tracks[i+1:]...)
				break
			}
		}
		kept := c.byKey[t.Key][:0]
		for _, x := range c.byKey[t.Key] {
			if x != t {
				kept = append(kept, x)
			}
		}
		if len(kept) == 0 {
			delete(c.byKey, t.Key)
		} else {
			c.byKey[t.Key] = kept
		}
	}
	for i := len(c.history) - 1; i >= 0; i-- {
		if c.history[i].ID == p.ID && c.history[i].State == StateFiled {
			c.history = append(c.history[:i], c.history[i+1:]...)
			break
		}
	}
	restored := p
	restored.Updated = time.Now()
	c.pending[p.VideoID] = &restored
	c.dirty = true
}

// MatchPending finds the pending download that corresponds to a track that has
// just appeared in the library, returning its YouTube id.
//
// The comparison runs against the same normalized key used for duplicate
// detection, because the filename Navidrome indexed comes from the very title
// yt-dlp reported — but tags and cleanup can still shift it slightly.
func (c *Catalog) MatchPending(artist, title string, duration int) (string, bool) {
	key := Key(artist, title)
	candidate := tokenize(key)

	c.mu.RLock()
	defer c.mu.RUnlock()

	bestID, bestScore := "", 0.0
	for id, p := range c.pending {
		// A request still being resolved has no title yet, so there is nothing
		// to compare against.
		if p.State == StateReceived || !durationClose(p.Duration, duration) {
			continue
		}
		pendingKey := Key(p.Artist, p.Title)
		if pendingKey == key {
			return id, true
		}
		if score := similarity(candidate, tokenize(pendingKey)); score > bestScore {
			bestScore, bestID = score, id
		}
	}
	// The pool of pending downloads is tiny, so a looser threshold than the
	// duplicate check is safe here and tolerates tag rewriting.
	if bestID != "" && bestScore >= 0.6 {
		return bestID, true
	}
	return "", false
}

// BumpAttempt increments the attempt counter. Once it reaches max the job is
// moved to the history as failed with the given reason — never silently
// dropped — and returned so the caller can tell the user.
func (c *Catalog) BumpAttempt(videoID string, max int, reason string) (Pending, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	p, ok := c.pending[videoID]
	if !ok {
		return Pending{}, false
	}
	p.Attempts++
	p.Updated = time.Now()
	c.dirty = true
	if p.Attempts >= max {
		delete(c.pending, videoID)
		p.State, p.Reason = StateFailed, reason
		c.remember(p)
		return *p, true
	}
	return *p, false
}

// --- normalization -----------------------------------------------------------

// noisePattern strips the decorations YouTube uploaders attach to titles. These
// carry no identity: the same recording appears with and without any of them.
var noisePattern = regexp.MustCompile(`(?i)\b(official\s+(music\s+)?video|official\s+audio|official\s+lyric\s+video|lyric\s+video|lyrics|video\s+oficial|audio\s+oficial|letra|con\s+letra|hd|hq|4k|full\s+hd|remaster(ed)?(\s+\d{4})?|explicit|visualizer|mv)\b`)

// bracketPattern removes bracketed segments, which almost always hold the noise
// above rather than part of the actual song name.
var bracketPattern = regexp.MustCompile(`[\(\[\{][^\)\]\}]*[\)\]\}]`)

// featPattern removes featured-artist credits, which appear inconsistently.
var featPattern = regexp.MustCompile(`(?i)\s*\b(feat|ft|featuring|con)\b\.?\s+.*$`)

// nonWordPattern collapses everything that is not a letter, digit or space.
var nonWordPattern = regexp.MustCompile(`[^\p{L}\p{N}\s]+`)

// spacePattern collapses runs of whitespace.
var spacePattern = regexp.MustCompile(`\s+`)

// accents maps the Latin letters that appear in Spanish and Portuguese titles
// to their unaccented form, so "Corazón" and "Corazon" agree.
var accents = strings.NewReplacer(
	"á", "a", "à", "a", "ä", "a", "â", "a", "ã", "a", "å", "a",
	"é", "e", "è", "e", "ë", "e", "ê", "e",
	"í", "i", "ì", "i", "ï", "i", "î", "i",
	"ó", "o", "ò", "o", "ö", "o", "ô", "o", "õ", "o",
	"ú", "u", "ù", "u", "ü", "u", "û", "u",
	"ñ", "n", "ç", "c", "ý", "y", "ÿ", "y",
)

// strip removes the decorations but keeps punctuation, so that the artist/title
// separator survives long enough to be found.
func strip(s string) string {
	s = strings.ToLower(s)
	s = accents.Replace(s)
	s = bracketPattern.ReplaceAllString(s, " ")
	s = noisePattern.ReplaceAllString(s, " ")
	s = featPattern.ReplaceAllString(s, " ")
	return spacePattern.ReplaceAllString(s, " ")
}

// Normalize reduces a title or artist to a comparable form.
func Normalize(s string) string {
	s = nonWordPattern.ReplaceAllString(strip(s), " ")
	return strings.TrimSpace(spacePattern.ReplaceAllString(s, " "))
}

// FileKey reduces a file name or path to a form that survives the differences
// between what MeTube reports and what Navidrome indexes for the same file.
//
// yt-dlp stores characters that are illegal on some filesystems as full-width
// look-alikes (｜ ？ ＂ ⧸) while Navidrome reports the plain ones, and the two
// may disagree on the extension or the folder. Only the base name's letters and
// digits are kept, with full-width forms folded to ASCII.
func FileKey(name string) string {
	name = strings.ReplaceAll(name, "\\", "/")
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	if i := strings.LastIndex(name, "."); i > 0 && len(name)-i <= 6 {
		name = name[:i]
	}
	var b strings.Builder
	for _, r := range name {
		if r >= 0xFF01 && r <= 0xFF5E { // full-width ASCII block
			r -= 0xFEE0
		}
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(unicode.ToLower(r))
		}
	}
	return b.String()
}

// topicSuffix is how YouTube names auto-generated artist channels.
var topicSuffix = regexp.MustCompile(`(?i)\s*-\s*topic$`)

// separatorPattern matches the dash that splits "Artist - Song" titles. Spaces
// are required on both sides so hyphenated words stay intact.
var separatorPattern = regexp.MustCompile(`\s+[-–—]\s+`)

// Key builds the normalized identity used for duplicate comparison.
//
// The uploader is a weak signal: the same song is posted by the artist's own
// channel, by "Artist - Topic", and by fan channels with unrelated names. When
// the title itself carries an "Artist - Song" split, that split is trusted over
// the channel name, which is what makes those three cases agree.
func Key(artist, title string) string {
	a := Normalize(topicSuffix.ReplaceAllString(artist, ""))

	if parts := separatorPattern.Split(strip(title), 2); len(parts) == 2 {
		left, right := Normalize(parts[0]), Normalize(parts[1])
		if left != "" && right != "" {
			return left + "|" + right
		}
	}

	t := Normalize(title)

	// Some uploaders repeat the channel name at the front of the title without
	// a separator. Dropping the repetition keeps both spellings comparable.
	if a != "" && strings.HasPrefix(t, a+" ") {
		t = strings.TrimSpace(strings.TrimPrefix(t, a))
	}
	if t == "" {
		return a
	}
	if a == "" {
		return t
	}
	return a + "|" + t
}

// tokenize splits a normalized key into a set of words.
func tokenize(key string) map[string]struct{} {
	out := make(map[string]struct{})
	for _, f := range strings.Fields(strings.ReplaceAll(key, "|", " ")) {
		out[f] = struct{}{}
	}
	return out
}

// similarity is the Jaccard overlap of two token sets: shared words over total
// distinct words. It tolerates reordering and extra decoration, which is
// exactly how YouTube titles differ from each other.
func similarity(a, b map[string]struct{}) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	shared := 0
	for tok := range a {
		if _, ok := b[tok]; ok {
			shared++
		}
	}
	union := len(a) + len(b) - shared
	if union == 0 {
		return 0
	}
	return float64(shared) / float64(union)
}
