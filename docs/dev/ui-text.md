# Writing text for the web UI

Trellis is network equipment. Its UI reads like the UI of a commercial or
military IP radio (Silvus StreamCaster, Persistent Systems MPU5, L3Harris,
Doodle Labs) or a managed switch: labelled figures, standard terms, terse
alerts. Operators are trained; they know what throttling or a captive
portal is. Text tells them the state and, when something is wrong, the
action. It does not explain how things work.

These rules cover everything a user reads: the web UI, alert messages,
toasts and dialogs. `meshctl` output and the user docs use the same
register.

**The test:** would this line appear on a Silvus web UI? If it reads
like a sentence from a manual or a chat, it is wrong.

## The rules

1. **Standard terms.** Use the term the industry uses: CPU, throttle,
   undervoltage, uptime, RSSI, SNR, Tx/Rx, Tx duty, throughput, PCIe Gen
   and lanes, captive portal, standalone, synced, stratum, PPS. Never
   invent one ("slows from 80", "carried", "in force").
2. **`Label: value`.** A figure with its unit; a limit is a figure too:
   `CPU temp 49 °C · Throttle at 80 °C`, not a sentence about it.
3. **States from a fixed set:** OK, Up, Down, Active, Off, On, Synced,
   Not synced, Throttling, Degraded, Fault, Unknown, N/A. Capitalised as
   values.
4. **Alerts:** `Subject condition: figures. Action.` One clause for the
   condition, the figures in it, then the action as an imperative. For
   example: `CPU temperature high: 83 °C (throttle at 80 °C). Improve
   cooling.` An alert that needs no action ends after the figures.
5. **No explanation of standard behaviour.** No "to cool off", "slower
   when idle", "so links on it are slower". A consequence is stated only
   when it is not obvious and it matters (`Risk of card hang and node
   reset`).
6. **No conversational voice:** no idioms, contractions ("doesn't"),
   chatty second person ("you may have to"), and nothing that narrates
   the UI or the node ("this page has seen", "the node asks").
7. **Hints only where an action needs one** (a dialog, an empty state, an
   irreversible step), as a short technical line.
8. **One name per thing** (glossary). Implementation names (`meshd`,
   reconciler) appear only on diagnostic screens.
9. **A switch has the name of the setting it governs,** as the system it
   belongs to names it, in the positive sense and with that setting's
   default: `Password authentication` (On), never an opposite made up
   for it (`SSH key only`). A switch that cannot be changed yet says what
   it needs (`Off: requires a public key`).

## Examples

| Not this | This |
|---|---|
| `cpu-thermal · slows from 80 °C` | `Throttle at 80 °C` |
| `The processor slows down above 80 °C to cool off.` | `Throttle at 80 °C` |
| `1,500 of 1,500 MHz · slower when idle, or when hot or short of power` | `1,500 / 1,500 MHz` |
| Power: `Dropped lately`, `Never too low since meshd started` | Supply: `OK` · `Last undervoltage 3 min ago` / `No undervoltage events` |
| `This node's own uplink on eth0 gives the whole mesh its internet.` | `Local uplink (eth0)` · `Gateway for the mesh` |
| `…is too hot at 121 °C: it now sends only 50% of the time to cool off, so links on it are slower.` | `MT7915 5/6 GHz thermal throttling: 121 °C, Tx duty 50%. Improve card cooling.` |
| `The lines show up to the last three minutes, from when this page was opened.` | `History: 3 min` |
| Switch `SSH key only` (off) | Switch `Password authentication` (on) |

## Glossary

| Use | Not | Meaning |
|---|---|---|
| node | host, box | A computer running Trellis |
| mesh | network, cluster | All nodes sharing a mesh profile |
| link | carrier, path | A mesh band and channel, or a connection between two nodes on it |
| client | device (in text), station, EUD | Phone, radio or laptop on a node's AP or ports ("Devices" stays the page name) |
| client AP | Wi-Fi for devices | The access point a node offers clients |
| uplink | WAN (in text) | A node's own internet connection |
| gateway | exit | A node that shares its uplink with the mesh |
| mesh profile | config | The settings every node of a mesh shares |
| CPU | processor, SoC | The board's processor; "CPU temp" for its sensor |
| supply | power rail, PSU | The board's power input |
| unplanned reset | crash, went down | A restart without a shutdown |
| alert | warning (as the general word) | A fault, a warning or a note |
| operator account | admin account, user | The Linux account `trellis`: SSH, sudo, meshctl |
| password authentication | SSH key only, key login only | SSH login with the operator account's password; On by default, Off needs a public key |

Add a term here when it first enters the UI.
