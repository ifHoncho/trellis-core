#!/usr/bin/env bash
# trellis-deploy from end to end (make deploy-test): two containers run
# sshd, one with sudo that takes no password and one with sudo that asks
# for one, and trellis-deploy, on this machine, works on them as it would on
# nodes:
#   1. installs release 0.9.0 with a mesh profile and callsigns, while a
#      third listed node does not answer (skipped, the run goes on);
#   2. upgrades both to 0.9.1 with meshctl upgrade;
#   3. runs again: nothing to do;
#   4. a wrong sudo password three times leaves that node out, and the
#      other is reinstalled (--force);
#   5. make deploy's way: an older build (--any-version) with apt
#      (--no-health-check), the key the nodes must trust sent along
#      (--node-key);
#   6. --setup leaves sudo alone; --setup --sudo-nopasswd gives sudo
#      without a password where it asked (the SSH key half needs a person
#      at ssh-copy-id's password prompt).
# There is no systemd in a container: no reboot (--no-reboot), and
# meshctl prepare reports what a container cannot do.
#
# Needs podman (or docker), ssh and ssh-keygen, and network access for the
# distributions' packages.
#
#   scripts/deploy-test.sh [image image]   default: debian:13 ubuntu:22.04
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"
RT="$(command -v podman || command -v docker || true)"
[ -n "$RT" ] || { echo "needs podman or docker" >&2; exit 2; }
IMAGES=("$@"); [ ${#IMAGES[@]} -eq 2 ] || IMAGES=(docker.io/library/debian:13 docker.io/library/ubuntu:22.04)
case "$(uname -m)" in x86_64) arch=amd64 ;; aarch64) arch=arm64 ;; *) echo "unsupported build machine" >&2; exit 2 ;; esac
PASSWORD="deploy-test-sudo"
PORTS=(2201 2202)
NAMES=(td-nopass td-password)

W="$(mktemp -d)"
cleanup() {
  for n in "${NAMES[@]}"; do "$RT" rm -f "$n" >/dev/null 2>&1 || true; done
  rm -rf "$W"
}
trap cleanup EXIT
fail() {
  echo "FAIL: $*" >&2
  for f in "$W"/logs/*.log; do [ -f "$f" ] && { echo "--- $f"; tail -n 40 "$f"; }; done >&2
  exit 1
}
ok() { echo "ok   $*"; }

echo "== building two releases, signed with a throwaway key"
tarball="dist/cache/batman-adv-$(sed -n 's/^BATADV_VERSION=//p' packaging/dkms/source).tar.gz"
[ -f "$tarball" ] || make -s "$tarball"
go run ./tools/mkdeb keygen --out "$W/key" --comment "deploy-test" > "$W/key.pub" 2>/dev/null
for v in 0.9.0 0.9.1; do
  mkdir -p "$W/rel-$v"
  CGO_ENABLED=0 GOOS=linux GOARCH=$arch go build -trimpath -ldflags "-s -w -X main.version=$v" -o "$W/trellis-$v" ./cmd/trellis
  CGO_ENABLED=0 GOOS=linux GOARCH=$arch go build -trimpath -ldflags "-s -w -X main.version=$v" -o "$W/trellis-voiced-$v" ./cmd/trellis-voiced
  go run ./tools/mkdeb trellis --version "$v" --arch "$arch" --binary "$W/trellis-$v" --voiced "$W/trellis-voiced-$v" --out "$W/rel-$v" --key "$W/key" --sign-dev >/dev/null
  go run ./tools/mkdeb batman-adv --source "$tarball" --out "$W/rel-$v" --key "$W/key" >/dev/null
  ( cd "$W/rel-$v" && sha256sum -- *.deb > SHA256SUMS )
done
CGO_ENABLED=0 go build -trimpath -o "$W/trellis-deploy" ./cmd/trellis-deploy
"$W/trellis-0.9.1" meshctl profile new --name deploy-test -o "$W/profile.yaml" >/dev/null
ssh-keygen -q -t ed25519 -N '' -f "$W/id"
mkdir -p "$W/keys" && cp "$W/id.pub" "$W/keys/"

echo "== starting the containers (they install sshd and sudo first)"
for i in 0 1; do
  if [ "$i" = 0 ]; then rule='op ALL=(ALL) NOPASSWD: ALL'; else rule='op ALL=(ALL) ALL'; fi
  "$RT" rm -f "${NAMES[$i]}" >/dev/null 2>&1 || true
  # Shared label (z): both containers read the key ("Z" would give each
  # its own, and the second would lock the first out).
  "$RT" run -d --name "${NAMES[$i]}" --hostname "${NAMES[$i]}" -p "127.0.0.1:${PORTS[$i]}:22" \
    -v "$W/keys:/keys:ro,z" "${IMAGES[$i]}" sh -c "
      export DEBIAN_FRONTEND=noninteractive
      apt-get update -q >/dev/null && apt-get install -y -q --no-install-recommends openssh-server sudo >/dev/null || exit 1
      useradd -m -s /bin/sh op && echo 'op:$PASSWORD' | chpasswd
      mkdir -p /home/op/.ssh && cp /keys/id.pub /home/op/.ssh/authorized_keys
      chown -R op:op /home/op/.ssh && chmod 700 /home/op/.ssh
      echo '$rule' > /etc/sudoers.d/op && chmod 0440 /etc/sudoers.d/op
      mkdir -p /run/sshd && ssh-keygen -A >/dev/null
      # Newer sshd shuts out a source after failed logins; not here.
      /usr/sbin/sshd -t -o PerSourcePenalties=no 2>/dev/null && echo 'PerSourcePenalties no' >> /etc/ssh/sshd_config
      exec /usr/sbin/sshd -D -e" >/dev/null
done
SSHOPT=(--ssh-option StrictHostKeyChecking=no --ssh-option UserKnownHostsFile=/dev/null
  --ssh-option "IdentityFile=$W/id" --ssh-option IdentitiesOnly=yes --ssh-option LogLevel=ERROR)
for i in 0 1; do
  for _ in $(seq 1 150); do
    ssh -o BatchMode=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR -o IdentitiesOnly=yes \
      -i "$W/id" -p "${PORTS[$i]}" op@127.0.0.1 true 2>/dev/null && break
    sleep 2
  done
  ssh -o BatchMode=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR -o IdentitiesOnly=yes \
    -i "$W/id" -p "${PORTS[$i]}" op@127.0.0.1 true 2>/dev/null || { "$RT" logs "${NAMES[$i]}" | tail -20; fail "${NAMES[$i]}: no ssh"; }
done
ok "two containers answer ssh (${IMAGES[0]}, ${IMAGES[1]})"

cat > "$W/hosts" <<EOF
op@127.0.0.1 port=${PORTS[0]} callsign=alpha-1 id=11
op@127.0.0.1 port=${PORTS[1]} callsign=alpha-2
op@127.0.0.1 port=2299 callsign=gone      # nothing listens: skipped
EOF
deploy() { # stdin: the answers to the password prompts
  "$W/trellis-deploy" --trust "$W/key.pub" --no-reboot --yes --logs "$W/logs" "${SSHOPT[@]}" "$@" "$W/hosts"
}
on() { "$RT" exec "$1" sh -c "$2"; }

echo "== 1. a new install, with a mesh profile"
out="$(printf '%s\n' "$PASSWORD" | deploy --from "$W/rel-0.9.0" --profile "$W/profile.yaml" 2>&1)" || { echo "$out" | tail -30; fail "install run"; }
for i in 0 1; do
  n="${NAMES[$i]}"
  [ "$(on "$n" "dpkg-query -W -f='\${Version}' trellis")" = 0.9.0 ] || { echo "$out" | tail -30; fail "$n: trellis 0.9.0 not installed"; }
  [ "$(on "$n" 'stat -c %a /etc/trellis/profile.yaml')" = 600 ] || fail "$n: mesh profile missing or not 0600"
  on "$n" "grep -q 'callsign: alpha-$((i + 1))' /etc/trellis/node.yaml" || fail "$n: node.yaml without its callsign"
  on "$n" '[ ! -e /home/op/.trellis-deploy ]' || fail "$n: the copied files were left behind"
  # Voice is in the package; its libraries (recommended) come with the install.
  if ! on "$n" 'trellis-voiced version' | grep -q 'libopus .*libasound loaded'; then
    grep -h -A12 'libraries voice needs' "$W"/logs/* | tail -30
    fail "$n: voice is not there with its libraries: $(on "$n" 'trellis-voiced version' 2>&1)"
  fi
done
on "${NAMES[0]}" "grep -q 'node_id: 11' /etc/trellis/node.yaml" || fail "id=11 not in node.yaml"
echo "$out" | grep -q 'sudo asks for a password on: alpha-2' || fail "the plan does not name the node that needs a sudo password"
echo "$out" | grep -E 'gone .*skipped: unreachable' >/dev/null || { echo "$out" | tail -15; fail "the unreachable node is not reported as skipped"; }
grep -rq 'sae_passphrase\|secret' "$W/logs" && fail "the mesh secret reached a log"
grep -rq "$PASSWORD" "$W/logs" && fail "the sudo password reached a log"
ok "installed 0.9.0 on both (profile 0600, callsigns, node ID, voice with its libraries), the unreachable node skipped, no secret in the logs"

echo "== 2. an upgrade to 0.9.1"
# meshctl upgrade takes only a package signed by a key the node trusts (as
# the bench trusts its development key): first, it is refused, and says so.
out="$(printf '%s\n' "$PASSWORD" | deploy --from "$W/rel-0.9.1" 2>&1)" && { echo "$out" | tail -15; fail "an untrusted key was taken"; }
echo "$out" | grep -q 'refused by the node: signed with key .* which this node does not trust' || { echo "$out" | tail -15; fail "the refusal does not say why"; }
ok "a package signed by a key the node does not trust is refused, and the summary says why"
for n in "${NAMES[@]}"; do
  "$RT" exec -i "$n" sh -c 'mkdir -p /etc/trellis/release-keys && cat > /etc/trellis/release-keys/deploy-test.pub' < "$W/key.pub"
done
out="$(printf '%s\n' "$PASSWORD" | deploy --from "$W/rel-0.9.1" 2>&1)" || { echo "$out" | tail -30; fail "upgrade run"; }
for n in "${NAMES[@]}"; do
  [ "$(on "$n" "dpkg-query -W -f='\${Version}' trellis")" = 0.9.1 ] || { echo "$out" | tail -30; fail "$n: not upgraded"; }
  on "$n" "grep -q '\"result\": *\"upgraded\"' /var/lib/trellis/upgrade/last.json" || fail "$n: meshctl upgrade did not record an upgrade"
done
echo "$out" | grep -c 'ok upgraded to 0.9.1' | grep -qx 2 || { echo "$out" | tail -15; fail "the summary does not show two upgrades"; }
echo "$out" | grep -q 'doctor: .* failed (' || { echo "$out" | tail -15; fail "the summary does not carry doctor's findings"; }
ok "both upgraded to 0.9.1 through meshctl upgrade; the summary carries doctor's findings"

echo "== 3. again: nothing to do"
out="$(deploy --from "$W/rel-0.9.1" < /dev/null 2>&1)" || { echo "$out" | tail -20; fail "rerun"; }
echo "$out" | grep -q 'Nothing to do' || { echo "$out" | tail -15; fail "a rerun found something to do"; }
ok "a rerun has nothing to do"

echo "== 4. a wrong sudo password leaves that node out"
out="$(printf 'wrong\nwrong\nwrong\n' | deploy --from "$W/rel-0.9.1" --force 2>&1)" || { echo "$out" | tail -30; fail "run with a wrong password"; }
echo "$out" | grep -q 'alpha-2 is left out' || { echo "$out" | tail -20; fail "alpha-2 not left out"; }
echo "$out" | grep -E 'alpha-1 .*ok reinstalled 0.9.1' >/dev/null || { echo "$out" | tail -20; fail "alpha-1 not reinstalled"; }
ok "a wrong password leaves out that node only; the other is reinstalled"

echo "== 5. the developer's way: an older build, no health check, the key sent along"
for n in "${NAMES[@]}"; do on "$n" 'rm -f /etc/trellis/release-keys/deploy-test.pub'; done
out="$(printf '%s\n' "$PASSWORD" | deploy --from "$W/rel-0.9.0" --any-version --no-health-check --node-key "$W/key.pub" 2>&1)" || { echo "$out" | tail -30; fail "fast run"; }
for n in "${NAMES[@]}"; do
  [ "$(on "$n" "dpkg-query -W -f='\${Version}' trellis")" = 0.9.0 ] || { echo "$out" | tail -30; fail "$n: 0.9.0 not installed"; }
  "$RT" exec -i "$n" sh -c 'cmp -s /etc/trellis/release-keys/deploy-dev.pub -' < "$W/key.pub" || fail "$n: the node key was not installed"
done
echo "$out" | grep -c 'ok installed (no health check) 0.9.0' | grep -qx 2 || { echo "$out" | tail -15; fail "the summary does not show two installs"; }
ok "--any-version --no-health-check --node-key: the older build installed with apt on both, the key in place"

echo "== 6. --setup: the key only; sudo without a password only when asked for"
out="$("$W/trellis-deploy" --setup --yes "${SSHOPT[@]}" "$W/hosts" < /dev/null 2>&1)" || true
echo "$out" | grep -q 'Nothing to do' || { echo "$out" | tail -20; fail "--setup with the keys in place found something to do"; }
on "${NAMES[1]}" '[ ! -e /etc/sudoers.d/trellis-deploy ]' || fail "--setup alone touched sudo"
out="$(printf '%s\n' "$PASSWORD" | "$W/trellis-deploy" --setup --sudo-nopasswd --yes "${SSHOPT[@]}" "$W/hosts" 2>&1)" || { echo "$out" | tail -20; fail "setup run"; }
[ "$(on "${NAMES[1]}" "stat -c %a /etc/sudoers.d/trellis-deploy")" = 440 ] || fail "the sudo rule is missing or not 0440"
ssh -o BatchMode=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR -o IdentitiesOnly=yes \
  -i "$W/id" -p "${PORTS[1]}" op@127.0.0.1 'sudo -n true' || fail "sudo still asks for a password"
out="$("$W/trellis-deploy" --setup --sudo-nopasswd --dry-run "${SSHOPT[@]}" "$W/hosts" 2>&1)"
echo "$out" | grep -q 'Nothing to do' || { echo "$out" | tail -10; fail "a second --setup still has something to do"; }
ok "--setup alone leaves sudo as it is; --sudo-nopasswd writes the rule (0440, checked by visudo) where sudo asked; again: nothing to do"

echo "== PASS"
