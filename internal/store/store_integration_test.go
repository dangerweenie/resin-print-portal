package store_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/dangerweenie/resin-print-portal/internal/store"
)

// These tests need a real Postgres. Set TEST_DATABASE_URL to a disposable
// database (its schema is migrated fresh) to run them; otherwise they skip,
// the same way the old Flask suite's loop-device test skips without root.
func testStore(t *testing.T) *store.Store {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set TEST_DATABASE_URL to run store integration tests")
	}
	if err := store.Migrate(dsn, "reset"); err != nil {
		t.Logf("migrate reset (ignored): %v", err)
	}
	if err := store.Migrate(dsn, "up"); err != nil {
		t.Fatalf("migrate up: %v", err)
	}
	st, err := store.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(st.Close)
	return st
}

func TestRosterSyncAndResolve(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()

	res, err := st.SyncRoster(ctx, []store.RosterEntry{
		{ID: 1, Name: "Jane Doe", Code: "111", Status: "A"},
		{ID: 2, Name: "John Roe", Code: "222", Status: "I"},
		{ID: 3, Name: "Sam Poe", Code: "deadbeef", Status: "S"}, // fob UID, lowercase hex
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Received != 3 {
		t.Fatalf("received = %d", res.Received)
	}

	// Fallback name match resolves an active member.
	m, byName, err := st.ResolveSlackName(ctx, "jane doe")
	if err != nil || !byName || m.ID != 1 || !m.Active {
		t.Fatalf("resolve jane = %+v byName=%v err=%v", m, byName, err)
	}

	// Inactive member is resolvable but not Active.
	m, _, err = st.ResolveSlackName(ctx, "john roe")
	if err != nil || m.Active {
		t.Fatalf("john should resolve inactive, got %+v err=%v", m, err)
	}

	// Explicit slack identity wins and is reported as not-a-name-match.
	if err := st.AddSlackIdentity(ctx, 3, "sammy", "tester"); err != nil {
		t.Fatal(err)
	}
	m, byName, err = st.ResolveSlackName(ctx, "sammy")
	if err != nil || byName || m.ID != 3 {
		t.Fatalf("resolve sammy = %+v byName=%v err=%v", m, byName, err)
	}

	// Resolve by tapped fob UID — the Pi sends canonical uppercase hex; the
	// store matches against members.code in whatever form it was recorded.
	byFob, err := st.ResolveRFIDCode(ctx, "DE:AD:BE:EF") // colon form, upper
	if err != nil || byFob.ID != 3 {
		t.Fatalf("ResolveRFIDCode(DE:AD:BE:EF) = %+v err=%v, want member 3", byFob, err)
	}
	if m, err := st.ResolveRFIDCode(ctx, "DEADBEEF"); err != nil || m.ID != 3 {
		t.Fatalf("ResolveRFIDCode(DEADBEEF) = %+v err=%v, want member 3", m, err)
	}
	if _, err := st.ResolveRFIDCode(ctx, "00000000"); err == nil {
		t.Fatal("ResolveRFIDCode should ErrNotFound for an unknown UID")
	}
	if _, err := st.ResolveRFIDCode(ctx, "not-hex"); err == nil {
		t.Fatal("ResolveRFIDCode should ErrNotFound for a non-hex UID")
	}
	if _, err := st.ResolveRFIDCode(ctx, ""); err == nil {
		t.Fatal("ResolveRFIDCode should not match on an empty UID")
	}

	// A member dropping off the roster is deactivated, not deleted.
	res, err = st.SyncRoster(ctx, []store.RosterEntry{
		{ID: 1, Name: "Jane Doe", Code: "111", Status: "A"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Deactivated != 1 { // Sam (id 3, was 'S'/active) is now missing
		t.Fatalf("deactivated = %d, want 1", res.Deactivated)
	}
	got, err := st.GetMember(ctx, 3)
	if err != nil || got.Active || got.SourceMissingSince == nil {
		t.Fatalf("member 3 after drop = %+v err=%v", got, err)
	}
}

func TestCertificationAndJobFlow(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()

	if _, err := st.SyncRoster(ctx, []store.RosterEntry{{ID: 10, Name: "Ed Diamond", Status: "A"}}); err != nil {
		t.Fatal(err)
	}
	p, err := st.CreatePrinter(ctx, store.Printer{
		Slug: "resin", DisplayName: "Resin", Model: "Anycubic Photon Mono M7 Pro",
		AllowedExtensions: []string{".pwsz"}, SafetyChecklist: []string{"vat ok"},
		APIKeyHash: "deadbeef",
	})
	if err != nil {
		t.Fatal(err)
	}

	if ok, _ := st.IsCertified(ctx, 10, p.ID); ok {
		t.Fatal("should not be certified yet")
	}
	if err := st.Certify(ctx, 10, p.ID, "trainer"); err != nil {
		t.Fatal(err)
	}
	if err := st.Certify(ctx, 10, p.ID, "trainer"); err != nil {
		t.Fatalf("re-certify should be idempotent: %v", err)
	}
	if ok, _ := st.IsCertified(ctx, 10, p.ID); !ok {
		t.Fatal("should be certified now")
	}

	mid := int64(10)
	est := int32(3600)
	eta := time.Now().Add(time.Hour)
	j1, err := st.StartJob(ctx, store.PrintJob{
		PrinterID: p.ID, MemberID: &mid, SlackNameUsed: "ed", Filename: "a.pwsz",
		EstimatedSeconds: &est, ETAExact: true, EstimatedCompleteAt: &eta,
		ChecklistAnswers: []string{"vat ok"},
	})
	if err != nil {
		t.Fatal(err)
	}
	j2, err := st.StartJob(ctx, store.PrintJob{PrinterID: p.ID, MemberID: &mid, SlackNameUsed: "ed", Filename: "b.pwsz"})
	if err != nil {
		t.Fatal(err)
	}

	cur, err := st.CurrentJob(ctx, p.ID)
	if err != nil || cur.ID != j2.ID {
		t.Fatalf("current job = %+v err=%v, want id %d", cur, err, j2.ID)
	}
	old, err := st.GetJob(ctx, p.ID, j1.ID)
	if err != nil || old.Status != "ended" || old.EndReason == nil || *old.EndReason != "superseded" {
		t.Fatalf("superseded job = %+v err=%v", old, err)
	}

	ended, err := st.EndJob(ctx, j2.ID, "member_finished")
	if err != nil || !ended {
		t.Fatalf("EndJob = %v %v", ended, err)
	}
	if _, err := st.CurrentJob(ctx, p.ID); err == nil {
		t.Fatal("expected no current job after finish")
	}

	if err := st.Revoke(ctx, 10, p.ID); err != nil {
		t.Fatal(err)
	}
	if ok, _ := st.IsCertified(ctx, 10, p.ID); ok {
		t.Fatal("should be revoked")
	}
}

func TestEnrollPrinter(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()

	// New device -> pending printer, slug from hostname.
	p, isNew, err := st.EnrollPrinter(ctx, "pi-serial-1", "resin", DefaultChecklistForTest, "k1", "h1")
	if err != nil || !isNew {
		t.Fatalf("first enroll: new=%v err=%v", isNew, err)
	}
	if p.Slug != "resin" || p.Approved || p.DeviceID != "pi-serial-1" || p.EnrolledAt == nil {
		t.Fatalf("enrolled printer = %+v", p)
	}
	if len(p.SafetyChecklist) == 0 {
		t.Error("expected the default checklist to be seeded")
	}

	// A different device asking for the same hostname gets a deduped slug.
	p2, _, err := st.EnrollPrinter(ctx, "pi-serial-2", "resin", nil, "k2", "h2")
	if err != nil || p2.Slug != "resin-2" {
		t.Fatalf("second device slug = %q err=%v, want resin-2", p2.Slug, err)
	}

	// Re-enroll the first device (e.g. re-flash): same row, rotated key.
	again, isNew, err := st.EnrollPrinter(ctx, "pi-serial-1", "resin", nil, "k1b", "h1b")
	if err != nil || isNew || again.ID != p.ID {
		t.Fatalf("re-enroll: id=%d isNew=%v err=%v (want id %d, isNew false)", again.ID, isNew, err, p.ID)
	}
	if again.APIKeyHash != "h1b" {
		t.Errorf("re-enroll key hash = %q, want h1b (rotated)", again.APIKeyHash)
	}

	// Approve it.
	if err := st.SetPrinterApproved(ctx, p.ID, true); err != nil {
		t.Fatal(err)
	}
	got, _ := st.GetPrinterBySlug(ctx, "resin")
	if !got.Approved {
		t.Error("printer should be approved now")
	}

	// last_seen touch, throttled but works on first call.
	if err := st.TouchPrinterSeen(ctx, p.ID); err != nil {
		t.Fatal(err)
	}
	got, _ = st.GetPrinterBySlug(ctx, "resin")
	if got.LastSeenAt == nil {
		t.Error("last_seen_at should be set after TouchPrinterSeen")
	}

	// Pending printers can be deleted; ones with history cannot (FK) — not
	// exercised here since that needs a job row.
	if err := st.DeletePrinter(ctx, p2.ID); err != nil {
		t.Fatalf("delete pending printer: %v", err)
	}
}

var DefaultChecklistForTest = []string{"check the vat", "check the plate"}
