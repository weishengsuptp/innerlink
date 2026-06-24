// innerlink frontend — vanilla TypeScript, no framework.
//
// Layout & visuals: v0.1 UI design mockup
// (D:\mavis-tmp\innerlink-ui-mockup.html). All
// user-facing text is Chinese; technical terms (peer,
// alias, scan, SM4-GCM) stay English.
//
// Talks to the Go side via the Wails-generated bindings
// in ../wailsjs/go/app/App and listens for runtime
// events ("peer:event", "message:event") emitted from
// app/app.go.
//
// Features this file implements:
//   - sidebar: peer list with 3-state dot (online/recent/
//     offline), relative time, alias + IP in meta,
//     unread badge.
//   - chat header: 38px avatar with first character,
//     peer name + alias + peerID + IP:PORT, icon buttons
//     (ping / 文件 / 更多).
//   - messages: 28px avatar on each message, day-divider
//     when day changes, centered system hint about E2E
//     encryption, file cards with size + "SM4-GCM 加密".
//   - composer: 📎 icon-btn toolbar + textarea + 发送
//     button. Drag-and-drop is scoped to the composer
//     (Wails OnFileDrop with useDropTarget=true).
//   - file transfer: drop -> SendFile (real path);
//     📎 picker -> FileReader -> SendFileContent (bytes).
//   - unread badge: bumps on incoming messages to a
//     non-selected peer, clears on selection.

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
  selfEntry: node.PeerInfo | null;
  peers: node.PeerInfo[];             // peers minus self
  selectedId: string | null;          // peer hex ID of open conversation
  history: Map<string, node.Message[]>; // peer hex ID -> msgs
  aliases: Map<string, string>;       // peer hex ID -> alias
  // nearBottom: are we within ~60px of the message list
  // bottom? Used by renderMessages to decide whether to
  // auto-stick to bottom on new incoming messages.
  nearBottom: boolean;
  // autoAliased: peer hex IDs we've already auto-aliased
  // from their hostname (avoids spamming SetAlias on
  // every peer:event when nothing changed).
  autoAliased: Set<string>;
  // pendingFile: a file the user staged via 📎 picker or
  // drag-and-drop, not yet sent. When set, the composer
  // shows a file card and submit routes to SendFile /
  // SendFileContent instead of SendText.
  //   - path: real on-disk path (drag-drop via Wails)
  //   - content: bytes from FileReader (📎 picker, since
  //     the browser File API hides the real path)
  pendingFile: { name: string; size: number; path?: string; content?: Uint8Array } | null;
  // unreadCount: peer hex ID -> number of incoming
  // messages not yet seen. Reset to 0 in selectPeer.
  unreadCount: Map<string, number>;
}

const state: UIState = {
  selfId: '',
  selfEntry: null,
  peers: [],
  selectedId: null,
  history: new Map(),
  aliases: new Map(),
  nearBottom: true,
  autoAliased: new Set(),
  pendingFile: null,
  unreadCount: new Map(),
};

// ----- small helpers -----
function shortId(id: string): string {
  return id ? id.slice(0, 8) : '';
}

function escapeHtml(s: string): string {
  return s.replace(/[&<>"']/g, ch => ({
    '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;',
  }[ch]!));
}

function fmtTime(ts: any): string {
  // Wails marshals time.Time as RFC3339 string into the
  // binding. Fall back to "now" if the parse fails.
  const d = ts ? new Date(ts) : new Date();
  if (isNaN(d.getTime())) return '';
  return d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });
}

function fmtSize(n: number): string {
  if (n < 1024) return `${n} B`;
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KB`;
  if (n < 1024 * 1024 * 1024) return `${(n / 1024 / 1024).toFixed(1)} MiB`;
  return `${(n / 1024 / 1024 / 1024).toFixed(2)} GiB`;
}

// avatarChar picks the first character for the avatar
// circle. Chinese names use the first character;
// everything else falls back to the first 1-2 letters
// uppercased. Falls back to "?" for empty.
function avatarChar(name: string): string {
  if (!name) return '?';
  // CJK ideographs: first code point
  const c = name.codePointAt(0);
  if (c && c >= 0x4e00 && c <= 0x9fff) return name.slice(0, 1);
  return name.slice(0, 2).toUpperCase();
}

// peerState classifies a peer into one of three states
// for the sidebar dot:
//   - online:  p.Online === true
//   - recent:  not online, but LastSeen within 30 minutes
//   - offline: everything else
function peerState(p: node.PeerInfo): 'online' | 'recent' | 'offline' {
  if (p.Online) return 'online';
  if (!p.LastSeen) return 'offline';
  const seen = new Date(p.LastSeen).getTime();
  if (isNaN(seen)) return 'offline';
  if (Date.now() - seen < 30 * 60 * 1000) return 'recent';
  return 'offline';
}

// timeAgo formats a timestamp as a Chinese relative
// phrase. Online peers get "在线"; recent peers get
// "刚刚" / "5 分钟前" / "30 分钟前" / "X 小时前";
// older timestamps get days / months / years.
function timeAgo(ts: any, online: boolean): string {
  if (online) return '在线';
  if (!ts) return '';
  const t = new Date(ts).getTime();
  if (isNaN(t)) return '';
  const diff = Date.now() - t;
  if (diff < 60 * 1000) return '刚刚';
  if (diff < 60 * 60 * 1000) return `${Math.floor(diff / 60000)} 分钟前`;
  if (diff < 24 * 60 * 60 * 1000) return `${Math.floor(diff / 3600000)} 小时前`;
  if (diff < 30 * 24 * 60 * 60 * 1000) return `${Math.floor(diff / 86400000)} 天前`;
  if (diff < 365 * 24 * 60 * 60 * 1000) return `${Math.floor(diff / 2592000000)} 月前`;
  return `${Math.floor(diff / 31536000000)} 年前`;
}

// dayLabel returns the Chinese day header for a message
// timestamp: "今天" for the current day, "昨天" for
// yesterday, otherwise "MM-DD" (or "YYYY-MM-DD" if not
// the same year as today).
function sameDay(a: any, b: any): boolean {
  if (!a || !b) return false;
  const da = new Date(a), db = new Date(b);
  if (isNaN(da.getTime()) || isNaN(db.getTime())) return false;
  return da.getFullYear() === db.getFullYear() &&
         da.getMonth() === db.getMonth() &&
         da.getDate() === db.getDate();
}

function dayLabel(ts: any): string {
  const d = ts ? new Date(ts) : new Date();
  if (isNaN(d.getTime())) return '';
  const now = new Date();
  if (sameDay(d, now)) return '今天';
  const yest = new Date(now); yest.setDate(now.getDate() - 1);
  if (sameDay(d, yest)) return '昨天';
  if (d.getFullYear() === now.getFullYear()) {
    return `${String(d.getMonth() + 1).padStart(2, '0')}-${String(d.getDate()).padStart(2, '0')}`;
  }
  return `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, '0')}-${String(d.getDate()).padStart(2, '0')}`;
}

function peerDisplay(p: node.PeerInfo): string {
  return p.Name || (p.Hostname ? p.Hostname : `peer ${shortId(p.PeerID)}`);
}

function selectedPeer(): node.PeerInfo | null {
  if (!state.selectedId) return null;
  return state.peers.find(p => p.PeerID === state.selectedId) ?? null;
}

function isNearBottom(el: HTMLElement): boolean {
  return (el.scrollHeight - el.scrollTop - el.clientHeight) < 60;
}

function toast(msg: string) {
  const t = document.getElementById('toast')!;
  t.textContent = msg;
  t.classList.add('show');
  setTimeout(() => t.classList.remove('show'), 2400);
}

// ----- DOM injection (one-shot at startup) -----
function mount() {
  document.querySelector('#app')!.innerHTML = `
    <div class="app-header">
      <div class="brand">
        <span class="brand-mark"></span>
        <span class="brand-name">innerlink</span>
        <span class="brand-tag">v0.1</span>
      </div>
      <div class="brand-hint">国密 P2P · 纯客户端 · 局域网</div>
    </div>
    <div class="app">
      <aside class="sidebar">
        <div class="me">
          <div class="me-label">我</div>
          <div class="me-name" id="me-name">—</div>
          <div class="me-host" id="me-host">本机 · —</div>
          <div class="me-status"><span class="led"></span><span id="me-status">启动中…</span></div>
        </div>
        <div class="sidebar-header">
          <span class="sidebar-title">Peers</span>
          <span class="sidebar-count"><span id="peer-online">0</span> / <span id="peer-count">0</span></span>
        </div>
        <div class="peer-list" id="peer-list"></div>
        <div class="sidebar-footer">
          <button class="btn-mini" id="btn-alias-create" title="给 IP 起名字">
            <svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.2"><path d="M12 5v14M5 12h14"/></svg>
            alias
          </button>
          <button class="btn-mini" id="btn-scan" title="扫描 IP 段">
            <svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.2"><circle cx="11" cy="11" r="7"/><path d="m21 21-4.3-4.3"/></svg>
            scan
          </button>
        </div>
      </aside>
      <section class="chat">
        <div class="chat-header">
          <div class="avatar" id="chat-avatar">?</div>
          <div class="peer-title">
            <div class="name" id="chat-name">未选择 peer</div>
            <div class="sub" id="chat-id">—</div>
          </div>
          <div class="header-actions">
            <button class="icon-btn" id="btn-ping" title="ping" disabled>
              <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M22 12h-4l-3 9L9 3l-3 9H2"/></svg>
            </button>
            <button class="icon-btn" id="btn-dial" title="发文件" disabled>
              <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="m21.44 11.05-9.19 9.19a6 6 0 0 1-8.49-8.49l9.19-9.19a4 4 0 0 1 5.66 5.66l-9.2 9.19a2 2 0 0 1-2.83-2.83l8.49-8.48"/></svg>
            </button>
            <button class="icon-btn" id="btn-more" title="更多" disabled>
              <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><circle cx="12" cy="5" r="1"/><circle cx="12" cy="12" r="1"/><circle cx="12" cy="19" r="1"/></svg>
            </button>
          </div>
        </div>
        <div class="messages" id="messages">
          <div class="empty" id="empty-state">
            <div>
              <div class="ico">💬</div>
              <h3>选一个 peer 开始聊天</h3>
              <p>左侧列表选人，或点 <code>+ alias</code> 给 IP 起个名字</p>
            </div>
          </div>
        </div>
        <div class="pending-file" id="pending-file"></div>
        <form class="composer" id="composer">
          <div class="composer-toolbar">
            <button type="button" class="icon-btn" id="btn-attach" title="发文件" disabled>
              <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="m21.44 11.05-9.19 9.19a6 6 0 0 1-8.49-8.49l9.19-9.19a4 4 0 0 1 5.66 5.66l-9.2 9.19a2 2 0 0 1-2.83-2.83l8.49-8.48"/></svg>
            </button>
          </div>
          <div class="composer-input">
            <textarea id="composer-input" placeholder="先选一个 peer…" disabled></textarea>
            <button class="send-btn" type="submit" id="composer-send" disabled>
              发送
              <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.4"><path d="m22 2-7 20-4-9-9-4Z"/><path d="M22 2 11 13"/></svg>
            </button>
          </div>
          <div class="drop-hint" id="drop-hint">松开以附加文件</div>
          <input type="file" id="file-input" style="display:none" />
        </form>
      </section>
    </div>
    <div class="toast" id="toast"></div>
  `;
}

// ----- render functions -----
function renderMe() {
  const self = state.selfEntry;
  const name = document.getElementById('me-name')!;
  const host = document.getElementById('me-host')!;
  const status = document.getElementById('me-status')!;
  if (!self) {
    name.textContent = '—';
    host.textContent = '本机 · —';
    status.textContent = '启动中…';
    return;
  }
  name.textContent = self.Name || self.Hostname || shortId(self.PeerID);
  const ip = self.Addrs[0]?.split(':')[0] || '—';
  host.textContent = `本机 · ${ip} · ${shortId(self.PeerID)}`;
  status.textContent = self.Online ? '在线 · UDP 发现已开启' : '启动中…';
}

function renderPeerList() {
  const list = document.getElementById('peer-list')!;
  // Sort: online first, then recent, then offline; within
  // each group, most recently seen first.
  const order = { online: 0, recent: 1, offline: 2 } as const;
  const sorted = [...state.peers].sort((a, b) => {
    const sa = order[peerState(a)], sb = order[peerState(b)];
    if (sa !== sb) return sa - sb;
    const at = a.LastSeen ? new Date(a.LastSeen).getTime() : 0;
    const bt = b.LastSeen ? new Date(b.LastSeen).getTime() : 0;
    return bt - at;
  });

  list.innerHTML = sorted.map(p => {
    const st = peerState(p);
    const unread = state.unreadCount.get(p.PeerID) || 0;
    const name = peerDisplay(p);
    // meta line: <alias> · IP  OR  peerID · IP
    let meta = '';
    if (p.Name && p.Hostname) {
      meta = `<span class="alias">${escapeHtml(p.Hostname)}</span> · ${escapeHtml(p.Addrs[0]?.split(':')[0] || '')}`;
    } else if (p.Name) {
      meta = `<span class="alias">${escapeHtml(p.Name)}</span>`;
    } else if (p.Hostname) {
      meta = `<span class="alias">${escapeHtml(p.Hostname)}</span> · ${escapeHtml(p.Addrs[0]?.split(':')[0] || '')}`;
    } else {
      meta = `${shortId(p.PeerID)} · ${escapeHtml(p.Addrs[0]?.split(':')[0] || '')}`;
    }
    return `
      <div class="peer ${st === 'online' ? 'online' : ''} ${p.PeerID === state.selectedId ? 'active' : ''}" data-peer="${p.PeerID}">
        <span class="peer-dot ${st}"></span>
        <div class="peer-info">
          <div class="peer-name">${escapeHtml(name)}</div>
          <div class="peer-meta">${meta}</div>
        </div>
        <span class="badge ${unread > 0 ? 'show' : ''}">${unread > 0 ? unread : ''}</span>
        <span class="peer-time">${escapeHtml(timeAgo(p.LastSeen, p.Online))}</span>
      </div>
    `;
  }).join('');

  list.querySelectorAll<HTMLElement>('.peer').forEach(el => {
    el.addEventListener('click', () => {
      const peerId = el.getAttribute('data-peer')!;
      selectPeer(peerId);
    });
  });

  // Update the "N / M" counter (online / total).
  const online = state.peers.filter(p => p.Online).length;
  document.getElementById('peer-online')!.textContent = String(online);
  document.getElementById('peer-count')!.textContent = String(state.peers.length);
}

function renderChatHeader() {
  const p = selectedPeer();
  const avatar = document.getElementById('chat-avatar')!;
  const name = document.getElementById('chat-name')!;
  const sub = document.getElementById('chat-id')!;
  const pingBtn = document.getElementById('btn-ping') as HTMLButtonElement;
  const dialBtn = document.getElementById('btn-dial') as HTMLButtonElement;
  const moreBtn = document.getElementById('btn-more') as HTMLButtonElement;
  const attachBtn = document.getElementById('btn-attach') as HTMLButtonElement;
  const input = document.getElementById('composer-input') as HTMLTextAreaElement;
  const send = document.getElementById('composer-send') as HTMLButtonElement;

  if (!p) {
    avatar.textContent = '?';
    name.textContent = '未选择 peer';
    sub.textContent = '—';
    pingBtn.disabled = true;
    dialBtn.disabled = true;
    moreBtn.disabled = true;
    attachBtn.disabled = true;
    input.disabled = true;
    send.disabled = true;
    input.placeholder = '先选一个 peer…';
    return;
  }
  avatar.textContent = avatarChar(peerDisplay(p));
  name.textContent = peerDisplay(p);
  // sub: alias · peerID · ip:port (or just what's known)
  const subParts: string[] = [];
  if (p.Name) subParts.push(p.Name);
  subParts.push(shortId(p.PeerID));
  if (p.Addrs[0]) subParts.push(p.Addrs[0]);
  sub.textContent = subParts.join(' · ');
  pingBtn.disabled = !p.Online;
  dialBtn.disabled = !p.Online;
  moreBtn.disabled = false;
  attachBtn.disabled = false;
  input.disabled = false;
  send.disabled = false;
  input.placeholder = `发到 ${peerDisplay(p)}…（Enter 发送，Shift+Enter 换行）`;
}

function renderEmpty() {
  const el = document.getElementById('messages')!;
  el.innerHTML = `
    <div class="empty">
      <div>
        <div class="ico">💬</div>
        <h3>${state.selectedId ? '还没有消息' : '选一个 peer 开始聊天'}</h3>
        <p>${state.selectedId ? '说点啥 👋' : '左侧列表选人，或点 <code>+ alias</code> 给 IP 起个名字'}</p>
      </div>
    </div>
  `;
}

function renderMessages() {
  const el = document.getElementById('messages')!;
  const wasNearBottom = isNearBottom(el);
  if (!state.selectedId) { renderEmpty(); return; }
  const msgs = state.history.get(state.selectedId) ?? [];
  if (msgs.length === 0) { renderEmpty(); return; }

  const parts: string[] = [];
  let lastDay = '';
  for (const m of msgs) {
    const day = dayLabel(m.Timestamp);
    if (day && day !== lastDay) {
      parts.push(`<div class="day-divider"><span>${escapeHtml(day)}</span></div>`);
      lastDay = day;
    }
    parts.push(renderMessage(m, state.selectedId));
  }
  // First-load system hint about E2E encryption.
  parts.push(`<div class="msg system"><div class="bubble">—— 端到端加密，每条消息独立会话密钥 ——</div></div>`);
  el.innerHTML = parts.join('');
  if (wasNearBottom) el.scrollTop = el.scrollHeight;
  state.nearBottom = isNearBottom(el);
}

function renderMessage(m: node.Message, peerId: string): string {
  const isOut = m.Direction === 'out';
  const sideClass = isOut ? 'self' : '';
  const ts = fmtTime(m.Timestamp);
  const peer = state.peers.find(p => p.PeerID === peerId);
  const avChar = isOut ? '我' : avatarChar(peer ? peerDisplay(peer) : '');

  // File message: Body has prefix "file:" (used by core
  // filetransfer to publish a "file" message to the chat
  // log so it shows up alongside text).
  if (m.Body.startsWith('file:')) {
    const name = m.Body.slice('file:'.length).split('|')[0];
    const size = m.Body.split('|')[1] || '';
    return `
      <div class="msg ${sideClass}">
        <div class="av">${escapeHtml(avChar)}</div>
        <div>
          <div class="bubble">
            <div class="file-msg">
              <div class="file-icon">
                <svg width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M14 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V8z"/><path d="M14 2v6h6"/></svg>
              </div>
              <div>
                <div class="file-name">${escapeHtml(name)}</div>
                <div class="file-meta">${size ? escapeHtml(size) + ' · ' : ''}SM4-GCM 加密</div>
              </div>
            </div>
          </div>
          <div class="ts">${ts}</div>
        </div>
      </div>
    `;
  }

  return `
    <div class="msg ${sideClass}">
      <div class="av">${escapeHtml(avChar)}</div>
      <div>
        <div class="bubble">${escapeHtml(m.Body)}</div>
        <div class="ts">${ts}</div>
      </div>
    </div>
  `;
}

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
      <div class="file-icon">
        <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M14 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V8z"/><path d="M14 2v6h6"/></svg>
      </div>
      <div class="pending-info">
        <div class="pending-name">${escapeHtml(f.name)}</div>
        <div class="pending-meta">${f.size > 0 ? fmtSize(f.size) : '文件'} · 准备发送</div>
      </div>
      <span class="pending-cancel" id="pending-cancel" title="取消">✕</span>
    </div>
  `;
  document.getElementById('pending-cancel')!.addEventListener('click', () => {
    state.pendingFile = null;
    renderPendingFile();
  });
}

// ----- actions -----
async function selectPeer(peerId: string) {
  state.selectedId = peerId;
  // Opening a conversation clears its unread badge.
  state.unreadCount.set(peerId, 0);
  try {
    const h = await History(peerId);
    state.history.set(peerId, (h as node.Message[]) || []);
  } catch (e) {
    state.history.set(peerId, []);
    toast(`读取历史失败: ${e}`);
  }
  renderPeerList();
  renderChatHeader();
  renderMessages();
  renderPendingFile();
  const input = document.getElementById('composer-input') as HTMLTextAreaElement;
  input.focus();
}

async function refreshAll() {
  try {
    const peers = (await ListPeers()) as node.PeerInfo[];
    state.selfId = await SelfPeerID();
    // Filter self defensively (see comment in innerlink
    // release notes; without the PeerID fallback, self
    // sometimes leaks into the peer list and shows up
    // in the chat header).
    state.peers = peers.filter(p => !p.IsSelf && p.PeerID !== state.selfId);
    state.selfEntry = peers.find(p => p.IsSelf || p.PeerID === state.selfId) ?? null;
    renderMe();
    renderPeerList();
    renderChatHeader();
    if (state.selectedId && !state.peers.find(p => p.PeerID === state.selectedId)) {
      state.selectedId = null;
      renderChatHeader();
    }
  } catch (e) {
    toast(`刷新失败: ${e}`);
  }
}

async function promptAlias(peerRef: string) {
  const p = state.peers.find(p => p.PeerID === peerRef);
  const current = p?.Name || '';
  const name = window.prompt(`给 ${shortId(peerRef)} 起个名字:`, current);
  if (name == null) return;
  const trimmed = name.trim();
  if (trimmed === '') {
    const r = await RemoveAlias(peerRef);
    if (r) toast(`删除别名: ${r}`);
  } else {
    const r = await SetAlias(peerRef, trimmed);
    if (r) toast(`设置别名: ${r}`);
  }
  await refreshAll();
}

// maybeAutoAlias promotes a peer's hostname into a
// persistent alias the first time we learn it. Survives
// restarts. We track autoAliased to avoid spamming
// SetAlias on every peer:event.
async function maybeAutoAlias() {
  for (const p of state.peers) {
    if (p.IsSelf) continue;
    if (p.Name) continue;
    if (!p.Hostname) continue;
    if (state.autoAliased.has(p.PeerID)) continue;
    state.autoAliased.add(p.PeerID);
    const r = await SetAlias(p.PeerID, p.Hostname);
    if (r) {
      // Roll back so we retry next time.
      state.autoAliased.delete(p.PeerID);
    }
  }
}

async function promptScan() {
  const cidr = window.prompt('扫描 IP 段 (例如 192.168.1.0/24):', '');
  if (!cidr) return;
  const r = await Scan(cidr.trim());
  if (r) toast(`扫描: ${r}`);
}

async function promptDial() {
  if (!state.selectedId) return;
  const p = selectedPeer();
  if (!p || p.Addrs.length === 0) {
    const addr = window.prompt(`手动填 ${shortId(state.selectedId)} 的 ip:port:`);
    if (!addr) return;
    const r = await DialAddr(addr.trim());
    if (r) toast(`连接: ${r}`);
  } else {
    const r = await DialAddr(p.Addrs[0]);
    if (r) toast(`连接: ${r}`);
  }
}

function promptMore() {
  // The "more" icon-btn opens a small action sheet:
  // rename this peer, clear chat history, etc. For v0.1
  // we just chain to promptAlias (rename) + a confirm
  // for clearing history.
  if (!state.selectedId) return;
  const choice = window.prompt(
    '操作: 1=重命名  2=清空聊天记录',
    '1',
  );
  if (choice === '1') {
    promptAlias(state.selectedId);
  } else if (choice === '2') {
    state.history.set(state.selectedId, []);
    renderMessages();
    toast('已清空当前聊天记录（仅本机显示）');
  }
}

// ----- file transfer helpers -----
function setPendingFile(f: UIState['pendingFile']) {
  state.pendingFile = f;
  renderPendingFile();
}

// ----- event wiring -----
function wireEvents() {
  // Composer submit: file mode first, then text mode.
  document.getElementById('composer')!.addEventListener('submit', async (ev) => {
    ev.preventDefault();
    if (!state.selectedId) return;
    if (state.pendingFile) {
      const f = state.pendingFile;
      setPendingFile(null);
      const r = f.path
        ? await SendFile(state.selectedId, f.path)
        : await SendFileContent(state.selectedId, f.name, Array.from(f.content!));
      if (r) toast(`发文件失败: ${r}`);
      return;
    }
    const input = document.getElementById('composer-input') as HTMLTextAreaElement;
    const text = input.value.trim();
    if (!text) return;
    const r = await SendText(state.selectedId, text);
    if (r) {
      toast(`发送失败: ${r}`);
    } else {
      input.value = '';
    }
  });

  // Enter to send, Shift+Enter for newline.
  document.getElementById('composer-input')!.addEventListener('keydown', (ev) => {
    const ke = ev as KeyboardEvent;
    if (ke.key === 'Enter' && !ke.shiftKey) {
      ke.preventDefault();
      (document.getElementById('composer') as HTMLFormElement).requestSubmit();
    }
  });

  // Header icon buttons.
  document.getElementById('btn-ping')!.addEventListener('click', async () => {
    if (!state.selectedId) return;
    const r = await Ping(state.selectedId);
    if (r) toast(`ping: ${r}`);
  });
  document.getElementById('btn-dial')!.addEventListener('click', () => promptDial());
  document.getElementById('btn-more')!.addEventListener('click', () => promptMore());

  // Sidebar footer mini buttons.
  document.getElementById('btn-alias-create')!.addEventListener('click', () => {
    if (state.selectedId) promptAlias(state.selectedId);
    else toast('先选一个 peer');
  });
  document.getElementById('btn-scan')!.addEventListener('click', () => promptScan());

  // 📎 picker: open a hidden <input type=file>, read its
  // bytes via FileReader, then stage as state.pendingFile.
  // We can't reuse SendFile (which takes a path) because
  // the browser File API hides the real on-disk path on
  // modern engines; the picker route goes through
  // SendFileContent.
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
    setPendingFile({ name: f.name, size: buf.length, content: buf });
  });

  // Track scroll position so renderMessages can decide
  // whether to stick to the bottom (chat-style) or leave
  // the user where they are reading history.
  const messagesEl = document.getElementById('messages')!;
  messagesEl.addEventListener('scroll', () => {
    state.nearBottom = isNearBottom(messagesEl);
  });

  // Live event streams from Go.
  EventsOn('peer:event', (_ev: any) => {
    refreshAll().then(() => maybeAutoAlias());
  });
  EventsOn('message:event', (m: node.Message) => {
    if (!m || !m.PeerID) return;
    const list = state.history.get(m.PeerID) || [];
    list.push(m);
    state.history.set(m.PeerID, list);
    if (state.selectedId === m.PeerID) {
      renderMessages();
    } else if (m.Direction === 'in') {
      // Incoming message to a peer we aren't looking at:
      // bump the unread badge.
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
  await maybeAutoAlias();

  // Wails drag-and-drop: useDropTarget=true tells Wails
  // to only fire OnFileDrop on elements that carry the
  // CSS variable --wails-drop-target: drop (we set it on
  // .composer). Wails adds a wails-drop-target-active
  // class while a file is being dragged over the drop
  // target so we can highlight it.
  OnFileDrop((_x, _y, paths) => {
    if (!state.selectedId) {
      toast('先选一个 peer');
      return;
    }
    if (!paths || paths.length === 0) return;
    const p = paths[0];
    const name = p.split(/[\\/]/).pop() || p;
    // Wails gives us a real OS path. We don't have the
    // byte size in TS without an extra fetch, but the Go
    // side SendFile will stat the file. Stage with
    // size=0; the card shows the name and the user hits
    // send.
    setPendingFile({ name, size: 0, path: p });
  }, true);
}

bootstrap();
