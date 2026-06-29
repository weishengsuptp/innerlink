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
// 2026-06-24+: aliases are broadcast self-display-names
// (M5 RosterSync), NOT per-peer local nicknames. The
// sidebar's .me box is the only UI affordance to set
// your own alias; clicking it prompts for the new
// value. Other peers' aliases are read from
// PeerInfo.SelfAlias (populated by Go from roster).
// peerID is never shown in the UI — internal routing
// key only.
//
// Features this file implements:
//   - sidebar: peer list with 3-state dot (online/recent/
//     offline), relative time, alias + IP in meta,
//     unread badge; .me self header (click to alias).
//   - chat header: 38px avatar with first character,
//     peer alias (or hostname fallback) + IP:PORT, icon
//     buttons (ping / 文件 / 更多).
//   - messages: 28px avatar on each message, day-divider
//     when day changes, centered system hint about E2E
//     encryption, file cards with size + "SM4-GCM 加密".
//   - composer: 📎 icon-btn toolbar + textarea + 发送
//     button. Drag-and-drop is scoped to the composer
//     (Wails OnFileDrop with useDropTarget=true).
//   - file transfer: drop -> SendFile (real path);
//     📎 picker -> SendFileStart/Chunk/Finish (1 MiB
//     chunks streamed over IPC, live progress via
//     "file:event" runtime events).
//   - unread badge: bumps on incoming messages to a
//     non-selected peer, clears on selection.

import './style.css';
import './app.css';

import {
  CancelFile,
  CreateGroup,
  DebugReveal,
  DialAddr,
  History,
  HistoryGroup,
  InviteToGroup,
  LeaveGroup,
  ListGroupMembers,
  ListGroups,
  ListPeers,
  OpenPath,
  PickFile,
  Ping,
  ReceivedFilePath,
  RevealInFolder,
  Scan,
  SelfPeerID,
  SendFile,
  SendFilePath,
  SendGroupFile,
  SendGroupMessage,
  SendText,
  SetGroupName,
  SetGroupRemark,
  SetMyAlias,
} from '../wailsjs/go/app/App';
import { EventsOn, OnFileDrop } from '../wailsjs/runtime/runtime';
import { node } from '../wailsjs/go/models';

// ----- in-memory state -----
//
// peer-display-name policy (2026-06-24+):
//   list / chat header / self header: alias || hostname || '-'
//   peerID is NEVER shown to the user — it's an internal
//   routing key only. Alias comes from M5 RosterSync
//   (broadcast self-display-name, the new <data-dir>/alias.txt
//   concept); the legacy internal/alias per-peer table is NOT
//   used by the GUI.
interface UIState {
  selfId: string;
  selfEntry: node.PeerInfo | null;
  peers: node.PeerInfo[];             // peers minus self
  // groups: every group on disk (v1.1, 2026-06-28). The
  // sidebar renders a "群组" section above "Peers" with
  // these. Each GroupInfo has GroupID (rendered), Name,
  // Members[] (peer hex), CreatedAt (RFC3339), Self=true
  // when our peerID is in the roster.
  groups: node.GroupInfo[];
  selectedId: string | null;          // peer hex OR rendered group ID
  history: Map<string, node.Message[]>; // conversation key -> msgs
  // nearBottom: are we within ~60px of the message list
  // bottom? Used by renderMessages to decide whether to
  // auto-stick to bottom on new incoming messages.
  nearBottom: boolean;
  // unreadCount: peer hex ID -> number of incoming
  // messages not yet seen. Reset to 0 in selectPeer.
  unreadCount: Map<string, number>;
  // groupUnread: rendered group ID -> incoming unread count
  // (mirrors unreadCount but for group conversations; they
  // share the same conversation-key routing).
  groupUnread: Map<string, number>;
  // fileBubbles: fileID -> file-bubble state. Picker
  // route creates a placeholder bubble the moment the
  // user picks a file (so the UI shows progress even
  // before the Wails IPC round-trip completes), then
  // file:event runtime events update the bar / speed /
  // done state in place. Drag-and-drop bubbles go
  // through the regular chat-message path (no
  // fileBubbles entry, no live progress). 2026-06-25+.
  fileBubbles: Map<string, FileBubbleState>;
  // historyDrawerOpen: is the right-side history drawer
  // currently visible? When true, message:event / peer:event
  // re-trigger renderHistoryList so the user sees new
  // messages arrive live. v1.1 (2026-06-27).
  historyDrawerOpen: boolean;
}

interface FileBubbleState {
  fileID: string;
  name: string;
  size: number;        // bytes total (populated by first file:event)
  sent: number;        // bytes sent (single-phase now)
  bps: number;         // bytes per second (sliding window)
  err: string;
  peerId: string;      // hex peer ID
  localPath: string;   // sender's on-disk path (picker has it; drag-and-drop also has it now)
  startTime: number;   // ms since epoch when SendFile was called — used to position the bubble in chronological order alongside text messages (2026-06-27)
}

const state: UIState = {
  selfId: '',
  selfEntry: null,
  peers: [],
  groups: [],
  selectedId: null,
  history: new Map(),
  nearBottom: true,
  unreadCount: new Map(),
  groupUnread: new Map(),
  fileBubbles: new Map(),
  historyDrawerOpen: false,
};

// isGroupId reports whether the conversation key is a
// rendered GroupID ("g_<64hex>"). Used everywhere we have
// to branch behavior: selectX / renderMessages / composer
// submit / message:event routing. v1.1 (2026-06-28).
function isGroupId(id: string | null): boolean {
  return !!id && id.startsWith('g_');
}

// senderDisplay looks up a 32-char hex PeerID in the
// current peer roster and returns the alias or hostname
// (never the peerID — internal routing only). Returns ""
// for unknown senders (e.g. a member we haven't seen
// online yet, or the local user when Direction == "out").
function senderDisplay(hex: string): string {
  if (!hex) return '';
  if (hex === state.selfId) return '';
  const p = state.peers.find(pp => pp.PeerID === hex);
  return p ? peerDisplay(p) : '';
}

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
  // Display-name policy: alias (broadcast from M5
  // RosterSync) wins; else hostname; else "-". peerID
  // is NOT shown anywhere — internal routing only.
  return p.SelfAlias || p.Hostname || '-';
}

// senderNameForRow returns the display name to show in the
// history drawer's per-row "who said this?" position.
// Differs from the per-pid conversation label: this is the
// SENDER of each message, so:
//   - outbound ("Direction == out"): "我", regardless of
//     whether pid is a peer or a group.
//   - 1:1 inbound: the OTHER party's display name (looked
//     up from state.peers via pid).
//   - group inbound: the actual member's display name,
//     looked up from state.peers via m.SenderID (populated
//     by HistoryGroup for reloaded records, by the live
//     dispatcher for in-session messages).
//
// v1.1 (2026-06-28) hotfix: prior version computed the row
// name from pid (group ID → "群 X") so every row in a group
// — even ones I typed — showed "群 X" instead of "我". The
// per-row sender is the correct granularity; the per-pid
// conversation label is still preserved separately
// (rows[*].convLabel) for context + click-to-scroll.
//
// Fallback policy: the unknown sender / unknown peer never
// crashes the row; it shortens the hex peer ID so the user
// can still tell rows apart even when the alias cache is
// stale.
function senderNameForRow(pid: string, m: node.Message): string {
  if (m.Direction === 'out') {
    return '我';
  }
  // inbound
  if (isGroupId(pid)) {
    // Group inbound: m.SenderID is the original member
    // peer hex. If history was reloaded from disk,
    // pkg/node.HistoryGroup populates it from
    // storage.Record.From (see groups.go). Live dispatch
    // also sets it. If empty (corrupted record /
    // pre-fix message), fall back to "(未知成员)".
    if (m.SenderID) {
      const senderInfo = state.peers.find(p => p.PeerID === m.SenderID);
      if (senderInfo) return peerDisplay(senderInfo);
      return shortId(m.SenderID);
    }
    return '(未知成员)';
  }
  // 1:1 inbound: the sender IS the conversation partner.
  // pid is already the peer's hex.
  const peerInfo = state.peers.find(p => p.PeerID === pid);
  if (peerInfo) return peerDisplay(peerInfo);
  return shortId(pid);
}

function selectedPeer(): node.PeerInfo | null {
  if (!state.selectedId) return null;
  if (isGroupId(state.selectedId)) return null;
  return state.peers.find(p => p.PeerID === state.selectedId) ?? null;
}

// selectedGroup returns the GroupInfo for the currently
// selected conversation, or null if it's not a group.
// v1.1 (2026-06-28).
function selectedGroup(): node.GroupInfo | null {
  if (!isGroupId(state.selectedId)) return null;
  return state.groups.find(g => g.group_id === state.selectedId) ?? null;
}

function isNearBottom(el: HTMLElement): boolean {
  return (el.scrollHeight - el.scrollTop - el.clientHeight) < 60;
}

// toast shows a transient notification. The default
// variant is a soft dark-glass pill (matches the cancel
// button / file-bubble palette so it reads as "this app
// telling you something", not "your system warning you
// about an error"). Error / success variants layer a
// coloured tint on top of the same pill so the form stays
// consistent. 2026-06-27 user feedback: the previous
// bottom-center warn-orange toast looked like a debugger
// error popup.
function toast(msg: string, variant: 'info' | 'success' | 'error' = 'info') {
  const t = document.getElementById('toast')!;
  t.textContent = msg;
  t.className = 'toast toast-' + variant + ' show';
  window.setTimeout(() => {
    t.className = 'toast toast-' + variant;
  }, 2400);
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
        <div class="sidebar-section">
          <div class="sidebar-header">
            <span class="sidebar-title">群组</span>
            <span class="sidebar-count">
              <span id="group-count">0</span>
              <button class="sidebar-add-btn" id="btn-create-group" title="新建群组">
                <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.4"><path d="M12 5v14"/><path d="M5 12h14"/></svg>
              </button>
            </span>
          </div>
          <div class="peer-list group-list" id="group-list"></div>
        </div>
        <div class="sidebar-section">
        <div class="sidebar-header">
          <span class="sidebar-title">Peers</span>
          <span class="sidebar-count"><span id="peer-online">0</span> / <span id="peer-count">0</span></span>
        </div>
        <div class="peer-list" id="peer-list"></div>
        </div>
        <div class="sidebar-footer">
          <button class="btn-mini" id="btn-scan" title="扫描 IP 段">
            <svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.2"><circle cx="11" cy="11" r="7"/><path d="m21 21-4.3-4.3"/></svg>
            scan
          </button>
          <button class="btn-mini" id="btn-test-reveal" title="测试 RevealInFolder: 弹资源管理器选 device.key">
            测试
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
            <!-- v1.1.1 (2026-06-29): group settings button.
                 Only enabled when the selected conversation
                 is a group (WeChat-style ⋯ → 群设置 panel).
                 For 1:1 chats the button stays disabled —
                 per-peer settings aren't a feature yet. -->
            <button class="icon-btn" id="btn-group-settings" title="群设置" disabled>
              <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.2"><circle cx="12" cy="5" r="1.5"/><circle cx="12" cy="12" r="1.5"/><circle cx="12" cy="19" r="1.5"/></svg>
            </button>
            <!-- "more" (⋮) button removed v1.1 — its only
                 action was "clear chat history", which
                 the user downprioritized. The history
                 affordance now lives in the composer
                 toolbar (the 📜 button) and opens a
                 right-side drawer with search. -->
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
            <!-- History button (rightmost in composer
                 toolbar). Toggles the right-side drawer
                 that shows every chat message across all
                 peers + search. v1.1 (2026-06-27). -->
            <button type="button" class="icon-btn" id="btn-history" title="历史消息">
              <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M3 12a9 9 0 1 0 9-9 9.75 9.75 0 0 0-6.74 2.74L3 8"/><path d="M3 3v5h5"/><path d="M12 7v5l4 2"/></svg>
            </button>
          </div>
          <div class="composer-input">
            <textarea id="composer-input" placeholder="先选一个 peer…" disabled></textarea>
            <button class="send-btn" type="submit" id="composer-send" disabled>
              发送
              <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.4"><path d="m22 2-7 20-4-9-9-4Z"/><path d="M22 2 11 13"/></svg>
            </button>
          </div>
          <!-- The picker route now uses runtime.OpenFileDialog
                via PickFile() (a native OS dialog that hands
                the real path to Go). The <input type=file>
                sandbox-route is gone; bytes never cross the
                JS/Go boundary on the picker. -->
        </form>
      </section>
      <!-- History drawer (right-side overlay). Hidden
           by default; .open class toggles visibility.
           We render the drawer OUTSIDE the chat section
           so its absolute positioning doesn't conflict
           with the chat layout's flex sizing. -->
      <aside class="drawer" id="history-drawer" aria-hidden="true">
        <div class="drawer-header">
          <div class="drawer-title">历史消息</div>
          <button class="icon-btn" id="btn-history-close" title="关闭">
            <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M18 6L6 18"/><path d="M6 6l12 12"/></svg>
          </button>
        </div>
        <div class="drawer-search">
          <input type="search" id="history-search" placeholder="搜索关键字（peer 名 / 内容）" />
        </div>
        <div class="drawer-body" id="history-body">
          <!-- list of messages injected by JS -->
        </div>
      </aside>
    </div>
    <!-- Create-group modal (v1.1, 2026-06-28). Hidden by
         default via .modal-hidden; toggled by
         openCreateGroupModal(). Centered overlay with
         name input + member multi-select + Cancel /
         Create buttons. The member list shows every peer
         in the current roster (online first, then
         offline). Self is omitted (creator is implicit). -->
    <div class="modal-overlay modal-hidden" id="create-group-modal" aria-hidden="true">
      <div class="modal-dialog" role="dialog" aria-labelledby="create-group-title">
        <div class="modal-header">
          <div class="modal-title" id="create-group-title">新建群组</div>
          <button class="icon-btn" id="btn-create-group-close" title="关闭">
            <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M18 6L6 18"/><path d="M6 6l12 12"/></svg>
          </button>
        </div>
        <div class="modal-body">
          <div class="modal-field">
            <label for="create-group-name">群组名称</label>
            <input type="text" id="create-group-name" maxlength="30" placeholder="例如：周末饭局" autocomplete="off" />
          </div>
          <div class="modal-field">
            <label>选择成员</label>
            <div class="modal-list" id="create-group-members"></div>
          </div>
        </div>
        <div class="modal-footer">
          <button class="modal-btn" id="btn-create-group-cancel">取消</button>
          <button class="modal-btn primary" id="btn-create-group-submit" disabled>创建</button>
        </div>
      </div>
    </div>
    <!-- Group settings panel (v1.1.1, 2026-06-29). Right-side
         drawer, opened by the ⋯ button in the chat header
         when a group is selected. WeChat-style: 群名 + 群
         公告 / 备注 + 成员列表. Editable fields are gated
         to the creator (mirrors the Go-side SetGroupName /
         SetGroupRemark which already reject non-creator
         edits). -->
    <aside class="drawer group-settings-drawer" id="group-settings-drawer" aria-hidden="true">
      <div class="drawer-header">
        <div class="drawer-title">群设置</div>
        <button class="icon-btn" id="btn-group-settings-close" title="关闭">
          <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M18 6L6 18"/><path d="M6 6l12 12"/></svg>
        </button>
      </div>
      <div class="group-settings-body" id="group-settings-body">
        <!-- populated by openGroupSettings(): name field,
             remark textarea, member list, etc. -->
      </div>
    </aside>
    <div class="toast" id="toast"></div>
  `;
}

// ----- render functions -----
function renderMe() {
  const self = state.selfEntry;
  const name = document.getElementById('me-name')!;
  const host = document.getElementById('me-host')!;
  const status = document.getElementById('me-status')!;
  const meBox = document.querySelector('.me')!;
  if (!self) {
    name.textContent = '—';
    host.textContent = '本机 · —';
    status.textContent = '启动中…';
    meBox.classList.remove('me-clickable');
    return;
  }
  // Self display-name: alias wins (set by the user via
  // SetMyAlias), else hostname. If unset, show a hint
  // "点我设置" so the user knows they CAN set one.
  const display = self.SelfAlias || self.Hostname || '';
  if (self.SelfAlias) {
    name.textContent = self.SelfAlias;
    meBox.classList.add('me-set');
    meBox.classList.remove('me-clickable');
  } else {
    name.textContent = '点我设置别名';
    meBox.classList.add('me-clickable');
    meBox.classList.remove('me-set');
  }
  const ip = self.Addrs[0]?.split(':')[0] || '—';
  // Sub: "本机 · IP · host · peerID-prefix" — peerID is
  // intentionally omitted from the *display* name above
  // but stays here for diagnostic power when needed
  // (truncated to 8 chars). User said "UI 不显示 peerID"
  // but the self header is our own device, not someone
  // else's; showing our own truncated peerID is harmless
  // and useful for cross-checking log lines.
  host.textContent = `${display ? display + ' · ' : ''}本机 · ${ip}`;
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
    // Meta line: <display-name> · IP
    // display-name falls back through alias → hostname
    // → "-" per peerDisplay(). peerID is intentionally
    // not part of the user-facing string.
    const display = peerDisplay(p);
    const ip = escapeHtml(p.Addrs[0]?.split(':')[0] || '');
    const meta = ip
      ? `<span class="alias">${escapeHtml(display)}</span> · ${ip}`
      : `<span class="alias">${escapeHtml(display)}</span>`;
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

// renderGroupList renders the "群组" section in the
// sidebar. One row per group, with:
//   - "#" avatar glyph (distinct from the peer avatar)
//   - group name
//   - meta line: "N 成员 · K 在线" (where K is members
//     currently in our peer roster as online)
//   - unread badge (groupUnread)
//   - active highlight when selectedId === GroupID
//
// Sorted: self groups first (you're a member), then by
// name. v1.1 (2026-06-28).
function renderGroupList() {
  const list = document.getElementById('group-list');
  if (!list) return;
  const sorted = [...state.groups].sort((a, b) => {
    const sa = a.self ? 0 : 1;
    const sb = b.self ? 0 : 1;
    if (sa !== sb) return sa - sb;
    return (a.group_name || '').localeCompare(b.group_name || '');
  });
  list.innerHTML = sorted.map(g => {
    const unread = state.groupUnread.get(g.group_id) || 0;
    const onlineCount = g.members.filter(m =>
      m !== state.selfId &&
      state.peers.some(p => p.PeerID === m && p.Online)
    ).length;
    const memberLabel = `${g.members.length} 成员`;
    const onlineLabel = onlineCount > 0 ? ` · ${onlineCount} 在线` : '';
    return `
      <div class="peer group ${g.group_id === state.selectedId ? 'active' : ''}" data-group="${escapeHtml(g.group_id)}">
        <span class="peer-dot ${g.self ? 'online' : 'offline'}"></span>
        <div class="peer-info">
          <div class="peer-name">${escapeHtml(g.group_name || '(未命名群组)')}</div>
          <div class="peer-meta">${memberLabel}${onlineLabel}</div>
        </div>
        <span class="badge ${unread > 0 ? 'show' : ''}">${unread > 0 ? unread : ''}</span>
      </div>
    `;
  }).join('');

  list.querySelectorAll<HTMLElement>('.peer.group').forEach(el => {
    el.addEventListener('click', () => {
      const gid = el.getAttribute('data-group')!;
      void selectGroup(gid);
    });
  });

  document.getElementById('group-count')!.textContent = String(state.groups.length);
}

function renderChatHeader() {
  const p = selectedPeer();
  const g = selectedGroup();
  const avatar = document.getElementById('chat-avatar')!;
  const name = document.getElementById('chat-name')!;
  const sub = document.getElementById('chat-id')!;
  const pingBtn = document.getElementById('btn-ping') as HTMLButtonElement;
  const dialBtn = document.getElementById('btn-dial') as HTMLButtonElement;
  // v1.1: btn-more removed — history lives in composer
  // toolbar now (📜 button + drawer).
  const attachBtn = document.getElementById('btn-attach') as HTMLButtonElement;
  // v1.1.1 (2026-06-29): group settings (⋯) button.
  // Enabled only when the selected conversation is a
  // group. For 1:1 it stays disabled — no per-peer
  // settings panel exists yet (out of scope for this
  // iteration).
  const groupSetBtn = document.getElementById('btn-group-settings') as HTMLButtonElement;
  const input = document.getElementById('composer-input') as HTMLTextAreaElement;
  const send = document.getElementById('composer-send') as HTMLButtonElement;

  // Group takes precedence over peer — the selectedId
  // branch (g_<64hex> vs 32-char hex) decides which
  // header variant we render. v1.1 (2026-06-28).
  if (g) {
    avatar.textContent = '#';
    name.textContent = g.group_name || '(未命名群组)';
    sub.textContent = `${g.members.length} 成员${g.self ? '' : ' · 只读'}`;
    // Ping/dial aren't meaningful for groups (we don't
    // call Ping / DialAddr on a group ID — they're per
    // member channels handled internally by the Go side).
    pingBtn.disabled = true;
    dialBtn.disabled = true;
    // v1.1.1: enable group settings when we're in a
    // group conversation. WeChat-style ⋯ → 群设置
    // panel. Disabled for "g_<id> not in state.groups"
    // edge cases (e.g. a roster update arrives that we
    // haven't refreshed yet) — the click handler below
    // re-checks state.groups before opening.
    groupSetBtn.disabled = false;
    // Composer: enabled only when self=true. Receiving
    // a message in a group we're not a member of
    // shouldn't be writable from the GUI; we'd have to
    // call SendGroupMessage and the server would reject
    // it because we're not in the roster.
    attachBtn.disabled = !g.self;
    input.disabled = !g.self;
    send.disabled = !g.self;
    input.placeholder = g.self
      ? `发到群 ${g.group_name || ''}…（Enter 发送，Shift+Enter 换行）`
      : `你不是群 ${g.group_name || ''} 的成员`;
    return;
  }

  if (!p) {
    avatar.textContent = '?';
    name.textContent = '未选择 peer';
    sub.textContent = '—';
    pingBtn.disabled = true;
    dialBtn.disabled = true;
    attachBtn.disabled = true;
    input.disabled = true;
    send.disabled = true;
    groupSetBtn.disabled = true;
    input.placeholder = '先选一个 peer…';
    return;
  }
  avatar.textContent = avatarChar(peerDisplay(p));
  name.textContent = peerDisplay(p);
  // Sub line: <display-name> · IP:PORT  (no peerID; user
  // doesn't want it shown anywhere — internal routing key
  // only). If alias and hostname differ, show both so the
  // user knows which is which; otherwise dedup.
  const subParts: string[] = [];
  if (p.SelfAlias && p.Hostname && p.SelfAlias !== p.Hostname) {
    subParts.push(`${p.SelfAlias} (${p.Hostname})`);
  } else if (p.Hostname) {
    subParts.push(p.Hostname);
  }
  if (p.Addrs[0]) subParts.push(p.Addrs[0]);
  sub.textContent = subParts.join(' · ') || '—';
  pingBtn.disabled = !p.Online;
  dialBtn.disabled = !p.Online;
  attachBtn.disabled = false;
  input.disabled = false;
  send.disabled = false;
  // v1.1.1: group settings (⋯) button is for groups only.
  // 1:1 chats don't have a settings panel yet.
  groupSetBtn.disabled = true;
  input.placeholder = `发到 ${peerDisplay(p)}…（Enter 发送，Shift+Enter 换行）`;
}

function renderEmpty() {
  const el = document.getElementById('messages')!;
  el.innerHTML = `
    <div class="empty">
      <div>
        <div class="ico">💬</div>
        <h3>${state.selectedId ? '还没有消息' : '选一个 peer 开始聊天'}</h3>
        <p>${state.selectedId ? '说点啥 👋' : '左侧列表选人，或点你的名字给自己起个对外称呼'}</p>
      </div>
    </div>
  `;
}

function renderMessages() {
  const el = document.getElementById('messages')!;
  const wasNearBottom = isNearBottom(el);
  if (!state.selectedId) { renderEmpty(); return; }
  // Sort history by timestamp — message:event appends in
  // event-arrival order, which is NOT necessarily
  // chronological when several file transfers complete
  // concurrently (Go goroutines finish in arbitrary
  // order). Without this sort, a 3-file drop can render
  // bottom-to-top if the back of the queue finished first.
  // 2026-06-27 user feedback: "最前面两个文件反而是后面发的".
  const rawMsgs = state.history.get(state.selectedId) ?? [];
  const msgs = [...rawMsgs].sort((a, b) => {
    const ta = a.Timestamp ? new Date(a.Timestamp).getTime() : 0;
    const tb = b.Timestamp ? new Date(b.Timestamp).getTime() : 0;
    return ta - tb;
  });
  if (msgs.length === 0) { renderEmpty(); return; }

  // Live file bubbles are positioned by startTime so a
  // text message sent WHILE a file is in flight lands
  // AFTER the file bubble (per send order), not after the
  // file completes. The previous renderMessages appended
  // all file bubbles at the END, which made any text sent
  // during a transfer appear "above" the file card —
  // user feedback: "消息的泡泡没有按顺序继续向下走".
  type Entry = { ts: number; html: string };
  const liveEntries: Entry[] = [];
  const fp = state.selectedId;
  if (fp) {
    for (const fb of state.fileBubbles.values()) {
      if (fb.peerId === fp) {
        liveEntries.push({ ts: fb.startTime || 0, html: renderFileBubble(fb) });
      }
    }
    liveEntries.sort((a, b) => a.ts - b.ts);
  }

  const parts: string[] = [];
  let lastDay = '';
  let liveIdx = 0;
  for (const m of msgs) {
    const ts = m.Timestamp ? new Date(m.Timestamp).getTime() : 0;
    // Insert live bubbles whose startTime is at or before
    // this history message's timestamp. Equal-timestamp
    // entries land the live bubble first (in-flight
    // bubbles precede a "now" chat message so the user
    // sees the file card they kicked off before the
    // follow-up text they typed).
    while (liveIdx < liveEntries.length && liveEntries[liveIdx].ts <= ts) {
      parts.push(liveEntries[liveIdx].html);
      liveIdx++;
    }
    const day = dayLabel(m.Timestamp);
    if (day && day !== lastDay) {
      parts.push(`<div class="day-divider"><span>${escapeHtml(day)}</span></div>`);
      lastDay = day;
    }
    parts.push(renderMessage(m, state.selectedId));
  }
  // Any remaining live bubbles (startTime after every
  // persisted message) go to the very end.
  while (liveIdx < liveEntries.length) {
    parts.push(liveEntries[liveIdx].html);
    liveIdx++;
  }
  // First-load system hint about E2E encryption.
  parts.push(`<div class="msg system"><div class="bubble">—— 端到端加密，每条消息独立会话密钥 ——</div></div>`);
  el.innerHTML = parts.join('');
  if (wasNearBottom) el.scrollTop = el.scrollHeight;
  state.nearBottom = isNearBottom(el);
}

// renderFileBubble builds the HTML for one picker-route
// file bubble. The bubble is part of the .msg.self
// stream (outgoing) and includes:
//   - file icon + name + size
//   - progress bar (color-filled from 0 to sent/size)
//   - live speed number ("X.X MB/s") once we have at
//     least one progress event
//   - status row that flips from "排队中…" to a
//     progress % to "已发送" / "失败: <err>"
//
// Identified by data-file-id so the file:event handler
// can find it without re-rendering the whole list.
function renderFileBubble(fb: FileBubbleState): string {
  const pct = fb.size > 0 ? Math.min(100, Math.floor(fb.sent * 100 / fb.size)) : 0;
  const sizeStr = humanSize(fb.size);
  let statusHtml: string;
  let barClass: string;
  // Single-phase status text. The picker route now
  // streams bytes from the user's disk → IPC →
  // io.Pipe → filetransfer.Send → network in ONE
  // pass; the picker no longer stages to a local
  // file. So there's only one progress stream and
  // the bar fills 0→100 exactly once.
  // Cancelled is its own state, not a failure: the user
  // explicitly hit ✕. Show "已取消" without the "失败:"
  // prefix and a neutral gray bar so the eye reads it
  // distinctly from a real error. User feedback 2026-06-27
  // 16:48: the previous "失败: 已取消" was hard to spot on
  // the green outgoing bubble — light-pink-on-green is
  // illegible. The .file-status-cancelled class is now
  // bold yellow on the outgoing bubble, much higher
  // contrast.
  const isCancelled = !!fb.err && fb.err === '已取消';
  if (fb.err && !isCancelled) {
    statusHtml = `<span class="file-status file-status-failed">失败: ${escapeHtml(fb.err || '未知错误')}</span>`;
    barClass = 'file-bar-failed';
  } else if (isCancelled) {
    statusHtml = `<span class="file-status file-status-cancelled">已取消</span>`;
    barClass = 'file-bar-cancelled';
  } else if (fb.sent >= fb.size && fb.size > 0) {
    statusHtml = `<span class="file-status file-status-done">已发送 · ${sizeStr}</span>`;
    barClass = 'file-bar-done';
  } else if (fb.sent > 0) {
    // Live progress: percent + speed + ETA. ETA is
    // computed from (size - sent) / bps; we hide it
    // when bps is still 0 (first tick, no rate yet) or
    // when the remaining time would be misleadingly
    // large (> 99 hours — likely a stall).
    const bpsStr = fb.bps > 0 ? humanSize(fb.bps) + '/s' : '';
    const etaStr = formatEta(fb.size - fb.sent, fb.bps);
    const parts = [`发送中 · ${pct}%`];
    if (bpsStr) parts.push(bpsStr);
    if (etaStr) parts.push(etaStr);
    statusHtml = `<span class="file-status">${parts.join(' · ')}</span>`;
    barClass = '';
  } else {
    statusHtml = `<span class="file-status file-status-pending">排队中…</span>`;
    barClass = '';
  }
  // data-file-path is the user's real on-disk path
  // (set by the picker via the native dialog, or by
  // drag-and-drop). Same value either way — both
  // routes have a real path in v4. Right-click uses
  // it to reveal in Explorer.
  const dataPath = fb.localPath || '';
  // Cancel button: show for ANY not-yet-done bubble. v1.1
  // (2026-06-27) fix: the previous condition `fb.sent < fb.size`
  // evaluated to false at the moment of send (both are 0 —
// `0 < 0` is false), so the ✕ button didn't appear until
  // the FIRST progress event arrived (and only if the user
  // had triggered a peer switch to force a re-render). The
  // right test is "not done": either size is still unknown
  // (waiting for first file:event progress) or we haven't
  // sent all the bytes yet.
  const showCancel = !fb.err && !(fb.size > 0 && fb.sent >= fb.size);
  const cancelBtn = showCancel
    ? `<button class="file-cancel" data-cancel-file-id="${escapeHtml(fb.fileID)}" title="取消发送">
         <svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.4"><path d="M18 6L6 18"/><path d="M6 6l12 12"/></svg>
       </button>`
    : '';
return `
    <div class="msg self" data-file-id="${escapeHtml(fb.fileID)}">
      <div class="av">我</div>
      <div>
        <div class="bubble file-bubble" data-file-name="${escapeHtml(fb.name)}" data-file-path="${escapeHtml(dataPath)}" title="双击打开 · 右键更多">
          <div class="file-msg">
            <div class="file-icon">
              <svg width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M14 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V8z"/><path d="M14 2v6h6"/></svg>
            </div>
            <div class="file-info">
              <div class="file-name">${escapeHtml(fb.name)}</div>
              <div class="file-bar ${barClass}">
                <div class="file-bar-fill" style="width: ${pct}%"></div>
              </div>
              <div class="file-meta">${statusHtml}</div>
            </div>
          </div>
          ${cancelBtn}
        </div>
      </div>
    </div>
  `;
}

// humanSize returns a human-readable file size string
// (B / KiB / MiB / GiB) for the progress / bubble
// labels. Binary units, no decimals above 99.9 to keep
// the bubble width stable.
function humanSize(n: number): string {
  if (!n || n < 0) return '0 B';
  if (n < 1024) return `${n} B`;
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(n < 10240 ? 1 : 0)} KiB`;
  if (n < 1024 * 1024 * 1024) return `${(n / 1024 / 1024).toFixed(n < 10 * 1024 * 1024 ? 1 : 0)} MiB`;
  return `${(n / 1024 / 1024 / 1024).toFixed(2)} GiB`;
}

// formatEta returns a Chinese "还需 X" string for the
// remaining bytes at the given rate. Returns "" (hidden)
// when the rate is too low to estimate (< 1 KiB/s —
// first tick after transfer start) or when the
// projected time is suspiciously long (> 99 hours,
// likely a stall — UI shouldn't show "还需 47 天").
//
// Output examples:
//   remaining=500_000_000  bps=12_000_000 → "还需 42 秒"
//   remaining=1_500_000_000 bps=11_000_000 → "还需 2 分 16 秒"
//   remaining=5_000_000_000_000 bps=10_000_000 → "还需 5 天 19 小时"
//   remaining=anything bps=0 → "" (hidden)
function formatEta(remaining: number, bps: number): string {
  if (remaining <= 0) return '即将完成';
  if (bps <= 0) return '';
  const seconds = remaining / bps;
  if (seconds < 1) return '还需 < 1 秒';
  if (seconds >= 99 * 3600) return ''; // > 99h = stall, hide
  if (seconds < 60) return `还需 ${Math.ceil(seconds)} 秒`;
  if (seconds < 3600) {
    const m = Math.floor(seconds / 60);
    const s = Math.ceil(seconds - m * 60);
    return s === 0 ? `还需 ${m} 分` : `还需 ${m} 分 ${s} 秒`;
  }
  if (seconds < 86400) {
    const h = Math.floor(seconds / 3600);
    const m = Math.ceil((seconds - h * 3600) / 60);
    return m === 0 ? `还需 ${h} 小时` : `还需 ${h} 小时 ${m} 分`;
  }
  const d = Math.floor(seconds / 86400);
  const h = Math.ceil((seconds - d * 86400) / 3600);
  return h === 0 ? `还需 ${d} 天` : `还需 ${d} 天 ${h} 小时`;
}

// findFileBubbleEl looks up the DOM node for a file
// bubble by its data-file-id. Returns null if the user
// has navigated away or the bubble was cleared.
function findFileBubbleEl(fileID: string): HTMLElement | null {
  return document.querySelector(`[data-file-id="${CSS.escape(fileID)}"]`);
}

function renderMessage(m: node.Message, peerId: string): string {
  const isOut = m.Direction === 'out';
  const sideClass = isOut ? 'self' : '';
  const ts = fmtTime(m.Timestamp);
  const peer = state.peers.find(p => p.PeerID === peerId);
  const isGroup = isGroupId(peerId);
  // v1.1 (2026-06-28): in a group, the avatar glyph comes
  // from the original sender (m.SenderID), NOT the
  // conversation partner (peerId, which is the group ID).
  // For outbound, we still show "我" (our own avatar is
  // always "我" regardless of conversation type).
  let avChar = isOut ? '我' : avatarChar(peer ? peerDisplay(peer) : '');
  let senderLabel = '';
  if (isGroup && !isOut) {
    const senderName = senderDisplay(m.SenderID || '');
    avChar = avatarChar(senderName || shortId(m.SenderID || ''));
    // Sender label above the bubble — "Alice" line so the
    // eye knows who's talking. Empty if sender is unknown
    // (member we haven't seen online yet).
    senderLabel = senderName;
  }

  // File message: Body has prefix "file://" (per core
  // pkg/node/messages.go SendFile + OnComplete), with an
  // optional "|size" suffix for the meta line.
  //
  // v1.1: data-msg-ts / data-msg-peer / data-msg-dir on
  // the outer .msg let the history drawer's click
  // handler scroll to this exact message.
  const msgAttrs = `data-msg-ts="${escapeHtml(m.Timestamp || '')}" data-msg-peer="${escapeHtml(m.PeerID || '')}" data-msg-dir="${escapeHtml(m.Direction || '')}"`;
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
      <div class="msg ${sideClass}" ${msgAttrs}>
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
    <div class="msg ${sideClass}" ${msgAttrs}>
      <div class="av">${escapeHtml(avChar)}</div>
      <div>
        ${senderLabel ? `<div class="sender">${escapeHtml(senderLabel)}</div>` : ''}
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
  renderChatList();
  renderChatHeader();
  renderMessages();
  const input = document.getElementById('composer-input') as HTMLTextAreaElement;
  input.focus();
}

// selectGroup opens a group conversation. Mirrors
// selectPeer but calls HistoryGroup (per-group chat.enc)
// instead of History (per-peer chat.enc). v1.1 (2026-06-28).
async function selectGroup(renderedID: string) {
  state.selectedId = renderedID;
  // Group sidebar entry uses .group class, so we need to
  // re-render both lists (the active highlight lives in
  // different DOM nodes for the two sections).
  state.groupUnread.set(renderedID, 0);
  try {
    const r = await HistoryGroup(renderedID);
    state.history.set(renderedID, (r && r.messages) || []);
  } catch (e) {
    state.history.set(renderedID, []);
    toast(`读取群历史失败: ${e}`);
  }
  renderPeerList();
  renderGroupList();
  renderChatHeader();
  renderMessages();
  const input = document.getElementById('composer-input') as HTMLTextAreaElement;
  if (!input.disabled) input.focus();
}

// renderChatList re-renders BOTH the group list and the
// peer list. After selectPeer / selectGroup we need to
// keep the active highlight in sync across both sections
// (a peer can become selected, an active group gets
// unselected, etc.). v1.1 (2026-06-28).
function renderChatList() {
  renderGroupList();
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
    // Also load groups — the sidebar's "群组" section
    // reflects the on-disk group roster. v1.1 (2026-06-28).
    await loadGroups();
    renderMe();
    renderPeerList();
    renderGroupList();
    renderChatHeader();
    if (state.selectedId) {
      const stillThere = isGroupId(state.selectedId)
        ? state.groups.some(g => g.group_id === state.selectedId)
        : state.peers.some(p => p.PeerID === state.selectedId);
      if (!stillThere) {
        state.selectedId = null;
        renderChatHeader();
      }
    }
  } catch (e) {
    toast(`刷新失败: ${e}`);
  }
}

// loadGroups fetches the on-disk group roster and
// updates state.groups. Called by refreshAll and after
// any group-membership change (currently only via
// peer:event, which fires when a new peer appears —
// v1.1 auto-accepts invites, so the roster change
// happens out-of-band; we re-pull to reflect it).
// v1.1 (2026-06-28).
async function loadGroups() {
  try {
    const r = await ListGroups();
    state.groups = (r && r.groups) || [];
  } catch (e) {
    // ListGroups failing shouldn't kill the rest of
    // refresh — surface as a toast and continue with
    // whatever we had cached.
    toast(`读取群列表失败: ${e}`);
  }
}

// ----- create-group modal (v1.1, 2026-06-28) -----
//
// Click the "+ 新建群组" button in the sidebar header
// to open a modal with:
//   - name input (required, max 30 chars — matches
//     pkg/node CreateGroup's hard cap)
//   - member multi-select (checkboxes; one row per peer
//     in state.peers; self is omitted — the creator is
//     implicit)
//
// Submit flow:
//   1. App.CreateGroup(name, peerHexes) → local members.json
//      + chat.enc. The group exists on disk; nothing
//      has been sent over the network yet.
//   2. For each member, App.InviteToGroup(renderedID,
//      memberHex). This signs a 1:1 invite envelope and
//      sends it over the existing per-member channel.
//      The remote side auto-accepts (Go dispatcher
//      TypeGroupInvite case), adds us to their
//      members.json, and starts receiving broadcasts.
//   3. Refresh the sidebar list, auto-select the new
//      group so the user lands in the chat panel.
//
// Failure modes we tolerate:
//   - CreateGroup rejects (bad name, too many members) →
//     toast the error, keep modal open so the user can
//     fix and retry.
//   - CreateGroup succeeds but InviteToGroup fails for
//     some members → toast the partial failure but still
//     select the group; the local group is real even if
//     some invites didn't go through (the user can
//     re-invite from a future menu). The offline members
//     will see nothing — invites over an inactive channel
//     silently fail (no queue yet).
function openCreateGroupModal() {
  const modal = document.getElementById('create-group-modal');
  if (!modal) return;
  // Reset state.
  const nameInput = document.getElementById('create-group-name') as HTMLInputElement;
  nameInput.value = '';
  renderCreateGroupMemberList();
  updateCreateGroupSubmitButton();
  modal.classList.remove('modal-hidden');
  modal.setAttribute('aria-hidden', 'false');
  // Focus the name field on next tick so the click that
  // opened the modal doesn't bubble into focus loss.
  setTimeout(() => nameInput.focus(), 0);
}

function closeCreateGroupModal() {
  const modal = document.getElementById('create-group-modal');
  if (!modal) return;
  modal.classList.add('modal-hidden');
  modal.setAttribute('aria-hidden', 'true');
}

// renderCreateGroupMemberList injects one .modal-list-item
// per peer (self omitted). Online peers first, then
// offline. Click anywhere on the row to toggle the
// checkbox; the input itself is the visual only.
function renderCreateGroupMemberList() {
  const list = document.getElementById('create-group-members');
  if (!list) return;
  if (state.peers.length === 0) {
    list.innerHTML = `<div style="padding:14px 12px;color:var(--muted);font-size:12px;">没有可邀请的 peer。先在右侧"Peers"列表里确认至少一个 peer 在网。</div>`;
    return;
  }
  const sorted = [...state.peers].sort((a, b) => {
    const sa = a.Online ? 0 : 1;
    const sb = b.Online ? 0 : 1;
    if (sa !== sb) return sa - sb;
    return peerDisplay(a).localeCompare(peerDisplay(b));
  });
  list.innerHTML = sorted.map(p => `
    <label class="modal-list-item" data-peer="${p.PeerID}">
      <input type="checkbox" data-pick-peer="${p.PeerID}" ${p.Online ? '' : 'disabled'} />
      <span class="peer-dot ${p.Online ? 'online' : 'offline'}" style="width:6px;height:6px;"></span>
      <div>
        <div class="name">${escapeHtml(peerDisplay(p))}</div>
        <div class="meta">${escapeHtml(p.Addrs[0]?.split(':')[0] || '')}${p.Online ? '' : ' · 离线'}</div>
      </div>
    </label>
  `).join('');
}

// pickedMemberPeerIDs returns the array of peerIDs the
// user checked. Used by submit + the disabled-state
// check below. Reads DOM each call (cheap; < 50 peers).
function pickedMemberPeerIDs(): string[] {
  const inputs = document.querySelectorAll<HTMLInputElement>('#create-group-members input[type=checkbox]:checked');
  const out: string[] = [];
  inputs.forEach(i => {
    const pid = i.getAttribute('data-pick-peer');
    if (pid) out.push(pid);
  });
  return out;
}

// updateCreateGroupSubmitButton enables the Create
// button only when the name is non-empty AND at least
// one member is checked. Wires the listener once via
// event delegation on the modal.
function updateCreateGroupSubmitButton() {
  const nameInput = document.getElementById('create-group-name') as HTMLInputElement;
  const submit = document.getElementById('btn-create-group-submit') as HTMLButtonElement;
  if (!nameInput || !submit) return;
  const hasName = nameInput.value.trim().length > 0;
  const hasMember = pickedMemberPeerIDs().length > 0;
  submit.disabled = !(hasName && hasMember);
}

async function submitCreateGroup() {
  const nameInput = document.getElementById('create-group-name') as HTMLInputElement;
  const submit = document.getElementById('btn-create-group-submit') as HTMLButtonElement;
  const name = nameInput.value.trim();
  const memberHexes = pickedMemberPeerIDs();
  if (!name || memberHexes.length === 0) return;
  // Lock the button while we wait so a double-click
  // doesn't fire two CreateGroup calls (and produce
  // two groups with the same name — different
  // timestamps make them distinct GroupIDs but still
  // an annoying duplicate for the user).
  submit.disabled = true;
  try {
    const r = await CreateGroup(name, memberHexes);
    const err = (r && r.err) || '';
    if (err) {
      toast(`建群失败: ${err}`);
      submit.disabled = false;
      return;
    }
    const info = r && r.group_info;
    if (!info) {
      toast('建群失败: 无返回');
      submit.disabled = false;
      return;
    }
    // Fire invites. Per-member: each gets its own 1:1
    // envelope via the existing channel. We collect
    // per-member errors and report a partial-failure
    // toast if any failed (offline / channel not up).
    const failedInvites: string[] = [];
    for (const hex of memberHexes) {
      const ir = await InviteToGroup(info.group_id, hex);
      const ierr = (ir && ir.err) || '';
      if (ierr) failedInvites.push(`${shortId(hex)}: ${ierr}`);
    }
    // Refresh the sidebar so the new group shows up,
    // then auto-select it so the user lands in the
    // chat panel.
    await loadGroups();
    renderGroupList();
    await selectGroup(info.group_id);
    closeCreateGroupModal();
    if (failedInvites.length > 0) {
      // v1.1 (2026-06-28) hotfix: previous wording
      // "(peer 不在线?)" was misleading because peers
      // in the PEERS list might still show as online
      // even when their 1:1 channel hasn't been
      // established yet (the GUI shows Online=true from
      // the last UDP announcement; the TCP/TLS
      // handshake lags behind). Pass through the
      // actual Go error per invitee so the user can
      // distinguish "offline" vs "send failed" vs
      // "context cancelled" without guessing.
      toast(`已建群，${failedInvites.length} 个邀请未送达：${failedInvites.join('; ')}`, 'error');
    } else {
      toast(`群 ${info.group_name} 已创建，${memberHexes.length} 个邀请已发送`);
    }
  } catch (e) {
    toast(`建群失败: ${e}`);
    submit.disabled = false;
  }
}

// ----- leave-group (v1.1, 2026-06-28) -----
//
// Right-click on a group row in the sidebar opens a
// small context menu with "退出群聊" (v1.1, 2026-06-28
// hotfix; previous version jumped straight to a confirm
// dialog, surprising users who expected Windows-style
// menu-first). The menu's click handler calls leaveGroup
// which prompts + dispatches.
//
// leaveGroup prompts the user to confirm, then calls
// App.LeaveGroup which deletes the local members.json +
// chat.enc + sender-keys/. We do NOT notify remaining
// members in v1.1 — that's a follow-up (a "user has
// left" system message broadcast through the per-member
// channels, similar to the existing TypeRosterSync
// plumbing).
//
// v1.1 (2026-06-28) hotfix: when we're the creator AND
// no other members remain, Go allows the leave and the
// empty-members branch deletes the group (self-dissolve).
// The error message for "creator + others present" is
// translated from the Go-side marker "群主无法直接退出"
// into a friendly toast. The proper "dissolve group"
// broadcast lands in DissolveGroup (v1.1.x TODO).
//
// showGroupContextMenu builds the right-click popup
// shown over a group row in the sidebar. Anchored at
// (clientX, clientY); removed on outside click. v1.1
// (2026-06-28).
function showGroupContextMenu(x: number, y: number, gid: string) {
  // Tear down any previous instance before mounting a
  // new one (the click outside handler hasn't fired yet
  // when the user right-clicks a second row).
  document.getElementById('group-ctx-menu')?.remove();
  const menu = document.createElement('div');
  menu.id = 'group-ctx-menu';
  menu.className = 'ctx-menu';
  menu.style.left = x + 'px';
  menu.style.top = y + 'px';
  // v1.1: only "退出群聊" for now. Future items:
  // "解散群聊" (creator only), "邀请成员" (creator
  // only), "群设置". Each would be a new ctx-menu-item.
  const item = document.createElement('div');
  item.className = 'ctx-menu-item danger';
  item.textContent = '退出群聊';
  item.addEventListener('click', () => {
    menu.remove();
    void leaveGroup(gid);
  });
  menu.appendChild(item);
  document.body.appendChild(menu);
  // Close on outside click — wrap in setTimeout(0) so
  // the click that opened the menu doesn't immediately
  // close it (it bubbles after the menu item's listener
  // fires).
  setTimeout(() => {
    const onOutside = (ev: MouseEvent) => {
      if (menu.contains(ev.target as Node)) return;
      menu.remove();
      document.removeEventListener('click', onOutside);
    };
    document.addEventListener('click', onOutside);
  }, 0);
}

async function leaveGroup(renderedID: string) {
  const g = state.groups.find(x => x.group_id === renderedID);
  const name = g ? g.group_name : renderedID;
  const ok = window.confirm(`退出群组 "${name}"？本机会删除本地聊天记录，其他成员仍能看到你之前的消息。`);
  if (!ok) return;
  const r = await LeaveGroup(renderedID);
  const err = (r && r.err) || '';
  if (err) {
    // v1.1 (2026-06-28) hotfix: Go returns a Chinese marker
    // "群主无法直接退出..." when the creator tries to
    // leave while other members remain. Translate that
    // to a friendly toast instead of leaking the raw Go
    // string ("node: LeaveGroup: 群主..."). Any other
    // error passes through verbatim.
    if (err.includes('群主无法直接退出')) {
      toast(`群主无法直接退出。请等所有成员先离开，或等「解散群聊」功能（v1.1.x TODO）`, 'error');
    } else {
      toast(`退出失败: ${err}`, 'error');
    }
    return;
  }
  if (state.selectedId === renderedID) {
    state.selectedId = null;
  }
  await loadGroups();
  renderGroupList();
  renderPeerList();
  renderChatHeader();
  renderMessages();
  toast(`已退出群 ${name}`);
}

// onGroupEvent handles Go-side "group:event" runtime
// events (mirror of the peer:event handler but for
// groups). Three responsibilities:
//   1. Refresh state.groups so the sidebar reflects
//      the new / removed row
//   2. Clear selectedId if the removed group was the
//      open conversation (UI safety)
//   3. Surface a toast for the invite-received path
//      so the user knows where the new row came from
//      and who pulled them in
// v1.1 (2026-06-28).
async function onGroupEvent(ev: any) {
  // Reload first so any subsequent render sees fresh data.
  await loadGroups();
  renderGroupList();
  if (ev.Type === 'removed') {
    if (state.selectedId === ev.GroupID) {
      state.selectedId = null;
      renderChatHeader();
      renderMessages();
    }
    return;
  }
  if (ev.Type !== 'added') return;
  // InviterHex set ⇒ someone invited us; the toast is
  // the only place we show "who pulled me in" since the
  // sidebar row only shows the group name. Self-created
  // groups (modal flow) don't fire an InviterHex — the
  // user just clicked "create" so they know where the
  // row came from.
  if (ev.InviterHex) {
    const inviter = state.peers.find(p => p.PeerID === ev.InviterHex);
    const inviterName = inviter ? peerDisplay(inviter) : shortId(ev.InviterHex);
    toast(`${inviterName} 把你拉进了「${ev.GroupName || '群'}」`, 'success');
  }
  // If the user already had this group selected (rare but
  // possible after a restart), the new GroupInfo needs
  // to land in the header — re-render.
  if (state.selectedId === ev.GroupID) {
    renderChatHeader();
  }
  // v1.1.1 (2026-06-29): if the settings panel for this
  // group is currently open, refresh its in-place content
  // so roster changes (a new joiner arriving via the
  // TypeGroupRosterUpdate envelope) are reflected
  // immediately. Cheap (one ListGroupMembers RPC + a
  // re-render).
  refreshGroupSettingsIfOpen();
}

async function promptMyAlias() {
  // Click on the .me box to set your own broadcast
  // alias (what other peers see for you). Empty clears.
  // No first-launch popup — silent-on-open per user
  // preference; user discovers the affordance via the
  // "点我设置别名" hint when unset.
  const current = state.selfEntry?.SelfAlias || '';
  const name = window.prompt('设置你的对外称呼（空 = 清除；其他 peer 会看到这个名字）:', current);
  if (name == null) return;
  const trimmed = name.trim();
  const r = await SetMyAlias(trimmed);
  if (r) toast(`设置别名: ${r}`);
  await refreshAll();
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

// ----- group settings panel (v1.1.1, 2026-06-29) -----
//
// WeChat-style right-side drawer. Opened by the ⋯ button
// in the chat header when a group is selected. Three
// sections:
//   1. 群名称 — input + save. Editable only by the creator
//      (Go-side enforces this; we hide the save button
//      for non-creators but show the read-only value).
//   2. 群备注 / 公告 — textarea + save. Same creator-only
//      edit rule.
//   3. 成员列表 — table of alias + 在线 dot + peerID prefix.
//      Self row marked "我". Creator marked "群主".
//
// The creator check uses GroupInfo's `members[]` array:
// we look up our own peerID (state.selfId) and see whether
// any member has the is_creator flag set AND that member
// is our self. v1.1.1: creator identification relies on
// the creator field returned by GetGroup/ListGroups,
// which is the canonical creator peerID. If our peerID
// matches, we're the creator.
//
// Note: state.GroupInfo doesn't expose an is_creator flag
// directly — it's in pkg/group.Members but the Wails
// GroupInfo flattens it to "members[]" (string slice).
// So we cross-reference with the more detailed
// ListGroupMembers (which DOES expose IsCreator per row).

interface GroupMemberDetail {
  peer_id: string;
  alias: string;
  joined_at: string;
  is_creator: boolean;
  self: boolean;
}

interface GroupSettingsState {
  renderedID: string;
  groupName: string;
  remark: string;
  members: GroupMemberDetail[];
  isCreator: boolean;
}

let groupSettingsCache: GroupSettingsState | null = null;

async function openGroupSettings(): Promise<void> {
  if (!state.selectedId || !isGroupId(state.selectedId)) return;
  const renderedID = state.selectedId;
  // Pull both GroupInfo (for the editable name / remark)
  // and ListGroupMembers (for the per-row detail). The
  // GroupInfo's `creator` field tells us whether we own
  // the edit rights; ListGroupMembers tells us which
  // members are currently online.
  const g = state.groups.find(x => x.group_id === renderedID);
  if (!g) {
    toast('群信息已过期，正在刷新…');
    await loadGroups();
    return;
  }
  let detailRes;
  try {
    detailRes = (await ListGroupMembers(renderedID)) as { members: GroupMemberDetail[]; err: string };
  } catch (e) {
    toast(`读取群成员失败: ${e}`);
    return;
  }
  if (detailRes.err) {
    toast(`读取群成员失败: ${detailRes.err}`);
    return;
  }
  const selfHex = state.selfId || '';
  const isCreator = selfHex !== '' && g.creator === selfHex;
  groupSettingsCache = {
    renderedID,
    groupName: g.group_name || '',
    remark: (g as any).remark || '',
    members: detailRes.members || [],
    isCreator,
  };
  renderGroupSettingsPanel();
  toggleGroupSettingsDrawer(true);
}

function closeGroupSettings(): void {
  toggleGroupSettingsDrawer(false);
}

function toggleGroupSettingsDrawer(open: boolean): void {
  const drawer = document.getElementById('group-settings-drawer');
  if (!drawer) return;
  drawer.classList.toggle('open', open);
  drawer.setAttribute('aria-hidden', open ? 'false' : 'true');
}

function renderGroupSettingsPanel(): void {
  const body = document.getElementById('group-settings-body');
  if (!body || !groupSettingsCache) return;
  const { groupName, remark, members, isCreator } = groupSettingsCache;
  // Sort: creator first, then self, then by joined_at
  // ascending. Falls back to the on-disk order if a row
  // is missing joined_at (shouldn't happen post-v1.1).
  const sortedMembers = [...members].sort((a, b) => {
    if (a.is_creator && !b.is_creator) return -1;
    if (!a.is_creator && b.is_creator) return 1;
    if (a.self && !b.self) return -1;
    if (!a.self && b.self) return 1;
    const ta = new Date(a.joined_at).getTime();
    const tb = new Date(b.joined_at).getTime();
    return ta - tb;
  });
  const memberRowsHtml = sortedMembers.map(m => {
    // Online status: the chat panel's state.peers list is
    // the source of truth (it's what the sidebar uses for
    // peer dots). Cross-reference by PeerID.
    const peerInfo = state.peers.find(p => p.PeerID === m.peer_id);
    const isOnline = !!peerInfo?.Online;
    const display = m.alias || (peerInfo ? peerDisplay(peerInfo) : '') || shortId(m.peer_id);
    const tags: string[] = [];
    if (m.is_creator) tags.push('群主');
    if (m.self) tags.push('我');
    const tagHtml = tags.length
      ? `<span class="member-tags">${tags.map(t => `<span class="member-tag">${escapeHtml(t)}</span>`).join('')}</span>`
      : '';
    return `
      <div class="member-row">
        <span class="member-dot ${isOnline ? 'online' : 'offline'}"></span>
        <div class="member-info">
          <div class="member-name">${escapeHtml(display)}${tagHtml}</div>
          <div class="member-meta">${escapeHtml(shortId(m.peer_id))}</div>
        </div>
      </div>
    `;
  }).join('');
  const editBlockerNote = isCreator
    ? ''
    : `<div class="settings-hint">仅群主可修改名称和备注。</div>`;
  body.innerHTML = `
    <div class="settings-section">
      <label class="settings-label" for="gs-name">群名称</label>
      <div class="settings-row">
        <input type="text" id="gs-name" maxlength="30" value="${escapeHtml(groupName)}" ${isCreator ? '' : 'disabled'} />
        ${isCreator ? '<button class="modal-btn primary" id="gs-name-save">保存</button>' : ''}
      </div>
    </div>
    <div class="settings-section">
      <label class="settings-label" for="gs-remark">群备注 / 公告</label>
      <textarea id="gs-remark" maxlength="500" rows="3" placeholder="选填；比如本周五晚聚餐" ${isCreator ? '' : 'disabled'}>${escapeHtml(remark)}</textarea>
      ${isCreator ? '<div class="settings-row"><button class="modal-btn primary" id="gs-remark-save">保存</button></div>' : ''}
    </div>
    ${editBlockerNote}
    <div class="settings-section">
      <div class="settings-label">成员（${members.length}）</div>
      <div class="member-list">${memberRowsHtml || '<div class="settings-empty">还没有成员</div>'}</div>
    </div>
  `;
  if (isCreator) {
    document.getElementById('gs-name-save')?.addEventListener('click', () => void saveGroupName());
    document.getElementById('gs-remark-save')?.addEventListener('click', () => void saveGroupRemark());
  }
}

async function saveGroupName(): Promise<void> {
  if (!groupSettingsCache || !groupSettingsCache.isCreator) return;
  const input = document.getElementById('gs-name') as HTMLInputElement | null;
  if (!input) return;
  const newName = input.value.trim();
  if (!newName) {
    toast('群名称不能为空');
    return;
  }
  if (newName === groupSettingsCache.groupName) {
    toast('群名称未变化');
    return;
  }
  try {
    const r = (await SetGroupName(groupSettingsCache.renderedID, newName)) as {
      group_info: node.GroupInfo;
      err: string;
    };
    if (r.err) {
      toast(`保存失败: ${r.err}`);
      return;
    }
    groupSettingsCache.groupName = newName;
    // Refresh sidebar / chat header so the new name lands
    // everywhere without waiting for the next event.
    await loadGroups();
    renderGroupList();
    renderChatHeader();
    toast('群名称已保存');
  } catch (e) {
    toast(`保存失败: ${e}`);
  }
}

async function saveGroupRemark(): Promise<void> {
  if (!groupSettingsCache || !groupSettingsCache.isCreator) return;
  const ta = document.getElementById('gs-remark') as HTMLTextAreaElement | null;
  if (!ta) return;
  const newRemark = ta.value;
  if (newRemark === groupSettingsCache.remark) {
    toast('群备注未变化');
    return;
  }
  try {
    const r = (await SetGroupRemark(groupSettingsCache.renderedID, newRemark)) as {
      group_info: node.GroupInfo;
      err: string;
    };
    if (r.err) {
      toast(`保存失败: ${r.err}`);
      return;
    }
    groupSettingsCache.remark = newRemark;
    await loadGroups();
    toast('群备注已保存');
  } catch (e) {
    toast(`保存失败: ${e}`);
  }
}

// refreshGroupSettingsIfOpen refreshes the panel content
// in-place when the roster updates (a new joiner arrived,
// the group name synced from the creator, etc.) so the
// user sees live data without closing + reopening the
// drawer. Called from onGroupEvent + after a creator
// SetGroupName / SetGroupRemark completes. v1.1.1
// (2026-06-29).
function refreshGroupSettingsIfOpen(): void {
  if (!groupSettingsCache) return;
  const drawer = document.getElementById('group-settings-drawer');
  if (!drawer || !drawer.classList.contains('open')) return;
  // Re-fetch the underlying state and re-render.
  const id = groupSettingsCache.renderedID;
  // Fire-and-forget; failures toast but don't block.
  void openGroupSettings().then(() => {
    // openGroupSettings re-reads; if the user already
    // closed the drawer between calls, just bail.
    if (!groupSettingsCache || groupSettingsCache.renderedID !== id) return;
  });
}

// v1.1 (2026-06-27): promptMore is REMOVED. The "more"
// (⋮) button used to host "clear chat history" — the
// user downprioritized that action ("清空的功能感觉
// 还是没啥用回头再说吧") so the whole affordance is
// gone. The composer-toolbar 📜 button + right-side
// drawer (added below) replaces this with a history
// browser + search. Clear-chat itself stays available
// at the API layer (Node.DeleteHistory + app.ClearHistory)
// for future use, but no UI surface exposes it.

// ----- history drawer (v1.1, 2026-06-27) -----
//
// Right-side overlay drawer toggled by the composer
// toolbar's 📜 button. Aggregates every chat message
// across every peer (newest first), with a search box
// that filters by peer display name + message body.
//
// Data flow:
//   - state.history is populated lazily: only the
//     currently selected peer's history is loaded by
//     selectPeer(). Opening the drawer calls History()
//     for every peer we know about so the list is
//     complete (otherwise peers you've never clicked
//     would show zero messages).
//   - renderHistoryList is called on every peer:event
//     and message:event WHILE THE DRAWER IS OPEN so new
//     messages appear live (no need to close + reopen).
//   - Click a row → switch the chat panel to that peer
//     + close the drawer (the row's body contains the
//     peer's PeerID; we call selectPeer with that).
async function toggleHistoryDrawer() {
  const drawer = document.getElementById('history-drawer');
  if (!drawer) return;
  const wasOpen = state.historyDrawerOpen;
  state.historyDrawerOpen = !wasOpen;
  drawer.classList.toggle('open', state.historyDrawerOpen);
  drawer.setAttribute('aria-hidden', state.historyDrawerOpen ? 'false' : 'true');
  if (state.historyDrawerOpen) {
    await refreshHistoryList();
  }
}

async function refreshHistoryList() {
  // Make sure state.history has every known peer's
  // messages AND every known group's messages.
  // selectPeer() / selectGroup() only load the
  // currently selected conversation; the rest stay
  // empty until clicked. Without this lazy fetch the
  // drawer would show only the open conversation's
  // messages on first open.
  const ids = new Set<string>();
  for (const p of state.peers) ids.add(p.PeerID);
  if (state.selectedId) ids.add(state.selectedId);
  if (state.selfId) ids.add(state.selfId);
  for (const gid of state.groups) ids.add(gid.group_id);
  for (const pid of ids) {
    if (!state.history.has(pid)) {
      try {
        if (isGroupId(pid)) {
          // v1.1 (2026-06-28) hotfix: history drawer
          // used to display group messages with their
          // raw rendered ID ("g_<64hex>") because the
          // peerName lookup fell through to shortId().
          // Now we fetch per-group chat.enc via
          // HistoryGroup so the row can render with the
          // group's display name.
          const r = await HistoryGroup(pid);
          state.history.set(pid, (r && r.messages) || []);
        } else {
          const h = (await History(pid)) as node.Message[];
          state.history.set(pid, h || []);
        }
      } catch {
        state.history.set(pid, []);
      }
    }
  }
  renderHistoryList();
}

function renderHistoryList() {
  const body = document.getElementById('history-body');
  const searchEl = document.getElementById('history-search') as HTMLInputElement | null;
  if (!body) return;
  const q = (searchEl?.value || '').trim().toLowerCase();
  // Aggregate every message across every peer in state.
  // Sort newest first (timestamps are RFC3339 strings;
  // Date(...) is monotonic enough for a chat list).
  type Row = { peer: string; peerName: string; msg: node.Message; convLabel: string };
  const rows: Row[] = [];
  // Compute the conversation label ONCE per pid (only used
  // for the row's `data-peer` routing and search hit on the
  // conversation name — NOT as the per-row display name).
  // v1.1 (2026-06-28) hotfix: peerName for a row is the
  // SENDER, not the conversation. Previously we computed
  // peerName per pid (group ID → "群 X") and stamped it on
  // every row, which made every group message — even ones
  // I typed — render as "群 X" instead of "我". This is
  // wrong: the drawer is asking "who said this?" per row,
  // not "which conversation is this?". For 1:1 chats this
  // incidentally happened to be the same value (outbound
  // shows "我", inbound shows the other party's name); for
  // groups the conversation routing was drowning out the
  // actual sender identity (Alice's message and my message
  // both → "群 X"). Per-row peerName is correct for both.
  for (const [pid, msgs] of state.history) {
    if (!msgs) continue;
    let convLabel: string;
    if (isGroupId(pid)) {
      const g = state.groups.find(gg => gg.group_id === pid);
      convLabel = g ? `群 ${g.group_name}` : '(未知群)';
    } else if (pid === state.selfId) {
      convLabel = '我';
    } else {
      const peerInfo = state.peers.find(p => p.PeerID === pid);
      convLabel = peerInfo ? peerDisplay(peerInfo) : shortId(pid);
    }
    for (const m of msgs) {
      const peerName = senderNameForRow(pid, m);
      if (q) {
        const bodyHit = (m.Body || '').toLowerCase().includes(q);
        const peerHit = peerName.toLowerCase().includes(q);
        const convHit = convLabel.toLowerCase().includes(q);
        if (!bodyHit && !peerHit && !convHit) continue;
      }
      rows.push({ peer: pid, peerName, msg: m, convLabel });
    }
  }
  rows.sort((a, b) => {
    const ta = a.msg.Timestamp ? new Date(a.msg.Timestamp).getTime() : 0;
    const tb = b.msg.Timestamp ? new Date(b.msg.Timestamp).getTime() : 0;
    return tb - ta;
  });
  if (rows.length === 0) {
    body.innerHTML = `<div class="history-empty">${q ? '没有匹配的聊天记录' : '还没有聊天记录'}</div>`;
    return;
  }
  body.innerHTML = rows.map(r => {
    const isSelf = r.msg.Direction === 'out';
    // File messages keep their essence in the history list
    // (icon + name + size) just like the chat panel — they
    // are NOT reduced to "📎 <name>" text. User feedback
    // 2026-06-27: history file rows must support the same
    // right-click open / reveal / copy actions as live
    // bubbles. v1.1.
    let bodyHtml: string;
    if ((r.msg.Body || '').startsWith('file://')) {
      const rest = r.msg.Body.slice('file://'.length);
      const pipe = rest.indexOf('|');
      const name = pipe >= 0 ? rest.slice(0, pipe) : rest;
      const size = pipe >= 0 ? rest.slice(pipe + 1) : '';
      const filePath = r.msg.LocalPath || '';
      bodyHtml = `
        <div class="history-item-file"
             data-file-name="${escapeHtml(name)}"
             data-file-path="${escapeHtml(filePath)}"
             title="双击打开 · 右键更多">
          <div class="file-icon">
            <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M14 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V8z"/><path d="M14 2v6h6"/></svg>
          </div>
          <div class="file-info">
            <div class="file-name">${highlightQuery(escapeHtml(name), q)}</div>
            <div class="file-meta">${size ? escapeHtml(size) + ' · ' : ''}文件</div>
          </div>
        </div>
      `;
    } else {
      const bodyText = historyBodyText(r.msg.Body);
      bodyHtml = `<span class="history-item-text">${highlightQuery(escapeHtml(bodyText), q)}</span>`;
    }
    return `
      <div class="history-item"
           data-peer="${escapeHtml(r.peer)}"
           data-ts="${escapeHtml(r.msg.Timestamp || '')}"
           data-dir="${escapeHtml(r.msg.Direction || '')}">
        <div class="history-item-meta">
          <span class="history-item-peer ${isSelf ? 'self' : ''}">${escapeHtml(r.peerName)}</span>
          <span class="history-item-conv">${escapeHtml(r.convLabel)}</span>
          <span>${fmtTime(r.msg.Timestamp)}</span>
        </div>
        <div class="history-item-body">${bodyHtml}</div>
      </div>
    `;
  }).join('');
  // File rows: right-click → file context menu (open /
  // reveal / copy), double-click → open file. v1.1
  // (2026-06-27). Row click is handled below (scroll-to-
  // message); file interaction is wired separately so
  // clicking the file doesn't conflict with row click.
  body.querySelectorAll<HTMLElement>('.history-item-file').forEach(el => {
    el.addEventListener('contextmenu', (ev) => {
      const e = ev as MouseEvent;
      e.preventDefault();
      e.stopPropagation();
      const name = el.getAttribute('data-file-name') || '';
      const dataPath = el.getAttribute('data-file-path') || '';
      showFileContextMenu(e.clientX, e.clientY, name, dataPath);
    });
    el.addEventListener('dblclick', (ev) => {
      ev.stopPropagation();
      const name = el.getAttribute('data-file-name') || '';
      const dataPath = el.getAttribute('data-file-path') || '';
      void openFileMessage(name, dataPath);
    });
  });
  // Click → switch to that peer + close drawer + scroll
  // to the exact message + brief flash highlight so the
  // user sees where they landed. The scroll happens AFTER
  // renderMessages() completes (requestAnimationFrame) so
  // the target .msg node is in the DOM by the time we
  // query for it. v1.1 (2026-06-27).
  body.querySelectorAll<HTMLElement>('.history-item').forEach(el => {
    el.addEventListener('click', () => {
      const pid = el.getAttribute('data-peer') || '';
      const ts = el.getAttribute('data-ts') || '';
      const dir = el.getAttribute('data-dir') || 'in';
      if (!pid) return;
      // v1.1 (2026-06-28) hotfix: row click was
      // unconditionally calling selectPeer, which sent
      // History(g_<64hex>) and surfaced empty results.
      // Now dispatch on group vs 1:1.
      if (state.selectedId !== pid) {
        if (isGroupId(pid)) void selectGroup(pid);
        else void selectPeer(pid);
      }
      void toggleHistoryDrawer();
      // The chat panel renders after selectPeer resolves;
      // wait one frame so the new peer's messages are in
      // the DOM before we try to find + scroll to the
      // target. Without this, the first query runs before
      // renderMessages has finished its innerHTML rewrite.
      requestAnimationFrame(() => {
        const sel = `.messages .msg[data-msg-ts="${CSS.escape(ts)}"][data-msg-peer="${CSS.escape(pid)}"][data-msg-dir="${CSS.escape(dir)}"]`;
        const target = document.querySelector(sel);
        if (target) {
          target.scrollIntoView({ behavior: 'smooth', block: 'center' });
          target.classList.add('msg-flash');
          window.setTimeout(() => target.classList.remove('msg-flash'), 1600);
        }
      });
    });
  });
}

// historyBodyText flattens a chat body for the history
// list view. Plain text shows verbatim; "file://" bodies
// (file message marker from the protocol layer) show as
// "[文件] <name>" with the size, since the drawer list is
// about skim-and-jump, not full content rendering.
function historyBodyText(body: string): string {
  if (!body) return '';
  if (body.startsWith('file://')) {
    const rest = body.slice('file://'.length);
    const pipe = rest.indexOf('|');
    const name = pipe >= 0 ? rest.slice(0, pipe) : rest;
    return `📎 ${name}`;
  }
  return body;
}

// highlightQuery wraps every case-insensitive match of
// query inside <mark> for visual highlight. Special
// regex chars in query are escaped so user input is
// safe.
function highlightQuery(text: string, q: string): string {
  if (!q) return text;
  const safe = q.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
  const re = new RegExp(`(${safe})`, 'gi');
  return text.replace(re, '<mark>$1</mark>');
}

// cancelFileInFlight sends App.CancelFile and visually
// disables the cancel button (so a double-click doesn't
// spam the bridge). The actual transition to "已取消"
// state comes from the file:event 'done' event with
// err="已取消" that core publishes when the sender
// goroutine unwinds. v1.1 (2026-06-27).
async function cancelFileInFlight(fileID: string) {
  // Disable the button right away so the user gets
  // immediate feedback (cancel can take up to one chunk
  // worth of bytes + a network round-trip).
  const btn = document.querySelector<HTMLElement>(
    `.file-cancel[data-cancel-file-id="${CSS.escape(fileID)}"]`
  );
  if (btn) {
    btn.setAttribute('disabled', 'disabled');
    btn.classList.add('cancelling');
  }
  try {
    const err = await CancelFile(fileID);
    if (err) {
      toast(`取消失败: ${err}`, 'error');
      // Re-enable so the user can try again.
      if (btn) {
        btn.removeAttribute('disabled');
        btn.classList.remove('cancelling');
      }
    }
  } catch (e) {
    toast(`取消失败: ${e}`, 'error');
    if (btn) {
      btn.removeAttribute('disabled');
      btn.classList.remove('cancelling');
    }
  }
}

// ----- file transfer helpers -----
//
// Files are sent on selection (📎 picker via the native
// OS dialog) or drop, not staged into a pending card.
// The core publishes a "file://" chat message on
// completion, so the receiver sees the file bubble in
// the conversation and the sender sees their own
// outgoing file bubble — no need for an intermediate
// "ready to send" UI step.
//
// Picker history (2026-06-25 → 2026-06-27):
//
//   v1: SendFileContent(peer, name, Array.from(uint8)).
//       Sent the whole file in one IPC call. 50 MiB
//       JSON.stringify froze the UI for several seconds.
//
//   v2: SendFileStart / SendFileChunk / SendFileFinish.
//       Streamed 1 MiB chunks via IPC into a staging file
//       under <data-dir>/sent/, then ran Node.SendFile
//       on the staging copy. User saw TWO progress
//       runs (staging + transfer) and "排队中…" for
//       several seconds while chunks went through IPC.
//
//   v3: SendFileOpen / SendFileChunk / SendFileClose
//       with io.Pipe. Removed the staging file so
//       bytes flow JS → IPC → pipe → encrypt → wire
//       in one pass. But the chunk-writing phase still
//       gave "排队中…" for ~half a second per MiB.
//
//   v4 (current): runtime.OpenFileDialog + SendFilePath.
//       The native OS file picker hands the real on-disk
//       path straight to Go. core opens the file
//       directly with os.Open and hands the *os.File to
//       Node.SendFile — same code path as drag-and-drop.
//       No JS byte streaming at all. The placeholder
//       bubble appears synchronously and file:event
//       progress starts within ~100 ms. User reaction
//       drove this: "go can handle files, why is JS
//       doing the transfer?" — the answer was the
//       HTML <input type=file> was sandbox-limited;
//       runtime.OpenFileDialog is not.
//
// appendFileBubble inserts a single file bubble at the
// END of the messages container (after the system
// hint) so the user sees it appear immediately. We
// don't go through renderMessages because that would
// re-render the whole conversation and lose the
// bubble's data-file-id reference (which the file:event
// handler uses to update progress in place).
function appendFileBubble(fileID: string) {
  const el = document.getElementById('messages');
  if (!el) return;
  const fb = state.fileBubbles.get(fileID);
  if (!fb) return;
  el.insertAdjacentHTML('beforeend', renderFileBubble(fb));
}

// updateFileBubble refreshes the progress bar / speed
// number on an existing bubble in place. Cheaper than
// re-rendering the whole message list. Single-phase
// now: sent = total bytes sent across the whole
// pipeline (Go reads from disk → encrypt → wire).
//
// `total` is the file's full size from the Go-side
// FileEvent (always present in the payload). We latch
// it into fb.size on the FIRST progress event so the
// bar percentage works correctly from that point on;
// otherwise fb.size stays at the placeholder's 0 and
// every pct computation reads 0% (the bug that left
// the bar stuck at 0% with speed numbers animating —
// 2026-06-27).
function updateFileBubble(fileID: string, sent: number, total: number, bps: number) {
  const fb = state.fileBubbles.get(fileID);
  if (!fb) return;
  fb.sent = sent;
  fb.bps = bps;
  if (total > 0 && fb.size === 0) fb.size = total;
  const root = findFileBubbleEl(fileID);
  if (!root) return;
  const pct = fb.size > 0 ? Math.min(100, Math.floor(sent * 100 / fb.size)) : 0;
  const fill = root.querySelector('.file-bar-fill') as HTMLElement | null;
  if (fill) fill.style.width = pct + '%';
  const status = root.querySelector('.file-status');
  if (status) {
    const bpsStr = bps > 0 ? humanSize(bps) + '/s' : '';
    const etaStr = formatEta(fb.size - sent, bps);
    const parts = [`发送中 · ${pct}%`];
    if (bpsStr) parts.push(bpsStr);
    if (etaStr) parts.push(etaStr);
    status.textContent = parts.join(' · ');
  }
}

// markFileBubbleDone flips the bubble into its final
// state. ok=true → green bar + "已发送"; ok=false → red
// bar + the error message.
//
// `total` is the file's full size from the Go-side
// FileEvent (latched into fb.size if the file finished
// before the first progress event, e.g. tiny files
// that complete in <100 ms — those skip the progress
// stream entirely; without this fallback the bubble
// would show "已发送 · 0 B" because fb.size was still 0).
//
// Failure bubbles only show a SHORT reason in the card
// (the receiver's reply is often a 200+ char Go-formatted
// error like "filetransfer: receiver reported failure: open
// C:\Users\foo\...: The process cannot access the file…").
// Pasting that into the bubble blows up the card and
// leaks the receiver's filesystem path to the sender's UI
// (it's the sender's own peer here, but still noisy).
// The full errMsg stays available via the bubble's title
// tooltip + the file:event 'done' handler's toast below.
function markFileBubbleDone(fileID: string, ok: boolean, errMsg: string, total: number) {
  const fb = state.fileBubbles.get(fileID);
  if (!fb) return;
  fb.err = ok ? '' : errMsg;
  if (total > 0 && fb.size === 0) fb.size = total;
  if (ok) fb.sent = fb.size; // bar at 100%
  const root = findFileBubbleEl(fileID);
  if (!root) return;
  const fill = root.querySelector('.file-bar-fill') as HTMLElement | null;
  if (fill) fill.style.width = ok ? '100%' : '0%';
  const bar = root.querySelector('.file-bar');
  // Cancelled is its own visual state — neither success
  // nor failure. User explicitly hit ✕ so we don't want
  // a green bar (suggests success) or red (suggests
  // error); gray reads "stopped, neither good nor bad".
  // 2026-06-27 user feedback: cancelled was previously
  // indistinguishable from a real error, both rendered as
  // "失败: ..." in light pink on green — illegible.
  const cancelled = !ok && errMsg === '已取消';
  if (bar) {
    bar.classList.remove('file-bar-done', 'file-bar-failed', 'file-bar-cancelled');
    bar.classList.add(ok ? 'file-bar-done' : (cancelled ? 'file-bar-cancelled' : 'file-bar-failed'));
  }
  const status = root.querySelector('.file-status');
  if (status) {
    status.classList.remove('file-status-done', 'file-status-failed', 'file-status-cancelled');
    if (ok) {
      status.textContent = `已发送 · ${humanSize(fb.size)}`;
      status.classList.add('file-status-done');
    } else if (cancelled) {
      status.textContent = '已取消';
      status.classList.add('file-status-cancelled');
    } else {
      // Short, friendly summary. The full reason is in
      // the bubble title (tooltip) and the toast — never
      // both on the same screen real estate at once.
      status.textContent = '发送失败';
      status.classList.add('file-status-failed');
      status.setAttribute('title', errMsg || '未知错误');
    }
  }
  // Stash the full errMsg on the bubble root so a future
  // "view details" affordance could surface it without
  // touching fb state.
  if (!ok) {
    const bubble = root.querySelector('.bubble.file-bubble') as HTMLElement | null;
    if (bubble) bubble.setAttribute('data-file-error', errMsg || '');
  }
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
  // v1.1 (2026-06-28): dispatch on group vs 1:1 — group
  // conversations go through SendGroupMessage which
  // broadcasts via per-member channels.
  document.getElementById('composer')!.addEventListener('submit', async (ev) => {
    ev.preventDefault();
    if (!state.selectedId) return;
    const input = document.getElementById('composer-input') as HTMLTextAreaElement;
    const text = input.value.trim();
    if (!text) return;
    const isGroup = isGroupId(state.selectedId);
    let err = '';
    if (isGroup) {
      // SendGroupMessage returns GroupMessageResult
      // { status, err }; empty err means success.
      const r = await SendGroupMessage(state.selectedId, text);
      err = (r && r.err) || '';
    } else {
      // SendText returns the error string directly;
      // empty string means success. (Wails binding
      // convention: first return value only.)
      err = await SendText(state.selectedId, text);
    }
    if (err) {
      toast(`发送失败: ${err}`);
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
  document.getElementById('btn-history')!.addEventListener('click', () => toggleHistoryDrawer());
  // v1.1.1 (2026-06-29): group settings (⋯) button →
  // open the WeChat-style right-side panel.
  document.getElementById('btn-group-settings')!.addEventListener('click', () => void openGroupSettings());
  document.getElementById('btn-group-settings-close')!.addEventListener('click', () => closeGroupSettings());

  // Sidebar self header: click to set your own alias.
  // This is the only path the user has to set their
  // broadcast display name — no first-launch popup, no
  // dedicated toolbar button. The "点我设置别名" hint
  // inside the box is the discoverability affordance.
  document.querySelector('.me')!.addEventListener('click', () => promptMyAlias());

  // Sidebar footer mini buttons.
  document.getElementById('btn-scan')!.addEventListener('click', () => promptScan());
  // Debug "测试" button in the sidebar footer. Calls the
  // Go-side DebugReveal / DebugOpen which snapshot the
  // process table before and after the launch to detect
  // whether explorer.exe / Finder / nautilus actually
  // started. The result is shown in a toast so we can
  // see at a glance whether the launch path works.
  document.getElementById('btn-test-reveal')!.addEventListener('click', async () => {
    const r = await DebugReveal('');
    if (r.startsWith('OK')) {
      toast('reveal: ' + r);
    } else {
      toast('reveal FAIL: ' + r);
    }
  });

  // Create-group modal (v1.1, 2026-06-28).
  document.getElementById('btn-create-group')!.addEventListener('click', () => openCreateGroupModal());
  document.getElementById('btn-create-group-close')!.addEventListener('click', () => closeCreateGroupModal());
  document.getElementById('btn-create-group-cancel')!.addEventListener('click', () => closeCreateGroupModal());
  document.getElementById('btn-create-group-submit')!.addEventListener('click', () => void submitCreateGroup());
  // Submit on Enter in the name field.
  document.getElementById('create-group-name')!.addEventListener('input', () => updateCreateGroupSubmitButton());
  document.getElementById('create-group-name')!.addEventListener('keydown', (ev) => {
    const ke = ev as KeyboardEvent;
    if (ke.key === 'Enter') {
      ke.preventDefault();
      if (!(document.getElementById('btn-create-group-submit') as HTMLButtonElement).disabled) {
        void submitCreateGroup();
      }
    }
  });
  // Close the modal on backdrop click (clicking the
  // overlay itself, not the dialog — the dialog click
  // bubbles up here too but stopPropagation in the
  // dialog handler would block it; we check the
  // event target instead).
  document.getElementById('create-group-modal')!.addEventListener('click', (ev) => {
    if (ev.target === ev.currentTarget) closeCreateGroupModal();
  });
  // Esc closes the modal too.
  document.addEventListener('keydown', (ev) => {
    const ke = ev as KeyboardEvent;
    if (ke.key !== 'Escape') return;
    const modal = document.getElementById('create-group-modal');
    if (modal && !modal.classList.contains('modal-hidden')) {
      closeCreateGroupModal();
    }
  });
  // Update submit button when a member checkbox toggles.
  // Event delegation on the list — cheaper than N
  // listeners if the user has a 20-peer roster.
  document.getElementById('create-group-members')!.addEventListener('change', () => updateCreateGroupSubmitButton());
  // Right-click on a group row → context menu with
  // "退出群聊" option. v1.1 (2026-06-28) hotfix:
  // previous version jumped straight to a confirm
  // dialog, which surprised users who right-clicked
  // expecting a menu (Windows convention is menu-first
  // → action via menu item, not action-on-right-click).
  document.getElementById('group-list')!.addEventListener('contextmenu', (ev) => {
    const e = ev as MouseEvent;
    const row = (e.target as HTMLElement).closest<HTMLElement>('.peer.group');
    if (!row) return;
    e.preventDefault();
    const gid = row.getAttribute('data-group');
    if (gid) void showGroupContextMenu(e.clientX, e.clientY, gid);
  });

  // 📎 picker: open the native OS file dialog via
  // PickFile. core gets the real on-disk path back and
  // hands it to SendFilePath, which opens the file
  // and runs the same Node.SendFile path as drag-and-
  // drop. No bytes cross the JS/Go boundary — core
  // reads the file straight from disk into the SM4-GCM
  // send loop.
//
// Why not <input type=file> + Wails IPC streaming (the
// 2026-06-26 first attempt): the browser File API hides
// the real path on modern engines, so core can't
// `os.Open` it, can't wire up "right-click → open
// folder", and has to stream bytes through IPC. That
// gave users a visible "排队中…" pause while JS pushed
// chunks into core. The native dialog bypasses the
// browser sandbox entirely and hands the path straight
// to Go.
  document.getElementById('btn-attach')!.addEventListener('click', async () => {
    if (!state.selectedId) {
      toast('先选一个 peer');
      return;
    }
    const peerId = state.selectedId;
    const isGroup = isGroupId(peerId);

    // 1. Native OS picker → path.
    //    Wails v2 only exposes the FIRST return value of a
    //    bound Go method, so PickFile / SendFilePath both
    //    return structs (PickFileResult / SendFilePathResult)
    //    instead of tuples — see app/app.go for the why.
    const pickRes = await PickFile();
    const path = pickRes.path;
    if (!path) {
      // 'cancelled' = user dismissed the dialog, silent.
      // Anything else is a real error.
      if (pickRes.err && pickRes.err !== 'cancelled') {
        toast('选文件失败: ' + pickRes.err);
      }
      return;
    }

    // 2. core opens the file + starts the transfer.
    //    v1.1 (2026-06-28): dispatch on group vs 1:1.
    //    Group sends broadcast per-member via SendGroupFile
    //    and the returned baseFileID is what we use as
    //    the bubble key (per-member fileIDs are derived
    //    inside Go and not surfaced to the GUI's progress
    //    stream — single bubble per logical send).
    let baseFileID = '';
    if (isGroup) {
      baseFileID = crypto.randomUUID();
      const sendRes = await SendGroupFile(peerId, path, baseFileID);
      if (sendRes.err) {
        toast('发送失败: ' + sendRes.err, 'error');
        return;
      }
      if (sendRes.sent === 0) {
        // No online members — file is still recorded in
        // the chat log but nothing went out. Warn so the
        // user understands why no live progress appears.
        toast('群内没有在线成员，文件已记录在聊天中（Phase 5 后离线消息会送达）', 'error');
      }
    } else {
      const sendRes = await SendFilePath(peerId, path);
      baseFileID = sendRes.fileID;
      if (!baseFileID) {
        toast('发送失败: ' + (sendRes.err || 'unknown'), 'error');
        return;
      }
    }

    // 3. Placeholder bubble. Progress comes from
    //    file:event which fires within ~100 ms.
    //    localPath is set NOW (not on done) so right-
    //    click on the live bubble already reveals the
    //    user's folder — the picker has the real path
    //    in hand the moment OpenFileDialog returns, no
    //    reason to gate the affordance on completion.
    const name = path.replace(/^.*[\\/]/, '');
    state.fileBubbles.set(baseFileID, {
      fileID: baseFileID, name,
      size: 0, // populated by first file:event 'progress'
      sent: 0, bps: 0,
      err: '',
      peerId,
      localPath: path,
      startTime: Date.now(),
    });
    appendFileBubble(baseFileID);
    const el = document.getElementById('messages');
    if (el) el.scrollTop = el.scrollHeight;
  });

  // Track scroll position so renderMessages can decide
  // whether to stick to the bottom (chat-style) or leave
  // the user where they are reading history.
  const messagesEl = document.getElementById('messages')!;
  messagesEl.addEventListener('scroll', () => {
    state.nearBottom = isNearBottom(messagesEl);
  });

  // File message interactions.
  //
  // Three trigger paths to be paranoid about WebView2
  // event quirks on different hosts:
  //   1. dblclick       → open file with default app
  //   2. contextmenu    → show action menu
  //   3. mousedown(btn2)→ show action menu (fallback if
  //                       contextmenu is swallowed)
  //   4. click          → show action menu (last-resort
  //                       fallback; also helps users who
  //                       don't know about right-click)
  //
  // All four are bound on the document with closest
  // delegation so they survive every innerHTML rewrite
  // from renderMessages AND work no matter which element
  // inside the bubble the user clicked.
  const findBubble = (target: EventTarget | null): {
    bubble: HTMLElement;
    name: string;
    dataPath: string;
  } | null => {
    const t = target as HTMLElement;
    if (!t) return null;
    const bubble = t.closest('.file-bubble') as HTMLElement | null;
    if (!bubble) return null;
    return {
      bubble,
      name: bubble.getAttribute('data-file-name') || '',
      dataPath: bubble.getAttribute('data-file-path') || '',
    };
  };
  const showMenu = (ev: MouseEvent, info: { name: string; dataPath: string }) => {
    ev.preventDefault();
    ev.stopPropagation();
    showFileContextMenu(ev.clientX, ev.clientY, info.name, info.dataPath);
  };
  document.addEventListener('dblclick', (ev) => {
    const info = findBubble(ev.target);
    if (!info) return;
    void openFileMessage(info.name, info.dataPath);
  });
  document.addEventListener('contextmenu', (ev) => {
    const info = findBubble(ev.target);
    if (!info) return;
    showMenu(ev as MouseEvent, info);
  });
  document.addEventListener('mousedown', (ev) => {
    if (ev.button !== 2) return; // right button only
    const info = findBubble(ev.target);
    if (!info) return;
    showMenu(ev, info);
  });
  // Last-resort: left-click on a file bubble shows the
  // menu. This guarantees the user has SOME way to
  // interact with the file even if the WebView2 host
  // has stripped all right-click behaviors. The handler
  // is run BEFORE the outside-click closer below, but
  // because both are 'click' listeners on document, the
  // one registered later runs first; the outside-click
  // is registered after this one, so it fires first and
  // closes any existing menu before we open a new one
  // (the old menu is hidden, the new one is shown).
  document.addEventListener('click', (ev) => {
    const target = ev.target as HTMLElement;
    if (!target) return;
    // Cancel button (✕) on in-flight file bubbles. v1.1
    // (2026-06-27). We stop propagation so the click
    // doesn't also fire the bubble's "show context menu"
    // path below.
    const cancelBtn = target.closest<HTMLElement>('.file-cancel');
    if (cancelBtn) {
      ev.preventDefault();
      ev.stopPropagation();
      const fid = cancelBtn.getAttribute('data-cancel-file-id') || '';
      if (!fid) return;
      void cancelFileInFlight(fid);
      return;
    }
    const info = findBubble(ev.target);
    if (!info) return;
    showFileContextMenu(ev.clientX, ev.clientY, info.name, info.dataPath);
  });
  // Click outside the menu (and outside any file bubble)
  // to close it. Registered AFTER the file-bubble click
  // handler so this fires first; if the click target is
  // not a file bubble, we just close the menu.
  document.addEventListener('click', (ev) => {
    const t = ev.target as HTMLElement;
    if (!t) return;
    if (t.closest('#file-ctx-menu')) return;
    if (t.closest('.file-bubble')) return; // bubble click handled above
    hideFileContextMenu();
  });
  // Esc closes the menu + the history drawer (whichever
// is open; both close if both are).
  document.addEventListener('keydown', (ev) => {
    if (ev.key === 'Escape') {
      hideFileContextMenu();
      if (state.historyDrawerOpen) {
        void toggleHistoryDrawer();
      }
    }
  });

  // History drawer wiring. The 📜 button toggles the
  // drawer; the X button always closes; the search input
  // re-filters the rendered list (no re-fetch).
  document.getElementById('btn-history-close')!.addEventListener('click', () => {
    if (state.historyDrawerOpen) void toggleHistoryDrawer();
  });
  const searchInput = document.getElementById('history-search') as HTMLInputElement | null;
  if (searchInput) {
    searchInput.addEventListener('input', () => {
      if (state.historyDrawerOpen) renderHistoryList();
    });
  }
  // Outside-click closes the drawer. v1.1 (2026-06-27)
  // user feedback: clicking the chat area should also
  // close the drawer, not just the X button or Esc.
  // Registered LAST so it doesn't preempt the file-bubble
  // / cancel-button / menu-closer handlers above.
  document.addEventListener('click', (ev) => {
    if (!state.historyDrawerOpen) return;
    const t = ev.target as HTMLElement | null;
    if (!t) return;
    // Clicks inside the drawer (header / search / list) —
    // let them propagate normally.
    if (t.closest('#history-drawer')) return;
    // Click on the toggle button (📜) is handled by its
    // own listener (toggles). Don't fight it here.
    if (t.closest('#btn-history')) return;
    // Anything else (chat area, sidebar, peer list, …)
    // closes the drawer.
    void toggleHistoryDrawer();
  });

  // Live event streams from Go.
  EventsOn('peer:event', (_ev: any) => {
    refreshAll();
    // New peer may have history we haven't fetched yet;
    // refresh the drawer if it's open (cheap).
    if (state.historyDrawerOpen) void refreshHistoryList();
  });
  // v1.1 (2026-06-28): group lifecycle events from Go.
  // Triggered by:
  //   - pkg/node CreateGroup (we just created a group)
  //   - pkg/node AcceptGroupInvite (we auto-accepted
  //     someone inviting us — InviterHex identifies them)
  //   - pkg/node LeaveGroup (we left a group)
  // We refresh the sidebar's group list in all cases
  // and surface a toast for the "you were invited" path
  // so the user knows where the new row came from.
  EventsOn('group:event', (ev: any) => {
    if (!ev || !ev.Type) return;
    void onGroupEvent(ev);
  });
  EventsOn('message:event', (m: node.Message) => {
    if (!m || !m.PeerID) return;
    // v1.1 (2026-06-28): the conversation key is just
    // m.PeerID — for 1:1 it's the peer's 32-char hex;
    // for groups it's the rendered "g_<64hex>" string.
    // The history Map already keys on this; we only need
    // to dispatch the unread-badge bump into the right
    // counter (unreadCount for 1:1, groupUnread for group).
    const list = state.history.get(m.PeerID) || [];
    list.push(m);
    state.history.set(m.PeerID, list);
    if (state.selectedId === m.PeerID) {
      renderMessages();
    } else if (m.Direction === 'in') {
      if (isGroupId(m.PeerID)) {
        const n = (state.groupUnread.get(m.PeerID) || 0) + 1;
        state.groupUnread.set(m.PeerID, n);
        renderGroupList();
      } else {
        const n = (state.unreadCount.get(m.PeerID) || 0) + 1;
        state.unreadCount.set(m.PeerID, n);
        renderPeerList();
      }
    }
    // Live-update the history drawer if it's open so
    // new messages appear without a manual refresh.
    if (state.historyDrawerOpen) renderHistoryList();
  });
  // File-transfer events (progress + done) keyed by
  // fileID. Picker-route bubbles listen on these to draw
  // the progress bar in place; drag-and-drop bubbles
  // ignore them (their fileID is "" so the lookup is a
  // no-op).
  EventsOn('file:event', (ev: any) => {
    if (!ev || !ev.fileID) return;
    if (ev.type === 'progress') {
      // Single-phase: file:event 'progress' IS the only
      // progress stream the frontend reads. ev.total
      // carries the file size; we latch it on the first
      // event so the bar percentage works from then on.
      updateFileBubble(ev.fileID, ev.sent ?? 0, ev.total ?? 0, ev.bytesPerSec ?? 0);
    } else if (ev.type === 'done') {
      // ev.total is 0 for failed transfers (Go doesn't
      // know the file size if the open failed) — that's
      // fine, the failure path doesn't read it.
      markFileBubbleDone(ev.fileID, !!ev.ok, ev.err || '', ev.total ?? 0);
      if (!ev.ok) {
        if (ev.err === '已取消') {
          // User-initiated cancel — distinct from a real
          // failure. No scary red, no "失败:" prefix.
          // 2026-06-27: the previous toast said
          // "发文件失败: 已取消" in orange and read like a
          // software bug. Now: soft dark-glass pill with
          // a neutral message.
          toast('已取消发送', 'info');
        } else {
          // The bubble shows a short "发送失败" status; the
          // toast carries the full reason so the user can
          // see what went wrong without it eating bubble
          // real estate. Truncate to one line to keep the
          // toast readable (toast lives 2.4s).
          const fullErr = ev.err || '未知错误';
          const oneLine = fullErr.replace(/\s+/g, ' ').slice(0, 120);
          toast(`发送失败: ${oneLine}${fullErr.length > 120 ? '…' : ''}`, 'error');
        }
      }
    }
  });
}

// ----- bootstrap -----
async function bootstrap() {
  mount();
  wireEvents();
  await refreshAll();

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
  // SendFileDragDrop kicks off a drag-and-drop transfer:
// creates the live placeholder bubble the same way the
// picker route does, then calls App.SendFile (which now
// returns {fileID, err} like SendFilePath — see
// app/app.go SendFile). We loop through `paths` so a
// multi-file drop sends all of them; previously we only
// sent paths[0] and silently dropped the rest.
//
// The placeholder is registered BEFORE the SendFile
// promise resolves. If core fails (open: / stat: /
// not-a-regular-file), we look up the placeholder by
// fileID (the one SendFile returned) and mark it failed
// in place — the user sees an error card instead of a
// silently vanished bubble.
async function SendFileDragDrop(paths: string[]) {
  const peerId = state.selectedId;
  if (!peerId) {
    toast('先选一个 peer');
    return;
  }
  if (!paths || paths.length === 0) return;
  const isGroup = isGroupId(peerId);
  const list = document.getElementById('messages');
  for (const p of paths) {
    if (!p) continue;
    const name = p.replace(/^.*[\\/]/, '');
    let fileID = '';
    if (isGroup) {
      // v1.1 (2026-06-28): GUI generates the baseFileID
      // so the placeholder bubble can wire up before
      // Go resolves. The Go side derives per-member
      // fileIDs from this base by appending "_<shortHex>".
      fileID = crypto.randomUUID();
      const r = await SendGroupFile(peerId, p, fileID);
      const err = r.err || '';
      if (err) {
        toast(`发送失败: ${err || 'unknown'}`, 'error');
        continue;
      }
      if (r.sent === 0) {
        toast('群内没有在线成员，文件已记录在聊天中', 'error');
      }
    } else {
      // Provisional fileID — replaced by the backend-
      // returned ID once SendFile resolves. Until then
      // we don't put the placeholder in state.fileBubbles
      // (the file:event handler keys off the real fileID)
      // so the file:event 'progress' / 'done' streams will
      // only match once the real ID is wired in.
      //
      // SendFile is the modern Wails binding (Go returns
      // SendFilePathResult); see app/app.go SendFile.
      const r = await SendFile(peerId, p);
      fileID = r.fileID || '';
      const err = r.err || '';
      if (!fileID) {
        toast(`发送失败: ${err || 'unknown'}`, 'error');
        continue;
      }
    }
    state.fileBubbles.set(fileID, {
      fileID,
      name,
      size: 0,
      sent: 0,
      bps: 0,
      err: '',
      peerId,
      localPath: p,
      startTime: Date.now(),
    });
    appendFileBubble(fileID);
    if (list) list.scrollTop = list.scrollHeight;
  }
}

  OnFileDrop((_x, _y, paths) => {
    void SendFileDragDrop(paths || []);
  }, true);
}

bootstrap();
