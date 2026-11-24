package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/MrT-coder/songkey/internal/catalog"
)

// Replies come in two shapes. Plain text is the default, because the phone
// shortcut shows the body as it is. Anything else, such as a player or a
// script, asks for JSON with format=json and gets the same outcome as data.
//
// Every JSON reply keeps the HTTP status of its text twin. A failure carries
// {"error": {"code", "message"}}; the codes are stable, the messages are not.

// wantsJSON reports whether the request asked for a JSON reply.
func wantsJSON(p map[string]string) bool {
	return p["format"] == "json"
}

// jobView is the public JSON form of a job.
type jobView struct {
	ID       string    `json:"id"`
	State    string    `json:"state"`
	VideoID  string    `json:"video_id"`
	URL      string    `json:"url,omitempty"`
	Title    string    `json:"title"`
	Artist   string    `json:"artist"`
	Duration int       `json:"duration"` // seconds, 0 when not known yet
	Reason   string    `json:"reason,omitempty"`
	Filename string    `json:"filename,omitempty"`
	Force    bool      `json:"force"`
	Attempts int       `json:"attempts"`
	Created  time.Time `json:"created"`
	Updated  time.Time `json:"updated"`
}

func viewJob(j catalog.Pending) jobView {
	return jobView{
		ID: j.ID, State: string(j.State), VideoID: j.VideoID, URL: j.URL,
		Title: j.Title, Artist: j.Artist, Duration: j.Duration, Reason: j.Reason,
		Filename: j.Filename, Force: j.Force, Attempts: j.Attempts,
		Created: j.Created, Updated: j.Updated,
	}
}

func viewJobs(list []catalog.Pending) []jobView {
	out := make([]jobView, len(list)) // never nil, so it encodes as []
	for i, j := range list {
		out[i] = viewJob(j)
	}
	return out
}

// trackView is the public JSON form of a song already in the library.
type trackView struct {
	VideoID  string `json:"video_id,omitempty"`
	Title    string `json:"title"`
	Artist   string `json:"artist"`
	Duration int    `json:"duration"`
}

// addResult is the JSON reply of /add for every outcome that is not an error.
type addResult struct {
	// Result is one of queued, already_queued, already_have, playlist_received.
	Result   string        `json:"result"`
	Message  string        `json:"message"`
	Job      *jobView      `json:"job,omitempty"`
	Track    *trackView    `json:"track,omitempty"`
	Playlist *playlistView `json:"playlist,omitempty"`
	// Notify tells whether the outcome will also arrive as a notification.
	Notify bool `json:"notify"`
}

type playlistView struct {
	ID   string `json:"id,omitempty"`
	Name string `json:"name,omitempty"`
}

// statusResult is the JSON reply of /status without a job parameter.
type statusResult struct {
	Playlist playlistView `json:"playlist"`
	Indexed  int          `json:"indexed"`
	Active   []jobView    `json:"active"`
	Recent   []jobView    `json:"recent"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("writing JSON reply: %v", err)
	}
}

// replyError answers a failure as text or as a JSON error with a stable code.
func replyError(w http.ResponseWriter, asJSON bool, status int, code, msg string) {
	if !asJSON {
		reply(w, status, msg)
		return
	}
	type apiError struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	writeJSON(w, status, map[string]apiError{"error": {Code: code, Message: msg}})
}

// replyAdd answers a successful /add as text (msg) or as JSON (res, with msg
// as its message).
func replyAdd(w http.ResponseWriter, asJSON bool, status int, msg string, res addResult) {
	if !asJSON {
		reply(w, status, msg)
		return
	}
	res.Message = msg
	writeJSON(w, status, res)
}

// replyQueued answers a single song accepted for download. named is false
// when the quick title lookup failed, so the text reply leaves the name out.
func replyQueued(w http.ResponseWriter, asJSON bool, job catalog.Pending, name string, named, notify bool) {
	msg := "QUEUED\n"
	if named {
		msg += name + "\n"
	}
	msg += fmt.Sprintf("Job %s\n\n", job.ID)
	if notify {
		msg += "You will get a notification with the result."
	} else {
		msg += "Check /status?job=" + job.ID + " for the result."
	}
	v := viewJob(job)
	replyAdd(w, asJSON, http.StatusAccepted, msg, addResult{Result: "queued", Job: &v, Notify: notify})
}
