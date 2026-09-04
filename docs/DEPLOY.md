# Deploying samo-proxy

The edge box is any Linux host with Docker whose default route is the plain
WAN — **not** the samo box, and not anything behind the VPN. Three things have
to be true before it is worth deploying at all:

```bash
ip route get 1.1.1.1                       # must not leave via a VPN interface
curl -sI http://192.168.1.10:6969/health   # must reach samo-server on the LAN
```

## The tunnel moves, it is not recreated

If you already run a Cloudflare tunnel on the samo box, move it rather than
making a new one. The existing one is **locally managed** — a `config.yml` plus
a credentials JSON, not a dashboard token — so reusing the same tunnel UUID
means the DNS records, the public hostnames and the dashboard configuration all
carry over untouched. Only the machine running the connector changes.

Start from the template. The real `cloudflared/config.yml` is gitignored,
because a tunnel id and a set of hostnames are deployment identity rather than
source:

```bash
cp cloudflared/config.yml.example cloudflared/config.yml
```

Fill in your tunnel id (`cloudflared tunnel list`), your hostnames and your
origin. If that config carries a second hostname — `tv.` back to nginx on the
samo box, in this deployment — carry it across too. Moving the tunnel without it
takes that service offline.

Then move the credentials JSON across without it touching a laptop's disk or a
Docker image layer:

```bash
ssh samo-box "cat ~/.cloudflared/<uuid>.json" \
  | ssh edge-box "install -m 600 /dev/stdin ~/samo-proxy/cloudflared/<uuid>.json"
```

## The uid the connector runs as

cloudflared reads those mode-600 credentials, and the image's default user
(65532) cannot. The alternatives are worse: loosening the file to 644 exposes a
secret to every user on the box, and chowning it to 65532 needs root, which
nothing else here does. So tell compose which user to run it as:

```bash
printf 'CLOUDFLARED_UID=%s\nCLOUDFLARED_GID=%s\n' "$(id -u)" "$(id -g)" >> .env
```

## Up

```bash
docker compose pull
docker compose up -d
```

To run a working tree that is ahead of the published image, `docker compose up
-d --build` instead.

## Cutover

Deliberately manual, and in this order — cutting over before the new path is
proven takes the site down, and never cutting over leaves the job half done.

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
   through the VPN and the speed fix looks only half-effective.

   **Do not stop nginx on that box** if it serves another hostname the tunnel
   points back at. Only the `cloudflared` unit moves.

3. Rollback, if step 1 looks wrong: start `cloudflared` again on the samo box
   and `docker compose down` on the edge box.
