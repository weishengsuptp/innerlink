// v1.2.2 (2026-07-08) regression: 用户报告"邀请进群同种 bug"。
//
// 真实三方日志复盘（群 g_422a61b3458d5be5ca944eb099430b882681c517a6e1dc50b995eaf2f70f15a1 "饭局"）：
//   1. 10:38:18 d85dc3bb 退群
//   2. peer 128 (511aab2) witnesslog.Record(d85dc3bb)
//   3. 10:39:51 创建者重新邀请 d85dc3bb，d85dc3bb 加入
//      本地 m.Members[d85dc3bb].JoinedAt = 10:39:51
//   4. 10:40:04 peer 128 上线握手，syncLeaveNoticesToPeer 把陈旧 witnesslog
//      推给创建者。ln.LeftAt ≈ 10:39:28（peer 128 当初见证退群的时间）
//   5. 创建者 ApplyLeaveNotice：d85dc3bb 在 m.Members 里 → 剔除 ← BUG
//      d85dc3bb 刚刚才被重新邀请加入！
//
// 修复: ApplyLeaveNotice 检查 leaver 的当前 JoinedAt vs ln.LeftAt，
// 如果 JoinedAt > LeftAt（leaver 退群后又重新加入），跳过剔除。
package integration_test

import (
	"testing"
	"time"

	"github.com/weishengsuptp/innerlink/pkg/group"
)

func TestScenario_ReInviteAfterLeaveStaleWitness(t *testing.T) {
	h := NewHarness(t, []string{"alice", "bob", "carol"})

	bobID, _ := h.ResolvePeerID("bob")
	carolID, _ := h.ResolvePeerID("carol")
	gid, err := h.CreateGroupAction("alice", "g1", []string{bobID, carolID})
	if err != nil {
		t.Fatalf("CreateGroup: %v", err)
	}
	invBob, _ := h.InviteAction("alice", gid, bobID)
	if err := h.AcceptInviteAction("bob", invBob, "alice"); err != nil {
		t.Fatalf("bob accept: %v", err)
	}
	invCarol, _ := h.InviteAction("alice", gid, carolID)
	if err := h.AcceptInviteAction("carol", invCarol, "alice"); err != nil {
		t.Fatalf("carol accept: %v", err)
	}

	rawID, _ := group.ParseGroupID(gid)
	expectMembers := func(peerName string, want int) {
		p := h.Peer(peerName)
		m, _ := group.LoadMembers(p.DataDir, rawID[:])
		if len(m.Members) != want {
			t.Errorf("%s 期望 %d 人，实际 %d 人 %v", peerName, want, len(m.Members), shortPeerIDs(m.Members))
		}
	}
	for _, name := range []string{"alice", "bob", "carol"} {
		expectMembers(name, 3)
	}

	// 第 1 步：carol 退群——bob 看到 witnesslog 记录。
	if err := h.LeaveGroupAction("carol", gid); err != nil {
		t.Fatalf("carol leave: %v", err)
	}
	for _, name := range []string{"alice", "bob"} {
		expectMembers(name, 2)
	}

	// 第 2 步：bob 短暂"下线"（模拟 511aab2 离线），
	// alice 重新邀请 carol，carol 加入并写入新的 JoinedAt。
	// 注意：carol 退群时本地群已被删，重新加入会建新 members.json，
	// carol 端 JoinedAt 是 AcceptGroupInvite 的 now。
	// 关键是 alice 端的 m.Members[carol].JoinedAt 也要 >= new time。
	if err := h.Peer("bob").Close(); err != nil {
		t.Fatalf("bob close: %v", err)
	}
	// 短暂停顿确保 alice 端 m.Members[carol].JoinedAt > carol 退群时间。
	time.Sleep(10 * time.Millisecond)

	invCarol2, err := h.InviteAction("alice", gid, carolID)
	if err != nil {
		t.Fatalf("invite carol 2: %v", err)
	}
	if err := h.AcceptInviteAction("carol", invCarol2, "alice"); err != nil {
		t.Fatalf("carol re-accept: %v", err)
	}
	expectMembers("alice", 3)
	expectMembers("carol", 3)

	// 第 3 步：bob 重启 + 跟 alice 握手。
	// bob 的 witnesslog 里有 carol 的旧记录，syncLeaveNoticesToPeer 会
	// 把陈旧 leave notice 推给 alice。alice.ApplyLeaveNotice 必须跳过剔除。
	if err := h.RestartPeerAction("bob"); err != nil {
		t.Fatalf("bob restart: %v", err)
	}
	// 模拟握手：bob 推 leavelog + witnesslog 给 alice。
	h.PushLeaveNoticesAction("bob", gid, []string{"alice"})

	// alice 端的 carol 必须仍然存在——不能被陈旧 leave notice 剔除。
	expectMembers("alice", 3)
}

func shortPeerIDs(ms []group.Member) []string {
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		if len(m.PeerID) >= 8 {
			out = append(out, m.PeerID[:8])
		} else {
			out = append(out, m.PeerID)
		}
	}
	return out
}