# AGENTS.md

> 这份文件是给接手 innerlink 项目的 AI agent（或未来的自己）看的项目规约。
> 跟代码同步维护；改业务逻辑前先看这里。

## 项目定位

**innerlink** 是单二进制、国密 (SM2/SM3/SM4) 端到端加密的 LAN P2P 通信 app。
同 WiFi / 同 NAT 局域网两台机器之间，无账号、无中心服务器，直接互发加密消息和文件。

**技术栈**：Wails v2 (Go + WebView) + 国密 `tjfoc/gmsm`。
**协议层**：纯 Go 标准库 + UDP/TCP socket，国密四件套在 `internal/crypto/`。
**前端**：vanilla TypeScript，零框架（刻意保持简单）。

**部署形态**：单进程 / 单二进制 / GUI + CLI 二合一（GUI 是默认壳，CLI 是同协议层的 REPL demo）。

## 仓库结构

```
D:\innerlink\
├── main.go                # Wails 入口 (package main)，embed frontend/dist
├── go.mod                 # module github.com/weishengsuptp/innerlink
├── wails.json             # Wails 项目配置
├── app/
│   └── app.go             # Wails App (package app)，绑给 JS 的桥接层
├── frontend/              # vanilla TS 前端（src/, dist/, wailsjs/, package.json）
├── cmd/innerlink/         # CLI 入口（main.go + repl.go，同协议层不同壳）
├── pkg/node/              # 协议层公共 API（New/Start/Close/SendText/...）
├── internal/              # 协议层内部包（不导出给库外）
│   ├── alias/             # 别名表（持久化 JSON）
│   ├── crypto/            # 国密 SM2/SM3/SM4 包装（基于 tjfoc/gmsm）
│   ├── discovery/         # UDP peer discovery + 主动扫描
│   ├── filetransfer/      # 加密文件传输（SM4-CTR + SHA-256 校验）
│   ├── handshake/         # SM2 ECDH + SM4 会话密钥协商
│   ├── identity/          # 设备 SM2 长期密钥对（device.key）
│   ├── logx/              # 文件日志 + 等级（debug/info/warn/error）
│   ├── paths/             # 跨平台路径布局（DataDir / SaveDir / LogFile）
│   ├── protocol/          # 协议帧编解码（AAD + replay protection）
│   ├── roster/            # LAN peer 目录（hostname + ip:port 列表）
│   ├── storage/           # 加密聊天记录（SM4-CBC + chat.enc）
│   └── transport/         # TCP 长连接池
└── docs/
    ├── PRD.md             # 产品需求
    └── ARCHITECTURE.md    # 架构说明
```

## 协议层与壳的边界

- `pkg/node/` 是产品边界。**任何业务功能都从这里走**，不要从 `internal/*` 直接 import。
- `internal/*` 是实现细节，可以重写。重写不要改 `pkg/node/` 的公共 API 签名。
- `app/app.go`（Wails 桥接层）只做四件事：
  1. 构造 + Start Node
  2. 把 SubscribePeers / SubscribeMessages 转发成 Wails runtime events
  3. 把 Wails bound methods 转给 Node 的同名方法
  4. OnShutdown 同步 Close Node
- `cmd/innerlink/`（CLI REPL）是协议的另一份 demo，独立于 Wails。

## 设计原则

- **CLI 单进程，无 daemon**。一次启动干一件事，干完退；不要 fork / IPC / JSON-RPC。
- **No CGO**。所有依赖必须纯 Go 编译（`tjfoc/gmsm` 满足）。
- **No GUI in core**。`pkg/node` 和 `internal/*` 不引入任何 GUI / Web 框架。
- **No SQL 框架**。持久化只有三个文件：`device.key`（SM2 私钥）、`aliases.json`（明文 JSON）、`chat.enc`（SM4-CBC 加密聊天记录）。
- **国密四件套**：SM2 身份 / 密钥协商 / 签名；SM3 哈希；SM4 会话加密；SM3 在握手做 KDF。
- **M5 协议 v2 推迟**。当前还是 v1（无 AAD、无 replay protection），M5 v2 上了再说。

## 写代码的注意事项

### Go
- **跑业务代码必须用 `C:\Go\bin\go.exe`**（不直接 `go`，PATH 不保证）。
- **改 import path**：本仓的 module 是 `github.com/weishengsuptp/innerlink`，**不要**写 `innerlink-core` 或 `innerlink-desktop`（历史残留）。
- **gmsm 1.4.1 用法**：SM4-CBC IV 必须是 16 字节（`internal/crypto/sm4.go` 里有 helper）。
- **测试**：`go test ./...` 跑全部；e2e 测试在历史仓，CI 还没接。
- **关闭进程**（close bug 旧账）：
  - **不要**加 OnBeforeClose / killWebView2Children / Job Object / SetWinEventHook / runtime.Quit。
  - **不要**再加 close workaround。Wails v2.12 + Win10 1909 的 close bug 历史上踩过 6+ 次。
  - 当前 app.go 是干净的：`OnShutdown` 同步 `nd.Close()`，等所有 goroutine 退，main 返。
  - 如果 close bug 再发，**先查上游**，不要在产品代码加补丁。

### 前端
- **vanilla TS，不引入任何框架**（Vue / React / Svelte 都不要）。
- 前端通过 `frontend/wailsjs/go/main/App.d.ts` 调 Go 方法（编译时自动生成）。
- 前端 listen 的 event 名跟 `app/app.go` 里 `wruntime.EventsEmit` 一致：
  - `peer:event` — peer 上下线 / 新增 / 移除
  - `message` — 收发消息

### Git
- **commit message 中文 + 祈使句**，例如 `git commit -m "修复 discovery 端口冲突"`。
- 不用 `-m` 直接写中文的，用 `git commit -F <file>`（PowerShell 反引号 / `$var` 解析坑）。
- **不要**把 `innerlink.log` / `.innerlink/` / `frontend/node_modules/` 提交（看 `.gitignore`）。
- **不要**用 `git add .`（会把 home 目录的凭证文件卷进去）。

## CI / Release

- CI 三平台跑：`ubuntu-22.04` / `windows-latest` / `macos-14`（**不要** `macos-latest`，新 dyld 严格 LC_UUID，Wails 2.12 CLI 起不来；也不要 `ubuntu-latest`，新 noble 24.04 没 webkit2gtk-4.0）。
- Wails CLI 装 `v2.12.0`（跟 `go.mod` 一致），不升 nightly。
- Release workflow：手动 tag + force push；不自动 release（公网公钥 push 用 inline token 临时方案，不入仓）。
- **`frontend/dist/.gitkeep` 必须入库**：`main.go:36` 有 `//go:embed all:frontend/dist`，fresh clone 目录空会编译失败；Go `all:` 前缀会包含 `.` 开头的文件，所以 `.gitkeep` 能满足 embed。`wails build -clean` 会清空 `frontend/dist/` 包括已跟踪的 `.gitkeep`，所以 build 后 commit 前要 `git checkout HEAD -- frontend/dist/.gitkeep` 恢复，或者用单独的 chore commit 兜底。
- **本地 pre-push 必跑 `wails build -clean -nopackage`**（不带 `-s`），`-s`/`-nopackage` 单独用会跳过 `tsc && vite build`，TS 类型错会漏到 CI（参考 879c55c 的 `Uint8Array`/`SendFileContent` 类型错）。

## 调试

- **CLI REPL** 是最快验证协议层的工具：`go build -o innerlink-cli.exe ./cmd/innerlink`，跑 `innerlink-cli.exe`。
- **Wails dev**：`wails dev`（首次跑要 npm install）。
- **日志文件**：默认 `<cwd>/innerlink.log`，等级 `-log-level debug`。
- **数据目录**：默认 `<cwd>/.innerlink/`（含 device.key / aliases.json / chat.enc）。
- **Windows 关 X 进程不退**（close bug 历史坑）：参考 `docs/PRD.md` §6.1。