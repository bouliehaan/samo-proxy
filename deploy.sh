#!/usr/bin/env bash
# deploy.sh — ship samo-proxy to the edge box and bring it up in Docker.
#
# Mirrors sxm-proxy/deploy.sh and deploy-samo.sh: ships the working tree, builds
# ON the target, and verifies before declaring success. Building remotely is
# deliberate — the servers here are x86_64 and the dev machines are arm64, so a
# locally-built image would be the wrong architecture.
#
# IMPORTANT: the target is NOT the samo box. The whole point of samo-proxy is to
# run cloudflared somewhere whose default route is the plain WAN, rather than
# through the samo box's ProtonVPN kill-switch. Deploying this onto 192.168.1.10
# would reproduce exactly the problem it exists to solve, so the script refuses.
#
# It is safe to re-run: every run is a rebuild + restart.
#
#   SAMOPROXY_HOST=192.168.1.12 ./deploy.sh
#   SAMOPROXY_SSH_USER=ubuntu ./deploy.sh
#   SAMOPROXY_ORIGIN=http://192.168.1.10:6969 ./deploy.sh
#
# Requirements: ssh/scp locally; ssh access to the target. NO sudo — the target
# runs docker as a group member and owns its project directory, and the script
# checks both up front rather than discovering it halfway through.

set -euo pipefail

# ---- knobs -------------------------------------------------------------------

HOST="${SAMOPROXY_HOST:-192.168.1.12}"
USER_NAME="${SAMOPROXY_SSH_USER:-jake}"
SRC="${SAMOPROXY_SRC:-$HOME/Developer/samo-proxy}"
REMOTE_TMP="${SAMOPROXY_REMOTE_TMP:-/tmp/samo-proxy-deploy}"
PROJECT_DIR="${SAMOPROXY_PROJECT_DIR:-/opt/samo-proxy}"

# Where samo-server actually is. samo-proxy reaches it over the LAN.
ORIGIN="${SAMOPROXY_ORIGIN:-http://192.168.1.10:6969}"

# The samo box: refused as a deploy target for the reason in the header, and
# also the machine the tunnel credentials currently live on.
SAMO_BOX="${SAMOPROXY_SAMO_BOX:-192.168.1.10}"
SAMO_BOX_USER="${SAMOPROXY_SAMO_BOX_USER:-jake}"

# The tunnel id is read out of cloudflared/config.yml rather than duplicated
# here. That file is gitignored precisely because the tunnel id and hostnames
# are deployment identity, so this script has no business carrying a copy that
# could drift from it — or that would ship a real one to anyone cloning the repo.
TUNNEL_ID="${SAMOPROXY_TUNNEL_ID:-}"
if [ -z "$TUNNEL_ID" ] && [ -f "$SRC/cloudflared/config.yml" ]; then
  TUNNEL_ID="$(sed -n 's/^tunnel:[[:space:]]*\([^[:space:]#]*\).*/\1/p' "$SRC/cloudflared/config.yml" | head -1)"
fi
# Not $HOME: this path is evaluated on the samo box inside a single-quoted ssh
# argument, where neither shell would expand it.
TUNNEL_CREDS_SRC="${SAMOPROXY_TUNNEL_CREDS_SRC:-/home/${SAMO_BOX_USER}/.cloudflared/${TUNNEL_ID}.json}"

# The hostnames for the cutover instructions come out of the (gitignored) tunnel
# config, not from this script. Scrubbing the repo for publication replaced the
# real domain here with example.com, which turned the final instructions into
# URLs that cannot be pasted. Reading them from the config keeps the published
# script generic AND the printed output real.
HOSTNAMES="$(sed -n 's/^[[:space:]]*-[[:space:]]*hostname:[[:space:]]*\([^[:space:]#]*\).*/\1/p' "$SRC/cloudflared/config.yml" 2>/dev/null)"
PRIMARY_HOST="$(printf '%s\n' "$HOSTNAMES" | head -1)"
SECONDARY_HOST="$(printf '%s\n' "$HOSTNAMES" | sed -n 2p)"
[ -n "$PRIMARY_HOST" ]   || PRIMARY_HOST="your-samo-hostname"
[ -n "$SECONDARY_HOST" ] || SECONDARY_HOST="your-other-hostname"

# ---- pretty printing ---------------------------------------------------------

if [ -t 1 ]; then
  C_STEP='\033[1;33m'; C_DIM='\033[2m'; C_OK='\033[1;32m'; C_ERR='\033[1;31m'; C_OFF='\033[0m'
else
  C_STEP=''; C_DIM=''; C_OK=''; C_ERR=''; C_OFF=''
fi
say()  { printf "\n${C_STEP}==>${C_OFF} %s\n" "$*"; }
note() { printf "    ${C_DIM}%s${C_OFF}\n" "$*"; }
warn() { printf "    ${C_ERR}!${C_OFF}  %s\n" "$*"; }
fail() { printf "\n${C_ERR}xx ${C_OFF}%s\n" "$*" >&2; exit 1; }

# ---- sanity ------------------------------------------------------------------

[ -d "$SRC" ] || fail "samo-proxy source not found at $SRC (set SAMOPROXY_SRC to override)"
[ -f "$SRC/Dockerfile" ] || fail "no Dockerfile at $SRC — is this the right source tree?"
[ -f "$SRC/docker-compose.yml" ] || fail "no docker-compose.yml at $SRC"
command -v ssh >/dev/null || fail "ssh not found locally"

if [ "$HOST" = "$SAMO_BOX" ]; then
  fail "refusing to deploy onto the samo box ($SAMO_BOX).
    samo-proxy exists so cloudflared runs OUTSIDE that box's VPN default route.
    Deploying here would put the tunnel back inside ProtonVPN, which is the
    problem this whole thing exists to solve.
    Set SAMOPROXY_HOST to the edge box, or SAMOPROXY_SAMO_BOX if the samo
    server has moved."
fi

[ -f "$SRC/cloudflared/config.yml" ] || fail "no cloudflared/config.yml at $SRC — the tunnel has nothing to run"

# The tunnel config must name the same UUID we are about to fetch credentials
# for, or the connector starts and immediately fails to authenticate.
if ! grep -q "^tunnel: *${TUNNEL_ID}\b" "$SRC/cloudflared/config.yml"; then
  fail "cloudflared/config.yml does not name tunnel ${TUNNEL_ID}.
    Either fix the config or set SAMOPROXY_TUNNEL_ID to the one it does name."
fi

# ---- verify SSH --------------------------------------------------------------

say "Checking SSH to ${USER_NAME}@${HOST}"
if ! ssh -o ConnectTimeout=5 -o BatchMode=yes "${USER_NAME}@${HOST}" 'true' 2>/dev/null; then
  ssh -o ConnectTimeout=10 "${USER_NAME}@${HOST}" 'true' || fail "cannot SSH to ${USER_NAME}@${HOST}"
fi
note "ok"

# ---- confirm the target is NOT on the VPN ------------------------------------
#
# This is the check that makes the whole exercise worth doing, so it runs before
# anything is shipped. If the edge box's egress is also going through the VPN,
# moving cloudflared here changes nothing.

say "Checking the target's egress is not the VPN"
EGRESS_IF="$(ssh "${USER_NAME}@${HOST}" "ip route get 1.1.1.1 2>/dev/null | sed -n 's/.* dev \([^ ]*\).*/\1/p' | head -1" || true)"
if [ -z "$EGRESS_IF" ]; then
  warn "could not determine the default egress interface — continuing"
elif printf '%s' "$EGRESS_IF" | grep -qiE '^(tun|wg|proton|nordlynx|tap)'; then
  fail "the target's default route is ${EGRESS_IF}, which looks like a VPN.
    samo-proxy on a VPN'd box is the same tunnel-inside-a-tunnel it exists to
    avoid. Point SAMOPROXY_HOST at a box whose default route is the plain WAN,
    or split-tunnel this one first."
else
  note "egress interface: ${EGRESS_IF}"
fi

# ---- confirm the target can reach samo-server --------------------------------

say "Checking the target can reach samo-server at ${ORIGIN}"
if ssh "${USER_NAME}@${HOST}" "curl -fsS -o /dev/null --max-time 5 '${ORIGIN}/health'" 2>/dev/null; then
  note "origin reachable"
else
  warn "could not reach ${ORIGIN}/health from ${HOST}"
  note "samo-proxy will start but every request will 502 until this works."
  note "Check the samo box's ufw rules allow ${HOST} to reach its port."
fi

# ---- confirm we can work without sudo ----------------------------------------
#
# This deploy deliberately needs no root. The target runs docker as a group
# member and owns its project directory, so every step below is an ordinary user
# operation. Checking it up front matters because the alternative — discovering
# it mid-run — means sudo tries to prompt for a password down an ssh pipe that
# has no terminal attached, and the error it produces says nothing useful about
# what to fix.

say "Checking the target needs no privilege escalation"
if ! ssh "${USER_NAME}@${HOST}" "docker ps >/dev/null 2>&1"; then
  fail "${USER_NAME}@${HOST} cannot run docker without sudo.
    Fix: sudo usermod -aG docker ${USER_NAME}   (then log out and back in)
    That is a one-time change on the target and avoids this script ever
    needing a password."
fi
PARENT_DIR="$(dirname "$PROJECT_DIR")"
if ! ssh "${USER_NAME}@${HOST}" "test -w '${PARENT_DIR}' || test -w '${PROJECT_DIR}'" 2>/dev/null; then
  fail "${USER_NAME}@${HOST} cannot write ${PROJECT_DIR}.
    Either make it writable, or set SAMOPROXY_PROJECT_DIR to somewhere that is
    (e.g. SAMOPROXY_PROJECT_DIR=\$HOME/samo-proxy)."
fi
note "docker and ${PROJECT_DIR} are usable without root"

# ---- package the source (working tree, so uncommitted changes ship too) ------

say "Packaging source"
LOCAL_TAR="$(mktemp -t samo-proxy-src.XXXXXX).tgz"
trap 'rm -f "$LOCAL_TAR"' EXIT

# COPYFILE_DISABLE stops macOS tar from injecting AppleDouble "._*" metadata
# files into the build context.
# .env is excluded deliberately and shipped separately below. It holds the
# Cloudflare tunnel token, and a credential inside the source bundle ends up in
# /tmp on the target, in the build context, and in any image layer that copies
# the tree. Same rule sxm-proxy's deploy.sh follows.
COPYFILE_DISABLE=1 tar -czf "$LOCAL_TAR" \
  --exclude='.git' \
  --exclude='._*' \
  --exclude='*.tgz' \
  --exclude='.env' \
  --exclude='cloudflared/*.json' \
  -C "$SRC" .
note "$(du -h "$LOCAL_TAR" | cut -f1)"

say "Shipping to ${HOST}:${REMOTE_TMP}"
ssh "${USER_NAME}@${HOST}" "rm -rf '${REMOTE_TMP}' && mkdir -p '${REMOTE_TMP}'"
scp -q "$LOCAL_TAR" "${USER_NAME}@${HOST}:${REMOTE_TMP}/src.tgz"

# .env carries only non-secret overrides now that the tunnel uses a credentials
# file rather than a token, but it is still shipped outside the bundle so the
# rule holds if anything sensitive is ever added to it.
if [ -f "$SRC/.env" ]; then
  say "Shipping credentials separately"
  scp -q "$SRC/.env" "${USER_NAME}@${HOST}:${REMOTE_TMP}/env"
  ssh "${USER_NAME}@${HOST}" "chmod 600 '${REMOTE_TMP}/env'"
  note "ok"
fi

# ---- stage the tunnel credentials, machine to machine ------------------------
#
# Piped straight from the samo box to the cloud server. It never touches this
# laptop's disk, never enters the source bundle, and never enters an image
# layer. If the target already has it, nothing is fetched — a routine re-run
# does not keep handling a credential it does not need to.

REMOTE_CREDS="${PROJECT_DIR}/cloudflared/${TUNNEL_ID}.json"

say "Checking tunnel credentials on ${HOST}"
if ssh "${USER_NAME}@${HOST}" "test -s '${REMOTE_CREDS}'" 2>/dev/null; then
  note "already present"
else
  note "not present — fetching from ${SAMO_BOX}"
  if ! ssh "${SAMO_BOX_USER}@${SAMO_BOX}" "test -s '${TUNNEL_CREDS_SRC}'" 2>/dev/null; then
    fail "tunnel credentials not found at ${SAMO_BOX}:${TUNNEL_CREDS_SRC}
    Set SAMOPROXY_TUNNEL_CREDS_SRC if they live somewhere else."
  fi
  ssh "${USER_NAME}@${HOST}" "mkdir -p '${REMOTE_TMP}'"
  ssh "${SAMO_BOX_USER}@${SAMO_BOX}" "cat '${TUNNEL_CREDS_SRC}'" \
    | ssh "${USER_NAME}@${HOST}" "install -m 600 /dev/stdin '${REMOTE_TMP}/tunnel-creds.json'"
  ssh "${USER_NAME}@${HOST}" "test -s '${REMOTE_TMP}/tunnel-creds.json'" \
    || fail "credentials did not arrive on ${HOST}"
  note "staged, mode 600"
fi

# ---- build and bring up ------------------------------------------------------
#
# No sudo anywhere: the precheck above established that docker and the project
# directory are both usable as this user. That is what keeps this a plain
# `ssh ... bash -s` — a sudo in here would try to prompt for a password on an
# stdin that is already occupied by this heredoc, which is exactly the failure
# `ssh -t` cannot rescue.

say "Building and starting on ${HOST}"
ssh "${USER_NAME}@${HOST}" bash -s <<REMOTE
set -euo pipefail

command -v docker >/dev/null || { echo "docker is not installed on this box" >&2; exit 1; }

mkdir -p '${PROJECT_DIR}'
tar -xzf '${REMOTE_TMP}/src.tgz' -C '${PROJECT_DIR}'

cd '${PROJECT_DIR}'
mkdir -p cloudflared

# Install the separately-staged pieces, if this run brought them.
if [ -f '${REMOTE_TMP}/env' ]; then
  install -m 600 '${REMOTE_TMP}/env' '${PROJECT_DIR}/.env'
fi
if [ -f '${REMOTE_TMP}/tunnel-creds.json' ]; then
  install -m 600 '${REMOTE_TMP}/tunnel-creds.json' '${REMOTE_CREDS}'
fi
touch .env && chmod 600 .env

# The origin travels as an env var rather than being baked into the compose
# file, so the same source tree works against a moved samo-server.
grep -q '^SAMOPROXY_ORIGIN=' .env 2>/dev/null \
  || echo 'SAMOPROXY_ORIGIN=${ORIGIN}' >> .env

# cloudflared runs as this user so it can read its mode-600 credentials. The
# image's default (65532) cannot, and the fix must not be to loosen the file.
sed -i '/^CLOUDFLARED_UID=/d; /^CLOUDFLARED_GID=/d' .env
printf 'CLOUDFLARED_UID=%s\nCLOUDFLARED_GID=%s\n' "\$(id -u)" "\$(id -g)" >> .env

test -s '${REMOTE_CREDS}' \
  || { echo "tunnel credentials missing at ${REMOTE_CREDS}" >&2; exit 1; }

docker compose -f docker-compose.yml -f docker-compose.build.yml up -d --build

# Staging copies have served their purpose; do not leave secrets in /tmp.
rm -rf '${REMOTE_TMP}'
REMOTE

# ---- verify ------------------------------------------------------------------

say "Verifying"
sleep 5

HEALTH_OK=0
for attempt in 1 2 3 4 5 6; do
  if ssh "${USER_NAME}@${HOST}" \
      "docker exec samo-proxy wget -q -O - http://127.0.0.1:6767/_samoproxy/health" 2>/dev/null | grep -q '"ok":true'; then
    HEALTH_OK=1
    break
  fi
  note "not ready yet (attempt ${attempt}/6)"
  sleep 5
done

if [ "$HEALTH_OK" -ne 1 ]; then
  ssh "${USER_NAME}@${HOST}" "cd '${PROJECT_DIR}' && docker compose logs --tail=40 samo-proxy" || true
  fail "samo-proxy did not come up healthy — logs above"
fi
note "samo-proxy healthy and can see samo-server"

# cloudflared needs a real check, not a status glance.
#
# `docker inspect` reports "running" for a container that is in the middle of a
# crash loop, so the first version of this check happily declared success while
# the connector restarted eleven times over unreadable credentials. The only
# thing that actually proves the tunnel works is the edge acknowledging the
# connection, which cloudflared logs as "Registered tunnel connection".

say "Verifying the tunnel connected"
TUNNEL_OK=0
for attempt in 1 2 3 4 5 6 7 8; do
  if ssh "${USER_NAME}@${HOST}" \
      "docker logs samo-cloudflared 2>&1 | grep -q 'Registered tunnel connection'" 2>/dev/null; then
    TUNNEL_OK=1
    break
  fi
  # A restarting container will never register; fail fast rather than waiting
  # out the whole loop for something that is already broken.
  CF_RESTARTS="$(ssh "${USER_NAME}@${HOST}" "docker inspect samo-cloudflared --format '{{.RestartCount}}'" 2>/dev/null || echo 0)"
  if [ "${CF_RESTARTS:-0}" -gt 2 ]; then
    break
  fi
  note "waiting for the edge to acknowledge (attempt ${attempt}/8)"
  sleep 4
done

if [ "$TUNNEL_OK" -ne 1 ]; then
  warn "cloudflared has not registered a tunnel connection"
  ssh "${USER_NAME}@${HOST}" "docker logs --tail=15 samo-cloudflared 2>&1" || true
  fail "the tunnel is not up — samo-proxy is healthy but nothing is reaching it.
    Nothing has changed for your users: the old connector on ${SAMO_BOX} is
    still serving both hostnames, so this is a failed rollout, not an outage."
fi
note "tunnel registered with the Cloudflare edge"

printf "\n${C_OK}done.${C_OFF}\n\n"

# ---- cutover ------------------------------------------------------------------
#
# Deliberately NOT automated. Until the old connector stops, both connectors
# serve the same tunnel and Cloudflare load-balances between them — so about
# half of all traffic still goes through ProtonVPN and the speed fix looks only
# half-effective. But cutting over before the new path is proven would take the
# site down, so this is a decision, printed rather than taken.

say "Cutover — verify first, then retire the old connector"
note ""
note "1. Check BOTH hostnames now serve through the new connector:"
note "     curl -sI https://${PRIMARY_HOST}/health | head -1"
note "     curl -sI https://${SECONDARY_HOST}/ | head -1"
note "   Confirm samo is going through the proxy (this header is proof):"
note "     curl -sI https://${PRIMARY_HOST}/api/v1/music/albums | grep -i samo-proxy"
note ""
note "2. Only once those look right, on the SAMO box (${SAMO_BOX}):"
note "     sudo systemctl disable --now cloudflared"
note ""
note "   Do NOT stop nginx on that box. It still serves ${SECONDARY_HOST},"
note "   which the new tunnel reaches over the LAN at https://${SAMO_BOX}:443."
note "   Only the cloudflared unit moves; nginx stays exactly where it is."
note ""
note "3. Rollback, if anything looks wrong at step 1:"
note "     sudo systemctl start cloudflared        # on ${SAMO_BOX}"
note "     cd ${PROJECT_DIR} && docker compose down                # on ${HOST}"
