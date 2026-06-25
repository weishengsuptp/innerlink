// innerlink e2e test orchestrator (v2)
//
// Spawns 2 innerlink CLI instances, drives them via
// REPL on stdin, captures logs. Tests:
//
//  S1: manual dial + mutual discovery
//  S2: A sets alias "老板" via `myalias` command
//      → verify B sees "老板" on next roster sync
//  S3: A changes alias to "新名" via `myalias`
//      → verify B sees update
//  S4: A clears alias (myalias clear)
//      → verify B falls back to hostname
//  S5: A kills → B should mark A offline / drop
//  S6: A restarts → B should re-discover
//  S7: device-key regenerate (rebuild A's roster.json
//      from scratch with new key) → verify A's old
//      self-entry gets marked reset by dedup scan
//
// Usage:
//   go build -o <BIN> ./cmd/innerlink
//   BIN=<BIN> node tests/e2e/test-e2e.js
//   echo $?  # 0 = pass, 1 = fail

const { spawn } = require('node:child_process');
const fs = require('node:fs');
const path = require('node:path');

const BIN = process.env.BIN || 'D:\\mavis-tmp\\test\\innerlink.exe';
const ROOT = process.env.ROOT || 'D:\\mavis-tmp\\test';

// Wipe previous state so each run starts clean.
for (const sub of ['a', 'b']) {
  const dir = path.join(ROOT, sub);
  if (fs.existsSync(dir)) {
    for (const f of fs.readdirSync(dir)) {
      try { fs.unlinkSync(path.join(dir, f)); } catch (_) {}
    }
  }
}

const instances = [
  { id: 'A', data: path.join(ROOT, 'a'), udp: 4747, tcp: 4748 },
  { id: 'B', data: path.join(ROOT, 'b'), udp:14747, tcp:14748 },
];

let procs = [];
const cleanups = [];

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
    console.log(`  ❌ ASSERT FAIL: ${msg}`);
    failures++;
    return false;
  }
  console.log(`  ✅ ${msg}`);
  return true;
}

// Read B's log and return the last "N known peer(s)"
// line + the peer lines that follow.
function lastPeersOutput(instId) {
  const f = path.join(ROOT, `${instId.toLowerCase()}.log`);
  const lines = fs.readFileSync(f, 'utf8').split(/\r?\n/);
  // Find the last "PEERS" line containing "known peer".
  // Log lines have a timestamp prefix like "2026/06/25 07:18:32.474410 "
  // before [PEERS], so the regex must skip the prefix.
  for (let i = lines.length - 1; i >= 0; i--) {
    if (/known peer\(s\)/.test(lines[i])) {
      const countMatch = lines[i].match(/(\d+) known peer/);
      const peers = [];
      for (let j = i + 1; j < lines.length; j++) {
        const m = lines[j].match(/\[PEERS\] (.+?) +last seen +(\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}) +\((\w+)\)/);
        if (!m) break;
        peers.push({ name: m[1].trim(), lastSeen: m[2], peerID: m[3] });
      }
      return { count: parseInt(countMatch[1]), peers };
    }
  }
  return { count: 0, peers: [] };
}

const TIMELINE = [];
function at(ms, action, note) { TIMELINE.push({ atMs: ms, action, note }); }

// ========== SCENARIOS ==========

// S1: mutual discovery via direct dial
at(2000, async () => {
  console.log('\n=== S1: B dials A ===');
  send('B', 'dial 127.0.0.1:4748');
  await sleep(2500);
  send('A', 'peers');
  send('B', 'peers');
}, 'dial');

// S2: A sets alias "老板"
at(6000, async () => {
  console.log('\n=== S2: A myalias 老板 ===');
  send('A', 'myalias 老板');
  await sleep(1500);
  send('B', 'peers');
  await sleep(500);
  const r = lastPeersOutput('B');
  console.log(`  B last peers: count=${r.count} ${JSON.stringify(r.peers)}`);
  assert(r.count === 1, 'B sees exactly 1 peer');
  assert(r.peers.some(p => p.name === '老板'), 'B sees alias "老板" for A');
  // Verify the peer:event signal fired (the bug fix
  // for "A changed alias but B's list didn't update").
  // The CLI doesn't drain peerEventCh (no frontend), but
  // the alias-update log line is emitted by the same code
  // path so we can assert it appears.
  const bLog = fs.readFileSync(path.join(ROOT, 'b.log'), 'utf8');
  assert(/alias updated for/.test(bLog),
    'B logs alias update (UI would refresh via peer:event)');
}, 'A: myalias 老板 + assert');

// S3: A changes alias to "新名"
at(9000, async () => {
  console.log('\n=== S3: A myalias 新名 ===');
  send('A', 'myalias 新名');
  await sleep(1500);
  send('B', 'peers');
  await sleep(500);
  const r = lastPeersOutput('B');
  console.log(`  B last peers: count=${r.count} ${JSON.stringify(r.peers)}`);
  assert(r.count === 1, 'B sees exactly 1 peer');
  assert(r.peers.some(p => p.name === '新名'), 'B sees alias "新名" for A');
}, 'A: myalias 新名 + assert');

// S4: A clears alias
at(12000, async () => {
  console.log('\n=== S4: A myalias clear ===');
  send('A', 'myalias clear');
  await sleep(1500);
  send('B', 'peers');
  await sleep(500);
  const r = lastPeersOutput('B');
  console.log(`  B last peers: count=${r.count} ${JSON.stringify(r.peers)}`);
  assert(r.count === 1, 'B sees exactly 1 peer');
  // After clear, B should fall back to hostname.
  assert(r.peers.some(p => p.name === '<test-host>'),
    'B sees hostname fallback (alias cleared)');
}, 'A: myalias clear + assert');

// S5: kill A, verify B marks offline
at(15000, async () => {
  console.log('\n=== S5: kill A ===');
  const recA = procs.find(p => p.inst.id === 'A');
  recA.proc.kill('SIGTERM');
  await sleep(2000);
  send('B', 'peers');
  await sleep(500);
  const bLog = fs.readFileSync(path.join(ROOT, 'b.log'), 'utf8');
  assert(/channel closed peer=/.test(bLog),
    'B logs channel closed for A');
}, 'kill A + assert');

// S6: restart A, verify B sees A back online
at(19000, async () => {
  console.log('\n=== S6: restart A ===');
  const recA = procs.find(p => p.inst.id === 'A');
  recA.proc = spawnInst(instances[0]);
  await sleep(3500);
  send('B', 'dial 127.0.0.1:4748');
  await sleep(2500);
  send('B', 'peers');
  await sleep(500);
  const r = lastPeersOutput('B');
  console.log(`  B last peers: count=${r.count} ${JSON.stringify(r.peers)}`);
  assert(r.count === 1, 'B sees exactly 1 peer after A restart');
}, 'restart A + assert');

// S7: device-key regenerate — A's roster.json will get
// a new peerID for self. Any old self-entry with same
// hostname+IP should get marked reset by the dedup scan.
at(26000, async () => {
  console.log('\n=== S7: A regenerates device.key ===');
  const aLog = path.join(ROOT, 'a.log');
  const logTxt = fs.readFileSync(aLog, 'utf8');
  const m = logTxt.match(/peerID=([0-9a-f]{32})/g);
  const oldID = m ? m[m.length-1].split('=')[1] : null;
  console.log(`A's current peerID (before regen): ${oldID}`);

  // Stop A
  const recA = procs.find(p => p.inst.id === 'A');
  recA.proc.kill('SIGTERM');
  await sleep(1500);

  for (const f of ['device.key', 'roster.json']) {
    try { fs.unlinkSync(path.join(ROOT, 'a', f)); } catch (_) {}
  }
  fs.writeFileSync(path.join(ROOT, 'a', 'alias.txt'), '新我');

  recA.proc = spawnInst(instances[0]);
  await sleep(3000);

  send('B', 'dial 127.0.0.1:4748');
  await sleep(3000);
  send('B', 'peers');
  await sleep(500);
  const r = lastPeersOutput('B');
  console.log(`  B last peers: count=${r.count} ${JSON.stringify(r.peers)}`);
  assert(r.count === 1, 'B sees exactly 1 peer after device-key regen');
  assert(r.peers.some(p => p.name === '新我'),
    'B sees A-new alias "新我"');
  assert(r.peers.every(p => p.peerID !== oldID),
    `B does NOT see A-old peerID ${oldID} (ghost dedup worked)`);
}, 'regen A device.key + assert no ghost');

at(32000, async () => {
  console.log('\n=== FINAL ===');
  if (failures > 0) {
    console.log(`❌ ${failures} assertion(s) FAILED`);
    process.exit(1);
  }
  console.log('✅ all assertions passed');
  for (const { inst, proc } of procs) {
    try { send(inst.id, 'quit'); } catch (_) {}
  }
  await sleep(2000);
  for (const { inst, proc } of procs) {
    try { proc.kill('SIGTERM'); } catch (_) {}
  }
  for (const fn of cleanups) try { fn(); } catch (_) {}
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
  for (const fn of cleanups) try { fn(); } catch (_) {}
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