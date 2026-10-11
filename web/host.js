'use strict';

let cfg = null;
let myId = null;
let stream = null;
let members = [];
let info = null;
const peers = new Map();   // guestId -> { pc, dc, rtt }

const preview = $('#preview');
const chatLog = $('#chatLog');

const channel = new Channel(onMessage, () => {
  $('#live').className = 'live off';
  $('#live').textContent = 'DISCONNECTED';
  addChatLine(chatLog, { from: null, text: 'Lost connection to the host app. Is it still running?' });
});
channel.ready.then(() => channel.send({ t: 'hello', role: 'host', name: 'Host' }));
wireChat($('#chatForm'), $('#chatInput'), channel);
$('#guestUrl').textContent = `http://<this PC's IP>:${location.port || 80}/`;

function onMessage(msg) {
  switch (msg.t) {
    case 'welcome':
      myId = msg.id;
      cfg = msg.config;
      $('#live').className = 'live off';
      $('#live').textContent = 'READY';
      break;
    case 'error':
      addChatLine(chatLog, { from: null, text: msg.msg });
      break;
    case 'roster':
      members = msg.members;
      info = msg.info;
      if (!$('#sessionName').value) $('#sessionName').value = info.session_name;
      if (!$('#gameName').value) $('#gameName').value = info.game_name;
      renderCounts($('#counts'), info.counts);
      $('#lockBtn').textContent = info.locked ? 'Open session to new people' : 'Close session to new people';
      $('#lockBtn').classList.toggle('on', info.locked);
      renderRoster();
      renderLobby(info.lobby);
      break;
    case 'joined':
      connectGuest(msg.id);
      break;
    case 'left':
      dropPeer(msg.id);
      break;
    case 'signal':
      onSignal(msg.from, msg.data);
      break;
    case 'chat':
      addChatLine(chatLog, msg);
      break;
  }
}

// ---------------------------------------------------------------- sharing --

$('#shareBtn').onclick = async () => {
  try {
    stream = await navigator.mediaDevices.getDisplayMedia({
      video: { frameRate: { ideal: 60, max: 60 }, width: { ideal: 1280 }, height: { ideal: 720 } },
      audio: true,
      selfBrowserSurface: 'exclude',
    });
  } catch (e) {
    addChatLine(chatLog, { from: null, text: 'Screen sharing was cancelled.' });
    return;
  }
  const v = stream.getVideoTracks()[0];
  if ('contentHint' in v) v.contentHint = 'motion';
  v.addEventListener('ended', stopSharing);
  preview.srcObject = stream;
  $('#previewHint').classList.add('hidden');
  $('#shareBtn').classList.add('hidden');
  $('#stopBtn').classList.remove('hidden');
  const s = v.getSettings();
  $('#shareInfo').textContent = `${s.width}×${s.height} @ ${Math.round(s.frameRate || 0)} fps` +
    (stream.getAudioTracks().length ? ' · with sound' : ' · no sound');
  setLive(true);
  reconnectAll();
};

$('#stopBtn').onclick = () => stopSharing();

function stopSharing() {
  if (!stream) return;
  stream.getTracks().forEach((t) => t.stop());
  stream = null;
  preview.srcObject = null;
  $('#previewHint').classList.remove('hidden');
  $('#shareBtn').classList.remove('hidden');
  $('#stopBtn').classList.add('hidden');
  $('#shareInfo').textContent = '';
  setLive(false);
  reconnectAll();
}

function setLive(on) {
  $('#live').className = on ? 'live' : 'live off';
  $('#live').textContent = on ? 'LIVE' : 'READY';
  sendMeta();
}

function sendMeta() {
  channel.send({
    t: 'meta',
    session_name: $('#sessionName').value,
    game_name: $('#gameName').value,
    sharing: !!stream,
  });
}
$('#sessionName').addEventListener('change', sendMeta);
$('#gameName').addEventListener('change', sendMeta);

$('#lockBtn').onclick = () => channel.send({ t: 'lock', on: !(info && info.locked) });

// Thumbnail for the join screen / future public lobby.
const thumbCanvas = document.createElement('canvas');
thumbCanvas.width = 320; thumbCanvas.height = 180;
setInterval(() => {
  if (!stream || preview.readyState < 2) return;
  thumbCanvas.getContext('2d').drawImage(preview, 0, 0, 320, 180);
  channel.send({ t: 'thumb', jpeg: thumbCanvas.toDataURL('image/jpeg', 0.7) });
}, 5000);

// ----------------------------------------------------------------- WebRTC --

function dropPeer(id) {
  const p = peers.get(id);
  if (p) {
    try { p.pc.close(); } catch {}
    peers.delete(id);
  }
}

function reconnectAll() {
  for (const m of members) if (m.role === 'guest') connectGuest(m.id);
}

async function connectGuest(id) {
  dropPeer(id);
  const pc = new RTCPeerConnection({ iceServers: cfg ? cfg.ice_servers : [] });
  const peer = { pc, dc: null, rtt: null };
  peers.set(id, peer);

  if (stream) {
    for (const track of stream.getTracks()) {
      const opts = { direction: 'sendonly', streams: [stream] };
      if (track.kind === 'video' && cfg) {
        opts.sendEncodings = [{
          maxBitrate: cfg.video_max_kbps * 1000,
          maxFramerate: cfg.video_max_fps,
        }];
      }
      pc.addTransceiver(track, opts);
    }
  }

  // Unordered, no retransmits: a late input is useless, the next one replaces it.
  const dc = pc.createDataChannel('input', { ordered: false, maxRetransmits: 0 });
  peer.dc = dc;
  dc.onmessage = (e) => {
    let s;
    try { s = JSON.parse(e.data); } catch { return; }
    channel.send({ t: 'in', id, s });
  };

  pc.onicecandidate = (e) => {
    if (e.candidate) channel.send({ t: 'signal', to: id, data: { candidate: e.candidate } });
  };
  pc.onconnectionstatechange = () => {
    if (pc.connectionState === 'connected') {
      // Keep frame rate smooth under pressure; fighting games need motion.
      for (const sender of pc.getSenders()) {
        if (sender.track && sender.track.kind === 'video') {
          const p = sender.getParameters();
          p.degradationPreference = 'maintain-framerate';
          sender.setParameters(p).catch(() => {});
        }
      }
    }
    renderRoster();
  };

  const offer = await pc.createOffer();
  await pc.setLocalDescription(offer);
  channel.send({ t: 'signal', to: id, data: { sdp: pc.localDescription } });
}

async function onSignal(from, data) {
  const peer = peers.get(from);
  if (!peer || !data) return;
  try {
    if (data.sdp && data.sdp.type === 'answer') {
      await peer.pc.setRemoteDescription(data.sdp);
    } else if (data.candidate) {
      await peer.pc.addIceCandidate(data.candidate);
    }
  } catch (e) {
    console.warn('signal error', e);
  }
}

setInterval(async () => {
  for (const [, p] of peers) p.rtt = await measureRtt(p.pc).catch(() => null);
  renderRoster();
}, 2000);

// ----------------------------------------------------------------- roster --

function renderRoster() {
  const box = $('#roster');
  const focused = document.activeElement;
  if (focused && focused.tagName === 'SELECT' && box.contains(focused)) return; // don't yank an open menu
  const slots = info ? info.slots : 0;
  const rows = [];
  for (const m of members) {
    if (m.role === 'host') {
      rows.push(el('div', { class: 'member' },
        el('div', { class: 'name' }, m.name, el('span', { class: 'tag host' }, 'host')),
        el('div', { class: 'meta' }, 'Your own controller')));
      continue;
    }
    const peer = peers.get(m.id);
    const state = peer ? peer.pc.connectionState : 'waiting';
    const ping = peer && peer.rtt !== null ? ` · ${peer.rtt} ms` : '';
    const tags = [];
    if (m.slot !== null) tags.push(el('span', { class: 'tag play' }, `Pad ${m.slot + 1}`));
    else if (m.wants) tags.push(el('span', { class: 'tag want' }, 'wants to play'));

    const select = el('select', {
      'aria-label': `Controller for ${m.name}`,
      onchange: (e) => {
        const v = e.target.value;
        channel.send({ t: 'assign', id: m.id, slot: v === '' ? null : Number(v) });
        e.target.blur();
      },
    }, el('option', { value: '' }, 'Watching'));
    for (let i = 0; i < slots; i++) {
      const holder = members.find((x) => x.slot === i && x.id !== m.id);
      select.append(el('option', { value: i, selected: m.slot === i },
        `Pad ${i + 1}${holder ? ` (swap ${holder.name})` : ''}`));
    }
    rows.push(el('div', { class: 'member' },
      el('div', { class: 'name' }, m.name, ...tags),
      el('div', { class: 'meta' }, `${state}${ping}`),
      el('div', { class: 'ctrls' },
        select,
        el('button', { class: 'danger', title: `Remove ${m.name}`,
          onclick: () => channel.send({ t: 'kick', id: m.id }) }, '✕'))));
  }
  if (rows.length <= 1) rows.push(el('div', { class: 'hint' }, 'No guests yet.'));
  box.replaceChildren(...rows);
}

// ------------------------------------------------------------ online lobby --

function renderLobby(lobby) {
  const btn = $('#publicBtn');
  if (!lobby) { btn.classList.add('hidden'); return; }
  btn.classList.remove('hidden');
  btn.classList.toggle('on', lobby.public);
  btn.textContent = lobby.public ? 'Listed in Online Lobby ✓' : 'List in Online Lobby';
  let state = '';
  if (!lobby.configured) state = 'No lobby server set (lobby_url in IkemenParty_config.json).';
  else if (lobby.public) state = lobby.state === 'listed' ? 'Everyone in the Online Lobby can see this session.' : lobby.state;
  $('#lobbyState').textContent = state;
  $('#lobbyLinkRow').classList.toggle('hidden', !lobby.join_url);
  $('#lobbyLink').textContent = lobby.join_url || '';
}
$('#publicBtn').onclick = () => {
  const on = !$('#publicBtn').classList.contains('on');
  channel.send({ t: 'public', on });
};
