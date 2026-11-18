#!/usr/bin/env python3
"""Analyse a voice lab recording (stereo, 16-bit, 48 kHz WAV).

    voicecheck.py rec.wav [hz ...]

Prints one line per ear: frames (20 ms) with sound, RMS while sounding,
and the share of each frequency's power (Goertzel) among those asked for.
Standard library only.
"""
import math
import struct
import sys
import wave


def goertzel(xs, hz, rate):
    k = 2 * math.cos(2 * math.pi * hz / rate)
    s1 = s2 = 0.0
    for x in xs:
        s1, s2 = x + k * s1 - s2, s1
    return s1 * s1 + s2 * s2 - k * s1 * s2


def main():
    path, freqs = sys.argv[1], [float(f) for f in sys.argv[2:]] or [440.0, 1000.0]
    w = wave.open(path)
    rate, n = w.getframerate(), w.getnframes()
    data = struct.unpack("<%dh" % (n * 2), w.readframes(n))
    frame = rate // 50
    for ear, name in ((0, "left"), (1, "right")):
        xs = data[ear::2]
        loud, sq, power = 0, 0.0, {f: 0.0 for f in freqs}
        for i in range(0, len(xs) - frame + 1, frame):
            f = xs[i:i + frame]
            rms = math.sqrt(sum(v * v for v in f) / frame)
            if rms > 300:
                loud += 1
                sq += rms * rms
                for hz in freqs:
                    power[hz] += goertzel(f, hz, rate)
        total = sum(power.values()) or 1.0
        shares = " ".join("%g=%.2f" % (hz, power[hz] / total) for hz in freqs)
        print("%s frames=%d rms=%.0f %s" % (name, loud, math.sqrt(sq / loud) if loud else 0, shares))


main()
