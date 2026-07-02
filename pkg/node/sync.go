package node

// sync.go — v1.1.4 (2026-07-02) sync infrastructure.
//
// Three operations live here, all driven from Node.Start:
//
//   1. selfidOpenAndRecord  — already in node.go (New-time)
//   2. claimSelfIdentity    — at Start, BEFORE the dispatcher fires.
//                             Walks every on-disk truth source and
//                             replaces references to the old self
//                             peerID with the new one.
//   3. auditRosterAndGroups — at Start, after claim. Drops stale
//                             tombstones from the roster AND
//                             tombstoned members from group
//                             rosters. This is the "two-way
//                             reconciliation" promised in the
//                             design doc.
//
// Both claim and audit are run under n.dataMu so they don't
// race with each other or with future background tickers.
// Right now they're only called from Start (synchronous,
// before the dispatcher accepts inbound frames), so the lock
// is for future-proofing — the same code path will get a
// 1h background ticker in v1.1.5.
//
// Logging convention: every state-changing line is tagged
// [SYNC    ] so a single grep across any innerlink.log
// surfaces the full timeline of "what did the device do to
// catch up after a wipe+reinstall".

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/weishengsuptp/innerlink/internal/roster"
	"github.com/weishengsuptp/innerlink/internal/selfid"
	"github.com/weishengsuptp/innerlink/pkg/group"
)

// claimStats summarizes what claimSelfIdentity changed. Logged
// in a single line at the end of the run so the user can
// glance at startup logs and see "yes, the claim happened
// and replaced N references across M groups".
type claimStats struct {
	oldPeerID        string
	newPeerID        string
	groupsTouched    int   // groups whose members.json we rewrote
	membersReplaced  int   // total member entries with peerID flipped
	creatorsReplaced int   // total Creator field flips
	aliasesRekeyed   int   // alias entries that moved from old → new
	rosterMarked     bool  // did we mark old self entry reset=true?
}

// claimSelfIdentity walks every on-disk truth source and
// replaces the old self peerID with the new one. Called
// from Node.Start BEFORE the dispatcher starts; safe
// because no peer frame can be in flight yet.
//
// Algorithm:
//
//   1. Lock n.dataMu for the entire run. Future background
//      tickers (group-audit, presence-probe) will also take
//      this lock; serializing keeps the on-disk state
//      consistent.
//   2. For each old_peer_id in selfid.History().OldPeerIDs():
//        a. For each groups/<gid>/members.json:
//           - if any member.PeerID == old → replace with new
//           - if Creator == old → replace with new
//           - m.Save (atomic rename)
//        b. aliases.Rekey(old, new)
//        c. roster.MarkReset(old)
//   3. selfid.Save() (the entries recorded in New are
//      already on disk by this point in some flows; this is
//      idempotent).
//   4. Release lock. Log a single [SYNC] summary line.
//
// If the history is empty (fresh install), this is a no-op
// and logs "[SYNC] claim: no prior identity, nothing to claim".
func (n *Node) claimSelfIdentity() claimStats {
	stats := claimStats{newPeerID: n.id.PeerIDHex()}

	if n.selfidStore == nil {
		log.Printf("[SYNC ] claim: selfid store unavailable, skipping (this session can't auto-claim)")
		return stats
	}
	oldPeerIDs := n.selfidStore.OldPeerIDs()
	if len(oldPeerIDs) == 0 {
		log.Printf("[SYNC ] claim: no prior identity, nothing to claim")
		return stats
	}
	log.Printf("[SYNC ] claim: %d prior identity(ies) to migrate to peerID=%s", len(oldPeerIDs), stats.newPeerID[:8])

	n.dataMu.Lock()
	defer n.dataMu.Unlock()

	// Enumerate on-disk groups. We walk the chat store's
	// ListGroups() (which is filtered by chat.enc existence)
	// so we don't accidentally claim into an empty directory.
	renderedIDs, err := n.chatStore.ListGroups()
	if err != nil {
		log.Printf("[WARN ] claim: list groups: %v (skipping group sweep)", err)
	}
	for _, rendered := range renderedIDs {
		rawID, err := group.ParseGroupID(rendered)
		if err != nil {
			log.Printf("[WARN ] claim: parse group %q: %v", rendered, err)
			continue
		}
		m, err := group.LoadMembers(n.dataDir(), rawID)
		if err != nil {
			log.Printf("[WARN ] claim: load members for %s: %v", rendered[:8], err)
			continue
		}
		touched := false
		for i := range m.Members {
			for _, old := range oldPeerIDs {
				if m.Members[i].PeerID == old {
					log.Printf("[SYNC ]   replace member: group=%s peer=%s → %s", rendered[:8], old[:8], stats.newPeerID[:8])
					m.Members[i].PeerID = stats.newPeerID
					// Bump JoinedAt to "now" so the UI
					// shows a sensible join time. The
					// old timestamp is meaningless once
					// the peerID has rotated.
					m.Members[i].JoinedAt = time.Now().UTC()
					stats.membersReplaced++
					touched = true
				}
			}
		}
		// Re-derive IsCreator flags after the swap. Only
		// the entry whose PeerID matches m.Creator gets
		// IsCreator=true; everyone else gets false. This
		// is the same invariant that the GUI's
		// toGroupInfo assumes.
		creatorIndex := -1
		for i := range m.Members {
			if m.Members[i].PeerID == m.Creator {
				m.Members[i].IsCreator = true
				creatorIndex = i
			} else {
				m.Members[i].IsCreator = false
			}
		}
		_ = creatorIndex // index reserved for future "creator was missing" handling
		// Flip the Creator field itself if it pointed at an
		// old peerID. Done after the member sweep so a
		// self-creator case is consistent.
		for _, old := range oldPeerIDs {
			if m.Creator == old {
				log.Printf("[SYNC ]   replace creator: group=%s peer=%s → %s", rendered[:8], old[:8], stats.newPeerID[:8])
				m.Creator = stats.newPeerID
				stats.creatorsReplaced++
				touched = true
				// Re-derive IsCreator again (Creator
				// just changed; the member that was
				// creator=old is now the new
				// peerID, which matches the new
				// Creator).
				for i := range m.Members {
					m.Members[i].IsCreator = m.Members[i].PeerID == m.Creator
				}
			}
		}
		if touched {
			if err := m.Save(n.dataDir(), rawID); err != nil {
				log.Printf("[ERROR] claim: save group %s: %v", rendered[:8], err)
				continue
			}
			stats.groupsTouched++
		}
	}

	// Aliases: rekey each old → new. Rekey is a no-op if
	// the old key doesn't exist, so a freshly-installed
	// user (no aliases yet) just iterates and exits.
	for _, old := range oldPeerIDs {
		// Rekey returns nil whether or not the old entry
		// existed. We track "did we actually move something"
		// by reading the alias map before/after.
		_, hadOld := n.aliasStore.Get(old)
		if err := n.aliasStore.Rekey(old, stats.newPeerID); err != nil {
			log.Printf("[WARN ] claim: rekey alias %s: %v", old[:8], err)
		}
		if hadOld {
			log.Printf("[SYNC ]   rekey alias: peer=%s → %s", old[:8], stats.newPeerID[:8])
			stats.aliasesRekeyed++
		}
	}

	// Roster: mark the old self entry reset=true. The
	// peerID stays in the file for now (audit will reap
	// it after MaxAge), but no one will see it in ListPeers
	// (which goes through ListActive).
	for _, old := range oldPeerIDs {
		entry, err := n.rosterStore.Get(old)
		if err != nil {
			// Not in roster — nothing to mark. The dedup
			// scan on the OTHER peer's side will mark it
			// when they next see the new peerID at this
			// IP. Self-claim here is best-effort.
			continue
		}
		if entry.Reset {
			continue // already marked
		}
		log.Printf("[SYNC ]   reset old self in roster: peer=%s", old[:8])
		if err := n.rosterStore.MarkReset(old); err != nil {
			log.Printf("[WARN ] claim: roster.MarkReset %s: %v", old[:8], err)
		}
		stats.rosterMarked = true
	}

	// Persist everything we touched. Save is a no-op if
	// the underlying store has no dirty entries; the
	// effort is O(fileSize) regardless.
	if err := n.aliasStore.Save(); err != nil {
		log.Printf("[WARN ] claim: alias save: %v", err)
	}
	if err := n.rosterStore.Save(); err != nil {
		log.Printf("[WARN ] claim: roster save: %v", err)
	}
	if err := n.selfidStore.Save(); err != nil {
		log.Printf("[WARN ] claim: self_history save: %v", err)
	}

	log.Printf("[SYNC ] claim done: groups=%d members=%d creators=%d aliases=%d roster_reset=%v",
		stats.groupsTouched, stats.membersReplaced, stats.creatorsReplaced, stats.aliasesRekeyed, stats.rosterMarked)
	return stats
}

// auditStats summarizes what auditRosterAndGroups changed.
// Like claimStats, surfaced as a single log line for
// easy grep.
type auditStats struct {
	rosterTombstonesDropped int      // peerIDs removed from roster.json
	groupMembersRemoved     int      // member entries removed across all groups
	groupsReassignedCreator int      // groups whose Creator field was repointed
	groupsDeleted           []string // groups that became empty and were wiped
}

// rosterTombstoneMaxAge is the cutoff for the
// AuditStaleTombstones drop policy. Tombstones older than
// this are guaranteed to no longer be referenced by any
// claim (the rolling window on selfid is 7 days; we use
// 24h here so a long-running peer cleans up after itself
// even without a wipe cycle). Pinned at 24h to match
// what's documented in the v1.1.4 design doc.
const rosterTombstoneMaxAge = 24 * time.Hour

// auditRosterAndGroups reconciles the roster against the
// group member list, in BOTH directions:
//
//   - Roster → Group: any group member whose peerID is
//     reset=true in the roster gets removed from the
//     group. Self is exempt (we never auto-kick ourselves
//     even if some peer's local view of us is reset=true).
//   - Group → Roster: nothing here — the roster is the
//     authoritative source of "who's alive" and groups
//     are downstream of it.
//
// Plus, AuditStaleTombstones drops roster entries that
// are reset=true AND older than rosterTombstoneMaxAge.
// This is the "file size stops growing" half of the
// reconciliation — without it, the file just accumulates
// tombstones forever.
//
// Like claim, audit is run under n.dataMu. Today it's
// only called from Start (synchronously, before the
// dispatcher opens the door to inbound frames); the lock
// is for future ticker safety.
func (n *Node) auditRosterAndGroups() auditStats {
	stats := auditStats{}

	n.dataMu.Lock()
	defer n.dataMu.Unlock()

	// 1) Roster: drop stale tombstones.
	dropped := n.rosterStore.AuditStaleTombstones(rosterTombstoneMaxAge)
	stats.rosterTombstonesDropped = len(dropped)
	for _, pid := range dropped {
		log.Printf("[SYNC ]   roster: drop stale tombstone peer=%s age>=%s", pid[:8], rosterTombstoneMaxAge)
	}
	if stats.rosterTombstonesDropped > 0 {
		if err := n.rosterStore.Save(); err != nil {
			log.Printf("[WARN ] audit: roster save: %v", err)
		}
	}

	// 2) Groups: scan each members.json and drop members
	//    whose peerID is reset=true. Self is exempt.
	renderedIDs, err := n.chatStore.ListGroups()
	if err != nil {
		log.Printf("[WARN ] audit: list groups: %v", err)
		return stats
	}
	selfHex := n.id.PeerIDHex()
	for _, rendered := range renderedIDs {
		rawID, err := group.ParseGroupID(rendered)
		if err != nil {
			continue
		}
		m, err := group.LoadMembers(n.dataDir(), rawID)
		if err != nil {
			log.Printf("[WARN ] audit: load group %s: %v", rendered[:8], err)
			continue
		}
		kept := m.Members[:0] // reuse backing array
		for _, mem := range m.Members {
			if mem.PeerID == selfHex {
				kept = append(kept, mem)
				continue
			}
			entry, err := n.rosterStore.Get(mem.PeerID)
			if err == nil && entry.Reset {
				log.Printf("[SYNC ]   group: drop tombstoned member group=%s peer=%s", rendered[:8], mem.PeerID[:8])
				stats.groupMembersRemoved++
				continue
			}
			// Member not in our local roster: keep it.
			// (Conservative — better to show a member we
			// don't know about than to kick an active
			// peer whose roster we haven't heard from.)
			kept = append(kept, mem)
		}
		if len(kept) == len(m.Members) {
			// No changes; skip the save and any
			// creator-reassignment logic.
			continue
		}
		// Group went empty. Drop it. The chat.enc + sender-keys
		// directory pair gets wiped via deleteGroupDirsLocal.
		if len(kept) == 0 {
			log.Printf("[SYNC ]   group: empty after audit, deleting group=%s", rendered[:8])
			if err := n.deleteGroupDirsLocal(rendered); err != nil {
				log.Printf("[ERROR] audit: delete empty group %s: %v", rendered[:8], err)
			} else {
				stats.groupsDeleted = append(stats.groupsDeleted, rendered[:8])
			}
			continue
		}
		m.Members = kept
		// Reassign Creator if the original creator was
		// removed. Pick the oldest remaining member; if
		// only self remains, self becomes creator.
		creatorRemoved := true
		for _, mem := range m.Members {
			if mem.PeerID == m.Creator {
				creatorRemoved = false
				break
			}
		}
		if creatorRemoved {
			oldest := m.Members[0]
			for _, mem := range m.Members {
				if mem.JoinedAt.Before(oldest.JoinedAt) {
					oldest = mem
				}
			}
			log.Printf("[SYNC ]   group: reassign creator group=%s peer=%s → %s", rendered[:8], m.Creator[:8], oldest.PeerID[:8])
			m.Creator = oldest.PeerID
			stats.groupsReassignedCreator++
		}
		// Re-derive IsCreator flags (same invariant as in
		// claimSelfIdentity: only the entry matching m.Creator
		// is creator).
		for i := range m.Members {
			m.Members[i].IsCreator = m.Members[i].PeerID == m.Creator
		}
		if err := m.Save(n.dataDir(), rawID); err != nil {
			log.Printf("[ERROR] audit: save group %s: %v", rendered[:8], err)
		}
	}

	log.Printf("[SYNC ] audit done: roster_tombstones_dropped=%d members_removed=%d creators_reassigned=%d groups_deleted=%d",
		stats.rosterTombstonesDropped, stats.groupMembersRemoved, stats.groupsReassignedCreator, len(stats.groupsDeleted))
	return stats
}

// runStartupSync is the single entry point that the rest
// of the package calls. It does claim → audit in that
// order, with rich [SYNC] logging throughout. Returns
// when both have finished; the dispatcher is still NOT
// running, so no peer frame can interfere.
//
// Why claim before audit: a fresh claim might have
// moved peerIDs around, including in the roster. audit
// operates on the post-claim state, so any tombstone
// it sees reflects the truth AFTER migration. Reversing
// the order would risk audit deleting a still-relevant
// old peerID before claim had a chance to mark it
// reset=true.
func (n *Node) runStartupSync() {
	log.Printf("[SYNC ] === startup sync BEGIN ===")
	t0 := time.Now()
	claimStats := n.claimSelfIdentity()
	auditStats := n.auditRosterAndGroups()
	elapsed := time.Since(t0)
	log.Printf("[SYNC ] === startup sync END elapsed=%s claim_groups=%d audit_drops=%d ===",
		elapsed, claimStats.groupsTouched, auditStats.rosterTombstonesDropped+auditStats.groupMembersRemoved)
}

// _ = selfid.MaxAge silences the "imported and not used"
// warning if a future commit removes the only reference
// to selfid from this file. Cheap insurance.
var _ = selfid.MaxAge

// _ = fmt.Sprintf silences the same for the "fmt" import
// in case we strip the only call site during a future
// refactor.
var _ = fmt.Sprintf

// _ = filepath.Join silences the same for filepath. Used
// here because we may want a "what's the path I'm about
// to write to" log line in a future debug pass.
var _ = filepath.Join

// _ = os.CreateTemp silences the same for os. Kept here
// because a future "self_history.json.tmp pre-flight"
// log line may use it.
var _ = os.CreateTemp

// _ = time.Second is here to anchor the import in case
// all the per-minute time constants migrate into a
// central place.
var _ = time.Second

// _ = roster.AuditStaleTombstones keeps the import
// path honest — if someone refactors the audit call
// out of this file, the test will catch it.
var _ = (*roster.Store)(nil).AuditStaleTombstones
