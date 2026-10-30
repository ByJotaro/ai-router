#!/usr/bin/env python3
"""Dump OpenCode CLI session ids (both storage tables) and optionally
validate them against the zen free-tier gate THROUGH a running ai-router.

Why through the router: the gate rejects non-OpenCode TLS fingerprints
(curl/python probes 403 even for genuine, app-primed sessions — verified
2026-10-13), while the router's Go transport passes for any real session.
The router forwards an inbound x-opencode-session header upstream, so a
probe = one tiny chat request with the header pinned.

Usage:
    python3 opencode-sessions.py                     # dump only
    python3 opencode-sessions.py --probe             # dump + validate
    python3 opencode-sessions.py --probe --url http://127.0.0.1:9001 \
        --token <admin_token> --model opencode/muse-spark-1.3-contributor-free
"""
import argparse
import json
import os
import sqlite3
import sys
import time
import urllib.request

DEFAULT_DB = os.path.expanduser("~/.local/share/opencode/opencode.db")


def dump(db_path):
    con = sqlite3.connect(f"file:{db_path}?mode=ro", uri=True)
    cur = con.cursor()
    rows = {}
    for table in ("session", "session_v2"):
        try:
            have = cur.execute(
                "select count(*) from sqlite_master where type='table' and name=?", (table,)
            ).fetchone()[0]
            if not have:
                continue
            for sid, updated in cur.execute(f"select id, time_updated from {table}"):
                if sid and sid.startswith("ses_"):
                    rows[sid] = max(rows.get(sid, 0), updated or 0)
        except sqlite3.Error as e:
            print(f"warn: {table}: {e}", file=sys.stderr)
    con.close()
    return sorted(rows.items(), key=lambda kv: kv[1], reverse=True)


def probe(sid, url, token, model):
    body = json.dumps({
        "model": model,
        "messages": [{"role": "user", "content": "ping"}],
        "max_tokens": 4,
        "stream": False,
    }).encode()
    req = urllib.request.Request(url.rstrip("/") + "/v1/chat/completions", data=body, method="POST")
    req.add_header("Content-Type", "application/json")
    req.add_header("Authorization", f"Bearer {token}")
    req.add_header("x-opencode-session", sid)
    req.add_header("x-opencode-session-id", sid)
    try:
        with urllib.request.urlopen(req, timeout=90) as r:
            return r.status == 200
    except urllib.error.HTTPError:
        return False
    except Exception as e:
        print(f"warn: probe {sid}: {e}", file=sys.stderr)
        return None


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--db", default=DEFAULT_DB)
    ap.add_argument("--probe", action="store_true")
    ap.add_argument("--url", default="http://127.0.0.1:9001")
    ap.add_argument("--token", default="")
    ap.add_argument("--model", default="opencode/muse-spark-1.3-contributor-free")
    ap.add_argument("--limit", type=int, default=40, help="probe only the N newest ids")
    args = ap.parse_args()

    now_ms = time.time() * 1000
    rows = dump(args.db)
    print(f"{len(rows)} session ids in {args.db}")
    valid = 0
    for i, (sid, updated) in enumerate(rows):
        age_h = (now_ms - updated) / 3.6e6 if updated else -1
        line = f"{i + 1:4d}. {sid}  age={age_h:7.1f}h"
        if args.probe:
            if i >= args.limit:
                print(line)
                continue
            ok = probe(sid, args.url, args.token, args.model)
            if ok is None:
                line += "  probe=ERR"
            else:
                valid += 1 if ok else 0
                line += "  probe=200" if ok else "  probe=403"
        print(line, flush=True)
    if args.probe:
        print(f"valid (200): {valid} of {min(args.limit, len(rows))} probed")


if __name__ == "__main__":
    main()
