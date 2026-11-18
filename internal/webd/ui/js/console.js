// The voice console: this browser monitors talk groups through the node
// and keys one. The node sends each monitored group as 16 kHz audio and
// takes 16 kHz from the microphone (it does the Opus work), so any
// current browser can do it. Audio runs in console-worklet.js.
import { apiBase } from './api.js';
import { state, redraw } from './state.js';

const C = () => (state.vconsole ||= {
  status: 'off', error: '', name: localStorage.getItem('trellis.console.name') || 'console',
  groups: [], monitor: new Set(JSON.parse(localStorage.getItem('trellis.console.monitor') || '[]')),
  mix: JSON.parse(localStorage.getItem('trellis.console.mix') || '{}'), tx: Number(localStorage.getItem('trellis.console.tx')) || 0,
  ptt: 'idle', levels: {}, mic: 0, live: null, txSince: 0, log: [],
});
export const consoleState = C;

let ws = null, ctx = null, player = null, micNode = null, micStream = null, retry = null, wanted = false;
// held: a talk button is down now. A press is only sent while it is: the
// browser's microphone prompt can come between the press and the key.
let held = false;
// fails counts connections in a row that closed before the node's hello.
let fails = 0;

function save() {
  const c = C();
  localStorage.setItem('trellis.console.name', c.name);
  localStorage.setItem('trellis.console.monitor', JSON.stringify([...c.monitor]));
  localStorage.setItem('trellis.console.mix', JSON.stringify(c.mix));
  localStorage.setItem('trellis.console.tx', String(c.tx || 0));
}

export const mixOf = (n) => (C().mix[n] ||= { gain: 0.8, pan: 0, muted: false });

function pushMix() {
  if (!player) return;
  const c = C(), mix = {};
  for (const g of c.groups) { const m = mixOf(g.number); mix[g.number] = { gain: m.muted ? 0 : m.gain, pan: m.pan }; }
  player.port.postMessage({ mix });
}

function send(obj) { if (ws && ws.readyState === 1) ws.send(JSON.stringify(obj)); }

// beep plays local tones: grant (two high), release (one low), busy
// (three low).
function beep(kind) {
  if (!ctx) return;
  const seq = { grant: [[1200, 0.04], [0, 0.02], [1200, 0.04]], release: [[700, 0.06]], busy: [[425, 0.12], [0, 0.08], [425, 0.12], [0, 0.08], [425, 0.12]] }[kind] || [];
  let t = ctx.currentTime;
  for (const [hz, d] of seq) {
    if (hz) {
      const o = ctx.createOscillator(), g = ctx.createGain();
      o.frequency.value = hz; g.gain.value = 0.15;
      o.connect(g).connect(ctx.destination);
      o.start(t); o.stop(t + d);
    }
    t += d;
  }
}

export async function start() {
  const c = C();
  wanted = true; fails = 0;
  c.status = 'connecting'; c.error = ''; redraw();
  try {
    if (!ctx) {
      ctx = new AudioContext({ latencyHint: 'interactive' });
      await ctx.audioWorklet.addModule('/js/console-worklet.js');
      player = new AudioWorkletNode(ctx, 'trellis-player', { numberOfInputs: 0, outputChannelCount: [2] });
      player.connect(ctx.destination);
      player.port.onmessage = (e) => { if (e.data.levels) { C().levels = e.data.levels; } };
    }
    await ctx.resume();
  } catch (e) {
    c.status = 'error'; c.error = 'This browser cannot play the console’s audio: ' + e.message; redraw();
    return;
  }
  connect();
}

function connect() {
  const c = C();
  clearTimeout(retry);
  const url = (location.protocol === 'https:' ? 'wss://' : 'ws://') + location.host + apiBase + '/voice/console';
  ws = new WebSocket(url);
  ws.binaryType = 'arraybuffer';
  ws.onopen = () => send({ t: 'hello', name: c.name });
  ws.onmessage = (e) => {
    if (typeof e.data !== 'string') {
      const b = e.data;
      if (b.byteLength < 3 || !player) return;
      const group = new Uint8Array(b, 0, 1)[0];
      const pcm = new Int16Array(b.slice(1));
      player.port.postMessage({ group, pcm }, [pcm.buffer]);
      return;
    }
    let m;
    try { m = JSON.parse(e.data); } catch (err) { return; }
    onMessage(m);
  };
  ws.onclose = () => {
    held = false;
    if (c.ptt === 'talking') stopTalking(false);
    c.ptt = 'idle'; c.allCall = false;
    if (c.status !== 'on') fails++;
    if (wanted) {
      // Slower each time it fails before the node's hello, and said: the
      // session may have ended, or voice stopped.
      c.status = 'connecting';
      if (fails >= 3) c.error = 'The console cannot connect to this node. If you were signed out, sign in again; otherwise voice may not be running. Still trying.';
      retry = setTimeout(connect, Math.min(30000, 2000 * 2 ** Math.min(fails, 4)));
    } else c.status = 'off';
    redraw();
  };
}

function onMessage(m) {
  const c = C();
  switch (m.t) {
    case 'hello':
      fails = 0; c.error = '';
      c.status = 'on'; c.consoleName = m.console; c.groups = m.groups || [];
      // Monitor what was monitored before, or everything the first time.
      if (!c.monitor.size) for (const g of c.groups) c.monitor.add(g.number);
      for (const n of [...c.monitor]) if (!c.groups.some((g) => g.number === n)) c.monitor.delete(n);
      if (!c.groups.some((g) => g.number === c.tx)) c.tx = (c.groups[0] || {}).number || 0;
      send({ t: 'monitor', groups: [...c.monitor] });
      if (c.tx) send({ t: 'tx', group: c.tx });
      pushMix();
      break;
    case 'state':
      c.live = m;
      break;
    case 'ptt':
      if (m.all && m.down && c.ptt === 'asking') {
        if (m.granted) { c.ptt = 'talking'; c.txSince = Date.now(); beep('grant'); micOn(true); }
        else { c.ptt = 'refused'; c.allCall = false; beep('busy'); }
        break;
      }
      if (m.down && c.ptt === 'asking') {
        if (m.granted) { c.ptt = 'talking'; c.txSince = Date.now(); beep('grant'); micOn(true); }
        else { c.ptt = 'refused'; beep('busy'); }
      }
      break;
    case 'notice':
      if (m.what === 'busy') { c.ptt = 'refused'; beep('busy'); }
      if (m.what === 'tx-limit') { stopTalking(false); c.ptt = 'refused'; c.error = 'Released after 60 s: press again to go on.'; beep('busy'); }
      break;
    case 'error':
      // Refused before the hello (voice not running, too many consoles):
      // trying again changes nothing until the operator does something.
      c.error = m.error;
      if (c.status !== 'on') wanted = false;
      break;
  }
  redraw();
}

export function stop() {
  wanted = false; held = false;
  clearTimeout(retry);
  if (C().ptt === 'talking') stopTalking(true);
  if (ws) ws.close();
  ws = null;
  // The microphone goes back to the browser: no "in use" mark is left.
  if (micStream) for (const t of micStream.getTracks()) t.stop();
  if (micNode) micNode.disconnect();
  micStream = null; micNode = null;
  C().status = 'off'; C().mic = 0;
  redraw();
}

export function setMonitor(n, on) {
  const c = C();
  if (on) c.monitor.add(n); else c.monitor.delete(n);
  save();
  send({ t: 'monitor', groups: [...c.monitor] });
  redraw();
}

export function setMix(n, patch) {
  Object.assign(mixOf(n), patch);
  save(); pushMix(); redraw();
}

export function setName(name) { C().name = name; save(); }
// setTx chooses the group the talk button keys. The node is told, so it
// listens to that group and can refuse a press while someone talks on it.
// (Not while a button is down: the press itself names its group.)
export function setTx(n) { const c = C(); c.tx = n; save(); if (c.ptt === 'idle') send({ t: 'tx', group: n }); redraw(); }

function micOn(on) {
  if (micNode) micNode.port.postMessage({ on });
  if (player) player.port.postMessage({ muted: on }); // no feedback through the speakers
}

async function ensureMic() {
  if (micNode) return true;
  try {
    micStream = await navigator.mediaDevices.getUserMedia({ audio: { echoCancellation: true, noiseSuppression: true, autoGainControl: true, channelCount: 1 } });
  } catch (e) {
    C().error = 'No microphone: ' + (e.name === 'NotAllowedError' ? 'the browser was not allowed to use it.' : e.message);
    redraw();
    return false;
  }
  const src = ctx.createMediaStreamSource(micStream);
  micNode = new AudioWorkletNode(ctx, 'trellis-mic', { numberOfOutputs: 1 });
  micNode.port.onmessage = (e) => {
    if (e.data.frame && ws && ws.readyState === 1 && C().ptt === 'talking') ws.send(e.data.frame);
    if ('level' in e.data) C().mic = e.data.level;
  };
  const sink = ctx.createGain();
  sink.gain.value = 0; // keeps the worklet running; nothing is heard
  src.connect(micNode).connect(sink).connect(ctx.destination);
  return true;
}

// pttDown keys the transmit group; pttUp releases it. With all, an
// all-call: every group at once, taking over any that is busy.
export async function pttDown(all = false) {
  const c = C();
  if (c.status !== 'on' || (!c.tx && !all) || c.ptt === 'talking' || c.ptt === 'asking') return;
  c.error = '';
  held = true;
  if (!(await ensureMic())) { held = false; return; }
  if (!held) {
    // Released while the browser asked for the microphone: nothing is keyed.
    c.error = 'The microphone is ready. Hold the button again to talk.'; redraw();
    return;
  }
  if (c.status !== 'on' || c.ptt === 'talking' || c.ptt === 'asking') return;
  c.ptt = 'asking'; c.allCall = all; redraw();
  send(all ? { t: 'ptt', all: true, down: true } : { t: 'ptt', group: c.tx, down: true });
}

export function pttUp() {
  const c = C();
  held = false;
  if (c.ptt === 'talking') stopTalking(true);
  else if (c.ptt === 'asking' || c.ptt === 'refused') { send(c.allCall ? { t: 'ptt', all: true, down: false } : { t: 'ptt', group: c.tx, down: false }); c.ptt = 'idle'; c.allCall = false; }
  redraw();
}

function stopTalking(tellNode) {
  const c = C();
  micOn(false);
  if (tellNode) send(c.allCall ? { t: 'ptt', all: true, down: false } : { t: 'ptt', group: c.tx, down: false });
  if (c.ptt === 'talking') {
    c.log.unshift({ at: new Date(c.txSince), group: c.allCall ? 0 : c.tx, s: Math.round((Date.now() - c.txSince) / 1000) });
    c.log.length = Math.min(c.log.length, 50);
    beep('release');
  }
  c.ptt = 'idle';
  c.allCall = false;
}

// A button held when the page loses the keyboard or goes out of sight is
// released: its key-up or pointer-up may never come.
window.addEventListener('blur', () => pttUp());
document.addEventListener('visibilitychange', () => { if (document.hidden) pttUp(); });
