# /// script
# dependencies = ["httpx>=0.27"]
# ///
#
# hello-python — showcases the async Python SDK globals added in PR #52:
#   - async def main() entry point (auto-detected by the runtime)
#   - kv.*_async, params.*_async, log.*_async, output.*_async
#   - httpx async HTTP call as a realistic async dependency example

import httpx


async def main():
    # params.get_async uses the shared cache — no extra IPC round-trip.
    name = await params.get_async("name", "World")
    count = int(await params.get_async("count", "1"))

    await log.info_async(f"Hello, {name}! (count={count})")

    # kv — async read/write.
    prev = await kv.get_async("previous_name")
    if prev:
        await log.info_async(f"Previous run greeted: {prev}")
    await kv.set_async("previous_name", name)

    # Async HTTP call with httpx (PEP 723 inline dep above).
    async with httpx.AsyncClient(timeout=5) as client:
        resp = await client.get("https://httpbin.org/get", params={"name": name})
        resp.raise_for_status()
        origin = resp.json().get("origin", "unknown")
    await log.info_async(f"Request origin: {origin}")

    greeting = f"Hello, {name}! (run #{count})"

    # output.html_async — new in PR #52.
    await output.html_async(
        f"<h2>{greeting}</h2><p>Origin: {origin}</p>",
        data={"name": name, "count": count},
    )

    return {"greeting": greeting, "origin": origin}
