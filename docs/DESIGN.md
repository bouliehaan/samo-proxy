# samo-proxy — design notes

Why the edge box exists and what the proxy does on it. For installing and
running it, see the [README](../README.md); for moving the tunnel, see
[DEPLOY.md](DEPLOY.md).

## Why

The samo box runs a ProtonVPN kill-switch — `ufw default deny outgoing`, VPN as
the default route. That is correct for the box and should stay. But it also
means `cloudflared`, running there as a systemd service, reaches Cloudflare's
edge *through* ProtonVPN: a tunnel inside a tunnel, inheriting the VPN's
congestion, its MTU, and its throughput ceiling. That is the speed problem.

Moving `cloudflared` to a second box whose default route is the plain WAN
removes it entirely. And once a process is sitting on that boundary, it is the
right place to do everything else a self-hosted audio service needs when its
bytes cross the internet rather than a LAN.

```
     phone / laptop / browser
              │  HTTPS — Cloudflare's own certificate, terminated at the edge
       Cloudflare edge
              │  the tunnel: mutually authenticated with the tunnel credentials
  ┌───────────┴──────────────┐
  │  CLOUD SERVER — no VPN   │   cloudflared + samo-proxy
  │  192.168.1.12            │   gzip · artwork sizing · FLAC→Opus · cache
  └──┬────────────────────┬──┘
     │ compose bridge     │ LAN, TLS verified via originServerName
     │ (never a wire)     │
  samo-proxy:6767         │
     │                    │
     │ LAN, plain HTTP    │
  ┌──┴────────────────────┴──┐
  │  SAMO BOX — ProtonVPN    │   samo-server :6969   ← samo.example.com
  │  192.168.1.10            │   nginx :443 → :8096  ← tv.example.com
  └──────────────────────────┘       (Jellyfin, unchanged)
```

The constrained link is the home **upload**, between the edge box and
Cloudflare. Everything samo-proxy does is aimed at putting fewer bytes on that
link. The LAN hop below it stays untouched and bit-perfect, so a client on the
LAN talking to samo-server directly is completely unaffected.

## What it does

**Moves the tunnel off the VPN.** The reason the repo exists. `cloudflared` runs
in this stack, on the edge box.

**Compresses JSON.** samo-server has no compression middleware at all — its
whole chain is `WithSecurityHeaders(WithCORS(server))` — so every API response
leaves the house raw. Cloudflare *does* compress, but at its edge, which is on
the far side of the uplink. Compressing here is the only place it saves
anything, and JSON gzips five to ten times over. The Android client's first
catalog sync is the case that matters: a real library measured 78 MB of detail
rows across 2,241 items.

**Sizes artwork.** samo-server has had a thumbnail ladder all along
(`internal/images/thumbnail.go`: 64/128/256/384/512/768/1024 at JPEG q85) and
the desktop client uses it, piping a rendered slot width through
`withSamoImageWidth` on every request. The Android client never does —
`getSamoMetadataImageUrl` builds `/media/images/{id}/image` bare — so every grid
tile pulls the full embedded cover, routinely 3000×3000. samo-proxy injects a
default `width=` on artwork requests that lack one, which fixes it for every
client at once. A request that already carries a width is left alone.

**Re-encodes lossless audio.** `ServeMediaFileAt` is documented as "streams
original on-disk bytes without transcoding", and the Subsonic adapter's
`handleStream` delegates to the same path, ignoring `maxBitRate` entirely. A
24/96 FLAC is roughly 4.5 Mbps sustained — most of a home uplink on its own.
samo-proxy transcodes lossless sources to Opus and caches the result. Lossy
sources pass through untouched by default: re-encoding a 192k MP3 is an audible
loss for a much smaller saving.

**Gets the forwarding headers right.** See [Trust](#trust) — this is the
part that is easy to get subtly wrong and quietly weakens samo-server's
brute-force protection.

### What it deliberately does not touch

- **Live radio and channel streams** (`/radio/…`, `/internet-radio/…`,
  `/channels/…`, `/api/v1/channels/…/stream`) pass straight through. They are
  endless; a disk cache behind one would fill until it burst.
- **The SSE dashboard channel** (`/api/v1/events`) is never compressed and never
  buffered. samo-server already sends a 25 s heartbeat to survive Cloudflare's
  100 s idle reap; holding it back here would defeat that.
- **Any route it does not recognise** is forwarded unchanged. The route table in
  `internal/classify` is an allow-list of exact shapes, so a new samo-server
  route costs a missed optimisation, never a broken response.

## Letting the samo box out, narrowly

Everything above is inbound. This part is the other direction, and it is off
unless you deliberately turn it on.

The samo box's kill-switch sends every outbound packet through ProtonVPN, which
is correct and should stay. But a commercial VPN exit address is a datacenter
address, and some CDNs refuse those outright. Deezer's artist artwork is the
case that forced this: from the samo box, `api.deezer.com` returns byte-
identical payloads to any other client, while **every** request to
`cdn-images.dzcdn.net` comes back `403`. So the artist photo backfill finds a
picture for an artist and then cannot download a single one of them. Measured
on the box, 20 consecutive requests, 20 refusals; from this box, 5 of 5 fine.

samo-proxy already sits on a machine whose default route is the plain WAN, so it
is the natural place to put a door. `internal/egress` is that door, and it is
deliberately a small one:

- **Only allow-listed hosts.** The list is closed, not a default-open policy
  with exceptions. It ships containing Deezer's image CDN and nothing else —
  notably *not* `api.deezer.com`, which works fine over the VPN and stays there,
  which is what keeps the search terms (artist names out of your library) on the
  VPN where they belong.
- **Only CONNECT, only ports 80 and 443.** Not a general TCP relay. Because it
  is CONNECT rather than a fetching proxy, samo-proxy never sees the bytes —
  samo-server's TLS runs end to end to the CDN.
- **Only with the token.** The listener has to be reachable from the LAN for
  samo-server to use it, so an unauthenticated one would be an open proxy on the
  one route in the house that is not behind the VPN. Starting without a token is
  a startup error, not a warning.
- **Never into the LAN.** The allow-list is a list of *names*, and names are
  resolved by DNS this process does not control. The dialer therefore refuses
  any destination that resolves to a loopback, private, link-local or CGNAT
  address — checked after resolution, which is what closes the rebinding window.
  immich and nextcloud are on this box; this is what keeps the egress port from
  reaching them.

### The tradeoff, stated rather than buried

**Traffic through this door does not go through the VPN.** An artwork request
carries the image id of an artist in your library, so an observer on the WAN
side learns which artists are being looked up, and when. That is strictly more
than they learned before, when the answer was nothing at all.

It is a much smaller leak than moving the library's whole metadata surface off
the VPN, and it is bounded by a list you control. But it is not zero, which is
why it defaults to off and why the list ships as short as it can be. If that
trade is not one you want, leave `SAMOPROXY_EGRESS_TOKEN` unset and the port is
never opened.

### Turning it on

```bash
# On the edge box, in the samo-proxy .env:
SAMOPROXY_EGRESS_TOKEN=$(openssl rand -base64 32)
SAMOPROXY_EGRESS_PORT=0.0.0.0:6768        # reachable from the LAN
```

Then point samo-server at it — see `SAMO_EGRESS_PROXY_URL` in that repo. It
routes only the listed hosts through the proxy and leaves every other request,
including the Deezer lookup, on the VPN exactly as before.

**samo-server treats this proxy as preferred, never required.** If it is not
running, not reachable, or refuses the token, samo-server logs the reason once
and falls back to its ordinary route, skipping the proxy for two minutes so a
long backfill does not pay a dial timeout per artist. That is the right default
in both directions: the fallback path *is* the VPN, so degrading to it is more
private rather than less, and the worst case is artwork failing the way it did
before this existed. So you can stop this stack, or never deploy it at all,
without breaking samo-server.

Verify from the samo box itself:

```bash
curl -x "http://samo:$TOKEN@192.168.1.12:6768" -o /dev/null -w '%{http_code}\n' \
  https://cdn-images.dzcdn.net/images/artist/8d13c0527064ba50cf0d0873f4f574dc/1000x1000-000000-80-0-0.jpg
```

`200` through the proxy where the same URL direct gives `403` is the whole
feature. A host that is not on the list gives `403` from samo-proxy, and a wrong
token gives `407`; both are logged with the reason.

## Trust

The security-critical setting is `SAMOPROXY_TRUST_FORWARDED_FROM`.

samo-server's login limiter (`internal/api/login_limiter.go`) reads
`CF-Connecting-IP` first and `X-Forwarded-For` second, and keys its
brute-force lockout on the result. Cloudflare's edge overwrites
`CF-Connecting-IP` on every request, so the value `cloudflared` hands us is
trustworthy — **but only because it came from cloudflared**. Anything else that
can reach samo-proxy's port gets to pick its own rate-limit bucket, and an
attacker with an unlimited supply of buckets has defeated the per-IP lockout
entirely. (The per-username limit still holds; it was always the real backstop.)

So samo-proxy believes those headers only from configured sources, and
otherwise replaces them with the address it actually saw. Either way samo-server
receives exactly one authoritative value, **set** rather than appended.

Two consequences worth stating plainly:

- The compose file publishes **no ports**. cloudflared reaches samo-proxy over
  the compose network. Publishing it to the LAN would put a spoofable header
  within reach of anything on the network.
- The trusted CIDR is the pinned compose subnet (`172.23.0.0/16`), which is why
  that subnet is pinned rather than left to Docker's pool. A drifted subnet
  would silently stop trusting cloudflared — which surfaces as every client
  sharing one rate-limit bucket, not as an error.

## TLS, and why this does not need a certificate

Worth stating plainly, because "put a proxy in front of it" usually means
wrangling certificates and it does not here.

**The certificate a browser sees is Cloudflare's, terminated at their edge.** It
is publicly trusted, auto-renewed, and covers the hostname. Nothing in this repo
touches it, and nothing here can cause a certificate warning. That was already
true before samo-proxy existed.

Behind the edge there are two more hops, and only the second crosses a network:

- **Cloudflare edge → cloudflared.** The tunnel itself, mutually authenticated
  with the tunnel credentials. Not a plaintext internet hop, and not something
  this repo configures.
- **cloudflared → samo-proxy.** Container-to-container on one Docker bridge
  network, on one host. This never reaches the LAN, let alone a wire. Plain HTTP
  is correct here — there is no network for anyone to be on — and it is the
  documented Cloudflare Tunnel pattern.
- **cloudflared → nginx, for `tv.`** This one does cross the LAN, so it gets
  real TLS: `originServerName: samo.example.com` in
  `cloudflared/config.yml`, verified against the public Let's Encrypt chain.

That last point is an **improvement on what was there before**. The old config
used `noTLSVerify: true`, which is encryption without authentication — it
accepts any certificate, so anything on the LAN could have sat in the middle of
it. Connecting to a bare IP would fail hostname verification, which is why
`originServerName` is needed rather than simply dropping the flag: it tells
cloudflared which name to present for SNI and validate against, while the `Host`
header still selects the right vhost on the far side. nginx serves both vhosts
from the same certificate (`CN=samo.example.com`, its only SAN), so that is
the name that verifies.

Two things worth checking in the Cloudflare dashboard, neither of which this
repo can do for you:

- **SSL/TLS mode should be Full (strict).** Tunnel-routed traffic is encrypted
  edge-to-connector regardless, but "Flexible" is a footgun for anything that
  ever bypasses the tunnel.
- **Always Use HTTPS** on, so a plaintext request is redirected rather than
  served. samo-server also emits HSTS, gated on `X-Forwarded-Proto: https`,
  which samo-proxy sets — so that keeps working through the new path.

## Caching notes

Cache keys deliberately exclude the stream token. samo-server mints those with a
30-minute TTL and the clients re-mint on their own schedule, so the same track
arrives under a different URL several times an hour — a cache keyed on the raw
query would miss every time and re-encode forever. This is the same trap that
once made artwork flash in the Android client, where the image cache was keyed
on the rotating token.

Entries are revalidated against the origin with a one-byte ranged GET before
being served, because samo-server serves audio as `private, max-age=3600`
precisely to allow a file's contents to change under a stable URL (a re-tag, a
replaced download). The ranged-GET trick rather than `HEAD` is borrowed from
samo-server's own podcast stream service, which uses it for the same reason.

A `Range` request against a cold cache is answered from the origin directly
rather than by transcoding the whole file first: it means a seek into something
never encoded, and the honest answer is the original bytes, immediately.
Sequential play is what populates the cache, and every request after that gets
full range support from a complete file on disk.

## What samo-proxy cannot fix

Being honest about the boundary, because three of the slow-link problems in
samo live on the client and no proxy can reach them:

- **The Android client's no-reuse connection pool.** `SamoHttp.kt` sets
  `ConnectionPool(0, …)`, so every call does a fresh TCP + TLS handshake. That
  was the right cure for a real LAN bug (silently half-open sockets, measured
  2026-06-15), and its own comment notes "on a LAN that handshake is a few ms."
  Against a Cloudflare edge over a high-latency link it is 2–3 RTTs on every
  request. Fixing it means gating pool behaviour on which endpoint kind is in
  use — not reverting the original fix.
- **The Android stream disk cache is off.** `SamoPlaybackService.kt` passes
  `enableDiskCache = false`; the 256 MB `SimpleCache` is written and unused. A
  replayed track re-crosses the internet.
- **`DEFAULT_ENDPOINT_PROBE_TIMEOUT_MS = 2_000`** is shared between the LAN and
  remote candidates, and that result decides whether the app drops into offline
  mode. Two seconds for a cold TLS handshake plus `/health` over a slow link is
  tight enough to misclassify a live remote as dead.

## Layout

```
cmd/samo-proxy      wiring, listener, graceful shutdown
internal/config     environment parsing and the trust list
internal/classify   which samo-server route a request is for
internal/forward    header hygiene, artwork sizing, the audio pipeline
internal/compress   gzip, with audio/images/SSE excluded
internal/artwork    width injection against samo-server's ladder
internal/transcode  the ffmpeg policy and command line
internal/cache      disk cache, LRU eviction, follow-while-writing
cloudflared/        the moved tunnel's config (credentials JSON is gitignored)
```

`ffmpeg` is a hard runtime dependency — it is the entire reason this is a Go
service rather than a Caddy config.
