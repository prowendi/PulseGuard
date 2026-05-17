package store

import (
	"context"
	"errors"
	"testing"

	"github.com/prowendi/PulseGuard/internal/domain"
)

// V7-2 MessageThreadRepo tests. The fixture chain
// (tenant → bot → template → channel) is shared with bot_template_channel
// because the message_threads FK references channels(id) with ON DELETE
// CASCADE — every row needs a real owning channel.

func TestMessageThreadRepo_UpsertInsertsThenUpdates(t *testing.T) {
	f := newResourceFixture(t)
	b := f.makeBot(t, "bot", "1:t")
	tpl := f.makeTemplate(t, "tpl")
	ch := f.makeChannel(t, "ch", "tok-mt-1", b.ID, tpl.ID)
	repo := NewMessageThreadRepo(f.db, f.clk)

	first := &domain.MessageThread{
		ChannelID:   ch.ID,
		TenantID:    f.tenant.ID,
		Fingerprint: "db01-cpu",
		ChatID:      "12345",
		TGMessageID: 1001,
	}
	if err := repo.Upsert(context.Background(), first); err != nil {
		t.Fatalf("Upsert insert: %v", err)
	}
	if first.ID == 0 {
		t.Fatalf("Upsert did not assign ID")
	}
	insertedID := first.ID

	// Second Upsert with same (channel, fingerprint) should update,
	// not duplicate: ID stays, tg_message_id swaps.
	second := &domain.MessageThread{
		ChannelID:   ch.ID,
		TenantID:    f.tenant.ID,
		Fingerprint: "db01-cpu",
		ChatID:      "12345",
		TGMessageID: 1002,
	}
	if err := repo.Upsert(context.Background(), second); err != nil {
		t.Fatalf("Upsert update: %v", err)
	}

	got, err := repo.GetByFingerprint(context.Background(), ch.ID, "db01-cpu")
	if err != nil {
		t.Fatalf("GetByFingerprint: %v", err)
	}
	if got.ID != insertedID {
		t.Fatalf("ID changed on upsert: was %d now %d (should match)", insertedID, got.ID)
	}
	if got.TGMessageID != 1002 {
		t.Fatalf("tg_message_id = %d want 1002 (latest upsert)", got.TGMessageID)
	}
}

func TestMessageThreadRepo_GetByFingerprintNotFound(t *testing.T) {
	f := newResourceFixture(t)
	b := f.makeBot(t, "bot", "1:t")
	tpl := f.makeTemplate(t, "tpl")
	ch := f.makeChannel(t, "ch", "tok-mt-nf", b.ID, tpl.ID)
	repo := NewMessageThreadRepo(f.db, f.clk)

	_, err := repo.GetByFingerprint(context.Background(), ch.ID, "missing")
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestMessageThreadRepo_UpsertValidationErrors(t *testing.T) {
	f := newResourceFixture(t)
	b := f.makeBot(t, "bot", "1:t")
	tpl := f.makeTemplate(t, "tpl")
	ch := f.makeChannel(t, "ch", "tok-mt-val", b.ID, tpl.ID)
	repo := NewMessageThreadRepo(f.db, f.clk)
	cases := map[string]*domain.MessageThread{
		"zero channel":  {ChannelID: 0, TenantID: f.tenant.ID, Fingerprint: "x", ChatID: "1", TGMessageID: 1},
		"zero tenant":   {ChannelID: ch.ID, TenantID: 0, Fingerprint: "x", ChatID: "1", TGMessageID: 1},
		"empty fp":      {ChannelID: ch.ID, TenantID: f.tenant.ID, Fingerprint: " ", ChatID: "1", TGMessageID: 1},
		"empty chat":    {ChannelID: ch.ID, TenantID: f.tenant.ID, Fingerprint: "x", ChatID: "", TGMessageID: 1},
		"zero msg_id":   {ChannelID: ch.ID, TenantID: f.tenant.ID, Fingerprint: "x", ChatID: "1", TGMessageID: 0},
	}
	for name, m := range cases {
		t.Run(name, func(t *testing.T) {
			if err := repo.Upsert(context.Background(), m); err == nil {
				t.Fatalf("expected validation error for %s", name)
			}
		})
	}
}

func TestMessageThreadRepo_DeleteByChannel(t *testing.T) {
	f := newResourceFixture(t)
	b := f.makeBot(t, "bot", "1:t")
	tpl := f.makeTemplate(t, "tpl")
	ch := f.makeChannel(t, "ch", "tok-mt-del", b.ID, tpl.ID)
	repo := NewMessageThreadRepo(f.db, f.clk)
	for _, fp := range []string{"a", "b", "c"} {
		if err := repo.Upsert(context.Background(), &domain.MessageThread{
			ChannelID: ch.ID, TenantID: f.tenant.ID,
			Fingerprint: fp, ChatID: "1", TGMessageID: 1,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := repo.DeleteByChannel(context.Background(), f.tenant.ID, ch.ID); err != nil {
		t.Fatalf("DeleteByChannel: %v", err)
	}
	if _, err := repo.GetByFingerprint(context.Background(), ch.ID, "a"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("after delete, err = %v want ErrNotFound", err)
	}
}

func TestMessageThreadRepo_GetByFingerprintEmptyInputs(t *testing.T) {
	f := newResourceFixture(t)
	repo := NewMessageThreadRepo(f.db, f.clk)
	// channelID==0 short-circuits to ErrNotFound (validation-style).
	if _, err := repo.GetByFingerprint(context.Background(), 0, "x"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("zero channel err = %v want ErrNotFound", err)
	}
	if _, err := repo.GetByFingerprint(context.Background(), 1, "  "); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("blank fp err = %v want ErrNotFound", err)
	}
}
