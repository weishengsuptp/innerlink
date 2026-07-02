package node

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/weishengsuptp/innerlink/internal/alias"
	"github.com/weishengsuptp/innerlink/internal/discovery"
	"github.com/weishengsuptp/innerlink/internal/filetransfer"
	"github.com/weishengsuptp/innerlink/internal/handshake"
	"github.com/weishengsuptp/innerlink/internal/identity"
	"github.com/weishengsuptp/innerlink/internal/leavelog"
	"github.com/weishengsuptp/innerlink/internal/logx"
	"github.com/weishengsuptp/innerlink/internal/paths"
	"github.com/weishengsuptp/innerlink/internal/protocol"
	"github.com/weishengsuptp/innerlink/internal/roster"
	"github.com/weishengsuptp/innerlink/internal/selfid"
	"github.com/weishengsuptp/innerlink/internal/storage"
	"github.com/weishengsuptp/innerlink/internal/transport"
	"github.com/weishengsuptp/innerlink/pkg/group"
)

// Node is the long-lived innerlink runtime.
//
// Construct one with New, start it with Start, drive it
// with SendText/SendFile/SubscribeMessages/etc., and stop
// it with Close. A Node is a single network identity: it
// owns one device.key, one chat.enc, one roster.json,
// one alias table, and one or more active encrypted
// channels (one per peer).
//
// A single process can run multiple Nodes if you give
// them distinct Options (different DataDir / DeviceKey
// / TCPPort). The default is one Node per process.
type Node struct {
	opts Options // user-provided, defaults applied

	// Persistent state (loaded from disk on New, written
	// back as needed).
	id          *identity.Identity
	layout      paths.Layout
	chatStore   *storage.Store
	aliasStore  *alias.Store
	rosterStore *roster.Store
	selfidStore *selfid.Store
	leavelog    *leavelog.Store // v1.1.4 (2026-07-02): offline-replay log of groups self has left
	selfAlias   *selfAliasStore

	// v1.1.4 (2026-07-02): global data lock. Held by every
	// operation that mutates on-disk persistent state
	// (members.json, chat.enc, roster, aliases) so that
	// concurrent paths — AcceptGroupInvite, LeaveGroup,
	// self-claim, group-audit, SendGroupMessage — never
	// interleave writes to the same file. RWMutex because
	// the read paths (History, ListGroups) are common and
	// the claim/audit are one-shot per Start.
	dataMu sync.RWMutex

	// Networking.
	ann       *discovery.Announcer
	tr        *transport.Transport
	channels  *channelRegistry
	autoScan  *autoScanState
	myIPs     []string // local IPs we treat as "self" for scan dedup

	// Lifecycle.
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	mu     sync.Mutex
	closed bool

	// Event channels for the public subscribe API.
	// messageCh receives every inbound + outbound chat
	// message as the dispatcher pump processes them.
	// peerEventCh receives peer add/remove/online/offline
	// transitions. fileEventCh receives file-transfer
	// progress / done notifications keyed by fileID so
	// the GUI can update its bubble in real time.
	// groupEventCh receives group added / removed
	// transitions so the GUI can refresh its "群组"
	// sidebar section without polling. v1.1 (2026-06-28).
	// All four are buffered to absorb short bursts
	// from gossip storms / network floods.
	messageCh    chan Message
	peerEventCh  chan PeerEvent
	fileEventCh  chan FileEvent
	groupEventCh chan GroupEvent

	// In-memory chat history cache. Source of truth is
	// the encrypted chat.enc on disk; this slice is just
	// a fast lookup for History().
	historyMu sync.Mutex
	history   []*storage.Record

	// cancelFiles: per-fileID cancel funcs for in-flight
	// outbound file transfers. The GUI's ✕ button calls
	// CancelFile(fileID) which looks up the cancel and
	// fires it; the sender goroutine then bails out of
	// filetransfer.Send (which already does ctx.Err()
	// checks in its chunk loop + uses ctx for every
	// ch.Send / waitForReply call). v1.1 (2026-06-27).
	cancelMu   sync.Mutex
	cancelFiles map[string]context.CancelFunc

	// v1.1.4 (2026-07-02) — exclusive DataDir lock. v1.1.4
	// hotfix against the user-reported 21:20 symptom: two
	// innerlink.exe processes pointing at the same DataDir
	// (the "vmware dual-IP test setup") silently race on
	// device.key / roster.json / members.json. The second
	// process binds UDP 4747 and TCP 4748 fails, but
	// node.Start would have returned the error BEFORE we
	// made this lock — except that nothing checked the
	// return until the GUI app crashed an entire message
	// stream later. With this lock, New() fails fast at
	// the top of the constructor with a clear "DataDir
	// already in use by another innerlink" message, and
	// the second process exits before touching any state
	// files. See the function comment on the lockfile
	// release in Close for the lifecycle contract.
	lockFile *os.File
}

// New constructs a Node. It loads (or creates) the SM2
// device identity and opens the persistent state files
// (chat log, aliases, roster). It does NOT start any
// networking 鈥?call Start for that.
//
// Safe to call multiple times in one process with
// different opts (different DataDir / TCPPort); each
// Node owns its own goroutines and listeners.
func New(opts Options) (*Node, error) {
	opts = opts.applyDefaults()

	layout, err := paths.NewLayout("", paths.Overrides{
		DataDir:   opts.DataDir,
		DeviceKey: opts.DeviceKey,
		SaveDir:   opts.SaveDir,
		LogFile:   opts.LogFile,
	})
	if err != nil {
		return nil, fmt.Errorf("resolve paths: %w", err)
	}
	if err := layout.Ensure(); err != nil {
		return nil, fmt.Errorf("create state dirs: %w", err)
	}
	// v1.1.4 (2026-07-02): exclusive DataDir lockfile.
	// O_CREATE|O_EXCL fails if the file already exists,
	// which happens whenever another innerlink.exe is
	// already running against this DataDir. We open the
	// file (and KEEP the handle — closing it would
	// release the lock on Windows where the OS-level
	// exclusive bind is the source of truth) and stash
	// it on the Node so Close can remove the file at
	// shutdown. Without this guard, two processes
	// silently race on device.key / roster.json /
	// members.json and the second process ends up
	// picking up whichever device.key the first one
	// happened to flush most recently. The user-reported
	// 21:20 "<vm-clone-host> shows up twice, peer B
	// drops itself from the group" was exactly this:
	// A and B were on the same physical box, same
	// DataDir, and the second innerlink silently
	// co-existed with the first.
	lockPath := filepath.Join(layout.DataDir, "innerlink.lock")
	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		if os.IsExist(err) {
			return nil, fmt.Errorf("node: DataDir %q is already in use by another innerlink process (lockfile %s exists). "+
				"This usually means two innerlink.exe instances are sharing the same DataDir — "+
				"use --data-dir or set INNERLINK_DATA_DIR to give each one its own state directory, "+
				"or close the other instance first", layout.DataDir, lockPath)
		}
		return nil, fmt.Errorf("node: lock DataDir %q: %w", layout.DataDir, err)
	}
	// Write our PID into the lockfile so a human looking
	// at the file (e.g. via "type innerlink.lock" in
	// cmd) can tell which process holds it. Best-effort
	// — a write failure here isn't fatal because the
	// OS-level exclusive bind is the actual lock; the
	// PID is just a debugging convenience.
	if pidStr := strconv.Itoa(os.Getpid()) + "\n"; true {
		if _, werr := lockFile.WriteString(pidStr); werr != nil {
			log.Printf("[WARN ] node: write lockfile pid: %v", werr)
		}
		_ = lockFile.Sync()
	}
	// Defer-releaser: any error return below this point
	// must release the lockfile so the caller can retry
	// (or the next process can start). The success path
	// in Close also releases, so we check `lockFile !=
	// nil` (Close nils it after closing).
	defer func() {
		if lockFile == nil {
			return
		}
		lockPath := lockFile.Name()
		_ = lockFile.Close()
		if err := os.Remove(lockPath); err != nil && !os.IsNotExist(err) {
			log.Printf("[WARN ] node: remove lockfile %s on error path: %v", lockPath, err)
		}
	}()
	if err := logx.Setup(logx.Options{
		Level:  logx.Level(opts.LogLevel),
		File:   layout.LogFile,
		Stderr: true,
	}); err != nil {
		return nil, fmt.Errorf("logx setup: %w", err)
	}
	log.Printf("[INFO ] data dir:        %s", layout.DataDir)
	log.Printf("[INFO ] incoming files:  %s", layout.Received)
	log.Printf("[INFO ] log file:        %s", layout.LogFile)

	id, created, err := identity.LoadOrCreate(layout.DeviceKey)
	if err != nil {
		return nil, fmt.Errorf("load identity: %w", err)
	}
	if created {
		log.Printf("[INFO ] device identity created peerID=%s", id.PeerIDHex())
	} else {
		log.Printf("[INFO ] device identity loaded  peerID=%s", id.PeerIDHex())
	}
	log.Printf("[INFO ] device key file: %s", layout.DeviceKey)

	chatStore, err := storage.Open(layout.DataDir, id.PrivateKeyD())
	if err != nil {
		return nil, fmt.Errorf("open chat log: %w", err)
	}
	// v1.1 (2026-06-27): per-peer chat files require the
	// device's own PeerID so Append can route each record
	// to the right peer file. SetSelfPeerID is idempotent
	// and must run before any Append — we do it here, before
	// returning the Node to the caller, so pkg/node and
	// cmd/innerlink both benefit without each having to
	// remember.
	chatStore.SetSelfPeerID(id.PeerIDHex())
	aliasStore, err := alias.Open(layout.Aliases)
	if err != nil {
		_ = chatStore.Close()
		return nil, fmt.Errorf("open alias file: %w", err)
	}
	rosterStore, err := roster.Open(layout.Roster)
	if err != nil {
		_ = chatStore.Close()
		_ = aliasStore.Close()
		return nil, fmt.Errorf("open roster: %w", err)
	}
	selfidStore, err := selfid.Open(filepath.Join(layout.DataDir, "self_history.json"))
	if err != nil {
		// v1.1.4 (2026-07-02): soft error per selfid.Open's
		// contract. A corrupt history file is a recoverable
		// condition — we lose one wipe-cycle's worth of claim
		// (acceptable) and start up empty. Log loudly so the
		// user sees it; don't refuse to start.
		log.Printf("[WARN ] self_history: parse failed (%v) — proceeding with empty history (claim disabled this session)", err)
		selfidStore, _ = selfid.Open(filepath.Join(layout.DataDir, "self_history.json.tmp")) //nolint:errcheck
		if selfidStore == nil {
			selfidStore = &selfid.Store{}
		}
	}
	// v1.1.4 (2026-07-02): persistent record of "groups I
	// have left". Replayed as TypeGroupLeaveNotice on every
	// subsequent handshake so the creator finally learns
	// about a leave that happened while they were offline
	// (the user-reported 2026-07-02 "退群都没成功真出bug了"
	// bug). Soft-error semantics match selfid above.
	leavelogStore, err := leavelog.Open(filepath.Join(layout.DataDir, "leaved_groups.json"))
	if err != nil {
		log.Printf("[WARN ] leavelog: parse failed (%v) — proceeding with empty log (offline-leave replay disabled this session)", err)
		leavelogStore, _ = leavelog.Open(filepath.Join(layout.DataDir, "leaved_groups.json.tmp")) //nolint:errcheck
		if leavelogStore == nil {
			leavelogStore = &leavelog.Store{}
		}
	}
	selfAliasStore, err := loadSelfAlias(filepath.Join(layout.DataDir, "alias.txt"))
	if err != nil {
		_ = chatStore.Close()
		_ = aliasStore.Close()
		_ = rosterStore.Close()
		return nil, fmt.Errorf("open self alias: %w", err)
	}

	n := &Node{
		opts:        opts,
		id:          id,
		layout:      layout,
		chatStore:   chatStore,
		aliasStore:  aliasStore,
		rosterStore: rosterStore,
		selfidStore: selfidStore,
		leavelog:    leavelogStore,
		selfAlias:   selfAliasStore,
		lockFile:    lockFile,
		channels:    newChannelRegistry(),
		messageCh:    make(chan Message, 64),
		peerEventCh:  make(chan PeerEvent, 64),
		groupEventCh: make(chan GroupEvent, 32),
		// 256 = ~12.8s of progress events per transfer at
		// the ~10 Hz flush cadence; comfortable margin for
		// 3 concurrent transfers without dropping events
		// when the Wails event-emit pump is briefly slow.
		// Was 32 (1.6s) which caused "排队中…" to stick on
		// parallel sends because the GUI's 'done' event
		// was dropped on overflow (2026-06-27).
		fileEventCh:  make(chan FileEvent, 256),
		cancelFiles:  make(map[string]context.CancelFunc),
	}
	// Transfer ownership: n.lockFile is now the source of
	// truth. The local var points at the same file; nil
	// it so the deferred releaser skips the success path
	// (Close handles release instead — single owner).
	lockFile = nil

	// v1.1.4 (2026-07-02): self-claim wiring at New time.
	// If the just-loaded device identity's peerID differs
	// from the LATEST entry in self_history, this is a
	// wipe+reinstall (or first launch after the file was
	// created). Record the migration so the next Start()
	// can claim ownership of any group / alias / roster
	// references still pointing at the old peerID.
	//
	// This is BEFORE the UDP/TCP goroutines start, so
	// there's no race: claim won't fire while a peer
	// message could land in the dispatcher. (The claim
	// itself happens in Start(), but the bookkeeping
	// entry needs to exist on disk by then.)
	currentPeerID := id.PeerIDHex()
	latest, hasLatest := selfidStore.Latest()
	switch {
	case !hasLatest:
		// First launch ever. Fresh install. Record it
		// with empty OldPeerID so the history is
		// self-documenting.
		log.Printf("[SYNC ] self_history: first launch, recording fresh_install peerID=%s", currentPeerID[:8])
		if err := selfidStore.RecordMigration(selfid.Entry{
			NewPeerID:  currentPeerID,
			SwitchedAt: time.Now().UTC(),
			Trigger:    selfid.TriggerFreshInstall,
		}); err != nil {
			log.Printf("[WARN ] self_history: record fresh install: %v", err)
		}
	case latest.NewPeerID != currentPeerID:
		// Wipe+reinstall (or manual reset). Old peerID
		// is latest.NewPeerID. The wipe doesn't change
		// the stored latest entry — we always APPEND a
		// new one, so the rolling window preserves the
		// full chain for gossip dedup.
		log.Printf("[SYNC ] self_history: peerID changed %s → %s (trigger=%s)", latest.NewPeerID[:8], currentPeerID[:8], latest.Trigger)
		if err := selfidStore.RecordMigration(selfid.Entry{
			OldPeerID:  latest.NewPeerID,
			NewPeerID:  currentPeerID,
			SwitchedAt: time.Now().UTC(),
			Trigger:    selfid.TriggerWipeReinstall,
		}); err != nil {
			log.Printf("[WARN ] self_history: record wipe+reinstall: %v", err)
		}
		if err := selfidStore.Save(); err != nil {
			log.Printf("[WARN ] self_history: save: %v", err)
		}
	default:
		log.Printf("[SYNC ] self_history: same peerID as last launch, no migration needed")
	}

	// Self entry in the roster: always include ourselves
	// so the first channel-ready send already tells the
	// other side "this is how you reach me". The alias
	// field is the broadcast self-display-name (loaded
	// from <DataDir>/alias.txt above; "" if unset).
	announcer := discovery.NewAnnouncerOnPortBind(id, hostname(), uint16(opts.TCPPort), uint16(opts.UDPPort), opts.BindIP)
	selfEntry := roster.Entry{
		PeerID:   id.PeerIDHex(),
		Hostname: hostname(),
		Alias:    selfAliasStore.Get(),
		Addrs:    []string{announcer.LocalAddr()},
	}
	if _, err := rosterStore.Add(selfEntry); err != nil {
		return nil, fmt.Errorf("roster: add self: %w", err)
	}
	// Tell the roster which entry is "us" so the dedup
	// scan can recognise a (hostname, IP) collision
	// with self as the device-key-reset case: the
	// incoming gossip entry is then the OLD self
	// identity, not a real new peer, and gets marked
	// Reset instead of hiding our own entry.
	// Without this, a user who deletes their data
	// folder and re-launches briefly sees their own
	// previous alias in their own peer list until
	// gossip converges.
	rosterStore.SetSelf(id.PeerIDHex())
	if selfEntry.Alias != "" {
		log.Printf("[INFO ] self alias:      %q", selfEntry.Alias)
	} else {
		log.Printf("[INFO ] self alias:      (unset — click your sidebar header to set)")
	}
	log.Printf("[INFO ] self in roster: %s @ %s (%s)",
		selfEntry.PeerID, selfEntry.Hostname, selfEntry.Addrs[0])

	n.ann = announcer
	n.myIPs = []string{announcer.LocalAddr()}
	n.autoScan = newAutoScanState(n.myIPs)

	log.Printf("[INFO ] alias file: %s", layout.Aliases)
	log.Printf("[INFO ] self-alias file: %s", filepath.Join(layout.DataDir, "alias.txt"))
	log.Printf("[INFO ] roster file: %s", layout.Roster)
	log.Printf("[INFO ] chat log: %d records loaded", 0) // placeholder; filled in Start

	return n, nil
}

// Start launches the UDP announcer, TCP transport,
// inbound dispatcher, discovery-driven dials, and
// (if Options.AutoScan is set) the auto-scan loop.
// Returns once Listen succeeds; goroutines run until
// Close is called.
//
// The provided ctx is used as the parent context 鈥?// canceling it is equivalent to calling Close.
func (n *Node) Start(ctx context.Context) error {
	if n.ann == nil {
		return fmt.Errorf("node: Start called before New")
	}
	if n.ctx != nil {
		return fmt.Errorf("node: already started")
	}
	n.ctx, n.cancel = context.WithCancel(ctx)

	// Load chat history now (after storage is open).
	history, err := n.chatStore.ReadAll()
	if err != nil {
		log.Printf("[ERROR] read chat log: %v (starting with empty history)", err)
		history = nil
	}
	n.historyMu.Lock()
	n.history = history
	n.historyMu.Unlock()
	log.Printf("[INFO ] chat log: %d records loaded", len(history))

	// v1.1.4 (2026-07-02) sync infrastructure: claim +
	// audit BEFORE any goroutine that can race with
	// persistent state. The dispatcher is started later
	// in this function; this block runs synchronously,
	// in the calling goroutine, so there's no concurrent
	// mutation of groups/members.json or roster.json
	// while claim + audit are working.
	n.runStartupSync()

	// 1) UDP announcer.
	n.wg.Add(1)
	go func() {
		defer n.wg.Done()
		if err := n.ann.Run(n.ctx); err != nil && n.ctx.Err() == nil {
			log.Printf("[ERROR] announcer: %v", err)
		}
	}()

	// 2) TCP transport. CRITICAL: bind + Listen BEFORE
	// printing the "listening for peers on TCP :N" log
	// line. The e2e tests (and the v0.6 CLI's `dial`
	// path) gate on that exact log line; if we log
	// before the bind syscall completes, a fast caller
	// can dial and hit "connectex: No connection could
	// be made" before the kernel finishes the bind.
	// The Windows CI runner is slow enough that this
	// race fires reliably; local Windows / Linux / macOS
	// have a 10-100x gap between log flush and bind,
	// which is why v0.6.x never caught it.
	n.tr = transport.NewTransportOnPortBind(n.opts.TCPPort, n.opts.BindIP)
	if err := n.tr.Listen(); err != nil {
		return fmt.Errorf("transport listen: %w", err)
	}
	log.Printf("[INFO ] listening for peers on UDP :%d", n.opts.UDPPort)
	log.Printf("[INFO ] listening for peers on TCP :%d", n.opts.TCPPort)
	n.wg.Add(1)
	go func() {
		defer n.wg.Done()
		if err := n.tr.Run(n.ctx); err != nil && n.ctx.Err() == nil {
			log.Printf("[ERROR] transport: %v", err)
		}
	}()

	// 3) Inbound dispatcher.
	n.wg.Add(1)
	go func() {
		defer n.wg.Done()
		for {
			select {
			case <-n.ctx.Done():
				return
			case inbound, ok := <-n.tr.Inbounds():
				if !ok {
					return
				}
				n.wg.Add(1)
				go func() {
					defer n.wg.Done()
					n.handleInbound(inbound)
				}()
			}
		}
	}()

	// 4) Discovery 鈫?dial.
	n.wg.Add(1)
	go func() {
		defer n.wg.Done()
		for ev := range n.ann.Events() {
			switch ev.Type {
			case discovery.PeerAdded:
				log.Printf("[PEER ] joined   peer=%s at %s", peerHex(ev.PeerID), ev.Peer.Addr)
				n.aliasStore.Touch(peerHex(ev.PeerID))
				n.publishPeerEvent(PeerEvent{Type: PeerAdded, PeerID: peerHex(ev.PeerID), Addr: ev.Peer.Addr.String()})
				n.wg.Add(1)
				go func() {
					defer n.wg.Done()
					n.dialAndHandshake(ev.Peer)
				}()
			case discovery.PeerRemoved:
				log.Printf("[PEER ] left     peer=%s", peerHex(ev.PeerID))
				n.publishPeerEvent(PeerEvent{Type: PeerRemoved, PeerID: peerHex(ev.PeerID)})
			}
		}
	}()

	// 5) Optional auto-scan loop.
	if n.opts.AutoScan {
		log.Printf("[INFO ] auto-scan: ENABLED (will probe new /24s as roster learns of them)")
		n.wg.Add(1)
		go func() {
			defer n.wg.Done()
			n.autoScanLoop()
		}()
	} else {
		log.Printf("[INFO ] auto-scan: disabled (use AutoScan=true to enable)")
	}

	return nil
}

// Close shuts down the Node: cancels context, closes
// listeners + channels, flushes persistent state.
// Safe to call multiple times.
func (n *Node) Close() error {
	n.mu.Lock()
	if n.closed {
		n.mu.Unlock()
		return nil
	}
	n.closed = true
	n.mu.Unlock()

	if n.cancel != nil {
		n.cancel()
	}
	n.channels.closeAll()
	if n.tr != nil {
		n.tr.Close()
	}
	// v1.1.4 (2026-07-02) hotfix — Close deadlock on shutdown.
	//
	// Symptom: nd.Close() took > 5s, app.Shutdown's 5s timer
	// fired, runtime.Stack dump showed 5 goroutines all
	// blocked on `chan receive`:
	//   - pkg/node/node.go:416  for ev := range n.ann.Events()
	//     (announcer consumer started in Node.Start)
	//   - app/app.go:999/1011/1024/1038  for ev := range
	//     nd.SubscribePeers/Messages/Groups/Files()
	//     (4 pump goroutines forwarding events to frontend)
	//
	// Root cause: the 4 public SubscribeXxx channels (messageCh,
	// peerEventCh, fileEventCh, groupEventCh) were being
	// close()d AFTER n.wg.Wait() at the bottom of this func.
	// But those 4 channels are exactly what the pump
	// goroutines are blocked on — so wg.Wait would never
	// complete (pumps waiting for close, close waiting for
	// wg.Wait) and the whole shutdown deadlocked for the
	// ~5s read-deadline interval of the announcer's UDP
	// socket (which would eventually let Run return, then
	// closeEvents() would unblock goroutine 53 at
	// node.go:416, but only after that 5s wall-clock
	// tick).
	//
	// Fix: close the 4 SubscribeXxx channels FIRST, so the
	// pump goroutines can drain their range loops and
	// wg.Done() before wg.Wait blocks on them. Same with
	// the announcer consumer — its events channel is closed
	// by Announcer.Run on ctx.Done, which the n.cancel()
	// above already triggered, so it'll fall through on
	// its own once Run's read deadline elapses.
	//
	// Order matters: close channels BEFORE wg.Wait (so
	// consumers can exit), but do the persistent-state
	// flushes AFTER wg.Wait (so no goroutine is still
	// mutating chat/roster/alias while we close them).
	_ = logx.Close()
	// v1.1.4 (2026-07-02): release the exclusive DataDir
	// lockfile so the next innerlink process can start
	// against this directory. The order matters: we close
	// the logx handle first (so the log line below lands
	// somewhere), then close the lock handle and remove
	// the file. We do the file removal AFTER closing the
	// handle because on Windows the OS-level exclusive
	// bind is what enforces the lock — closing the handle
	// first means a racing process could see "file gone"
	// and try to OpenFile|O_EXCL before we Remove, getting
	// EEXIST spuriously. Remove() first, then Close(), is
	// also a valid order on Windows but is slightly more
	// racy on POSIX where the inode is reused briefly. The
	// current order (Close then Remove) is the same one
	// cmd/innerlink's defer uses elsewhere.
	if n.lockFile != nil {
		lockPath := n.lockFile.Name()
		_ = n.lockFile.Close()
		if err := os.Remove(lockPath); err != nil && !os.IsNotExist(err) {
			log.Printf("[WARN ] node: remove lockfile %s: %v", lockPath, err)
		}
		n.lockFile = nil
	}
	close(n.messageCh)
	close(n.peerEventCh)
	close(n.fileEventCh)
	close(n.groupEventCh)
	// Announcer has no Close() — Run() exits when ctx is
	// canceled (defer conn.Close inside Run), so cancelling
	// n.ctx above is enough to bring it down.
	n.wg.Wait()

	// v1.1.4 (2026-07-02): flush the leave log to disk on
	// shutdown. LeaveGroup calls Record + Save eagerly so
	// most rows are already persisted by the time we get
	// here, but a Record call right before Close (no Save
	// in between) would otherwise be lost.
	if n.leavelog != nil {
		if err := n.leavelog.Save(); err != nil {
			log.Printf("[WARN ] leavelog: close-time save: %v", err)
		}
	}

	if n.chatStore != nil {
		if err := n.chatStore.Close(); err != nil {
			log.Printf("[ERROR] close chat log: %v", err)
		}
	}
	if n.aliasStore != nil {
		if err := n.aliasStore.Close(); err != nil {
			log.Printf("[ERROR] close alias file: %v", err)
		}
	}
	if n.rosterStore != nil {
		if err := n.rosterStore.Close(); err != nil {
			log.Printf("[ERROR] close roster: %v", err)
		}
	}
	if n.selfAlias != nil {
		if err := n.selfAlias.Save(); err != nil {
			log.Printf("[ERROR] close self alias: %v", err)
		}
	}
	return nil
}

// SelfPeerID returns our own 32-char hex PeerID.
func (n *Node) SelfPeerID() string {
	return n.id.PeerIDHex()
}

// --- internal orchestration (called by the dispatcher
//     pumps and the public methods) ---

// appendHistory adds a record to the in-memory history
// cache. Source of truth is the encrypted chat.enc file;
// this slice is a fast lookup for the History() public
// API and for the CLI's `history` command.
func (n *Node) appendHistory(rec *storage.Record) {
	n.historyMu.Lock()
	n.history = append(n.history, rec)
	n.historyMu.Unlock()
}

func (n *Node) publishMessage(msg Message) {
	select {
	case n.messageCh <- msg:
	default:
		// Channel full 鈥?drop the oldest by draining one
		// and retrying. The UI is best-effort and the
		// encrypted local log is the source of truth.
		select {
		case <-n.messageCh:
		default:
		}
		select {
		case n.messageCh <- msg:
		default:
		}
	}
}

func (n *Node) publishPeerEvent(ev PeerEvent) {
	select {
	case n.peerEventCh <- ev:
	default:
		select {
		case <-n.peerEventCh:
		default:
		}
		select {
		case n.peerEventCh <- ev:
		default:
		}
	}
}

// dialAndHandshake is the discovery-driven initiator:
// dials the peer's TCP endpoint, runs the handshake,
// wraps the resulting Channel. If a Channel already
// exists for this peer, this is a no-op (the announcer
// fires every announce interval, so the same peer can
// re-trigger PeerAdded; without this guard we'd
// double-handshake and tear down the first channel).
func (n *Node) dialAndHandshake(p *discovery.Peer) {
	if p.Addr == nil {
		return
	}
	if n.channels.get(p.PeerID) != nil {
		return
	}
	tcpAddr := fmt.Sprintf("%s:%d", p.Addr.IP.String(), transport.DefaultPort)
	dctx, cancel := context.WithTimeout(n.ctx, 5*time.Second)
	defer cancel()
	// Use Transport.Dial, NOT transport.DialStandalone. The
	// former registers the Conn in the transport's registry,
	// so the heartbeat loop sends keepalives to it.
	// DialStandalone would skip registration and the conn
	// would die from read-deadline timeout after 60s of idle.
	conn, err := n.tr.Dial(dctx, tcpAddr)
	if err != nil {
		log.Printf("[ERROR] dial %s: %v", tcpAddr, err)
		return
	}
	sess, err := handshake.RunAsInitiator(dctx, n.id, conn)
	if err != nil {
		log.Printf("[ERROR] handshake (initiator) with %s: %v", peerHex(p.PeerID), err)
		return
	}
	log.Printf("[HANDS] ok       peer=%s (initiator)", peerHex(p.PeerID))
	n.wrapChannel(conn, sess)
}

// handleInbound is the responder counterpart: another
// peer dialed us, ran the handshake; we accept and wrap.
func (n *Node) handleInbound(conn *transport.Conn) {
	hctx, cancel := context.WithTimeout(n.ctx, 5*time.Second)
	defer cancel()
	sess, err := handshake.RunAsResponder(hctx, n.id, conn)
	if err != nil {
		log.Printf("[ERROR] handshake (responder) from %s: %v", conn.RemoteAddr(), err)
		return
	}
	log.Printf("[HANDS] ok       peer=%s (responder)", peerHex(sess.RemotePeerID))
	n.wrapChannel(conn, sess)
}

// dialAddr is the manual-connect counterpart to
// dialAndHandshake: skips UDP discovery and goes
// straight to transport.Dial + handshake. Returns
// immediately; the handshake runs in a goroutine.
//
// Used by the CLI `dial <addr>` command and any
// future "force connect" UI button. Cross-VLAN /
// cross-subnet peers don't show up in the UDP
// yellow-pages, so this is the escape hatch.
func (n *Node) dialAddr(addr string) {
	n.wg.Add(1)
	go func() {
		defer n.wg.Done()
		dctx, cancel := context.WithTimeout(n.ctx, 5*time.Second)
		defer cancel()
		conn, err := n.tr.Dial(dctx, addr)
		if err != nil {
			log.Printf("[ERROR] dial %s: %v", addr, err)
			return
		}
		sess, err := handshake.RunAsInitiator(dctx, n.id, conn)
		if err != nil {
			log.Printf("[ERROR] handshake (initiator) with %s: %v", addr, err)
			return
		}
		if n.channels.get(sess.RemotePeerID) != nil {
			return
		}
		log.Printf("[HANDS] ok       peer=%s (initiator) addr=%s", peerHex(sess.RemotePeerID), addr)
		n.wrapChannel(conn, sess)
	}()
}

// wrapChannel takes a freshly-handed-shaked Conn+Session
// and constructs a Channel. Starts the dispatcher pump
// for inbound envelopes on that Channel.
func (n *Node) wrapChannel(conn *transport.Conn, sess *handshake.Session) {
	// M4: bump last_seen on every channel open.
	n.aliasStore.Touch(peerHex(sess.RemotePeerID))
	// M5: same touch for the roster.
	n.rosterStore.Touch(peerHex(sess.RemotePeerID))

	ch, err := protocol.NewChannel(conn, sess)
	if err != nil {
		log.Printf("[ERROR] new channel: %v", err)
		_ = conn.Close()
		return
	}
	// v0.5.2: mark this peer's /24 as known so auto-scan
	// doesn't re-enqueue it.
	n.autoScan.MarkConnectedSubnet(ch.RemoteAddr())

	peerHexStr := peerHex(sess.RemotePeerID)
	rcv, err := filetransfer.NewReceiver(ch, n.layout.Received, func(o filetransfer.FileOffer, _ string) error {
		log.Printf("[FILE] incoming peer=%s name=%q size=%d from=%s",
			peerHexStr, o.Name, o.Size, peerHexStr)
		return nil // accept everything by default; UI may add a confirmation hook
	}, peerHexStr)
	if err != nil {
		log.Printf("[ERROR] filetransfer receiver for %s: %v", peerHexStr, err)
		_ = ch.Close()
		return
	}
	// GUI chat panel: announce received file with a
	// "file://<name>" message so the chat UI can render
	// a file card next to the drop. Body is intentionally
	// prefixed with file:// (frontend parses this).
	rcv.SetOnComplete(func(name, finalPath, groupID string) {
		log.Printf("[FILE] received %s -> %s (peer=%s group=%q)", name, finalPath, peerHexStr, groupID)
		// The body's name field reflects what the receiver
		// ACTUALLY saved, not the original offer name. If
		// the receiver had to rename on collision (e.g.
		// "download.png" -> "download (1).png"), the chat
		// card on the receiver side should show the renamed
		// name so the user can find the file in Explorer.
		savedName := filepath.Base(finalPath)
		sizeStr := ""
		if info, err := os.Stat(finalPath); err == nil {
			sizeStr = humanSize(info.Size())
		}
		body := "file://" + savedName
		if sizeStr != "" {
			body += "|" + sizeStr
		}
		now := time.Now().UTC()
		// v1.1 (2026-06-28): group vs 1:1 routing. For
		// group file offers, we route to per-group chat.enc
		// + publish Message with PeerID = groupID and
		// SenderID = the member who sent it. We also move
		// the file from the default received/ dir to
		// groups/<id>/received/ so all of a group's
		// artefacts live under one directory tree.
		if groupID != "" {
			groupDir := filepath.Join(n.dataDir(), storage.GroupDirName, groupID, storage.GroupReceivedDirName)
			if err := os.MkdirAll(groupDir, 0o755); err != nil {
				log.Printf("[WARN  ] mkdir group received dir %s: %v (keeping default location)", groupDir, err)
			} else {
				target := filetransfer.UniquePath(groupDir, savedName)
				if err := os.Rename(finalPath, target); err == nil {
					finalPath = target
				} else {
					// Cross-device link etc.; fall back to copy+remove.
					if err2 := moveFileCrossDev(finalPath, target); err2 != nil {
						log.Printf("[WARN  ] move group file %s -> %s: %v (keeping default location)", finalPath, target, err2)
					} else {
						finalPath = target
					}
				}
			}
			rec := &storage.Record{
				Timestamp: now,
				From:      peerHexStr,
				To:        "",
				Direction: "in",
				Body:      body,
				MsgID:     "",
				LocalPath: finalPath,
				GroupID:   groupID,
			}
			if err := n.chatStore.AppendGroup(groupID, rec); err != nil {
				log.Printf("[ERROR] chat log group append (file): %v", err)
			}
			n.appendHistory(rec)
			n.publishMessage(Message{
				PeerID:    groupID,
				SenderID:  peerHexStr,
				Body:      body,
				Timestamp: now,
				Direction: DirIn,
				LocalPath: finalPath,
			})
			return
		}
		n.publishMessage(Message{
			PeerID:    peerHexStr,
			Body:      body,
			Timestamp: now,
			Direction: DirIn,
			LocalPath: finalPath,
		})
		// Persist so the GUI can re-render the file
		// card after peer-switch / app-restart.
		// Without this, History() reload shows the
		// received files but their LocalPath is empty
		// and right-click → "open folder" fails.
		rec := &storage.Record{
			Timestamp: now,
			From:      peerHexStr,
			To:        n.id.PeerIDHex(),
			Direction: "in",
			Body:      body,
			MsgID:     "",
			LocalPath: finalPath,
		}
		if err := n.chatStore.Append(rec); err != nil {
			log.Printf("[ERROR] chat log append (file): %v", err)
		}
		n.appendHistory(rec)
	})
	if !n.channels.set(sess.RemotePeerID, &channelState{
		ch:     ch,
		rcv:    rcv,
		peerID: append([]byte(nil), sess.RemotePeerID...),
		pubKey: append([]byte(nil), sess.RemotePubKey...),
	}) {
		log.Printf("[INFO ] channel superseded peer=%s (keeping existing)", peerHexStr)
		_ = ch.Close()
		return
	}
	log.Printf("[INFO ] channel ready peer=%s", peerHexStr)
	n.publishPeerEvent(PeerEvent{Type: PeerOnline, PeerID: peerHexStr})

	// M5 gossip: send our roster to the new peer right now,
	// so the LAN-wide peer directory converges fast on connect.
	n.sendRosterSync(ch)
	// v0.5.3: also send our scan history.
	n.sendScanHistory(ch)
	// v1.1.2 (2026-06-30): push fresh TypeGroupRosterUpdate
	// envelopes for every group this new peer is a member of.
	// Pre-fix the only sync trigger was a roster-MUTATING
	// event (join / leave / rename), so a peer who came back
	// online after a long absence kept the stale pre-disconnect
	// roster on their disk. UI symptom: "stale member count
	// even though we all see each other + can chat" — chat
	// flows because message dispatch doesn't gate on roster,
	// but ListGroups returns the stale count. Now any peer
	// whose channel comes up gets a healing push so its
	// members.json re-aligns with the canonical truth on disk.
	n.syncRostersToPeer(peerHexStr, ch)
	// v1.1.4 (2026-07-02): replay any persisted
	// TypeGroupLeaveNotice entries to the new peer. If
	// the new peer is a group creator who missed a prior
	// LeaveGroup broadcast (we were offline at the time),
	// this is the path that heals their local roster.
	// See internal/leavelog package doc for the full
	// offline-replay rationale.
	n.syncLeaveNoticesToPeer(peerHexStr, ch)

	n.wg.Add(1)
	go func() {
		defer n.wg.Done()
		defer n.channels.delete(sess.RemotePeerID)
		defer n.publishPeerEvent(PeerEvent{Type: PeerOffline, PeerID: peerHexStr})
		for {
			if n.ctx.Err() != nil {
				return
			}
			env, err := ch.Recv(n.ctx)
			if err != nil {
				log.Printf("[INFO ] channel closed peer=%s (%v)", peerHexStr, err)
				return
			}
			switch env.Type {
			case protocol.TypeText:
				log.Printf("[MSG  ] in  <%s> %s", peerHexStr, string(env.Payload))
				n.aliasStore.Touch(peerHexStr)
				now := time.Now().UTC()
				if env.IsGroup() {
					// Group text: route to per-group chat.enc,
					// publish with PeerID = group ID so the GUI
					// sidebar lands the bubble in the group
					// conversation, and SenderID = original
					// sender so the GUI can render "Alice: hi".
					rendered := group.RenderGroupID(env.GroupID)
					rec := &storage.Record{
						Timestamp: now,
						From:      peerHexStr,
						To:        "",
						Direction: "in",
						Body:      string(env.Payload),
						MsgID:     "",
						GroupID:   rendered,
					}
					if err := n.chatStore.AppendGroup(rendered, rec); err != nil {
						log.Printf("[ERROR] chat log group append: %v", err)
					}
					n.appendHistory(rec)
					n.publishMessage(Message{
						PeerID:    rendered,
						SenderID:  peerHexStr,
						Body:      string(env.Payload),
						Timestamp: rec.Timestamp,
						Direction: "in",
					})
					break
				}
				rec := &storage.Record{
					Timestamp: now,
					From:      peerHexStr,
					To:        n.id.PeerIDHex(),
					Direction: "in",
					Body:      string(env.Payload),
					MsgID:     "",
				}
				if err := n.chatStore.Append(rec); err != nil {
					log.Printf("[ERROR] chat log append: %v", err)
				}
				n.appendHistory(rec)
				n.publishMessage(Message{
					PeerID: peerHexStr, Body: string(env.Payload),
					Timestamp: rec.Timestamp, Direction: "in",
				})
			case protocol.TypePing:
				log.Printf("[MSG  ] in  <%s> ping", peerHexStr)
				n.aliasStore.Touch(peerHexStr)
				_ = ch.SendPong(n.ctx)
			case protocol.TypePong:
				log.Printf("[MSG  ] in  <%s> pong", peerHexStr)
				n.aliasStore.Touch(peerHexStr)
			case protocol.TypeRosterSync:
				var rs protocol.RosterSync
				if err := json.Unmarshal(env.Payload, &rs); err != nil {
					log.Printf("[ERROR] roster sync parse: %v", err)
					break
				}
				remote := make([]roster.Entry, 0, len(rs.Entries))
				for _, e := range rs.Entries {
					remote = append(remote, roster.Entry{
						PeerID:    e.PeerID,
						Hostname:  e.Hostname,
						Alias:     e.Alias,
						Addrs:     e.Addrs,
						FirstSeen: e.FirstSeen,
					})
				}
				res, err := n.rosterStore.MergeFromGossip(remote)
				if err != nil {
					log.Printf("[ERROR] roster merge: %v", err)
					break
				}
				if err := n.rosterStore.Save(); err != nil {
					log.Printf("[ERROR] roster save: %v", err)
				}
				if len(res.Added) > 0 {
					log.Printf("[ROSTER] sync from %s: %d new entries: %s",
						peerHexStr, len(res.Added), strings.Join(res.Added, ", "))
					n.broadcastRosterToAll(sess.RemotePeerID)
					if n.autoScan != nil {
						n.broadcastScanHistoryToAll(sess.RemotePeerID)
					}
					if n.autoScan != nil {
						for _, peerID := range res.Added {
							entry, err := n.rosterStore.Get(peerID)
							if err == nil {
								n.autoScan.EnqueueIfNew(entry.Addrs)
							}
						}
					}
				} else {
					log.Printf("[ROSTER] sync from %s: 0 new (already known; alias may have updated)", peerHexStr)
				}
				// Emit a local peer:event whenever the roster
				// changed in any visible way — new entries,
				// alias updates, or dedup resets. The frontend
				// listens for this and re-fetches ListPeers, so
				// a remote alias change becomes visible in B's
				// UI without waiting for B to do anything itself
				// (the user's reported bug: "A changed their
				// alias but B's list didn't update until B also
				// changed theirs" was exactly this signal being
				// missing).
				if len(res.Added)+len(res.AliasChanged)+len(res.Reset) > 0 {
					for _, pid := range res.Added {
						n.publishPeerEvent(PeerEvent{Type: PeerUpdated, PeerID: pid})
					}
					for _, pid := range res.AliasChanged {
						n.publishPeerEvent(PeerEvent{Type: PeerUpdated, PeerID: pid})
					}
					for _, pid := range res.Reset {
						n.publishPeerEvent(PeerEvent{Type: PeerUpdated, PeerID: pid})
					}
					if len(res.AliasChanged) > 0 {
						log.Printf("[ROSTER] alias updated for %s", strings.Join(res.AliasChanged, ", "))
					}
					if len(res.Reset) > 0 {
						log.Printf("[ROSTER] marked %s reset (ghost dedup)", strings.Join(res.Reset, ", "))
					}
				}
			case protocol.TypeScanHistory:
				var sh protocol.ScanHistory
				if err := json.Unmarshal(env.Payload, &sh); err != nil {
					log.Printf("[ERROR] scan-history unmarshal: %v", err)
					break
				}
				if n.autoScan != nil {
					for _, c := range sh.Scanned {
						n.autoScan.MarkScanned(c)
					}
					if len(sh.Scanned) > 0 {
						log.Printf("[SCAN-HIST] learned %d scanned subnet(s) from %s",
							len(sh.Scanned), peerHexStr)
					}
				}
			case protocol.TypeGroupInvite:
				// Inbound group invite (1:1 from current
				// member → us, prospective member). v1.1
				// auto-accepts after Verify; the GUI / CLI
				// can also call DeclineGroupInvite directly
				// in response to a "would you like to
				// join?" prompt (deferred to a UI follow-
				// up — for now we accept on receive).
				log.Printf("[GROUP ] invite from %s", peerHexStr)
				if err := n.AcceptGroupInvite(env, sess.RemotePeerID); err != nil {
					log.Printf("[ERROR] AcceptGroupInvite: %v", err)
				}
			case protocol.TypeGroupInviteAccept:
				// Accepter's confirmation. Add them to
				// our local members.json roster.
				log.Printf("[GROUP ] accept from %s", peerHexStr)
				if err := n.CreatorOnAccept(env, sess.RemotePeerID); err != nil {
					log.Printf("[ERROR] CreatorOnAccept: %v", err)
				}
			case protocol.TypeGroupInviteDecline:
				log.Printf("[GROUP ] decline from %s (invite was declined)", peerHexStr)
				// TODO: notify the user via the GUI; for
				// now we just log.
			case protocol.TypeGroupRosterUpdate:
				// v1.1.1 (2026-06-29): creator (or any
				// future roster-sync authority) broadcasts
				// the updated members.json to every existing
				// member when membership changes. Replace
				// our local members.json wholesale so the
				// sidebar + settings panel show the same
				// roster on every peer.
				log.Printf("[GROUP ] roster update from %s", peerHexStr)
				if err := n.ApplyRosterUpdate(env, sess.RemotePeerID); err != nil {
					log.Printf("[ERROR] ApplyRosterUpdate: %v", err)
				}
			case protocol.TypeGroupMetaUpdate:
				// v1.1.1 (2026-06-29): same idea as
				// roster update but for editable metadata
				// (name, remark). Sent on a SetGroupName /
				// SetGroupRemark so all peers see the new
				// name without having to re-fetch.
				log.Printf("[GROUP ] meta update from %s", peerHexStr)
				if err := n.ApplyMetaUpdate(env, sess.RemotePeerID); err != nil {
					log.Printf("[ERROR] ApplyMetaUpdate: %v", err)
				}
			case protocol.TypeGroupLeaveNotice:
				// v1.1.4 (2026-07-02): peer-to-peer
				// notification that the sender has left
				// (or is replaying a persisted "I left
				// this group" entry from a prior
				// offline LeaveGroup). Idempotent on the
				// receiver — see ApplyLeaveNotice.
				log.Printf("[GROUP ] leave notice from %s", peerHexStr)
				if err := n.ApplyLeaveNotice(env, sess.RemotePeerID); err != nil {
					log.Printf("[ERROR] ApplyLeaveNotice: %v", err)
				}
			default:
				// File traffic + anything else: hand off
				// to the file receiver. Handle() is also
				// the dispatch point for the Sender's
				// WaitForReply (Accept / Done / Abort).
				rcv.Handle(n.ctx, env)
			}
		}
	}()
}

// sendRosterSync encodes the local roster and ships it
// to a single peer. Each entry carries the peer's
// broadcast self-alias (alias.txt field) so the
// receiver learns every peer's display name in one
// message. Alias is omitted from the wire when empty
// (json:"omitempty") — older clients that don't know
// the field will see it as missing and default to "".
func (n *Node) sendRosterSync(ch *protocol.Channel) {
	entries := n.rosterStore.List()
	wire := protocol.RosterSync{
		Entries: make([]protocol.RosterEntry, 0, len(entries)),
	}
	for _, e := range entries {
		wire.Entries = append(wire.Entries, protocol.RosterEntry{
			PeerID:    e.PeerID,
			Hostname:  e.Hostname,
			Alias:     e.Alias,
			Addrs:     e.Addrs,
			FirstSeen: e.FirstSeen,
		})
	}
	payload, err := json.Marshal(wire)
	if err != nil {
		log.Printf("[ERROR] roster sync marshal: %v", err)
		return
	}
	if err := ch.Send(n.ctx, protocol.Envelope{
		Type:    protocol.TypeRosterSync,
		Payload: payload,
	}); err != nil {
		log.Printf("[ERROR] roster sync send: %v", err)
	}
}

// broadcastRosterToAll fans the local roster out to
// every active channel except the one specified by
// exclude (the peer we just received from — pushing
// back to it is wasted bytes).
func (n *Node) broadcastRosterToAll(exclude []byte) {
	all := n.channels.snapshot()
	for _, st := range all {
		if string(st.peerID) == string(exclude) {
			continue
		}
		n.sendRosterSync(st.ch)
	}
}

// syncLeaveNoticesToPeer replays every entry in our
// leavelog to peerHexStr as a TypeGroupLeaveNotice
// envelope over the freshly established channel. v1.1.4
// (2026-07-02) — see internal/leavelog package doc for
// the offline-replay rationale.
//
// Called right after syncRostersToPeer in handleInbound
// so that the peer we're connecting to gets our "I left
// these groups" history at the same time as the rest
// of the sync. The receiver's ApplyLeaveNotice is
// idempotent (no-op when the leaver isn't in their
// local roster), so a re-broadcast of a notice the peer
// already processed does no harm and incurs one packet
// per entry.
//
// Skipped when the leavelog is empty (the common case
// for a peer that has never left a group).
func (n *Node) syncLeaveNoticesToPeer(peerHex string, ch *protocol.Channel) {
	if n.leavelog == nil || ch == nil || len(peerHex) == 0 {
		return
	}
	entries := n.leavelog.List()
	if len(entries) == 0 {
		return
	}
	sent := 0
	for _, e := range entries {
		// We only send a notice if WE were the leaver
		// (this is always true — the log is per-device,
		// not per-group — but the comment is here so a
		// future "I saw someone else leave" log
		// structure doesn't surprise readers). The
		// leaver_id field carries our own peerID; the
		// receiver uses it to find the row in their
		// members.json and RemoveMember(leaver_id).
		notice := protocol.LeaveNotice{
			GroupID:  e.GroupID,
			LeaverID: n.id.PeerIDHex(),
			LeftAt:   e.LeftAt,
		}
		payload, err := json.Marshal(notice)
		if err != nil {
			log.Printf("[WARN ] syncLeaveNoticesToPeer(%s): marshal %s: %v", peerHex, e.GroupID, err)
			continue
		}
		env := protocol.Envelope{
			Type:    protocol.TypeGroupLeaveNotice,
			Payload: payload,
		}
		if err := ch.Send(n.ctx, env); err != nil {
			log.Printf("[WARN ] syncLeaveNoticesToPeer(%s): send %s: %v", peerHex, e.GroupID, err)
			continue
		}
		sent++
	}
	if sent > 0 {
		log.Printf("[GROUP ] leave pre-sync to %s: %d notice(s)", peerHex, sent)
	}
}

// ApplyLeaveNotice is the receiver-side handler for
// TypeGroupLeaveNotice. Sent 1:1 from a peer who has
// just left (or is replaying a persisted leave) to the
// group's creator. v1.1.4 (2026-07-02).
//
// Idempotent contract:
//   - We don't have a local members.json for GroupID →
//     the group is gone on our side (either we never
//     joined, or we already self-dissolved it). No-op.
//   - The leaver isn't in our local roster → the
//     "offline broadcast succeeded" branch already
//     removed them. No-op.
//   - The leaver IS in our local roster → RemoveMember
//     + Save + (if empty after removal) delete the
//     group + broadcast the new roster to the
//     remaining members. The new roster may also be
//     empty if we (the creator) were the only other
//     member — in that case we self-dissolve.
//
// This is the offline-replay healing path: when peer A
// calls LeaveGroup while the creator is offline, the
// online best-effort broadcast fails. A persists the
// leave in leavelog, and on the next handshake A
// replays the notice. The creator's ApplyLeaveNotice
// is what actually drops A from the local roster so
// the creator's UI doesn't get stuck at "<N> 成员"
// with A still listed.
func (n *Node) ApplyLeaveNotice(env protocol.Envelope, fromPeerID []byte) error {
	var ln protocol.LeaveNotice
	if err := json.Unmarshal(env.Payload, &ln); err != nil {
		return fmt.Errorf("node: ApplyLeaveNotice unmarshal: %w", err)
	}
	rawID, err := group.ParseGroupID(ln.GroupID)
	if err != nil {
		return fmt.Errorf("node: ApplyLeaveNotice bad GroupID: %w", err)
	}
	// 1. No local members.json → group is gone for us.
	// This is the common case for non-creator members
	// (a regular member receiving a leave notice was
	//never in the group) and also covers "we already
	//self-dissolved because we were the only remaining
	//member". Either way: no-op.
	m, err := group.LoadMembers(n.dataDir(), rawID)
	if err != nil {
		if os.IsNotExist(err) {
			// Quiet path: this is the "replay hit a
			// group we don't have" branch, which is
			// expected for a non-creator who is the
			// notice target. No log here — the
			// caller already logged "leave notice
			// from <peer>" at INFO level.
			return nil
		}
		return fmt.Errorf("node: ApplyLeaveNotice load: %w", err)
	}
	// 2. Leaver not in our roster → already removed
	// (probably via the online best-effort broadcast
	// in a prior session). Idempotent no-op.
	if !m.Contains(ln.LeaverID) {
		return nil
	}
	// 3. Leaver is in our roster. Remove + (maybe
	// dissolve) + broadcast.
	if !m.RemoveMember(ln.LeaverID) {
		// RemoveMember returns false when the leaver is
		// the Creator. That can only happen if the
		// leaver is somehow the same peer as the
		// creator, which by definition can't be true
		// (the creator never leaves their own group
		// via LeaveGroup — the solo-creator branch
		// self-dissolves instead). Defensive log +
		// no-op.
		log.Printf("[WARN ] ApplyLeaveNotice: RemoveMember(%s) returned false for group=%s; ignoring",
			ln.LeaverID[:8], ln.GroupID[:8])
		return nil
	}
	log.Printf("[GROUP ] leave notice applied: dropping %s from group=%s (now %d members)",
		ln.LeaverID[:8], ln.GroupID[:8], len(m.Members))
	// If we just emptied the roster, dissolve the group
	// (mirrors LeaveGroup's own empty-members branch in
	// pkg/node/groups.go).
	if len(m.Members) == 0 {
		// Use the same helper LeaveGroup uses so the
		// local cleanup is identical (members.json +
		// chat.enc wiped together).
		if err := n.deleteGroupDirsLocal(m.GroupID); err != nil {
			return fmt.Errorf("node: ApplyLeaveNotice dissolve: %w", err)
		}
		log.Printf("[GROUP ] leave notice dissolved empty group=%s", ln.GroupID[:8])
		n.publishGroupEvent(GroupEvent{
			Type:      GroupRemoved,
			GroupID:   m.GroupID,
			GroupName: m.GroupName,
		})
		return nil
	}
	if err := m.Save(n.dataDir(), rawID); err != nil {
		return fmt.Errorf("node: ApplyLeaveNotice save: %w", err)
	}
	// Broadcast the new roster to remaining members
	// (best-effort; offline members will catch up on
	// reconnect via syncRostersToPeer).
	n.broadcastRosterUpdate(m)
	n.publishGroupEvent(GroupEvent{
		Type:      GroupUpdated,
		GroupID:   m.GroupID,
		GroupName: m.GroupName,
	})
	return nil
}

// GetSelfAlias returns our broadcast display name (the
// string from <DataDir>/alias.txt, "" if unset). This
// is what other peers will see when our roster entry
// reaches them.
func (n *Node) GetSelfAlias() string {
	return n.selfAlias.Get()
}

// SetSelfAlias sets our broadcast display name. Empty
// string clears it. Returns nil on success, or one of
// the sentinel errors from selfalias.go (ErrNameTooLong,
// ErrNameHasNewline) on bad input.
//
// When the value actually changes (compared against
// the in-memory current), the change is persisted to
// disk AND broadcast to every connected peer via
// RosterSync. This satisfies the user requirement
// "改了别名要广播给其他客户端 (只要变就广播)" — the
// broadcast happens on every change, not just on
// startup. Peers that come online later will pull our
// roster on their first channel-ready, so they also
// learn the current alias without us needing to
// retain undelivered messages.
func (n *Node) SetSelfAlias(name string) error {
	changed, err := n.selfAlias.Set(name)
	if err != nil {
		return err
	}
	if !changed {
		return nil
	}
	if err := n.selfAlias.Save(); err != nil {
		// Roll back in-memory state? No — disk write
		// failure is rare and the next SetSelfAlias
		// call will retry. Logging is enough.
		log.Printf("[ERROR] self alias save: %v", err)
		return err
	}
	// Update the local self-entry in the roster so the
	// next RosterSync we send carries the new alias
	// for ourselves.
	selfHex := n.id.PeerIDHex()
	if existing, err := n.rosterStore.Get(selfHex); err == nil {
		existing.Alias = n.selfAlias.Get()
		if _, addErr := n.rosterStore.Add(existing); addErr != nil {
			log.Printf("[WARN ] roster self-alias refresh: %v", addErr)
		}
	}
	log.Printf("[INFO ] self alias updated: %q", n.selfAlias.Get())
	// Broadcast to every connected peer (exclude nil —
	// we want EVERY peer to get this, including the
	// ones that just received our old alias in their
	// last RosterSync; they need the update too).
	n.broadcastRosterToAll(nil)
	return nil
}

// sendScanHistory sends the v0.5.3 scan-history gossip
// envelope to one peer.
func (n *Node) sendScanHistory(ch *protocol.Channel) {
	_, seen := n.autoScan.Queue().Snapshot()
	wire := protocol.ScanHistory{Scanned: seen}
	payload, err := json.Marshal(wire)
	if err != nil {
		log.Printf("[ERROR] scan-history marshal: %v", err)
		return
	}
	if err := ch.Send(n.ctx, protocol.Envelope{
		Type:    protocol.TypeScanHistory,
		Payload: payload,
	}); err != nil {
		log.Printf("[ERROR] scan-history send: %v", err)
	}
}

// broadcastScanHistoryToAll fans the local scan history
// out to every active channel except the one specified.
func (n *Node) broadcastScanHistoryToAll(exclude []byte) {
	all := n.channels.snapshot()
	for _, st := range all {
		if string(st.peerID) == string(exclude) {
			continue
		}
		n.sendScanHistory(st.ch)
	}
}

// autoScanLoop is the Node method that runs the auto-scan
// queue consumer. It calls Scan for each subnet the
// queue produces, then marks the subnet as known via
// MarkScanned so a future roster update doesn't re-enqueue.
//
// Single-goroutine by design 鈥?sequential scans prevent
// overloading the LAN when several /24s get gossiped at
// once. MarkScanned happens after the scan returns so
// a transient failure doesn't permanently skip the subnet.
func (n *Node) autoScanLoop() {
	for {
		select {
		case <-n.ctx.Done():
			return
		case cidr := <-n.autoScan.queue.ch:
			log.Printf("[AUTOSCAN] %s (triggered by new roster entry)", cidr)
			err := n.Scan(n.ctx, cidr)
			n.autoScan.MarkScanned(cidr)
			if err != nil && n.ctx.Err() == nil {
				log.Printf("[ERROR] auto-scan %s: %v", cidr, err)
			}
		}
	}
}

// --- shared bits moved out of cmd/innerlink/main.go ---

// defaultLogFile returns the default log file path.
// Kept here so flag default strings read naturally.
func defaultLogFile() string {
	return "innerlink.log"
}

// shutdownDelay gives in-flight ops a moment to drain
// after ctx is canceled. The CLI sleeps 200ms; we
// preserve that here for callers that want a graceful
// shutdown pause.
const shutdownDelay = 200 * time.Millisecond

// keep os import referenced.
var _ = os.Stderr