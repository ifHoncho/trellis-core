#!/usr/bin/env bash
# Build Trellis and put it on the test nodes over SSH (make deploy).
#
#   scripts/deploy.sh [options] [user@host ...]
#   FAST=1 scripts/deploy.sh        no health check: quicker iterations
#   DRY_RUN=1 scripts/deploy.sh     build, and show what would be done
#
# Hosts come from the arguments, or from deploy.hosts in the repo root (one
# user@host per line, mesh=ADDRESS optional; # comments allowed). The
# deploy builds the trellis package for the architectures the nodes have,
# signed with the development key (TRELLIS_DEV_KEY, its public line in
# dev_key.pub; git ignores it), and hands them to trellis-deploy, as a user
# would: every node is looked at, then upgraded in turn with meshctl
# upgrade, which checks the node and goes back if it is not healthy
# (FAST=1: with apt, no health check), nodes reached over the mesh first.
# The nodes are made to trust the development key
# (/etc/trellis/release-keys/deploy-dev.pub); a key built into Trellis is
# refused. If mesh-profile.yaml exists in the repo root (git ignores it), a
# node without a profile gets it.
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
#   --setup       one-time per node: install your SSH key
#   --setup --sudo-nopasswd  also let the node user use sudo without a
#                 password, so deploys stop asking (whoever logs in as that
#                 user with your key is then root on the node)
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
    --sudo-nopasswd) SUDO_NOPASS=1 ;;
    --web-password) MODE=webpw ;;
    --web-wan) MODE=webwan; WEBWAN="${2:-}"; shift
      case "$WEBWAN" in on|off) ;; *) echo "--web-wan takes on or off" >&2; exit 2 ;; esac ;;
    -h|--help) sed -n '2,45p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
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
# ServerAlive*: notice within ~30 s when a node drops off the network,
# instead of hanging until TCP gives up.
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
    [ -n "${SUDO_NOPASS:-}" ] || continue
    # All of sudo: the deploy runs its scripts as root (sudo sh …), which a
    # narrower rule would allow anyway. Asked for explicitly only.
    say "$h: sudo without a password"
    user="${h%%@*}"; [ "$user" = "$h" ] && user="$(whoami)"
    rule="$user ALL=(ALL) NOPASSWD: ALL"
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
say "Build $VERSION"
# The packages are signed with the development key, which the deploy makes
# the nodes trust (dev_key.pub): through meshctl upgrade a node takes only
# a package signed by a key it trusts. Never a key built into Trellis:
# every node would take these builds.
DEVKEY="${TRELLIS_DEV_KEY:-}"
if [ -z "$DEVKEY" ] || ! [ -f dev_key.pub ]; then
  echo "make deploy signs the packages with the development key: set TRELLIS_DEV_KEY to its secret file, with its public line in dev_key.pub" >&2
  echo "(docs/dev/building.md, \"Deploying to test nodes\"). To make one:" >&2
  echo "  go run ./tools/mkdeb keygen --out ~/keys/trellis-dev.key --comment dev > dev_key.pub" >&2
  exit 2
fi
devpub="$(go run ./tools/mkdeb pubkey --key "$DEVKEY" | cut -d' ' -f1-3)"
if grep -q -F "$devpub" internal/release/trusted.keys; then
  echo "TRELLIS_DEV_KEY is a release key built into Trellis: every node would take these development builds. Use the development key." >&2
  exit 1
fi
if ! grep -q -F "$devpub" dev_key.pub; then
  echo "TRELLIS_DEV_KEY's public key is not in dev_key.pub, so the nodes would not trust what it signs:" >&2
  echo "  go run ./tools/mkdeb pubkey --key $DEVKEY --comment dev > dev_key.pub" >&2
  exit 1
fi

OUT=dist/deploy/release
rm -rf dist/deploy
mkdir -p "$OUT"
declare -A BUILT
for h in "${HOSTS[@]}"; do
  # The package must match the userland, which dpkg names; the kernel may
  # differ (Raspberry Pi OS 32-bit boots a 64-bit kernel). A few tries: a
  # node reached over the mesh may be cut off for a moment.
  m=""
  for try in 1 2 3; do
    m="$(rssh "$h" 'dpkg --print-architecture 2>/dev/null || uname -m')" && [ -n "$m" ] && break
    m=""
    [ "$try" -lt 3 ] && { echo "($h did not answer; trying again)" >&2; sleep 5; }
  done
  [ -n "$m" ] || { echo "($h does not answer: trellis-deploy will skip it)" >&2; continue; }
  case "$m" in
    arm64|aarch64) a=arm64 goarch=arm64 goarm="" ;;
    # armhf: one build for every 32-bit ARM board, the Pi Zero's ARMv6 included.
    armhf|armv7l|armv7|armv6l) a=armhf goarch=arm goarm=6 ;;
    amd64|x86_64) a=amd64 goarch=amd64 goarm="" ;;
    *) echo "$h: unsupported architecture $m" >&2; continue ;;
  esac
  [ -z "${BUILT[$a]:-}" ] || continue
  CGO_ENABLED=0 GOOS=linux GOARCH=$goarch GOARM=$goarm go build -trimpath \
    -ldflags "-s -w -X main.version=$VERSION" -o "dist/deploy/trellis-$a" ./cmd/trellis
  go run ./tools/mkdeb trellis --version "$VERSION" --arch "$a" --binary "dist/deploy/trellis-$a" --out "$OUT" --key "$DEVKEY" --sign-dev
  BUILT[$a]=1
done
[ ${#BUILT[@]} -gt 0 ] || { echo "no node answered" >&2; exit 1; }
# The batman-adv package needs the pinned release's tarball, fetched once.
# Without it (no network here) nodes keep, or fetch, their own copy.
BATADV_TARBALL="dist/cache/batman-adv-$(sed -n 's/^BATADV_VERSION=//p' packaging/dkms/source).tar.gz"
[ -f "$BATADV_TARBALL" ] || make -s "$BATADV_TARBALL" || echo "(no batman-adv tarball: deploying without the trellis-batman-adv-dkms package)"
if [ -f "$BATADV_TARBALL" ]; then
  go run ./tools/mkdeb batman-adv --source "$BATADV_TARBALL" --out "$OUT" --epoch "$(git log -1 --format=%ct)" --key "$DEVKEY"
fi
( cd "$OUT" && sha256sum -- *.deb > SHA256SUMS )
CGO_ENABLED=0 go build -trimpath -o dist/deploy/trellis-deploy ./cmd/trellis-deploy

# trellis-deploy does the rest, as it does for users: every node checked,
# then each one upgraded in turn (meshctl upgrade, which checks the node and
# goes back if it is not healthy; FAST=1: apt, no health check), nodes
# reached over the mesh first. It finds them through deploy.hosts' mesh=
# addresses itself.
hosts="$(mktemp)"
trap 'rm -f "$hosts" ${RSYNC_RSH:+"$RSYNC_RSH"} ${PROXY:+"$PROXY"}' EXIT
for h in "${HOSTS[@]}"; do
  if [ -n "${MESH[${h#*@}]:-}" ]; then echo "$h mesh=${MESH[${h#*@}]}"; else echo "$h"; fi
done > "$hosts"
args=(--from "$OUT" --trust dev_key.pub --node-key dev_key.pub --any-version --yes)
[ -f mesh-profile.yaml ] && args+=(--profile mesh-profile.yaml)
[ -n "${FAST:-}" ] && args+=(--no-health-check)
[ -n "${DRY_RUN:-}" ] && args+=(--dry-run)
[ -n "$KEY" ] && args+=(--ssh-option "IdentityFile=$KEY")
if [ -n "${TRELLIS_SSH_JUMP:-}" ]; then
  jcmd="ssh -o ConnectTimeout=8 -o ServerAliveInterval=10 -o ServerAliveCountMax=3"
  [ -n "$KEY" ] && jcmd+=" -o IdentityFile=$KEY"
  args+=(--ssh-option "ProxyCommand=$jcmd -W %h:%p $TRELLIS_SSH_JUMP")
fi
say "Deploy $VERSION"
rc=0
dist/deploy/trellis-deploy "${args[@]}" "$hosts" || rc=$?
exit $rc
