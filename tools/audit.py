#!/usr/bin/env python3
"""Read-only audit: which songs are out of sync between disk, Navidrome and the playlist.

Reads Navidrome credentials from a .env file and prints only results, never the
credentials. It changes nothing anywhere: no scans, no playlist edits.

Usage:
    python tools/audit.py <path-to-.env> [--host HOST] [--user USER] [--key KEY] [--playlist NAME]

Server access comes from flags or, when omitted, from these keys in the same .env:
    SONGKEY_SSH_HOST, SONGKEY_SSH_USER, SONGKEY_SSH_KEY,
    SONGKEY_MUSIC_DIR (the music folder), SONGKEY_APPDATA_DIR (holds catalog.json),
    NAVIDROME_PLAYLIST.
"""
import argparse
import hashlib
import json
import os
import secrets
import re
import subprocess
import sys
import unicodedata
import urllib.parse
import urllib.request

AUDIO = ("mp3", "m4a", "flac", "opus", "ogg", "wav", "aac")


def load_env(path):
    out = {}
    with open(path, encoding="utf-8") as f:
        for line in f:
            line = line.strip()
            if not line or line.startswith("#") or "=" not in line:
                continue
            k, v = line.split("=", 1)
            out[k.strip()] = v.strip().strip('"').strip("'")
    return out


def pick(env, *names):
    for n in names:
        if env.get(n):
            return env[n]
    return ""


class Subsonic:
    def __init__(self, base, user, password):
        self.base, self.user, self.password = base.rstrip("/"), user, password

    def call(self, endpoint, **params):
        salt = secrets.token_hex(6)
        token = hashlib.md5((self.password + salt).encode()).hexdigest()
        q = {"u": self.user, "t": token, "s": salt, "v": "1.16.1", "c": "songkey-audit", "f": "json"}
        q.update(params)
        # doseq: updatePlaylist takes songIdToAdd once per song.
        url = f"{self.base}/rest/{endpoint}?{urllib.parse.urlencode(q, doseq=True)}"
        with urllib.request.urlopen(url, timeout=30) as r:
            body = json.load(r)["subsonic-response"]
        if body.get("status") != "ok":
            raise RuntimeError(f"{endpoint}: {body.get('error', {}).get('message', 'error')}")
        return body


def add_server_args(ap):
    """Flags shared by the tools; unset ones fall back to the .env (see module docstring)."""
    ap.add_argument("--host")
    ap.add_argument("--user")
    ap.add_argument("--key")
    ap.add_argument("--music-dir")
    ap.add_argument("--appdata-dir")


def resolve_server_args(args, env):
    args.host = args.host or env.get("SONGKEY_SSH_HOST")
    args.user = args.user or env.get("SONGKEY_SSH_USER")
    args.key = args.key or env.get("SONGKEY_SSH_KEY")
    args.music_dir = args.music_dir or env.get("SONGKEY_MUSIC_DIR")
    args.appdata_dir = args.appdata_dir or env.get("SONGKEY_APPDATA_DIR")
    missing = [n for n, v in (("SONGKEY_SSH_HOST / --host", args.host),
                              ("SONGKEY_MUSIC_DIR / --music-dir", args.music_dir),
                              ("SONGKEY_APPDATA_DIR / --appdata-dir", args.appdata_dir)) if not v]
    if missing:
        sys.exit("missing settings: " + ", ".join(missing))


def ssh(args, cmd):
    base = ["ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=10"]
    if args.key:
        base += ["-i", os.path.expanduser(args.key), "-o", "IdentitiesOnly=yes"]
    target = f"{args.user}@{args.host}" if args.user else args.host
    base += [target, cmd]
    # Windows decodes subprocess output as cp1252 by default, which chokes on
    # the accents and symbols in song file names.
    r = subprocess.run(base, capture_output=True, text=True, timeout=60,
                       encoding="utf-8", errors="replace")
    return (r.stdout or "") if r.returncode == 0 else ""


def base_name(p):
    return p.replace("\\", "/").rsplit("/", 1)[-1]


def norm(name):
    """Comparable form of a file name.

    yt-dlp stores characters that are illegal on some filesystems as full-width
    look-alikes (｜ ？ ＂ ⧸), while Navidrome reports the plain ones. Comparing raw
    text made every such song look missing from one side; keeping only letters
    and digits after NFKC removes the difference.
    """
    name = unicodedata.normalize("NFKC", name).lower()
    return re.sub(r"[\W_]+", "", name)


def main():
    # Song titles carry characters the Windows console codepage cannot print.
    sys.stdout.reconfigure(encoding="utf-8", errors="replace")
    ap = argparse.ArgumentParser()
    ap.add_argument("env")
    add_server_args(ap)
    ap.add_argument("--playlist")
    ap.add_argument("--apply", action="store_true",
                    help="WRITE: add to the playlist the songs it is missing (never duplicates)")
    args = ap.parse_args()

    env = load_env(args.env)
    resolve_server_args(args, env)
    args.playlist = args.playlist or env.get("NAVIDROME_PLAYLIST") or "songkey"
    user = pick(env, "NAVIDROME_USER", "ND_USER", "NAVIDROME_USERNAME")
    pw = pick(env, "NAVIDROME_PASS", "NAVIDROME_PASSWORD", "ND_PASSWORD", "ND_PASS")
    url = pick(env, "NAVIDROME_URL", "ND_URL", "NAVIDROME_BASE_URL")
    if not user or not pw:
        print("No encontre usuario/clave de Navidrome. Variables presentes en el .env:")
        print("  " + ", ".join(sorted(env)))
        sys.exit(1)

    candidates = [c for c in (url, f"http://{args.host}:4533") if c]
    nd = None
    for c in candidates:
        try:
            s = Subsonic(c, user, pw)
            s.call("ping")
            nd = s
            print(f"Navidrome: conectado a {c}")
            break
        except Exception as e:  # noqa: BLE001
            print(f"Navidrome: {c} no respondio ({type(e).__name__})")
    if nd is None:
        sys.exit(1)

    # --- everything Navidrome knows -------------------------------------------
    songs, offset = [], 0
    while True:
        res = nd.call("search3", query='""', songCount=500, songOffset=offset,
                      artistCount=0, albumCount=0)
        batch = res.get("searchResult3", {}).get("song", [])
        songs += batch
        if len(batch) < 500:
            break
        offset += 500
    by_id = {s["id"]: s for s in songs}
    print(f"\nCanciones en Navidrome: {len(songs)}")

    scan = nd.call("getScanStatus").get("scanStatus", {})
    print(f"Escaneo: scanning={scan.get('scanning')} count={scan.get('count')} "
          f"ultimo={scan.get('lastScan', '?')}")

    # --- the playlist ------------------------------------------------------------
    pls = nd.call("getPlaylists").get("playlists", {}).get("playlist", [])
    target = next((p for p in pls if p["name"] == args.playlist), None)
    if not target:
        print(f"No existe la playlist {args.playlist}. Existen: {[p['name'] for p in pls]}")
        sys.exit(1)
    entries = nd.call("getPlaylist", id=target["id"])["playlist"].get("entry", [])
    in_playlist = {e["id"] for e in entries}
    print(f"Playlist {args.playlist}: {len(entries)} canciones")

    missing = [s for s in songs if s["id"] not in in_playlist]
    print(f"\n[A] En Navidrome pero NO en {args.playlist}: {len(missing)}")
    for s in sorted(missing, key=lambda x: x.get("created", ""), reverse=True)[:60]:
        print(f"    {s.get('created', '')[:10]}  {s.get('artist', '?')} - {s.get('title', '?')}   [{base_name(s.get('path', ''))}]")

    # --- the disk ------------------------------------------------------------------
    pattern = " -o ".join(f'-iname "*.{e}"' for e in AUDIO)
    listing = ssh(args, f'find "{args.music_dir}" -type f \\( {pattern} \\) -printf "%T@|%p\\n" 2>/dev/null')
    disk = {}
    for line in listing.splitlines():
        if "|" in line:
            ts, p = line.split("|", 1)
            disk[base_name(p)] = float(ts)
    if disk:
        disk_keys = {norm(n) for n in disk}
        known = {norm(base_name(s.get("path", ""))): base_name(s.get("path", "")) for s in songs}
        orphan = sorted((n for n in disk if norm(n) not in known), key=lambda n: -disk[n])
        print(f"\nArchivos de audio en disco: {len(disk)}")
        print(f"[B] En disco pero Navidrome NO los conoce: {len(orphan)}")
        for n in orphan[:60]:
            print(f"    {n}")
        gone = sorted(v for k, v in known.items() if k and k not in disk_keys)
        print(f"[C] Navidrome los conoce pero ya no estan en disco: {len(gone)}")
        for n in gone[:30]:
            print(f"    {n}")
    else:
        print("\n(No pude listar el disco por SSH; omito B y C.)")

    # --- the only write: top up the playlist -----------------------------------------
    if args.apply:
        # Re-read the playlist right now, so a second run (or a worker that added
        # something in the meantime) can never produce a duplicate.
        fresh = nd.call("getPlaylist", id=target["id"])["playlist"].get("entry", [])
        present = {e["id"] for e in fresh}
        to_add, skipped = [], []
        for s in missing:
            if s["id"] in present:
                continue
            # Never file a song whose file is gone: it would show up and not play.
            if disk and norm(base_name(s.get("path", ""))) not in {norm(n) for n in disk}:
                skipped.append(s)
                continue
            to_add.append(s)

        print(f"\n[APPLY] a agregar: {len(to_add)}   omitidas (archivo no esta en disco): {len(skipped)}")
        for s in skipped:
            print(f"    omitida: {s.get('artist', '?')} - {s.get('title', '?')}")
        if to_add:
            nd.call("updatePlaylist", playlistId=target["id"], songIdToAdd=[s["id"] for s in to_add])
            after = nd.call("getPlaylist", id=target["id"])["playlist"].get("entry", [])
            ids = [e["id"] for e in after]
            dupes = len(ids) - len(set(ids))
            print(f"[APPLY] {args.playlist}: {len(fresh)} -> {len(after)} canciones; "
                  f"agregadas realmente: {len(set(ids) - present)}; duplicados en la playlist: {dupes}")
    else:
        print("\n(Solo lectura. Para agregar las faltantes a la playlist: repetir con --apply)")

    # --- songkey's own memory ------------------------------------------------------
    raw = ssh(args, f'f=$(find "{args.appdata_dir}" -maxdepth 4 -name catalog.json 2>/dev/null | head -1); [ -n "$f" ] && cat "$f"')
    if raw.strip():
        cat = json.loads(raw)
        pend = cat.get("pending") or {}
        hist = cat.get("history") or []
        print(f"\n[D] songkey: {len(cat.get('tracks', []))} tracks indexados, "
              f"{len(pend)} en curso, {len(hist)} en historial")
        for p in pend.values():
            print(f"    en curso: {p.get('artist', '')} - {p.get('title', '')} "
                  f"(estado={p.get('state', 'antiguo')}, intentos={p.get('attempts')})")
        for h in hist[-15:]:
            print(f"    historial: [{h.get('state')}] {h.get('title', '')} :: {h.get('reason', '')}")
        tracked = {t.get("songId") for t in cat.get("tracks", []) if t.get("songId")}
        stale = [i for i in tracked if i not in by_id]
        print(f"    songIds del catalogo que Navidrome ya no tiene: {len(stale)}")
    else:
        print("\n[D] No pude leer el catalog.json de songkey por SSH.")


if __name__ == "__main__":
    main()
