package db

import (
	"errors"
	"strings"
	"testing"
)

func seedBoardCard(t *testing.T, store *Store, boardID int64, body string) *Card {
	t.Helper()

	col := seedColumn(t, store, boardID, "col", 0)
	return seedCard(t, store, col.ID, body, "ana")
}

func TestCreateCommentStoresBodyAndAuthor(t *testing.T) {
	store := NewStore(openTestDB(t))
	ctx := t.Context()
	board := seedBoard(t, store)
	card := seedBoardCard(t, store, board.ID, "a card")

	got, err := store.CreateComment(ctx, card.ID, "nice point", "bo")
	if err != nil {
		t.Fatalf("CreateComment: %v", err)
	}
	if got.CardID != card.ID || got.BoardID != board.ID {
		t.Errorf("comment linkage = card %d board %d, want card %d board %d",
			got.CardID, got.BoardID, card.ID, board.ID)
	}
	if got.Body != "nice point" || got.AuthorName != "bo" {
		t.Errorf("comment = %+v, want body %q by %q", got, "nice point", "bo")
	}

	listed, err := store.ListComments(ctx, card.ID)
	if err != nil {
		t.Fatalf("ListComments: %v", err)
	}
	if len(listed) != 1 || listed[0].ID != got.ID {
		t.Fatalf("listed comments = %+v, want the single stored row", listed)
	}
}

func TestCreateCommentValidation(t *testing.T) {
	store := NewStore(openTestDB(t))
	ctx := t.Context()
	board := seedBoard(t, store)
	card := seedBoardCard(t, store, board.ID, "a card")

	if _, err := store.CreateComment(ctx, card.ID, "   ", "bo"); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("blank comment err = %v, want ErrInvalidInput", err)
	}
	long := strings.Repeat("x", 1001)
	if _, err := store.CreateComment(ctx, card.ID, long, "bo"); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("over-length comment err = %v, want ErrInvalidInput", err)
	}
	if _, err := store.CreateComment(ctx, 9999, "valid", "bo"); !errors.Is(err, ErrNotFound) {
		t.Errorf("comment on unknown card err = %v, want ErrNotFound", err)
	}
}

func TestListCommentsByBoardGroupsPerCard(t *testing.T) {
	store := NewStore(openTestDB(t))
	ctx := t.Context()
	board := seedBoard(t, store)
	first := seedBoardCard(t, store, board.ID, "first")
	second := seedBoardCard(t, store, board.ID, "second")

	if _, err := store.CreateComment(ctx, first.ID, "one", "bo"); err != nil {
		t.Fatalf("CreateComment: %v", err)
	}
	if _, err := store.CreateComment(ctx, first.ID, "two", "ana"); err != nil {
		t.Fatalf("CreateComment: %v", err)
	}
	if _, err := store.CreateComment(ctx, second.ID, "three", "bo"); err != nil {
		t.Fatalf("CreateComment: %v", err)
	}

	grouped, err := store.ListCommentsByBoard(ctx, board.ID)
	if err != nil {
		t.Fatalf("ListCommentsByBoard: %v", err)
	}
	if len(grouped[first.ID]) != 2 {
		t.Errorf("first card comments = %d, want 2", len(grouped[first.ID]))
	}
	if len(grouped[second.ID]) != 1 {
		t.Errorf("second card comments = %d, want 1", len(grouped[second.ID]))
	}
	if got := grouped[first.ID]; len(got) == 2 && (got[0].Body != "one" || got[1].Body != "two") {
		t.Errorf("comment order = %q, %q, want insertion order", got[0].Body, got[1].Body)
	}
}

func TestCreateKudoStoresWallEntry(t *testing.T) {
	store := NewStore(openTestDB(t))
	ctx := t.Context()
	board := seedBoard(t, store)

	got, err := store.CreateKudo(ctx, board.ID, "ana", "shipped the fix", "bo")
	if err != nil {
		t.Fatalf("CreateKudo: %v", err)
	}
	if got.BoardID != board.ID || got.To != "ana" || got.Body != "shipped the fix" || got.From != "bo" {
		t.Errorf("kudo = %+v, want the stored entry", got)
	}

	listed, err := store.ListKudos(ctx, board.ID)
	if err != nil {
		t.Fatalf("ListKudos: %v", err)
	}
	if len(listed) != 1 || listed[0].ID != got.ID {
		t.Fatalf("listed kudos = %+v, want the single stored row", listed)
	}
}

func TestCreateKudoValidation(t *testing.T) {
	store := NewStore(openTestDB(t))
	ctx := t.Context()
	board := seedBoard(t, store)

	if _, err := store.CreateKudo(ctx, board.ID, "   ", "body", "bo"); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("blank recipient err = %v, want ErrInvalidInput", err)
	}
	if _, err := store.CreateKudo(ctx, board.ID, "ana", "   ", "bo"); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("blank kudo body err = %v, want ErrInvalidInput", err)
	}
	long := strings.Repeat("x", 501)
	if _, err := store.CreateKudo(ctx, board.ID, "ana", long, "bo"); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("over-length kudo err = %v, want ErrInvalidInput", err)
	}
	if _, err := store.CreateKudo(ctx, 9999, "ana", "body", "bo"); !errors.Is(err, ErrNotFound) {
		t.Errorf("kudo on unknown board err = %v, want ErrNotFound", err)
	}
}

func TestDeleteKudoRemovesRow(t *testing.T) {
	store := NewStore(openTestDB(t))
	ctx := t.Context()
	board := seedBoard(t, store)
	kudo, err := store.CreateKudo(ctx, board.ID, "ana", "thanks", "bo")
	if err != nil {
		t.Fatalf("CreateKudo: %v", err)
	}

	deleted, err := store.DeleteKudo(ctx, kudo.ID)
	if err != nil {
		t.Fatalf("DeleteKudo: %v", err)
	}
	if deleted.ID != kudo.ID || deleted.BoardID != board.ID {
		t.Errorf("deleted snapshot = %+v, want id %d on board %d", deleted, kudo.ID, board.ID)
	}
	if _, err := store.GetKudo(ctx, kudo.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetKudo after delete err = %v, want ErrNotFound", err)
	}
	if _, err := store.DeleteKudo(ctx, kudo.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("second DeleteKudo err = %v, want ErrNotFound", err)
	}
}

func TestGetAndUpdateActionRoundTrip(t *testing.T) {
	store := NewStore(openTestDB(t))
	ctx := t.Context()
	board := seedBoard(t, store)
	created, err := store.CreateAction(ctx, board.ID, "follow up", "ana", "bo", nil)
	if err != nil {
		t.Fatalf("CreateAction: %v", err)
	}
	if created.AuthorName != "bo" {
		t.Errorf("action author = %q, want bo", created.AuthorName)
	}

	got, err := store.GetAction(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetAction: %v", err)
	}
	if got.Text != "follow up" || got.Owner != "ana" || got.Done {
		t.Errorf("fetched action = %+v, want the stored row", got)
	}

	text, owner, done := "follow up soon", "cy", true
	updated, err := store.UpdateAction(ctx, created.ID, &text, &owner, &done)
	if err != nil {
		t.Fatalf("UpdateAction: %v", err)
	}
	if updated.Text != text || updated.Owner != owner || !updated.Done {
		t.Errorf("updated action = %+v, want all three fields applied", updated)
	}

	reopened, err := store.UpdateAction(ctx, created.ID, nil, nil, &[]bool{false}[0])
	if err != nil {
		t.Fatalf("UpdateAction reopen: %v", err)
	}
	if reopened.Done || reopened.Text != text || reopened.Owner != owner {
		t.Errorf("reopened action = %+v, want only done flipped", reopened)
	}

	if _, err := store.GetAction(ctx, 9999); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetAction unknown err = %v, want ErrNotFound", err)
	}
	blank := "   "
	if _, err := store.UpdateAction(ctx, created.ID, &blank, nil, nil); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("blank text err = %v, want ErrInvalidInput", err)
	}
	if _, err := store.UpdateAction(ctx, created.ID, &[]string{strings.Repeat("x", 501)}[0], nil, nil); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("over-length text err = %v, want ErrInvalidInput", err)
	}
	longOwner := strings.Repeat("x", 101)
	if _, err := store.UpdateAction(ctx, created.ID, nil, &longOwner, nil); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("over-length owner err = %v, want ErrInvalidInput", err)
	}
	if _, err := store.UpdateAction(ctx, 9999, &text, nil, nil); !errors.Is(err, ErrNotFound) {
		t.Errorf("update unknown err = %v, want ErrNotFound", err)
	}
}

func TestListActionsShowsOpenAndDone(t *testing.T) {
	store := NewStore(openTestDB(t))
	ctx := t.Context()
	board := seedBoard(t, store)
	open1, open2, done := seedActions(t, store, board.ID)

	got, err := store.ListActions(ctx, board.ID)
	if err != nil {
		t.Fatalf("ListActions: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("listed actions = %d, want 3 (open plus done)", len(got))
	}
	byID := map[int64]Action{}
	for _, a := range got {
		byID[a.ID] = a
	}
	for _, want := range []Action{*open1, *open2, *done} {
		if _, ok := byID[want.ID]; !ok {
			t.Errorf("listed actions missing id %d", want.ID)
		}
	}
}

func TestDeleteActionRemovesRow(t *testing.T) {
	store := NewStore(openTestDB(t))
	ctx := t.Context()
	board := seedBoard(t, store)
	action, err := store.CreateAction(ctx, board.ID, "doomed", "", "ana", nil)
	if err != nil {
		t.Fatalf("CreateAction: %v", err)
	}

	deleted, err := store.DeleteAction(ctx, action.ID)
	if err != nil {
		t.Fatalf("DeleteAction: %v", err)
	}
	if deleted.ID != action.ID || deleted.BoardID != board.ID {
		t.Errorf("deleted snapshot = %+v, want id %d on board %d", deleted, action.ID, board.ID)
	}
	if _, err := store.GetAction(ctx, action.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetAction after delete err = %v, want ErrNotFound", err)
	}
	if _, err := store.DeleteAction(ctx, action.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("second DeleteAction err = %v, want ErrNotFound", err)
	}
}

func TestDuplicateCarryKeepsActionAuthor(t *testing.T) {
	store := NewStore(openTestDB(t))
	ctx := t.Context()
	src := seedBoard(t, store)
	seedColumn(t, store, src.ID, "col", 0)
	open, err := store.CreateAction(ctx, src.ID, "carry me", "ana", "bo", nil)
	if err != nil {
		t.Fatalf("CreateAction: %v", err)
	}

	dup, err := store.DuplicateBoard(ctx, src.ID, "another-token", true)
	if err != nil {
		t.Fatalf("DuplicateBoard with carry: %v", err)
	}
	carried, err := store.ListOpenActions(ctx, dup.ID)
	if err != nil {
		t.Fatalf("ListOpenActions: %v", err)
	}
	if len(carried) != 1 {
		t.Fatalf("carried actions = %d, want 1", len(carried))
	}
	if carried[0].CarriedFrom == nil || *carried[0].CarriedFrom != open.ID {
		t.Errorf("carried_from = %v, want %d", carried[0].CarriedFrom, open.ID)
	}
	if carried[0].AuthorName != "bo" {
		t.Errorf("carried author = %q, want bo", carried[0].AuthorName)
	}
}
