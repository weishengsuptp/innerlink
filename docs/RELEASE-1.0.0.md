# innerlink v1.0.0 发布说明

> 2026-06-24 — Wails v2 单仓，国密 P2P 局域网聊天，干净收尾。

## 这版本是什么

把历史 `innerlink-core`（纯 Go 协议层，M1-M4）的代码 + 历史 `innerlink-desktop`
（Wails v2 + 13 ahead commit 一直 fix 没修完的 close bug）的 UI，**合并成一个干净
的单仓** `github.com/weishengsuptp/innerlink`。

模块路径：`github.com/weishengsuptp/innerlink`。

## 仓结构

```
D:\innerlink\
├── main.go                # Wails 入口 (package main)，embed frontend/dist
├── app/app.go             # Wails 桥接层（Startup / Shutdown / bound methods）
├── frontend/              # vanilla TypeScript，零框架
├── cmd/innerlink/         # CLI REPL，同协议层不用 GUI
├── pkg/node/              # 协议层公共 API
├── internal/              # 12 个内部包
├── docs/                  # PRD / ARCHITECTURE / 本文件
├── .github/workflows/     # 三平台 CI
└── frontend/dist/         # Wails embed 的 frontend 产物
```

## 关键决策（按时间顺序）

| # | 决策 | 为什么 |
|---|---|---|
| 1 | 合并 core + desktop 到单仓 | innerlink-desktop 13 个 ahead commit 全是 close bug 失败尝试，单仓能复用 code 又能干净收尾 |
| 2 | Wails v2，不用 v3 | v3 仍 ALPHA，alpha.5x，桌面三端基本可用但 iOS/Android experimental |
| 3 | 协议层从 innerlink-core 复制过来，不重写 | M1-M4 已经测过 + 写完，重写浪费时间 + 可能引入新 bug |
| 4 | 复用 innerlink-desktop 的 UI | 那个 v0.1 mockup 经过 review，结构 OK |
| 5 | CI 跑 wails build，不裸 go build | `wails build` 是 Wails 唯一支持的 build 方式（带 `-tags wails` + Windows GUI ldflags + frontend embed）|
| 6 | CI pin ubuntu-22.04 / macos-14 | 24.04 noble 没 webkit2gtk-4.0，macos-26 dyld 严格 LC_UUID 让 Wails 2.12 CLI 起不来 |
| 7 | GitHub 仓替换 Rust 历史（手动 web 删 + 重建）| 旧仓只是占位 + 跟 innerlink 仓同名，统一用 innerlink |
| 8 | **不加 OnBeforeClose / Job Object / 强制 os.Exit** 等 workaround | 13 个版本翻车教训：workaround 只藏症状不修根因 |

## close bug 根因 + 修复

### 症状

关 X 之后 process 残留，不退。Task Manager 看 `innerlink.exe` 一直在。

### 之前 13 个版本怎么"修"的（全错）

| 版本 | 尝试 | 结果 |
|---|---|---|
| v0.1.5 | Job Object 强制 kill 进程树 | dev VM 过，物理机挂 |
| v0.1.6 | OnShutdown runtime.Quit + os.Exit | OnShutdown 不被调，os.Exit 不跑 |
| v0.1.7 | OnBeforeClose runtime.Quit | 同上 |
| v0.1.8 | pumpCancel + watchdog IsWindowVisible poll | 物理机 hwnd 一直 visible=1，不 trip |
| v0.1.9 | IsWindow poll | 永远返 1 |
| v0.1.10 | SetWinEventHook EVENT_OBJECT_HIDE/DESTROY | Wails 用别方式 hide，OS event 不触发 |
| v0.1.11 | OnBeforeClose 同步 nd.Close | main thread block 死锁 |
| v0.1.12 | OnBeforeClose 异步 go nd.Close + watchdog | 还是挂 |
| v0.1.13 | 删 OnBeforeClose | 看着过，实际没退（VM 测试 race）|

13 次全在 UI 层 / Wails quit chain / OS event 加补丁。**没人去看 goroutine dump**。

### 这次怎么修对的

1. **加 log 标记生命周期**：`[INFO app] Startup/Shutdown/pumpPeers/pumpMessages ENTER/EXIT` + 5s 超时 + 自动 `runtime.Stack(buf, true)` dump
2. **Wails Logger + LogLevel: DEBUG**：`Logger: logger.NewDefaultLogger()` 让 Wails 自身 quit chain log 出来
3. **看 goroutine dump**：4 个 goroutine 卡住，其中 `goroutine 24 [chan receive]` 在 `node.go:264` `for ev := range n.ann.Events()`
4. **查 `Announcer.Run`**：发现 `defer conn.Close()` 时**没关 events channel**——producer 漏 close，consumer 永远 range

### 修复代码

`internal/discovery/discovery.go` 净 +40 行：

- Announcer 加 `eventsMu sync.Mutex` + `eventsClosed bool`
- `emit` 持锁 + check `eventsClosed`，closed 后静默退出（避免 send-on-closed panic）
- 新增 `closeEvents()` 持锁 + close channel（idempotent）
- `Run` 在 `ctx.Done` 分支：等 `gcDone` + `readErr` 都 done 后调 `closeEvents()`（这时所有内部 emit goroutine 都退了，不 race）

### 验证

```
[INFO  app] BeforeClose ENTER (goroutines=14, nd=true)
[INFO  app] Shutdown ENTER (goroutines=14, nd=true)
[INFO  app] pumpMessages EXIT (goroutines=14)
[INFO  app] pumpPeers EXIT (goroutines=14)
[INFO  app] nd.Close returned in 2.4378s (goroutines=6)
[INFO  app] Shutdown RETURN (goroutines=4)
Process EXITED ✓
```

WM_CLOSE → process 退：约 3s（主要 2.4s 在等 announcer 内部 goroutine 退）。

## 当前仓库状态

```
[main] 5 commits:
  673771e fix(close-exit): close Announcer events channel on Run exit
  99055ee ci: test job install wails CLI + wails generate module
  fee84d0 chore: keep frontend/dist/.gitkeep for go:embed on fresh clone
  6ee73b6 ci: test job npm build + frontend/dist/.gitkeep
  accb1a8 ci: 三平台 build + test workflow
  ba3815b frontend: 复用 innerlink-desktop v0.1 UI 结构
  b6ba5c3 frontend: vanilla TS 3-pane shell
  14a8e36 v1.0.0: Wails v2 单仓, 复用 innerlink-core 协议层
```

GitHub: https://github.com/weishengsuptp/innerlink

CI：3 平台 6 jobs 全 green（run 28061501296 @ 673771e）。

Artifacts：linux 3.4 MB / windows 4.3 MB / macos 6.6 MB。

## 教训（三条）

1. **遇到 bug 第一反应是查自己代码，不是加 workaround**。Workaround 只藏症状不修根因，换机器换版本就复发。
2. **Process hang 三件套**：log 标记生命周期 + Wails `LogLevel: DEBUG` 抓 stderr + 5s 超时后 `runtime.Stack` dump goroutine。顺着 dump 里的 `[chan receive]` 找 producer 的 close / select ctx.Done。
3. **Wails v2 在 Windows 上 `os.Chdir` 到 `%APPDATA%\Roaming\<bin>\`**。`os.Getwd()` 拿到的不是 binary 旁边，是 app data。data 落盘路径要对，否则你以为"程序不写日志"实际写到别处。

## 还能做但这次没做

- [ ] Release workflow（push tag → 自动出 3 平台 binary + 上传 GitHub Release）
- [ ] CHANGELOG 维护（每次 release 标 version + 改了什么）
- [ ] 两 peer 端到端 e2e 测试（需要两台机器 / 虚拟机）
- [ ] `app.go` 里之前加的诊断 log 留着还是删（DEBUG 级别 log 噪音）—— 倾向留，给未来调试用
- [ ] Wails v3 GA 后迁（v2→v3 大约 2-4 天，只改 main.go + app.go + wails.json）

## Backup 留档

- `D:\innerlink-bak\` — innerlink-core Go 仓完整备份（含 .git, be30206）
- `D:\innerlink-desktop\` — 旧 Wails 仓 + 13 ahead commit 历史
- `D:\mavis-tmp\design-innerlink-v1.md` — v1.0.0 design doc
