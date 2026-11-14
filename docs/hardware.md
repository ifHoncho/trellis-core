# Supported hardware

A Trellis node is a small Linux computer with one or more Wi-Fi radios, and
optionally a Wi-Fi HaLow radio for long range. Trellis finds the radios
itself, works out what each can do, and gives each a job: carrying a mesh
link, or serving the node's own Wi-Fi for the user's phone.

## What a node needs

**A board** running Debian, Ubuntu or Raspberry Pi OS (arm64, armv7 or
x86-64) whose kernel has:

- version 5.15 or newer;
- batman-adv as a module or not at all (Trellis installs the version it
  needs), never built in;
- the bridge with IGMP snooping (`CONFIG_BRIDGE_IGMP_SNOOPING=y`);
- 802.11s mesh support (`CONFIG_MAC80211_MESH=y`);
- nftables with the bridge family;
- headers or a build tree for the running kernel.

`sudo meshctl doctor` checks all of this on a running system and names
anything missing. A board that fails a check needs a different kernel.

**At least one radio that supports 802.11s mesh point** (`iw list` lists
"mesh point" under supported interface modes). That radio carries the mesh.

**Optionally, a GNSS receiver** supported by gpsd (on a serial port, USB or
I²C). The node then keeps the whole mesh on GNSS time without internet; a
receiver whose pulse per second (PPS) reaches a GPIO gives sub-millisecond
time, one without it about a tenth of a second. Keep a receiver off the
serial port the kernel uses as its console: `meshctl doctor` checks.

**Optionally, more radios.** A second Wi-Fi radio serves the client Wi-Fi
on its own channel, so the phone's traffic doesn't share the mesh's
airtime; otherwise the client Wi-Fi shares a mesh radio. Further radios
carry further mesh links on other bands, all used at once: the mesh picks
the fastest path to each node over any of them.

## How Trellis uses the radios

- **One link per radio.** A mesh profile can have several links (for
  example 2.4 GHz, 5 GHz and HaLow). Each runs on its own radio. A
  dual-band USB adapter is one radio: it carries one link, on one band at a
  time. A card with two radios (one per band) carries two links at once.
- **Ranking.** The radio with the most transmit power and the most proven
  mesh support carries the first link; the weakest serves the client Wi-Fi,
  which only has to reach a phone nearby. You can pin any role to a radio
  on the Radios page.
- **Stable roles.** Roles stay put while the node runs, so plugging in a
  radio never drops a link. Radios are recognised by a fingerprint (their
  permanent address, or the port they sit in), never by the name the kernel
  gives them.
- **Driver quirks are handled for you.** Trellis carries a database of
  adapters: known limits (largest frame, driver options needed for an
  encrypted mesh, real transmit power) are applied automatically.

## Radios

"Verified" means tested on Trellis nodes; "reported" means known from public
reports and not yet tested here.

| Chipset | Driver | Bus | Bands | Mesh | Client Wi-Fi | Notes |
|---|---|---|---|---|---|---|
| MediaTek MT7915 / MT7916 (Wi-Fi 6/6E) | mt7915e | PCIe (M.2) | 2.4 + 5/6 GHz, two radios | Verified | Verified | Carries two mesh links at once (one per band), up to 160 MHz. Needs MediaTek firmware; `scripts/install-mt7915e-dkms.sh` builds the driver where the kernel lacks it |
| MediaTek MT7610U | mt76x0u | USB 2 | 2.4 or 5 GHz | Verified | Verified | Single stream; good all-round adapter |
| MediaTek MT7612U | mt76x2u | USB 3 | 2.4 or 5 GHz | Reported | Reported | |
| MediaTek MT7921AU | mt7921u | USB 3 | 2.4 or 5/6 GHz | Unchecked | Unchecked | Kernel 5.18 or newer |
| Atheros AR9271 | ath9k_htc | USB 2 | 2.4 GHz | Verified | Verified | Trellis applies the driver options and frame-size limit it needs |
| Ralink RT5372 | rt2800usb | USB 2 | 2.4 GHz | Verified | Verified | |
| Ralink RT5572 | rt2800usb | USB 2 | 2.4 or 5 GHz | Reported | Reported | |
| Morse Micro MM6108 (Wi-Fi HaLow) | Morse Micro (out of tree) | SPI / SDIO / USB | 902–928 MHz (and other regional plans) | Verified (starts) | No | Needs Morse Micro's driver, firmware and `wpa_supplicant_s1g`; `scripts/install-halow-tools.sh` builds the latter |
| Intel AX200 / AX210 | iwlwifi | PCIe | 2.4/5/6 GHz | No | 2.4 GHz only | No mesh point in the firmware |
| Broadcom (Raspberry Pi onboard) | brcmfmac | SDIO | 2.4 (+5) GHz | No | Yes | No mesh point in the firmware; left alone while the system uses it |
| Realtek, out-of-tree drivers | 88XXau and others | USB | — | No | — | No working mesh point |

Results for adapters not listed here are welcome: see
[CONTRIBUTING.md](../CONTRIBUTING.md). The procedure below gives a result
that can go straight into the database.

## Testing a radio adapter

Once per adapter model, on any supported board:

1. **Identify:** `lsusb` or `lspci -nn` for the device ID; the driver from
   `readlink /sys/class/ieee80211/phy*/device/driver`.
2. **Capabilities:** `iw list`: interface modes (`mesh point` for a mesh
   link, `AP` for the client Wi-Fi), interface combinations (both at once
   on one radio?), bands, and per-channel maximum power.
3. **Mesh:** join a running mesh; check that a node with a wrong mesh
   secret is refused.
4. **Signal:** at a fixed distance, compare the signal a neighbour reports
   for this adapter with another adapter's. This catches drivers that
   report a power they don't deliver.
5. **Throughput:** iperf3 across one mesh hop, both directions.
6. **Identity:** is the permanent MAC address real and unique, and does it
   survive reboots and re-plugging?
7. **Quirks:** anything odd under load: hangs, firmware errors, channels it
   refuses.

## Regulatory rules

Each mesh profile chooses its rules: **country** mode applies one country's
licence-exempt channels and power; **unrestricted** mode lets every radio
use all the channels, widths and power its hardware has, for operators who
hold their own frequency authorization. See
[architecture.md §5.1](architecture.md#51-regulatory-mode-and-transmit-power).
