package db

import (
	"errors"
	"testing"
)

func seedCard(t *testing.T, store *Store, columnID int64, body, author string) *Card {
	t.Helper()

	card, err := store.CreateCard(t.Context(), columnID, body, author)
	if err != nil {
		t.Fatalf("CreateCard: %v", err)
	}
	return card
}

func cardOrder(t *testing.T, store *Store, columnID int64) []string {
	t.Helper()

	cards, err := store.ListCards(t.Context(), columnID)
	if err != nil {
		t.Fatalf("ListCards: %v", err)
	}
	var bodies []string
	for _, card := range cards {
		bodies = append(bodies, card.Body)
	}
	return bodies
}

func TestCreateCardAppendsPositions(t *testing.T) {
	store := NewStore(openTestDB(t))
	ctx := t.Context()
	board := seedBoard(t, store)
	col := seedColumn(t, store, board.ID, "col", 0)

	first := seedCard(t, store, col.ID, "first", "ana")
	second := seedCard(t, store, col.ID, "second", "bo")
	third := seedCard(t, store, col.ID, "third", "cy")

	if first.Position != 0 || second.Position != 1 || third.Position != 2 {
		t.Errorf("positions = %d/%d/%d, want 0/1/2",
			first.Position, second.Position, third.Position)
	}
	for _, card := range []*Card{first, second, third} {
		if card.BoardID != board.ID {
			t.Errorf("card %q board = %d, want %d", card.Body, card.BoardID, board.ID)
		}
		if card.ColumnID != col.ID {
			t.Errorf("card %q column = %d, want %d", card.Body, card.ColumnID, col.ID)
		}
	}
	if got := cardOrder(t, store, col.ID); len(got) != 3 ||
		got[0] != "first" || got[1] != "second" || got[2] != "third" {
		t.Errorf("list order = %v, want [first second third]", got)
	}

	round, err := store.GetCard(ctx, second.ID)
	if err != nil {
		t.Fatalf("GetCard: %v", err)
	}
	if round.Body != "second" || round.AuthorName != "bo" || round.Votes != 0 {
		t.Errorf("round trip mismatch: %+v", round)
	}
	if _, err := store.GetCard(ctx, 999999); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown card err = %v, want ErrNotFound", err)
	}
}

func TestCreateCardRejectsBadInput(t *testing.T) {
	store := NewStore(openTestDB(t))
	ctx := t.Context()
	board := seedBoard(t, store)
	col := seedColumn(t, store, board.ID, "col", 0)

	if _, err := store.CreateCard(ctx, col.ID, "   ", "ana"); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("blank body err = %v, want ErrInvalidInput", err)
	}
	long := make([]rune, 2001)
	for i := range long {
		long[i] = 'x'
	}
	if _, err := store.CreateCard(ctx, col.ID, string(long), "ana"); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("overlong body err = %v, want ErrInvalidInput", err)
	}
	if _, err := store.CreateCard(ctx, 999999, "body", "ana"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown column err = %v, want ErrNotFound", err)
	}
}

func TestListCardsEmptyColumn(t *testing.T) {
	store := NewStore(openTestDB(t))
	board := seedBoard(t, store)
	col := seedColumn(t, store, board.ID, "col", 0)

	cards, err := store.ListCards(t.Context(), col.ID)
	if err != nil {
		t.Fatalf("ListCards: %v", err)
	}
	if len(cards) != 0 {
		t.Errorf("empty column lists %d cards, want 0", len(cards))
	}
}

func TestUpdateCardBodyKeepsPlacement(t *testing.T) {
	store := NewStore(openTestDB(t))
	ctx := t.Context()
	board := seedBoard(t, store)
	if _, err := store.SetPhase(ctx, board.ID, "vote"); err != nil {
		t.Fatalf("SetPhase vote: %v", err)
	}
	col := seedColumn(t, store, board.ID, "col", 0)
	card := seedCard(t, store, col.ID, "before", "ana")
	voter := seedParticipant(t, store, board.ID, "bo")
	if err := store.Vote(ctx, voter.ID, card.ID); err != nil {
		t.Fatalf("Vote: %v", err)
	}

	updated, err := store.UpdateCardBody(ctx, card.ID, "after")
	if err != nil {
		t.Fatalf("UpdateCardBody: %v", err)
	}
	if updated.Body != "after" {
		t.Errorf("body = %q, want %q", updated.Body, "after")
	}
	if updated.Votes != 1 || updated.ColumnID != col.ID || updated.Position != 0 ||
		updated.AuthorName != "ana" || updated.BoardID != board.ID {
		t.Errorf("edit disturbed the rest of the row: %+v", updated)
	}

	if _, err := store.UpdateCardBody(ctx, 999999, "x"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown card err = %v, want ErrNotFound", err)
	}
	if _, err := store.UpdateCardBody(ctx, card.ID, "  "); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("blank body err = %v, want ErrInvalidInput", err)
	}
	long := make([]rune, 2001)
	for i := range long {
		long[i] = 'y'
	}
	if _, err := store.UpdateCardBody(ctx, card.ID, string(long)); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("overlong body err = %v, want ErrInvalidInput", err)
	}
}

func TestMoveCardReordersWithinColumn(t *testing.T) {
	store := NewStore(openTestDB(t))
	ctx := t.Context()
	board := seedBoard(t, store)
	col := seedColumn(t, store, board.ID, "col", 0)
	a := seedCard(t, store, col.ID, "a", "ana")
	b := seedCard(t, store, col.ID, "b", "ana")
	c := seedCard(t, store, col.ID, "c", "ana")

	if err := store.MoveCard(ctx, c.ID, col.ID, 0); err != nil {
		t.Fatalf("MoveCard to front: %v", err)
	}
	if got := cardOrder(t, store, col.ID); len(got) != 3 ||
		got[0] != "c" || got[1] != "a" || got[2] != "b" {
		t.Fatalf("order after front move = %v, want [c a b]", got)
	}
	cards, err := store.ListCards(ctx, col.ID)
	if err != nil {
		t.Fatalf("ListCards: %v", err)
	}
	for i, card := range cards {
		if card.Position != i {
			t.Errorf("card %q position = %d, want %d (dense after move)", card.Body, card.Position, i)
		}
	}

	if err := store.MoveCard(ctx, a.ID, col.ID, 2); err != nil {
		t.Fatalf("MoveCard to back: %v", err)
	}
	if got := cardOrder(t, store, col.ID); len(got) != 3 ||
		got[0] != "c" || got[1] != "b" || got[2] != "a" {
		t.Errorf("order after back move = %v, want [c b a]", got)
	}
	_ = b
}

func TestMoveCardAcrossColumnsShiftsBothSides(t *testing.T) {
	store := NewStore(openTestDB(t))
	ctx := t.Context()
	board := seedBoard(t, store)
	left := seedColumn(t, store, board.ID, "left", 0)
	right := seedColumn(t, store, board.ID, "right", 1)
	a := seedCard(t, store, left.ID, "a", "ana")
	b := seedCard(t, store, left.ID, "b", "bo")
	x := seedCard(t, store, right.ID, "x", "cy")

	if err := store.MoveCard(ctx, b.ID, right.ID, 0); err != nil {
		t.Fatalf("MoveCard across: %v", err)
	}
	if got := cardOrder(t, store, left.ID); len(got) != 1 || got[0] != "a" {
		t.Errorf("source order = %v, want [a]", got)
	}
	if got := cardOrder(t, store, right.ID); len(got) != 2 || got[0] != "b" || got[1] != "x" {
		t.Errorf("destination order = %v, want [b x]", got)
	}
	moved, err := store.GetCard(ctx, b.ID)
	if err != nil {
		t.Fatalf("GetCard: %v", err)
	}
	if moved.ColumnID != right.ID || moved.Position != 0 {
		t.Errorf("moved card at column %d position %d, want %d/0",
			moved.ColumnID, moved.Position, right.ID)
	}
	if moved.Body != "b" || moved.AuthorName != "bo" {
		t.Errorf("move changed card content: %+v", moved)
	}
	leftCards, err := store.ListCards(ctx, left.ID)
	if err != nil {
		t.Fatalf("ListCards: %v", err)
	}
	if len(leftCards) != 1 || leftCards[0].ID != a.ID || leftCards[0].Position != 0 {
		t.Errorf("source not compacted: %+v", leftCards)
	}
	rightCards, err := store.ListCards(ctx, right.ID)
	if err != nil {
		t.Fatalf("ListCards: %v", err)
	}
	if len(rightCards) != 2 || rightCards[1].ID != x.ID || rightCards[1].Position != 1 {
		t.Errorf("destination not shifted: %+v", rightCards)
	}
}

func TestMoveCardRejects(t *testing.T) {
	store := NewStore(openTestDB(t))
	ctx := t.Context()
	board := seedBoard(t, store)
	otherBoard, err := store.CreateBoard(ctx, "Other", "", "other-token", 3)
	if err != nil {
		t.Fatalf("CreateBoard: %v", err)
	}
	left := seedColumn(t, store, board.ID, "left", 0)
	foreign := seedColumn(t, store, otherBoard.ID, "foreign", 0)
	card := seedCard(t, store, left.ID, "a", "ana")

	if err := store.MoveCard(ctx, 999999, left.ID, 0); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown card err = %v, want ErrNotFound", err)
	}
	if err := store.MoveCard(ctx, card.ID, 999999, 0); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown column err = %v, want ErrNotFound", err)
	}
	if err := store.MoveCard(ctx, card.ID, foreign.ID, 0); !errors.Is(err, ErrCrossColumnBoard) {
		t.Errorf("cross-board err = %v, want ErrCrossColumnBoard", err)
	}
	if err := store.MoveCard(ctx, card.ID, left.ID, -1); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("negative position err = %v, want ErrInvalidInput", err)
	}
}

func TestMoveCardClampsPositionToColumnBounds(t *testing.T) {
	store := NewStore(openTestDB(t))
	ctx := t.Context()
	board := seedBoard(t, store)
	left := seedColumn(t, store, board.ID, "left", 0)
	right := seedColumn(t, store, board.ID, "right", 1)
	a := seedCard(t, store, left.ID, "a", "ana")
	b := seedCard(t, store, left.ID, "b", "bo")
	c := seedCard(t, store, left.ID, "c", "cy")
	x := seedCard(t, store, right.ID, "x", "cy")

	// Past-the-end within one column lands last with dense positions.
	if err := store.MoveCard(ctx, a.ID, left.ID, 99); err != nil {
		t.Fatalf("MoveCard past the end: %v", err)
	}
	if got := cardOrder(t, store, left.ID); len(got) != 3 ||
		got[0] != "b" || got[1] != "c" || got[2] != "a" {
		t.Errorf("order after clamped move = %v, want [b c a]", got)
	}
	moved, err := store.GetCard(ctx, a.ID)
	if err != nil {
		t.Fatalf("GetCard: %v", err)
	}
	if moved.Position != 2 {
		t.Errorf("clamped position = %d, want 2 (last slot)", moved.Position)
	}
	assertDense(t, store, left.ID)

	// The exact end index appends without clamping.
	if err := store.MoveCard(ctx, b.ID, left.ID, 2); err != nil {
		t.Fatalf("MoveCard to end: %v", err)
	}
	if got := cardOrder(t, store, left.ID); len(got) != 3 ||
		got[0] != "c" || got[1] != "a" || got[2] != "b" {
		t.Errorf("order after end move = %v, want [c a b]", got)
	}
	assertDense(t, store, left.ID)

	// Past-the-end across columns appends to the destination and
	// compacts the source.
	if err := store.MoveCard(ctx, c.ID, right.ID, 99); err != nil {
		t.Fatalf("MoveCard across past the end: %v", err)
	}
	if got := cardOrder(t, store, right.ID); len(got) != 2 || got[0] != "x" || got[1] != "c" {
		t.Errorf("destination order = %v, want [x c]", got)
	}
	if got := cardOrder(t, store, left.ID); len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("source order = %v, want [a b]", got)
	}
	appended, err := store.GetCard(ctx, c.ID)
	if err != nil {
		t.Fatalf("GetCard: %v", err)
	}
	if appended.ColumnID != right.ID || appended.Position != 1 {
		t.Errorf("appended card at column %d position %d, want %d/1",
			appended.ColumnID, appended.Position, right.ID)
	}
	assertDense(t, store, left.ID)
	assertDense(t, store, right.ID)
	_ = x
}

func assertDense(t *testing.T, store *Store, columnID int64) {
	t.Helper()

	cards, err := store.ListCards(t.Context(), columnID)
	if err != nil {
		t.Fatalf("ListCards: %v", err)
	}
	for i, card := range cards {
		if card.Position != i {
			t.Errorf("card %q position = %d, want %d (dense after clamped move)",
				card.Body, card.Position, i)
		}
	}
}

func TestMoveCardExpectedMovesOnMatchingSlot(t *testing.T) {
	store := NewStore(openTestDB(t))
	ctx := t.Context()
	board := seedBoard(t, store)
	left := seedColumn(t, store, board.ID, "left", 0)
	right := seedColumn(t, store, board.ID, "right", 1)
	a := seedCard(t, store, left.ID, "a", "ana")
	seedCard(t, store, left.ID, "b", "ana")

	// The drop carries the slot seen at drag start; a matching
	// expectation moves exactly like an unconditional move.
	if err := store.MoveCardExpected(ctx, a.ID, right.ID, 0, left.ID, 0); err != nil {
		t.Fatalf("MoveCardExpected on match: %v", err)
	}
	moved, err := store.GetCard(ctx, a.ID)
	if err != nil {
		t.Fatalf("GetCard: %v", err)
	}
	if moved.ColumnID != right.ID || moved.Position != 0 {
		t.Errorf("stored card at column %d position %d, want %d/0", moved.ColumnID, moved.Position, right.ID)
	}
	assertDense(t, store, left.ID)
	assertDense(t, store, right.ID)
}

func TestMoveCardExpectedRejectsStaleSlot(t *testing.T) {
	store := NewStore(openTestDB(t))
	ctx := t.Context()
	board := seedBoard(t, store)
	left := seedColumn(t, store, board.ID, "left", 0)
	right := seedColumn(t, store, board.ID, "right", 1)
	a := seedCard(t, store, left.ID, "a", "ana")
	seedCard(t, store, left.ID, "b", "ana")

	// A remote move lands before the drop: the dragged slot is stale.
	if err := store.MoveCard(ctx, a.ID, right.ID, 0); err != nil {
		t.Fatalf("remote MoveCard: %v", err)
	}
	for _, tc := range []struct {
		name           string
		expectedColumn int64
		expectedPos    int
	}{
		{"stale column", left.ID, 1},
		{"stale position", right.ID, 1},
	} {
		err := store.MoveCardExpected(ctx, a.ID, left.ID, 0, tc.expectedColumn, tc.expectedPos)
		if !errors.Is(err, ErrStalePosition) {
			t.Errorf("%s: err = %v, want ErrStalePosition", tc.name, err)
		}
	}
	kept, err := store.GetCard(ctx, a.ID)
	if err != nil {
		t.Fatalf("GetCard: %v", err)
	}
	if kept.ColumnID != right.ID || kept.Position != 0 {
		t.Errorf("rejected drop moved the card to column %d position %d, want %d/0",
			kept.ColumnID, kept.Position, right.ID)
	}

	if err := store.MoveCardExpected(ctx, 999999, left.ID, 0, left.ID, 0); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown card err = %v, want ErrNotFound", err)
	}
}

func TestDeleteCardCompactsAndCascadesVotes(t *testing.T) {
	store := NewStore(openTestDB(t))
	ctx := t.Context()
	board := seedBoard(t, store)
	if _, err := store.SetPhase(ctx, board.ID, "vote"); err != nil {
		t.Fatalf("SetPhase vote: %v", err)
	}
	col := seedColumn(t, store, board.ID, "col", 0)
	a := seedCard(t, store, col.ID, "a", "ana")
	b := seedCard(t, store, col.ID, "b", "bo")
	_ = seedCard(t, store, col.ID, "c", "cy")
	voter := seedParticipant(t, store, board.ID, "voter")
	if err := store.Vote(ctx, voter.ID, b.ID); err != nil {
		t.Fatalf("Vote: %v", err)
	}

	deleted, affected, err := store.DeleteCard(ctx, b.ID)
	if err != nil {
		t.Fatalf("DeleteCard: %v", err)
	}
	if deleted.ID != b.ID || deleted.Body != "b" || deleted.BoardID != board.ID || deleted.ColumnID != col.ID {
		t.Errorf("deleted snapshot mismatch: %+v", deleted)
	}
	if len(affected) != 0 {
		t.Errorf("affected columns = %v, want none for a lone ungrouped card", affected)
	}
	if _, err := store.GetCard(ctx, b.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("deleted card still readable: %v", err)
	}
	if got := cardOrder(t, store, col.ID); len(got) != 2 || got[0] != "a" || got[1] != "c" {
		t.Errorf("order after delete = %v, want [a c]", got)
	}
	rest, err := store.ListCards(ctx, col.ID)
	if err != nil {
		t.Fatalf("ListCards: %v", err)
	}
	for i, card := range rest {
		if card.Position != i {
			t.Errorf("card %q position = %d, want %d (dense after delete)", card.Body, card.Position, i)
		}
	}
	if n := store.countVotesForTest(t, voter.ID); n != 0 {
		t.Errorf("vote survived its card: count = %d, want 0", n)
	}
	_ = a

	if _, _, err := store.DeleteCard(ctx, 999999); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown card err = %v, want ErrNotFound", err)
	}
}

func TestDeleteCardReportsGroupedColumns(t *testing.T) {
	store := NewStore(openTestDB(t))
	ctx := t.Context()
	board := seedBoard(t, store)
	left := seedColumn(t, store, board.ID, "left", 0)
	right := seedColumn(t, store, board.ID, "right", 1)
	leader := seedCard(t, store, left.ID, "leader", "ana")
	member := seedCard(t, store, left.ID, "member", "bo")
	if _, err := store.SetGroup(ctx, member.ID, &leader.ID); err != nil {
		t.Fatalf("SetGroup: %v", err)
	}

	// Deleting the member retotals the leader: its column refreshes.
	if _, affected, err := store.DeleteCard(ctx, member.ID); err != nil {
		t.Fatalf("DeleteCard member: %v", err)
	} else if len(affected) != 1 || affected[0] != left.ID {
		t.Errorf("affected = %v, want [%d]", affected, left.ID)
	}

	// A cross-column member promotes into its own column on leader
	// delete, so both columns refresh.
	other := seedCard(t, store, left.ID, "other", "cy")
	if _, err := store.SetGroup(ctx, other.ID, &leader.ID); err != nil {
		t.Fatalf("SetGroup other: %v", err)
	}
	if err := store.MoveCard(ctx, other.ID, right.ID, 0); err != nil {
		t.Fatalf("MoveCard other: %v", err)
	}
	if _, affected, err := store.DeleteCard(ctx, leader.ID); err != nil {
		t.Fatalf("DeleteCard leader: %v", err)
	} else if len(affected) != 2 || affected[0] != left.ID || affected[1] != right.ID {
		t.Errorf("affected = %v, want [%d %d] ascending", affected, left.ID, right.ID)
	}
	promoted, err := store.GetCard(ctx, other.ID)
	if err != nil {
		t.Fatalf("GetCard promoted: %v", err)
	}
	if promoted.GroupID != nil {
		t.Errorf("promoted member group_id = %v, want nil after leader delete", promoted.GroupID)
	}
}

func TestGetColumnAndBoardByID(t *testing.T) {
	store := NewStore(openTestDB(t))
	ctx := t.Context()
	board := seedBoard(t, store)
	col := seedColumn(t, store, board.ID, "col", 0)

	gotCol, err := store.GetColumn(ctx, col.ID)
	if err != nil {
		t.Fatalf("GetColumn: %v", err)
	}
	if gotCol.BoardID != board.ID || gotCol.Title != "col" {
		t.Errorf("column round trip mismatch: %+v", gotCol)
	}
	if _, err := store.GetColumn(ctx, 999999); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown column err = %v, want ErrNotFound", err)
	}

	gotBoard, err := store.GetBoardByID(ctx, board.ID)
	if err != nil {
		t.Fatalf("GetBoardByID: %v", err)
	}
	if gotBoard.PublicID != board.PublicID {
		t.Errorf("board round trip mismatch: %+v", gotBoard)
	}
	if _, err := store.GetBoardByID(ctx, 999999); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown board err = %v, want ErrNotFound", err)
	}
}

func TestVotedCardIDsTracksVotes(t *testing.T) {
	store := NewStore(openTestDB(t))
	ctx := t.Context()
	board := seedBoard(t, store)
	if _, err := store.SetPhase(ctx, board.ID, "vote"); err != nil {
		t.Fatalf("SetPhase vote: %v", err)
	}
	col := seedColumn(t, store, board.ID, "col", 0)
	first := seedCard(t, store, col.ID, "first", "ana")
	second := seedCard(t, store, col.ID, "second", "ana")
	voter := seedParticipant(t, store, board.ID, "bo")

	empty, err := store.VotedCardIDs(ctx, voter.ID)
	if err != nil {
		t.Fatalf("VotedCardIDs: %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("voted set = %v, want empty before any vote", empty)
	}

	if err := store.Vote(ctx, voter.ID, first.ID); err != nil {
		t.Fatalf("Vote: %v", err)
	}
	got, err := store.VotedCardIDs(ctx, voter.ID)
	if err != nil {
		t.Fatalf("VotedCardIDs: %v", err)
	}
	if !got[first.ID] || got[second.ID] || len(got) != 1 {
		t.Errorf("voted set = %v, want only card %d", got, first.ID)
	}

	if err := store.Unvote(ctx, voter.ID, first.ID); err != nil {
		t.Fatalf("Unvote: %v", err)
	}
	cleared, err := store.VotedCardIDs(ctx, voter.ID)
	if err != nil {
		t.Fatalf("VotedCardIDs: %v", err)
	}
	if len(cleared) != 0 {
		t.Errorf("voted set = %v, want empty after unvote", cleared)
	}
}
