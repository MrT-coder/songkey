// Command server exposes a small HTTP endpoint that sits between a phone
// shortcut and MeTube.
//
// It answers one question before anything is downloaded — "do I already have
// this?" — and afterwards files the finished download into a Navidrome
// playlist. The design goal throughout is to do as little work as possible on
// very modest hardware: the cheap checks run first, and the background worker
// stays completely idle whenever there is nothing in flight.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/MrT-coder/songkey/internal/catalog"
	"github.com/MrT-coder/songkey/internal/metube"
	"github.com/MrT-coder/songkey/internal/navidrome"
	"github.com/MrT-coder/songkey/internal/notify"
	"github.com/MrT-coder/songkey/internal/youtube"
)

// maxPendingAttempts bounds how many worker passes look for a download before
// giving up on it, so a failed or blocked download cannot be polled forever.
//
// At the default one-minute interval this is roughly two hours of patience.
// That is deliberately generous: when something upstream is broken — a stalled
// MeTube download, a library scan that cannot run — a shorter window discards
// the record of the download before anyone has had a chance to notice.
const maxPendingAttempts = 120

type config struct {
	listenAddr       string
	apiToken         string
	dataPath         string
	navidromeURL     string
	navidromeUser    string
	navidromePass    string
	playlistName     string
	metubeURL        string
	metubeQuality    string
	metubeFormat     string
	metubeFolder     string
	ytdlpBinary      string
	maxPlaylistItems int
	ntfyURL          string
	workerInterval   time.Duration
	reconcileEvery   time.Duration
}

func loadConfig() (config, error) {
	c := config{
		listenAddr:       env("LISTEN_ADDR", ":8090"),
		apiToken:         os.Getenv("API_TOKEN"),
		dataPath:         env("DATA_PATH", "/data/catalog.json"),
		navidromeURL:     strings.TrimRight(os.Getenv("NAVIDROME_URL"), "/"),
		navidromeUser:    os.Getenv("NAVIDROME_USER"),
		navidromePass:    os.Getenv("NAVIDROME_PASS"),
		playlistName:     env("NAVIDROME_PLAYLIST", "songkey"),
		metubeURL:        strings.TrimRight(env("METUBE_URL", "http://host.docker.internal:8081"), "/"),
		metubeQuality:    env("METUBE_QUALITY", "best"),
		metubeFormat:     env("METUBE_FORMAT", "mp3"),
		metubeFolder:     os.Getenv("METUBE_FOLDER"),
		ytdlpBinary:      env("YTDLP_BIN", "yt-dlp"),
		maxPlaylistItems: envInt("PLAYLIST_MAX_ITEMS", 100),
		ntfyURL:          os.Getenv("NTFY_URL"),
		workerInterval:   envDuration("WORKER_INTERVAL", 60*time.Second),
		reconcileEvery:   envDuration("RECONCILE_INTERVAL", time.Hour),
	}

	var missing []string
	if c.navidromeURL == "" {
		missing = append(missing, "NAVIDROME_URL")
	}
	if c.navidromeUser == "" {
		missing = append(missing, "NAVIDROME_USER")
	}
	if c.navidromePass == "" {
		missing = append(missing, "NAVIDROME_PASS")
	}
	if len(missing) > 0 {
		return c, fmt.Errorf("missing required settings: %s", strings.Join(missing, ", "))
	}
	return c, nil
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) int {
	if v, err := strconv.Atoi(os.Getenv(key)); err == nil && v > 0 {
		return v
	}
	return fallback
}

func envDuration(key string, fallback time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	if secs, err := strconv.Atoi(v); err == nil {
		return time.Duration(secs) * time.Second
	}
	if d, err := time.ParseDuration(v); err == nil {
		return d
	}
	return fallback
}

// server holds everything the handlers and the worker share.
type server struct {
	cfg      config
	cat      *catalog.Catalog
	nd       *navidrome.Client
	mt       *metube.Client
	probe    *youtube.Prober
	notifier *notify.Notifier

	// probeSem serializes yt-dlp runs; see processAdd.
	probeSem chan struct{}

	// guard rate-limits wrong tokens; see authorize.
	guard *authGuard

	// playlistMu keeps the worker and the reconciler from editing the playlist at
	// the same moment, which is how a song would end up in it twice.
	playlistMu sync.Mutex

	// playlistID is the target playlist, resolved once at startup.
	playlistID string

	// scanWarned keeps the missing-admin-rights notice from repeating on
	// every worker pass.
	scanWarned bool

	// filedSinceIdle counts tracks filed since the pending queue was last
	// empty, so one notification can cover a whole batch.
	filedSinceIdle int
}

// version is set at build time with -ldflags "-X main.version=v1.2.3".
var version = "dev"

func main() {
	log.SetFlags(log.Ldate | log.Ltime)
	log.Printf("songkey %s", version)

	cfg, err := loadConfig()
	if err != nil {
		log.Fatalf("configuration: %v", err)
	}

	cat, err := catalog.Load(cfg.dataPath)
	if err != nil {
		log.Fatalf("loading catalog: %v", err)
	}

	s := &server{
		cfg:      cfg,
		cat:      cat,
		nd:       navidrome.New(cfg.navidromeURL, cfg.navidromeUser, cfg.navidromePass, 30*time.Second),
		mt:       metube.New(cfg.metubeURL, cfg.metubeQuality, cfg.metubeFormat, cfg.metubeFolder, 20*time.Second),
		probe:    youtube.NewProber(cfg.ytdlpBinary, 45*time.Second),
		notifier: notify.New(cfg.ntfyURL),
		probeSem: make(chan struct{}, 1),
		// Ten wrong tokens in ten minutes lock a client out for fifteen.
		guard: newAuthGuard(10, 10*time.Minute, 15*time.Minute),
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Generous: on a cold boot this waits for Navidrome to finish starting.
	startupCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	if err := s.waitForNavidrome(startupCtx); err != nil {
		log.Fatalf("cannot reach Navidrome at %s: %v", cfg.navidromeURL, err)
	}

	pl, err := s.nd.FindPlaylist(startupCtx, cfg.playlistName)
	if err != nil {
		log.Fatalf("resolving playlist: %v", err)
	}
	s.playlistID = pl.ID
	log.Printf("target playlist %q resolved (id=%s, %d songs)", pl.Name, pl.ID, pl.SongCount)

	// An empty catalog means a first run: seed it from the playlist itself,
	// which already lists everything the library holds.
	if s.cat.Size() == 0 {
		if err := s.bootstrap(startupCtx); err != nil {
			log.Fatalf("bootstrap: %v", err)
		}
	} else {
		log.Printf("catalog loaded with %d tracks", s.cat.Size())
	}

	if n := s.cat.PendingCount(); n > 0 {
		log.Printf("%d download(s) still pending, the worker will keep looking for them", n)
	}

	s.resumeReceived()
	go s.runWorker(ctx)
	go s.runReconciler(ctx)

	mux := http.NewServeMux()
	mux.HandleFunc("/add", s.handleAdd)
	mux.HandleFunc("/reindex", s.handleReindex)
	mux.HandleFunc("/status", s.handleStatus)
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, "ok")
	})

	httpServer := &http.Server{
		Addr:              cfg.listenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		log.Printf("listening on %s", cfg.listenAddr)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("http server: %v", err)
		}
	}()

	<-ctx.Done()
	log.Println("shutting down")

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	_ = httpServer.Shutdown(shutdownCtx)

	if err := s.cat.Save(); err != nil {
		log.Printf("saving catalog: %v", err)
	}
}

// waitForNavidrome blocks until Navidrome answers or the context runs out.
//
// On a machine that is powered down overnight, both containers start at once
// and this one frequently wins the race. Exiting on the first failed ping would
// hand the problem to Docker's restart backoff, which grows after each attempt
// and can leave the service down long after Navidrome is ready.
func (s *server) waitForNavidrome(ctx context.Context) error {
	const retryDelay = 10 * time.Second

	attempt := 0
	for {
		attempt++
		err := s.nd.Ping(ctx)
		if err == nil {
			if attempt > 1 {
				log.Printf("Navidrome answered on attempt %d", attempt)
			}
			return nil
		}

		select {
		case <-ctx.Done():
			return err
		case <-time.After(retryDelay):
			log.Printf("Navidrome not ready yet (%v), retrying", err)
		}
	}
}

// bootstrap fills an empty catalog from the target playlist. One API call
// replaces what would otherwise be a full filesystem scan.
func (s *server) bootstrap(ctx context.Context) error {
	log.Printf("catalog is empty, seeding from playlist %q", s.cfg.playlistName)

	pl, err := s.nd.GetPlaylist(ctx, s.playlistID)
	if err != nil {
		return err
	}

	tracks := make([]catalog.Track, 0, len(pl.Entry))
	for _, song := range pl.Entry {
		tracks = append(tracks, catalog.Track{
			SongID:   song.ID,
			Title:    song.Title,
			Artist:   song.Artist,
			Duration: song.Duration,
		})
	}
	s.cat.Replace(tracks)

	if err := s.cat.Save(); err != nil {
		return err
	}
	log.Printf("catalog seeded with %d tracks", len(tracks))
	return nil
}

// readParams collects request parameters from wherever the client chose to put
// them: the query string, a form body, or a JSON body.
//
// Shortcut apps differ in what they send, and a misplaced parameter produces a
// confusing "unauthorized" rather than a useful error. Accepting all three
// removes a whole category of setup problems.
func readParams(r *http.Request) map[string]string {
	out := make(map[string]string)
	for key, values := range r.URL.Query() {
		if len(values) > 0 {
			out[key] = values[0]
		}
	}

	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err == nil {
			for key, value := range body {
				if s, ok := value.(string); ok {
					out[key] = s
				}
			}
		}
		return out
	}

	// ParseForm only understands url-encoded bodies. Shortcut apps commonly
	// send multipart instead, which needs its own parser — without it every
	// field arrives empty and the request looks unauthenticated.
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		const maxMemory = 1 << 20 // 1 MB; these requests carry only short fields.
		_ = r.ParseMultipartForm(maxMemory)
	} else {
		_ = r.ParseForm()
	}

	for key, values := range r.PostForm {
		if len(values) > 0 {
			out[key] = values[0]
		}
	}
	return out
}

// authorize decides whether a request may proceed and, when it may not, writes
// the reply. It returns true only for a request that carries the shared token.
// An empty API_TOKEN leaves the service open, which is only appropriate on a
// trusted network.
//
// A client that keeps failing is locked out first, before its token is even
// looked at: checking the token of a locked-out client would let it keep
// guessing at no cost.
func (s *server) authorize(w http.ResponseWriter, r *http.Request, p map[string]string) bool {
	if s.cfg.apiToken == "" {
		return true
	}

	client := clientIP(r)
	if wait, blocked := s.guard.Blocked(client); blocked {
		w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
		replyError(w, wantsJSON(p), http.StatusTooManyRequests, "too_many_attempts",
			"too many failed attempts, try again later")
		return false
	}

	if s.tokenOK(r, p) {
		s.guard.Reset(client)
		return true
	}

	// Say enough to diagnose a misconfigured shortcut without ever logging the
	// token itself.
	log.Printf("unauthorized: client=%s method=%s content-type=%q params=%v token_sent=%v auth_header=%v",
		client, r.Method, r.Header.Get("Content-Type"), keysOf(p),
		p["token"] != "", r.Header.Get("Authorization") != "")
	if s.guard.Fail(client) {
		log.Printf("locking out %s after repeated wrong tokens", client)
	}
	replyError(w, wantsJSON(p), http.StatusUnauthorized, "unauthorized", "unauthorized")
	return false
}

// tokenOK reports whether the request presents the shared token, either as a
// parameter or as a Bearer header. The header is the better choice for a
// public hostname: URLs end up in access logs, headers do not.
func (s *server) tokenOK(r *http.Request, p map[string]string) bool {
	if tokenMatches(strings.TrimSpace(p["token"]), s.cfg.apiToken) {
		return true
	}
	if bearer, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
		return tokenMatches(strings.TrimSpace(bearer), s.cfg.apiToken)
	}
	return false
}

// keysOf lists parameter names, so the log shows what arrived without showing
// any of the values.
func keysOf(p map[string]string) []string {
	keys := make([]string, 0, len(p))
	for k := range p {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// handleAdd is the endpoint the phone shortcut calls. It accepts the URL as a
// query parameter or a form field, and answers in plain text so the shortcut
// can show the result directly, or in JSON with format=json (see respond.go).
func (s *server) handleAdd(w http.ResponseWriter, r *http.Request) {
	p := readParams(r)
	if !s.authorize(w, r, p) {
		return
	}
	asJSON := wantsJSON(p)

	// Some shortcut apps send the link JSON-escaped, so the slashes arrive as
	// "\/". Undoing that keeps the logs readable and the matching predictable.
	rawURL := strings.ReplaceAll(strings.TrimSpace(p["url"]), `\/`, "/")
	if rawURL == "" {
		replyError(w, asJSON, http.StatusBadRequest, "missing_url", "missing url parameter")
		return
	}

	force := p["force"] == "1"

	// Step one: the free check. A link already downloaded is answered here
	// without touching the network or spawning yt-dlp.
	// A link naming a playlist and no single video is a request for the whole
	// playlist. One shared while a playlist is merely open names both, and is
	// handled below as the single song it is.
	if _, isPlaylist := youtube.ExtractPlaylistID(rawURL); isPlaylist {
		s.addPlaylist(w, asJSON, rawURL, force)
		return
	}

	videoID, ok := youtube.ExtractID(rawURL)
	if !ok {
		if strings.Contains(rawURL, "/@") {
			replyError(w, asJSON, http.StatusBadRequest, "channel_not_supported",
				"CHANNELS ARE NOT SUPPORTED\nSend a song or a playlist link.")
			return
		}
		replyError(w, asJSON, http.StatusBadRequest, "invalid_url",
			"could not read a YouTube video id from that link")
		return
	}
	if existing, found := s.cat.FindByVideo(videoID); found && !force {
		replyAdd(w, asJSON, http.StatusConflict,
			fmt.Sprintf("ALREADY HAVE IT\n%s - %s\n\nAdd &force=1 to download anyway.",
				existing.Artist, existing.Title),
			addResult{Result: "already_have", Track: &trackView{
				VideoID: existing.VideoID, Title: existing.Title,
				Artist: existing.Artist, Duration: existing.Duration,
			}})
		return
	}

	// Everything downstream uses the canonical URL rather than the link that
	// was shared. Share links carry tracking parameters and come in several
	// shapes; rebuilding from the id gives yt-dlp and MeTube exactly one form
	// to deal with.
	canonicalURL := "https://www.youtube.com/watch?v=" + videoID

	// A request for something already in flight is answered with that job.
	if active, ok := s.cat.Active(videoID); ok && !force {
		v := viewJob(active)
		replyAdd(w, asJSON, http.StatusOK,
			fmt.Sprintf("ALREADY QUEUED\nJob %s is %s.\nCheck /status?job=%s",
				active.ID, active.State, active.ID),
			addResult{Result: "already_queued", Job: &v, Notify: s.notifier.Enabled()})
		return
	}

	// Everything past this point is slow — yt-dlp alone can take most of a
	// minute on this hardware, longer than a phone will wait — so the request is
	// written down and acknowledged now, and the work continues in the
	// background. The outcome, good or bad, arrives as a notification and is
	// always visible in /status.
	//
	// Naming what was accepted is worth a moment, though: a sub-second oEmbed
	// lookup, bounded so that a slow answer costs nothing but the name.
	name, err := s.probe.Preview(r.Context(), canonicalURL)
	if err != nil {
		log.Printf("preview of %s failed (the reply will not name it): %v", videoID, err)
		name = "(reading metadata)"
	}

	job := s.cat.AddPending(catalog.Pending{
		VideoID: videoID,
		URL:     canonicalURL,
		Title:   name,
		State:   catalog.StateReceived,
		Force:   force,
	})
	s.save()
	log.Printf("job %s received: %s (%s)", job.ID, name, videoID)

	go s.processAdd(job)

	replyQueued(w, asJSON, job, name, err == nil, s.notifier.Enabled())
}

// processAdd carries a received job through metadata, duplicate check and
// hand-off to MeTube. It never returns an error: every outcome is written to
// the job, so nothing can vanish between the phone and MeTube.
func (s *server) processAdd(job catalog.Pending) {
	// One probe at a time. Several yt-dlp processes at once would exhaust this
	// machine's memory, and the shortcut no longer waits on the answer.
	s.probeSem <- struct{}{}
	defer func() { <-s.probeSem }()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	label := job.VideoID

	// Step two: the expensive check, reached only for genuinely new links.
	meta, err := s.probe.Probe(ctx, job.URL)
	if err != nil {
		log.Printf("job %s: probe failed: %v", job.ID, err)
		s.fail(ctx, job.VideoID, label, "could not read the video metadata: "+err.Error())
		return
	}
	label = fmt.Sprintf("%s - %s", meta.Artist, meta.Title)

	if meta.Degraded {
		log.Printf("job %s: yt-dlp failed, used the oEmbed fallback: no duration, "+
			"so the duplicate check matched on title alone", job.ID)
	}

	s.cat.Update(job.VideoID, func(p *catalog.Pending) {
		p.Title, p.Artist, p.Duration = meta.Title, meta.Artist, meta.Duration
	})

	if !job.Force {
		if match, dup := s.cat.FindDuplicate(meta.Artist, meta.Title, meta.Duration); dup {
			reason := fmt.Sprintf("probable duplicate of %s - %s (%s)",
				match.Track.Artist, match.Track.Title, match.Reason)
			done, _ := s.cat.Finish(job.VideoID, catalog.StateDuplicate, reason)
			log.Printf("job %s: %s", done.ID, reason)
			s.save()
			s.notifier.Send(ctx, "Skipped: "+label,
				reason+"\n\nSend it again with force=1 to download anyway.\nJob "+done.ID,
				notify.PriorityDefault, "no_entry_sign")
			return
		}
	}

	// MeTube is sometimes slow to answer right after boot, so one retry is
	// cheaper than asking the user to resend.
	var addErr error
	for attempt := 1; attempt <= 3; attempt++ {
		if addErr = s.mt.Add(ctx, job.URL); addErr == nil {
			break
		}
		log.Printf("job %s: metube rejected (attempt %d): %v", job.ID, attempt, addErr)
		select {
		case <-ctx.Done():
		case <-time.After(5 * time.Second):
		}
	}
	if addErr != nil {
		s.fail(ctx, job.VideoID, label, "MeTube rejected the download: "+addErr.Error())
		return
	}

	s.cat.Update(job.VideoID, func(p *catalog.Pending) { p.State = catalog.StateQueued })
	s.save()
	log.Printf("job %s queued in MeTube: %s (%s)", job.ID, label, job.VideoID)
}

// fail ends a job as failed and tells the user why.
func (s *server) fail(ctx context.Context, videoID, label, reason string) {
	done, ok := s.cat.Finish(videoID, catalog.StateFailed, reason)
	s.save()
	id := ""
	if ok {
		id = "\nJob " + done.ID
	}
	s.notifier.Send(ctx, "Failed: "+label, reason+id, notify.PriorityHigh, "x")
}

// save persists the catalog, logging instead of failing: a disk hiccup must not
// take a request down with it.
func (s *server) save() {
	if err := s.cat.Save(); err != nil {
		log.Printf("saving catalog: %v", err)
	}
}

// resumeReceived restarts jobs that were accepted but interrupted by a shutdown.
// The server is powered off every night, so this is a normal path, not an edge.
func (s *server) resumeReceived() {
	for _, p := range s.cat.Pending() {
		if p.State == catalog.StateReceived {
			log.Printf("job %s: resuming after restart (%s)", p.ID, p.VideoID)
			go s.processAdd(p)
		}
	}
}

// addPlaylist accepts a playlist and hands the work to the background.
//
// Listing a playlist takes around a minute on modest hardware — longer than a
// phone shortcut will wait — so the reply goes out at once and the result
// arrives as a notification instead.
func (s *server) addPlaylist(w http.ResponseWriter, asJSON bool, rawURL string, force bool) {
	msg := "PLAYLIST RECEIVED\n"
	var pl playlistView
	if id, ok := youtube.ExtractPlaylistID(rawURL); ok {
		pl.ID = id
		// Same quick, bounded lookup as for a single song, so the reply names it.
		if name, err := s.probe.Preview(context.Background(), "https://www.youtube.com/playlist?list="+id); err == nil {
			msg += name + "\n"
			pl.Name = name
		} else {
			log.Printf("preview of playlist %s failed (the reply will not name it): %v", id, err)
		}
	}
	msg += "Checking it against your library."
	if s.notifier.Enabled() {
		msg += "\n\nYou will get a notification when the downloads are queued."
	} else {
		msg += "\n\nCheck /status or the logs to see the result."
	}
	replyAdd(w, asJSON, http.StatusAccepted, msg,
		addResult{Result: "playlist_received", Playlist: &pl, Notify: s.notifier.Enabled()})

	go s.processPlaylist(rawURL, force)
}

// processPlaylist queues every track of a playlist that is not already in the
// library.
//
// The expensive part is done once: a single flat listing yields the id, title
// and duration of every entry, and the duplicate check then runs entirely
// against the in-memory index. Downloads are handed to MeTube one by one, which
// has its own queue and paces them — there is no need to throttle here beyond
// not asking for more entries than the configured ceiling.
func (s *server) processPlaylist(rawURL string, force bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	pl, err := s.probe.ProbePlaylist(ctx, rawURL, s.cfg.maxPlaylistItems)
	if err != nil {
		log.Printf("listing playlist %s: %v", rawURL, err)
		s.notifier.Send(ctx, "Playlist failed",
			"Could not read the playlist:\n"+err.Error(),
			notify.PriorityHigh, "warning")
		return
	}
	if len(pl.Entries) == 0 {
		log.Printf("playlist %s is empty", rawURL)
		s.notifier.Send(ctx, "Playlist empty",
			"That playlist has no tracks in it.", notify.PriorityDefault, "warning")
		return
	}

	log.Printf("playlist %q: %d entries, checking against %d indexed tracks",
		pl.Title, len(pl.Entries), s.cat.Size())

	var queued, skipped, failed int
	var failures []string

	for _, e := range pl.Entries {
		if !force {
			if _, found := s.cat.FindByVideo(e.ID); found {
				skipped++
				continue
			}
			if _, dup := s.cat.FindDuplicate(e.Artist, e.Title, e.Duration); dup {
				skipped++
				continue
			}
			// Also skip anything queued moments ago, whether earlier in this
			// same playlist or by a previous request still downloading.
			if _, pending := s.cat.MatchPending(e.Artist, e.Title, e.Duration); pending {
				skipped++
				continue
			}
		}

		entry := catalog.Pending{
			VideoID:  e.ID,
			URL:      "https://www.youtube.com/watch?v=" + e.ID,
			Title:    e.Title,
			Artist:   e.Artist,
			Duration: e.Duration,
			Force:    force,
		}
		if err := s.mt.Add(ctx, entry.URL); err != nil {
			log.Printf("metube rejected %s (%s): %v", e.Title, e.ID, err)
			s.cat.Record(entry, catalog.StateFailed, "MeTube rejected the download: "+err.Error())
			failures = append(failures, e.Title)
			failed++
			continue
		}
		entry.State = catalog.StateQueued
		s.cat.AddPending(entry)
		queued++
	}

	if err := s.cat.Save(); err != nil {
		log.Printf("saving catalog: %v", err)
	}
	log.Printf("playlist %q: queued %d, skipped %d, failed %d", pl.Title, queued, skipped, failed)

	var b strings.Builder
	fmt.Fprintf(&b, "%d songs found\n", len(pl.Entries))
	fmt.Fprintf(&b, "downloading: %d\n", queued)
	fmt.Fprintf(&b, "already had: %d", skipped)
	if failed > 0 {
		fmt.Fprintf(&b, "\nfailed: %d", failed)
		for i, t := range failures {
			if i == 5 {
				fmt.Fprintf(&b, "\n  ...and %d more (see /status)", len(failures)-5)
				break
			}
			fmt.Fprintf(&b, "\n  - %s", t)
		}
	}
	if len(pl.Entries) == s.cfg.maxPlaylistItems {
		fmt.Fprintf(&b, "\n\nOnly the first %d entries were read.", s.cfg.maxPlaylistItems)
	}

	priority, tags := notify.PriorityDefault, "arrow_down"
	if queued == 0 {
		tags = "white_check_mark"
	}
	if failed > 0 {
		priority, tags = notify.PriorityHigh, "warning"
	}
	s.notifier.Send(ctx, pl.Title, b.String(), priority, tags)
}

// handleReindex rebuilds the catalog from the playlist.
func (s *server) handleReindex(w http.ResponseWriter, r *http.Request) {
	p := readParams(r)
	if !s.authorize(w, r, p) {
		return
	}
	if err := s.bootstrap(r.Context()); err != nil {
		reply(w, http.StatusBadGateway, "reindex failed: "+err.Error())
		return
	}
	reply(w, http.StatusOK, fmt.Sprintf("reindexed: %d tracks", s.cat.Size()))
}

// handleStatus reports what the service currently knows: every active job with
// its state, and the most recent finished ones with the reason they ended.
//
// With ?job=<id> (a job id or a YouTube video id) it describes just that one.
// With format=json it answers the same as data (see respond.go).
func (s *server) handleStatus(w http.ResponseWriter, r *http.Request) {
	p := readParams(r)
	if !s.authorize(w, r, p) {
		return
	}
	asJSON := wantsJSON(p)

	if ref := strings.TrimSpace(p["job"]); ref != "" {
		job, ok := s.cat.FindJob(ref)
		if !ok {
			replyError(w, asJSON, http.StatusNotFound, "job_not_found", "no job with that id")
			return
		}
		if asJSON {
			writeJSON(w, http.StatusOK, map[string]jobView{"job": viewJob(job)})
			return
		}
		var b strings.Builder
		writeJob(&b, job, true)
		reply(w, http.StatusOK, b.String())
		return
	}

	if asJSON {
		writeJSON(w, http.StatusOK, statusResult{
			Playlist: playlistView{ID: s.playlistID, Name: s.cfg.playlistName},
			Indexed:  s.cat.Size(),
			Active:   viewJobs(s.cat.Pending()),
			Recent:   viewJobs(s.cat.History(20)),
		})
		return
	}

	var b strings.Builder
	fmt.Fprintf(&b, "playlist: %s (%s)\n", s.cfg.playlistName, s.playlistID)
	fmt.Fprintf(&b, "indexed:  %d tracks\n", s.cat.Size())

	active := s.cat.Pending()
	fmt.Fprintf(&b, "\nIN PROGRESS: %d\n", len(active))
	for _, j := range active {
		writeJob(&b, j, false)
	}

	history := s.cat.History(20)
	fmt.Fprintf(&b, "\nRECENT (newest first):\n")
	for _, j := range history {
		writeJob(&b, j, false)
	}
	reply(w, http.StatusOK, b.String())
}

// writeJob prints one job. The long form adds everything known about it.
func writeJob(b *strings.Builder, j catalog.Pending, long bool) {
	name := strings.TrimSpace(j.Artist + " - " + j.Title)
	if j.Artist == "" {
		name = j.Title
	}
	fmt.Fprintf(b, "  [%s] %-10s %s\n", j.ID, j.State, name)
	if j.Reason != "" {
		fmt.Fprintf(b, "      why: %s\n", j.Reason)
	}
	if long {
		fmt.Fprintf(b, "      video:    %s\n", j.VideoID)
		if j.Filename != "" {
			fmt.Fprintf(b, "      file:     %s\n", j.Filename)
		}
		fmt.Fprintf(b, "      checks:   %d\n", j.Attempts)
		fmt.Fprintf(b, "      created:  %s\n", j.Created.Format(time.RFC3339))
		fmt.Fprintf(b, "      updated:  %s\n", j.Updated.Format(time.RFC3339))
	}
}

// runWorker files finished downloads into the playlist.
//
// It wakes on a timer but does nothing at all when no downloads are pending,
// so an idle service costs no CPU and makes no requests.
func (s *server) runWorker(ctx context.Context) {
	ticker := time.NewTicker(s.cfg.workerInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if s.cat.PendingCount() == 0 {
				continue
			}
			if err := s.filePending(ctx); err != nil {
				log.Printf("worker: %v", err)
			}
		}
	}
}

// filePending reconciles every active job with what MeTube and Navidrome say.
//
// MeTube answers "did the download work?" and Navidrome answers "is it in the
// library?". Asking only Navidrome — as this once did — cannot tell a failed
// download from a slow scan, so failures looked like songs that never arrived.
func (s *server) filePending(ctx context.Context) error {
	s.playlistMu.Lock()
	defer s.playlistMu.Unlock()

	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	// One request covers every job. If MeTube is unreachable the pass still
	// continues with Navidrome alone; losing the failure signal for a minute is
	// better than losing the filing.
	history, err := s.mt.History(ctx)
	if err != nil {
		log.Printf("worker: %v", err)
	}

	for _, p := range s.cat.Pending() {
		if p.State == catalog.StateReceived {
			continue // still being resolved by processAdd
		}
		if err != nil {
			continue
		}
		entry, known := history.Find(p.VideoID)
		if !known {
			continue
		}
		switch entry.Where {
		case metube.Errored:
			if done, ok := s.cat.Finish(p.VideoID, catalog.StateFailed, "MeTube: "+entry.Message); ok {
				log.Printf("job %s failed in MeTube: %s", done.ID, entry.Message)
				s.notifier.Send(ctx, "Failed: "+displayName(done), "MeTube: "+entry.Message+"\nJob "+done.ID,
					notify.PriorityHigh, "x")
			}
		case metube.Finished:
			s.cat.Update(p.VideoID, func(j *catalog.Pending) {
				j.State, j.Filename = catalog.StateDownloaded, entry.Filename
			})
		}
	}

	// A scan in progress means the results are not ready yet; wait it out
	// rather than piling another scan onto a slow machine.
	scanning, scanErr := s.nd.Scanning(ctx)
	if scanErr != nil {
		s.save()
		return fmt.Errorf("checking scan status: %w", scanErr)
	}
	if scanning {
		s.save()
		return nil
	}

	pending := s.cat.Pending()
	log.Printf("worker: looking for %d pending download(s)", len(pending))

	var toAdd []string
	var filedNow []catalog.Pending
	for _, p := range pending {
		if p.State == catalog.StateReceived {
			continue
		}
		// Still downloading in MeTube: there is nothing to find yet, and a slow
		// download must not use up the attempts reserved for a missing file.
		if err == nil {
			if e, known := history.Find(p.VideoID); known && e.Where == metube.InQueue {
				continue
			}
		}

		songID, found := s.locate(ctx, p)
		if !found {
			log.Printf("worker: %s - %s not in the library yet", p.Artist, p.Title)
			reason := "MeTube no longer lists it and it never appeared in Navidrome"
			if p.State == catalog.StateDownloaded {
				reason = "MeTube finished it (" + p.Filename +
					") but it never appeared in Navidrome — check the folder MeTube writes to and that Navidrome scans it"
			}
			if done, dropped := s.cat.BumpAttempt(p.VideoID, maxPendingAttempts, reason); dropped {
				log.Printf("job %s gave up: %s", done.ID, reason)
				s.notifier.Send(ctx, "Not found: "+displayName(done), reason+"\nJob "+done.ID,
					notify.PriorityHigh, "warning")
			}
			continue
		}
		toAdd = append(toAdd, songID)
		filedNow = append(filedNow, p)
		s.cat.ResolvePending(p.VideoID, songID)
		log.Printf("job %s filed %s - %s into %s", p.ID, p.Artist, p.Title, s.cfg.playlistName)
	}

	if len(toAdd) > 0 {
		if err := s.nd.AddToPlaylist(ctx, s.playlistID, toAdd); err != nil {
			// The tracks are already promoted into the index, so a failure here
			// would leave songs the service believes are filed but are not. Put
			// them back so the next pass retries, and say so.
			for _, p := range filedNow {
				s.cat.Unresolve(p)
			}
			s.save()
			return fmt.Errorf("adding to playlist: %w", err)
		}
		s.filedSinceIdle += len(toAdd)
	}

	// Downloads still in flight need the library refreshed before the next
	// pass can see them. One incremental scan covers all of them at once.
	//
	// Triggering a scan requires an admin account. Without one the service
	// still works — it just waits for Navidrome's own scheduled scan instead
	// of asking for one — so this is reported once and never treated as fatal.
	inFlight := s.cat.PendingCount()
	if inFlight > 0 {
		if err := s.nd.StartScan(ctx); err != nil && !s.scanWarned {
			s.scanWarned = true
			log.Printf("cannot trigger library scans (%v). "+
				"NAVIDROME_USER needs admin rights for that; without it downloads "+
				"are filed only after Navidrome's own scheduled scan runs.", err)
		}
	}

	// Notify once a batch settles: the queue drained, or a pass found nothing
	// new. Waiting for a full drain alone meant one stuck download silenced
	// every notification for the songs that did arrive.
	if s.filedSinceIdle > 0 && (inFlight == 0 || len(toAdd) == 0) {
		msg := fmt.Sprintf("%d song(s) added to %s.", s.filedSinceIdle, s.cfg.playlistName)
		if inFlight > 0 {
			msg += fmt.Sprintf("\n%d more still in progress (see /status).", inFlight)
		}
		msg += "\nSync Symfonium to get them."
		s.notifier.Send(ctx, "Music ready", msg, notify.PriorityDefault, "musical_note")
		s.filedSinceIdle = 0
	}

	return s.cat.Save()
}

// displayName is how a job is named in messages.
func displayName(j catalog.Pending) string {
	if j.Artist == "" {
		return j.Title
	}
	return j.Artist + " - " + j.Title
}

// locate finds the library entry for a finished download.
//
// The search runs twice when needed: once with the title as YouTube reported
// it, and once with the normalized form, because the tags MeTube writes do not
// always survive intact.
func (s *server) locate(ctx context.Context, p catalog.Pending) (string, bool) {
	// MeTube told us the exact file it wrote, which is a far stronger signal than
	// the YouTube title: channels with localized titles (the file says "Atrapado
	// y Libre", YouTube's oEmbed says "Trapped and Free") can never be found by
	// title. The library is listed only for downloads that already finished, and
	// the pool of those is tiny.
	if p.Filename != "" {
		if songs, err := s.nd.AllSongs(ctx, 500); err != nil {
			log.Printf("listing library for %q: %v", p.Filename, err)
		} else if id, ok := findByFile(songs, p.Filename); ok {
			return id, true
		}
	}

	queries := []string{p.Title}
	if normalized := catalog.Normalize(p.Title); normalized != "" && normalized != p.Title {
		queries = append(queries, normalized)
	}

	for _, query := range queries {
		songs, err := s.nd.SearchSongs(ctx, query, 25)
		if err != nil {
			log.Printf("searching for %q: %v", query, err)
			continue
		}
		for _, song := range songs {
			// MatchPending confirms the hit belongs to this download rather
			// than to some other track with a similar name.
			if videoID, ok := s.cat.MatchPending(song.Artist, song.Title, song.Duration); ok && videoID == p.VideoID {
				return song.ID, true
			}
		}
	}
	return "", false
}

// findByFile returns the library song whose file is the one MeTube wrote.
func findByFile(songs []navidrome.Song, filename string) (string, bool) {
	want := catalog.FileKey(filename)
	if want == "" {
		return "", false
	}
	for _, song := range songs {
		if catalog.FileKey(song.Path) == want {
			return song.ID, true
		}
	}
	return "", false
}

func reply(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	fmt.Fprintln(w, msg)
}

func formatDuration(seconds int) string {
	if seconds <= 0 {
		return "?"
	}
	return fmt.Sprintf("%d:%02d", seconds/60, seconds%60)
}

// runReconciler periodically makes the playlist match the library.
//
// The worker only files what this service itself queued and managed to
// recognise in Navidrome. Anything else — a song whose tags differ from what
// yt-dlp reported, one dropped into the music folder by hand, one filed while
// the service was down — never reached the playlist, and nothing noticed. This
// pass does not depend on recognising anything: it asks Navidrome for every
// song and the playlist for its contents, and adds the difference.
//
// RECONCILE_INTERVAL=0 turns it off, for anyone who prunes the playlist on
// purpose and does not want songs put back.
func (s *server) runReconciler(ctx context.Context) {
	if s.cfg.reconcileEvery <= 0 {
		log.Println("reconcile: disabled (RECONCILE_INTERVAL=0)")
		return
	}

	run := func() {
		if err := s.reconcile(ctx); err != nil {
			log.Printf("reconcile: %v", err)
		}
	}

	// Shortly after boot, once Navidrome has had a moment to settle.
	select {
	case <-ctx.Done():
		return
	case <-time.After(30 * time.Second):
		run()
	}

	ticker := time.NewTicker(s.cfg.reconcileEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			run()
		}
	}
}

// reconcile adds to the playlist every library song it is missing, and teaches
// the duplicate index about songs it had never seen.
func (s *server) reconcile(ctx context.Context) error {
	s.playlistMu.Lock()
	defer s.playlistMu.Unlock()

	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()

	// A scan in progress means the library is still changing underneath us.
	if scanning, err := s.nd.Scanning(ctx); err != nil {
		return fmt.Errorf("checking scan status: %w", err)
	} else if scanning {
		return nil
	}

	songs, err := s.nd.AllSongs(ctx, 500)
	if err != nil {
		return fmt.Errorf("listing library: %w", err)
	}
	pl, err := s.nd.GetPlaylist(ctx, s.playlistID)
	if err != nil {
		return fmt.Errorf("reading playlist: %w", err)
	}

	// An empty answer from a healthy-looking server would make every song look
	// missing from nothing, or the playlist look empty and trigger a mass add.
	if len(songs) == 0 {
		return errors.New("Navidrome returned an empty library, not touching the playlist")
	}

	missing := missingFromPlaylist(songs, pl.Entry)

	// Keep the duplicate index complete too: a song that never went through the
	// worker is otherwise invisible to the duplicate check.
	known := s.cat.SongIDs()
	learned := 0
	for _, song := range songs {
		if _, ok := known[song.ID]; ok {
			continue
		}
		s.cat.Add(catalog.Track{
			SongID:   song.ID,
			Title:    song.Title,
			Artist:   song.Artist,
			Duration: song.Duration,
		})
		learned++
	}

	if len(missing) > 0 {
		ids := make([]string, 0, len(missing))
		for _, song := range missing {
			ids = append(ids, song.ID)
		}
		// In chunks, so one huge request cannot time out and leave it half done.
		const chunk = 100
		for start := 0; start < len(ids); start += chunk {
			end := min(start+chunk, len(ids))
			if err := s.nd.AddToPlaylist(ctx, s.playlistID, ids[start:end]); err != nil {
				s.save()
				return fmt.Errorf("adding to playlist: %w", err)
			}
		}
		log.Printf("reconcile: added %d song(s) that were in the library but not in %s",
			len(missing), s.cfg.playlistName)

		var b strings.Builder
		fmt.Fprintf(&b, "%d song(s) were in your library but not in %s, so I added them:\n",
			len(missing), s.cfg.playlistName)
		for i, song := range missing {
			if i == 8 {
				fmt.Fprintf(&b, "...and %d more", len(missing)-8)
				break
			}
			fmt.Fprintf(&b, "- %s - %s\n", song.Artist, song.Title)
		}
		s.notifier.Send(ctx, "Playlist repaired", strings.TrimSpace(b.String()),
			notify.PriorityDefault, "wrench")
	}
	if learned > 0 {
		log.Printf("reconcile: indexed %d song(s) the duplicate check had not seen", learned)
	}

	s.save()
	return nil
}

// missingFromPlaylist returns the songs of the library that are not in the
// playlist, each at most once and in library order.
func missingFromPlaylist(library, playlist []navidrome.Song) []navidrome.Song {
	in := make(map[string]struct{}, len(playlist))
	for _, p := range playlist {
		in[p.ID] = struct{}{}
	}
	var out []navidrome.Song
	for _, song := range library {
		if _, ok := in[song.ID]; ok {
			continue
		}
		in[song.ID] = struct{}{} // a song repeated in the library is added once
		out = append(out, song)
	}
	return out
}
