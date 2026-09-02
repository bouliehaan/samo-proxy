# samo-proxy

The internet-facing edge for a `samo-server` that lives behind a VPN.

Deliberately siloed from `samo-server`, in the same way `sxm-proxy` is: nothing
here is part of samo, samo has no knowledge of it, and the only coupling is a
URL. samo-server is unchanged and does not need to know this exists.

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

**Gets the forwarding headers right.** See [Trust](#trust) below — this is the
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

## Configuration

Everything has a default that is correct for the topology above. In practice
only `SAMOPROXY_ORIGIN` ever needs setting.

| Variable | Default | Notes |
|---|---|---|
| `SAMOPROXY_ADDR` | `127.0.0.1:6767` | Loopback by default; compose overrides to the container network |
| `SAMOPROXY_ORIGIN` | `http://192.168.1.10:6969` | samo-server on the LAN |
| `SAMOPROXY_FORWARDED_PROTO` | `https` | What to tell samo-server the client's scheme was |
| `SAMOPROXY_TRUST_FORWARDED_FROM` | `127.0.0.0/8,::1/128` | Whose `CF-Connecting-IP` to believe |
| `SAMOPROXY_TRANSCODE` | `true` | |
| `SAMOPROXY_TRANSCODE_CODEC` | `opus` | `opus`, `aac` or `mp3` |
| `SAMOPROXY_TRANSCODE_BITRATE` | `128` | kbps |
| `SAMOPROXY_TRANSCODE_LOSSY` | `false` | Also re-encode lossy sources |
| `SAMOPROXY_IMAGE_DEFAULT_WIDTH` | `768` | `0` disables injection |
| `SAMOPROXY_CACHE_DIR` | `/var/cache/samo-proxy` | |
| `SAMOPROXY_CACHE_MAX_MB` | `16384` | LRU eviction above this |
| `SAMOPROXY_COMPRESS_MIN_BYTES` | `1024` | Below this, gzip costs more than it saves |
| `SAMOPROXY_FFMPEG` | `ffmpeg` | |
| `SAMOPROXY_LOG_LEVEL` | `info` | |

Responses carry `X-Samo-Proxy-Transcode` (`opus@128k` or `passthrough`) and
`X-Samo-Proxy-Cache` (`hit` / `miss`), which is the quickest way to confirm the
pipeline is doing what you expect from a client.

## Run locally

```bash
SAMOPROXY_ORIGIN=http://192.168.1.10:6969 \
SAMOPROXY_CACHE_DIR=./cache \
SAMOPROXY_ADDR=127.0.0.1:6767 \
go run ./cmd/samo-proxy
```

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

## Deploy

```bash
./deploy.sh
```

Ships the working tree, builds on the target, and verifies before declaring
success — same pattern as `deploy-samo.sh` and `sxm-proxy/deploy.sh`. Before
shipping anything it checks three things that would make the exercise pointless
if wrong: that the target is not the samo box, that the target's default route
is not itself a VPN interface, and that the target can actually reach
samo-server.

### The tunnel moves, it is not recreated

The existing tunnel is **locally managed** — a `config.yml` plus a credentials
JSON, not a dashboard token. That is a good thing: reusing the same tunnel UUID
means the DNS records, the public hostnames and the dashboard configuration all
carry over untouched. Only the machine running the connector moves.

Start from the template — the real `cloudflared/config.yml` is gitignored,
because a tunnel id and a set of hostnames are deployment identity rather than
source:

```bash
cp cloudflared/config.yml.example cloudflared/config.yml
```

Fill in your tunnel id (`cloudflared tunnel list`), your hostnames and your
origin. `deploy.sh` reads the tunnel id straight out of that file, so there is
only ever one copy of it.

That config carries **both** hostnames — `samo.` to samo-proxy, `tv.` back to nginx on the
samo box. Moving the tunnel without carrying `tv.` across would take Jellyfin
offline.

The credentials JSON is gitignored and never enters the source bundle.
`deploy.sh` pipes it straight from the samo box to the cloud server:

```
ssh samo-box "cat <uuid>.json" | ssh cloud-server "install -m 600 /dev/stdin …"
```

It never lands on the laptop's disk and never enters a Docker image layer. If
the target already has it, nothing is fetched.

### Cutover

Deliberately manual, and in this order — cutting over before the new path is
proven would take the site down, and never cutting over leaves the job half
done.

1. Verify **both** hostnames through the new connector, and confirm samo is
   actually going through the proxy:

   ```bash
   curl -sI https://samo.example.com/health | head -1
   curl -sI https://tv.example.com/ | head -1
   curl -sI https://samo.example.com/api/v1/music/albums | grep -i samo-proxy
   ```

2. Only then, on the samo box, retire the old connector:

   ```bash
   sudo systemctl disable --now cloudflared
   ```

   Until you do, **both connectors serve the same tunnel and Cloudflare
   load-balances between them** — so roughly half of all traffic still goes
   through ProtonVPN and the speed fix looks only half-effective.

   **Do not stop nginx on that box.** It still serves `tv.example.com`,
   which the new tunnel reaches over the LAN. Only the `cloudflared` unit moves.

3. Rollback, if step 1 looks wrong: start `cloudflared` again on the samo box
   and `docker compose down` on the cloud server.

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
