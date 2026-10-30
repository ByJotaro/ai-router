"""Budget-simulating upstream for the ai-router pool bench.

No real provider quota is touched and no real tokens are generated: the point
is to exercise the router's pool mechanics (acquire / wait-for-egress / heal
rotate / lockout) under a flat 1000 rpm with 200k-token requests.

mode.txt: "ok"    -> 200 SSE, streams `chunks` chunks then [DONE]
          "burn"  -> 429 with `Retry-After: <retry_after>` (upstream window gone)
Requests are answered per connection; concurrency is asyncio-native.
"""
import asyncio, json, os, time
from pathlib import Path

HERE = Path(__file__).parent
MODE = HERE / "mode.txt"
COUNTS = {"ok": 0, "burn": 0, "other": 0}
STARTED = time.time()


def setting(name, default):
    p = HERE / f"{name}.txt"
    try:
        return p.read_text(encoding="utf-8").strip()
    except OSError:
        return default


async def handle(reader, writer):
    try:
        head = await reader.readuntil(b"\r\n\r\n")
    except Exception:
        writer.close()
        return
    lines = head.split(b"\r\n")
    target = lines[0].split(b" ")[1].decode("latin1") if len(lines) > 1 else "/"
    # ai-router talks to this server as an HTTP proxy (lease.proxy_url), so the
    # request line carries the absolute URI: strip scheme+host to get the path.
    if target.startswith("http"):
        from urllib.parse import urlsplit
        u = urlsplit(target)
        path = u.path or "/"
    else:
        path = target
    clen = 0
    for ln in lines:
        if ln.lower().startswith(b"content-length:"):
            clen = int(ln.split(b":")[1])
    if clen:
        try:
            await reader.readexactly(clen)
        except Exception:
            pass
    if path.startswith("/stats"):
        body = json.dumps(dict(COUNTS, uptime=round(time.time() - STARTED, 1))).encode()
        writer.write(b"HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n" % len(body) + body)
        await writer.drain()
        writer.close()
        return

    mode = setting("mode", "ok")
    if mode == "burn":
        COUNTS["burn"] += 1
        ra = setting("retry_after", "5")
        body = b'{"error":{"message":"simulated window burned","type":"rate_limit"}}'
        writer.write(
            b"HTTP/1.1 429 Too Many Requests\r\nContent-Type: application/json\r\nRetry-After: "
            + ra.encode()
            + b"\r\nContent-Length: %d\r\n\r\n" % len(body)
            + body
        )
        await writer.drain()
        writer.close()
        return

    COUNTS["ok"] += 1
    serve_s = float(setting("serve_s", "0.4"))
    chunks = int(setting("chunks", "400"))
    writer.write(
        b"HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nCache-Control: no-cache\r\n"
        b"Transfer-Encoding: chunked\r\n\r\n"
    )
    payload = b"data: " + json.dumps(
        {"id": "stub", "object": "chat.completion.chunk",
         "choices": [{"index": 0, "delta": {"content": "x" * 64}}]}
    ).encode() + b"\n\n"
    delay = serve_s / max(chunks, 1)
    try:
        for _ in range(chunks):
            writer.write(b"%x\r\n%s\r\n" % (len(payload), payload))
            await writer.drain()
            if delay:
                await asyncio.sleep(delay)
        end = b"data: [DONE]\n\n"
        writer.write(b"%x\r\n%s\r\n" % (len(end), end))
        writer.write(b"0\r\n\r\n")
        await writer.drain()
    except Exception:
        COUNTS["other"] += 1
    finally:
        try:
            writer.close()
        except Exception:
            pass


async def main():
    srv = await asyncio.start_server(handle, "127.0.0.1", 9099, backlog=2048)
    print("stub upstream on 127.0.0.1:9099", flush=True)
    async with srv:
        await srv.serve_forever()


if __name__ == "__main__":
    asyncio.run(main())
