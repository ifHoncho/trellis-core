# Getting started

Deploy Trellis to a set of devices on your LAN from one machine.
On a board with a Trellis disk image, install from the image instead:
[Installing from an image](reference.md#installing-from-an-image).

## What you need

- **Two or more nodes** running Debian, Ubuntu or Raspberry Pi OS, each
  with a Wi-Fi radio that supports mesh mode ([hardware.md](hardware.md)).
  On a Raspberry Pi use the 64-bit OS: the batman-adv DKMS module does not
  build on the 32-bit one. Wire the nodes to your LAN: Trellis takes over
  their Wi-Fi radios.
- **SSH access** to each node as a user with `sudo`.
- **A Linux, macOS or Windows machine** on the same LAN to run
  `trellis-deploy` from (on Windows, with its OpenSSH client, built into
  Windows 10 and 11).

## 1. Download trellis-deploy

From the newest [release](https://github.com/ifHoncho/trellis-core/releases),
download the build for your machine (`linux_amd64`, `linux_arm64`,
`darwin_arm64` for Apple silicon, `darwin_amd64` for Intel Macs,
`windows_amd64.exe`, `windows_arm64.exe`):

```sh
mv trellis-deploy_* trellis-deploy && chmod +x trellis-deploy
xattr -d com.apple.quarantine trellis-deploy   # macOS, if downloaded with a browser
```

On Windows, rename it `trellis-deploy.exe` and run the steps below in
PowerShell or a command prompt. Runs are slower there: Windows' OpenSSH
cannot share one connection between commands.

## 2. List the nodes

`nodes.txt`, one node per line as `user@address`:

```
pi@192.0.2.11  callsign=alpha-1
pi@192.0.2.12  callsign=alpha-2
pi@192.0.2.13
```

`callsign` is optional (the default is the hostname). Other per-node
settings (node ID, SSH port, a mesh address for nodes off the LAN) are in
the [reference](reference.md#the-devices-file).

## 3. Add your SSH key to the nodes

```sh
./trellis-deploy --setup nodes.txt
```

Puts your SSH key on each node it does not log in to yet (with
`ssh-copy-id` where there is one; each asks for that node's password). No key yet: `ssh-keygen -t ed25519`.
`sudo` is left as it is: deployments ask for the sudo password where a
node needs one. To make it password-free instead, see
[Passwords and sudo](reference.md#passwords-and-sudo) first.

## 4. Create a mesh profile

```sh
./trellis-deploy --new-mesh mesh-alpha --country US
```

Writes `mesh.yaml` (mode 0600): the mesh's name, a generated secret, and
your country's regulatory rules, on 2.4 GHz (`--band 5g` for 5 GHz). It
holds the mesh secret: keep it private, and keep it for adding nodes
later. More links and bands can be added from the web UI afterwards.

## 5. Deploy

```sh
./trellis-deploy --dry-run --profile mesh.yaml nodes.txt   # optional dry-run
./trellis-deploy --profile mesh.yaml nodes.txt
```

It verifies the release (checksums and package signatures), inspects each
node, shows the plan and asks to proceed, then asks for the sudo password
(if any node needs one) and a web UI admin password for the new nodes.
Nodes are installed a few at a time, rebooted, and waited for. The
summary lists each node's result, any `meshctl doctor` findings, and each
new node's client AP SSID and passphrase.

## 6. Join the mesh

Connect a phone or laptop to any node's client AP (SSID and passphrase
from the summary), open **https://10.73.0.1**, accept the node's self-signed
certificate, and sign in with the admin password. The **Mesh** page shows
every node and link.

Clients on any node reach each other by address or as `callsign.mesh`,
and use whichever node has an internet uplink. For ATAK, see
[atak.md](atak.md).

## Next

- **Add nodes:** add them to `nodes.txt` and run step 5 again; nodes
  already on the release are left alone.
- **Upgrade:** run step 5 again when a new release is out. Nodes are
  upgraded one at a time; each checks itself and rolls back on its own if
  it is not healthy, and the first rollback stops the rest. From one node
  instead: Settings → System → **Mesh upgrade** sends a release through
  the mesh the same way ([reference.md](reference.md#upgrading-the-whole-mesh-from-one-node)).
- **Change the mesh** (channels, bands, links): web UI, Settings → Mesh
  profile → **All nodes**. Make changes while every node is up.
- **Firewall:** from your LAN, new nodes answer only SSH and ping (the
  mesh side is not filtered); Settings → Access opens more.
- **Link sites:** parts of the mesh in different places join over the
  internet: Internet → Site link ([reference.md](reference.md#linking-sites-over-the-internet)).

## Troubleshooting

| Message | Fix |
|---|---|
| `Permission denied` | The key is not on that node: rerun step 3. |
| `unreachable`, timeout | Check the address; `ssh user@address` must work. |
| `the sudo password did not work` | Wrong password, or the user is not in `sudo`. |
| `no ... package in this release` | Unsupported architecture: arm64, armhf and amd64 are built. |
| `doctor:` in the summary | Run `sudo meshctl doctor` on that node for the details and fixes. |
| anything else | Per-node logs are in `trellis-deploy-logs/`. |

Everything else (all options, single-node installs, phone setup, upgrades,
radios, the web UI) is in the [reference](reference.md).
