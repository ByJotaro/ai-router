"""Minimal warp-router pool API stand-in for ai-router bench runs.

Emulates exactly the endpoints ai-router calls:
  POST /api/v2/pool/acquire   -> {"epoch":N,"leases":[{lease_id,proxy_url,public_ip,country}]}
  POST /api/v2/pool/release   -> 200
  POST /api/v1/pool/rotate    -> 200 after `rotate_s` seconds; bumps `epoch`
  GET  /api/v1/pool/stats     -> debug view
  GET  /ctl?...               -> bench driver knobs

Knobs (files in this directory, so the driver only needs curl/write):
  burn_all.txt           "1" -> acquire returns an empty batch (all egresses busy/parked)
  rotate_new_prefix.txt  "1" -> a rotate mints NEW /48 prefixes (proves renewal)
                         "0" -> a rotate respawns onto the SAME /48 (the live
                                2026-10-22 case: fresh accounts in a burned
                                prefix — renewal must NOT be claimed)
Proxy for every lease is the stub upstream itself (it speaks absolute-URI HTTP),
so no real egress is used and no provider quota is consumed.
"""
import json, time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

HERE = Path(__file__).parent
STUB = "http://127.0.0.1:9099"
COUNT = 6
STATE = {"epoch": 1, "rotates": 0, "generation": 1, "acquires": 0, "empty": 0}


def knob(name, default="0"):
    try:
        return (HERE / f"{name}.txt").read_text(encoding="utf-8").strip() or default
    except OSError:
        return default


def leases():
    gen = STATE["generation"]
    out = []
    for i in range(COUNT):
        out.append({
            "lease_id": f"lease-{gen}-{i}",
            "proxy_url": STUB,
            "public_ip": f"2a09:bac5:48f{gen + i % 2}:682::a6:{i + 1:x}",
            "country": "MD",
            "epoch": STATE["epoch"],
        })
    return out


class H(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def _json(self, obj, code=200):
        body = json.dumps(obj).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def _drain(self):
        n = int(self.headers.get("Content-Length") or 0)
        if n:
            self.rfile.read(n)

    def do_GET(self):
        if self.path.startswith("/ctl"):
            self._json({"epoch": STATE["epoch"], "generation": STATE["generation"],
                        "rotates": STATE["rotates"], "acquires": STATE["acquires"],
                        "empty": STATE["empty"]})
            return
        if self.path.startswith("/api/v1/pool/stats"):
            self._json({"leases": leases(), "epoch": STATE["epoch"]})
            return
        self._json({"error": "not found"}, 404)

    def do_POST(self):
        self._drain()
        if "/pool/acquire" in self.path:
            STATE["acquires"] += 1
            if knob("burn_all") == "1":
                STATE["empty"] += 1
                self._json({"epoch": STATE["epoch"], "leases": []})
                return
            self._json({"epoch": STATE["epoch"], "leases": leases()})
            return
        if "/pool/release" in self.path:
            self._json({"ok": True})
            return
        if "/pool/rotate" in self.path:
            time.sleep(float(knob("rotate_s", "2")))
            STATE["rotates"] += 1
            STATE["epoch"] += 1
            if knob("rotate_new_prefix") == "1":
                STATE["generation"] += 1
            self._json({"ok": True, "epoch": STATE["epoch"]})
            return
        self._json({"error": "not found"}, 404)

    def log_message(self, *a):
        pass


if __name__ == "__main__":
    srv = ThreadingHTTPServer(("127.0.0.1", 8788), H)
    print("fake pool api on 127.0.0.1:8788", flush=True)
    srv.serve_forever()
