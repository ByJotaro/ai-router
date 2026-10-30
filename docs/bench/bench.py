"""1000 rpm bench against the ai-router iso stand.

Pacing is open-loop (1000 submissions/min regardless of completions), so the
offered rate is real and the stand's own request_log is the source of truth for
what the pool actually served. Completions are tracked separately from
submissions — the earlier stand floods conflated the two.
"""
import argparse, asyncio, csv, json, random, string, time

import httpx

BODY_TOKENS = 200_000
PROMPT = "x" * 4000  # ~1k tokens of prompt per request, keeps bodies distinct


def stamp():
    return "".join(random.choices(string.ascii_lowercase + string.digits, k=8))


async def one(client, url, token, model, idx, results, lock):
    body = {
        "model": model,
        "stream": True,
        "max_tokens": BODY_TOKENS,
        "messages": [{"role": "user", "content": f"req {idx} {stamp()} {PROMPT}"}],
    }
    t0 = time.perf_counter()
    status, err, chunks = 0, "", 0
    try:
        async with client.stream("POST", url, json=body,
                                 headers={"Authorization": f"Bearer {token}"}) as r:
            status = r.status_code
            if status == 200:
                async for line in r.aiter_lines():
                    if line.startswith("data:"):
                        chunks += 1
            else:
                err = (await r.aread()).decode("utf-8", "replace")[:160]
    except Exception as e:  # timeout, reset, etc.
        err = f"{type(e).__name__}: {str(e)[:120]}"
    dt = time.perf_counter() - t0
    async with lock:
        results.append((idx, status, round(dt, 3), chunks, err))


async def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--url", default="http://127.0.0.1:9013/v1/chat/completions")
    ap.add_argument("--token", required=True)
    ap.add_argument("--model", default="opencode/deepseek-v4.1-flash-free")
    ap.add_argument("--rpm", type=int, default=1000)
    ap.add_argument("--seconds", type=int, default=120)
    ap.add_argument("--concurrency", type=int, default=400)
    ap.add_argument("--timeout", type=float, default=300.0)
    ap.add_argument("--out", default="bench.csv")
    a = ap.parse_args()

    results, lock = [], asyncio.Lock()
    sem = asyncio.Semaphore(a.concurrency)
    interval = 60.0 / a.rpm
    limits = httpx.Limits(max_connections=a.concurrency + 50,
                          max_keepalive_connections=a.concurrency + 50)
    async with httpx.AsyncClient(timeout=a.timeout, limits=limits) as client:
        tasks = []
        start = time.perf_counter()
        n = 0
        while time.perf_counter() - start < a.seconds:
            n += 1
            await sem.acquire()

            async def run(i=n):
                try:
                    await one(client, a.url, a.token, a.model, i, results, lock)
                finally:
                    sem.release()

            tasks.append(asyncio.create_task(run()))
            drift = start + n * interval - time.perf_counter()
            if drift > 0:
                await asyncio.sleep(drift)
        await asyncio.gather(*tasks)

    with open(a.out, "w", newline="", encoding="utf-8") as f:
        w = csv.writer(f)
        w.writerow(["idx", "status", "seconds", "chunks", "error"])
        w.writerows(results)

    ok = [r for r in results if r[1] == 200]
    print(json.dumps({
        "submitted": len(results),
        "ok": len(ok),
        "failed": len(results) - len(ok),
        "rpm_offered": round(len(results) / a.seconds * 60, 1),
        "seconds": a.seconds,
        "latency_p50": round(sorted(r[2] for r in results)[len(results) // 2], 2) if results else None,
    }, ensure_ascii=False))


if __name__ == "__main__":
    asyncio.run(main())
