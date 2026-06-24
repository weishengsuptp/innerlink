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
//     on-disk path; the user knows where they picked
//     the file from and can find it themselves).
//
// Progress is logged to the configured log sink.
func (n *Node) SendFile(peerRef, path, offerName, localPath string) error {
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
	progress := func(sent, total int64) {
		pct := int64(0)
		if total > 0 {
			pct = sent * 100 / total
		}
		log.Printf("[FILE] sending %s to %s  %d/%d bytes (%d%%)",
			path, peerHexStr, sent, total, pct)
	}
	log.Printf("[FILE] start send peer=%s path=%s offerName=%s localPath=%q",
		peerHexStr, path, offerName, localPath)
	n.wg.Add(1)
	go func() {
		defer n.wg.Done()
		if err := filetransfer.Send(context.Background(), st.ch, path, offerName, progress, st.rcv.WaitForReply); err != nil {
			log.Printf("[ERROR] sendfile: %v", err)
			return
		}
		log.Printf("[FILE] done peer=%s path=%s", peerHexStr, path)
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
		})
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