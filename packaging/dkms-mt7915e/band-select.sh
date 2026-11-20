#!/bin/sh
# Give an mt7915e older than Linux 6.14 that release's band selection.
#
#   band-select.sh path/to/mt76/mt7915/eeprom.c
#
# An MT7916 whose EEPROM says "5 GHz and 6 GHz" has one radio for both
# bands, and can run one of them at a time. Before 6.14 the driver
# registers both at once: the radio then looks normal, lists 5 GHz and
# 6 GHz channels, and hears and sends nothing on either. 6.14 picks one,
# 5 GHz unless the module option enable_6ghz is set, and writes the choice
# into the EEPROM data the firmware is given. This script makes the same
# change to older source, with the same option name, so that
# "options mt7915e enable_6ghz=1" means one thing on every kernel.
#
# It changes the file only when it finds exactly the code 6.14 replaced:
#   - source that has enable_6ghz already is left alone (exit 0);
#   - source without the 5/6 GHz case (before the MT7916) is left alone
#     (exit 0);
#   - source where that case is not the two lines it was from 5.19 to 6.13
#     is refused (exit 1, file unchanged): someone changed it, and a patch
#     written for other code must not be forced onto it.
set -eu

f="${1:?usage: band-select.sh path/to/mt7915/eeprom.c}"
[ -f "$f" ] || { echo "band-select: $f not found" >&2; exit 1; }

if grep -q 'enable_6ghz' "$f"; then
  echo "band-select: $f selects the band already"
  exit 0
fi
if ! grep -q 'case MT_EE_V2_BAND_SEL_5GHZ_6GHZ:' "$f"; then
  echo "band-select: $f has no 5/6 GHz radios to select a band for"
  exit 0
fi
# The driver's own name for "the EEPROM is handed to the firmware whole".
if ! grep -q 'flash_mode' "$(dirname "$f")/mt7915.h" 2>/dev/null && ! grep -q 'flash_mode' "$f"; then
  echo "band-select: this driver has no flash_mode; not patched" >&2
  exit 1
fi

tmp="$f.band-select.$$"
trap 'rm -f "$tmp"' EXIT
awk '
function fail(msg) { print "band-select: " msg > "/dev/stderr"; bad = 1; exit 1 }
{
  line[NR] = $0
}
END {
  if (bad) exit 1
  # The index of this radio in the EEPROM: "phy->band_idx" up to 6.1,
  # "band" after.
  idx = ""
  for (i = 1; i <= NR; i++) {
    if (match(line[i], /val = eeprom\[MT_EE_WIFI_CONF \+ [A-Za-z_>-]+\];/)) {
      s = substr(line[i], RSTART, RLENGTH)
      sub(/^val = eeprom\[MT_EE_WIFI_CONF \+ /, "", s)
      sub(/\];$/, "", s)
      idx = s
      break
    }
  }
  if (idx == "") fail("cannot find how the band is read from the EEPROM")
  inc = 0; par = 0; done = 0
  for (i = 1; i <= NR; i++) {
    l = line[i]
    if (!inc && l == "#include <linux/firmware.h>") {
      print l
      print "#include <linux/moduleparam.h>"
      inc = 1
      continue
    }
    if (!par && l == "#include \"eeprom.h\"") {
      print l
      print ""
      print "static bool enable_6ghz;"
      print "module_param(enable_6ghz, bool, 0644);"
      print "MODULE_PARM_DESC(enable_6ghz, \"Enable 6 GHz instead of 5 GHz on hardware that supports both\");"
      par = 1
      continue
    }
    if (l ~ /^[ \t]*case MT_EE_V2_BAND_SEL_5GHZ_6GHZ:$/) {
      if (done) fail("the 5/6 GHz case appears twice")
      a = line[i + 1]; b = line[i + 2]; c = line[i + 3]
      gsub(/^[ \t]+/, "", a); gsub(/^[ \t]+/, "", b); gsub(/^[ \t]+/, "", c)
      if (a != "phy->mt76->cap.has_5ghz = true;" || b != "phy->mt76->cap.has_6ghz = true;" || c != "return;")
        fail("the 5/6 GHz case is not the code this patch was written for")
      ind = l
      sub(/case MT_EE_V2_BAND_SEL_5GHZ_6GHZ:$/, "", ind)
      t = ind "\t"
      print l
      print t "if (enable_6ghz) {"
      print t "\tphy->mt76->cap.has_6ghz = true;"
      print t "\tu8p_replace_bits(&eeprom[MT_EE_WIFI_CONF + " idx "],"
      print t "\t\t\t MT_EE_V2_BAND_SEL_6GHZ,"
      print t "\t\t\t MT_EE_WIFI_CONF0_BAND_SEL);"
      print t "} else {"
      print t "\tphy->mt76->cap.has_5ghz = true;"
      print t "\tu8p_replace_bits(&eeprom[MT_EE_WIFI_CONF + " idx "],"
      print t "\t\t\t MT_EE_V2_BAND_SEL_5GHZ,"
      print t "\t\t\t MT_EE_WIFI_CONF0_BAND_SEL);"
      print t "}"
      print t "/* force to buffer mode */"
      print t "dev->flash_mode = true;"
      print ""
      print t "return;"
      i += 3
      done = 1
      continue
    }
    print l
  }
  if (!inc || !par) fail("cannot find where the module option goes")
  if (!done) fail("the 5/6 GHz case was not found")
}
' "$f" > "$tmp" || { echo "band-select: $f left as it was" >&2; exit 1; }
cat "$tmp" > "$f"
echo "band-select: $f now selects 5 GHz or 6 GHz (module option enable_6ghz)"
