// Shared helpers for the host and guest pages.
'use strict';

const $ = (sel, root = document) => root.querySelector(sel);

function el(tag, attrs = {}, ...kids) {
  const e = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs)) {
    if (k === 'class') e.className = v;
    else if (k.startsWith('on')) e.addEventListener(k.slice(2), v);
    else if (v !== undefined && v !== null && v !== false) e.setAttribute(k, v === true ? '' : v);
  }
  for (const kid of kids) {
    if (kid === null || kid === undefined || kid === false) continue;
    e.append(kid instanceof Node ? kid : document.createTextNode(String(kid)));
  }
  return e;
}

// WebSocket with JSON messages and simple event handlers.
class Channel {
  constructor(onMessage, onClose) {
    const proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
    this.ws = new WebSocket(`${proto}//${location.host}/ws`);
    this.ws.onmessage = (e) => {
      let msg;
      try { msg = JSON.parse(e.data); } catch { return; }
      onMessage(msg);
    };
    this.ws.onclose = () => onClose && onClose();
    this.ready = new Promise((res, rej) => {
      this.ws.onopen = res;
      this.ws.onerror = rej;
    });
  }
  send(obj) {
    if (this.ws.readyState === WebSocket.OPEN) this.ws.send(JSON.stringify(obj));
  }
}

function renderCounts(container, c) {
  container.replaceChildren(
    el('span', {}, '👁 ', el('strong', {}, c.watching), ' watching'),
    el('span', {}, '🎮 ', el('strong', {}, `${c.playing}/${c.player_capacity}`), ' playing'),
    el('span', {}, '👥 ', el('strong', {}, c.in_lobby), ' in lobby'),
  );
}

function addChatLine(log, msg) {
  const atBottom = log.scrollHeight - log.scrollTop - log.clientHeight < 40;
  let line;
  if (msg.from === null) {
    line = el('div', { class: 'chat-line sys' }, msg.text);
  } else {
    line = el('div', { class: 'chat-line' },
      el('span', { class: 'who' + (msg.host ? ' host' : '') }, msg.from + ':'),
      msg.text);
  }
  log.append(line);
  while (log.childElementCount > 300) log.firstElementChild.remove();
  if (atBottom) log.scrollTop = log.scrollHeight;
}

function wireChat(form, input, channel) {
  form.addEventListener('submit', (e) => {
    e.preventDefault();
    const text = input.value.trim();
    if (!text) return;
    channel.send({ t: 'chat', text });
    input.value = '';
  });
}

// Round-trip time (ms) of the active connection, or null.
async function measureRtt(pc) {
  if (!pc || pc.connectionState !== 'connected') return null;
  const stats = await pc.getStats();
  let rtt = null;
  stats.forEach((r) => {
    if (r.type === 'candidate-pair' && r.nominated && r.state === 'succeeded' &&
        typeof r.currentRoundTripTime === 'number') {
      rtt = Math.round(r.currentRoundTripTime * 1000);
    }
  });
  return rtt;
}
