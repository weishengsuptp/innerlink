# innerlink

> 国密端到端加密的局域网 P2P 通信 app。
> 单二进制、同 WiFi 直连、无账号、无服务器。

## 这是什么

innerlink 让你在同一局域网里（家里、办公室、会议室）两台电脑之间，直接发加密消息和传文件。
不需要注册账号，不需要中心服务器，**同 WiFi 就能用**。

加密用的是国密（SM2/SM3/SM4）四件套，符合国内合规要求。

## 特性

- **零配置**：装上就能用，不需要账号 / 邮箱 / 手机号
- **国密算法**：SM2 身份认证 + 密钥协商，SM4-GCM 会话加密，SM3 哈希
- **局域网直连**：UDP 主动发现 + TCP 长连接，握手后开始加密会话
- **加密文件传输**：流式分块，断点续传，SHA-256 校验
- **加密聊天记录**：本地存储用 SM4-CBC，重装系统也带不走
- **别名系统**：给设备起个名字，下次自动识别

## 架构

```
┌─────────────────────────────────────────┐
│             Wails v2 (前端)              │
│     vanilla TS, 无框架, 简洁 UI          │
└─────────────────┬───────────────────────┘
                  │ Wails bound methods
                  │ runtime events (peer:event, message)
┌─────────────────▼───────────────────────┐
│       app/app.go (Wails 桥接层)          │
│   pumpPeers / pumpMessages 转发 event    │
└─────────────────┬───────────────────────┘
                  │ pkg/node public API
┌─────────────────▼───────────────────────┐
│         pkg/node (公共 API)              │
│   New/Start/Close/SendText/SendFile/    │
│   ListPeers/SubscribeMessages/History/  │
│   SetAlias/...                          │
└──┬──────────┬──────────┬──────────────┬─┘
   │          │          │              │
┌──▼───┐  ┌───▼────┐  ┌──▼──────┐  ┌────▼────┐
│discovery│ │handshake│ │filetransfer│ │storage │
│(UDP/TCP)│ │(SM2 ECDH)│ │(SM4-CTR)│ │(SM4-CBC)│
└──┬───┘  └───┬────┘  └──┬──────┘  └────┬────┘
   │          │          │              │
   └──────────┴──────────┴──────────────┘
                  │
┌─────────────────▼───────────────────────┐
│   internal/crypto (SM2/SM3/SM4 包装)    │
│   github.com/tjfoc/gmsm v1.4.1          │
└─────────────────────────────────────────┘
```

## 安装

下载对应平台 release（[Releases 页面](../../releases)），解压即用。

## 自己编译

```bash
git clone https://github.com/weishengsuptp/innerlink.git
cd innerlink
go build -o innerlink-cli.exe ./cmd/innerlink  # CLI 验证协议层
wails build                                   # GUI（需要 Node + Wails CLI）
```

## 协议层文档

详细的需求清单和架构说明，看 `docs/PRD.md` 和 `docs/ARCHITECTURE.md`。

## 致谢

- 协议层早期来自 [innerlink-core](https://github.com/weishengsuptp/innerlink-core) 仓（v0.1-v0.4），合并到本仓 v1.0.0。
- 国密算法：[tjfoc/gmsm](https://github.com/tjfoc/gmsm)。
- 桌面壳：[Wails v2](https://wails.io)。

## 许可证

Apache License 2.0 — 详见 [LICENSE](LICENSE)。