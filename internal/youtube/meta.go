// Package youtube extracts the identity and metadata of a candidate video.
//
// Two operations live here and they cost very different amounts. ExtractID is
// pure string work and runs in microseconds. Probe shells out to yt-dlp and
// takes seconds. Callers should always try ExtractID first so that the common
// case — a link that was already downloaded — never touches the network.
package youtube

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// idPattern matches the video id in every YouTube URL shape: watch links,
// youtu.be short links, shorts, embeds, and the music subdomain.
var idPattern = regexp.MustCompile(`(?:v=|/shorts/|/embed/|youtu\.be/|/v/)([A-Za-z0-9_-]{11})`)

// ExtractID pulls the eleven-character video id out of a URL. It performs no
// I/O, which is what makes the fast duplicate check free.
func ExtractID(rawURL string) (string, bool) {
	if m := idPattern.FindStringSubmatch(rawURL); len(m) == 2 {
		return m[1], true
	}
	// A bare id pasted on its own is also accepted.
	trimmed := strings.TrimSpace(rawURL)
	if len(trimmed) == 11 && !strings.ContainsAny(trimmed, "/:.? ") {
		return trimmed, true
	}
	return "", false
}

// playlistPattern matches the playlist id in a YouTube URL.
var playlistPattern = regexp.MustCompile(`[?&]list=([A-Za-z0-9_-]+)`)

// ExtractPlaylistID returns the playlist id when a URL points at one and does
// not also name a single video.
//
// A link carrying both is a song shared while a playlist happened to be open,
// which means the song — not the playlist — is what was asked for.
func ExtractPlaylistID(rawURL string) (string, bool) {
	if _, hasVideo := ExtractID(rawURL); hasVideo {
		return "", false
	}
	if m := playlistPattern.FindStringSubmatch(rawURL); len(m) == 2 {
		return m[1], true
	}
	return "", false
}

// Playlist is a playlist and the tracks it holds.
type Playlist struct {
	Title   string
	Entries []Meta
}

// Meta is the subset of yt-dlp's output this service uses.
type Meta struct {
	ID       string
	Title    string
	Artist   string
	Duration int
	// Degraded is true when the metadata came from the oEmbed fallback, which
	// means Duration is unknown and the duplicate check is weaker.
	Degraded bool
}

// rawMeta mirrors the yt-dlp JSON fields worth reading. Music uploads carry
// track/artist; ordinary uploads only have title/uploader.
type rawMeta struct {
	ID       string  `json:"id"`
	Title    string  `json:"title"`
	Track    string  `json:"track"`
	Artist   string  `json:"artist"`
	Uploader string  `json:"uploader"`
	Channel  string  `json:"channel"`
	Duration float64 `json:"duration"`
}

// Prober runs yt-dlp to read metadata without downloading anything.
type Prober struct {
	binary  string
	timeout time.Duration
	http    *http.Client

	// oembedURL overrides YouTube's oEmbed endpoint; tests point it at a local
	// server. Empty means the real one.
	oembedURL string
}

// NewProber builds a prober around a yt-dlp executable.
func NewProber(binary string, timeout time.Duration) *Prober {
	return &Prober{
		binary:  binary,
		timeout: timeout,
		http:    &http.Client{Timeout: 15 * time.Second},
	}
}

// Probe reads a video's title, artist and duration without fetching the media.
//
// YouTube breaks yt-dlp regularly, and a home server does not always carry the
// newest build. When the extractor fails, this falls back to YouTube's oEmbed
// endpoint, which is a stable public API. The fallback carries no duration, so
// the duplicate check loses its runtime guard and leans harder on the title —
// degraded, but far better than refusing to work at all.
func (p *Prober) Probe(ctx context.Context, rawURL string) (Meta, error) {
	meta, err := p.probeYtDlp(ctx, rawURL)
	if err == nil {
		return meta, nil
	}

	fallback, oerr := p.probeOEmbed(ctx, rawURL)
	if oerr != nil {
		// Report the original failure: it is the one worth acting on.
		return Meta{}, err
	}
	return fallback, nil
}

// probeYtDlp is the rich path: title, artist and duration in one call.
func (p *Prober) probeYtDlp(ctx context.Context, rawURL string) (Meta, error) {
	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, p.binary,
		"--dump-single-json",
		"--skip-download",
		"--no-playlist",
		"--no-warnings",
		"--socket-timeout", "15",
		rawURL,
	)

	out, err := cmd.Output()
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return Meta{}, fmt.Errorf("yt-dlp timed out reading metadata")
		}
		var exitErr *exec.ExitError
		if ok := asExitError(err, &exitErr); ok && len(exitErr.Stderr) > 0 {
			return Meta{}, fmt.Errorf("yt-dlp: %s", strings.TrimSpace(string(exitErr.Stderr)))
		}
		return Meta{}, fmt.Errorf("yt-dlp: %w", err)
	}

	var raw rawMeta
	if err := json.Unmarshal(out, &raw); err != nil {
		return Meta{}, fmt.Errorf("parsing yt-dlp output: %w", err)
	}

	return Meta{
		ID:       raw.ID,
		Title:    firstNonEmpty(raw.Track, raw.Title),
		Artist:   firstNonEmpty(raw.Artist, raw.Uploader, raw.Channel),
		Duration: int(raw.Duration),
	}, nil
}

// rawPlaylist mirrors the flat-playlist JSON yt-dlp emits.
type rawPlaylist struct {
	Title   string    `json:"title"`
	Entries []rawMeta `json:"entries"`
}

// ProbePlaylist lists a playlist's contents without downloading anything.
//
// --flat-playlist asks YouTube for the index only, so one call returns the id,
// title and duration of every track. That is what keeps deduplicating a fifty
// song album as cheap as deduplicating a single song.
func (p *Prober) ProbePlaylist(ctx context.Context, rawURL string, limit int) (Playlist, error) {
	// Listing many entries takes longer than reading one video, so this gets
	// its own, more generous budget.
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(ctx, p.binary,
		"--flat-playlist",
		"--dump-single-json",
		"--no-warnings",
		"--socket-timeout", "15",
		"--playlist-end", strconv.Itoa(limit),
		rawURL,
	)

	out, err := cmd.Output()
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return Playlist{}, fmt.Errorf("yt-dlp timed out listing the playlist")
		}
		var exitErr *exec.ExitError
		if ok := asExitError(err, &exitErr); ok && len(exitErr.Stderr) > 0 {
			return Playlist{}, fmt.Errorf("yt-dlp: %s", strings.TrimSpace(string(exitErr.Stderr)))
		}
		return Playlist{}, fmt.Errorf("yt-dlp: %w", err)
	}

	var raw rawPlaylist
	if err := json.Unmarshal(out, &raw); err != nil {
		return Playlist{}, fmt.Errorf("parsing yt-dlp output: %w", err)
	}

	pl := Playlist{Title: raw.Title}
	for _, e := range raw.Entries {
		if e.ID == "" {
			continue // Deleted or private entries carry no id.
		}
		pl.Entries = append(pl.Entries, Meta{
			ID:       e.ID,
			Title:    firstNonEmpty(e.Track, e.Title),
			Artist:   firstNonEmpty(e.Artist, e.Uploader, e.Channel),
			Duration: int(e.Duration),
		})
	}
	return pl, nil
}

// oembedResponse is the subset of YouTube's oEmbed payload worth reading.
type oembedResponse struct {
	Title  string `json:"title"`
	Author string `json:"author_name"`
}

// probeOEmbed asks YouTube's public oEmbed endpoint for the title and channel.
//
// It needs no API key and no extractor, so it keeps working through the
// YouTube changes that periodically break yt-dlp. It cannot report duration.
func (p *Prober) probeOEmbed(ctx context.Context, rawURL string) (Meta, error) {
	id, ok := ExtractID(rawURL)
	if !ok {
		return Meta{}, fmt.Errorf("no video id in %q", rawURL)
	}

	raw, err := p.oembed(ctx, "https://www.youtube.com/watch?v="+id)
	if err != nil {
		return Meta{}, err
	}

	return Meta{
		ID:     id,
		Title:  raw.Title,
		Artist: raw.Author,
		// Duration is unavailable here; zero means "unknown" to the catalog,
		// which then matches on title alone.
		Duration: 0,
		Degraded: true,
	}, nil
}

// oembed fetches the oEmbed record of any public YouTube URL, a video or a
// playlist alike.
func (p *Prober) oembed(ctx context.Context, target string) (oembedResponse, error) {
	base := p.oembedURL
	if base == "" {
		base = "https://www.youtube.com/oembed"
	}
	endpoint := base + "?format=json&url=" + url.QueryEscape(target)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return oembedResponse{}, err
	}

	resp, err := p.http.Do(req)
	if err != nil {
		return oembedResponse{}, fmt.Errorf("oembed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return oembedResponse{}, fmt.Errorf("oembed: unexpected status %s", resp.Status)
	}

	var raw oembedResponse
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return oembedResponse{}, fmt.Errorf("oembed: decoding response: %w", err)
	}
	if raw.Title == "" {
		return oembedResponse{}, fmt.Errorf("oembed: empty title")
	}
	return raw, nil
}

// previewTimeout bounds Preview: it is called while a phone is waiting for its
// reply, so a slow answer is worth less than no answer.
const previewTimeout = 4 * time.Second

// Preview returns the title of a video or playlist, quickly, so the caller can
// name what it just accepted before the slow work has finished. It returns an
// error rather than waiting when YouTube is slow; callers show no name then.
func (p *Prober) Preview(ctx context.Context, target string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, previewTimeout)
	defer cancel()

	raw, err := p.oembed(ctx, target)
	if err != nil {
		return "", err
	}
	return raw.Title, nil
}

// asExitError unwraps an *exec.ExitError without pulling in errors.As at every
// call site.
func asExitError(err error, target **exec.ExitError) bool {
	e, ok := err.(*exec.ExitError)
	if ok {
		*target = e
	}
	return ok
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
