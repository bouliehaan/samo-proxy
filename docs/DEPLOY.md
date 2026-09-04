# Deploying samo-proxy

The edge box, and moving the existing Cloudflare tunnel onto it.

## Shipping it

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
