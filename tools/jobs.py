#!/usr/bin/env python3
"""Read-only: trace specific songkey jobs across songkey's catalog, disk and Navidrome.

Prints only results, never credentials. Changes nothing.

Usage:
    python tools/jobs.py <path-to-.env> aded2b 4f917f ...   (job id prefixes)
"""
import argparse
import json
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import audit  # noqa: E402


def main():
    sys.stdout.reconfigure(encoding="utf-8", errors="replace")
    ap = argparse.ArgumentParser()
    ap.add_argument("env")
    ap.add_argument("jobs", nargs="+")
    audit.add_server_args(ap)
    args = ap.parse_args()

    env = audit.load_env(args.env)
    audit.resolve_server_args(args, env)
    user = audit.pick(env, "NAVIDROME_USER", "ND_USER", "NAVIDROME_USERNAME")
    pw = audit.pick(env, "NAVIDROME_PASS", "NAVIDROME_PASSWORD", "ND_PASSWORD", "ND_PASS")
    url = audit.pick(env, "NAVIDROME_URL", "ND_URL", "NAVIDROME_BASE_URL")
    nd = None
    for c in [x for x in (url, f"http://{args.host}:4533") if x]:
        try:
            s = audit.Subsonic(c, user, pw)
            s.call("ping")
            nd = s
            break
        except Exception as e:  # noqa: BLE001
            print(f"Navidrome: {c} no respondio ({type(e).__name__})")
    if nd is None:
        sys.exit(1)

    songs, off = [], 0
    while True:
        b = nd.call("search3", query='""', songCount=500, songOffset=off,
                    artistCount=0, albumCount=0).get("searchResult3", {}).get("song", [])
        songs += b
        if len(b) < 500:
            break
        off += 500
    nd_keys = {audit.norm(audit.base_name(s.get("path", ""))): s for s in songs}

    pattern = " -o ".join(f'-iname "*.{e}"' for e in audit.AUDIO)
    listing = audit.ssh(args, f'find "{args.music_dir}" -type f \\( {pattern} \\) -printf "%T@|%p\\n" 2>/dev/null')
    disk = {}
    for line in listing.splitlines():
        if "|" in line:
            ts, p = line.split("|", 1)
            disk[audit.base_name(p)] = float(ts)
    disk_keys = {audit.norm(n): n for n in disk}

    raw = audit.ssh(args, f'f=$(find "{args.appdata_dir}" -maxdepth 4 -name catalog.json 2>/dev/null | head -1); [ -n "$f" ] && cat "$f"')
    if not raw.strip():
        print("No pude leer catalog.json por SSH.")
        sys.exit(1)
    cat = json.loads(raw)
    entries = list((cat.get("pending") or {}).values()) + list(cat.get("history") or [])

    print(f"Navidrome: {len(songs)} canciones | disco: {len(disk)} archivos | catalogo: {len(entries)} jobs\n")
    for prefix in args.jobs:
        hits = [e for e in entries if (e.get("id") or "").startswith(prefix)]
        if not hits:
            print(f"== {prefix}: NO esta en pending ni historial\n")
            continue
        for e in hits:
            print(f"== {e.get('id')}  [{e.get('state')}]  {e.get('artist', '')} - {e.get('title', '')}")
            print(f"   razon: {e.get('reason', '')}")
            print(f"   videoId={e.get('videoId')} intentos={e.get('attempts')} "
                  f"creado={e.get('created')} actualizado={e.get('updated')}")
            fn = e.get("filename") or ""
            print(f"   filename: {fn or '(vacio)'}")
            if fn:
                k = audit.norm(fn.rsplit(".", 1)[0])
                d = next((n for kk, n in disk_keys.items() if kk.startswith(k)), None)
                n = next((s for kk, s in nd_keys.items() if kk.startswith(k)), None)
                print(f"   en disco: {d or 'NO'} | en Navidrome: {'SI' if n else 'NO'}")
            # fallback: title match in Navidrome / disk
            t = audit.norm(e.get("title", ""))
            if t:
                nm = [s for s in songs if t in audit.norm(s.get("title", ""))][:3]
                dm = [n for kk, n in disk_keys.items() if t in kk][:3]
                print(f"   por titulo -> Navidrome: {[s.get('artist','')+' - '+s.get('title','') for s in nm] or 'NO'}")
                print(f"   por titulo -> disco: {dm or 'NO'}")
            print()


if __name__ == "__main__":
    main()
