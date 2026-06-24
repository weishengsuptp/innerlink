# innerlink tests/e2e

End-to-end CLI multi-instance test runner. The
canonical regression check for the alias + dedup
features (and any cross-instance semantics).

## What it does

Spawns two `innerlink` CLI processes (A and B) on
loopback with different ports + data dirs, drives
them via stdin REPL commands, captures per-instance
log files, and asserts the expected outcome of each
scenario.

The CLI binary is built and lives outside the repo:
`D:\mavis-tmp\test\innerlink.exe`. Edit the `BIN`
constant in `test-e2e.js` if your checkout is
elsewhere.

## Scenarios

S1 — mutual discovery via direct `dial` (UDP broadcast
     doesn't work on loopback; real LAN use is similar
     but uses mDNS).

S2 — A sets `myalias 老板`, B must see "老板" within
     ~5s of RosterSync.

S3 — A changes alias to "新名", B sees update.

S4 — A clears alias (`myalias clear`), B falls back to
     hostname.

S5 — A kills, B logs `channel closed`.

S6 — A restarts (same device.key), B dials again, A
     re-appears with same peerID.

S7 — A regenerates `device.key` (new peerID, same
     hostname+IP). B's roster must:
       a) contain A's new entry with the latest alias
       b) mark A's OLD peerID as `reset=true` (one-shot,
          sticky)
       c) hide the old entry from `cmdPeers` output
          (i.e. ListActive filters it out)

This is the dedup regression test. Without the fix
in `roster.Store.Add()` (which runs the (hostname,
IP-overlap) scan at every entry insertion, not only
in MergeFromGossip), B's `cmdPeers` would still show
the old A peerID as "unnamed, last seen <old timestamp>".

## Run

```
go build -o D:\mavis-tmp\test\innerlink.exe ./cmd/innerlink
node D:\innerlink\tests\e2e\test-e2e.js
```

Exit code 0 = all assertions passed.
Exit code 1 = at least one assertion failed; the failing
assertion messages are printed to stdout.

## Why not in `go test`?

The e2e test needs:
  * two child processes with pipes (Node's `child_process`
    is the simplest)
  * text-based assertions on log file output
  * a per-scenario timeline scheduler (kinda like cron)

We have a `internal/filetransfer/dispatcher_e2e_test.go`
that does in-process two-Node tests. That covers transport-
layer flows. This `tests/e2e` is the companion that covers
the **whole stack** (mDNS discovery, handshake, RosterSync,
alias broadcast, dedup reset, persistence).

## Adding a scenario

Edit `test-e2e.js`. Each entry in the `TIMELINE` array
is `{ atMs, action, note }`. Add an entry, restart the
script, watch stdout for your scenario's `[OK]` /
`[FAIL]` lines.