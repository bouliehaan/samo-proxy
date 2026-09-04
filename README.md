# samo-proxy

The internet-facing edge for a [samo-server](https://github.com/bouliehaan/samo-server)
that lives behind a VPN. It runs `cloudflared` on a box whose default route is
the plain WAN — instead of inside the VPN, which is a tunnel in a tunnel — and
while it is sitting on that boundary it compresses JSON, sizes artwork and
re-encodes lossless audio.

Nothing here is part of samo-server, and samo-server has no knowledge of it. The
only coupling is a URL, so you can stop this stack, or never deploy it at all,
without breaking anything.

```
     phone / laptop / browser
              │  HTTPS, terminated at Cloudflare's edge
       Cloudflare edge
              │  the tunnel
  ┌───────────┴──────────────┐
  │  EDGE BOX — no VPN       │   cloudflared + samo-proxy
  └────────────┬─────────────┘   gzip · artwork sizing · FLAC→Opus · cache
               │  LAN, plain HTTP
  ┌────────────┴─────────────┐
  │  SAMO BOX — ProtonVPN    │   samo-server :6969
  └──────────────────────────┘
```

## Install

On the edge box — a Linux host with Docker that is **not** behind the VPN.

```bash
git clone https://github.com/bouliehaan/samo-proxy.git
cd samo-proxy
cp cloudflared/config.yml.example cloudflared/config.yml   # tunnel id + hostnames
echo 'SAMOPROXY_ORIGIN=http://192.168.1.10:6969' > .env    # your samo-server
docker compose pull && docker compose up -d
```

That pulls [`ghcr.io/bouliehaan/samo-proxy:latest`](https://github.com/bouliehaan/samo-proxy/pkgs/container/samo-proxy)
alongside `cloudflare/cloudflared`. The tunnel's credentials JSON goes in
`cloudflared/` and is gitignored — it is deployment identity, not source.

Moving an existing Cloudflare tunnel onto this box rather than making a new one
is [docs/DEPLOY.md](docs/DEPLOY.md), along with the two checks worth doing
first: that the box's default route is not itself a VPN, and that it can reach
samo-server.

Nothing is published to the LAN. cloudflared reaches samo-proxy over the compose
network, and that is deliberate — see [Trust](#trust).

Running it without Docker:

```bash
SAMOPROXY_ORIGIN=http://192.168.1.10:6969 SAMOPROXY_CACHE_DIR=./cache \
  go run ./cmd/samo-proxy
```

`ffmpeg` is a hard runtime dependency; it is the whole reason this is a Go
service rather than a Caddy config.

## Configuration

Every default is correct for the topology above. In practice only
`SAMOPROXY_ORIGIN` ever needs setting.

| Variable | Default | Notes |
|---|---|---|
| `SAMOPROXY_ADDR` | `127.0.0.1:6767` | Compose overrides to the container network |
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
| `SAMOPROXY_EGRESS_TOKEN` | *(unset)* | Shared secret. **Setting it is what turns egress on.** |
| `SAMOPROXY_EGRESS_ADDR` | `0.0.0.0:6768` when a token is set | Egress listener |
| `SAMOPROXY_EGRESS_ALLOW_HOSTS` | Deezer image CDN only | Closed list; exact host or `.suffix` |
| `SAMOPROXY_EGRESS_PORT` | `127.0.0.1:6768` | Host side of the published port |
| `SAMOPROXY_LOG_LEVEL` | `info` | |

Responses carry `X-Samo-Proxy-Transcode` (`opus@128k` or `passthrough`) and
`X-Samo-Proxy-Cache` (`hit` / `miss`), which is the quickest way to confirm from
a client that the pipeline is doing what you expect.

## Trust

The one security-critical setting is `SAMOPROXY_TRUST_FORWARDED_FROM`.

samo-server keys its brute-force lockout on `CF-Connecting-IP`. Cloudflare's
edge overwrites that header on every request, so what `cloudflared` hands us is
trustworthy — **but only because it came from cloudflared**. Anything else that
can reach this port gets to pick its own rate-limit bucket, and an attacker with
an unlimited supply of buckets has defeated the per-IP lockout.

So the compose file publishes no ports, and the trusted CIDR is the pinned
compose subnet (`172.23.0.0/16`) — pinned rather than left to Docker's pool
because a drifted subnet would silently stop trusting cloudflared, which surfaces
as every client sharing one bucket rather than as an error.

## Letting the samo box out, narrowly

Off unless you set `SAMOPROXY_EGRESS_TOKEN`. It opens a CONNECT proxy the samo
box can use to reach the few hosts that refuse its VPN exit address — Deezer's
image CDN is the case that forced it. Only allow-listed hosts, only CONNECT,
only ports 80 and 443, never into the LAN.

**Traffic through that door does not go through the VPN**, and an artwork URL
names an artist in your library. The tradeoff, and how samo-server falls back
when the proxy is not there, are in
[docs/DESIGN.md](docs/DESIGN.md#letting-the-samo-box-out-narrowly).

## Docs

- [docs/DESIGN.md](docs/DESIGN.md) — why the edge box exists, what the proxy
  does to each kind of response and what it deliberately leaves alone, the
  egress door, why none of this needs a certificate, the cache keys, and the
  three slow-link problems that live in the Android client where no proxy can
  reach them.
- [docs/DEPLOY.md](docs/DEPLOY.md) — moving an existing Cloudflare tunnel onto
  the edge box without recreating it, and the cutover order.
