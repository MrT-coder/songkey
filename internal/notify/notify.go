// Package notify pushes short status messages to a phone through ntfy.
//
// Notifications are best-effort by design: this service exists to save the user
// a trip to the computer, and failing to deliver a status message is never a
// reason to fail the work it describes.
package notify

import (
	"context"
	"log"
	"net/http"
	"strings"
	"time"
)

// Priority levels ntfy understands.
const (
	PriorityDefault = "default"
	PriorityHigh    = "high"
)

// Notifier sends messages to an ntfy topic. A Notifier with an empty URL is a
// working no-op, so callers never need to check whether notifications are on.
type Notifier struct {
	url  string
	http *http.Client
}

// New builds a notifier for a full ntfy topic URL, e.g.
// https://ntfy.sh/<random-topic>. An empty url disables notifications.
func New(url string) *Notifier {
	return &Notifier{
		url:  strings.TrimRight(url, "/"),
		http: &http.Client{Timeout: 10 * time.Second},
	}
}

// Enabled reports whether messages will actually be sent.
func (n *Notifier) Enabled() bool { return n.url != "" }

// Send delivers a notification. Errors are logged, never returned: no caller
// should have to decide what to do when a notification fails.
func (n *Notifier) Send(ctx context.Context, title, message, priority, tags string) {
	if n.url == "" {
		return
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.url, strings.NewReader(message))
	if err != nil {
		log.Printf("notify: building request: %v", err)
		return
	}
	req.Header.Set("Title", title)
	if priority != "" {
		req.Header.Set("Priority", priority)
	}
	if tags != "" {
		req.Header.Set("Tags", tags)
	}

	resp, err := n.http.Do(req)
	if err != nil {
		log.Printf("notify: %v", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		log.Printf("notify: unexpected status %s", resp.Status)
	}
}
