package node

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
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
type Message struct {
	PeerID    string    // sender (in) or recipient (out)
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
// progress events. Buffered to 32; under sustained
// progress (e.g. 1 MiB chunks at 30 Hz) drops oldest
// rather than blocking the sender goroutine.
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

// SendFile streams a local file to a peer. Runs in a
// background goroutine (file transfers can take
// minutes for GiB-sized files); this method returns
// immediately once the goroutine is launched.
//
// offerName is the name the receiver should save and
// display. If empty, the on-disk basename of path is
// used. The GUI's picker route passes the user's
// original filename here (the actual bytes live in a
// temp file in <data-dir>/outbox/ — which is deleted
// shortly after the transfer, so we never want to
// advertise the temp path as the "open me" location).
//
// localPath is what the GUI should hand to the
// double-click / right-click "open" handler on the
// sender's chat bubble:
//   - drag-and-drop route: the source file path on the
//     user's disk (so opening reveals the user's own
//     folder, not a temp copy).
//   - picker route: "" (the File API hides the real
//     on-disk path; the user knows where they picked the
//     file from and can find it themselves).
//
// fileID is the bubble ID on the GUI side. The
// frontend generates a UUID when the user picks a file
// and passes it through SendFileStart → SendFileFinish
// → this call. Drag-and-drop synthesises one if the
// caller passes "" so progress events still reach
// the right bubble.
//
// skipChatLog controls whether to publish a chat
// message + persist a chat.enc record for this send.
//   - drag-and-drop route (app.App.SendFile → this):
//     false. The chat message is the only UI artefact
//     for the file (no live placeholder bubble), so it
//     must exist.
//   - picker route (app.App.SendFileFinish → this):
//     true. The frontend already created a live progress
//     bubble via state.fileBubbles; publishing a chat
//     message here would create a SECOND bubble for
//     the same file, which looks like a duplicate.
//
// Progress is logged to the configured log sink and
// also published to SubscribeFiles() as FileEvent
// values, so the GUI can update its bubble in real
// time without polling logs.
func (n *Node) SendFile(peerRef, path, offerName, localPath, fileID string, skipChatLog bool) error {
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
			path, peerHexStr, sent, total, pct)
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
	log.Printf("[FILE] start send peer=%s path=%s fileID=%s offerName=%s localPath=%q",
		peerHexStr, path, fileID, offerName, localPath)
	n.wg.Add(1)
	go func() {
		defer n.wg.Done()
		if err := filetransfer.Send(context.Background(), st.ch, path, offerName, progress, st.rcv.WaitForReply); err != nil {
			log.Printf("[ERROR] sendfile: %v", err)
			// Tell the GUI: bubble turns red, show err.
			n.publishFileEvent(FileEvent{
				Type:   FileEventDone,
				FileID: fileID,
				OK:     false,
				Err:    err.Error(),
			})
			return
		}
		log.Printf("[FILE] done peer=%s path=%s", peerHexStr, path)
		// Final 100% tick so the GUI's progress bar
		// reaches the end before the done event.
		if info, ferr := os.Stat(path); ferr == nil {
			n.publishFileEvent(FileEvent{
				Type:   FileEventProgress,
				FileID: fileID,
				Sent:   info.Size(),
				Total:  info.Size(),
			})
		}
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
		base := path
		if i := strings.LastIndexAny(base, "/\\"); i >= 0 {
			base = base[i+1:]
		}
		// Size: stat the file we just sent. Logged as
		// human-readable in the file: message body so the
		// GUI can show "2.4 MiB · SM4-GCM 加密" without a
		// second fs call.
		sizeStr := ""
		if info, err := os.Stat(path); err == nil {
			sizeStr = humanSize(info.Size())
		}
		body := "file://" + base
		if sizeStr != "" {
			body += "|" + sizeStr
		}
		// Use the LocalPath the caller gave us, not the
		// on-disk path of the temp/source file. For the
		// picker route the caller passes "" because the
		// temp file is going to be deleted.
		outLocalPath := localPath
		if !skipChatLog {
			n.publishMessage(Message{
				PeerID:    peerHexStr,
				Body:      body,
				Timestamp: time.Now().UTC(),
				Direction: DirOut,
				LocalPath: outLocalPath,
			})
			// Also append to the encrypted chat log so the
			// file event persists across restarts and the GUI
			// can re-render the file card on relaunch.
			n.chatStore.Append(&storage.Record{
				Timestamp: time.Now().UTC(),
				From:      n.id.PeerIDHex(),
				To:        peerHexStr,
				Direction: "out",
				Body:      body,
				MsgID:     "",
				LocalPath: outLocalPath,
			})
		} else {
			// Picker route. The staging file at <path>
			// has served its purpose; clean it up so we
			// don't accumulate <data-dir>/sent/ entries
			// forever. The frontend holds the live bubble,
			// the receiver has its own copy on the other
			// side, and the sender has the original file
			// in the place they picked it from.
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				log.Printf("[WARN  ] picker staging cleanup failed: %v", err)
			}
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