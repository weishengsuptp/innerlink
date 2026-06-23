// innerlink frontend — vanilla TypeScript, no framework.
//
// Talks to the Go side via the Wails-generated bindings in
// ../wailsjs/go/app/App and listens for runtime events
// ("peer:event", "message") emitted from app/app.go.
//
// Layout: 3-pane shell (sidebar peer list, chat panel,
// status bar). No external UI deps. Keep it that way until
// v0.5 ships.

import './style.css';

import {
    ListPeers,
    SelfPeerID,
    SendText,
} from '../wailsjs/go/app/App';
import { node } from '../wailsjs/go/models';

// --- state -----------------------------------------------------------------

type PeerInfo = node.PeerInfo;
type ChatMessage = node.Message;

const state = {
    self: '' as string,
    peers: [] as PeerInfo[],
    active: '' as string, // peer id
    messages: [] as ChatMessage[],
};

// --- helpers ---------------------------------------------------------------

function el<K extends keyof HTMLElementTagNameMap>(
    tag: K,
    props: Partial<HTMLElementTagNameMap[K]> = {},
    ...children: (Node | string)[]
): HTMLElementTagNameMap[K] {
    const node = document.createElement(tag);
    Object.assign(node, props);
    for (const c of children) {
        node.append(c instanceof Node ? c : document.createTextNode(c));
    }
    return node;
}

function fmtTime(ts: unknown): string {
    const d = ts instanceof Date ? ts : new Date(ts as string);
    if (isNaN(d.getTime())) return '';
    return d.toLocaleTimeString('zh-CN', { hour: '2-digit', minute: '2-digit' });
}

function shortId(id: string): string {
    if (!id) return '';
    return id.slice(0, 8) + '…' + id.slice(-4);
}

// --- render ----------------------------------------------------------------

function render() {
    const app = document.querySelector<HTMLDivElement>('#app');
    if (!app) return;
    app.innerHTML = '';
    app.append(renderSidebar(), renderMain(), renderStatus());
}

function renderSidebar(): HTMLElement {
    const sb = el('aside', { className: 'sidebar' });
    sb.append(el('div', { className: 'sidebar-header' }, 'peers'));

    if (state.peers.length === 0) {
        sb.append(el('div', { className: 'empty' }, '尚未发现任何 peer'));
        return sb;
    }

    for (const p of state.peers) {
        const dotClass = p.IsSelf
            ? 'peer-dot self'
            : (p.Online ? 'peer-dot online' : 'peer-dot offline');
        const name = p.IsSelf
            ? (p.Name || 'self')
            : (p.Name || (p.Hostname ? p.Hostname : shortId(p.PeerID)));

        const item = el('div', {
            className: 'peer' + (p.PeerID === state.active ? ' active' : ''),
        });
        item.append(
            el('div', { className: dotClass }),
            el('div', { className: 'peer-info' },
                el('div', { className: 'peer-name' }, name),
                el('div', { className: 'peer-id' }, shortId(p.PeerID)),
            ),
        );
        item.addEventListener('click', () => {
            state.active = p.PeerID;
            render();
        });

        sb.append(item);
    }
    return sb;
}

function renderMain(): HTMLElement {
    const main = el('main', { className: 'main' });
    const peer = state.peers.find(p => p.PeerID === state.active);

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
    const head = el('div', { className: 'main-header' });
    head.append(
        el('h2', {}, peerName),
        el('div', { className: 'subtitle' }, shortId(peer.PeerID)),
    );

    // messages
    const msgs = el('div', { className: 'messages' });
    for (const m of state.messages.filter(m => m.PeerID === peer.PeerID)) {
        const msgEl = el('div', { className: 'message ' + m.Direction });
        msgEl.append(
            el('div', {}, m.Body),
            el('div', { className: 'ts' }, fmtTime(m.Timestamp)),
        );
        msgs.append(msgEl);
    }
    msgs.scrollTop = msgs.scrollHeight;

    // composer
    const input = el('input', {
        type: 'text',
        placeholder: '输入消息，回车发送',
        autocomplete: 'off',
    }) as HTMLInputElement;
    const send = el('button', {}, '发送') as HTMLButtonElement;

    const submit = async () => {
        const text = input.value.trim();
        if (!text || !state.active) return;
        send.disabled = true;
        const err = await SendText(state.active, text);
        send.disabled = false;
        if (err) {
            console.error('send failed:', err);
            input.value = '';
            input.focus();
            return;
        }
        input.value = '';
        input.focus();
    };

    input.addEventListener('keydown', e => {
        if (e.key === 'Enter') submit();
    });
    send.addEventListener('click', submit);

    const composer = el('div', { className: 'composer' });
    composer.append(input, send);

    main.append(head, msgs, composer);
    return main;
}

function renderStatus(): HTMLElement {
    const status = el('footer', { className: 'status' });
    status.append(
        el('span', { className: 'label' }, 'self'),
        el('span', {}, shortId(state.self)),
        el('span', {}, '·'),
        el('span', {}, `${state.peers.length} peer(s)`),
    );
    return status;
}

// --- bootstrap -------------------------------------------------------------

async function refresh() {
    try {
        state.peers = (await ListPeers()) ?? [];
        state.self = (await SelfPeerID()) ?? '';
    } catch (e) {
        console.error('refresh:', e);
    }
    render();
}

window.addEventListener('DOMContentLoaded', async () => {
    render();
    await refresh();

    // Wails events emitted from app/app.go
    import('../wailsjs/runtime/runtime').then(({ EventsOn }) => {
        EventsOn('peer:event', () => {
            refresh();
        });
        EventsOn('message', (_e: unknown, msg: ChatMessage) => {
            state.messages.push(msg);
            render();
        });
    });
});