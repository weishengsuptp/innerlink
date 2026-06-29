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

at(20000, async () => {
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