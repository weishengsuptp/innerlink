// innerlink group sync e2e (3-peer)
//
// Spins up 3 innerlink CLI instances, drives them via
// REPL on stdin, verifies the roster-sync flow end-to-end
// over real TCP:
//   1. A creates a group
//   2. A invites B and C
//   3. Wait for accept + roster sync
//   4. All 3 should see the same number of members
//      (3 — creator + 2 invitees) in `group show`
//   5. A renames the group via `group set-name`
//      (or — until that CLI command lands — via the
//      Go-side SetGroupName; for now we just verify
//      the roster consistency which is the user-visible
//      bug)
//
// This catches the v1.1.1 bug where CreatorOnAccept
// wrote the new member to disk but never told the
// creator's own frontend to refresh ListGroups — the
// creator's CLI's `group show` would still report
// 1 member even after both invites + accepts.
//
// Usage:
//   go build -o <BIN> ./cmd/innerlink
//   BIN=<BIN> node tests/e2e/test-e2e-groups.js

const { spawn } = require('node:child_process');
const fs = require('node:fs');
const path = require('node:path');

const BIN = process.env.BIN || 'D:\\mavis-tmp\\test\\innerlink.exe';
const ROOT = process.env.ROOT || 'D:\\mavis-tmp\\test';

// Wipe previous state so each run starts clean.
for (const sub of ['a', 'b', 'c']) {
  const dir = path.join(ROOT, sub);
  if (fs.existsSync(dir)) {
    for (const f of fs.readdirSync(dir)) {
      try { fs.unlinkSync(path.join(dir, f)); } catch (_) {}
    }
  }
}

const instances = [
  { id: 'A', data: path.join(ROOT, 'a'), udp: 2747, tcp: 2748 },
  { id: 'B', data: path.join(ROOT, 'b'), udp:12747, tcp:12748 },
  { id: 'C', data: path.join(ROOT, 'c'), udp:22747, tcp:22748 },
];

let procs = [];

function spawnInst(inst) {
  const logFile = path.join(ROOT, `${inst.id.toLowerCase()}.log`);
  const stdoutFile = path.join(ROOT, `${inst.id.toLowerCase()}.stdout`);
  const stderrFile = path.join(ROOT, `${inst.id.toLowerCase()}.stderr`);
  fs.writeFileSync(logFile, '');
  fs.writeFileSync(stdoutFile, '');
  fs.writeFileSync(stderrFile, '');
  const stdout = fs.openSync(stdoutFile, 'a');
  const stderr = fs.openSync(stderrFile, 'a');
  const p = spawn(BIN, [
    '-data-dir', inst.data,
    '-bind', '127.0.0.1',
    '-udp-port', String(inst.udp),
    '-tcp-port', String(inst.tcp),
    '-auto-scan=false',
    '-log-level=info',
    `-log-file=${logFile}`,
  ], { stdio: ['pipe', stdout, stderr], windowsHide: true });
  p.on('exit', (code, signal) => {
    console.log(`[${inst.id}] exit code=${code} signal=${signal}`);
  });
  return p;
}

function startAll() {
  for (const inst of instances) {
    const p = spawnInst(inst);
    procs.push({ inst, proc: p });
  }
}

function send(instId, cmd) {
  const rec = procs.find(p => p.inst.id === instId);
  if (!rec) throw new Error(`unknown instance ${instId}`);
  rec.proc.stdin.write(cmd + '\n');
}

function sleep(ms) { return new Promise(r => setTimeout(r, ms)); }

let failures = 0;
function assert(cond, msg) {
  if (!cond) {
    console.log(`  \u274C ASSERT FAIL: ${msg}`);
    failures++;
    return false;
  }
  console.log(`  \u2705 ${msg}`);
  return true;
}

// lastGroupShowOutput returns the most recent `group show <id>`
// line in instance I's log, parsed for member count.
function lastGroupShowOutput(instId, gid) {
  const f = path.join(ROOT, `${instId.toLowerCase()}.log`);
  if (!fs.existsSync(f)) return null;
  const lines = fs.readFileSync(f, 'utf8').split(/\r?\n/);
  // The line format from cmdGroupShow is:
  //   [GROUP ] g_<hex>  name="..."  creator=<hex8>  members=N  self_member=...
  // We match on `members=N` after a group_id that starts
  // with the prefix we expect.
  for (let i = lines.length - 1; i >= 0; i--) {
    const line = lines[i];
    if (line.includes('members=') && line.includes(gid)) {
      const m = line.match(/members=(\d+)\s+self_member=(\w+)/);
      if (m) {
        return { count: parseInt(m[1]), selfMember: m[2] === 'true' };
      }
    }
  }
  return null;
}

const TIMELINE = [];
function at(ms, action, note) { TIMELINE.push({ atMs: ms, action, note }); }

// ========== SCENARIOS ==========

// Discovery: B and C dial A so A learns both peerIDs.
// group create needs hex peerIDs (or aliases) so we
// need the roster populated first.
at(2500, async () => {
  console.log('\n=== DISCOVERY: B and C dial A ===');
  send('B', 'dial 127.0.0.1:2748');
  send('C', 'dial 127.0.0.1:2748');
  await sleep(3500);
  send('A', 'peers');
  await sleep(500);
  // We don't strictly need to assert here — the
  // dial succeeded if A's group invite later doesn't
  // fail with "peer offline".
}, 'discovery dial');

// G1: A creates the group. Capture the GroupID for
// later commands.
let createdGroupID = null;
at(7000, async () => {
  console.log('\n=== G1: A creates group "e2e群" ===');
  send('A', 'group create e2e群');
  await sleep(500);
  // Parse the group ID out of A's log: the `created
  // <id>` line.
  const aLog = fs.readFileSync(path.join(ROOT, 'a.log'), 'utf8');
  const m = aLog.match(/created\s+(g_[0-9a-f]{64})/);
  if (!m) {
    assert(false, 'A: could not parse created group id from log');
    return;
  }
  createdGroupID = m[1];
  console.log(`  captured group id: ${createdGroupID}`);
  // Print `group show` so the members=N line lands in
  // the log — lastGroupShowOutput only finds lines that
  // actually exist.
  send('A', `group show ${createdGroupID}`);
  await sleep(300);
  // A should have 1 member (itself) immediately.
  const r = lastGroupShowOutput('A', createdGroupID);
  assert(r !== null, 'A: has a group show line');
  if (r) assert(r.count === 1, `A: initial members count = ${r.count}, want 1 (creator only)`);
}, 'A: create group');

// G2: A invites B and C. After ~3s the roster-sync
// envelopes should land and all three should agree
// the group has 3 members.
at(9000, async () => {
  console.log('\n=== G2: A invites B and C ===');
  // We need B and C's peerIDs on A's roster. Use the
  // `peers` output of A to extract them. Each line is
  // "[PEERS] <name> last seen <ts> (<peerID>)".
  const aLog = fs.readFileSync(path.join(ROOT, 'a.log'), 'utf8');
  // Find the LAST "N known peer" line + the lines below
  // until the empty line.
  const lines = aLog.split(/\r?\n/);
  let idx = -1;
  for (let i = lines.length - 1; i >= 0; i--) {
    if (/known peer\(s\)/.test(lines[i])) { idx = i; break; }
  }
  if (idx < 0) {
    assert(false, 'A: no known-peer(s) line found; discovery may have failed');
    return;
  }
  const peers = [];
  for (let j = idx + 1; j < lines.length; j++) {
    const m = lines[j].match(/\(([0-9a-f]{32})\)/);
    if (!m) break;
    peers.push(m[1]);
  }
  console.log(`  A's roster peerIDs: ${JSON.stringify(peers)}`);
  if (peers.length < 2) {
    assert(false, `A: roster has only ${peers.length} peers, need 2 (B+C)`);
    return;
  }
  // Invite both.
  send('A', `group invite ${createdGroupID} ${peers[0]}`);
  await sleep(300);
  send('A', `group invite ${createdGroupID} ${peers[1]}`);
  await sleep(4000); // wait for invites + accepts + roster-sync broadcast

  // Have all three instances print `group show` so the
  // members=N line lands in each log; lastGroupShowOutput
  // searches for that line.
  send('A', `group show ${createdGroupID}`);
  send('B', `group show ${createdGroupID}`);
  send('C', `group show ${createdGroupID}`);
  await sleep(500);

  const aR = lastGroupShowOutput('A', createdGroupID);
  const bR = lastGroupShowOutput('B', createdGroupID);
  const cR = lastGroupShowOutput('C', createdGroupID);
  console.log(`  members: A=${aR?.count} B=${bR?.count} C=${cR?.count} ` +
    `(self: A=${aR?.selfMember} B=${bR?.selfMember} C=${cR?.selfMember})`);
  assert(aR?.count === 3, `A: members = ${aR?.count}, want 3 (creator + B + C). ` +
    `Pre-fix bug: A would stay at 1 because CreatorOnAccept never told the local frontend to refresh.`);
  assert(bR?.count === 3, `B: members = ${bR?.count}, want 3. Pre-fix: 2 (creator + self).`);
  assert(cR?.count === 3, `C: members = ${cR?.count}, want 3. Pre-fix: 2 (creator + self).`);
  // All three should be self_member=true
  assert(aR?.selfMember === true, `A: self_member should be true`);
  assert(bR?.selfMember === true, `B: self_member should be true`);
  assert(cR?.selfMember === true, `C: self_member should be true`);
}, 'A: invite + assert 3-peer sync');

// G3: A renames the group. After ~2s, B and C should
// see the new name too.
at(16000, async () => {
  console.log('\n=== G3: A renames group to "新群名" ===');
  send('A', `group show ${createdGroupID}`); // warm cache, capture old name
  await sleep(200);
  // The CLI doesn't have a set-name command yet, so we
  // skip this assertion if there's no CLI surface. For
  // now, we just verify the rename-via-Go API path
  // works by checking that the underlying envelope
  // machinery is in place — the Go-side is verified
  // by the unit tests; the CLI command is a TODO.
  // Skip silently if no command: assert nothing.
  console.log('  (CLI rename not yet wired — covered by Go unit tests)');
}, 'A: rename (TODO: CLI command)');

// G4: best-effort dial for VM-to-VM (no direct channel)
// pre-fix bug: VM-B sends a message; VM-C has no channel
// to VM-B (they dialed A only); the broadcast loop in
// SendGroupMessage drops silently. Post-fix (v1.1.1,
// 2026-06-30): it fires a best-effort dialAddr() in
// the background. The CURRENT message still drops,
// but the NEXT message after dial completes goes
// through.
//
// Setup: B and C dialed A only (per DISCOVERY at T+2500).
// No direct B-C channel. C sends a message; we verify:
//   1. C's log shows "firing best-effort dial" for B
//   2. A receives the first message immediately
//   3. B drops the first message (logged)
//   4. After waiting for dial (~2 s), C sends another
//      message and B picks it up.
at(17000, async () => {
  console.log('\n=== G4: best-effort dial for VM-to-VM group broadcast ===');
  // First message — expect B to NOT have it (no channel yet).
  send('C', `group send ${createdGroupID} g4-first`);
  await sleep(1500);
  // Second message after the dial should have completed.
  send('C', `group send ${createdGroupID} g4-second`);
  await sleep(2500);

  const cLog = fs.readFileSync(path.join(ROOT, 'c.log'), 'utf8');
  const aLog = fs.readFileSync(path.join(ROOT, 'a.log'), 'utf8');
  const bLog = fs.readFileSync(path.join(ROOT, 'b.log'), 'utf8');

  assert(/firing best-effort dial/.test(cLog),
    'C log shows best-effort dial fired for offline peer');
  // A should have received BOTH messages (it always has a channel).
  assert(/\[MSG  \] in  <[^>]+> g4-first/.test(aLog),
    'A received g4-first on first send');
  assert(/\[MSG  \] in  <[^>]+> g4-second/.test(aLog),
    'A received g4-second on second send');
  // B's first message must have been dropped (no channel yet at that moment).
  // We check by absence of an inbound log line for g4-first.
  const bHasFirst = /\[MSG  \] in  <[^>]+> g4-first/.test(bLog);
  assert(!bHasFirst, 'B did NOT receive g4-first (no channel yet) — pre-fix bug dropped silently without firing dial; post-fix fires the dial but this specific message still drops');
  // After the dial fired + completed, B should have g4-second.
  assert(/\[MSG  \] in  <[^>]+> g4-second/.test(bLog),
    'B DID receive g4-second after the dial completed (best-effort dial worked)');
}, 'best-effort VM-to-VM dial');

// G6: creator-solo send. Build a fresh group, A sends
// immediately as the only member. Pins down "一建群就
// 发消息不通" — the question is whether the sender's own
// chat.enc + history + publishMessage path works when
// m.Members == [selfHex]. If the in-memory publish fires,
// frontend should display the message instantly. We assert
// the log + the HistoryGroup subcommand renders the body.
at(18000, async () => {
  console.log('\n=== G6: creator-solo send ===');
  // Make a new group dedicated to this test so we don't
  // muddle with the 3-peer "去11" group above.
  send('A', 'group create g6test');
  await sleep(500);
  const aLogEarly = fs.readFileSync(path.join(ROOT, 'a.log'), 'utf8');
  const m = aLogEarly.match(/created\s+(g_[0-9a-f]{64})/g);
  // Take the LATEST created line (this run's G6 group).
  const gidLine = m ? m[m.length - 1] : null;
  if (!gidLine) {
    assert(false, 'G6: could not parse created group id');
    return;
  }
  const gid = gidLine.split(/\s+/)[1];
  // Solo send.
  send('A', `group send ${gid} g6-creator-solo-payload`);
  await sleep(800);
  send('A', `group history ${gid}`);
  await sleep(500);

  const aLog = fs.readFileSync(path.join(ROOT, 'a.log'), 'utf8');
  const sentLine = aLog.match(new RegExp(`\\[GROUP \\] sent to ${gid}: 0\\/0 delivered`));
  assert(sentLine !== null, 'G6: SendGroupMessage ran solo without erroring out');
  assert(/g6-creator-solo-payload/.test(aLog),
    'G6: sender-side body landed in own log (chat.enc written + publishMessage emitted)');
}, 'creator-solo send (no broadcast recipients)');

// G7: best-effort leave-group sync. Pre-fix bug (v1.1.2
// 2026-06-30): LeaveGroup removed self from the local
// members.json but never broadcast a roster update to
// remaining members. Result: A and C's `group show`
// stayed at 3 members forever even though B had clearly
// left. Post-fix: LeaveGroup calls broadcastRosterUpdate
// after the local Save, so ApplyRosterUpdate on A and C
// rebuilds their members.json without B and publishes
// GroupUpdated for the sidebar — counts drop to 2 / 2.
//
// Uses the original `createdGroupID` (the 3-peer "去11"
// group from G1/G2) so we have a populated roster to
// tear down.
at(22000, async () => {
  console.log('\n=== G7: B leaves the group, remaining peers sync ===');

  // Sanity baseline: all three should currently see 3.
  // (lastGroupShowOutput returns the LATEST line that
  // matches, so this is a defensive re-check before we
  // trigger the leave — surfaces ordering issues early.)
  send('A', `group show ${createdGroupID}`);
  send('B', `group show ${createdGroupID}`);
  send('C', `group show ${createdGroupID}`);
  await sleep(400);
  const pre = {
    A: lastGroupShowOutput('A', createdGroupID),
    B: lastGroupShowOutput('B', createdGroupID),
    C: lastGroupShowOutput('C', createdGroupID),
  };
  console.log(`  pre-leave members: A=${pre.A?.count} B=${pre.B?.count} C=${pre.C?.count}`);
  if (pre.A?.count !== 3 || pre.B?.count !== 3 || pre.C?.count !== 3) {
    assert(false, `G7 precondition: expected all 3 peers at 3 members before leave; ` +
      `got A=${pre.A?.count} B=${pre.B?.count} C=${pre.C?.count}. Earlier test may have torn state down.`);
    return;
  }

  // B leaves. cmdGroupLeave calls nd.LeaveGroup which now
  // (post-fix) fires broadcastRosterUpdate to A and C.
  send('B', `group leave ${createdGroupID}`);
  await sleep(2000);

  // Print fresh `group show` lines so lastGroupShowOutput
  // finds the post-leave ones (not the pre-leave ones).
  send('A', `group show ${createdGroupID}`);
  send('C', `group show ${createdGroupID}`);
  await sleep(500);

  const aPost = lastGroupShowOutput('A', createdGroupID);
  const cPost = lastGroupShowOutput('C', createdGroupID);
  console.log(`  post-leave members: A=${aPost?.count} C=${cPost?.count} (B should be gone)`);
  assert(aPost?.count === 2, `G7: A post-leave members = ${aPost?.count}, want 2. ` +
    `Pre-fix bug: A would stay at 3 because LeaveGroup never broadcast — sidebar ` +
    `stuck at "3 成员 · 3 在线" until restart.`);
  assert(cPost?.count === 2, `G7: C post-leave members = ${cPost?.count}, want 2. ` +
    `Pre-fix bug: C would stay at 3 for the same reason.`);

  // B should also show "left ..." in its log (cmdGroupLeave
  // echoes "[GROUP ] left <gid> (local cleanup done)") and
  // the new Node log line "left group=... (local cleanup
  // done, broadcasting roster to N remaining)" from the
  // post-fix LeaveGroup.
  const bLog = fs.readFileSync(path.join(ROOT, 'b.log'), 'utf8');
  assert(/\[GROUP \] left /g.test(bLog),
    'G7: B log shows "[GROUP ] left ..." from cmdGroupLeave');
  assert(/broadcasting roster to \d+ remaining/.test(bLog),
    'G7: B log shows post-fix LeaveGroup log line ("broadcasting roster to N remaining") — ' +
    'if this is missing, the broadcast was never wired into LeaveGroup');

  // And B should not see the group anymore (chat.enc deleted
  // → ListGroups filters it out). We use `group list` and
  // look for absence of the gid.
  send('B', 'group list');
  await sleep(400);
  const bLogAfter = fs.readFileSync(path.join(ROOT, 'b.log'), 'utf8');
  // Find the gid printed by the most recent list (cmdGroupList
  // prints one line per group). After leave there should be
  // at most 1 group (the g6test group from G6 — possibly).
  // The simplest assertion: the gid does NOT appear in the
  // last "GROUP list" section. We just check that B's log
  // doesn't have a fresh group-list line that mentions the
  // createdGroupID after the "left ..." line.
  const lines = bLogAfter.split(/\r?\n/);
  const leftIdx = lines.findIndex(l => /\[GROUP \] left /g.test(l));
  const linesAfterLeft = leftIdx >= 0 ? lines.slice(leftIdx) : [];
  const stillShows = linesAfterLeft.some(l =>
    l.includes(createdGroupID) && /group list/i.test(l) && /members=/.test(l));
  assert(!stillShows,
    'G7: B does not show left group anymore in its sidebar (chat.enc gone → ListGroups filters it out)');
}, 'G7: leave-group sync (multi-peer roster cleanup)');

// G8: after a member leaves, the remaining peer can keep
// sending and the leaver doesn't receive a ghost message.
// C sends `post-leave-payload` to the now-2-member group;
// A receives it (always has channel to C); B does NOT
// receive it (B deleted its local chat.enc and has no
// subscription anymore).
//
// Why this matters: a common bug shape is "removing self
// from the group but still receiving messages" because
// the per-peer dispatcher subscribes via chat.enc existence.
// Since B deleted chat.enc on leave, the broadcast loop's
// `n.channels.get(B)` would still find the existing TCP
// channel — verify the SendGroupMessage code path also
// gates by members.json (which B already lost), NOT by
// active TCP channel.
at(24500, async () => {
  console.log('\n=== G8: sender to post-leave group does not reach the leaver ===');
  send('C', `group send ${createdGroupID} g8-post-leave`);
  await sleep(1500);

  const aLog = fs.readFileSync(path.join(ROOT, 'a.log'), 'utf8');
  const bLog = fs.readFileSync(path.join(ROOT, 'b.log'), 'utf8');

  assert(/\[MSG  \] in  <[^>]+> g8-post-leave/.test(aLog),
    'G8: A DID receive g8-post-leave (A still a member)');
  assert(!/g8-post-leave/.test(bLog),
    'G8: B did NOT receive g8-post-leave (B left, no longer on roster)');
}, 'G8: leaver is dropped from the post-leave broadcast list');

// G9: solo creator self-dissolve. Pins down v1.1.2
// (2026-06-30) hotfix. Pre-fix bug: a creator who's the
// SOLE remaining member could NOT leave because
// pkg/group/members.go RemoveMember protects the
// creator from being removed. LeaveGroup errored with
// "RemoveMember returned false" and chat.enc was left
// in place. Post-fix: explicit solo-creator branch
// that calls deleteGroupDirsLocal (which removes BOTH
// chat.enc AND members.json) and publishes GroupRemoved.
//
// Use a fresh group dedicated to G9 so it doesn't share
// state with G7/G8 (which already torn down the original
// 3-peer group). G8 starts at T+24500 with ~1500ms of
// sleeps; scheduling G9 at T+26000 lets G8 finish first.
at(26000, async () => {
  console.log('\n=== G9: solo creator self-dissolve ===');
  // Spin up a brand-new group on A, no invites.
  send('A', 'group create g9-solo');
  await sleep(400);
  const aLogEarly = fs.readFileSync(path.join(ROOT, 'a.log'), 'utf8');
  const matches = aLogEarly.match(/created\s+(g_[0-9a-f]{64})/g);
  // Take the LAST created line (G9's group).
  if (!matches) {
    assert(false, 'G9: could not parse created group id');
    return;
  }
  const gid = matches[matches.length - 1].split(/\s+/)[1];
  console.log(`  G9 group id: ${gid}`);
  // Sanity: A has 1 member on this group.
  send('A', `group show ${gid}`);
  await sleep(300);
  const pre = lastGroupShowOutput('A', gid);
  assert(pre?.count === 1, `G9: pre-leave A's members = ${pre?.count}, want 1`);
  // A leaves. Before the fix this errored with
  // "RemoveMember returned false" and left chat.enc.
  send('A', `group leave ${gid}`);
  await sleep(800);
  // After the fix: members.json + chat.enc both gone, so
  // `group list` filters this gid out (chat.enc gone).
  send('A', 'group list');
  await sleep(400);
  const aLog = fs.readFileSync(path.join(ROOT, 'a.log'), 'utf8');
  assert(/solo creator self-dissolved/.test(aLog),
    'G9: A log shows "solo creator self-dissolved" — post-fix branch wired in');
  // The group should be invisible to ListGroups now (chat.enc gone).
  // lastGroupShowOutput scans for a "members=N" line; if gid is no
  // longer on disk we should NOT find one for gid after the leave.
  const linesAfterLeave = aLog.split(/\r?\n/);
  const leaveIdx = linesAfterLeave.findIndex(l => /solo creator self-dissolved/.test(l));
  const after = leaveIdx >= 0 ? linesAfterLeave.slice(leaveIdx) : [];
  const stillShows = after.some(l => l.includes(gid) && /members=\d+/.test(l));
  assert(!stillShows,
    'G9: gid no longer surfaces in A\'s `group show` (chat.enc gone → ListGroups filters it)');
}, 'G9: solo creator self-dissolve (v1.1.2 LeaveGroup hotfix)');

at(28000, async () => {
  console.log('\n=== FINAL ===');
  if (failures > 0) {
    console.log(`\u274C ${failures} assertion(s) FAILED`);
    process.exit(1);
  }
  console.log('\u2705 all group-sync assertions passed');
  for (const { inst, proc } of procs) {
    try { send(inst.id, 'quit'); } catch (_) {}
  }
  await sleep(1500);
  for (const { inst, proc } of procs) {
    try { proc.kill('SIGTERM'); } catch (_) {}
  }
  process.exit(0);
}, 'quit');

// ========== RUN ==========

startAll();

process.on('SIGINT', () => {
  console.log('\nSIGINT — quitting');
  for (const { inst, proc } of procs) {
    try { send(inst.id, 'quit'); } catch (_) {}
    try { proc.kill('SIGTERM'); } catch (_) {}
  }
  setTimeout(() => process.exit(0), 1000);
});

(async () => {
  const start = Date.now();
  for (const ev of TIMELINE) {
    const wait = (start + ev.atMs) - Date.now();
    if (wait > 0) await sleep(wait);
    console.log(`\n[T+${ev.atMs}ms] ${ev.note}`);
    try {
      await ev.action();
    } catch (e) {
      console.error(`  action error: ${e.message}`);
    }
  }
})().catch(e => { console.error(e); process.exit(1); });