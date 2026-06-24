// innerlink-desktop frontend.
//
// Vanilla TS — no framework, no virtual DOM. Direct DOM
// manipulation + Wails events. Mirrors the v0.1 mockup:
// peer sidebar on the left, chat panel on the right.
//
// All backend interaction goes through the auto-generated
// bindings under frontend/wailsjs/go/main/App.* and the
// runtime.EventsOn event bus. See app.go for the Go side.
//
// Refresh strategy:
//   - On startup we poll ListPeers + History once.
//   - We listen on the "peer:event" + "message:event"
//     Wails event streams for live updates.
//   - On peer selection we re-fetch History and scroll
//     the message list to the bottom.

import './style.css';
import './app.css';

import {
  DialAddr,
  History,
  ListPeers,
  Ping,
  RemoveAlias,
  Scan,
  SelfPeerID,
  SendFile,
  SendFileContent,
  SendText,
  SetAlias,
} from '../wailsjs/go/app/App';
import { EventsOn, OnFileDrop } from '../wailsjs/runtime/runtime';
import { node } from '../wailsjs/go/models';

// ----- in-memory state -----
interface UIState {
  selfId: string;
  peers: node.PeerInfo[];        // peers minus self (sidebar)
  selfEntry: node.PeerInfo | null;
  selectedId: string | null;     // peer hex ID of the open conversation
  history: Map<string, node.Message[]>; // peer hex ID → msgs
  aliases: Map<string, string>;  // peer hex ID → alias name
  // nearBottom reflects whether the messages panel is
  // currently scrolled within ~60px of the bottom. We
  // update it on every scroll event and read it after
  // appending a new message to decide whether to
  // stick to the bottom (chat-style auto-scroll) or
  // leave the user where they are (so they can read
  // history without being yanked around).
  nearBottom: boolean;
  // peers we've already auto-aliased from their
  // hostname. Avoids spamming SetAlias on every
  // peer:event when nothing changed.
  autoAliased: Set<string>;
  // pendingFile: a file the user staged via 📎 picker
  // or drag-and-drop, not yet sent. When set, the
  // composer shows a file card and submit routes to
  // SendFile / SendFileContent instead of SendText.
  // `path` is used for drag-drop (real on-disk path);
  // `content` is used for picker (File API hides the
  // path on modern browsers, so we read the bytes).
  pendingFile: { name: string; size: number; path?: string; content?: Uint8Array } | null;
  // unreadCount: peer hex ID -> number of incoming
  // messages not yet seen. Reset to 0 when the user
  // opens that peer's chat (in selectPeer).
  unreadCount: Map<string, number>;
}

const state: UIState = {
  selfId: '',
  peers: [],
  selfEntry: null,
  selectedId: null,
  history: new Map(),
  aliases: new Map(),
  nearBottom: true,
  autoAliased: new Set(),
  pendingFile: null,
  unreadCount: new Map(),
};

// ----- DOM injection (one-shot at startup) -----
function mount() {
  document.querySelector('#app')!.innerHTML = `
    <div class="app">
      <aside class="sidebar">
        <div class="me">
          <div class="me-label">this device</div>
          <div class="me-name" id="me-name">—</div>
          <div class="me-id" id="me-id">…</div>
          <div class="me-status"><span class="led"></span> <span id="me-status">starting…</span></div>
        </div>
        <div class="sidebar-header">
          <span class="sidebar-title">peers</span>
          <span class="sidebar-count" id="peer-count">0</span>
        </div>
        <div class="peer-list" id="peer-list"></div>
        <form class="sidebar-footer" id="scan-form">
          <input id="scan-input" type="text" placeholder="scan 192.168.1.0/24" autocomplete="off"/>
          <button type="submit">scan</button>
        </form>
      </aside>
      <main class="chat">
        <header class="chat-header">
          <div class="chat-peer" id="chat-peer">
            <span class="led"></span>
            <div>
              <div class="chat-name" id="chat-name">no peer selected</div>
              <div class="chat-id" id="chat-id">—</div>
            </div>
          </div>
          <div class="chat-actions">
            <button id="btn-ping" disabled>ping</button>
            <button id="btn-alias" disabled>name…</button>
            <button id="btn-dial" disabled>dial…</button>
          </div>
        </header>
        <section class="messages" id="messages">
          <div class="empty">
            <div class="em-title">no peer selected</div>
            <div>pick someone from the sidebar to start chatting</div>
          </div>
        </section>
        <form class="composer" id="composer">
          <button type="button" id="btn-attach" title="attach file" disabled>📎</button>
          <textarea id="composer-input" placeholder="type a message…" disabled></textarea>
          <button type="submit" id="composer-send" disabled>send</button>
        </form>
        <div class="drop-hint" id="drop-hint">drop file to attach</div>
        <div class="pending-file" id="pending-file"></div>
        <input type="file" id="file-input" style="display:none" />
      </main>
    </div>
    <div class="toast" id="toast"></div>
  `;
}

// ----- helpers -----
function toast(msg: string) {
  const t = document.getElementById('toast')!;
  t.textContent = msg;
  t.classList.add('show');
  setTimeout(() => t.classList.remove('show'), 2400);
}

function shortId(id: string): string {
  return id ? id.slice(0, 8) : '';
}

function fmtTime(ts: any): string {
  // ts comes back from Go as an RFC3339 string (Wails
  // marshals time.Time as a string into the binding).
  const d = ts ? new Date(ts) : new Date();
  if (isNaN(d.getTime())) return '';
  return d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });
}

function peerDisplay(p: node.PeerInfo): string {
  return p.Name || (p.Hostname ? p.Hostname : `peer ${shortId(p.PeerID)}`);
}

function selectedPeer(): node.PeerInfo | null {
  if (!state.selectedId) return null;
  return state.peers.find(p => p.PeerID === state.selectedId) ?? null;
}

// ----- renders -----
function renderMe() {
  const me = state.selfEntry;
  const id = state.selfId;
  document.getElementById('me-name')!.textContent = me?.Name || me?.Hostname || 'this device';
  document.getElementById('me-id')!.textContent = shortId(id);
  document.getElementById('me-status')!.textContent = 'listening';
}

function renderPeerList() {
  const list = document.getElementById('peer-list')!;
  document.getElementById('peer-count')!.textContent = String(state.peers.length);

  if (state.peers.length === 0) {
    list.innerHTML = `<div style="padding:14px; color:var(--muted); font-size:12.5px; text-align:center;">
      no peers yet<br/><br/>
      <span style="font-size:11px;">try <code>scan 192.168.x.0/24</code> below</span>
    </div>`;
    return;
  }

  // Sort: online first, then by recency.
  const sorted = [...state.peers].sort((a, b) => {
    if (a.Online !== b.Online) return a.Online ? -1 : 1;
    const at = a.LastSeen ? new Date(a.LastSeen).getTime() : 0;
    const bt = b.LastSeen ? new Date(b.LastSeen).getTime() : 0;
    return bt - at;
  });

  list.innerHTML = sorted.map(p => `
    <div class="peer ${p.Online ? 'online' : ''} ${p.PeerID === state.selectedId ? 'active' : ''}" data-peer="${p.PeerID}">
      <span class="led"></span>
      <div>
        <div class="peer-name">${escapeHtml(peerDisplay(p))}</div>
        <div class="peer-id">${shortId(p.PeerID)}${p.Hostname && p.Name ? ' · ' + escapeHtml(p.Hostname) : ''}</div>
      </div>
      <span class="badge ${(state.unreadCount.get(p.PeerID) || 0) > 0 ? 'show' : ''}">${state.unreadCount.get(p.PeerID) || 0}</span>
      <span class="peer-action" data-action="alias" data-peer="${p.PeerID}" title="set alias">rename</span>
    </div>
  `).join('');

  list.querySelectorAll<HTMLElement>('.peer').forEach(el => {
    el.addEventListener('click', (ev) => {
      const target = ev.target as HTMLElement;
      const action = target.getAttribute('data-action');
      if (action === 'alias') {
        const peerRef = target.getAttribute('data-peer')!;
        promptAlias(peerRef);
        return;
      }
      selectPeer(el.getAttribute('data-peer')!);
    });
  });
}

function renderChatHeader() {
  const p = selectedPeer();
  const head = document.getElementById('chat-peer')!;
  const name = document.getElementById('chat-name')!;
  const idEl = document.getElementById('chat-id')!;
  const pingBtn = document.getElementById('btn-ping') as HTMLButtonElement;
  const aliasBtn = document.getElementById('btn-alias') as HTMLButtonElement;
  const dialBtn = document.getElementById('btn-dial') as HTMLButtonElement;
  const attachBtn = document.getElementById('btn-attach') as HTMLButtonElement;

  if (!p) {
    head.classList.remove('online');
    name.textContent = 'no peer selected';
    idEl.textContent = '—';
    pingBtn.disabled = true;
    aliasBtn.disabled = true;
    dialBtn.disabled = true;
    attachBtn.disabled = true;
    return;
  }
  head.classList.toggle('online', p.Online);
  name.textContent = peerDisplay(p);
  idEl.textContent = shortId(p.PeerID);
  pingBtn.disabled = !p.Online;
  aliasBtn.disabled = false;
  dialBtn.disabled = false;
  attachBtn.disabled = false;
}

function renderMessages() {
  const el = document.getElementById('messages')!;
  // Capture pre-render position so we can decide after
  // the innerHTML swap whether to stick to the bottom
  // or stay where the user has scrolled to.
  const wasNearBottom = isNearBottom(el);
  if (!state.selectedId) {
    el.innerHTML = `<div class="empty">
      <div class="em-title">no peer selected</div>
      <div>pick someone from the sidebar to start chatting</div>
    </div>`;
    return;
  }
  const msgs = state.history.get(state.selectedId) ?? [];
  if (msgs.length === 0) {
    el.innerHTML = `<div class="empty">
      <div class="em-title">no messages yet</div>
      <div>say hi 👋</div>
    </div>`;
    return;
  }
  el.innerHTML = msgs.map(m => {
    const isFile = m.Body.startsWith('file:');
    if (isFile) {
      const payload = m.Body.slice('file:'.length);
      return `<div class="msg ${m.Direction === 'out' ? 'out' : 'in'} file">
        <div class="bubble">
          <span class="file-icon">📎</span>
          <div class="file-info">
            <div class="file-name">${escapeHtml(payload)}</div>
            <div class="file-meta">${m.Direction === 'out' ? 'sent' : 'received'}</div>
          </div>
        </div>
        <div class="msg-time">${fmtTime(m.Timestamp)}</div>
      </div>`;
    }
    return `<div class="msg ${m.Direction === 'out' ? 'out' : 'in'}">
      <div class="bubble">${escapeHtml(m.Body)}</div>
      <div class="msg-time">${fmtTime(m.Timestamp)}</div>
    </div>`;
  }).join('');
  // Smart auto-scroll: only pin to bottom if the user
  // was already near the bottom when the render started.
  // Otherwise leave them where they are so reading
  // history doesn't get yanked around.
  if (wasNearBottom) {
    el.scrollTop = el.scrollHeight;
  }
  // Refresh the tracked position so subsequent renders
  // (e.g. live incoming messages) make the right call.
  state.nearBottom = isNearBottom(el);
}

// isNearBottom reports whether `el` is within ~60px of
// its scroll bottom. Used by renderMessages for
// "stick-to-bottom-or-not" decisions and by the scroll
// listener to keep state.nearBottom up to date.
function isNearBottom(el: HTMLElement): boolean {
  return (el.scrollHeight - el.scrollTop - el.clientHeight) < 60;
}

function escapeHtml(s: string): string {
  return s.replace(/[&<>"']/g, ch => ({
    '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;',
  }[ch]!));
}

// ----- actions -----
async function selectPeer(peerId: string) {
  state.selectedId = peerId;
  // Opening a conversation clears its unread badge.
  state.unreadCount.set(peerId, 0);
  // Lazy-load history for this peer.
  try {
    const h = await History(peerId);
    state.history.set(peerId, (h as node.Message[]) || []);
  } catch (e) {
    state.history.set(peerId, []);
    toast(`history failed: ${e}`);
  }
  renderPeerList();
  renderChatHeader();
  renderMessages();
  // Enable composer.
  const input = document.getElementById('composer-input') as HTMLTextAreaElement;
  const send = document.getElementById('composer-send') as HTMLButtonElement;
  const attach = document.getElementById('btn-attach') as HTMLButtonElement;
  input.disabled = false;
  send.disabled = false;
  attach.disabled = false;
  input.focus();
}

async function refreshAll() {
  try {
    const peers = (await ListPeers()) as node.PeerInfo[];
    state.selfId = await SelfPeerID();
    // Filter out self. We do this defensively in two ways:
    //   1. p.IsSelf (the official flag from core)
    //   2. p.PeerID === selfId (fallback in case IsSelf
    //      wasn't set on an early roster merge before the
    //      channel-registry path caught up).
    // Without (2), a window's own entry sometimes ends up
    // in the peer list, which then leaks into the chat
    // header (header displays self's hostname as if it
    // were the peer you were talking to).
    state.peers = peers.filter(p => !p.IsSelf && p.PeerID !== state.selfId);
    state.selfEntry = peers.find(p => p.IsSelf || p.PeerID === state.selfId) ?? null;
    renderMe();
    renderPeerList();
    renderChatHeader();
    if (state.selectedId) {
      // If the selected peer got filtered out (e.g. it
      // disconnected and was removed from the roster),
      // drop the selection so we don't keep showing a
      // stale conversation.
      if (!state.peers.find(p => p.PeerID === state.selectedId)) {
        state.selectedId = null;
      }
      renderChatHeader();
    }
  } catch (e) {
    toast(`refresh failed: ${e}`);
  }
}

async function promptAlias(peerRef: string) {
  const p = state.peers.find(p => p.PeerID === peerRef);
  const current = p?.Name || '';
  const name = window.prompt(`Alias for ${shortId(peerRef)}:`, current);
  if (name == null) return;
  const trimmed = name.trim();
  if (trimmed === '') {
    const r = await RemoveAlias(peerRef);
    if (r) toast(`remove alias: ${r}`);
  } else {
    const r = await SetAlias(peerRef, trimmed);
    if (r) toast(`set alias: ${r}`);
  }
  await refreshAll();
}

// maybeAutoAlias promotes a peer's hostname into a
// persistent alias the first time we learn it.
//
// Why: ListPeers() returns Name (alias) or Hostname
// (gossip'd via M5 roster sync). Until the first roster
// sync envelope lands after channel-ready, Hostname
// can be empty and the peer shows up as "peer bef56954".
// Once Hostname arrives we persist it as the alias so
// the friendly name survives restarts. Subsequent calls
// are no-ops (we track autoAliased to avoid spamming
// SetAlias on every peer:event).
async function maybeAutoAlias() {
  for (const p of state.peers) {
    if (p.IsSelf) continue;
    if (p.Name) continue;             // user (or we) already set an alias
    if (!p.Hostname) continue;         // no hostname yet — wait for gossip
    if (state.autoAliased.has(p.PeerID)) continue;
    state.autoAliased.add(p.PeerID);
    const r = await SetAlias(p.PeerID, p.Hostname);
    if (r) {
      // Roll back the marker so we retry next time.
      state.autoAliased.delete(p.PeerID);
      // Don't toast every retry — quietly retry on next
      // peer:event is friendlier than spamming the user.
    }
  }
}

async function promptDial() {
  if (!state.selectedId) return;
  const p = selectedPeer();
  if (!p || p.Addrs.length === 0) {
    // No known address — ask for one.
    const addr = window.prompt(`ip:port for ${shortId(state.selectedId)}:`);
    if (!addr) return;
    const r = await DialAddr(addr.trim());
    if (r) toast(`dial: ${r}`);
  } else {
    const r = await DialAddr(p.Addrs[0]);
    if (r) toast(`dial: ${r}`);
  }
}

// ----- file transfer helpers -----
//
// Pending-file card sits between the messages and the
// composer. It's only visible when state.pendingFile is
// non-null. Click ✕ to cancel and go back to text mode.
function renderPendingFile() {
  const host = document.getElementById('pending-file')!;
  if (!state.pendingFile) {
    host.innerHTML = '';
    host.classList.remove('show');
    return;
  }
  const f = state.pendingFile;
  host.classList.add('show');
  host.innerHTML = `
    <div class="pending-card">
      <span class="file-icon">📎</span>
      <div class="pending-info">
        <div class="pending-name">${escapeHtml(f.name)}</div>
        <div class="pending-meta">${fmtSize(f.size)} · ready to send</div>
      </div>
      <span class="pending-cancel" id="pending-cancel" title="cancel">✕</span>
    </div>`;
  document.getElementById('pending-cancel')!.addEventListener('click', () => {
    state.pendingFile = null;
    renderPendingFile();
  });
}

function fmtSize(n: number): string {
  if (n < 1024) return `${n} B`;
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KB`;
  if (n < 1024 * 1024 * 1024) return `${(n / 1024 / 1024).toFixed(1)} MB`;
  return `${(n / 1024 / 1024 / 1024).toFixed(2)} GB`;
}

// ----- event wiring -----
function wireEvents() {
  // Composer.
  document.getElementById('composer')!.addEventListener('submit', async (ev) => {
    ev.preventDefault();
    if (!state.selectedId) return;
    // File mode: if a file is staged, send it instead of text.
    if (state.pendingFile) {
      const f = state.pendingFile;
      state.pendingFile = null;
      renderPendingFile();
      const r = f.path
        ? await SendFile(state.selectedId, f.path)
        : await SendFileContent(state.selectedId, f.name, Array.from(f.content!));
      if (r) toast(`file send failed: ${r}`);
      return;
    }
    const input = document.getElementById('composer-input') as HTMLTextAreaElement;
    const text = input.value.trim();
    if (!text) return;
    const r = await SendText(state.selectedId, text);
    if (r) {
      toast(`send failed: ${r}`);
    } else {
      input.value = '';
    }
  });

  // Enter to send (Shift+Enter for newline).
  document.getElementById('composer-input')!.addEventListener('keydown', (ev) => {
    const ke = ev as KeyboardEvent;
    if (ke.key === 'Enter' && !ke.shiftKey) {
      ke.preventDefault();
      (document.getElementById('composer') as HTMLFormElement).requestSubmit();
    }
  });

  // Scan form.
  document.getElementById('scan-form')!.addEventListener('submit', async (ev) => {
    ev.preventDefault();
    const inp = document.getElementById('scan-input') as HTMLInputElement;
    const cidr = inp.value.trim();
    if (!cidr) return;
    const r = await Scan(cidr);
    if (r) toast(`scan: ${r}`);
    inp.value = '';
  });

  // Header action buttons.
  document.getElementById('btn-ping')!.addEventListener('click', async () => {
    if (!state.selectedId) return;
    const r = await Ping(state.selectedId);
    if (r) toast(`ping: ${r}`);
  });
  document.getElementById('btn-alias')!.addEventListener('click', () => {
    if (state.selectedId) promptAlias(state.selectedId);
  });
  document.getElementById('btn-dial')!.addEventListener('click', () => promptDial());

  // 📎 picker: open a hidden <input type=file>, read its
  // bytes via FileReader, then stage as state.pendingFile.
  // We can't reuse SendFile (which takes a path) because
  // the browser File API hides the real on-disk path. So
  // the picker route goes through SendFileContent.
  const fileInput = document.getElementById('file-input') as HTMLInputElement;
  document.getElementById('btn-attach')!.addEventListener('click', () => {
    if (!state.selectedId) return;
    fileInput.value = '';
    fileInput.click();
  });
  fileInput.addEventListener('change', async () => {
    const f = fileInput.files?.[0];
    if (!f) return;
    if (!state.selectedId) return;
    const buf = new Uint8Array(await f.arrayBuffer());
    state.pendingFile = { name: f.name, size: buf.length, content: buf };
    renderPendingFile();
  });

  // Track whether the user has scrolled away from the
  // bottom of the message list. We read this flag from
  // renderMessages() to decide whether to pin a new
  // message to the bottom or leave the user where they
  // are reading history. Bound at wire-up time so the
  // listener survives every innerHTML rewrite.
  const messagesEl = document.getElementById('messages')!;
  messagesEl.addEventListener('scroll', () => {
    state.nearBottom = isNearBottom(messagesEl);
  });

  // Live event streams from the Go side.
  EventsOn('peer:event', (_ev: any) => {
    // Cheap strategy: re-pull the list on every transition.
    // For v0.1 this is fine — peer counts are tiny. Once we
    // hit thousands of peers we'll switch to incremental.
    refreshAll().then(() => maybeAutoAlias());
  });

  EventsOn('message:event', (m: node.Message) => {
    if (!m || !m.PeerID) return;
    // Message arrives; append to the right peer's history.
    // We don't know the direction relative to the local
    // conversation — pull History to keep things in sync.
    const list = state.history.get(m.PeerID) || [];
    list.push(m);
    state.history.set(m.PeerID, list);
    if (state.selectedId === m.PeerID) {
      renderMessages();
    } else if (m.Direction === 'in') {
      // Incoming message to a peer the user isn't currently
      // looking at — bump the unread badge in the sidebar.
      const n = (state.unreadCount.get(m.PeerID) || 0) + 1;
      state.unreadCount.set(m.PeerID, n);
      renderPeerList();
    }
  });
}

// ----- bootstrap -----
async function bootstrap() {
  mount();
  wireEvents();
  await refreshAll();
  // First sweep: any peer we already see with a hostname
  // but no alias gets auto-aliased now, so the sidebar
  // is friendly from the very first paint instead of
  // waiting for the next peer:event transition.
  await maybeAutoAlias();

  // Wails drag-and-drop: useDropTarget=true tells Wails
  // to only fire on elements that carry the CSS variable
  // --wails-drop-target: drop (we set it on .composer).
  // Wails adds a wails-drop-target-active class while a
  // file is being dragged over the drop target so we can
  // highlight it.
  OnFileDrop((_x, _y, paths) => {
    if (!state.selectedId) {
      toast('select a peer first');
      return;
    }
    if (!paths || paths.length === 0) return;
    const p = paths[0];
    const name = p.split(/[\\/]/).pop() || p;
    // We don't have the file size from the path alone on
    // the TS side without an extra fetch, but the Go side
    // SendFile will stat it. Stage with size=0; the card
    // shows the name and the user can hit send.
    state.pendingFile = { name, size: 0, path: p };
    renderPendingFile();
  }, true);
}

bootstrap();