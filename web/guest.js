'use strict';

let channel = null;
let cfg = null;
let myId = null;
let mySlot = null;
let wants = false;
let pc = null;
let dc = null;

const video = $('#video');
const chatLog = $('#chatLog');

// ------------------------------------------------------------ join screen --

async function refreshCard() {
  try {
    const r = await fetch('/api/session', { cache: 'no-store' });
    const s = await r.json();
    $('#joinTitle').textContent = s.session_name;
    $('#joinGame').textContent = s.game_name + (s.locked ? ' · closed to new people' : '');
    $('#joinLive').className = s.sharing ? 'live' : 'live off';
    $('#joinLive').textContent = s.sharing ? 'LIVE' : (s.host_online ? 'STARTING' : 'OFFLINE');
    renderCounts($('#joinCounts'), s.counts);
    if (s.sharing) {
      const img = new Image();
      img.onload = () => {
        $('#thumb').style.backgroundImage = `url(${img.src})`;
        $('#thumb').textContent = '';
      };
      img.src = '/thumb.jpg?t=' + Date.now();
    }
  } catch {
    $('#joinTitle').textContent = 'Session not reachable';
  }
}
refreshCard();
const cardTimer = setInterval(refreshCard, 5000);

try { $('#nameInput').value = localStorage.getItem('ikemenParty.name') || ''; } catch {}

$('#joinForm').addEventListener('submit', (e) => {
  e.preventDefault();
  const name = $('#nameInput').value.trim();
  if (!name) return;
  try { localStorage.setItem('ikemenParty.name', name); } catch {}
  join(name);
});

function join(name) {
  $('#joinError').classList.add('hidden');
  channel = new Channel(onMessage, () => {
    if (!$('#sessionView').classList.contains('hidden')) {
      addChatLine(chatLog, { from: null, text: 'Disconnected from the session.' });
      $('#live').className = 'live off';
      $('#live').textContent = 'DISCONNECTED';
      setSlot(null);
    }
  });
  channel.ready.then(() => channel.send({ t: 'hello', role: 'guest', name }))
    .catch(() => showJoinError('Could not reach the session.'));
  wireChat($('#chatForm'), $('#chatInput'), channel);
}

function showJoinError(text) {
  const b = $('#joinError');
  b.textContent = text;
  b.classList.remove('hidden');
}

// -------------------------------------------------------------- messages --

function onMessage(msg) {
  switch (msg.t) {
    case 'welcome':
      myId = msg.id;
      cfg = msg.config;
      clearInterval(cardTimer);
      $('#joinView').classList.add('hidden');
      $('#sessionView').classList.remove('hidden');
      if (!window.isSecureContext && location.hostname !== 'localhost') {
        $('#secureWarn').classList.remove('hidden');
      }
      break;
    case 'error':
      if ($('#sessionView').classList.contains('hidden')) showJoinError(msg.msg);
      else addChatLine(chatLog, { from: null, text: msg.msg });
      break;
    case 'roster':
      renderSession(msg.info, msg.members);
      break;
    case 'slot':
      setSlot(msg.slot);
      break;
    case 'signal':
      onSignal(msg.data);
      break;
    case 'chat':
      addChatLine(chatLog, msg);
      break;
  }
}

function renderSession(info, members) {
  const h = members.find((m) => m.role === 'host');
  hostMemberId = h ? h.id : null;
  $('#title').textContent = `${info.session_name} · ${info.game_name}`;
  $('#live').className = info.sharing ? 'live' : 'live off';
  $('#live').textContent = info.sharing ? 'LIVE' : (info.host_online ? 'NOT SHARING' : 'HOST AWAY');
  renderCounts($('#counts'), info.counts);
  if (!info.sharing) {
    $('#videoHint').textContent = info.host_online
      ? 'Waiting for the host to share the game…' : 'The host is away.';
    $('#videoHint').classList.remove('hidden');
  }
  const rows = members.map((m) => {
    const tags = [];
    if (m.role === 'host') tags.push(el('span', { class: 'tag host' }, 'host'));
    if (m.slot !== null) tags.push(el('span', { class: 'tag play' }, `Pad ${m.slot + 1}`));
    else if (m.wants) tags.push(el('span', { class: 'tag want' }, 'wants to play'));
    return el('div', { class: 'member' },
      el('div', { class: 'name' }, m.name + (m.id === myId ? ' (you)' : ''), ...tags));
  });
  $('#roster').replaceChildren(...rows);
  const me = members.find((m) => m.id === myId);
  if (me) {
    wants = me.wants;
    $('#wantBtn').classList.toggle('on', wants);
    $('#wantBtn').textContent = wants ? '✋ Asked to play' : '🎮 Ask to play';
    $('#wantBtn').classList.toggle('hidden', me.slot !== null);
  }
}

$('#wantBtn').onclick = () => channel.send({ t: 'want', on: !wants });

function setSlot(slot) {
  mySlot = slot;
  const b = $('#slotStatus');
  if (slot === null) {
    b.className = 'banner';
    b.textContent = "You're watching.";
  } else {
    b.className = 'banner ok';
    b.textContent = `You're playing on Pad ${slot + 1}. Your controller is live in the host's game.`;
    seq = Date.now(); // always ahead of anything sent before
    lastSent = '';
  }
}

// ----------------------------------------------------------------- WebRTC --

async function onSignal(data) {
  if (!data) return;
  try {
    if (data.sdp && data.sdp.type === 'offer') {
      if (pc) pc.close();
      pc = new RTCPeerConnection({ iceServers: cfg ? cfg.ice_servers : [] });
      pc.onicecandidate = (e) => {
        if (e.candidate) channel.send({ t: 'signal', to: hostId(), data: { candidate: e.candidate } });
      };
      pc.ontrack = (e) => {
        // Ask the browser to play frames as soon as they arrive.
        try { e.receiver.jitterBufferTarget = 0; } catch {}
        try { e.receiver.playoutDelayHint = 0; } catch {}
        if (e.track.kind === 'video') {
          video.srcObject = e.streams[0] || new MediaStream([e.track]);
          $('#videoHint').classList.add('hidden');
          video.play().catch(() => {});
        }
      };
      pc.ondatachannel = (e) => { dc = e.channel; };
      pc.onconnectionstatechange = () => {
        if (pc && pc.connectionState === 'failed') {
          addChatLine(chatLog, { from: null,
            text: "Couldn't connect video directly. Your network may need a relay (coming in a later version)." });
        }
      };
      await pc.setRemoteDescription(data.sdp);
      const answer = await pc.createAnswer();
      await pc.setLocalDescription(answer);
      channel.send({ t: 'signal', to: hostId(), data: { sdp: pc.localDescription } });
      if (!pc.getReceivers().some((r) => r.track && r.track.kind === 'video')) {
        video.srcObject = null;
        $('#videoHint').classList.remove('hidden');
      }
    } else if (data.candidate && pc) {
      await pc.addIceCandidate(data.candidate);
    }
  } catch (e) {
    console.warn('signal error', e);
  }
}

// The host's member id is learned from the roster (see renderSession).
let hostMemberId = null;
function hostId() { return hostMemberId; }

setInterval(async () => {
  const rtt = await measureRtt(pc).catch(() => null);
  $('#ping').textContent = rtt === null ? '' : `Ping to host: ${rtt} ms`;
}, 2000);

$('#soundBtn').onclick = () => {
  video.muted = !video.muted;
  $('#soundBtn').textContent = video.muted ? '🔇 Turn sound on' : '🔊 Sound on';
  video.play().catch(() => {});
};
$('#fsBtn').onclick = () => {
  const w = $('#videoWrap');
  if (document.fullscreenElement) document.exitFullscreen();
  else if (w.requestFullscreen) w.requestFullscreen();
};

// ------------------------------------------------------------------ input --

// Browser "standard gamepad" button order (matches pads.py).
const BTN = { A: 0, B: 1, X: 2, Y: 3, LB: 4, RB: 5, LT: 6, RT: 7, BACK: 8, START: 9,
  L3: 10, R3: 11, UP: 12, DOWN: 13, LEFT: 14, RIGHT: 15, GUIDE: 16 };
const LABELS = ['A', 'B', 'X', 'Y', 'LB', 'RB', 'LT', 'RT', 'Back', 'Start', 'L3', 'R3', '↑', '↓', '←', '→'];

const KEYMAP = {
  KeyW: BTN.UP, ArrowUp: BTN.UP, KeyS: BTN.DOWN, ArrowDown: BTN.DOWN,
  KeyA: BTN.LEFT, ArrowLeft: BTN.LEFT, KeyD: BTN.RIGHT, ArrowRight: BTN.RIGHT,
  KeyU: BTN.X, KeyI: BTN.Y, KeyO: BTN.RB,
  KeyJ: BTN.A, KeyK: BTN.B, KeyL: BTN.RT,
  Enter: BTN.START, Backslash: BTN.BACK,
};
let keyMask = 0;
const typing = () => document.activeElement && ['INPUT', 'TEXTAREA'].includes(document.activeElement.tagName);
addEventListener('keydown', (e) => {
  if (typing() || !(e.code in KEYMAP)) return;
  keyMask |= 1 << KEYMAP[e.code];
  e.preventDefault();
});
addEventListener('keyup', (e) => {
  if (!(e.code in KEYMAP)) return;
  keyMask &= ~(1 << KEYMAP[e.code]);
});
addEventListener('blur', () => { keyMask = 0; });

const padView = $('#padView');
LABELS.forEach((l) => padView.append(el('span', {}, l)));

let padIndex = null;
let seq = 0;
let lastSent = '';
let lastSendTime = 0;

const axis = (v) => Math.max(-127, Math.min(127, Math.round((Math.abs(v) < 0.12 ? 0 : v) * 127)));

function readState() {
  let mask = keyMask, lx = 0, ly = 0, rx = 0, ry = 0, lt = 0, rt = 0;
  const pads = navigator.getGamepads ? navigator.getGamepads() : [];
  // Lock onto whichever controller presses a button first.
  if (padIndex === null || !pads[padIndex]) {
    padIndex = null;
    for (const p of pads) {
      if (p && p.buttons.some((b) => b.pressed)) {
        padIndex = p.index;
        $('#padName').textContent = `Using: ${p.id.replace(/\s*\(.*?\)\s*/g, ' ').trim() || 'controller'}`;
        break;
      }
    }
  }
  const p = padIndex !== null ? pads[padIndex] : null;
  if (p) {
    p.buttons.forEach((b, i) => { if (i <= 16 && b.pressed) mask |= 1 << i; });
    lx = axis(p.axes[0] || 0); ly = axis(p.axes[1] || 0);
    rx = axis(p.axes[2] || 0); ry = axis(p.axes[3] || 0);
    lt = Math.round(((p.buttons[6] && p.buttons[6].value) || 0) * 255);
    rt = Math.round(((p.buttons[7] && p.buttons[7].value) || 0) * 255);
  }
  return [mask, lx, ly, rx, ry, lt, rt];
}

let lastView = -1;
function poll() {
  const state = readState();
  const mask = state[0] | (state[2] < -64 ? 1 << BTN.UP : 0) | (state[2] > 64 ? 1 << BTN.DOWN : 0) |
    (state[1] < -64 ? 1 << BTN.LEFT : 0) | (state[1] > 64 ? 1 << BTN.RIGHT : 0);
  if (mask !== lastView) {
    lastView = mask;
    [...padView.children].forEach((s, i) => s.classList.toggle('on', !!(mask & (1 << i))));
  }
  if (mySlot === null || !channel) return;
  const key = state.join(',');
  const now = performance.now();
  // Send on every change, plus a 100 ms heartbeat so the host knows we're here.
  if (key === lastSent && now - lastSendTime < 100) return;
  lastSent = key;
  lastSendTime = now;
  const msg = [++seq, ...state];
  if (dc && dc.readyState === 'open') dc.send(JSON.stringify(msg));
  else channel.send({ t: 'in', s: msg });
}
setInterval(poll, 4);
