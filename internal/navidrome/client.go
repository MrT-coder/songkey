// Package navidrome is a minimal Subsonic API client covering only what this
// service needs: reading playlists, appending songs to one, and triggering an
// incremental library scan.
package navidrome

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// apiVersion is the Subsonic protocol level this client speaks.
const apiVersion = "1.16.1"

// clientName identifies this service in Navidrome's logs.
const clientName = "songkey"

// Client talks to a Navidrome server over the Subsonic API.
type Client struct {
	baseURL  string
	user     string
	password string
	http     *http.Client
}

// Song is a track as Navidrome knows it.
type Song struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Artist   string `json:"artist"`
	Album    string `json:"album"`
	Duration int    `json:"duration"`
	Path     string `json:"path"`
}

// Playlist is a playlist summary.
type Playlist struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	SongCount int    `json:"songCount"`
	Entry     []Song `json:"entry"`
}

// New builds a client. baseURL is the server root, e.g. http://host:4533.
func New(baseURL, user, password string, timeout time.Duration) *Client {
	return &Client{
		baseURL:  baseURL,
		user:     user,
		password: password,
		http:     &http.Client{Timeout: timeout},
	}
}

// envelope is the wrapper every Subsonic response carries.
type envelope struct {
	Response struct {
		Status string `json:"status"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
		Playlists struct {
			Playlist []Playlist `json:"playlist"`
		} `json:"playlists"`
		Playlist      Playlist `json:"playlist"`
		SearchResult3 struct {
			Song []Song `json:"song"`
		} `json:"searchResult3"`
		ScanStatus struct {
			Scanning bool `json:"scanning"`
			Count    int  `json:"count"`
		} `json:"scanStatus"`
	} `json:"subsonic-response"`
}

// auth builds the token/salt credentials Subsonic expects. A fresh salt is
// generated per request, which is what the protocol asks for.
func (c *Client) auth() url.Values {
	salt := strconv.FormatInt(rand.Int63(), 36)
	sum := md5.Sum([]byte(c.password + salt))

	v := url.Values{}
	v.Set("u", c.user)
	v.Set("t", hex.EncodeToString(sum[:]))
	v.Set("s", salt)
	v.Set("v", apiVersion)
	v.Set("c", clientName)
	v.Set("f", "json")
	return v
}

// call performs one Subsonic request and decodes the envelope.
func (c *Client) call(ctx context.Context, method string, params url.Values) (*envelope, error) {
	v := c.auth()
	for key, values := range params {
		for _, value := range values {
			v.Add(key, value)
		}
	}

	endpoint := fmt.Sprintf("%s/rest/%s?%s", c.baseURL, method, v.Encode())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", method, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: unexpected status %s", method, resp.Status)
	}

	var env envelope
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return nil, fmt.Errorf("%s: decoding response: %w", method, err)
	}
	if env.Response.Error != nil {
		return nil, fmt.Errorf("%s: navidrome error %d: %s",
			method, env.Response.Error.Code, env.Response.Error.Message)
	}
	return &env, nil
}

// Ping verifies credentials and reachability.
func (c *Client) Ping(ctx context.Context) error {
	_, err := c.call(ctx, "ping", nil)
	return err
}

// FindPlaylist resolves a playlist by its display name.
func (c *Client) FindPlaylist(ctx context.Context, name string) (Playlist, error) {
	env, err := c.call(ctx, "getPlaylists", nil)
	if err != nil {
		return Playlist{}, err
	}
	for _, p := range env.Response.Playlists.Playlist {
		if p.Name == name {
			return p, nil
		}
	}
	return Playlist{}, fmt.Errorf("playlist %q not found", name)
}

// GetPlaylist returns a playlist with all of its songs.
func (c *Client) GetPlaylist(ctx context.Context, id string) (Playlist, error) {
	env, err := c.call(ctx, "getPlaylist", url.Values{"id": {id}})
	if err != nil {
		return Playlist{}, err
	}
	return env.Response.Playlist, nil
}

// AddToPlaylist appends songs to an existing playlist.
//
// This deliberately uses updatePlaylist rather than createPlaylist: passing
// song ids to createPlaylist defines the playlist's entire contents, which
// would wipe every track already in it.
func (c *Client) AddToPlaylist(ctx context.Context, playlistID string, songIDs []string) error {
	if len(songIDs) == 0 {
		return nil
	}
	params := url.Values{"playlistId": {playlistID}}
	for _, id := range songIDs {
		params.Add("songIdToAdd", id)
	}
	_, err := c.call(ctx, "updatePlaylist", params)
	return err
}

// SearchSongs looks for tracks matching a free-text query.
//
// Navidrome's search3 is a simple autocomplete rather than a full query
// language, so the caller must treat the results as candidates and confirm the
// right one itself instead of trusting the first hit.
func (c *Client) SearchSongs(ctx context.Context, query string, limit int) ([]Song, error) {
	env, err := c.call(ctx, "search3", url.Values{
		"query":       {query},
		"songCount":   {strconv.Itoa(limit)},
		"artistCount": {"0"},
		"albumCount":  {"0"},
	})
	if err != nil {
		return nil, err
	}
	return env.Response.SearchResult3.Song, nil
}

// AllSongs returns every song in the library.
//
// An empty search3 query matches everything, which is how Subsonic clients
// sync a whole library. It is read in pages so a large library never produces
// one enormous response on a machine with little memory.
func (c *Client) AllSongs(ctx context.Context, pageSize int) ([]Song, error) {
	if pageSize <= 0 {
		pageSize = 500
	}
	var all []Song
	for offset := 0; ; offset += pageSize {
		env, err := c.call(ctx, "search3", url.Values{
			"query":       {`""`},
			"songCount":   {strconv.Itoa(pageSize)},
			"songOffset":  {strconv.Itoa(offset)},
			"artistCount": {"0"},
			"albumCount":  {"0"},
		})
		if err != nil {
			return nil, err
		}
		page := env.Response.SearchResult3.Song
		all = append(all, page...)
		if len(page) < pageSize {
			return all, nil
		}
	}
}

// StartScan asks Navidrome to pick up new files. The scan is always
// incremental — a full scan re-reads the entire library and is far too
// expensive to run routinely on modest hardware.
func (c *Client) StartScan(ctx context.Context) error {
	_, err := c.call(ctx, "startScan", url.Values{"fullScan": {"false"}})
	return err
}

// Scanning reports whether a library scan is currently running.
func (c *Client) Scanning(ctx context.Context) (bool, error) {
	env, err := c.call(ctx, "getScanStatus", nil)
	if err != nil {
		return false, err
	}
	return env.Response.ScanStatus.Scanning, nil
}
