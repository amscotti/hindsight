package db

import (
	"errors"
	"testing"
	"time"
)

// seedFacilitationBoard builds a board with one column holding three
// cards with distinct vote counts, returning the board, the column,
// and the cards in creation order.
func seedFacilitationBoard(t *testing.T, store *Store) (*Board, *Column, []*Card) {
	t.Helper()

	ctx := t.Context()
	board := seedBoard(t, store)
	if _, err := store.SetPhase(ctx, board.ID, "vote"); err != nil {
		t.Fatalf("SetPhase vote: %v", err)
	}
	col := seedColumn(t, store, board.ID, "Mad", 0)
	var cards []*Card
	for i, body := range []string{"alpha", "beta", "gamma"} {
		card, err := store.CreateCard(ctx, col.ID, body, "ana")
		if err != nil {
			t.Fatalf("CreateCard %q: %v", body, err)
		}
		cards = append(cards, card)
		_ = i
	}
	// Distinct vote counts so ordering assertions are meaningful:
	// alpha ends with 1 vote, beta with 3, gamma with 2.
	voters := []string{"v1", "v2", "v3"}
	for i, name := range voters {
		p, err := store.CreateParticipant(ctx, board.ID, name, "token-"+name)
		if err != nil {
			t.Fatalf("CreateParticipant %q: %v", name, err)
		}
		targets := [][]int64{{1}, {0, 1, 2}, {1, 2}}[i]
		for _, idx := range targets {
			if err := store.Vote(ctx, p.ID, cards[idx].ID); err != nil {
				t.Fatalf("Vote %s -> card %d: %v", name, idx, err)
			}
		}
	}
	return board, col, cards
}

func cardVotes(t *testing.T, store *Store, cardID int64) int {
	t.Helper()

	card, err := store.GetCard(t.Context(), cardID)
	if err != nil {
		t.Fatalf("GetCard: %v", err)
	}
	return card.Votes
}

func TestSetPhaseAcceptsAnyTransition(t *testing.T) {
	store := NewStore(openTestDB(t))
	board := seedBoard(t, store)
	ctx := t.Context()

	// Forward, backward, and jumping transitions are all allowed; the
	// recommendation to move forward lives in the UI, not the store.
	for _, phase := range []string{"vote", "discuss", "done", "collect", "done"} {
		updated, err := store.SetPhase(ctx, board.ID, phase)
		if err != nil {
			t.Fatalf("SetPhase %q: %v", phase, err)
		}
		if updated.Phase != phase {
			t.Errorf("phase = %q, want %q", updated.Phase, phase)
		}
		got, err := store.GetBoardByID(ctx, board.ID)
		if err != nil {
			t.Fatalf("GetBoardByID: %v", err)
		}
		if got.Phase != phase {
			t.Errorf("reloaded phase = %q, want %q", got.Phase, phase)
		}
	}

	if _, err := store.SetPhase(ctx, board.ID, "archived"); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("SetPhase bogus err = %v, want ErrInvalidInput", err)
	}
	if _, err := store.SetPhase(ctx, board.ID, ""); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("SetPhase empty err = %v, want ErrInvalidInput", err)
	}
	if _, err := store.SetPhase(ctx, 999999, "vote"); !errors.Is(err, ErrNotFound) {
		t.Errorf("SetPhase unknown board err = %v, want ErrNotFound", err)
	}
}

func TestSetLocksAndHiddenRoundTrip(t *testing.T) {
	store := NewStore(openTestDB(t))
	board := seedBoard(t, store)
	ctx := t.Context()

	if _, err := store.SetVotingLocked(ctx, board.ID, true); err != nil {
		t.Fatalf("SetVotingLocked: %v", err)
	}
	if _, err := store.SetCardsLocked(ctx, board.ID, true); err != nil {
		t.Fatalf("SetCardsLocked: %v", err)
	}
	if _, err := store.SetCardsHidden(ctx, board.ID, true); err != nil {
		t.Fatalf("SetCardsHidden: %v", err)
	}
	got, err := store.GetBoardByID(ctx, board.ID)
	if err != nil {
		t.Fatalf("GetBoardByID: %v", err)
	}
	if !got.VotingLocked || !got.CardsLocked || !got.CardsHidden {
		t.Errorf("flags = %+v, want all set", got)
	}
	if _, err := store.SetVotingLocked(ctx, board.ID, false); err != nil {
		t.Fatalf("SetVotingLocked clear: %v", err)
	}
	if _, err := store.SetCardsLocked(ctx, board.ID, false); err != nil {
		t.Fatalf("SetCardsLocked clear: %v", err)
	}
	if _, err := store.SetCardsHidden(ctx, board.ID, false); err != nil {
		t.Fatalf("SetCardsHidden clear: %v", err)
	}
	cleared, err := store.GetBoardByID(ctx, board.ID)
	if err != nil {
		t.Fatalf("GetBoardByID: %v", err)
	}
	if cleared.VotingLocked || cleared.CardsLocked || cleared.CardsHidden {
		t.Errorf("flags after clear = %+v, want all false", cleared)
	}
	if _, err := store.SetVotingLocked(ctx, 999999, true); !errors.Is(err, ErrNotFound) {
		t.Errorf("SetVotingLocked unknown board err = %v, want ErrNotFound", err)
	}
	if _, err := store.SetCardsLocked(ctx, 999999, true); !errors.Is(err, ErrNotFound) {
		t.Errorf("SetCardsLocked unknown board err = %v, want ErrNotFound", err)
	}
	if _, err := store.SetCardsHidden(ctx, 999999, true); !errors.Is(err, ErrNotFound) {
		t.Errorf("SetCardsHidden unknown board err = %v, want ErrNotFound", err)
	}
}

func TestSetTimerEndsAtAndDisarm(t *testing.T) {
	store := NewStore(openTestDB(t))
	board := seedBoard(t, store)
	ctx := t.Context()

	endsAt := time.Now().UTC().Add(90 * time.Second).Truncate(time.Millisecond)
	if armed, err := store.DisarmTimer(ctx, board.ID, endsAt); err != nil || armed {
		t.Fatalf("DisarmTimer on idle board = %v, %v; want false, nil", armed, err)
	}

	updated, err := store.SetTimerEndsAt(ctx, board.ID, &endsAt)
	if err != nil {
		t.Fatalf("SetTimerEndsAt: %v", err)
	}
	if updated.TimerEndsAt == nil || updated.TimerEndsAt.Unix() != endsAt.Unix() {
		t.Errorf("timer = %v, want %v", updated.TimerEndsAt, endsAt)
	}
	got, err := store.GetBoardByID(ctx, board.ID)
	if err != nil {
		t.Fatalf("GetBoardByID: %v", err)
	}
	if got.TimerEndsAt == nil {
		t.Fatal("reloaded timer is nil, want it armed")
	}

	// A stale instant never disarms: a re-arm landing in the window
	// keeps its own expiry instead of being cleared by the old fire.
	stale := endsAt.Add(-time.Second)
	if armed, err := store.DisarmTimer(ctx, board.ID, stale); err != nil || armed {
		t.Fatalf("stale DisarmTimer = %v, %v; want false, nil", armed, err)
	}
	still, err := store.GetBoardByID(ctx, board.ID)
	if err != nil {
		t.Fatalf("GetBoardByID: %v", err)
	}
	if still.TimerEndsAt == nil {
		t.Fatal("stale disarm cleared the timer, want it still armed")
	}

	// The first disarm wins; a racing second disarm reports no timer.
	armed, err := store.DisarmTimer(ctx, board.ID, endsAt)
	if err != nil || !armed {
		t.Fatalf("first DisarmTimer = %v, %v; want true, nil", armed, err)
	}
	armed, err = store.DisarmTimer(ctx, board.ID, endsAt)
	if err != nil || armed {
		t.Fatalf("second DisarmTimer = %v, %v; want false, nil", armed, err)
	}
	cleared, err := store.GetBoardByID(ctx, board.ID)
	if err != nil {
		t.Fatalf("GetBoardByID: %v", err)
	}
	if cleared.TimerEndsAt != nil {
		t.Errorf("timer after disarm = %v, want nil", cleared.TimerEndsAt)
	}

	if _, err := store.SetTimerEndsAt(ctx, 999999, &endsAt); !errors.Is(err, ErrNotFound) {
		t.Errorf("SetTimerEndsAt unknown board err = %v, want ErrNotFound", err)
	}
}

func TestDisarmTimerKeepsRearmedInstant(t *testing.T) {
	store := NewStore(openTestDB(t))
	board := seedBoard(t, store)
	ctx := t.Context()

	first := time.Now().UTC().Add(time.Second).Truncate(time.Millisecond)
	second := first.Add(time.Minute)
	if _, err := store.SetTimerEndsAt(ctx, board.ID, &first); err != nil {
		t.Fatalf("SetTimerEndsAt first: %v", err)
	}
	if _, err := store.SetTimerEndsAt(ctx, board.ID, &second); err != nil {
		t.Fatalf("SetTimerEndsAt second: %v", err)
	}
	// The old fire must not clear the re-armed timer.
	if armed, err := store.DisarmTimer(ctx, board.ID, first); err != nil || armed {
		t.Fatalf("stale DisarmTimer = %v, %v; want false, nil", armed, err)
	}
	got, err := store.GetBoardByID(ctx, board.ID)
	if err != nil {
		t.Fatalf("GetBoardByID: %v", err)
	}
	if got.TimerEndsAt == nil || !got.TimerEndsAt.Equal(second) {
		t.Fatalf("timer = %v, want the re-armed %v", got.TimerEndsAt, second)
	}
	if armed, err := store.DisarmTimer(ctx, board.ID, second); err != nil || !armed {
		t.Fatalf("current DisarmTimer = %v, %v; want true, nil", armed, err)
	}
}

func TestSetFocusRoundTrip(t *testing.T) {
	store := NewStore(openTestDB(t))
	board, _, cards := seedFacilitationBoard(t, store)
	ctx := t.Context()

	updated, err := store.SetFocus(ctx, board.ID, &cards[1].ID)
	if err != nil {
		t.Fatalf("SetFocus: %v", err)
	}
	if updated.FocusedCardID == nil || *updated.FocusedCardID != cards[1].ID {
		t.Errorf("focus = %v, want %d", updated.FocusedCardID, cards[1].ID)
	}
	cleared, err := store.SetFocus(ctx, board.ID, nil)
	if err != nil {
		t.Fatalf("SetFocus clear: %v", err)
	}
	if cleared.FocusedCardID != nil {
		t.Errorf("focus after clear = %v, want nil", cleared.FocusedCardID)
	}

	missing := int64(999999)
	if _, err := store.SetFocus(ctx, board.ID, &missing); !errors.Is(err, ErrNotFound) {
		t.Errorf("SetFocus unknown card err = %v, want ErrNotFound", err)
	}
	other := seedBoard(t, store)
	otherCol := seedColumn(t, store, other.ID, "Other", 0)
	foreign, err := store.CreateCard(ctx, otherCol.ID, "foreign", "bo")
	if err != nil {
		t.Fatalf("CreateCard foreign: %v", err)
	}
	if _, err := store.SetFocus(ctx, board.ID, &foreign.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("SetFocus foreign card err = %v, want ErrNotFound", err)
	}
	if _, err := store.SetFocus(ctx, 999999, &cards[0].ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("SetFocus unknown board err = %v, want ErrNotFound", err)
	}
}

func TestSetDiscussedRoundTrip(t *testing.T) {
	store := NewStore(openTestDB(t))
	_, _, cards := seedFacilitationBoard(t, store)
	ctx := t.Context()

	updated, err := store.SetDiscussed(ctx, cards[0].ID, true)
	if err != nil {
		t.Fatalf("SetDiscussed: %v", err)
	}
	if !updated.Discussed {
		t.Error("discussed = false, want true")
	}
	again, err := store.GetCard(ctx, cards[0].ID)
	if err != nil {
		t.Fatalf("GetCard: %v", err)
	}
	if !again.Discussed {
		t.Error("reloaded discussed = false, want true")
	}
	if _, err := store.SetDiscussed(ctx, cards[0].ID, false); err != nil {
		t.Fatalf("SetDiscussed clear: %v", err)
	}
	if _, err := store.SetDiscussed(ctx, 999999, true); !errors.Is(err, ErrNotFound) {
		t.Errorf("SetDiscussed unknown card err = %v, want ErrNotFound", err)
	}
}

func TestSetGroupUngroupAndValidation(t *testing.T) {
	store := NewStore(openTestDB(t))
	board, _, cards := seedFacilitationBoard(t, store)
	ctx := t.Context()
	leader, member := cards[0].ID, cards[1].ID

	grouped, err := store.SetGroup(ctx, member, &leader)
	if err != nil {
		t.Fatalf("SetGroup: %v", err)
	}
	if grouped.GroupID == nil || *grouped.GroupID != leader {
		t.Errorf("group_id = %v, want %d", grouped.GroupID, leader)
	}

	// Grouping under a member resolves to the leader: no chains.
	third := cards[2].ID
	flattened, err := store.SetGroup(ctx, third, &member)
	if err != nil {
		t.Fatalf("SetGroup under member: %v", err)
	}
	if flattened.GroupID == nil || *flattened.GroupID != leader {
		t.Errorf("nested group_id = %v, want flattened leader %d", flattened.GroupID, leader)
	}

	ungrouped, err := store.SetGroup(ctx, member, nil)
	if err != nil {
		t.Fatalf("SetGroup ungroup: %v", err)
	}
	if ungrouped.GroupID != nil {
		t.Errorf("group_id after ungroup = %v, want nil", ungrouped.GroupID)
	}

	if _, err := store.SetGroup(ctx, leader, &leader); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("SetGroup self err = %v, want ErrInvalidInput", err)
	}
	// Reverse cycle: member groups under leader, then the leader
	// grouping under its own member must fail.
	reva, revb := cards[0].ID, cards[1].ID
	if _, err := store.SetGroup(ctx, revb, nil); err != nil {
		t.Fatalf("SetGroup ungroup for cycle setup: %v", err)
	}
	if _, err := store.SetGroup(ctx, cards[2].ID, nil); err != nil {
		t.Fatalf("SetGroup ungroup for cycle setup: %v", err)
	}
	if _, err := store.SetGroup(ctx, revb, &reva); err != nil {
		t.Fatalf("SetGroup A->B setup: %v", err)
	}
	if _, err := store.SetGroup(ctx, reva, &revb); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("SetGroup reverse cycle err = %v, want ErrInvalidInput", err)
	}
	missing := int64(999999)
	if _, err := store.SetGroup(ctx, member, &missing); !errors.Is(err, ErrNotFound) {
		t.Errorf("SetGroup unknown leader err = %v, want ErrNotFound", err)
	}
	other := seedBoard(t, store)
	otherCol := seedColumn(t, store, other.ID, "Other", 0)
	foreign, err := store.CreateCard(ctx, otherCol.ID, "foreign", "bo")
	if err != nil {
		t.Fatalf("CreateCard foreign: %v", err)
	}
	if _, err := store.SetGroup(ctx, member, &foreign.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("SetGroup foreign leader err = %v, want ErrNotFound", err)
	}
	if _, err := store.SetGroup(ctx, missing, &leader); !errors.Is(err, ErrNotFound) {
		t.Errorf("SetGroup unknown card err = %v, want ErrNotFound", err)
	}
	_ = board
}

func TestSetGroupMergesMembersIntoResolvedLeader(t *testing.T) {
	store := NewStore(openTestDB(t))
	ctx := t.Context()
	board := seedBoard(t, store)
	if _, err := store.SetPhase(ctx, board.ID, "vote"); err != nil {
		t.Fatalf("SetPhase vote: %v", err)
	}
	col := seedColumn(t, store, board.ID, "Mad", 0)
	mkcard := func(body string) *Card {
		t.Helper()
		card, err := store.CreateCard(ctx, col.ID, body, "ana")
		if err != nil {
			t.Fatalf("CreateCard %q: %v", body, err)
		}
		return card
	}
	leader, member, final := mkcard("leader"), mkcard("member"), mkcard("final")
	voter := seedParticipant(t, store, board.ID, "voter")
	for _, id := range []int64{leader.ID, member.ID, final.ID} {
		if err := store.Vote(ctx, voter.ID, id); err != nil {
			t.Fatalf("Vote %d: %v", id, err)
		}
	}

	if _, err := store.SetGroup(ctx, member.ID, &leader.ID); err != nil {
		t.Fatalf("SetGroup member under leader: %v", err)
	}
	// Grouping the leader under a third card must re-home its members
	// to the resolved leader: no B→A→C chains may remain.
	merged, err := store.SetGroup(ctx, leader.ID, &final.ID)
	if err != nil {
		t.Fatalf("SetGroup leader under final: %v", err)
	}
	if merged.GroupID == nil || *merged.GroupID != final.ID {
		t.Fatalf("leader group_id = %v, want %d", merged.GroupID, final.ID)
	}
	reloaded, err := store.GetCard(ctx, member.ID)
	if err != nil {
		t.Fatalf("GetCard member: %v", err)
	}
	if reloaded.GroupID == nil || *reloaded.GroupID != final.ID {
		t.Fatalf("member group_id = %v, want re-homed to %d", reloaded.GroupID, final.ID)
	}
	// One-level invariant: every group link points at a top-level card.
	for _, id := range []int64{leader.ID, member.ID} {
		card, err := store.GetCard(ctx, id)
		if err != nil {
			t.Fatalf("GetCard %d: %v", id, err)
		}
		if card.GroupID == nil {
			t.Fatalf("card %d lost its group link", id)
		}
		target, err := store.GetCard(ctx, *card.GroupID)
		if err != nil {
			t.Fatalf("GetCard leader %d: %v", *card.GroupID, err)
		}
		if target.GroupID != nil {
			t.Errorf("card %d groups under %d, which is itself grouped: chain remains", id, target.ID)
		}
	}
	sums, err := store.GroupVoteSums(ctx, board.ID)
	if err != nil {
		t.Fatalf("GroupVoteSums: %v", err)
	}
	if got := sums[final.ID]; got != 3 {
		t.Errorf("final sum = %d, want 3 (one vote per card)", got)
	}
	if len(sums) != 1 {
		t.Errorf("sums = %v, want only the final leader", sums)
	}

	// Grouping under a member resolves and merges at once: d carries
	// member e, and targeting the (now grouped) member lands both flat
	// under the final leader.
	d, e := mkcard("d"), mkcard("e")
	if _, err := store.SetGroup(ctx, e.ID, &d.ID); err != nil {
		t.Fatalf("SetGroup e under d: %v", err)
	}
	if _, err := store.SetGroup(ctx, d.ID, &member.ID); err != nil {
		t.Fatalf("SetGroup d under member: %v", err)
	}
	for _, id := range []int64{d.ID, e.ID} {
		card, err := store.GetCard(ctx, id)
		if err != nil {
			t.Fatalf("GetCard %d: %v", id, err)
		}
		if card.GroupID == nil || *card.GroupID != final.ID {
			t.Errorf("card %d group_id = %v, want flattened leader %d", id, card.GroupID, final.ID)
		}
	}
}

func TestGroupVoteSumsTotalLeaderPlusMembers(t *testing.T) {
	store := NewStore(openTestDB(t))
	_, _, cards := seedFacilitationBoard(t, store)
	ctx := t.Context()
	// Seeded counts: alpha 1, beta 3, gamma 2.
	leader, member := cards[0].ID, cards[1].ID

	empty, err := store.GroupVoteSums(ctx, cards[0].BoardID)
	if err != nil {
		t.Fatalf("GroupVoteSums: %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("sums without groups = %v, want empty", empty)
	}

	if _, err := store.SetGroup(ctx, member, &leader); err != nil {
		t.Fatalf("SetGroup: %v", err)
	}
	sums, err := store.GroupVoteSums(ctx, cards[0].BoardID)
	if err != nil {
		t.Fatalf("GroupVoteSums: %v", err)
	}
	if got := sums[leader]; got != 4 {
		t.Errorf("leader sum = %d, want 4 (1 own + 3 member)", got)
	}

	// Stored counters stay per-card: sums are computed, never stored.
	if got := cardVotes(t, store, leader); got != 1 {
		t.Errorf("stored leader votes = %d, want 1", got)
	}
	if got := cardVotes(t, store, member); got != 3 {
		t.Errorf("stored member votes = %d, want 3", got)
	}
}

func TestSortColumnByVotesOrdersByGroupTotals(t *testing.T) {
	store := NewStore(openTestDB(t))
	board, col, cards := seedFacilitationBoard(t, store)
	ctx := t.Context()
	// Stored order is alpha(1), beta(3), gamma(2) at positions 0,1,2.
	// Grouping gamma under alpha totals alpha to 3, tying beta.
	if _, err := store.SetGroup(ctx, cards[2].ID, &cards[0].ID); err != nil {
		t.Fatalf("SetGroup: %v", err)
	}

	if err := store.SortColumnByVotes(ctx, col.ID); err != nil {
		t.Fatalf("SortColumnByVotes: %v", err)
	}
	ordered, err := store.ListCards(ctx, col.ID)
	if err != nil {
		t.Fatalf("ListCards: %v", err)
	}
	// Totals: alpha 3, beta 3, gamma is a member pinned after alpha.
	want := []int64{cards[0].ID, cards[2].ID, cards[1].ID}
	for i, id := range want {
		if ordered[i].ID != id {
			t.Errorf("position %d = card %d, want %d (order %v)", i, ordered[i].ID, id, want)
		}
		if ordered[i].Position != i {
			t.Errorf("card %d position = %d, want dense %d", ordered[i].ID, ordered[i].Position, i)
		}
	}

	if err := store.SortColumnByVotes(ctx, 999999); !errors.Is(err, ErrNotFound) {
		t.Errorf("SortColumnByVotes unknown column err = %v, want ErrNotFound", err)
	}
	_ = board
}

func TestStepFocusAdvancesAndMarksDiscussed(t *testing.T) {
	store := NewStore(openTestDB(t))
	board, _, cards := seedFacilitationBoard(t, store)
	ctx := t.Context()
	// Totals: alpha 1, beta 3, gamma 2. Queue order: beta, gamma, alpha.
	byBody := map[string]int64{}
	for _, c := range cards {
		byBody[c.Body] = c.ID
	}

	// No focus yet: next opens with the top card.
	first, affected, err := store.StepFocus(ctx, board.ID, true)
	if err != nil {
		t.Fatalf("StepFocus next from empty: %v", err)
	}
	if first.FocusedCardID == nil || *first.FocusedCardID != byBody["beta"] {
		t.Errorf("focus = %v, want beta", first.FocusedCardID)
	}
	if len(affected) != 1 || affected[0] != byBody["beta"] {
		t.Errorf("affected = %v, want [beta]", affected)
	}

	// Next marks beta discussed and advances to gamma.
	second, affected, err := store.StepFocus(ctx, board.ID, true)
	if err != nil {
		t.Fatalf("StepFocus next: %v", err)
	}
	if second.FocusedCardID == nil || *second.FocusedCardID != byBody["gamma"] {
		t.Errorf("focus = %v, want gamma", second.FocusedCardID)
	}
	discussed, err := store.GetCard(ctx, byBody["beta"])
	if err != nil {
		t.Fatalf("GetCard beta: %v", err)
	}
	if !discussed.Discussed {
		t.Error("beta must be marked discussed after advancing past it")
	}
	if len(affected) != 2 {
		t.Errorf("affected = %v, want the marked card plus the new focus", affected)
	}

	// Prev from gamma marks gamma and steps back to beta — except beta
	// is discussed, so the queue before it is empty and focus clears.
	third, _, err := store.StepFocus(ctx, board.ID, false)
	if err != nil {
		t.Fatalf("StepFocus prev: %v", err)
	}
	if third.FocusedCardID != nil {
		t.Errorf("focus = %v, want nil (nothing undiscussed before gamma)", third.FocusedCardID)
	}

	// Draining the queue clears focus: next from alpha marks it and
	// finds nothing after.
	alpha := byBody["alpha"]
	if _, err := store.SetFocus(ctx, board.ID, &alpha); err != nil {
		t.Fatalf("SetFocus alpha: %v", err)
	}
	drained, _, err := store.StepFocus(ctx, board.ID, true)
	if err != nil {
		t.Fatalf("StepFocus drain: %v", err)
	}
	if drained.FocusedCardID != nil {
		t.Errorf("focus after drain = %v, want nil", drained.FocusedCardID)
	}

	// Backward from queue head marks the head and clears focus.
	beta := byBody["beta"]
	if _, err := store.SetFocus(ctx, board.ID, nil); err != nil {
		t.Fatalf("SetFocus clear for backward-head: %v", err)
	}
	for _, c := range cards {
		if _, err := store.SetDiscussed(ctx, c.ID, false); err != nil {
			t.Fatalf("SetDiscussed reset: %v", err)
		}
	}
	if _, err := store.SetFocus(ctx, board.ID, &beta); err != nil {
		t.Fatalf("SetFocus beta: %v", err)
	}
	head, _, err := store.StepFocus(ctx, board.ID, false)
	if err != nil {
		t.Fatalf("StepFocus backward from head: %v", err)
	}
	if head.FocusedCardID != nil {
		t.Errorf("focus = %v, want nil (nothing before queue head)", head.FocusedCardID)
	}
	if got, err := store.GetCard(ctx, beta); err != nil {
		t.Fatalf("GetCard beta: %v", err)
	} else if !got.Discussed {
		t.Error("beta must be marked discussed after stepping backward past it")
	}

	if _, _, err := store.StepFocus(ctx, 999999, true); !errors.Is(err, ErrNotFound) {
		t.Errorf("StepFocus unknown board err = %v, want ErrNotFound", err)
	}
}

func TestStepFocusSkipsGroupedMembers(t *testing.T) {
	store := NewStore(openTestDB(t))
	board, _, cards := seedFacilitationBoard(t, store)
	ctx := t.Context()

	// Members never take focus themselves; the leader carries the group.
	if _, err := store.SetGroup(ctx, cards[1].ID, &cards[0].ID); err != nil {
		t.Fatalf("SetGroup: %v", err)
	}
	first, _, err := store.StepFocus(ctx, board.ID, true)
	if err != nil {
		t.Fatalf("StepFocus: %v", err)
	}
	// Totals: alpha group 4, gamma 2.
	if first.FocusedCardID == nil || *first.FocusedCardID != cards[0].ID {
		t.Errorf("focus = %v, want the group leader %d", first.FocusedCardID, cards[0].ID)
	}
}

func TestListArmedTimers(t *testing.T) {
	store := NewStore(openTestDB(t))
	ctx := t.Context()

	if got, err := store.ListArmedTimers(ctx); err != nil || len(got) != 0 {
		t.Fatalf("ListArmedTimers on fresh DB = %v, %v; want 0, nil", got, err)
	}
	armed := seedBoard(t, store)
	idle := seedBoard(t, store)
	endsAt := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	if _, err := store.SetTimerEndsAt(ctx, armed.ID, &endsAt); err != nil {
		t.Fatalf("SetTimerEndsAt: %v", err)
	}

	got, err := store.ListArmedTimers(ctx)
	if err != nil {
		t.Fatalf("ListArmedTimers: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("ListArmedTimers = %d rows, want 1 (idle board %d excluded)", len(got), idle.ID)
	}
	if got[0].BoardID != armed.ID || got[0].PublicID != armed.PublicID {
		t.Errorf("ListArmedTimers row = %+v, want board %d", got[0], armed.ID)
	}
	if !got[0].EndsAt.Equal(endsAt) {
		t.Errorf("ListArmedTimers instant = %v, want %v", got[0].EndsAt, endsAt)
	}
}
