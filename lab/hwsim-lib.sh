# shellcheck shell=bash
# Shared by the root-only hwsim labs (hwsim.sh, hwsim-dual.sh,
# hwsim-roles.sh). Source it after setting RUN; pass the lab's namespace
# names to hwsim_preflight and hwsim_cleanup.
#
# These labs run as root on the developer's own machine and load and unload
# mac80211_hwsim. Unloading it while meshd still holds interfaces on its
# radios can wedge the kernel's network configuration lock: a
# `modprobe -r mac80211_hwsim` under three live meshd processes once never
# finished, and NetworkManager, systemd-networkd and systemd-resolved hung
# until the machine was powered off. So:
#   - background processes start with `ip netns exec` directly, so that $! is
#     the process itself and not a subshell that a kill would miss;
#   - cleanup stops meshd and waits for it, kills anything left in the
#     namespaces, and unloads the module only when nothing is left;
#   - a run refuses to start while an earlier run's processes or namespaces
#     are still there.

# bg_in NS CMD...: start CMD in namespace NS in the background; $! is CMD.
bg_in() { ip netns exec "$@" & }

# stop_pid PID [SECONDS]: SIGTERM, wait for the exit (meshd tears down its
# interfaces first), then SIGKILL.
stop_pid() {
  local pid="$1" secs="${2:-30}"
  [ -n "$pid" ] || return 0
  kill "$pid" 2>/dev/null || return 0
  for _ in $(seq 1 $((secs * 10))); do kill -0 "$pid" 2>/dev/null || return 0; sleep 0.1; done
  echo "== note: pid $pid did not exit within ${secs}s; killing it" >&2
  kill -9 "$pid" 2>/dev/null
  for _ in $(seq 1 50); do kill -0 "$pid" 2>/dev/null || return 0; sleep 0.1; done
}

# ns_busy NS...: true if any process is still running in one of them.
ns_busy() {
  local ns
  for ns in "$@"; do [ -n "$(ip netns pids "$ns" 2>/dev/null)" ] && return 0; done
  return 1
}

# hwsim_preflight NS...: refuse to run on a real machine, or over the
# leftovers of an earlier run.
#
# These labs have crashed a developer's desktop (Fedora, kernel 7.2.15): deleting an hwsim radio that still had interfaces left an
# orphaned wireless device (WARNING in wiphy_unregister, net/wireless/core.c),
# and tearing down its namespace later hit a NULL pointer in
# nl80211_send_iface (cleanup_net) while holding the network locks. The
# whole host's networking froze. So they run only inside a virtual machine
# unless TRELLIS_HWSIM_ON_HOST=1 says otherwise.
hwsim_preflight() {
  local ns left="" procs
  if command -v systemd-detect-virt >/dev/null 2>&1 && ! systemd-detect-virt -q; then
    if [ "${TRELLIS_HWSIM_ON_HOST:-}" != 1 ]; then
      echo "!! not running: this is a real machine, not a VM. These labs load and unload kernel" >&2
      echo "   modules as root, and on kernel 7.2.15 they crashed the host's networking (see lab/hwsim-lib.sh)." >&2
      echo "   Run them in a virtual machine, or set TRELLIS_HWSIM_ON_HOST=1 to accept the risk." >&2
      exit 2
    fi
    echo "== warning: running on the host (TRELLIS_HWSIM_ON_HOST=1)" >&2
  fi
  for ns in "$@"; do ip netns list 2>/dev/null | grep -qw "^$ns" && left="$left $ns"; done
  procs=$(pgrep -af "meshd --node $(dirname "$RUN")" 2>/dev/null)
  if [ -n "$left" ] || [ -n "$procs" ]; then
    echo "!! an earlier lab run is still around; not touching mac80211_hwsim." >&2
    [ -n "$left" ] && echo "   namespaces:$left" >&2
    [ -n "$procs" ] && echo "   processes:" >&2 && echo "$procs" | sed 's/^/     /' >&2
    echo "   Stop them first: for n in$left; do sudo ip netns pids \$n | xargs -r sudo kill; sudo ip netns del \$n; done" >&2
    echo "   (a reboot also clears them)" >&2
    exit 2
  fi
}

# hwsim_cleanup NS...: stop meshd (pid files under RUN), clear the
# namespaces, then unload mac80211_hwsim if nothing is left.
hwsim_cleanup() {
  local f ns
  for f in "$RUN"/pid "$RUN"/*/pid; do
    [ -f "$f" ] || continue
    stop_pid "$(cat "$f")"
    rm -f "$f"
  done
  for ns in "$@"; do
    ip netns pids "$ns" 2>/dev/null | xargs -r kill 2>/dev/null
  done
  for _ in $(seq 1 50); do ns_busy "$@" || break; sleep 0.1; done
  for ns in "$@"; do
    ip netns pids "$ns" 2>/dev/null | xargs -r kill -9 2>/dev/null
  done
  for _ in $(seq 1 50); do ns_busy "$@" || break; sleep 0.1; done
  if ns_busy "$@"; then
    echo "!! processes are still running in the lab namespaces; leaving them and mac80211_hwsim alone" >&2
    echo "   (unloading the module under them can hang the host's networking). A reboot clears them." >&2
  else
    for ns in "$@"; do ip netns del "$ns" 2>/dev/null; done
    sleep 2   # namespace teardown finishes asynchronously
    modprobe -r mac80211_hwsim 2>/dev/null || echo "== note: mac80211_hwsim is still in use; left loaded" >&2
  fi
  # Hand the run folder back to whoever ran sudo, so the logs can be read
  # and the unprivileged lab (lab.sh) can still write lab/run.
  [ -n "${SUDO_UID:-}" ] && chown -R "$SUDO_UID:${SUDO_GID:-$SUDO_UID}" "$RUN" 2>/dev/null
  return 0
}
