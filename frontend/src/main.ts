// innerlink frontend — vanilla TypeScript, no framework.
//
// Talks to the Go side via the Wails-generated bindings in
// ../wailsjs/go/app/App and listens for runtime events
// ("peer:event", "message:event") emitted from app/app.go.
//
// Layout: 3-pane shell (sidebar peer list, chat panel,
// status bar). No external UI deps.
//
// Features this file implements (2026-06-24):
//   - sidebar: peer list with unread-message badge per row
//   - chat header: peer name + id + alias (rename) button
//     (ping/dial buttons removed; innerlink is LAN P2P and
//     the manual probes were CLI artifacts with no GUI
//     feedback)
//   - composer: text input + 📎 attach button + drag-and-drop
//     (Wails OnFileDrop with useDropTarget=true so drop is
//     only accepted on the composer area, not the whole
//     window). Pending file shows as a card with name +
//     size + cancel. Send button: text -> SendText,
//     file -> SendFile.
//   - file message rendering: incoming "file://" messages
//     show as a clickable file card with name + meta.

import './style.css';

import {
    ListPeers,
    SelfPeerID,
    SendText,
    SendFile,
    SendFileContent,
    SetAlias,
    Scan,
} from '../wailsjs/go/app/App';
import { node } from '../wailsjs/go/models';
import { EventsOn } from '../wailsjs/runtime/runtime';

// ----- in-memory state -----
type PeerInfo = node.PeerInfo;
type ChatMessage = node.Message;

interface UIState {
    selfId: string;
    peers: PeerInfo[];
    selfEntry: PeerInfo | null;
    selectedId: string | null;     // peer hex ID of the open conversation
    history: Map<string, ChatMessage[]>;
    aliases: Map<string, string>;
    // nearBottom reflects whether the messages panel is
    // currently scrolled within ~60px of the bottom.
    nearBottom: boolean;
    // peers we've already auto-aliased from their hostname
    autoAliased: Set<string>;
    // unreadCount tracks inbound messages received while
    // user wasn't looking at that peer's chat. Reset on
    // selection.
    unreadCount: Map<string, number>;
    // pendingFile is the staged attachment waiting to be
    // sent. Two source paths:
    //   - drag-drop: path is set, content is undefined
    //     (Go reads file from path via SendFile)
    //   - 📎 button: path is "", content holds the bytes
    //     (Go writes them to <data-dir>/outbox/ then calls
    //      SendFile via SendFileContent)
    pendingFile:
    | { name: string; size: number; path: string; content?: Uint8Array }
    | null;
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
    unreadCount: new Map(),
    pendingFile: null,
};

// ----- DOM helpers -----
function el<K extends keyof HTMLElementTagNameMap>(
    tag: K,
    props: Partial<HTMLElementTagNameMap[K]> & { className?: string; id?: string; title?: string } = {},
    ...children: (Node | string)[]
): HTMLElementTagNameMap[K] {
    const node = document.createElement(tag);
    Object.assign(node, props);
    for (const c of children) {
        node.append(c instanceof Node ? c : document.createTextNode(c));
    }
    return node;
}

function shortId(id: string): string {
    if (!id) return '';
    return id.slice(0, 8) + '…' + id.slice(-4);
}

function fmtSize(n: number): string {
    if (n < 1024) return n + ' B';
    if (n < 1024 * 1024) return (n / 1024).toFixed(1) + ' KB';
    if (n < 1024 * 1024 * 1024) return (n / 1024 / 1024).toFixed(1) + ' MB';
    return (n / 1024 / 1024 / 1024).toFixed(2) + ' GB';
}

function fmtTime(ts: unknown): string {
    const d = ts instanceof Date ? ts : new Date(ts as string);
    if (isNaN(d.getTime())) return '';
    return d.toLocaleTimeString('zh-CN', { hour: '2-digit', minute: '2-digit' });
}

function toast(msg: string) {
    const t = document.getElementById('toast')!;
    t.textContent = msg;
    t.classList.add('show');
    setTimeout(() => t.classList.remove('show'), 2400);
}

// ----- pending file -----
function setPendingFile(opts: { name: string; size: number; path?: string; content?: Uint8Array }) {
    state.pendingFile = {
        name: opts.name,
        size: opts.size,
        path: opts.path ?? '',
        content: opts.content,
    };
    renderPendingFile();
    updateComposerSendLabel();
}

function clearPendingFile() {
    state.pendingFile = null;
    renderPendingFile();
    updateComposerSendLabel();
}

function renderPendingFile() {
    const elPf = document.getElementById('pending-file')!;
    if (!state.pendingFile) {
        elPf.classList.remove('show');
        elPf.innerHTML = '';
        return;
    }
    const { name, size, path } = state.pendingFile;
    elPf.classList.add('show');
    elPf.innerHTML = '';
    elPf.append(
        el('span', { className: 'pf-icon' }, '📄'),
        el('span', { className: 'pf-info' },
            el('span', { className: 'pf-name', title: path }, name),
            el('span', { className: 'pf-meta' }, fmtSize(size)),
        ),
        (() => {
            const btn = el('button', { className: 'pf-cancel', title: 'cancel attachment', type: 'button' }, '✕');
            btn.addEventListener('click', () => clearPendingFile());
            return btn;
        })(),
    );
}

function updateComposerSendLabel() {
    const sendBtn = document.getElementById('composer-send') as HTMLButtonElement | null;
    if (!sendBtn) return;
    sendBtn.textContent = state.pendingFile ? 'send file' : 'send';
}

// ----- mount (one-shot at startup) -----
function mount() {
    const app = document.querySelector<HTMLDivElement>('#app')!;
    app.innerHTML = '';
    app.append(renderSidebar(), renderMain(), renderStatus());
    // toast container is appended as a static div, see below
    document.body.append(el('div', { className: 'toast', id: 'toast' }));
}

// ----- render -----
function renderSidebar(): HTMLElement {
    const sb = el('aside', { className: 'sidebar' });

    sb.append(
        el('div', { className: 'me' },
            el('div', { className: 'me-label' }, 'this device'),
            el('div', { className: 'me-name', id: 'me-name' }, '—'),
            el('div', { className: 'me-id', id: 'me-id' }, '…'),
            el('div', { className: 'me-status' },
                el('span', { className: 'led' }),
                el('span', { id: 'me-status' }, 'starting…'),
            ),
        ),
        el('div', { className: 'sidebar-header' },
            el('span', { className: 'sidebar-title' }, 'peers'),
            el('span', { className: 'sidebar-count', id: 'peer-count' }, '0'),
        ),
    );

    const list = el('div', { className: 'peer-list', id: 'peer-list' });
    if (state.peers.length === 0) {
        list.append(el('div', { className: 'empty' }, '尚未发现任何 peer'));
    } else {
        for (const p of state.peers) {
            list.append(renderPeerRow(p));
        }
    }
    sb.append(list);

    const scanForm = el('form', { className: 'sidebar-footer', id: 'scan-form' },
        el('input', { id: 'scan-input', type: 'text', placeholder: 'scan 192.168.1.0/24', autocomplete: 'off' }),
        el('button', { type: 'submit' }, 'scan'),
    );
    scanForm.addEventListener('submit', async e => {
        e.preventDefault();
        const inp = document.getElementById('scan-input') as HTMLInputElement;
        const cidr = inp.value.trim();
        if (!cidr) return;
        const r = await Scan(cidr);
        if (r) toast(`scan: ${r}`);
    });
    sb.append(scanForm);

    return sb;
}

function renderPeerRow(p: PeerInfo): HTMLElement {
    const dotClass = p.IsSelf
        ? 'peer-dot self'
        : (p.Online ? 'peer-dot online' : 'peer-dot offline');
    const name = p.IsSelf
        ? (p.Name || 'self')
        : (p.Name || (p.Hostname ? p.Hostname : shortId(p.PeerID)));
    const unread = state.unreadCount.get(p.PeerID) || 0;
    const active = p.PeerID === state.selectedId;

    const row = el('div', {
        className: 'peer' + (active ? ' active' : '') + (unread > 0 ? ' has-unread' : ''),
    });
    row.append(
        el('div', { className: dotClass }),
        el('div', { className: 'peer-info' },
            el('div', { className: 'peer-name' }, name),
            el('div', { className: 'peer-id' }, shortId(p.PeerID)),
        ),
    );
    if (unread > 0) {
        row.append(el('span', { className: 'badge' }, unread > 99 ? '99+' : String(unread)));
    }
    row.addEventListener('click', () => {
        state.selectedId = p.PeerID;
        state.unreadCount.set(p.PeerID, 0);
        mount();
    });
    return row;
}

function renderMain(): HTMLElement {
    const main = el('main', { className: 'chat' });
    const peer = state.peers.find(p => p.PeerID === state.selectedId);

    if (!peer || peer.IsSelf) {
        const head = el('div', { className: 'main-header' });
        head.append(
            el('h2', {}, 'innerlink'),
            el('div', { className: 'subtitle' }, '选择一个 peer 开始聊天'),
        );
        const noChat = el('div', { className: 'no-chat' }, '— 无聊天 —');
        main.append(head, noChat);
        return main;
    }

    // header
    const peerName = peer.Name || peer.Hostname || shortId(peer.PeerID);
    const head = el('div', { className: 'main-header' },
        el('h2', {}, peerName),
        el('div', { className: 'subtitle' }, shortId(peer.PeerID)),
    );
    // alias button (only if peer is not self)
    if (!peer.IsSelf) {
        const actions = el('div', { className: 'chat-actions' });
        const aliasBtn = el('button', { id: 'btn-alias' }, 'name…');
        aliasBtn.addEventListener('click', () => promptAlias(peer));
        actions.append(aliasBtn);
        head.append(actions);
    }

    // messages
    const msgs = el('div', { className: 'messages' });
    const peerMsgs = state.history.get(peer.PeerID) || [];
    for (const m of peerMsgs) {
        msgs.append(renderMessage(m));
    }
    // scroll to bottom
    requestAnimationFrame(() => { msgs.scrollTop = msgs.scrollHeight; });

    main.append(head, msgs);

    // composer
    main.append(renderComposer(peer));

    return main;
}

function renderMessage(m: ChatMessage): HTMLElement {
    if (m.Body.startsWith('file://')) {
        const fname = m.Body.slice('file://'.length);
        const msgEl = el('div', { className: 'message file ' + m.Direction.toLowerCase() });
        msgEl.append(
            el('div', { className: 'file-icon' }, '📄'),
            el('div', { className: 'file-info' },
                el('div', { className: 'file-name' }, fname),
                el('div', { className: 'file-meta' },
                    m.Direction === 'in' ? 'received file' : 'sent file'),
            ),
            el('div', { className: 'ts' }, fmtTime(m.Timestamp)),
        );
        return msgEl;
    }
    const msgEl = el('div', { className: 'message ' + m.Direction.toLowerCase() });
    msgEl.append(
        el('div', { className: 'body' }, m.Body),
        el('div', { className: 'ts' }, fmtTime(m.Timestamp)),
    );
    return msgEl;
}

function renderComposer(peer: PeerInfo): HTMLElement {
    const wrap = el('div', { className: 'composer-wrap' });

    // drop hint
    const dropHint = el('div', { className: 'drop-hint', id: 'drop-hint' }, 'drop file to attach');
    wrap.append(dropHint);

    // pending file card
    const pending = el('div', { className: 'pending-file', id: 'pending-file' });
    wrap.append(pending);

    // composer form
    const form = el('form', { className: 'composer', id: 'composer' });
    const attachBtn = el('button', {
        type: 'button', id: 'btn-attach', title: 'attach file', className: 'btn-attach',
    }, '📎');
    const input = el('input', {
        type: 'text', id: 'composer-input', className: 'composer-input',
        placeholder: '输入消息，回车发送', autocomplete: 'off',
    }) as HTMLInputElement;
    const sendBtn = el('button', {
        type: 'submit', id: 'composer-send', className: 'btn-send',
    }, state.pendingFile ? 'send file' : 'send') as HTMLButtonElement;

    attachBtn.addEventListener('click', () => pickFile());
    form.addEventListener('submit', async e => {
        e.preventDefault();
        await submitComposer(peer, input, sendBtn);
    });

    form.append(attachBtn, input, sendBtn);
    wrap.append(form);

    return wrap;
}

function renderStatus(): HTMLElement {
    return el('footer', { className: 'status' },
        el('span', { className: 'label' }, 'self'),
        el('span', {}, shortId(state.selfId)),
        el('span', {}, '·'),
        el('span', {}, `${state.peers.length} peer(s)`),
    );
}

// ----- actions -----
async function submitComposer(peer: PeerInfo, input: HTMLInputElement, sendBtn: HTMLButtonElement) {
    if (state.pendingFile) {
        sendBtn.disabled = true;
        let err: string;
        if (state.pendingFile.path) {
            // Drag-drop or other path-bearing source
            err = await SendFile(peer.PeerID, state.pendingFile.path);
        } else {
            // File-picker source — no path, must go
            // through SendFileContent which writes a
            // temp file in <data-dir>/outbox/. Wails v2.12
            // binding expects number[] (not Uint8Array)
            // for the byte slice parameter.
            const bytes = state.pendingFile.content
                ? Array.from(state.pendingFile.content)
                : [];
            err = await SendFileContent(peer.PeerID, state.pendingFile.name, bytes);
        }
        sendBtn.disabled = false;
        if (err) {
            toast(`send file failed: ${err}`);
            return;
        }
        clearPendingFile();
        input.value = '';
        // no input.focus() — composer is a different mode now
    } else {
        const text = input.value.trim();
        if (!text) return;
        sendBtn.disabled = true;
        const err = await SendText(peer.PeerID, text);
        sendBtn.disabled = false;
        if (err) {
            toast(`send failed: ${err}`);
            return;
        }
        input.value = '';
        input.focus();
    }
}

async function promptAlias(peer: PeerInfo) {
    const current = peer.Name || '';
    const next = window.prompt(`Set alias for ${shortId(peer.PeerID)}:`, current);
    if (next === null) return;
    const trimmed = next.trim();
    if (!trimmed) return;
    const err = await SetAlias(peer.PeerID, trimmed);
    if (err) toast(`alias failed: ${err}`);
    await refresh();
}

async function pickFile() {
    // Wails v2.12 has no OpenFileDialog runtime API
    // (added in v2.5+). Fall back to the browser's
    // <input type="file"> picker; read the file as
    // ArrayBuffer and stage it in state.pendingFile.
    // The composer send button then calls SendFileContent
    // (new bound method) which writes to
    // <data-dir>/outbox/ and runs the existing SendFile
    // pipeline.
    const inp = document.createElement('input');
    inp.type = 'file';
    inp.style.display = 'none';
    document.body.appendChild(inp);
    inp.addEventListener('change', () => {
        const f = inp.files?.[0];
        document.body.removeChild(inp);
        if (!f) return;
        const reader = new FileReader();
        reader.onload = () => {
            const buf = reader.result as ArrayBuffer;
            setPendingFile({
                name: f.name,
                size: f.size,
                content: new Uint8Array(buf),
            });
        };
        reader.onerror = () => toast('file read failed');
        reader.readAsArrayBuffer(f);
    });
    inp.click();
}

async function attachPath(path: string) {
    const name = path.split(/[\\/]/).pop() || path;
    // Wails OnFileDrop doesn't return file size; we
    // set 0 and let the composer show just the name.
    setPendingFile({ name, size: 0, path });
}

// ----- bootstrap -----
async function refresh() {
    try {
        state.peers = (await ListPeers()) ?? [];
        state.selfId = (await SelfPeerID()) ?? '';
    } catch (e) {
        console.error('refresh:', e);
    }
    mount();
}

async function bootstrap() {
    // HTML shell first
    const app = document.querySelector<HTMLDivElement>('#app')!;
    app.innerHTML = '';
    mount();

    // initial data
    await refresh();

    // Wails events from app/app.go
    EventsOn('peer:event', () => {
        refresh();
    });
    EventsOn('message:event', (_e: unknown, msg: ChatMessage) => {
        const list = state.history.get(msg.PeerID) || [];
        list.push(msg);
        state.history.set(msg.PeerID, list);
        if (msg.Direction === 'in' && msg.PeerID !== state.selectedId) {
            state.unreadCount.set(msg.PeerID, (state.unreadCount.get(msg.PeerID) || 0) + 1);
        }
        mount();
    });

    // Wails drag-and-drop: only the .composer-wrap element
    // (which carries --wails-drop-target: drop via CSS) is
    // accepted. The user drags a file in, we stage it in
    // state.pendingFile and let them hit send (WeChat-style
    // preview-then-send). useDropTarget=true makes Wails add
    // a `wails-drop-target-active` class while the mouse is
    // over the drop zone, so the CSS can highlight it.
    try {
        const { OnFileDrop } = await import('../wailsjs/runtime/runtime');
        OnFileDrop((_x: number, _y: number, paths: string[]) => {
            if (!paths || paths.length === 0) return;
            attachPath(paths[0]);
        }, true);
    } catch (e) {
        console.error('OnFileDrop:', e);
    }
}

window.addEventListener('DOMContentLoaded', () => {
    bootstrap();
});

// wails runtime re-export (alias import above uses original runtime.js)
// keep the typed accessors we use as untyped imports in case the
// generated file is missing under another name; the dynamic imports
// inside bootstrap() / pickFile() / attachPath() fetch them on demand.