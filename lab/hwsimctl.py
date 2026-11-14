#!/usr/bin/env python3
"""Create and delete mac80211_hwsim radios on demand (root).

  hwsimctl.py new NAME --addr 00:c0:ca:7e:00:01 [--no-ap]
  hwsimctl.py del NAME

The radio appears in the network namespace of the caller, so run it under
`ip netns exec NS`. Radios in different namespaces don't hear each other
(hwsim gives each namespace its own medium); the role tests need only one
node. NAME becomes the wiphy name. --addr sets the permanent MAC; a
globally administered one gives the radio a `mac:` fingerprint that doesn't
depend on creation order. --no-ap leaves the AP interface type out, giving a
radio that can mesh but can't run an AP.

Speaks generic netlink directly (no pyroute2). Command and attribute numbers
are from drivers/net/wireless/virtual/mac80211_hwsim.h; the enums only ever
grow at the end, so they hold across kernels.
"""
import argparse
import os
import socket
import struct
import sys

NETLINK_GENERIC = 16
GENL_ID_CTRL = 0x10
CTRL_CMD_GETFAMILY = 3
CTRL_ATTR_FAMILY_ID = 1
CTRL_ATTR_FAMILY_NAME = 2
NLM_F_REQUEST = 0x1
NLM_F_ACK = 0x4
NLMSG_ERROR = 0x2

HWSIM_FAMILY = "MAC80211_HWSIM"
HWSIM_CMD_NEW_RADIO = 4
HWSIM_CMD_DEL_RADIO = 5
HWSIM_ATTR_RADIO_NAME = 17
HWSIM_ATTR_PERM_ADDR = 22
HWSIM_ATTR_IFTYPE_SUPPORT = 23

# enum nl80211_iftype
IFTYPE = {"adhoc": 1, "station": 2, "ap": 3, "mesh": 7, "p2p_client": 8, "p2p_go": 9, "ocb": 11}


def nla(typ, data):
    length = 4 + len(data)
    return struct.pack("=HH", length, typ) + data + b"\0" * ((4 - length % 4) % 4)


def attrs(buf):
    out, off = {}, 0
    while off + 4 <= len(buf):
        length, typ = struct.unpack_from("=HH", buf, off)
        if length < 4:
            break
        out[typ & 0x3FFF] = buf[off + 4:off + length]
        off += (length + 3) & ~3
    return out


class GenlSocket:
    def __init__(self):
        self.sock = socket.socket(socket.AF_NETLINK, socket.SOCK_RAW, NETLINK_GENERIC)
        self.sock.bind((0, 0))
        self.seq = 0

    def call(self, family, cmd, payload_attrs, version=1, ack=True):
        """Send one request; return (ack value, reply attributes)."""
        self.seq += 1
        payload = struct.pack("=BBH", cmd, version, 0) + b"".join(payload_attrs)
        flags = NLM_F_REQUEST | (NLM_F_ACK if ack else 0)
        self.sock.send(struct.pack("=IHHII", 16 + len(payload), family, flags, self.seq, 0) + payload)
        reply = {}
        while True:
            data = self.sock.recv(65536)
            off = 0
            while off + 16 <= len(data):
                length, typ, _flags, seq, _pid = struct.unpack_from("=IHHII", data, off)
                body = data[off + 16:off + length]
                off += (length + 3) & ~3
                if seq != self.seq:
                    continue
                if typ == NLMSG_ERROR:
                    (err,) = struct.unpack_from("=i", body, 0)
                    if err < 0:
                        raise OSError(-err, os.strerror(-err))
                    return err, reply
                reply = attrs(body[4:])  # skip the genl header
                if not ack:
                    return 0, reply

    def family_id(self, name):
        missing = OSError(2, f"generic netlink family {name} not found: is mac80211_hwsim loaded?")
        try:
            _, reply = self.call(GENL_ID_CTRL, CTRL_CMD_GETFAMILY,
                                 [nla(CTRL_ATTR_FAMILY_NAME, name.encode() + b"\0")], ack=False)
        except FileNotFoundError:
            raise missing from None
        if CTRL_ATTR_FAMILY_ID not in reply:
            raise missing
        return struct.unpack("=H", reply[CTRL_ATTR_FAMILY_ID][:2])[0]


def mac_bytes(text):
    parts = text.split(":")
    if len(parts) != 6:
        raise ValueError(f"bad MAC {text!r}")
    return bytes(int(p, 16) for p in parts)


def main():
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    sub = ap.add_subparsers(dest="cmd", required=True)
    new = sub.add_parser("new")
    new.add_argument("name")
    new.add_argument("--addr", required=True)
    new.add_argument("--no-ap", action="store_true")
    dele = sub.add_parser("del")
    dele.add_argument("name")
    args = ap.parse_args()

    g = GenlSocket()
    fam = g.family_id(HWSIM_FAMILY)
    name = nla(HWSIM_ATTR_RADIO_NAME, args.name.encode() + b"\0")
    if args.cmd == "new":
        req = [name, nla(HWSIM_ATTR_PERM_ADDR, mac_bytes(args.addr))]
        if args.no_ap:
            types = sum(1 << v for k, v in IFTYPE.items() if k not in ("ap", "p2p_go"))
            req.append(nla(HWSIM_ATTR_IFTYPE_SUPPORT, struct.pack("=I", types)))
        idx, _ = g.call(fam, HWSIM_CMD_NEW_RADIO, req)
        print(f"created {args.name} (hwsim radio {idx}, {args.addr}{', no AP' if args.no_ap else ''})")
    else:
        g.call(fam, HWSIM_CMD_DEL_RADIO, [name])
        print(f"deleted {args.name}")


if __name__ == "__main__":
    try:
        main()
    except OSError as e:
        print(f"hwsimctl: {e}", file=sys.stderr)
        sys.exit(1)
