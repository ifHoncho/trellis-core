#!/usr/bin/env bash
# Build Trellis and update bench nodes over SSH.
#
#   scripts/deploy.sh [options] [user@host ...]
#
# Hosts come from the arguments, or from deploy.hosts in the repo root (one
# user@host per line; # comments allowed). For each node: detect its CPU
# architecture, build the trellis package for it once (tools/mkdeb), sync
# only what a node needs to ~/trellis-deploy/ on it, and run
# scripts/node-update.sh there with sudo: it installs the package.
# If mesh-profile.yaml exists in the repo root (git ignores it), it travels
# with the deploy and is installed on nodes that have no profile yet.
# If dev_key.pub exists in the repo root (git ignores it), the nodes trust
# it: it is installed as /etc/trellis/release-keys/deploy-dev.pub (and
# removed again by a deploy without it). With TRELLIS_DEV_KEY=<its secret
# key file> the deploy's trellis packages are signed with it, so they can
# be tried through the web UI, the API or a rollout. Never a key built
# into Trellis (internal/release/trusted.keys): the deploy refuses one.
#
# Options:
#   --no-check    skip gofmt, go vet and unit tests before building
#   --status      only show "meshctl status" from every node
#   --profile     only push mesh-profile.yaml to every node and apply it live
#                 (no build; e.g. after "meshctl profile band 2g mesh-profile.yaml")
#   --web-password  ask for a password once and make it the web UI's admin
#                 password on every node
#   --web-wan on|off  let every node serve its web UI on its uplink network
#                 too (a bench reached over the LAN), or stop that
#   --setup       one-time per node: install your SSH key and let the node
#                 user run node-update.sh and meshctl via sudo without a
#                 password (the user already has sudo; this only removes the prompt)
#   -h, --help
#
# SSH key: set TRELLIS_SSH_KEY=~/.ssh/yourkey if it has a non-default name
# and is not the only key in ~/.ssh.
#
# Nodes with no cable to your network: give their mesh address (10.73.N.1;
# meshctl status on any node lists them) on their deploy.hosts line,
#   user@192.0.2.13 mesh=10.73.89.1
# and when the LAN address does not answer, the node is reached at its mesh
# address through another node of the list that does (its host key is still
# the one known for the LAN address). Or, for every node at once:
#   TRELLIS_SSH_JUMP=user@lan-node scripts/deploy.sh user@10.73.129.1
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
REMOTE_DIR="trellis-deploy"
CHECK=1
MODE=deploy
HOSTS=()
declare -A MESH=()   # LAN host (no user) → mesh address, from deploy.hosts
LISTED=()            # every host in deploy.hosts: candidates to reach the others through

while [ $# -gt 0 ]; do
  case "$1" in
    --no-check) CHECK=0 ;;
    --status) MODE=status ;;
    --profile) MODE=profile ;;
    --setup) MODE=setup ;;
    --web-password) MODE=webpw ;;
    --web-wan) MODE=webwan; WEBWAN="${2:-}"; shift
      case "$WEBWAN" in on|off) ;; *) echo "--web-wan takes on or off" >&2; exit 2 ;; esac ;;
    -h|--help) sed -n '2,43p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    -*) echo "unknown option $1" >&2; exit 2 ;;
    *) HOSTS+=("$1") ;;
  esac
  shift
done
# deploy.hosts gives the hosts when none are named, and the mesh= address of
# any host either way.
if [ -f "$ROOT/deploy.hosts" ]; then
  named=${#HOSTS[@]}
  while IFS= read -r line || [ -n "$line" ]; do
    line="${line%%#*}"
    read -r host extra _ <<<"$line" || true
    [ -n "${host:-}" ] || continue
    [ "$named" -eq 0 ] && HOSTS+=("$host")
    LISTED+=("$host")
    case "${extra:-}" in mesh=*) MESH["${host#*@}"]="${extra#mesh=}" ;; esac
  done < "$ROOT/deploy.hosts"
fi
if [ ${#HOSTS[@]} -eq 0 ]; then
  echo "no hosts: pass user@host arguments or create $ROOT/deploy.hosts (see deploy.hosts.example)" >&2
  exit 2
fi

# One SSH connection per host, reused by every command below.
mkdir -p "$HOME/.ssh"
# ServerAlive*: notice within ~30 s when a node drops off the network
# mid-update, instead of hanging until TCP gives up (node-update.sh runs
# detached on the node, so it still finishes there).
SSH_OPTS=(-o ControlMaster=auto -o "ControlPath=$HOME/.ssh/cm-trellis-%r@%h:%p" -o ControlPersist=120 -o ConnectTimeout=8
          -o ServerAliveInterval=10 -o ServerAliveCountMax=3)

# Key: TRELLIS_SSH_KEY (private key path), else the only key in ~/.ssh when
# it does not have a default id_* name (ssh would not offer it on its own).
KEY="${TRELLIS_SSH_KEY:-}"
if [ -z "$KEY" ] && ! compgen -G "$HOME/.ssh/id_*.pub" >/dev/null; then
  mapfile -t pubs < <(compgen -G "$HOME/.ssh/*.pub" || true)
  [ ${#pubs[@]} -eq 1 ] && KEY="${pubs[0]%.pub}"
fi
[ -n "$KEY" ] && SSH_OPTS+=(-o "IdentityFile=$KEY")
# TRELLIS_SSH_JUMP: go through this host (a node on your network) to reach
# nodes that are only on the mesh. A ProxyCommand rather than -J, so the key
# above is offered to the jump host too.
if [ -n "${TRELLIS_SSH_JUMP:-}" ]; then
  JUMP_CMD="ssh -o ConnectTimeout=8 -o ServerAliveInterval=10 -o ServerAliveCountMax=3"
  [ -n "$KEY" ] && JUMP_CMD+=" -o IdentityFile=$KEY"
  SSH_OPTS+=(-o "ProxyCommand=$JUMP_CMD -W %h:%p $TRELLIS_SSH_JUMP")
fi
# Nodes off the LAN with a mesh= address: through the first node of the
# list that answers, by a ProxyCommand that connects every other host
# directly.
PROXY=""
if [ -z "${TRELLIS_SSH_JUMP:-}" ] && [ ${#MESH[@]} -gt 0 ]; then
  reach() { timeout 3 bash -c 'exec 3<>"/dev/tcp/$0/22"' "$1" 2>/dev/null; }
  declare -A VIA=()
  jump=""
  for h in "${HOSTS[@]}"; do
    reach "${h#*@}" || { [ -n "${MESH[${h#*@}]:-}" ] && VIA["${h#*@}"]=1; }
  done
  if [ ${#VIA[@]} -gt 0 ]; then
    for h in "${HOSTS[@]}" "${LISTED[@]}"; do
      [ -z "${VIA[${h#*@}]:-}" ] && reach "${h#*@}" && { jump="$h"; break; }
    done
  fi
  if [ ${#VIA[@]} -gt 0 ] && [ -n "$jump" ]; then
    PROXY="$(mktemp)"
    {
      echo '#!/usr/bin/env bash'
      echo 'case "$1" in'
      for lan in "${!VIA[@]}"; do
        jcmd="ssh -o ConnectTimeout=8 -o ServerAliveInterval=10 -o ServerAliveCountMax=3"
        [ -n "$KEY" ] && jcmd+=" -o IdentityFile=$KEY"
        printf '  %q) exec %s -W %q:"$2" %q ;;\n' "$lan" "$jcmd" "${MESH[$lan]}" "$jump"
        echo "-- ${lan} is not on the LAN: reaching it over the mesh at ${MESH[$lan]} through ${jump}" >&2
      done
      echo 'esac'
      # Everyone else: straight to the host.
      echo 'exec 3<>"/dev/tcp/$1/$2" || exit 1'
      echo 'cat <&3 & cat >&3'
    } > "$PROXY"
    chmod +x "$PROXY"
    SSH_OPTS+=(-o "ProxyCommand=$PROXY %h %p")
    # Nodes reached over the mesh go first: updating the others restarts
    # their meshd, and with it the mesh the path runs over.
    first=() rest=()
    for h in "${HOSTS[@]}"; do
      if [ -n "${VIA[${h#*@}]:-}" ]; then first+=("$h"); else rest+=("$h"); fi
    done
    HOSTS=("${first[@]}" "${rest[@]}")
  fi
fi
# rsync takes its ssh command as one string. The options above survive that
# only while none contains a space; the ProxyCommand does, so it goes
# through a wrapper that keeps each option whole.
RSYNC_RSH="ssh ${SSH_OPTS[*]}"
if [ -n "${TRELLIS_SSH_JUMP:-}" ] || [ -n "$PROXY" ]; then
  RSYNC_RSH="$(mktemp)"
  trap 'rm -f "$RSYNC_RSH" ${PROXY:+"$PROXY"}' EXIT
  { echo '#!/usr/bin/env bash'; printf 'exec ssh'; printf ' %q' "${SSH_OPTS[@]}"; echo ' "$@"'; } > "$RSYNC_RSH"
  chmod +x "$RSYNC_RSH"
fi
rssh() { ssh "${SSH_OPTS[@]}" "$@"; }
say() { printf '\n\033[1m### %s\033[0m\n' "$*"; }

if [ "$MODE" = status ]; then
  for h in "${HOSTS[@]}"; do
    say "$h"
    rssh -t "$h" 'sudo meshctl status' || echo "(failed)"
  done
  exit 0
fi

if [ "$MODE" = webpw ]; then
  printf 'Admin password for the web UI on %d node(s): ' "${#HOSTS[@]}"; read -rs PW; echo
  printf 'Again: '; read -rs PW2; echo
  [ "$PW" = "$PW2" ] || { echo "the two passwords differ" >&2; exit 1; }
  [ ${#PW} -ge 8 ] || { echo "use at least 8 characters" >&2; exit 1; }
  rc=0
  for h in "${HOSTS[@]}"; do
    say "$h"
    printf '%s\n' "$PW" | rssh "$h" 'sudo -n meshctl web password --stdin' || { rc=1; echo "(failed: needs the current meshctl, and scripts/deploy.sh --setup for password-less sudo)"; }
  done
  exit $rc
fi

if [ "$MODE" = webwan ]; then
  rc=0
  for h in "${HOSTS[@]}"; do
    say "$h"
    rssh "$h" "sudo -n meshctl web wan-access $WEBWAN" || { rc=1; echo "(failed)"; }
    [ "$WEBWAN" = on ] && echo "   https://${h#*@}/"
  done
  exit $rc
fi

if [ "$MODE" = profile ]; then
  [ -f "$ROOT/mesh-profile.yaml" ] || { echo "no $ROOT/mesh-profile.yaml (fetch it: ssh <node> sudo meshctl profile show > mesh-profile.yaml)" >&2; exit 2; }
  declare -A PRESULT
  for h in "${HOSTS[@]}"; do
    say "$h: install profile"
    if rssh "$h" 'sudo -n meshctl profile install -' < "$ROOT/mesh-profile.yaml"; then
      PRESULT[$h]=ok
    else
      PRESULT[$h]=FAILED
      echo "   (needs the current meshctl: run make deploy once; and scripts/deploy.sh --setup for password-less sudo)"
    fi
  done
  say "Waiting for the mesh to re-form"
  sleep "${SETTLE:-12}"
  rc=0
  for h in "${HOSTS[@]}"; do
    say "$h (${PRESULT[$h]})"
    rssh "$h" 'sudo -n meshctl status' 2>&1 \
      | grep -E '^(mesh "|Mesh link|batman-adv:|Warnings|  \[ERROR\])|^  ([0-9a-f]{2}:){5}[0-9a-f]{2} ' || true
    [ "${PRESULT[$h]}" = ok ] || rc=1
  done
  exit $rc
fi

if [ "$MODE" = setup ]; then
  for h in "${HOSTS[@]}"; do
    say "$h: SSH key"
    if [ -n "$KEY" ]; then ssh-copy-id -i "$KEY.pub" "$h"; else ssh-copy-id "$h"; fi
    say "$h: passwordless sudo for the update script and meshctl"
    user="${h%%@*}"; [ "$user" = "$h" ] && user="$(whoami)"
    rule="$user ALL=(root) NOPASSWD: /home/$user/$REMOTE_DIR/scripts/node-update.sh, /usr/bin/meshctl"
    rssh -t "$h" "echo '$rule' | sudo tee /etc/sudoers.d/trellis-deploy >/dev/null && sudo chmod 0440 /etc/sudoers.d/trellis-deploy && sudo visudo -cf /etc/sudoers.d/trellis-deploy"
  done
  exit 0
fi

cd "$ROOT"
if [ "$CHECK" = 1 ]; then
  say "Checks (gofmt, vet, unit tests)"
  make -s check
fi

base="$(git describe --tags --always --dirty 2>/dev/null || echo dev)"
VERSION="${base#v}+$(date +%m%d.%H%M%S)"
declare -A ARCH_OF BUILT
say "Build $VERSION"
# Development builds are signed only with the development key, which only
# the nodes deployed with dev_key.pub trust; a key built into Trellis would
# make every node take them.
DEV_SIGN=()
if [ -n "${TRELLIS_DEV_KEY:-}" ]; then
  devpub="$(go run ./tools/mkdeb pubkey --key "$TRELLIS_DEV_KEY" | cut -d' ' -f1-3)"
  if grep -q -F "$devpub" internal/release/trusted.keys; then
    echo "TRELLIS_DEV_KEY is a release key built into Trellis: every node would take these development builds. Use the development key." >&2
    exit 1
  fi
  if ! [ -f dev_key.pub ] || ! grep -q -F "$devpub" dev_key.pub; then
    echo "TRELLIS_DEV_KEY's public key is not in dev_key.pub, so the nodes would not trust what it signs:" >&2
    echo "  go run ./tools/mkdeb pubkey --key $TRELLIS_DEV_KEY --comment dev > dev_key.pub" >&2
    exit 1
  fi
  DEV_SIGN=(--key "$TRELLIS_DEV_KEY" --sign-dev)
  echo "(trellis packages signed with the development key)"
fi
if [ -f dev_key.pub ]; then
  devlines="$(grep -v -E '^[[:space:]]*(#|$)' dev_key.pub || true)"
  if [ -z "$devlines" ] || grep -v -q -E '^trellis-release-key v1 [A-Za-z0-9+/]{43}=( .*)?$' <<<"$devlines"; then
    echo "dev_key.pub is not a Trellis release public key (go run ./tools/mkdeb pubkey --key FILE makes one)" >&2
    exit 1
  fi
fi
rm -rf dist/deploy/batman-adv
# The batman-adv package needs the pinned release's tarball, fetched once.
# Without it (no network here) nodes keep, or fetch, their own copy.
BATADV_TARBALL="dist/cache/batman-adv-$(sed -n 's/^BATADV_VERSION=//p' packaging/dkms/source).tar.gz"
[ -f "$BATADV_TARBALL" ] || make -s "$BATADV_TARBALL" || echo "(no batman-adv tarball: deploying without the trellis-batman-adv-dkms package)"
for h in "${HOSTS[@]}"; do
  # The package must match the userland, which dpkg names; the kernel may
  # differ (Raspberry Pi OS 32-bit boots a 64-bit kernel). A few tries: a
  # node reached over the mesh may be cut off for a moment (a node on the
  # path restarting).
  m=""
  for try in 1 2 3; do
    m="$(rssh "$h" 'dpkg --print-architecture 2>/dev/null || uname -m')" && [ -n "$m" ] && break
    m=""
    [ "$try" -lt 3 ] && { echo "($h did not answer; trying again)" >&2; sleep 5; }
  done
  [ -n "$m" ] || { echo "cannot reach $h" >&2; exit 1; }
  case "$m" in
    arm64|aarch64) a=arm64 ;;
    # armhf: one build for every 32-bit ARM board, the Pi Zero's ARMv6 included.
    armhf|armv7l|armv7|armv6l) a=armv6 ;;
    amd64|x86_64) a=amd64 ;;
    *) echo "$h: unsupported architecture $m" >&2; exit 1 ;;
  esac
  ARCH_OF[$h]=$a
  if [ -z "${BUILT[$a]:-}" ]; then
    goarch=$a; goarm=""
    [ "$a" = armv6 ] && { goarch=arm; goarm=6; }
    out="dist/deploy/$a/bin/trellis"
    rm -rf "dist/deploy/$a"
    mkdir -p "dist/deploy/$a/bin"
    CGO_ENABLED=0 GOOS=linux GOARCH=$goarch GOARM=$goarm go build -trimpath \
      -ldflags "-s -w -X main.version=$VERSION" -o "$out" ./cmd/trellis
    debarch=$a; [ "$a" = armv6 ] && debarch=armhf
    go run ./tools/mkdeb trellis --version "$VERSION" --arch "$debarch" --binary "$out" --out "dist/deploy/$a/deb" "${DEV_SIGN[@]}"
    if [ -f "$BATADV_TARBALL" ]; then
      [ -n "$(ls dist/deploy/batman-adv/*.deb 2>/dev/null)" ] ||
        go run ./tools/mkdeb batman-adv --source "$BATADV_TARBALL" --out dist/deploy/batman-adv --epoch "$(git log -1 --format=%ct)"
      cp dist/deploy/batman-adv/*.deb "dist/deploy/$a/deb/"
    fi
    rm -rf "dist/deploy/$a/bin"   # the package carries the binary
    BUILT[$a]=1
  fi
done

# update_outcome reconnects to a node whose session dropped and prints the
# update's exit status from its log, once it has one (up to 2 minutes).
update_outcome() {
  local line
  for _ in $(seq 1 24); do
    sleep 5
    line="$(rssh "$1" 'tail -n 1 /var/log/trellis-node-update.log' 2>/dev/null)" || continue
    case "$line" in "== node-update exit "*) echo "${line##* }"; return 0 ;; esac
  done
  return 1
}

declare -A RESULT
for h in "${HOSTS[@]}"; do
  a="${ARCH_OF[$h]}"
  say "$h ($a): sync"
  stage="dist/deploy/$a"
  rm -rf "$stage/scripts" "$stage/packaging" "$stage/profile.yaml" "$stage/dev-key.pub"
  cp -r scripts packaging "$stage/"
  if [ -f mesh-profile.yaml ]; then
    install -m 0600 mesh-profile.yaml "$stage/profile.yaml"
  fi
  if [ -f dev_key.pub ]; then
    install -m 0644 dev_key.pub "$stage/dev-key.pub"
  fi
  if rssh "$h" command -v rsync >/dev/null 2>&1; then
    rsync -a --delete -e "$RSYNC_RSH" "$stage/" "$h:$REMOTE_DIR/"
  else
    rssh "$h" "rm -rf $REMOTE_DIR && mkdir -p $REMOTE_DIR"
    scp -q -r -o "ControlPath=$HOME/.ssh/cm-trellis-%r@%h:%p" "$stage/." "$h:$REMOTE_DIR/"
  fi
  say "$h: update"
  if rssh -t "$h" 'sudo "$HOME/'"$REMOTE_DIR"'/scripts/node-update.sh"'; then
    RESULT[$h]=ok
  elif rc="$(update_outcome "$h")" && [ "$rc" = 0 ]; then
    # The session went (a node reached over the mesh loses its path when
    # its own meshd restarts), but the update finished.
    RESULT[$h]="ok (the session dropped; the update finished)"
  elif [ -n "$rc" ]; then
    RESULT[$h]="FAILED (exit $rc; see /var/log/trellis-node-update.log on the node)"
  else
    RESULT[$h]="FAILED or disconnected (the update runs on; see /var/log/trellis-node-update.log on the node)"
  fi
done

say "Summary ($VERSION)"
rc=0
for h in "${HOSTS[@]}"; do
  printf '  %-28s %s\n' "$h" "${RESULT[$h]}"
  [ "${RESULT[$h]}" = ok ] || rc=1
done
exit $rc
