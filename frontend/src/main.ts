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
  OpenPath,
  Ping,
  ReceivedFilePath,
  RemoveAlias,
  RevealInFolder,
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
// older timestamps get days / months / calendar-year
// diff. We use calendar-year diff (now.getFullYear() -
// t.getFullYear()) rather than elapsed time so the
// output matches user intuition: an entry from 2024
// is "2 年前" in 2026, not "1 年前" (elapsed 18 months
// floors to 1 year, but the year boundary crossed).
function timeAgo(ts: any, online: boolean): string {
  if (online) return '在线';
  if (!ts) return '';
  const t = new Date(ts);
  if (isNaN(t.getTime())) return '';
  const now = new Date();
  const diff = now.getTime() - t.getTime();
  if (diff < 0) return ''; // future timestamp, skip
  if (diff < 60 * 1000) return '刚刚';
  if (diff < 60 * 60 * 1000) return `${Math.floor(diff / 60000)} 分钟前`;
  if (diff < 24 * 60 * 60 * 1000) return `${Math.floor(diff / 3600000)} 小时前`;
  if (diff < 30 * 24 * 60 * 60 * 1000) {
    const d = Math.floor(diff / 86400000);
    return d === 1 ? '昨天' : `${d} 天前`;
  }
  // Calendar-aware: 30+ days uses year/month/day diff
  // rather than raw elapsed milliseconds, so a Feb 2024
  // entry in Jun 2026 reads "2 年前", not "2 年 4 个月前".
  const yearDiff = now.getFullYear() - t.getFullYear();
  if (yearDiff >= 1) return `${yearDiff} 年前`;
  // Same year but 30+ days ago: count months.
  const monthDiff = now.getMonth() - t.getMonth();
  if (monthDiff >= 1) return `${monthDiff} 个月前`;
  return '1 个月前';
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
  // Mockup shows "在线 · UDP 发现已开启" once we're up.
  // self.Online is too strict (waits for the first peer
  // announcement to round-trip), so fall back to "have
  // any bound address" -> "在线" (which is what
  // listening-on-UDP/TCP actually means).
  const listening = (self.Addrs?.length ?? 0) > 0;
  status.textContent = (self.Online || listening) ? '在线 · UDP 发现已开启' : '启动中…';
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

  // File message: Body has prefix "file://" (per core
  // pkg/node/messages.go SendFile + OnComplete), with an
  // optional "|size" suffix for the meta line.
  if (m.Body.startsWith('file://')) {
    const rest = m.Body.slice('file://'.length);
    const pipe = rest.indexOf('|');
    const name = pipe >= 0 ? rest.slice(0, pipe) : rest;
    const size = pipe >= 0 ? rest.slice(pipe + 1) : '';
    // LocalPath is set by core for live messages; for
    // history reloads (chat.enc) LocalPath is empty but
    // the file may still be in <data-dir>/received/.
    // The click handler resolves the actual path.
    const dataPath = m.LocalPath || '';
    return `
      <div class="msg ${sideClass}">
        <div class="av">${escapeHtml(avChar)}</div>
        <div>
          <div class="bubble file-bubble" data-file-name="${escapeHtml(name)}" data-file-path="${escapeHtml(dataPath)}" title="双击打开 · 右键更多">
            <div class="file-msg">
              <div class="file-icon">
                <svg width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M14 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V8z"/><path d="M14 2v6h6"/></svg>
              </div>
              <div class="file-info">
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
//
// Files are sent on selection (📎 picker) or drop, not
// staged into a pending card. The core publishes a
// "file://" chat message on completion, so the receiver
// sees the file bubble in the conversation and the sender
// sees their own outgoing file bubble — no need for an
// intermediate "ready to send" UI step.
//
// sendPickerFile reads the file's bytes via FileReader
// and hands them to SendFileContent. We can't reuse
// SendFile (which takes a path) because the browser File
// API hides the real on-disk path on modern engines.
async function sendPickerFile(file: File) {
  if (!state.selectedId) return;
  const buf = new Uint8Array(await file.arrayBuffer());
  const r = await SendFileContent(state.selectedId, file.name, Array.from(buf));
  if (r) toast(`发文件失败: ${r}`);
}

// resolveFilePath returns the on-disk path for a file
// message. Live messages carry LocalPath; history
// reloads don't, so we fall back to the core's
// ReceivedFilePath(<name>) which scans <data-dir>/received/.
// Returns "" if the file can't be located.
async function resolveFilePath(name: string, dataPath: string): Promise<string> {
  if (dataPath) return dataPath;
  const p = await ReceivedFilePath(name);
  return p || '';
}

async function openFileMessage(name: string, dataPath: string) {
  // Picker route: dataPath is empty because the browser
  // File API hides the real on-disk path. The user
  // knows where they picked the file from; tell them
  // that instead of "file not found".
  if (!dataPath) {
    toast('此文件来自文件选择器，本地路径不可用；请用拖拽重发');
    return;
  }
  const path = await resolveFilePath(name, dataPath);
  if (!path) {
    toast('找不到文件: ' + name);
    return;
  }
  const r = await OpenPath(path);
  if (r) toast('打开失败: ' + r);
}

async function revealFileMessage(name: string, dataPath: string) {
  if (!dataPath) {
    toast('此文件来自文件选择器，本地路径不可用；请用拖拽重发');
    return;
  }
  const path = await resolveFilePath(name, dataPath);
  if (!path) {
    toast('找不到文件: ' + name);
    return;
  }
  const r = await RevealInFolder(path);
  if (r) toast('打开文件夹失败: ' + r);
}

// showFileContextMenu renders a small action menu at the
// given screen coords. Used by the right-click handler on
// .file-bubble. We re-use the same menu shape for any
// file message (incoming or outgoing) — the actions
// resolve the path the same way either direction.
function showFileContextMenu(x: number, y: number, name: string, dataPath: string) {
  hideFileContextMenu();
  const menu = document.createElement('div');
  menu.className = 'ctx-menu';
  menu.id = 'file-ctx-menu';
  menu.innerHTML = `
    <div class="ctx-item" data-act="open">打开文件</div>
    <div class="ctx-item" data-act="reveal">打开文件所在文件夹</div>
    <div class="ctx-sep"></div>
    <div class="ctx-item ctx-info" data-act="copy">复制文件名: ${escapeHtml(name)}</div>
  `;
  menu.style.left = x + 'px';
  menu.style.top = y + 'px';
  document.body.appendChild(menu);
  menu.querySelector<HTMLElement>('[data-act="open"]')!.addEventListener('click', () => {
    hideFileContextMenu();
    void openFileMessage(name, dataPath);
  });
  menu.querySelector<HTMLElement>('[data-act="reveal"]')!.addEventListener('click', () => {
    hideFileContextMenu();
    void revealFileMessage(name, dataPath);
  });
  menu.querySelector<HTMLElement>('[data-act="copy"]')!.addEventListener('click', () => {
    hideFileContextMenu();
    void navigator.clipboard.writeText(name);
  });
}

function hideFileContextMenu() {
  document.getElementById('file-ctx-menu')?.remove();
}

// ----- event wiring -----
function wireEvents() {
  // Composer submit: text only. File transfer is wired
  // to 📎 click and drag-drop (auto-send on selection).
  document.getElementById('composer')!.addEventListener('submit', async (ev) => {
    ev.preventDefault();
    if (!state.selectedId) return;
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
  // bytes via FileReader, then call SendFileContent
  // immediately. We can't reuse SendFile (which takes a
  // path) because the browser File API hides the real
  // on-disk path on modern engines; the picker route
  // always goes through SendFileContent.
  const fileInput = document.getElementById('file-input') as HTMLInputElement;
  document.getElementById('btn-attach')!.addEventListener('click', () => {
    if (!state.selectedId) return;
    fileInput.value = '';
    fileInput.click();
  });
  fileInput.addEventListener('change', async () => {
    const f = fileInput.files?.[0];
    if (!f) return;
    await sendPickerFile(f);
  });

  // Track scroll position so renderMessages can decide
  // whether to stick to the bottom (chat-style) or leave
  // the user where they are reading history.
  const messagesEl = document.getElementById('messages')!;
  messagesEl.addEventListener('scroll', () => {
    state.nearBottom = isNearBottom(messagesEl);
  });

  // File message interactions: double-click opens with
  // the OS default app, right-click shows an action menu
  // (open / reveal in folder / copy name).
  //
  // We bind on the document (not on messagesEl) so the
  // handlers survive every innerHTML rewrite from
  // renderMessages AND so we don't depend on the event
  // bubbling through messagesEl specifically — on the
  // receiver the messages container can be re-mounted in
  // a way that breaks the older delegation. Document-
  // level delegation with closest('.file-bubble') is the
  // most robust option.
  document.addEventListener('dblclick', (ev) => {
    const t = ev.target as HTMLElement;
    if (!t) return;
    const bubble = t.closest('.file-bubble') as HTMLElement | null;
    if (!bubble) return;
    const name = bubble.getAttribute('data-file-name') || '';
    const dataPath = bubble.getAttribute('data-file-path') || '';
    void openFileMessage(name, dataPath);
  });
  document.addEventListener('contextmenu', (ev) => {
    const t = ev.target as HTMLElement;
    if (!t) return;
    const bubble = t.closest('.file-bubble') as HTMLElement | null;
    if (!bubble) return;
    ev.preventDefault();
    const name = bubble.getAttribute('data-file-name') || '';
    const dataPath = bubble.getAttribute('data-file-path') || '';
    showFileContextMenu(ev.clientX, ev.clientY, name, dataPath);
  });
  // Click outside the menu to close it.
  document.addEventListener('click', (ev) => {
    const t = ev.target as HTMLElement;
    if (!t.closest('#file-ctx-menu')) hideFileContextMenu();
  });
  // Esc closes the menu.
  document.addEventListener('keydown', (ev) => {
    if (ev.key === 'Escape') hideFileContextMenu();
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
  //
  // On drop we kick off SendFile right away. Wails gives
  // us a real on-disk path here (unlike the picker, which
  // only exposes File objects), so we can stream the
  // file directly without re-reading bytes in JS.
  OnFileDrop((_x, _y, paths) => {
    if (!state.selectedId) {
      toast('先选一个 peer');
      return;
    }
    if (!paths || paths.length === 0) return;
    const p = paths[0];
    const r = SendFile(state.selectedId, p);
    r.then(err => { if (err) toast(`发文件失败: ${err}`); });
  }, true);
}

bootstrap();
