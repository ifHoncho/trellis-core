// Voice: push-to-talk. Who is on the air on each talk group, this node's
// two channels, its headset, and the talk groups every node shares.
import { h } from '../dom.js';
import { post } from '../api.js';
import { state, redraw, toast } from '../state.js';
import { panel, field, toggle, button, kv, table, notice, pill, dot } from '../ui.js';
import { count } from '../fmt.js';
import { ensureConfig, cfg, saveBar, saveMeshProfile, saveNode, changeLock, editProfile, sentPending, noteUnsaved } from '../config.js';
import { consoleState, start, stop, setMonitor, setMix, mixOf, setName, setTx, pttDown, pttUp } from '../console.js';
import { clock } from '../time.js';

const EARS = [['left', 'Left ear'], ['right', 'Right ear'], ['both', 'Both ears']];
const err = (name) => cfg().fields[name];

// meter shows a level in dBFS on a -60 to 0 scale.
function meter(dbfs, tone = 'ok') {
  const pct = Math.max(0, Math.min(100, ((dbfs ?? -96) + 60) / 60 * 100));
  return h('div.meter.vmeter', { class: tone, style: { '--w': pct.toFixed(0) + '%' }, title: dbfs > -96 ? `${dbfs} dBFS` : 'silence' }, h('i'));
}

const groupName = (groups, n) => { const g = groups.find((x) => x.number === n); return g ? g.name : `group ${n}`; };

// onAir is the live table: every talk group, who talks on it, what this
// node does with it.
function onAir(vs, d, groups) {
  const role = (n) => {
    const out = [];
    const c = d.voice.channels || {};
    if (c.a && c.a.group === n) out.push('A · ' + (c.a.ear || 'both'));
    if (c.b && c.b.group === n) out.push('B · ' + (c.b.ear || 'both'));
    if ((d.voice.listen || []).includes(n)) out.push('listen');
    return out;
  };
  const live = new Map(((vs && vs.groups) || []).map((g) => [g.number, g]));
  return table({ stack: true, key: (g) => g.number, rowClass: (g) => (live.get(g.number) || {}).talker || (live.get(g.number) || {}).sending ? 'selected' : '' }, [
    { title: '#', cls: 'mono', cell: (g) => String(g.number) },
    { title: 'Talk group', cls: 'wide', wide: true, cell: (g) => h('div',
      h('div', h('span.primary', g.name), g.priority ? h('span', ' ', pill('priority', 'warn')) : null),
      h('div.secondary', (live.get(g.number) || {}).addr || '')) },
    { title: 'This node', cell: (g) => { const r = role(g.number); return r.length ? h('span.mono', r.join(', ')) : h('span.faint', 'not heard here'); } },
    { title: 'On the air', cell: (g) => {
      const l = live.get(g.number);
      if (!l) return h('span.faint', '—');
      if (l.sending) return h('span.row', { style: { gap: '8px' } }, dot('warn'), h('b', 'this node'));
      if (l.talker) return h('span.row', { style: { gap: '8px' } }, dot('ok'), h('b.mono', l.talker), l.all_call ? pill('all call', 'bad') : null);
      return h('span.faint', l.listening ? 'quiet' : '—');
    } },
    { title: 'Packets', cls: 'num', cell: (g) => { const l = live.get(g.number); return l && l.listening ? h('span', count(l.received), l.lost ? h('span.warn', ` · ${count(l.lost)} lost`) : null) : h('span.faint', '—'); } },
  ], groups, 'No talk groups yet: add them below, for every node.');
}

// channelCard is one channel's live state.
function channelCard(name, live, groups, d) {
  const silent = live && live.mic_cut;
  const c = live || {};
  let word = 'Off', tone = 'plain';
  if (c.group) {
    word = 'Listening'; tone = 'plain';
    if (c.sending) { word = 'Talking'; tone = 'warn'; }
    else if (c.ptt) { word = 'Busy: PTT refused'; tone = 'bad'; }
    else if (c.local_talker) { word = c.local_talker.startsWith('channel') ? 'Other channel talking' : 'This node’s console talking'; tone = 'warn'; }
    else if (c.out_dbfs > -60) { word = 'Receiving'; tone = 'ok'; }
  }
  return h('div.vchan',
    h('div.row.between',
      h('div.row', { style: { gap: '12px' } }, h('span.vchan-name', name.toUpperCase()),
        h('div', h('div.primary', c.group ? groupName(groups, c.group) : 'No group'),
          h('div.secondary', c.group ? `${c.ear || 'both'} ear · volume ${c.volume}` : 'this channel is off'))),
      pill(word, tone)),
    meter(c.out_dbfs, 'ok'),
    silent ? h('div', { style: { marginTop: '10px' } }, notice({ level: 'warning', message: 'A talk was stopped: the microphone was silent while the PTT was held (you heard a low long-short-long tone). This PTT seems to cut the microphone, as a phone-style adapter does.' }), pttFixes(d)) : null);
}

// pttPicker edits a PTT spec: a choice, or a serial or GPIO line typed in.
function pttPicker(obj, key, errName, base, linePlaceholder = 'serial:ttyACM0:cts or gpio:gpiochip0:17', button = true) {
  const ui = (state.ui.vptt ||= {});
  const v = obj[key] || '';
  const isLine = /^(serial|gpio):/.test(v) || ui[errName] === 'line';
  const opts = [...base, ...(button ? [['button', 'The adapter’s headset button (a phone-style PTT adapter)']] : []), ['cm108:1', 'CM108 pin 1'], ['cm108:2', 'CM108 pin 2'], ['cm108:3', 'CM108 pin 3'], ['cm108:4', 'CM108 pin 4'], ['line', 'A serial or GPIO line…']];
  return h('div', { style: { display: 'flex', flexDirection: 'column', gap: '8px' } },
    h('select.select', { value: isLine ? 'line' : (v || base[0][0]), onchange: (e) => {
      const x = e.target.value;
      ui[errName] = x === 'line' ? 'line' : '';
      obj[key] = x === 'line' ? '' : x === base[0][0] && base[0][0] === 'auto' ? '' : x;
      redraw();
    } }, opts.map(([id, l]) => h('option', { value: id }, l))),
    isLine ? h('input.input.mono', { value: v, placeholder: linePlaceholder, class: err(errName) ? 'invalid' : '',
      oninput: (e) => { obj[key] = e.target.value.trim(); redraw(); } }) : null);
}

function channelFields(d, key, groups, devices = []) {
  const ch = ((d.voice.channels ||= {})[key] ||= {});
  const f = `voice.channels.${key}`;
  return h('div.vchan-edit',
    h('div.label', `Channel ${key.toUpperCase()}`),
    h('div.fields',
      field('Talk group', h('select.select', { value: String(ch.group || 0), onchange: (e) => {
        ch.group = Number(e.target.value);
        // A channel's group is not also listened to: it would play twice.
        if (d.voice.listen) { d.voice.listen = d.voice.listen.filter((n) => n !== ch.group); if (!d.voice.listen.length) delete d.voice.listen; }
        redraw();
      } },
        [h('option', { value: '0' }, 'Off'), ...groups.map((g) => h('option', { value: String(g.number) }, `${g.number} · ${g.name}`))]), { error: err(f + '.group') }),
      field('Ear', h('select.select', { value: ch.ear || 'both', onchange: (e) => { ch.ear = e.target.value; redraw(); } },
        EARS.map(([v, l]) => h('option', { value: v }, l))), { error: err(f + '.ear') }),
      field(`Volume ${ch.volume ?? 80}`, h('input.range', { type: 'range', min: '0', max: '100', step: '5', value: String(ch.volume ?? 80),
        oninput: (e) => { ch.volume = Number(e.target.value); redraw(); } }), { error: err(f + '.volume') }),
      key === 'b' ? field('Device for channel B', h('select.select', { value: ch.device || '', onchange: (e) => { ch.device = e.target.value || undefined; redraw(); } },
        [h('option', { value: '' }, 'The same as channel A'), ...devices.filter((x) => x.capture).map((x) => h('option', { value: x.id }, `${x.model || x.name} (${x.id})`)),
          ...(ch.device && !devices.some((x) => x.id === ch.device) ? [h('option', { value: ch.device }, `${ch.device} (not plugged in)`)] : [])]),
        { error: err(f + '.device'), hint: 'A second headset with its own PTT: two OpenVLMs give one channel each.' }) : null,
      key === 'b' ? field('PTT for channel B', pttPicker(ch, 'ptt', f + '.ptt', [['', 'None']]),
        { error: err(f + '.ptt'), hint: 'A second PTT: dual PTT, one per channel.' }) : null,
      field('Radio key', pttPicker(ch, 'key', f + '.key', [['', 'None']], 'serial:ttyUSB0:rts or gpio:gpiochip0:27', false),
        { error: err(f + '.key'), hint: 'Only to bridge a two-way radio into this group (its transmitter is keyed while the group talks). Leave it at None for a headset and PTT.' })));
}

function settingsPanel(d, vs, groups) {
  const v = d.voice;
  const devices = (vs && vs.audio && vs.audio.devices) || [];
  const devOptions = [['auto', 'Automatic'],
    ...devices.map((x) => [x.id, `${x.model || x.name} (${x.id})${x.capture ? '' : ', no microphone'}${x.hid ? ', CM108 pins' : x.button ? ', headset button' : ''}`])];
  if (v.device && v.device !== 'auto' && !devOptions.some(([id]) => id === v.device)) devOptions.push([v.device, `${v.device} (not plugged in)`]);
  // Back to the defaults: device, PTT, levels and the rest. Kept: voice
  // on or off, and the talk groups this node is on. Saved with the usual
  // review.
  const reset = () => {
    const keep = (c) => (c && c.group ? { group: c.group } : {});
    d.voice = { enable: v.enable, channels: { a: keep(v.channels?.a), b: keep(v.channels?.b) }, ...(v.listen?.length ? { listen: v.listen } : {}) };
    redraw();
  };
  return panel('This node', { tools: v.enable ? [h('span.meta', 'saved to this node only'),
    button('Reset to defaults', reset, 'small', { title: 'Device, PTT, volumes, gain and the switches back to their defaults; the talk groups stay. Nothing changes until you save.' })] : null,
  meta: 'saved to this node only' },
    h('div.form',
      toggle('Voice on this node', 'Runs push-to-talk here: the headset and PTT plugged into this node, on the talk groups chosen below.', !!v.enable, (x) => { v.enable = x; redraw(); }),
      v.enable ? [
        channelFields(d, 'a', groups),
        channelFields(d, 'b', groups, devices),
        h('div.fields',
          field('Also listen to', h('div.vlisten', groups.filter((g) => g.number !== (v.channels?.a?.group || -1) && g.number !== (v.channels?.b?.group || -1)).map((g) => h('label.check.inline',
            h('input', { type: 'checkbox', checked: (v.listen || []).includes(g.number), onchange: (e) => {
              v.listen = (v.listen || []).filter((n) => n !== g.number);
              if (e.target.checked) v.listen.push(g.number);
              v.listen.sort((a, b) => a - b); redraw();
            } }), h('span', g.name)))), { hint: 'Heard in both ears, quieter; this node cannot talk on them.', error: err('voice.listen') }),
          field(`Listen volume ${v.listen_volume ?? 60}`, h('input.range', { type: 'range', min: '0', max: '100', step: '5', value: String(v.listen_volume ?? 60),
            oninput: (e) => { v.listen_volume = Number(e.target.value); redraw(); } }))),
        h('div.fields',
          field('Audio device', h('select.select', { value: v.device || 'auto', onchange: (e) => { v.device = e.target.value === 'auto' ? '' : e.target.value; redraw(); } },
            devOptions.map(([id, l]) => h('option', { value: id }, l))), { error: err('voice.device'), hint: 'Automatic takes an OpenVLM first, then any other CM108 adapter, then USB audio, then the board’s own.' }),
          field('PTT (channel A)', pttPicker(v, 'ptt', 'voice.ptt', [['auto', 'Automatic'], ['none', 'None: listen only'], ['vox', 'Voice (VOX)']]), { error: err('voice.ptt'), hint: 'Automatic: the adapter’s own PTT: an OpenVLM’s, or the headset button of a plain USB adapter, which a phone-style PTT adapter (3.5 mm to Kenwood) presses. It keys channel A. On a CM108 pin, a press reads high (a button to 3.3 V).' })),
        h('div.fields',
          field(`Microphone gain ${(v.mic_gain || 0) > 0 ? '+' : ''}${v.mic_gain || 0} dB`, h('input.range', { type: 'range', min: '-20', max: '20', step: '1', value: String(v.mic_gain || 0),
            oninput: (e) => { v.mic_gain = Number(e.target.value) || undefined; redraw(); } }), { error: err('voice.mic_gain'), hint: 'Before encoding. Watch the microphone meter: speech should peak well short of the end.' })),
        field('PTT action', h('select.select', { value: v.ptt_mode || 'hold', onchange: (e) => { v.ptt_mode = e.target.value === 'hold' ? undefined : e.target.value; redraw(); } },
          [['hold', 'Hold to talk'], ['toggle', 'Press to start, press to stop']].map(([id, l]) => h('option', { value: id }, l))),
          { error: err('voice.ptt_mode'), hint: 'Press to start, press to stop is for a PTT that silences the microphone while held, as a phone-style adapter (3.5 mm to Kenwood) does. Talk then stops after 60 s if not pressed again.' }),
        toggle('Automatic microphone gain', 'Brings speech to a steady level whatever the microphone: a radio speaker-mic puts out far less than a headset. Up to +30 dB; background noise is not raised.', v.mic_agc !== false, (x) => { v.mic_agc = x ? undefined : false; redraw(); }),
        toggle('Sidetone', 'Hear your own voice quietly in the channel’s ear while you talk.', !!v.sidetone, (x) => { v.sidetone = x || undefined; redraw(); }),
        toggle('Tones', 'Two beeps when the PTT is granted, one when released, three when the group is busy. Heard on this node only.', v.tones !== false, (x) => { v.tones = x ? undefined : false; redraw(); }),
      ] : null));
}

// pttFixes are the two ways out for a PTT that cuts the microphone: press
// to start and stop (the mic works then), or VOX. Saved with the usual
// review.
function pttFixes(d) {
  const go = (patch) => { Object.assign(d.voice, patch); saveNode(); };
  return h('div.row', { style: { marginTop: '8px' } },
    button('Use press to start, press to stop', () => go({ ptt_mode: 'toggle' }), 'small primary'),
    button('Use VOX', () => go({ ptt: 'vox' }), 'small'));
}

const VERDICTS = {
  ok: ['ok', 'Your PTT and microphone work: what you heard is what goes out.'],
  'cuts-mic': ['bad', 'The microphone goes silent while the PTT is held: this PTT cuts the microphone (a phone-style adapter does). Hold to talk cannot work with it.'],
  quiet: ['warn', 'The microphone works but is very quiet while the PTT is held. Speak closer, raise the microphone gain, or check the adapter.'],
  short: ['info', 'The PTT was released at once. Hold it while you speak a sentence.'],
  'no-ptt': ['warn', 'No PTT press came within 20 s. Is the PTT plugged in, and is it the PTT this channel uses?'],
};

// checkPanel runs "Check my PTT": the next press transmits nothing; what
// the microphone gives while it is held is played back and judged.
function checkPanel(vs, d) {
  const k = vs && vs.check;
  const start = async (ch) => {
    try { await post('/voice/test', { kind: 'ptt', channel: ch }); } catch (e) { toast(e.message, 'bad', 6000); }
  };
  const chB = vs && vs.channels && vs.channels[1] && vs.channels[1].group && ((d.voice.channels || {}).b || {}).ptt;
  let body = h('p.dim', { style: { margin: '0', fontSize: '13.5px' } }, 'Hold your PTT, say a sentence, release: you hear what would have been sent, and how it did. Nothing goes on the air.');
  if (k && k.state === 'waiting') body = notice({ level: 'info', message: `Channel ${k.channel.toUpperCase()}: press and hold the PTT now, say a sentence, then release.` });
  if (k && k.state === 'recording') body = notice({ level: 'info', message: 'Recording while you hold it… release when you are done.' });
  if (k && k.state === 'playing') body = notice({ level: 'info', message: 'Playing back what would have been sent.' });
  if (k && k.state === 'done') {
    const [tone, text] = VERDICTS[k.verdict] || ['info', k.verdict];
    body = h('div', notice({ level: { ok: 'ok', bad: 'error', warn: 'warning', info: 'info' }[tone], message: text }),
      k.verdict !== 'no-ptt' && k.verdict !== 'short' ? h('p.dim', { style: { margin: '8px 0 0', fontSize: '13px' } },
        `Microphone while held: ${k.held_dbfs} dBFS; at rest: ${k.released_dbfs} dBFS; ${k.seconds} s.`) : null,
      k.verdict === 'cuts-mic' ? pttFixes(d) : null);
  }
  return h('div', { style: { marginTop: '16px' } },
    h('div.label', { style: { marginBottom: '8px' } }, 'Check my PTT'), body,
    h('div.row', { style: { marginTop: '10px' } },
      button('Check channel A', () => start('a'), 'small', { disabled: !vs || (k && (k.state === 'waiting' || k.state === 'recording')) }),
      chB ? button('Check channel B', () => start('b'), 'small', { disabled: k && (k.state === 'waiting' || k.state === 'recording') }) : null));
}

function headsetPanel(vs, d) {
  const a = (vs && vs.audio) || null;
  const test = async (kind, channel) => {
    try { await post('/voice/test', { kind, channel }); }
    catch (e) { toast(e.message, 'bad', 6000); }
  };
  return panel('Headset', {},
    a ? kv([
      ['Device', a.device === 'none' ? h('span.warn', 'none with a microphone: listening only') : h('span', a.model || a.name || a.device, h('span.faint', `  ·  ${a.device}`))],
      ['PTT', a.ptt === 'none' ? h('span.faint', 'none') : a.ptt],
      a.mono ? ['Earpiece', 'one: both channels play in it'] : null,
      (a.levels || []).length ? ['Card levels', h('span', a.levels.map((l) => l.kind === 'monitor off' || l.kind === 'off' ? `${l.control} off` : `${l.control} ${l.db > 0 ? '+' : ''}${l.db} dB`).join(' · '))] : null,
      vs && vs.agc_db ? ['Mic AGC', `+${vs.agc_db} dB now`] : null,
    ]) : h('p.dim', 'Waiting for the voice service.'),
    h('div', { style: { margin: '16px 0 6px' } }, h('div.label', 'Microphone')),
    meter(vs ? vs.mic_dbfs : -96, vs && vs.loopback ? 'warn' : 'ok'),
    h('div.row', { style: { marginTop: '14px' } },
      button('Tone, channel A', () => test('tone', 'a'), 'small', { disabled: !vs }),
      button('Tone, channel B', () => test('tone', 'b'), 'small', { disabled: !vs }),
      vs && vs.loopback
        ? button('Stop hearing the microphone', () => test('stop'), 'small primary')
        : button('Hear my microphone, 10 s', () => test('loopback'), 'small', { disabled: !vs })),
    h('p.dim', { style: { marginTop: '12px', fontSize: '13px' } }, 'Checks on this node only: nothing goes on the air.'),
    a && a.no_mic ? h('div', { style: { marginTop: '14px' } }, notice({ level: 'warning',
      message: `${a.name || a.device} shows no microphone, so this node can listen but not talk. Try putting the 3.5 mm plug back in. Adapters other than the supported ones (an OpenVLM or a CM108 adapter) may not support every feature.` })) : null,
    a && a.no_ptt ? h('div', { style: { marginTop: '14px' } }, notice({ level: 'warning',
      message: `${a.name || a.device} offers no PTT, so channel A listens only. Adapters other than the supported ones (an OpenVLM or a CM108 adapter) may not support every feature.` })) : null,
    a && vs && vs.mic_dead && (a.device || '').startsWith('onboard:') ? h('div', { style: { marginTop: '10px' } }, notice({ level: 'warning',
      message: `No signal from ${a.name || a.device}’s microphone input: this board’s jack has no microphone, so it can only listen. A USB audio adapter adds one.` })) : null,
    a && (a.button_hold || []).length ? h('div', { style: { marginTop: '14px' } },
      notice({ level: 'warning', message: `The PTT (channel ${a.button_hold.join(' and ').toUpperCase()}) is the adapter’s headset button, which silences the microphone while it is held: with hold to talk, nothing you say is sent.` }), pttFixes(d)) : null,
    checkPanel(vs, d));
}

// groupsPanel edits the profile's talk groups and sends them to every node.
function groupsPanel(st) {
  const ui = (state.ui.voiceGroups ||= { draft: null, for: '', fields: {}, busy: false });
  const cur = editProfile();
  const curGroups = JSON.stringify((cur.voice && cur.voice.groups) || []);
  if (!ui.draft || ui.for !== curGroups) { ui.draft = JSON.parse(curGroups); ui.for = curGroups; ui.fields = {}; }
  const gs = ui.draft;
  const changed = JSON.stringify(gs) !== ui.for;
  // A mesh-wide change on its way: nothing more is sent until every node
  // has switched (a second change would replace it).
  const waits = !!changeLock(st);
  // Only a change this editor sent makes its draft "sent".
  const mine = sentPending('voice', st);
  noteUnsaved('voice', changed && !mine);
  const others = (st.nodes || []).length;
  const send = async () => {
    ui.busy = true; ui.fields = {}; redraw();
    try {
      const p = JSON.parse(JSON.stringify(cur));
      p.voice = { ...(p.voice || {}), groups: gs };
      await saveMeshProfile({ profile: p }, 60, 'voice');
    } catch (e) {
      for (const f of e.fields || []) ui.fields[f.field] = f.message;
      toast(e.message.split('\n')[0], 'bad', 7000);
    }
    ui.busy = false; redraw();
  };
  const free = () => { for (let n = 1; n <= 16; n++) if (!gs.some((g) => g.number === n)) return n; return 0; };
  return panel('Talk groups', { meta: 'the same on every node' },
    h('div.form',
      gs.map((g, i) => h('div.fields.vgroup',
        field(`Group ${g.number}`, h('input.input', { value: g.name, maxlength: '32', placeholder: 'name', class: ui.fields[`voice.groups[${i}].name`] ? 'invalid' : '',
          oninput: (e) => { g.name = e.target.value; redraw(); } }), { error: ui.fields[`voice.groups[${i}].name`] || ui.fields[`voice.groups[${i}]`] }),
        field('Address and port', h('input.input.mono', { value: g.address ? `${g.address}:${g.port || ''}` : '', placeholder: `239.73.80.${g.number}:${38799 + 2 * g.number}`,
          oninput: (e) => { const [a, p] = e.target.value.split(':'); g.address = a || undefined; g.port = p ? Number(p) : undefined; if (!g.address) delete g.address; if (!g.port) delete g.port; redraw(); } }),
          { error: ui.fields[`voice.groups[${i}].address`] || ui.fields[`voice.groups[${i}].port`], hint: 'Empty: the default. RTCP uses the next port.' }),
        h('div.row', { style: { alignSelf: 'end', gap: '14px' } },
          toggle('Priority', '', !!g.priority, (x) => { g.priority = x || undefined; redraw(); }),
          button('Remove', () => { gs.splice(i, 1); redraw(); }, 'small')))),
      h('div.row',
        button('Add a talk group', () => { const n = free(); gs.push({ number: n, name: '' }); redraw(); }, 'small', { disabled: gs.length >= 16 }),
        h('span.hint', 'Up to 16. A group is a multicast address and port: an ATAK Voice plugin joins one with the same address and port, protocol RTP.')),
      h('div.row.end',
        changed && !mine ? button('Discard', () => { ui.draft = null; redraw(); }, '', { disabled: ui.busy }) : null,
        button(ui.busy ? 'Checking…' : waits ? 'Waiting for the switch…' : others ? 'Review and send to every node' : 'Review and save', send, 'primary',
          { disabled: !changed || ui.busy || waits, title: waits ? 'A mesh-wide change is on its way: a second one now would replace it and drop what it changes' : null }))));
}

// consolePanel is this browser as an operator: it monitors talk groups
// through the node and keys one (a TOC).
function consolePanel(vstate) {
  const c = consoleState();
  const running = vstate && vstate.running;
  if (c.status === 'off') {
    return panel('Console', { meta: 'monitor and talk from this browser' },
      h('div.form',
        c.error ? notice({ level: 'warning', message: c.error }) : null,
        h('p.dim', { style: { margin: '0', fontSize: '14px', maxWidth: '70ch' } },
          'Hear the talk groups here, and talk on one, through this node: for a TOC with the UI open. Your browser asks for the microphone the first time you talk.'),
        h('div.row',
          h('input.input', { value: c.name, maxlength: '24', style: { maxWidth: '220px' }, placeholder: 'console name', oninput: (e) => setName(e.target.value) }),
          button('Start the console', () => start(), 'primary', { disabled: !running }),
          !running ? h('span.hint', 'Voice must be running on this node (it needs no headset to serve a console).') : h('span.hint', 'Listeners see you as ' + (state.status ? state.status.node.callsign : 'node') + '/' + (c.name || 'console') + '.'))));
  }
  const live = new Map(((c.live && c.live.groups) || []).map((g) => [g.number, g]));
  const tx = c.groups.find((g) => g.number === c.tx);
  const all = !!c.allCall;
  const txWord = { idle: 'Hold to talk', asking: 'Keying…', talking: 'Talking: release to stop', refused: 'Busy: release, then try again' }[all ? 'idle' : c.ptt];
  const allWord = all ? { asking: 'Keying every group…', talking: 'ALL CALL: release to stop', refused: 'Not keyed', idle: 'Hold for all call' }[c.ptt] : 'Hold for all call';
  // press builds the pointer and key handlers of a hold-to-talk button.
  const press = (isAll) => ({
    onpointerdown: (e) => { e.preventDefault(); try { e.currentTarget.setPointerCapture(e.pointerId); } catch (err) { /* a synthetic press */ } pttDown(isAll); },
    onpointerup: () => pttUp(), onpointercancel: () => pttUp(), onlostpointercapture: () => pttUp(),
    onblur: () => pttUp(),
    onkeydown: (e) => { if ((e.key === ' ' || e.key === 'Enter') && !e.repeat) { e.preventDefault(); pttDown(isAll); } },
    onkeyup: (e) => { if (e.key === ' ' || e.key === 'Enter') { e.preventDefault(); pttUp(); } },
    oncontextmenu: (e) => e.preventDefault(),
  });
  const txSecs = c.ptt === 'talking' ? Math.floor((Date.now() - c.txSince) / 1000) : 0;
  const level = (n) => { const v = c.levels[n] || 0; return v > 0.001 ? Math.round(20 * Math.log10(v)) : -96; };
  return panel('Console', { meta: c.status === 'on' ? `on the air as ${c.consoleName}` : c.status === 'connecting' ? 'connecting…' : '',
    tools: [button('Stop', () => stop(), 'small')] },
    c.error ? notice({ level: 'warning', message: c.error }) : null,
    h('div.vtiles', c.groups.map((g) => {
      const l = live.get(g.number) || {};
      const m = mixOf(g.number);
      const on = c.monitor.has(g.number);
      const talker = l.sending ? (c.ptt === 'talking' && (all || c.tx === g.number) ? (all ? 'you: all call' : 'you') : 'this node') : l.talker;
      return h('div.vtile', { class: [on ? 'on' : '', talker ? 'active' : ''].join(' ') },
        h('div.row.between',
          h('div', h('span.primary', g.name), g.priority ? h('span', ' ', pill('priority', 'warn')) : null),
          toggle('', '', on, (x) => setMonitor(g.number, x))),
        h('div.vtalker', talker ? h('span.row', { style: { gap: '8px' } }, dot(l.sending ? 'warn' : 'ok'), h('b.mono', talker), l.all_call ? pill('all call', 'bad') : null) : h('span.faint', on ? 'quiet' : 'not monitored')),
        meter(on && !m.muted ? level(g.number) : -96, 'ok'),
        on ? h('div.row', { style: { marginTop: '8px', gap: '8px' } },
          h('input.range', { type: 'range', min: '0', max: '1', step: '0.05', value: String(m.gain), style: { flex: '1' }, title: 'Volume',
            oninput: (e) => setMix(g.number, { gain: Number(e.target.value) }) }),
          h('div.seg.small', [[-1, 'L'], [0, 'C'], [1, 'R']].map(([v, lbl]) =>
            h('button', { 'aria-pressed': String(m.pan === v), onclick: () => setMix(g.number, { pan: v }) }, lbl))),
          button(m.muted ? 'Unmute' : 'Mute', () => setMix(g.number, { muted: !m.muted }), 'small')) : null);
    })),
    h('div.vtx',
      h('div.form', { style: { gap: '10px' } },
        field('Talk on', h('select.select', { value: String(c.tx || ''), onchange: (e) => setTx(Number(e.target.value)), disabled: c.ptt === 'talking' },
          c.groups.map((g) => h('option', { value: String(g.number) }, `${g.number} · ${g.name}`)))),
        h('div.label', 'Microphone'), meter(c.mic > 0.001 ? Math.round(20 * Math.log10(c.mic)) : -96, c.ptt === 'talking' ? 'warn' : 'ok')),
      h('div.vbuttons',
        h('button.vptt', { class: all ? '' : c.ptt, disabled: c.status !== 'on' || !tx || (all && c.ptt !== 'idle'), ...press(false) },
          h('span.vptt-word', txWord), h('span.vptt-sub', tx ? (!all && c.ptt === 'talking' ? `${tx.name} · ${txSecs} s of 60` : tx.name) : 'no talk group')),
        h('button.vptt.vall', { class: all ? c.ptt : '', disabled: c.status !== 'on' || !c.groups.length || (!all && c.ptt !== 'idle'),
          title: 'Every talk group at once, for urgent messages: it goes out even where someone is talking, and Trellis nodes switch to it.', ...press(true) },
          h('span.vptt-word', allWord), h('span.vptt-sub', all && c.ptt === 'talking' ? `${c.groups.length} groups · ${txSecs} s of 60` : `every group (${c.groups.length}), overrides`)))),
    c.log.length ? h('div.vlog', h('div.label', 'You talked'), c.log.slice(0, 6).map((x) =>
      h('div.mono', `${clock(x.at)}  ${x.group === 0 ? 'ALL CALL' : (c.groups.find((g) => g.number === x.group) || {}).name || x.group}  ${x.s} s`))) : null);
}

export function voice() {
  const st = state.status;
  const c = ensureConfig();
  const vstate = state.voice;
  const vs = vstate && vstate.status;
  const head = h('header.page-head', h('div', h('h1', 'Voice'),
    h('p.sub', 'Push-to-talk over the mesh, from the headset and PTT plugged into this node. Talk groups are the same on every node; each node hears two of them, one per ear.')));
  if (!c.draft || !state.config) return h('div.page', head, panel('', {}, h('p.dim', c.error || 'Loading…')));
  const d = c.draft;
  d.voice ||= {};
  const groups = (state.config.profile.voice && state.config.profile.voice.groups) || [];
  let line = null;
  if (vstate && vstate.enabled && !vstate.running) line = notice({ level: 'warning', message: vstate.error || 'The voice service is not running.' });
  if (vstate && !vstate.enabled && !d.voice.enable) line = notice({ level: 'info', message: vstate.installed === false
    ? 'Voice is off, and Trellis on this node has no voice: it is built for arm64 and amd64 only.'
    : 'Voice is off on this node. Turn it on below.' });
  return h('div.page', head,
    line,
    consolePanel(vstate),
    h('div.grid',
      h('div.c7.stackcol',
        panel('On the air', { flush: true, meta: vs && vs.fec_loss_perc ? `redundancy for ${vs.fec_loss_perc} % loss` : null }, onAir(vs, d, groups)),
        settingsPanel(d, vs, groups)),
      h('div.c5.stackcol.vside',
        panel('Channels', {},
          h('div.vchans', ['a', 'b'].map((k, i) => channelCard(k, vs && vs.channels && vs.channels[i], groups, d)))),
        headsetPanel(vstate && vstate.running ? vs : null, d))),
    groupsPanel(st),
    saveBar());
}
