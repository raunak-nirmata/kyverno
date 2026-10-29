#!/usr/bin/env python3
"""TCP proxy in front of the benchmark registry: injects latency, counts requests.

Three jobs, all load-bearing for the ivpol measurement:

1. **Latency injection.** The serial-vs-parallel image prefetch difference
   (#17313) is invisible against a local registry, because a round trip that
   costs microseconds costs the same whether you make them one at a time or
   twenty at once. DELAY_MS makes each registry request cost something, which is
   what a real remote registry does.

2. **Request counting.** Every cell asserts how many registry requests actually
   happened. Without that, a cell where verification silently short-circuited -
   a match failure, an unexpected cache hit - looks exactly like a fast one.

3. **TLS termination.** The proxy terminates TLS from the client (Kyverno) using
   asyncio's `StreamWriter.start_tls()`, and keeps speaking plain HTTP to the
   registry backend. asyncio does TLS below the stream API, so the decrypted
   bytes still flow through the same `pump()`, the same request-line regex, and
   the same per-request delay. A failed handshake counts as
   `tls_handshake_failed` rather than vanishing silently, which is why
   `start_tls` is used instead of `start_server(ssl=...)` (see `handle()`).

The proxy is in path for *every* cell, including the 0 ms baseline, so the
network topology is identical across lanes and the counter is always available.
A cell that bypassed it would be measuring a different system.

Delay is applied once per HTTP request, on the request path only, which models
one round trip per request. Responses are not delayed: doing both would model
two round trips and double-count the thing being measured.
"""

from __future__ import annotations

import asyncio
import json
import os
import re
import ssl
import sys
from collections import Counter

if sys.version_info < (3, 11):
    sys.exit(f"delay-proxy requires Python 3.11+ for start_tls(); running {sys.version}")

UPSTREAM_HOST = os.environ.get("UPSTREAM_HOST", "127.0.0.1")
UPSTREAM_PORT = int(os.environ.get("UPSTREAM_PORT", "5000"))
LISTEN_PORT = int(os.environ.get("LISTEN_PORT", "5000"))
STATS_PORT = int(os.environ.get("STATS_PORT", "5001"))
DELAY_MS = int(os.environ.get("DELAY_MS", "0"))
TLS_CERT = os.environ.get("TLS_CERT")
TLS_KEY = os.environ.get("TLS_KEY")
if not TLS_CERT or not TLS_KEY:
    sys.exit("TLS_CERT and TLS_KEY must both be set; a proxy that falls back to plaintext hides the failure this project keeps hitting")

# No ALPN: Go would otherwise negotiate h2, and the request-line counter only
# understands HTTP/1.1 text, so it would go blind. Never call set_alpn_protocols.
_tls_ctx = ssl.create_default_context(ssl.Purpose.CLIENT_AUTH)
_tls_ctx.load_cert_chain(TLS_CERT, TLS_KEY)

# Request lines, so a chunk carrying a blob body is not miscounted as a request.
REQUEST_LINE = re.compile(rb"(?:^|\r\n)(GET|HEAD|POST|PUT|PATCH|DELETE) ([^ ]+) HTTP/")
# The OCI dist-spec 1.1 referrers tag schema: /manifests/sha256-<64 hex>.
# Distinct from an ordinary manifest fetch by tag or by sha256:<hex> digest.
FALLBACK_TAG = re.compile(r"/manifests/sha256-[0-9a-f]{64}$")
# cosign's legacy signature tag: /manifests/sha256-<64 hex>.sig. This is the
# one bucket that reports an *outcome* rather than an attempt, which is why the
# bundle gate keys on it - see classify().
SIG_TAG = re.compile(r"/manifests/sha256-[0-9a-f]{64}\.sig$")

counts: Counter = Counter()
started = asyncio.get_event_loop_policy().new_event_loop().time()


def classify(path: bytes) -> str:
    """Bucket a registry path so a cell can assert the shape of its traffic.

    Manifest fetches are what the per-image prefetch loop does; blob fetches are
    signature and attestation payloads. Counting them separately tells a
    cache-hit cell from a cache-miss one without reading Kyverno's logs.

    `referrers` and `fallback_tag` count the bundle *probe*, which Kyverno
    issues on every uncached verification regardless of what the image actually
    carries: buildCheckOptsWithBundleDetection calls GetBundles unconditionally
    (verifier.go:39) and only then decides the format. So neither bucket
    distinguishes "this image has a bundle" from "we looked and found none" -
    a legacy .sig cell probes too. The gap between them is still useful: it is
    exactly the extra round trip a registry without the Referrers API costs.

    `sig_tag` is the bucket that reports an outcome. Only the legacy path
    fetches sha256-<hex>.sig, so its presence proves which format was actually
    verified, and its absence under a bundle label proves the bundle was found.
    """
    text = path.decode("latin-1", "replace")
    if "/referrers/" in text:
        return "referrers"
    # Before FALLBACK_TAG: both match /manifests/sha256-<hex>, and only the
    # anchored .sig suffix separates them.
    if SIG_TAG.search(text):
        return "sig_tag"
    if FALLBACK_TAG.search(text):
        return "fallback_tag"
    if "/manifests/" in text:
        return "manifest"
    if "/blobs/" in text:
        return "blob"
    if text.rstrip("/").endswith("/v2"):
        return "ping"
    if "/tags/" in text:
        return "tags"
    return "other"


async def pump(reader, writer, *, delay: bool) -> None:
    try:
        while True:
            chunk = await reader.read(65536)
            if not chunk:
                break
            if delay:
                seen = REQUEST_LINE.findall(chunk)
                for method, path in seen:
                    counts["total"] += 1
                    counts[classify(path)] += 1
                # One sleep per chunk rather than per request: pipelined requests
                # share a round trip on the wire, so charging each one separately
                # would overstate the latency a real registry imposes.
                if seen and DELAY_MS:
                    await asyncio.sleep(DELAY_MS / 1000.0)
            writer.write(chunk)
            await writer.drain()
    except (ConnectionResetError, BrokenPipeError, asyncio.IncompleteReadError):
        pass
    finally:
        try:
            writer.close()
        except Exception:
            pass


async def handle(client_reader, client_writer) -> None:
    counts["connections"] += 1
    # start_tls, not start_server(ssl=...): a failed handshake under start_server
    # is an OSError that asyncio drops before it reaches the handler (sslproto
    # only logs it in debug mode), so a plaintext probe would be invisible. Here
    # it is an exception in our own coroutine, so it becomes a counter.
    try:
        await client_writer.start_tls(_tls_ctx, ssl_handshake_timeout=10)
    except (ssl.SSLError, OSError, asyncio.TimeoutError):
        counts["tls_handshake_failed"] += 1
        try:
            client_writer.close()
        except Exception:
            pass
        return
    try:
        up_reader, up_writer = await asyncio.open_connection(UPSTREAM_HOST, UPSTREAM_PORT)
    except OSError:
        counts["upstream_errors"] += 1
        client_writer.close()
        return
    await asyncio.gather(
        pump(client_reader, up_writer, delay=True),
        pump(up_reader, client_writer, delay=False),
    )


async def stats(reader, writer) -> None:
    """Minimal HTTP endpoint. GET /stats reads, POST /reset zeroes the counters.

    Reset exists so a cell can start from a known zero and assert an exact
    count, rather than diffing against a number another cell left behind.
    """
    try:
        request = await asyncio.wait_for(reader.read(8192), timeout=5)
    except (asyncio.TimeoutError, ConnectionResetError):
        writer.close()
        return
    if request.startswith(b"POST /reset"):
        counts.clear()
        body = json.dumps({"reset": True}).encode()
    else:
        body = json.dumps({"delay_ms": DELAY_MS, **dict(counts)}).encode()
    writer.write(
        b"HTTP/1.1 200 OK\r\nContent-Type: application/json\r\n"
        b"Content-Length: " + str(len(body)).encode() + b"\r\n"
        b"Connection: close\r\n\r\n" + body
    )
    try:
        await writer.drain()
    except ConnectionResetError:
        pass
    writer.close()


async def main() -> None:
    proxy = await asyncio.start_server(handle, "0.0.0.0", LISTEN_PORT)
    stats_server = await asyncio.start_server(stats, "0.0.0.0", STATS_PORT)
    print(
        f"delay-proxy: :{LISTEN_PORT} -> {UPSTREAM_HOST}:{UPSTREAM_PORT} "
        f"delay={DELAY_MS}ms stats=:{STATS_PORT}",
        flush=True,
    )
    async with proxy, stats_server:
        await asyncio.gather(proxy.serve_forever(), stats_server.serve_forever())


if __name__ == "__main__":
    asyncio.run(main())
