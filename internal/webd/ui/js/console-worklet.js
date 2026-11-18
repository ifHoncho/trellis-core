// The voice console's audio thread (an AudioWorklet): plays the groups
// the node sends, 16 kHz each, mixed with their own gain and pan; and
// records the microphone, sent to the node as 16 kHz frames of 20 ms.

const RATE = 16000;
const FRAME = 320; // 20 ms at 16 kHz
const PREBUFFER = 4 * FRAME; // 80 ms before a group starts playing
const MAX = 25 * FRAME; // 0.5 s: more is dropped (the network caught up)

class Player extends AudioWorkletProcessor {
  constructor() {
    super();
    this.groups = new Map(); // number → {buf: Float32Array ring, r, w, n, playing, pos, gain, pan}
    this.muted = false;
    this.port.onmessage = (e) => {
      const m = e.data;
      if (m.pcm) this.push(m.group, m.pcm);
      if (m.mix) for (const [n, g] of Object.entries(m.mix)) this.group(Number(n)).gain = g.gain, this.group(Number(n)).pan = g.pan;
      if ('muted' in m) this.muted = m.muted;
    };
    // Levels back to the page, ten times a second.
    this.peaks = new Map();
    this.count = 0;
  }
  group(n) {
    let g = this.groups.get(n);
    if (!g) {
      g = { buf: new Float32Array(MAX), r: 0, w: 0, n: 0, playing: false, pos: 0, gain: 1, pan: 0 };
      this.groups.set(n, g);
    }
    return g;
  }
  push(n, pcm) {
    const g = this.group(n);
    for (let i = 0; i < pcm.length; i++) {
      if (g.n === MAX) { g.r = (g.r + 1) % MAX; g.n--; }
      g.buf[g.w] = pcm[i] / 32768;
      g.w = (g.w + 1) % MAX;
      g.n++;
    }
  }
  process(inputs, outputs) {
    const out = outputs[0];
    const L = out[0], R = out[1] || out[0];
    const step = RATE / sampleRate;
    for (const [n, g] of this.groups) {
      if (!g.playing) {
        if (g.n < PREBUFFER) continue;
        g.playing = true;
      }
      const gl = g.gain * Math.min(1, 1 - g.pan), gr = g.gain * Math.min(1, 1 + g.pan);
      let peak = this.peaks.get(n) || 0;
      for (let i = 0; i < L.length; i++) {
        if (g.n < 2) { g.playing = false; break; } // ran dry: buffer again
        const a = g.buf[g.r], b = g.buf[(g.r + 1) % MAX];
        const v = a + (b - a) * g.pos;
        if (!this.muted) { L[i] += v * gl; if (R !== L) R[i] += v * gr; }
        peak = Math.max(peak, Math.abs(v));
        g.pos += step;
        while (g.pos >= 1) { g.pos -= 1; g.r = (g.r + 1) % MAX; g.n--; }
      }
      this.peaks.set(n, peak);
    }
    if (++this.count >= sampleRate / 128 / 10) {
      this.count = 0;
      this.port.postMessage({ levels: Object.fromEntries(this.peaks) });
      this.peaks.clear();
    }
    return true;
  }
}

class Mic extends AudioWorkletProcessor {
  constructor() {
    super();
    this.on = false;
    this.frame = new Int16Array(FRAME);
    this.k = 0;
    this.pos = 0; // the next 16 kHz sample's place in the input
    this.acc = 0;
    this.accN = 0;
    this.peak = 0;
    this.count = 0;
    this.port.onmessage = (e) => { if ('on' in e.data) { this.on = e.data.on; this.k = 0; } };
  }
  process(inputs) {
    const ch = inputs[0] && inputs[0][0];
    if (!ch) return true;
    const step = sampleRate / RATE; // input samples per 16 kHz sample
    for (let i = 0; i < ch.length; i++) {
      // Average the input samples that fall into each output sample: a
      // crude low-pass ahead of the decimation, good enough for speech.
      this.acc += ch[i];
      this.accN++;
      this.peak = Math.max(this.peak, Math.abs(ch[i]));
      this.pos += 1;
      if (this.pos >= step) {
        this.pos -= step;
        const v = this.acc / this.accN;
        this.acc = 0; this.accN = 0;
        if (this.on) {
          this.frame[this.k++] = Math.max(-32768, Math.min(32767, Math.round(v * 32767)));
          if (this.k === FRAME) {
            this.port.postMessage({ frame: this.frame.buffer.slice(0) });
            this.k = 0;
          }
        }
      }
    }
    if (++this.count >= sampleRate / 128 / 10) {
      this.count = 0;
      this.port.postMessage({ level: this.peak });
      this.peak = 0;
    }
    return true;
  }
}

registerProcessor('trellis-player', Player);
registerProcessor('trellis-mic', Mic);
