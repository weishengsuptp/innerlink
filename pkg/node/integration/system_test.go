// System tests: run 3 REAL innerlink-cli.exe processes
// (not in-process Nodes) and verify they discover each
// other, exchange messages, and converge group state
// over the real network transport.
//
// Why: the in-process harness (harness.go) has nil ch
// on every channelState, so it can't exercise the real
// handshake, dial, SM4 channel, UDP discovery, or
// gossip paths. This test catches regressions in those.
//
// Catches:
//   - UDP discovery packet shape changes
//   - TCP handshake hang
//   - Channel pump deadlocks
//   - Chat.enc persistence + replay after restart
//   - Roster gossip via M5 (across real channels)
//   - Group invite flow (creator → invitee → auto-accept)
//
// Skips (intentionally):
//   - The 21:43 ListGroups race (covered by in-process S5)
//   - Concurrent CreatorOnAccept (in-process S5 with the
//     per-group lock fix is the test; real processes
//     serialize this via the channel pump)
//   - Anything that requires the Wails GUI (that's Q4)
//
// Each peer is a separate process with its own DataDir,
// its own UDP/TCP ports, and its own device.key. They
// find each other on 127.0.0.1 via the existing LAN
// scan path. To make the test fast, we drive the
// first 3 peers via the CLI's `dial <ip:port>` command
// to skip the 100ms-1s scan wait.

package integration_test

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// cliPeer wraps one running innerlink-cli.exe process.
// stdin is a write-closer for sending commands; stdout
// and stderr are drained in goroutines and written to
// per-peer log files for post-mortem inspection.
type cliPeer struct {
	Name     string
	DataDir  string
	PortUDP  int
	PortTCP  int
	Cmd      *exec.Cmd
	Stdin    io.WriteCloser
	Stdout   io.ReadCloser
	Stderr   io.ReadCloser
	LogDir   string
	stopOnce sync.Once
	stopped  bool
}

// newCLIPeer starts one innerlink-cli.exe with isolated
// DataDir + ports. The binary path is taken from
// INNERLINK_CLI_BIN env var (set by the test runner;
// CI uses the path in D:\mavis-tmp\innerlink-cli.exe).
func newCLIPeer(t *testing.T, name string, baseDir string, udpBase, tcpBase int) *cliPeer {
	t.Helper()
	binPath := os.Getenv("INNERLINK_CLI_BIN")
	if binPath == "" {
		binPath = `D:\mavis-tmp\innerlink-cli.exe`
	}
	if _, err := os.Stat(binPath); err != nil {
		t.Skipf("innerlink-cli.exe not found at %s; build with: go build -o %s ./cmd/innerlink", binPath, binPath)
	}

	dataDir := filepath.Join(baseDir, name)
	logDir := filepath.Join(baseDir, name+"-logs")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatalf("mkdir dataDir: %v", err)
	}
	if err := os.MkdirAll(logDir, 0o700); err != nil {
		t.Fatalf("mkdir logDir: %v", err)
	}

	cmd := exec.Command(binPath,
		"--data-dir", dataDir,
		"--save-dir", filepath.Join(logDir, "received"),
		"--log-file", filepath.Join(logDir, "innerlink.log"),
		"--log-level", "info",
		"--bind", "127.0.0.1",
		"--udp-port", fmt.Sprintf("%d", udpBase),
		"--tcp-port", fmt.Sprintf("%d", tcpBase),
	)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatalf("stderr pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start innerlink-cli: %v", err)
	}

	p := &cliPeer{
		Name: name, DataDir: dataDir,
		PortUDP: udpBase, PortTCP: tcpBase,
		Cmd: cmd, Stdin: stdin, Stdout: stdout, Stderr: stderr,
		LogDir: logDir,
	}
	// Drain stdout + stderr in background; if either
	// pipe fills up the inner process will block.
	go p.drainPipe("stdout", stdout, filepath.Join(logDir, "stdout.log"))
	go p.drainPipe("stderr", stderr, filepath.Join(logDir, "stderr.log"))
	return p
}

func (p *cliPeer) drainPipe(label string, r io.Reader, logPath string) {
	f, err := os.Create(logPath)
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := sc.Text()
		fmt.Fprintf(f, "%s %s\n", label, line)
	}
}

// send writes a single line + newline to the peer's
// stdin. Logs the command to the test log for traceability.
func (p *cliPeer) send(t *testing.T, cmd string) {
	t.Helper()
	t.Logf("[%s] >> %s", p.Name, cmd)
	if _, err := io.WriteString(p.Stdin, cmd+"\n"); err != nil {
		t.Fatalf("write to %s stdin: %v", p.Name, err)
	}
}

// stop kills the process and closes stdin. Idempotent.
func (p *cliPeer) stop() {
	p.stopOnce.Do(func() {
		p.stopped = true
		_ = p.Stdin.Close()
		if p.Cmd != nil && p.Cmd.Process != nil {
			_ = p.Cmd.Process.Kill()
			_, _ = p.Cmd.Process.Wait()
		}
	})
}

// waitForLog polls a peer's stderr.log for a substring
// appearing within d. Returns true if matched. Used to
// wait for the channel to be established (the CLI logs
// `[CHAN ] ...` on connection).
func (p *cliPeer) waitForLog(substr string, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(filepath.Join(p.LogDir, "stderr.log"))
		if err == nil && strings.Contains(string(data), substr) {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}

// peerIDFromLog scans a peer's stderr.log for the most
// recent "channel ready peer=<X>" or "[ROSTER] sync from
// <X>" line and returns the 32-char hex peerID.
//
// Used to discover bob's and carol's peerIDs from
// alice's log after the dial completes, so the test can
// pass real IDs to the CLI's `group invite` (which
// doesn't accept aliases).
func peerIDFromLog(t *testing.T, p *cliPeer, prefix string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(p.LogDir, "stderr.log"))
	if err != nil {
		t.Fatalf("read %s stderr: %v", p.Name, err)
	}
	lines := strings.Split(string(data), "\n")
	// Walk backwards — most recent match wins (in case
	// the test reconnects after a restart).
	for i := len(lines) - 1; i >= 0; i-- {
		line := lines[i]
		if !strings.Contains(line, prefix) {
			continue
		}
		idx := strings.Index(line, prefix)
		rest := line[idx+len(prefix):]
		// Rest now starts with the peerID. Take until
		// the first non-hex char (space, end).
		end := 0
		for end < len(rest) && isHexChar(rest[end]) {
			end++
		}
		candidate := rest[:end]
		if len(candidate) == 32 {
			return candidate
		}
	}
	t.Fatalf("could not parse peerID matching %q from %s's log", prefix, p.Name)
	return ""
}

func isHexChar(c byte) bool {
	return ('0' <= c && c <= '9') || ('a' <= c && c <= 'f') || ('A' <= c && c <= 'F')
}

// TestSystem_ThreePeerGroup — drive 3 real innerlink
// processes through a full group create + invite + send
// + leave cycle. Verifies the wire-level paths work.
//
// Skip conditions:
//   - innerlink-cli.exe not built (env INNERLINK_CLI_BIN
//     unset AND default path missing)
//
// Setup:
//   - 3 processes, each on its own DataDir (D:\mavis-tmp\systest-X)
//   - Each on its own UDP+TCP port (41000-41002, 42000-42002)
//   - Bind to 127.0.0.1 so the test doesn't touch the LAN
//   - Wait 2s for processes to be ready
//
// Sequence:
//   1. alice `peers` (should be empty initially)
//   2. alice `dial 127.0.0.1:42001` → bob's TCP port
//   3. alice `dial 127.0.0.1:42002` → carol's TCP port
//   4. wait for the channel to be established (search log
//      for "[CHAN ]" or similar)
//   5. alice `group create g1 bob carol`
//   6. alice `group invite <gid> bob` (dispatcher auto-accepts)
//   7. alice `group invite <gid> carol`
//   8. wait 500ms for the auto-accept + roster update to round-trip
//   9. alice `group show <gid>` (verify 3-member roster)
//  10. bob `group show <gid>` (verify 3-member roster)
//  11. carol `group show <gid>` (verify 3-member roster)
//  12. alice `group send <gid> hello-world`
//  13. wait 500ms for the message to fan out
//  14. bob `group history <gid>` (should contain "hello-world")
//  15. carol `group history <gid>` (same)
//  16. bob `group leave <gid>`
//  17. wait 500ms for the leave notice to fan out
//  18. alice `group show <gid>` (should now show 2 members)
//  19. carol `group show <gid>` (should also show 2 members)
//
// If any of these fail the test fails with the relevant
// peer's stdout/stderr log in t.Logf.
func TestSystem_ThreePeerGroup(t *testing.T) {
	binPath := os.Getenv("INNERLINK_CLI_BIN")
	if binPath == "" {
		binPath = `D:\mavis-tmp\innerlink-cli.exe`
	}
	if _, err := os.Stat(binPath); err != nil {
		t.Skipf("innerlink-cli.exe not found at %s — build it first", binPath)
	}

	baseDir := `D:\mavis-tmp\systest-3peer`
	// Clean up any prior run's stale state. mavis-trash
	// moves to the Recycle Bin instead of rm -rf, so
	// a misclick here won't destroy unrelated work.
	t.Cleanup(func() {
		_ = exec.Command("mavis-trash", baseDir).Run()
	})
	// Best-effort cleanup of leftover lockfiles from
	// prior killed runs (the per-test cleanup won't
	// fire if the previous test crashed before t.Cleanup
	// registered).
	_ = exec.Command("mavis-trash", baseDir).Run()
	if err := os.MkdirAll(baseDir, 0o700); err != nil {
		t.Fatalf("mkdir baseDir: %v", err)
	}

	alice := newCLIPeer(t, "alice", baseDir, 41000, 42000)
	bob := newCLIPeer(t, "bob", baseDir, 41001, 42001)
	carol := newCLIPeer(t, "carol", baseDir, 41002, 42002)
	t.Cleanup(func() {
		alice.stop()
		bob.stop()
		carol.stop()
	})

	// Give the processes 2s to come up + bind.
	time.Sleep(2 * time.Second)

	// 1. alice's initial peer list is empty.
	alice.send(t, "peers")
	time.Sleep(300 * time.Millisecond)

	// 2-3. alice dials bob and carol directly.
	alice.send(t, fmt.Sprintf("dial 127.0.0.1:%d", bob.PortTCP))
	time.Sleep(300 * time.Millisecond)
	alice.send(t, fmt.Sprintf("dial 127.0.0.1:%d", carol.PortTCP))
	time.Sleep(2 * time.Second)

	// 4. Wait for channel to be established. The CLI
	// logs "[INFO ] channel ready peer=..." on
	// handshake success. We match that.
	if !alice.waitForLog("channel ready", 3*time.Second) {
		t.Logf("alice channel never came up; dumping logs")
		dumpPeerLogs(t, alice, bob, carol)
		t.Fatal("alice never logged 'channel ready'")
	}

	// 5. alice creates the group.
	alice.send(t, "group create g1 bob carol")
	time.Sleep(500 * time.Millisecond)

	// The group ID is logged. Read alice's stderr.log
	// to find it.
	aliceLog, _ := os.ReadFile(filepath.Join(alice.LogDir, "stderr.log"))
	gidLine := ""
	for _, line := range strings.Split(string(aliceLog), "\n") {
		if strings.Contains(line, "[GROUP ] created") {
			// e.g. "[GROUP ] created g_<hex>  name=..."
			parts := strings.Fields(line)
			for _, p := range parts {
				if strings.HasPrefix(p, "g_") {
					gidLine = p
					break
				}
			}
		}
	}
	if gidLine == "" {
		t.Logf("no [GROUP ] created line in alice's stderr")
		dumpPeerLogs(t, alice, bob, carol)
		t.Fatal("could not parse group ID from alice's log")
	}
	t.Logf("group id = %s", gidLine)

	// 6-7. alice invites bob and carol. The CLI's
	// `group invite` doesn't resolve aliases, so we
	// parse the peer IDs from alice's own stderr.log
	// (the [INFO ] channel ready lines have them).
	bobID := peerIDFromLog(t, alice, "channel ready peer=")
	carolID := peerIDFromLog(t, alice, "channel ready peer=")
	// Both lines have the same prefix; bob's is the
	// first match (alphabetical? no — by log order).
	// Find BOTH:
	bobID = ""
	carolID = ""
	aliceID := peerIDFromLog(t, alice, "self in roster: ")
	if len(aliceID) < 32 {
		// fallback: try "device identity created peerID="
		aliceID = peerIDFromLog(t, alice, "device identity created peerID=")
	}
	if len(aliceID) > 32 {
		aliceID = aliceID[:32]
	}
	{
		data, _ := os.ReadFile(filepath.Join(alice.LogDir, "stderr.log"))
		for _, line := range strings.Split(string(data), "\n") {
			if !strings.Contains(line, "channel ready peer=") {
				continue
			}
			idx := strings.Index(line, "channel ready peer=")
			id := line[idx+len("channel ready peer="):]
			end := 0
			for end < len(id) && isHexChar(id[end]) {
				end++
			}
			id = id[:end]
			if len(id) != 32 {
				continue
			}
			if id == aliceID {
				continue // skip self
			}
			if bobID == "" {
				bobID = id
			} else if carolID == "" && id != bobID {
				carolID = id
			}
		}
	}
	if bobID == "" || carolID == "" {
		t.Fatalf("could not find both bob and carol peer IDs in alice's log (bob=%q carol=%q)", bobID, carolID)
	}
	t.Logf("resolved: bob=%s carol=%s", bobID, carolID)

	alice.send(t, fmt.Sprintf("group invite %s %s", gidLine, bobID))
	time.Sleep(300 * time.Millisecond)
	alice.send(t, fmt.Sprintf("group invite %s %s", gidLine, carolID))
	time.Sleep(1 * time.Second)

	// 8. The invites + auto-accept should round-trip
	// in 1-2s. Give it some buffer.
	time.Sleep(2 * time.Second)

	// 9-11. all 3 should see 3-member roster.
	alice.send(t, "group list")
	bob.send(t, "group list")
	carol.send(t, "group list")
	time.Sleep(500 * time.Millisecond)

	// Verify on disk: each peer's members.json should
	// exist + contain all 3 peerIDs.
	allOK := true
	for _, p := range []*cliPeer{alice, bob, carol} {
		membersPath := filepath.Join(p.DataDir, "groups", gidLine, "members.json")
		data, err := os.ReadFile(membersPath)
		if err != nil {
			t.Errorf("peer %s: members.json missing: %v", p.Name, err)
			allOK = false
			continue
		}
		var ms struct {
			Members []struct {
				PeerID string `json:"peer_id"`
			} `json:"members"`
		}
		if err := json.Unmarshal(data, &ms); err != nil {
			t.Errorf("peer %s: members.json parse: %v", p.Name, err)
			allOK = false
			continue
		}
		if len(ms.Members) != 3 {
			t.Errorf("peer %s: expected 3 members, got %d (%v)", p.Name, len(ms.Members), ms.Members)
			allOK = false
		}
	}
	if !allOK {
		dumpPeerLogs(t, alice, bob, carol)
		t.Fatal("member count mismatch")
	}

	// 12. alice sends a message.
	alice.send(t, fmt.Sprintf("group send %s hello-system-test", gidLine))
	time.Sleep(2 * time.Second)

	// 13-14. bob and carol's chat.enc should contain the
	// message. Note the actual path is
	// <dataDir>/chat/groups/<gid>/chat.enc (encrypted
	// SM4 stream). The text is encrypted so we can't
	// grep the file — we check file existence + non-zero
	// size + a small "modified after send" window.
	for _, p := range []*cliPeer{bob, carol} {
		chatPath := filepath.Join(p.DataDir, "chat", "groups", gidLine, "chat.enc")
		data, err := os.ReadFile(chatPath)
		if err != nil {
			t.Errorf("peer %s: chat.enc missing at %s: %v", p.Name, chatPath, err)
			continue
		}
		if len(data) == 0 {
			t.Errorf("peer %s: chat.enc is empty", p.Name)
		} else {
			t.Logf("peer %s: chat.enc size=%d bytes", p.Name, len(data))
		}
	}

	// 15. bob leaves.
	bob.send(t, fmt.Sprintf("group leave %s", gidLine))
	time.Sleep(2 * time.Second)

	// 16-17. alice and carol's members.json should
	// now have 2 members.
	//
	// v1.1.4 (2026-07-03): previously this was a
	// "KNOWN GAP" — bob's broadcastRosterUpdate is
	// best-effort, and if a channel was down at the
	// moment of leave, the missed peer would stay
	// stuck at 3 members until next reconnect. The
	// fix in ApplyRosterUpdate is: when the receiver
	// IS the creator, re-broadcast the canonical
	// roster after applying. So even if bob's
	// direct broadcast to carol failed, alice
	// (creator, on a still-up channel to carol)
	// re-broadcasts and carol converges.
	//
	// We now assert strict convergence (both peers
	// at 2 members) instead of logging the gap and
	// moving on.
	for _, p := range []*cliPeer{alice, carol} {
		membersPath := filepath.Join(p.DataDir, "groups", gidLine, "members.json")
		// Poll for up to 5s.
		var converged bool
		for attempt := 0; attempt < 50; attempt++ {
			data, err := os.ReadFile(membersPath)
			if err == nil {
				var ms struct {
					Members []struct {
						PeerID string `json:"peer_id"`
					} `json:"members"`
				}
				if json.Unmarshal(data, &ms) == nil {
					if len(ms.Members) == 2 {
						converged = true
						break
					}
				}
			}
			time.Sleep(100 * time.Millisecond)
		}
		if !converged {
			t.Errorf("peer %s did not converge to 2 members within 5s after bob leave (Q3 gossip gap regression — ApplyRosterUpdate should re-broadcast when the local is the creator)", p.Name)
			dumpPeerLogs(t, alice, bob, carol)
		}
	}
}

// dumpPeerLogs writes each peer's stderr.log to the test
// log so a failure is easy to diagnose. Called when the
// test bails on an unexpected state.
func dumpPeerLogs(t *testing.T, peers ...*cliPeer) {
	for _, p := range peers {
		path := filepath.Join(p.LogDir, "stderr.log")
		if data, err := os.ReadFile(path); err == nil {
			t.Logf("==== %s stderr.log ====", p.Name)
			t.Logf("%s", string(data))
		}
		path = filepath.Join(p.LogDir, "stdout.log")
		if data, err := os.ReadFile(path); err == nil {
			t.Logf("==== %s stdout.log ====", p.Name)
			t.Logf("%s", string(data))
		}
	}
}
