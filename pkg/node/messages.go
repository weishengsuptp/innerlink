package node

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"strings"
	"time"

	"github.com/weishengsuptp/innerlink/internal/filetransfer"
	"github.com/weishengsuptp/innerlink/internal/protocol"
	"github.com/weishengsuptp/innerlink/internal/storage"
)

// Direction of a Message: "in" for received, "out" for sent.
const (
	DirIn  = "in"
	DirOut = "out"
)

// Message is one chat message, either received from a
// peer (Direction == "in") or sent to one (Direction == "out").
// Delivered on SubscribeMessages; also returned by History().
//
// Group context (v1.1, 2026-06-27):
// For messages in a group conversation, PeerID is the
// rendered GroupID ("g_<64hex>"). SenderID is the 32-char
// hex PeerID of the original sender when Direction == "in";
// for Direction == "out" SenderID is empty (the local user
// is the sender). For 1:1 messages SenderID is always empty
// (PeerID alone identifies the conversation partner).
//
// The frontend uses PeerID as the conversation key (sidebar
// routing + History() lookup), and SenderID to render the
// "Alice: hello" prefix on incoming group bubbles. Without
// SenderID, the GUI would either misroute a group message
// as a 1:1 message from the original sender, or lose the
// sender identity entirely.
type Message struct {
	PeerID    string    // peer hex (1:1) or "g_<64hex>" (group)
	SenderID  string    // sender's peer hex (group inbound only); empty otherwise
	Body      string    // text body (UTF-8)
	Timestamp time.Time // UTC; UI may render in local tz
	Direction string    // DirIn or DirOut
	// LocalPath is set for "file://" messages. It is the
	// absolute path of the file on the local filesystem
	// (the file the user picked / dropped for outbound;
	// the saved copy in <data-dir>/received/ for inbound).
	// The frontend uses it to wire up "double-click to
	// open" and "right-click to reveal in folder".
	LocalPath string
}

// SubscribeMessages returns a channel of every chat
// message, inbound and outbound, as the dispatcher
// processes them. Buffered to 64; under sustained flood
// drops oldest.
//
// The channel is closed when Close() is called.
func (n *Node) SubscribeMessages() <-chan Message {
	return n.messageCh
}

// publishFileEvent drops a file-transfer event into
// the SubscribeFiles() channel with drop-oldest
// backpressure (mirrors publishPeerEvent's pattern).
// Used by SendFile to fan progress + done notifications
// out to the GUI's "file:event" listener.
func (n *Node) publishFileEvent(ev FileEvent) {
	if n.fileEventCh == nil {
		return
	}
	select {
	case n.fileEventCh <- ev:
	default:
		select {
		case <-n.fileEventCh:
		default:
		}
		select {
		case n.fileEventCh <- ev:
		default:
		}
	}
}

// FileEventType enumerates the file-transfer progress
// events the GUI subscribes to. The frontend listens on
// "file:event" Wails runtime events; the per-file
// updates carry fileID so a single channel can carry
// many concurrent transfers without the GUI having to
// match by name or timestamp.
type FileEventType string

const (
	FileEventProgress FileEventType = "progress" // sent/total updated; draw bar + speed
	FileEventDone     FileEventType = "done"     // ok + err; draw ✓ or ✗
)

// FileEvent is the payload of the "file:event" runtime
// event. fileID identifies the bubble on the GUI side
// (the picker frontend generates a UUID when the user
// picks a file and passes it to SendFileStart; the drag-
// and-drop route also synthesises one if missing).
//
// Sent/Total are bytes moved / bytes total. For the
// "done" event they are equal; for a failed transfer
// Err is non-empty.
//
// 2026-06-25+: progress is now Wails-runtime-driven
// instead of [FILE] log lines only. The old log lines
// are still emitted (they're useful for log dumps
// during triage), but the GUI updates the bubble from
// this event so progress is visible without tabbing
// to the terminal.
type FileEvent struct {
	Type      FileEventType `json:"type"`
	FileID    string        `json:"fileID"`
	Sent      int64         `json:"sent"`
	Total     int64         `json:"total"`
	BytesPerSec int64       `json:"bytesPerSec"`
	OK        bool          `json:"ok"`
	Err       string        `json:"err,omitempty"`
}

// SubscribeFiles returns a channel of file-transfer
// progress events. Buffered to 256 (about 12 s of
// progress at the 10 Hz flush cadence for one transfer;
// comfortable for ~3 concurrent transfers without
// dropping the final 'done' event when the Wails event
// pump is briefly slow); under sustained progress drops
// oldest rather than blocking the sender goroutine.
//
// The channel is closed when Close() is called.
func (n *Node) SubscribeFiles() <-chan FileEvent {
	return n.fileEventCh
}

// SendText sends a chat message to a peer, identified
// by either an alias name or a 32-char hex PeerID.
// Returns an error if no active channel exists for the
// peer, or if the underlying Channel.Send fails.
//
// The send is synchronous on the underlying connection:
// returns once the bytes have been handed to the TCP
// stack. UI callers can fire-and-forget without
// worrying about lost messages 鈥?the encrypted local
// log (chat.enc) is written on success.
func (n *Node) SendText(peerRef, text string) error {
	if n.ctx == nil {
		return errors.New("node: not started")
	}
	if peerRef == "" {
		return errors.New("peer ref is empty")
	}
	peerHexStr, err := n.resolvePeerRef(peerRef)
	if err != nil {
		return err
	}
	pid, err := hexToBytes(peerHexStr)
	if err != nil {
		return errors.New("bad peer id hex: " + err.Error())
	}
	st := n.channels.get(pid)
	if st == nil {
		return errors.New("no active channel for peer " + peerHexStr)
	}
	if err := st.ch.SendText(n.ctx, text); err != nil {
		return err
	}
	log.Printf("[MSG  ] out >%s> %s", peerHexStr, text)
	rec := &storage.Record{
		Timestamp: time.Now().UTC(),
		From:      n.id.PeerIDHex(),
		To:        peerHexStr,
		Direction: "out",
		Body:      text,
		MsgID:     "",
	}
	if err := n.chatStore.Append(rec); err != nil {
		log.Printf("[ERROR] chat log append: %v", err)
	}
	n.historyMu.Lock()
	n.history = append(n.history, rec)
	n.historyMu.Unlock()
	n.publishMessage(Message{
		PeerID: peerHexStr, Body: text,
		Timestamp: rec.Timestamp, Direction: DirOut,
	})
	return nil
}

// SendFile ships the byte stream from src to peerRef under
// offerName. The src argument is an io.Reader the caller
// owns (typically *os.File for drag-and-drop, the read end
// of an io.Pipe for the picker route). SendFile does NOT
// close src — when the caller is done with it, the caller
// closes it.
//
// size is the total byte count of src. It is sent in the
// FileOffer so the receiver knows the file length up front.
//
// localPath is what the GUI should hand to the
// double-click / right-click "open" handler on the
// sender's chat bubble:
//   - drag-and-drop route: the source file path on the
//     user's disk (so opening reveals the user's own
//     folder, not a temp copy).
//   - picker route: the user's original path (the native
//     OS dialog hands the real path to Go, no sandbox
//     hiding). Same value, different origin.
//
// fileID is the bubble ID on the GUI side. The frontend
// generates a UUID when the user picks a file and stores
// it in state.fileBubbles before the file:event
// progress stream arrives. Drag-and-drop synthesises one
// if the caller passes "" so progress events still reach
// the right bubble.
//
// skipChatLog controls whether to publish a chat message
// to the message channel (SubscribeMessages). The chat.enc
// record is ALWAYS written regardless of skipChatLog —
// that's the persistence contract that lets the file card
// re-render after an app restart.
//   - drag-and-drop route (app.App.SendFile → this):
//     false. No live placeholder bubble on the GUI; the
//     chat message IS the only UI artefact for the file.
//   - picker route (app.App.SendFilePath → this):
//     true. The frontend already maintains a live
//     placeholder bubble in state.fileBubbles (driven by
//     file:event). Publishing a chat message here too
//     would render a SECOND bubble for the same file,
//     which is what happened before skipChatLog became
//     publish-only (2026-06-27).
func (n *Node) SendFile(peerRef, name string, size int64, src io.Reader, localPath, fileID string, skipChatLog bool) error {
	if n.ctx == nil {
		return errors.New("node: not started")
	}
	if peerRef == "" {
		return errors.New("peer ref is empty")
	}
	peerHexStr, err := n.resolvePeerRef(peerRef)
	if err != nil {
		return err
	}
	pid, err := hexToBytes(peerHexStr)
	if err != nil {
		return errors.New("bad peer id hex: " + err.Error())
	}
	st := n.channels.get(pid)
	if st == nil {
		return errors.New("no active channel for peer " + peerHexStr)
	}
	// Sliding-window speed estimate (last 1 s of
	// progress samples) so the GUI can show "X.X MB/s"
	// without doing its own EWMA. We accumulate bytes
	// since the last speed-flush tick and emit the
	// rate on each flush.
	var lastFlush time.Time
	var lastSent int64
	var windowBytes int64
	progress := func(sent, total int64) {
		pct := int64(0)
		if total > 0 {
			pct = sent * 100 / total
		}
		log.Printf("[FILE] sending %s to %s  %d/%d bytes (%d%%)",
			name, peerHexStr, sent, total, pct)
		// Emit at most ~10 Hz to avoid flooding the
		// event channel on a fast transfer.
		now := time.Now()
		if lastFlush.IsZero() {
			lastFlush = now
			lastSent = sent
		}
		windowBytes += sent - lastSent
		lastSent = sent
		if now.Sub(lastFlush) < 100*time.Millisecond {
			return
		}
		bps := int64(float64(windowBytes) / now.Sub(lastFlush).Seconds())
		n.publishFileEvent(FileEvent{
			Type:        FileEventProgress,
			FileID:      fileID,
			Sent:        sent,
			Total:       total,
			BytesPerSec: bps,
		})
		lastFlush = now
		windowBytes = 0
	}
	log.Printf("[FILE] start send peer=%s name=%s size=%d fileID=%s localPath=%q",
		peerHexStr, name, size, fileID, localPath)
	// v1.1 (2026-06-27): cancellable ctx so the GUI's ✕
	// button (App.CancelFile) can abort a stuck send.
	// The cancel func is registered in cancelFiles
	// under fileID and removed when the goroutine exits
	// (success or failure). filetransfer.Send checks
	// ctx.Err() at the top of every chunk + uses ctx
	// for ch.Send / waitForReply, so cancel propagates
	// all the way down.
	ctx, cancel := context.WithCancel(context.Background())
	n.cancelMu.Lock()
	n.cancelFiles[fileID] = cancel
	n.cancelMu.Unlock()

	n.wg.Add(1)
	go func() {
		defer n.wg.Done()
		defer func() {
			n.cancelMu.Lock()
			delete(n.cancelFiles, fileID)
			n.cancelMu.Unlock()
		}()
		if err := filetransfer.Send(ctx, st.ch, src, size, name, progress, st.rcv.WaitForReply); err != nil {
			log.Printf("[ERROR] sendfile: %v", err)
			// Normalize the cancel reason so the GUI shows
			// "已取消" instead of leaking "context canceled".
			errMsg := err.Error()
			if errors.Is(err, context.Canceled) {
				errMsg = "已取消"
			}
			// Tell the GUI: bubble turns red, show err.
			n.publishFileEvent(FileEvent{
				Type:   FileEventDone,
				FileID: fileID,
				OK:     false,
				Err:    errMsg,
			})
			return
		}
		log.Printf("[FILE] done peer=%s name=%s", peerHexStr, name)
		// Final 100% tick so the GUI's progress bar
		// reaches the end before the done event.
		n.publishFileEvent(FileEvent{
			Type:   FileEventProgress,
			FileID: fileID,
			Sent:   size,
			Total:  size,
		})
		n.publishFileEvent(FileEvent{
			Type:   FileEventDone,
			FileID: fileID,
			OK:     true,
		})
		// GUI chat panel: announce the sent file so the
		// chat UI can render a file card. Body uses
		// "file://<basename>" prefix (frontend parses).
		// The basename rather than full path because
		// the receiver already has its own copy and we
		// don't want to leak the sender's local layout.
		base := name
		if i := strings.LastIndexAny(base, "/\\"); i >= 0 {
			base = base[i+1:]
		}
		sizeStr := humanSize(size)
		body := "file://" + base
		if sizeStr != "" {
			body += "|" + sizeStr
		}
		// Always append to the encrypted chat log so the
		// file event persists across restarts and the GUI
		// can re-render the file card on relaunch. This is
		// the source of truth — even if the in-process
		// state.fileBubbles is gone after a restart, the
		// chat.enc record survives and History() will
		// surface it as a regular file:// bubble with the
		// right LocalPath for right-click "open folder".
		//
		// skipChatLog gates only the publishMessage call
		// below. Picker route passes skipChatLog=true so
		// the frontend's live placeholder bubble (driven
		// by file:event) is the only chat artefact during
		// the active session — publishing a chat message
		// here too would render a SECOND bubble for the
		// same file (one live-progress, one final),
		// which is what happened before this split
		// (2026-06-27).
		rec := &storage.Record{
			Timestamp: time.Now().UTC(),
			From:      n.id.PeerIDHex(),
			To:        peerHexStr,
			Direction: "out",
			Body:      body,
			MsgID:     "",
			LocalPath: localPath,
		}
		if err := n.chatStore.Append(rec); err != nil {
			log.Printf("[ERROR] chat log append (file): %v", err)
		}
		// Also keep the in-memory history slice in sync so
		// History() returns the file without a chat.enc
		// reload (cmd/innerlink reads it on every History()
		// call).
		n.appendHistory(rec)
		if !skipChatLog {
			n.publishMessage(Message{
				PeerID:    peerHexStr,
				Body:      body,
				Timestamp: rec.Timestamp,
				Direction: DirOut,
				LocalPath: localPath,
			})
		}
	}()
	return nil
}

// Ping sends a protocol-level Ping envelope to a peer.
// The peer's dispatcher auto-replies with Pong (see
// the wrapChannel recv pump), which is logged and
// counted as a touch in the alias store. Returns an
// error if no active channel exists.
//
// Distinct from SendText("ping"), which sends a regular
// chat message and never gets an automatic reply.
// Protocol Ping is the v0.4+ liveness probe and is what
// the CLI's `ping <peer>` command + the
// TestE2E_PingPongRoundTrip test rely on.
func (n *Node) Ping(peerRef string) error {
	if n.ctx == nil {
		return errors.New("node: not started")
	}
	if peerRef == "" {
		return errors.New("peer ref is empty")
	}
	peerHexStr, err := n.resolvePeerRef(peerRef)
	if err != nil {
		return err
	}
	pid, err := hexToBytes(peerHexStr)
	if err != nil {
		return errors.New("bad peer id hex: " + err.Error())
	}
	st := n.channels.get(pid)
	if st == nil {
		return errors.New("no active channel for peer " + peerHexStr)
	}
	if err := st.ch.SendPing(n.ctx); err != nil {
		return err
	}
	log.Printf("[MSG  ] out >%s> ping", peerHexStr)
	return nil
}

// keep helpers.go from being the only file that owns
// the protocol package 鈥?messages.go needs SendPing.
// (Node already imports protocol via channelState;
// this comment is for the linter.)
var _ protocol.Envelope // reference to silence "unused import"

// History returns the most recent chat records from
// the encrypted local log. If peerRef is non-empty,
// only records between us and that peer (alias or
// hex) are returned; "" returns records for all peers.
//
// Records are ordered oldest-first within the result,
// capped at 200 entries (older entries are still on
// disk in chat.enc 鈥?the UI can request more via a
// future HistoryRange API).
const historyLimit = 200

func (n *Node) History(peerRef string) []Message {
	n.historyMu.Lock()
	src := make([]*storage.Record, len(n.history))
	copy(src, n.history)
	n.historyMu.Unlock()

	var filterPeer string
	if peerRef != "" {
		var err error
		filterPeer, err = n.resolvePeerRef(peerRef)
		if err != nil {
			return nil
		}
	}
	out := make([]Message, 0, len(src))
	for _, r := range src {
		if filterPeer != "" && r.From != filterPeer && r.To != filterPeer {
			continue
		}
		out = append(out, Message{
			PeerID:    pickOther(r.From, r.To, n.id.PeerIDHex()),
			Body:      r.Body,
			Timestamp: r.Timestamp,
			Direction: r.Direction,
			// LocalPath is persisted in chat.enc since
			// 2026-06-26; older records omit it (json
			// default = ""). Pass it through so
			// peer-switch / app-restart doesn't wipe the
			// "right-click → reveal in folder" target.
			LocalPath: r.LocalPath,
		})
		if len(out) >= historyLimit {
			break
		}
	}
	return out
}

// pickOther returns the peer ID on the OTHER side of a
// chat record (relative to self). For inbound records
// that's the sender; for outbound, the recipient.
func pickOther(from, to, self string) string {
	if from == self {
		return to
	}
	return from
}

// humanSize formats a byte count for the file: message
// meta line (e.g. "2.4 MiB", "496 B"). Kept here (rather
// than in filetransfer) because it's a UI concern, not
// a protocol concern.
func humanSize(n int64) string {
	const (
		KiB = 1024
		MiB = 1024 * 1024
		GiB = 1024 * 1024 * 1024
	)
	switch {
	case n < KiB:
		return fmt.Sprintf("%d B", n)
	case n < MiB:
		return fmt.Sprintf("%.1f KiB", float64(n)/KiB)
	case n < GiB:
		return fmt.Sprintf("%.1f MiB", float64(n)/MiB)
	default:
		return fmt.Sprintf("%.2f GiB", float64(n)/GiB)
	}
}

// CancelFile aborts an in-flight outbound file transfer
// identified by fileID. The sender goroutine bails out
// of filetransfer.Send on the next ctx.Err() check (top
// of every chunk + every ch.Send / waitForReply call)
// and publishes a 'done' event with ok=false and
// err="已取消", which the GUI's markFileBubbleDone
// renders as a failed bubble with the friendly cancel
// reason.
//
// Idempotent: calling CancelFile on a fileID that has
// already finished (success, failure, or already
// cancelled) is a no-op + nil return. The GUI's ✕ button
// is hit-or-miss on a fast transfer and that's fine.
func (n *Node) CancelFile(fileID string) error {
	if n.ctx == nil {
		return errors.New("node: not started")
	}
	if fileID == "" {
		return errors.New("file id is empty")
	}
	n.cancelMu.Lock()
	cancel, ok := n.cancelFiles[fileID]
	n.cancelMu.Unlock()
	if !ok {
		// Either the fileID was never registered, or
		// the transfer already finished and the goroutine
		// removed its own entry. Either way, no cancel to
		// fire — treat as success so the GUI doesn't
		// toast a spurious error.
		return nil
	}
	cancel()
	log.Printf("[FILE] cancel requested fileID=%s", fileID)
	return nil
}

// DeleteHistory removes all on-disk chat records for the
// peer identified by peerRef (alias name or 32-char hex
// PeerID). Returns nil on success.
//
// What gets removed:
//   1. The per-peer encrypted file <chatDir>/<peerID>.enc
//      (via storage.DeleteAllForPeer).
//   2. The matching in-memory history slice entry
//      (so subsequent History() / SubscribeMessages
//      re-renders don't show the deleted records).
//   3. In-flight file:event 'done' callbacks for that
//      peer are not affected (they update state.fileBubbles
//      in the frontend, which has its own lifecycle).
//
// What is NOT removed:
//   - The peer's roster entry (still discoverable via UDP
//     / gossip).
//   - The alias mapping (still resolves the alias → peer ID).
//   - chat.enc on the OTHER device (it's their copy of the
//     chat; deleting on our side doesn't propagate).
//
// v1.1 (2026-06-27) replaces the v0.x "clear in-memory
// only" pseudo-action with a real on-disk delete. The
// previous UI button was a lie; this API makes it real.
func (n *Node) DeleteHistory(peerRef string) error {
	if n.ctx == nil {
		return errors.New("node: not started")
	}
	if peerRef == "" {
		return errors.New("peer ref is empty")
	}
	peerHexStr, err := n.resolvePeerRef(peerRef)
	if err != nil {
		return err
	}
	if n.chatStore == nil {
		return errors.New("node: chat store not initialised")
	}
	if err := n.chatStore.DeleteAllForPeer(peerHexStr); err != nil {
		return fmt.Errorf("delete history: %w", err)
	}
	// Drop the matching slice from the in-memory cache.
	// Without this, History() / SubscribeMessages-driven
	// re-renders would still show the deleted records
	// (until the next ReadAll refresh, which only
	// happens at startup). Filter in place to avoid
	// reallocating the whole slice.
	n.historyMu.Lock()
	filtered := n.history[:0:0]
	for _, rec := range n.history {
		if rec.From == peerHexStr || rec.To == peerHexStr {
			continue
		}
		filtered = append(filtered, rec)
	}
	n.history = filtered
	n.historyMu.Unlock()

	log.Printf("[INFO ] deleted chat history for peer=%s", peerHexStr)
	return nil
}