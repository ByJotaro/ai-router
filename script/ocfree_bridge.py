#!/usr/bin/env python3
"""ocfree-bridge: OpenAI-compatible endpoint over the real OpenCode CLI.

The zen free-tier gate (2026-10-13) only passes the compiled OpenCode client
for every free model except the muse-*-contributor family (those pass with a
pooled session through ai-router's Go transport). curl/python/bun/Go replays
all 403. So the remaining existing free models are served by running the real
CLI: `opencode run -m opencode/<model> <prompt>`.

POST /v1/chat/completions  {"model": "<model>", "messages": [...], "max_tokens": N}
POST /v1/models            -> list of servable models
GET  /healthz              -> {"ok": true}

Requests are serialized per model (the CLI is one interactive process per
call and shares one opencode.db). Long prompts are passed via a temp file to
avoid argv length limits. Model names accept an optional "opencode/" prefix
and map onto the CLI's -m opencode/<model>.
"""
import json
import os
import subprocess
import sys
import tempfile
import threading
import time
import uuid
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

PORT = int(os.environ.get("OCFREE_PORT", "9009"))
CMD_TIMEOUT = int(os.environ.get("OCFREE_TIMEOUT", "240"))
OPENCODE = os.environ.get("OPENCODE_BIN", "opencode")
MODELS = [m.strip() for m in os.environ.get(
    "OCFREE_MODELS",
    "cat-jam-free,jev-1.13-free,ling-3.1-flash-free,longcat-2.5-preview-free,"
    "mimo-v2.6-flash-free,space-bunny-free,nemotron-3.5-lightning-free,"
    "step-5-preview-free",
).split(",") if m.strip()]

LOCKS = {m: threading.Lock() for m in MODELS}
LOCK_GUARD = threading.Lock()


def lock_for(model):
    with LOCK_GUARD:
        if model not in LOCKS:
            LOCKS[model] = threading.Lock()
        return LOCKS[model]


def last_user_text(messages):
    for m in reversed(messages or []):
        if m.get("role") == "user":
            c = m.get("content")
            if isinstance(c, str):
                return c
            if isinstance(c, list):
                return "".join(p.get("text", "") for p in c if isinstance(p, dict))
    return "ping"


def run_cli(model, prompt):
    """One opencode CLI call; returns (ok, text). The prompt goes straight to
    argv (the CLI's [message..] arg); only very long prompts use a temp file
    to stay under the Windows 32k argv limit."""
    fd, path = tempfile.mkstemp(prefix="ocfree_", suffix=".txt")
    os.close(fd)
    try:
        if len(prompt) > 20000:
            with open(path, "w", encoding="utf-8") as f:
                f.write(prompt)
            msg = f"Read {path} and answer it."
        else:
            msg = prompt
        cmd = [OPENCODE, "run", "-m", f"opencode/{model}", msg]
        t0 = time.time()
        proc = subprocess.run(cmd, capture_output=True, text=True, timeout=CMD_TIMEOUT,
                              encoding="utf-8", errors="replace")
        out = (proc.stdout or "").strip()
        if proc.returncode != 0 and not out:
            return False, (proc.stderr or "cli failed").strip()[:400]
        return True, out
    except subprocess.TimeoutExpired:
        return False, f"cli timeout after {CMD_TIMEOUT}s"
    except Exception as e:
        return False, f"{type(e).__name__}: {e}"
    finally:
        try:
            os.unlink(path)
        except OSError:
            pass


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, fmt, *args):
        sys.stderr.write("[ocfree] %s %s\n" % (self.address_string(), fmt % args))

    def _send(self, code, obj):
        body = json.dumps(obj).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        if self.path.rstrip("/") == "/healthz":
            return self._send(200, {"ok": True, "models": MODELS})
        if self.path.rstrip("/") == "/v1/models":
            return self._send(200, {"object": "list", "data": [
                {"id": m, "object": "model", "owned_by": "opencode-cli"} for m in MODELS]})
        return self._send(404, {"error": "not found"})

    def do_POST(self):
        n = int(self.headers.get("Content-Length") or 0)
        try:
            req = json.loads(self.rfile.read(n) or b"{}")
        except Exception:
            return self._send(400, {"error": "bad json"})
        path = self.path.rstrip("/")
        if path not in ("/v1/chat/completions", "/chat/completions"):
            return self._send(404, {"error": "not found"})
        raw = (req.get("model") or "").strip()
        model = raw.split("/")[-1] if "/" in raw else raw
        if model not in MODELS:
            return self._send(404, {"error": f"model {model!r} not servable; have {MODELS}"})
        prompt = last_user_text(req.get("messages"))
        with lock_for(model):
            ok, text = run_cli(model, prompt)
        if not ok:
            return self._send(502, {"error": {"message": text, "type": "bridge_error"}})
        return self._send(200, {
            "id": "chatcmpl-" + uuid.uuid4().hex[:12],
            "object": "chat.completion",
            "created": int(time.time()),
            "model": model,
            "choices": [{"index": 0, "finish_reason": "stop",
                         "message": {"role": "assistant", "content": text}}],
        })


if __name__ == "__main__":
    srv = ThreadingHTTPServer(("127.0.0.1", PORT), Handler)
    sys.stderr.write(f"[ocfree] listening on http://127.0.0.1:{PORT} models={len(MODELS)}\n")
    srv.serve_forever()
