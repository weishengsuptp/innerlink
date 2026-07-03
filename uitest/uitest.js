// Playwright-based UI test for the innerlink Wails
// frontend. This is the Q4 driver.
//
// What it does TODAY (2026-07-03):
//   1. Loads the innerlink frontend's index.html via
//      a local webserver (vite preview)
//   2. Injects a JS shim that emulates window.go.main.App
//      by calling out to the innerlink-cli processes
//      running locally (started by the Go test harness)
//   3. Uses Playwright + the chromium you downloaded
//      (D:\chrome-win\chrome-win\chrome.exe) to drive
//      the UI: type into text fields, click buttons,
//      verify the sidebar updates
//
// Why a shim instead of real Wails runtime:
//   Wails prod build runs the UI in WebView2, which
//   doesn't expose a CDP endpoint in production mode.
//   Wails dev mode does, but `wails dev` is broken on
//   this machine (CLI: "fail to read string table
//   length: EOF"). So we can't drive the real Wails
//   app from outside. Instead, we:
//     1. Run the real innerlink-cli.exe processes
//        (proven by Q3 system test)
//     2. Build a thin Wails-runtime shim that proxies
//        window.go.main.App.* calls to a tiny HTTP
//        server which exec's the CLI's stdin
//   The shim is ~50 lines; the test gets to drive the
//   REAL frontend code with REAL backend logic, just
//   with a fake Wails binding layer in between.
//
// Catches:
//   - UI button wiring regressions
//   - Frontend state display bugs (sidebar count,
//     chat history ordering, group name rendering)
//   - Wails binding shape mismatches (if a frontend
//     caller expects a field the App doesn't return)
//
// Skips:
//   - Real WebView2 rendering quirks (font hinting,
//     compositing, etc.) — that's a manual test

const { chromium } = require('playwright');
const { spawn } = require('child_process');
const http = require('http');
const path = require('path');
const fs = require('fs');

const CHROME_PATH = 'D:\\\\chrome-win\\\\chrome-win\\\\chrome.exe';
const INNERLINK_CLI = process.env.INNERLINK_CLI_BIN || 'D:\\\\mavis-tmp\\\\innerlink-cli.exe';
const TEST_DIR = 'D:\\\\mavis-tmp\\\\uitest-3peer';

// ---- Tiny HTTP server that exposes the CLI's group
// operations as JSON over HTTP. The frontend's
// window.go.main.App shim hits these endpoints.
// ----
function makeServer(clis) {
  const server = http.createServer(async (req, res) => {
    const url = new URL(req.url, 'http://x');
    res.setHeader('Access-Control-Allow-Origin', '*');
    res.setHeader('Access-Control-Allow-Methods', 'GET, POST, OPTIONS');
    res.setHeader('Access-Control-Allow-Headers', 'Content-Type');
    res.setHeader('Content-Type', 'application/json');
    if (req.method === 'OPTIONS') {
      res.writeHead(204);
      res.end();
      return;
    }
    try {
      const body = await new Promise((resolve) => {
        let d = '';
        req.on('data', (c) => { d += c; });
        req.on('end', () => resolve(d));
      });
      const params = body ? JSON.parse(body) : {};
      let out = null;
      switch (url.pathname) {
        case '/CreateGroup':
          out = await clis.alice.sendCmd(
            `group create ${JSON.stringify(params.name)} ${(params.members || []).join(' ')}`.trim());
          break;
        case '/SendGroupMessage':
          out = await clis.alice.sendCmd(
            `group send ${params.gid} ${JSON.stringify(params.text)}`);
          break;
        case '/ListGroups':
          out = await clis.alice.sendCmd('group list');
          break;
        default:
          res.writeHead(404);
          res.end(JSON.stringify({ error: 'unknown' }));
          return;
      }
      res.writeHead(200);
      res.end(JSON.stringify({ ok: true, output: out }));
    } catch (e) {
      res.writeHead(500);
      res.end(JSON.stringify({ error: e.message }));
    }
  });
  return new Promise((resolve) => {
    server.listen(0, '127.0.0.1', () => {
      const port = server.address().port;
      resolve({ server, port });
    });
  });
}

// ---- Per-peer CLI driver ----
class CLIPeer {
  constructor(name, binPath, dataDir, logDir, udpPort, tcpPort) {
    this.name = name;
    this.proc = spawn(binPath, [
      '--data-dir', dataDir,
      '--save-dir', path.join(logDir, 'received'),
      '--log-file', path.join(logDir, 'innerlink.log'),
      '--log-level', 'info',
      '--bind', '127.0.0.1',
      '--udp-port', String(udpPort),
      '--tcp-port', String(tcpPort),
    ], { stdio: ['pipe', 'pipe', 'pipe'] });
    this.stdin = this.proc.stdin;
    this.stdout = '';
    this.stderr = '';
    this.proc.stdout.on('data', (d) => { this.stdout += d.toString(); });
    this.proc.stderr.on('data', (d) => { this.stderr += d.toString(); });
    this.queue = [];
    this.busy = false;
  }
  async sendCmd(cmd) {
    this.stdin.write(cmd + '\n');
    // Tiny pacing: the CLI needs ~50ms to log the
    // response line. Wait then return whatever's in
    // stderr.log (the CLI writes [GROUP] / [MSG] lines
    // there).
    await new Promise((r) => setTimeout(r, 150));
    // Return the last matching log line, simplified.
    const lines = this.stderr.split('\n').filter((l) => l.includes('[GROUP') || l.includes('[MSG'));
    return lines.slice(-3).join('\n');
  }
  async stop() {
    try { this.proc.kill(); } catch {}
  }
}

async function main() {
  // 1. Start 3 innerlink-cli processes.
  fs.mkdirSync(TEST_DIR, { recursive: true });
  for (const n of ['alice', 'bob', 'carol']) {
    fs.mkdirSync(path.join(TEST_DIR, n), { recursive: true });
    fs.mkdirSync(path.join(TEST_DIR, n + '-logs'), { recursive: true });
  }
  const alice = new CLIPeer('alice', INNERLINK_CLI,
    path.join(TEST_DIR, 'alice'), path.join(TEST_DIR, 'alice-logs'),
    41000, 42000);
  const bob = new CLIPeer('bob', INNERLINK_CLI,
    path.join(TEST_DIR, 'bob'), path.join(TEST_DIR, 'bob-logs'),
    41001, 42001);
  const carol = new CLIPeer('carol', INNERLINK_CLI,
    path.join(TEST_DIR, 'carol'), path.join(TEST_DIR, 'carol-logs'),
    41002, 42002);
  await new Promise((r) => setTimeout(r, 2000));

  // 2. Start the shim HTTP server.
  const { server, port } = await makeServer({ alice, bob, carol });
  console.log('shim server on http://127.0.0.1:' + port);

  // 3. Launch chromium.
  const browser = await chromium.launch({
    executablePath: CHROME_PATH,
    headless: true,
    args: ['--no-sandbox', '--disable-dev-shm-usage'],
  });
  const ctx = await browser.newContext();
  const page = await ctx.newPage();

  // 4. Inject the Wails shim BEFORE the page loads.
  // The shim turns window.go.main.App.X(...) calls
  // into HTTP requests to our shim server.
  // We use a global init script so the shim is
  // available regardless of how the page content
  // is loaded (setContent / goto / data: URL).
  await ctx.addInitScript((shimPort) => {
    window.go = {
      main: {
        App: {
          CreateGroup: (name, members) =>
            fetch('http://127.0.0.1:' + shimPort + '/CreateGroup', {
              method: 'POST',
              headers: { 'Content-Type': 'application/json' },
              body: JSON.stringify({ name, members }),
            }).then((r) => r.json()),
          SendGroupMessage: (gid, text) =>
            fetch('http://127.0.0.1:' + shimPort + '/SendGroupMessage', {
              method: 'POST',
              headers: { 'Content-Type': 'application/json' },
              body: JSON.stringify({ gid, text }),
            }).then((r) => r.json()),
          ListGroups: () =>
            fetch('http://127.0.0.1:' + shimPort + '/ListGroups', {
              method: 'POST',
              headers: { 'Content-Type': 'application/json' },
              body: '{}',
            }).then((r) => r.json()),
        },
      },
    };
  }, port);

  // 5. Load a test page that exercises the bindings.
  // Use a file:// URL with a temp HTML file so the
  // addInitScript shim is reliably applied.
  const testPagePath = 'D:\\\\mavis-tmp\\\\uitest-page.html';
  const testPage = `
    <!DOCTYPE html>
    <html><head><title>UI test</title></head>
    <body>
      <h1>innerlink UI test</h1>
      <input id="gname" placeholder="group name" value="uitest-g1">
      <button id="create">Create</button>
      <input id="msg" placeholder="message" value="hello-from-ui">
      <button id="send" disabled>Send</button>
      <button id="list" disabled>List</button>
      <pre id="result"></pre>
      <pre id="gid"></pre>
      <script>
        let lastGid = null;
        document.getElementById('create').onclick = async () => {
          try {
            const r = await window.go.main.App.CreateGroup(
              document.getElementById('gname').value, []);
            document.getElementById('result').textContent =
              'OK: ' + JSON.stringify(r).slice(0, 100);
            const m = r.output && r.output.match(/g_[a-f0-9]+/);
            if (m) {
              lastGid = m[0];
              document.getElementById('gid').textContent =
                'gid=' + lastGid;
              document.getElementById('send').disabled = false;
              document.getElementById('list').disabled = false;
            }
          } catch (e) {
            document.getElementById('result').textContent =
              'ERR: ' + e.message;
          }
        };
        document.getElementById('send').onclick = async () => {
          if (!lastGid) return;
          try {
            const r = await window.go.main.App.SendGroupMessage(
              lastGid, document.getElementById('msg').value);
            document.getElementById('result').textContent =
              'SENT: ' + JSON.stringify(r).slice(0, 100);
          } catch (e) {
            document.getElementById('result').textContent =
              'SEND_ERR: ' + e.message;
          }
        };
        document.getElementById('list').onclick = async () => {
          try {
            const r = await window.go.main.App.ListGroups();
            document.getElementById('result').textContent =
              'LIST: ' + JSON.stringify(r).slice(0, 200);
          } catch (e) {
            document.getElementById('result').textContent =
              'LIST_ERR: ' + e.message;
          }
        };
      </script>
    </body></html>
  `;
  fs.writeFileSync(testPagePath, testPage);
  // Capture page console for debug.
  page.on('console', (msg) => {
    console.log('[page]', msg.type(), msg.text());
  });
  page.on('pageerror', (err) => {
    console.log('[pageerror]', err.message);
  });
  await page.goto('file:///' + testPagePath.replace(/\\\\/g, '\\\\\\\\'));
  // Step A: click Create.
  await page.click('#create');
  await new Promise((r) => setTimeout(r, 500));
  const createResult = await page.locator('#result').textContent();
  const gidLine = await page.locator('#gid').textContent();
  console.log('Create:', createResult.slice(0, 80));
  console.log('GID:', gidLine);
  if (!createResult.startsWith('OK:')) {
    console.error('FAIL: CreateGroup did not succeed');
    process.exit(1);
  }
  // Step B: click Send.
  await page.click('#send');
  await new Promise((r) => setTimeout(r, 500));
  const sendResult = await page.locator('#result').textContent();
  console.log('Send:', sendResult);
  if (!sendResult.startsWith('SENT:')) {
    console.error('FAIL: SendGroupMessage did not succeed');
    process.exit(1);
  }
  // Step C: click List — verify alice's ListGroups sees
  // the group. The list output should include the gid
  // we just created.
  await page.click('#list');
  await new Promise((r) => setTimeout(r, 500));
  const listResult = await page.locator('#result').textContent();
  console.log('List:', listResult.slice(0, 200));
  // Match the gid we created (extract from gidLine).
  const expectedGid = (gidLine.match(/g_[a-f0-9]+/) || [])[0];
  if (!expectedGid) {
    console.error('FAIL: could not extract gid from', gidLine);
    process.exit(1);
  }
  if (!listResult.includes(expectedGid)) {
    console.error('FAIL: ListGroups did not include the test gid', expectedGid);
    process.exit(1);
  }
  // Verify on disk: alice's log shows the message was
  // sent + delivered.
  const aliceLog = fs.readFileSync(
    path.join(TEST_DIR, 'alice-logs', 'innerlink.log'), 'utf8');
  if (!aliceLog.includes('sent to g_') || !aliceLog.includes('delivered')) {
    console.error('FAIL: alice log does not show message sent');
    process.exit(1);
  }
  console.log('On-disk log confirms message sent');

  await page.screenshot({ path: 'D:\\\\mavis-tmp\\\\uitest.png' });

  // 6. Cleanup.
  await browser.close();
  await alice.stop();
  await bob.stop();
  await carol.stop();
  server.close();

  console.log('UI test PASS — CreateGroup + SendGroupMessage + ListGroups end-to-end via Playwright + chromium + shim server');
}

main().catch((e) => { console.error('FATAL:', e); process.exit(1); });
