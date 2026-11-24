# songkey

**Answer one question reliably: "do I already have this song?" — before you
download it.**

songkey is a small self-hosted service that sits in front of your music
downloader. You send it a YouTube link (a song or a whole playlist). It checks
the link against your existing library, refuses duplicates, hands only the new
songs to the downloader, and files the finished downloads into a library
playlist.

It is written in Go, has no external dependencies, and idles at roughly 20 MB
of RAM, so it runs happily on old hardware.

## The idea

I listen to music from my own server, and I add songs to it from my phone: I
find something on YouTube, tap *Share*, and a shortcut sends the link to my
home server. The problem was that after a few hundred songs I no longer knew
what I already had, so the library filled up with duplicates — the same song
from the official video, the lyric video and the topic channel.

songkey is the piece in the middle that remembers. A day of use looks like
this:

1. Share a YouTube link (or a whole playlist) to a phone shortcut.
2. songkey answers in about a second: *queued*, *already have it*, or *already
   on its way*.
3. Only new songs are downloaded. When each one lands in the library, it is
   added to a playlist and a notification arrives on the phone.

The same endpoints answer in JSON (`format=json`), so a player or a dashboard
can submit links and follow their progress too.

## Project status

This is a **personal project** that I use every day on my own home server. I am
publishing it because it may be useful to someone running a similar setup
(MeTube + Navidrome on modest hardware), and as part of my portfolio.

- It is **under active development**. Until version 1.0, configuration and
  replies may change between releases; the changes are listed in each release.
- It is tested against my own setup first. Issues and pull requests are
  welcome, but support is best effort.

## Where it fits

songkey is a gate, not a downloader and not a player. It talks to two things you
probably already run:

- **[MeTube](https://github.com/alexta69/metube)** downloads the audio
  (it wraps `yt-dlp`). songkey just tells it what to fetch.
- **[Navidrome](https://www.navidrome.org/)** is your music library. songkey
  reads it to learn what you already own, and appends new songs to a playlist.

```
 phone shortcut / script / your own front-end
                  |
                  v
              songkey  ---- "already have it?" ---->  rejects duplicates
                  |
                  v  (only new songs)
               MeTube  -->  shared music folder  -->  Navidrome
                  ^                                        |
                  +----- songkey adds them to a playlist --+
```

MeTube and Navidrome are the supported backends today. The code keeps them in
separate packages (`internal/metube`, `internal/navidrome`) so other downloaders
and libraries can be added later.

> **Status:** early (v0.x). It works in daily use against MeTube + Navidrome,
> but the HTTP API is not yet versioned or stable.

## Duplicate detection

Three layers, cheapest first:

| Layer | Compares | Cost | Catches |
|---|---|---|---|
| 1 | YouTube video id | free | the same link shared twice |
| 2 | normalized title + duration | one `yt-dlp` metadata call | the same song under a different title |
| 3 | you | zero | everything else — the answer says what it found and waits |

Layer 2 strips what YouTube titles carry but songs do not: `(Official Video)`,
`[HD]`, `Lyrics`, `Remastered 2023`, `feat. …`, accents and punctuation. It also
trusts an `Artist - Song` split in the title over the channel name, so the
artist's own upload, the `- Topic` auto-channel and a fan re-upload all reduce
to the same key.

Duration is a guard, not a hint: a title match whose runtime differs by more
than four seconds is not treated as the same recording, which keeps live
versions and covers out of the duplicate bucket.

Nothing is silently discarded. A suspected duplicate is reported back with both
titles and the reason, and `&force=1` overrides it.

## Roadmap

- **Stable, versioned HTTP API** (`/v1/...`). JSON replies and job ids already
  exist (`format=json`); the next step is freezing them under a version.
- **Acoustic fingerprinting** (Chromaprint) as a fourth layer, computed *after*
  download. A fingerprint needs the audio, so it cannot replace the
  pre-download layers; it catches what slipped through.
- **Library bootstrap** that fingerprints the songs you already have.
- **More backends:** direct `yt-dlp`, other libraries.
- Artist watchlist that downloads new releases automatically.

## Why it is shaped this way

It was built for a home server with two cores from 2007 and under 2 GB of RAM,
so the design avoids work rather than parallelising it:

- **The free check runs first.** A link is reduced to its YouTube id with a
  regular expression and looked up in memory. A repeated link is rejected in
  microseconds, with no network call and no `yt-dlp` process.
- **`yt-dlp` only runs for genuinely new links**, and only to read metadata. It
  is a standalone binary invoked on demand, so no Python runtime stays resident.
- **The worker does nothing while idle.** It wakes on a timer, sees no pending
  downloads and goes back to sleep without making a request.
- **Library scans are incremental and batched.** One scan covers every pending
  download; a full scan is never requested.
- **No music files are mounted.** songkey speaks only HTTP, so it cannot touch
  the library even by accident.

## Quick start

Requirements: Docker, a running MeTube and a running Navidrome, and a Navidrome
user. The Navidrome music folder and MeTube's download folder must be the same
directory, so finished downloads show up in the library.

### With the published image

Every release publishes `ghcr.io/mrt-coder/songkey` for `linux/amd64` and
`linux/arm64` (a Raspberry Pi works). Tags: `latest`, `0.1` and `0.1.0`.

```yaml
services:
  songkey:
    image: ghcr.io/mrt-coder/songkey:0.1
    container_name: songkey
    restart: unless-stopped
    ports:
      - "8090:8090"
    volumes:
      - ./data:/data          # the duplicate index; keep it
    extra_hosts:
      - "host.docker.internal:host-gateway"
    env_file:
      - .env
    mem_limit: 256m
```

```bash
cp env.example .env
nano .env          # Navidrome URL/user/password, MeTube URL, API_TOKEN
docker compose up -d
docker compose logs -f   # the first line names the running version
```

Pin `0.1` rather than `latest` while the project is below 1.0: a minor version
may change configuration or replies, and the release notes say how.

### On a server too slow to build images

`songkey.yml` takes a different route, made for the 2007 machine songkey was
born on: the image only holds yt-dlp, and the songkey binary is mounted from
`./bin`, so an update is a file copy instead of an image rebuild. Get the
binary from the [releases page](https://github.com/MrT-coder/songkey/releases)
(`songkey-linux-amd64` or `songkey-linux-arm64`, renamed to `bin/songkey`) or
build it yourself (below), then:

```bash
cp env.example .env
nano .env
docker compose -f songkey.yml up -d
docker compose -f songkey.yml logs -f
```

On first run the catalog is empty, so songkey seeds it from the playlist named
in `NAVIDROME_PLAYLIST` — one API call instead of a filesystem scan. The log
reports how many tracks it indexed.

### Building the binary

The compose file mounts a prebuilt binary instead of building inside the
container, because compiling Go on very small hardware is slow. Cross-compile on
your own machine:

```powershell
.\build.ps1              # runs the tests, then produces bin/songkey
```

```bash
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -ldflags="-s -w" -o bin/songkey ./cmd/server
```

`Dockerfile.selfbuild` is the multi-stage, multi-architecture build behind the
published image.

### Updating

The binary is mounted from `./bin`, so shipping new code never needs an image
rebuild. It does need the container stopped: Linux refuses to overwrite a
running executable ("text file busy").

```bash
docker compose -f songkey.yml stop
# copy the new bin/songkey over the old one
docker compose -f songkey.yml start
```

Always finish with `start`. With `restart: unless-stopped`, a container you left
stopped will not come back when the machine reboots.

### Running on OpenMediaVault

The OMV Compose plugin does not detect files copied by hand. Use
*Services → Compose → Files → Import*, and keep the folder name and the `.yml`
name identical (`songkey/songkey.yml`).

## Use

```
http://<host>:8090/add?url=<SHARED_URL>&token=<API_TOKEN>
```

Replies are plain text so a phone shortcut can show them directly:

```
QUEUED
Bad Bunny - Tití Me Preguntó (Video Oficial)
Job 3fa91c

You will get a notification with the result.
```

A playlist link is acknowledged the same way, naming the playlist:

```
PLAYLIST RECEIVED
Un Verano Sin Ti
Checking it against your library.
```

The name comes from YouTube's oEmbed endpoint, which answers in well under a
second and is cut off after four; if it is slow or unavailable the reply simply
omits the name. The request is acknowledged immediately and the slow work (reading metadata,
duplicate check, handing off to MeTube) continues in the background, so the
shortcut never times out. Every request ends in exactly one outcome, always with
a reason, delivered as a notification and visible in `/status`:

| State | Meaning |
|---|---|
| `received` | accepted, resolving metadata |
| `queued` | MeTube accepted it and is downloading |
| `downloaded` | MeTube finished; waiting for Navidrome to index the file |
| `filed` | in the playlist |
| `duplicate` | skipped on purpose; the reason names the existing track |
| `failed` | something broke; the reason says what (MeTube's own error, "never appeared in Navidrome", …) |

A link whose video id is already in the library is still rejected instantly,
since that check costs nothing:

```
ALREADY HAVE IT
Bad Bunny - Tití Me Preguntó

Add &force=1 to download anyway.
```

A playlist link queues everything in it that is missing and answers
asynchronously (expanding a long playlist can take a while on slow hardware):

```
PLAYLIST: Un Verano Sin Ti
23 songs found

downloading: 5
already had: 18
```

A link that names both a video and a playlist — a song shared while a playlist
happened to be open — is treated as that single song.

## Keeping the playlist in sync

The worker only files what songkey itself queued and managed to recognise in
Navidrome. A song whose tags differ from what yt-dlp reported, or one dropped
into the music folder by hand, would otherwise stay out of the playlist
forever. So, shortly after boot and then every `RECONCILE_INTERVAL` (default
`1h`), songkey asks Navidrome for every song and for the playlist, adds the
difference — never a song already there — and sends a notification listing what
it repaired. Set `RECONCILE_INTERVAL=0` to turn it off if you remove songs from
the playlist on purpose.

## Exposing it publicly

Every endpoint except `/health` requires `API_TOKEN`. If the service sits behind
a public hostname (a Cloudflare Tunnel, for instance):

- Use a long random token: `openssl rand -hex 32`. An empty `API_TOKEN` leaves
  the service open to anyone who can reach it.
- Send it as a header, `Authorization: Bearer <token>`, instead of `?token=`.
  URLs end up in access logs; headers do not.
- Tokens are compared in constant time, and a client that sends ten wrong ones
  within ten minutes is refused for fifteen (`429`, with `Retry-After`) — even if
  its next attempt is correct, so a guesser gains nothing by continuing. The
  client is identified by `CF-Connecting-IP` when present, otherwise
  `X-Forwarded-For`, otherwise the socket address.
- Publish only what the phone needs. In Cloudflare, allow just `/add` and
  `/health` and leave `/status` and `/reindex` reachable from the LAN only.

## Endpoints

| Endpoint | Purpose |
|---|---|
| `GET/POST /add?url=…` | check, enqueue and file. `&force=1` overrides a duplicate verdict |
| `POST /reindex` | rebuild the catalog from the playlist |
| `GET /status` | jobs in progress and the 20 most recent finished ones, each with its state and reason |
| `GET /status?job=<id>` | everything known about one job (by job id or YouTube video id) |
| `GET /health` | liveness |

### JSON replies

`/add` and `/status` answer in plain text by default. Add `format=json` (query
parameter, form field or JSON body field) to get the same outcome as data, for
a player, a dashboard or a script. The HTTP status is the same in both shapes.

`/add` returns a `result` plus the details that go with it:

| `result` | Status | Also carries |
|---|---|---|
| `queued` | 202 | `job` |
| `already_queued` | 200 | `job` (the one already in flight) |
| `already_have` | 409 | `track` (the song in your library) |
| `playlist_received` | 202 | `playlist` (`id`, `name`) |

```json
{
  "result": "queued",
  "message": "QUEUED\nBad Bunny - Tití Me Preguntó\nJob 3fa91c\n\n...",
  "job": {
    "id": "3fa91c", "state": "received", "video_id": "Cr8K88UcO5s",
    "url": "https://www.youtube.com/watch?v=Cr8K88UcO5s",
    "title": "Tití Me Preguntó", "artist": "", "duration": 0,
    "force": false, "attempts": 0,
    "created": "2026-11-24T10:00:00Z", "updated": "2026-11-24T10:00:00Z"
  },
  "notify": true
}
```

`message` is the text reply, ready to show to a person. `notify` says whether
the outcome will also arrive as a notification; otherwise poll
`/status?job=<id>&format=json`, which returns `{"job": {...}}`.

A job's `state` is one of `received`, `queued`, `downloaded`, `filed`,
`failed` or `duplicate`; `reason` explains the last two.

`/status?format=json` returns:

```json
{
  "playlist": { "id": "…", "name": "songkey" },
  "indexed": 1212,
  "active": [ { "id": "3fa91c", "state": "queued", "...": "..." } ],
  "recent": [ { "id": "9b02de", "state": "failed", "reason": "…" } ]
}
```

Errors carry a stable `code` and a human `message`:

```json
{ "error": { "code": "invalid_url", "message": "could not read a YouTube video id from that link" } }
```

| `code` | Status |
|---|---|
| `missing_url`, `invalid_url`, `channel_not_supported` | 400 |
| `unauthorized` | 401 |
| `job_not_found` | 404 |
| `too_many_attempts` | 429 |

## Configuration

See [`env.example`](env.example). Notable variables: `NAVIDROME_URL`,
`NAVIDROME_USER`, `NAVIDROME_PASS`, `NAVIDROME_PLAYLIST`, `METUBE_URL`,
`API_TOKEN`, `NTFY_URL` (optional phone notifications), `LISTEN_ADDR`.

## Security notes

- Set `API_TOKEN` before exposing the service beyond a network you trust. Treat
  it like a password and never commit your `.env`.
- An ntfy topic name is effectively a secret: anyone who knows it can read your
  notifications. Use a long random one.
- Songs are appended with Subsonic `updatePlaylist`, never `createPlaylist`: the
  latter defines a playlist's entire contents and would erase every existing
  track.

## Notes

- `./data/catalog.json` holds the index. Keep the volume — losing it means every
  duplicate check starts from scratch until the next reindex.
- songkey is not affiliated with MeTube, Navidrome or YouTube. Only download
  content you have the right to download.

## License

[MIT](LICENSE)
