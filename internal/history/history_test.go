package history

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "history.db")
	s, err := NewStore(dbPath)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func addRecord(t *testing.T, s *Store, brokerID string, status Status, sentAt time.Time) {
	t.Helper()
	rec := &Record{
		BrokerID:   brokerID,
		BrokerName: brokerID,
		Email:      brokerID + "@example.com",
		Template:   "gdpr",
		Status:     status,
		SentAt:     sentAt,
	}
	if err := s.Add(rec); err != nil {
		t.Fatalf("Add: %v", err)
	}
}

func TestCountSentSince(t *testing.T) {
	s := newTestStore(t)
	now := time.Now()

	addRecord(t, s, "broker-a", StatusSent, now.Add(-1*time.Hour))
	addRecord(t, s, "broker-b", StatusSent, now.Add(-30*time.Hour)) // outside 24h window
	addRecord(t, s, "broker-c", StatusFailed, now.Add(-1*time.Hour))

	count, err := s.CountSentSince("", now.Add(-24*time.Hour))
	if err != nil {
		t.Fatalf("CountSentSince: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1 sent in last 24h, got %d", count)
	}
}

func TestLastSuccessfulSendTimes(t *testing.T) {
	s := newTestStore(t)
	now := time.Now()

	// Two sends for the same broker - should return the more recent one.
	addRecord(t, s, "broker-a", StatusSent, now.Add(-40*24*time.Hour))
	addRecord(t, s, "broker-a", StatusSent, now.Add(-2*24*time.Hour))
	addRecord(t, s, "broker-b", StatusFailed, now.Add(-1*time.Hour))

	times, err := s.LastSuccessfulSendTimes("")
	if err != nil {
		t.Fatalf("LastSuccessfulSendTimes: %v", err)
	}

	if _, ok := times[SendKey{BrokerID: "broker-b", RequestType: RequestErasure}]; ok {
		t.Errorf("broker-b never had a successful send, should not appear")
	}

	got, ok := times[SendKey{BrokerID: "broker-a", RequestType: RequestErasure}]
	if !ok {
		t.Fatalf("expected broker-a in results")
	}
	wantApprox := now.Add(-2 * 24 * time.Hour)
	if got.Before(wantApprox.Add(-time.Minute)) || got.After(wantApprox.Add(time.Minute)) {
		t.Errorf("expected broker-a's last send near %v, got %v", wantApprox, got)
	}
}

// The resend cooldown is keyed on (broker, request type) so that asking a
// broker what they hold and asking them to erase it are separate sends. If
// this collapsed back to a broker-only key, sending an Article 15 access
// request would silently suppress the Article 17 erasure request to the same
// broker for the whole cooldown window (and vice versa).
func TestLastSuccessfulSendTimesSeparatesRequestTypes(t *testing.T) {
	s := newTestStore(t)
	now := time.Now()

	addRecordOfType(t, s, "broker-a", RequestAccess, StatusSent, now.Add(-3*24*time.Hour))

	times, err := s.LastSuccessfulSendTimes("")
	if err != nil {
		t.Fatalf("LastSuccessfulSendTimes: %v", err)
	}

	if _, ok := times[SendKey{BrokerID: "broker-a", RequestType: RequestAccess}]; !ok {
		t.Error("the access request that was sent should be recorded under the access key")
	}
	if _, ok := times[SendKey{BrokerID: "broker-a", RequestType: RequestErasure}]; ok {
		t.Error("an access request must not mark the erasure request as already sent - " +
			"that would suppress the erasure send for the whole cooldown window")
	}

	// And the reverse: an erasure send must not suppress a later access request.
	addRecordOfType(t, s, "broker-b", RequestErasure, StatusSent, now.Add(-3*24*time.Hour))
	times, err = s.LastSuccessfulSendTimes("")
	if err != nil {
		t.Fatalf("LastSuccessfulSendTimes: %v", err)
	}
	if _, ok := times[SendKey{BrokerID: "broker-b", RequestType: RequestAccess}]; ok {
		t.Error("an erasure request must not mark the access request as already sent")
	}
}

// Records written before the request_type column existed were all erasure
// requests; they must keep behaving that way rather than reading as a blank
// type that matches nothing.
func TestAddDefaultsRequestTypeToErasure(t *testing.T) {
	s := newTestStore(t)
	addRecord(t, s, "broker-a", StatusSent, time.Now().Add(-time.Hour)) // no RequestType set

	times, err := s.LastSuccessfulSendTimes("")
	if err != nil {
		t.Fatalf("LastSuccessfulSendTimes: %v", err)
	}
	if _, ok := times[SendKey{BrokerID: "broker-a", RequestType: RequestErasure}]; !ok {
		t.Error("a record added with no RequestType should be stored as an erasure request")
	}

	recs, err := s.GetRecentRequests("", 10)
	if err != nil {
		t.Fatalf("GetRecentRequests: %v", err)
	}
	if len(recs) != 1 || recs[0].RequestType != RequestErasure {
		t.Errorf("expected one record with RequestType=%q, got %+v", RequestErasure, recs)
	}
}

func addRecordOfType(t *testing.T, s *Store, brokerID, requestType string, status Status, sentAt time.Time) {
	t.Helper()
	rec := &Record{
		BrokerID:    brokerID,
		BrokerName:  brokerID,
		Email:       brokerID + "@example.com",
		Template:    "uk-access",
		RequestType: requestType,
		Status:      status,
		SentAt:      sentAt,
	}
	if err := s.Add(rec); err != nil {
		t.Fatalf("Add: %v", err)
	}
}

// TestGetAllBrokerStatusesScansAggregateTime guards against a regression
// where GetAllBrokerStatuses scanned its MAX(sent_at) column into
// sql.NullTime directly. Aggregate functions lose the driver's native
// time.Time conversion (confirmed against modernc.org/sqlite - a raw
// `sent_at` column scans fine, but `MAX(sent_at)` comes back as a plain
// string), so that scan errored on every call, and the error was silently
// discarded by getBrokersWithStatus's `brokerStatuses, _ =
// s.historyStore.GetAllBrokerStatuses(...)`, making every broker show as
// "never sent" on the web UI's Brokers page regardless of real history.
// LastSuccessfulSendTimes already had the right fix (parseSQLiteTime) -
// this test would have caught that GetAllBrokerStatuses didn't.
func TestGetAllBrokerStatusesScansAggregateTime(t *testing.T) {
	s := newTestStore(t)
	now := time.Now()

	addRecord(t, s, "broker-a", StatusSent, now.Add(-1*time.Hour))
	addRecord(t, s, "broker-b", StatusFailed, now.Add(-2*time.Hour))

	statuses, err := s.GetAllBrokerStatuses("")
	if err != nil {
		t.Fatalf("GetAllBrokerStatuses: %v", err)
	}

	a, ok := statuses["broker-a"]
	if !ok {
		t.Fatalf("expected broker-a in results")
	}
	if a.Status != StatusSent {
		t.Errorf("broker-a: expected status %q, got %q", StatusSent, a.Status)
	}
	if a.LastSent.IsZero() {
		t.Errorf("broker-a: expected a non-zero LastSent, got zero value")
	}
	wantApprox := now.Add(-1 * time.Hour)
	if a.LastSent.Before(wantApprox.Add(-time.Minute)) || a.LastSent.After(wantApprox.Add(time.Minute)) {
		t.Errorf("broker-a: expected LastSent near %v, got %v", wantApprox, a.LastSent)
	}

	b, ok := statuses["broker-b"]
	if !ok {
		t.Fatalf("expected broker-b in results")
	}
	if b.Status != StatusFailed {
		t.Errorf("broker-b: expected status %q, got %q", StatusFailed, b.Status)
	}
}

func TestMarkFailedRemovesFromResendCooldown(t *testing.T) {
	s := newTestStore(t)
	now := time.Now()

	addRecord(t, s, "broker-a", StatusSent, now.Add(-1*time.Hour))

	n, err := s.MarkFailed("", "broker-a", "bounced - manually confirmed")
	if err != nil {
		t.Fatalf("MarkFailed: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 row updated, got %d", n)
	}

	times, err := s.LastSuccessfulSendTimes("")
	if err != nil {
		t.Fatalf("LastSuccessfulSendTimes: %v", err)
	}
	if _, ok := times[SendKey{BrokerID: "broker-a", RequestType: RequestErasure}]; ok {
		t.Errorf("broker-a was just marked failed, should no longer count as a successful send")
	}
}

func TestMarkFailedNoRecordReturnsZero(t *testing.T) {
	s := newTestStore(t)

	n, err := s.MarkFailed("", "never-sent", "bounced")
	if err != nil {
		t.Fatalf("MarkFailed: %v", err)
	}
	if n != 0 {
		t.Errorf("expected 0 rows updated for a broker with no sent record, got %d", n)
	}
}

func TestMarkFailedOnlyTouchesMostRecentSentRecord(t *testing.T) {
	s := newTestStore(t)
	now := time.Now()

	addRecord(t, s, "broker-a", StatusSent, now.Add(-40*24*time.Hour))
	addRecord(t, s, "broker-a", StatusSent, now.Add(-1*time.Hour))

	if _, err := s.MarkFailed("", "broker-a", "bounced"); err != nil {
		t.Fatalf("MarkFailed: %v", err)
	}

	records, err := s.GetRecentRequests("", 10)
	if err != nil {
		t.Fatalf("GetRecentRequests: %v", err)
	}
	sentCount, failedCount := 0, 0
	for _, r := range records {
		if r.BrokerID != "broker-a" {
			continue
		}
		switch r.Status {
		case StatusSent:
			sentCount++
		case StatusFailed:
			failedCount++
		}
	}
	if sentCount != 1 || failedCount != 1 {
		t.Errorf("expected exactly one older 'sent' record to survive and one 'failed', got sent=%d failed=%d", sentCount, failedCount)
	}
}

// Regression coverage for digisamroc/eraser#3: broker_responses.email_body
// was referenced by AddBrokerResponse's INSERT (and UpdateBrokerResponseBody,
// GetAllBrokerResponses, GetBrokerResponses) but missing from the CREATE
// TABLE, so `eraser monitor` failed on every classified reply with "table
// broker_responses has no column named email_body" on any database created
// before the migrate() fix. This exercises the exact path that broke.
func TestAddBrokerResponse_EmailBodyColumn(t *testing.T) {
	s := newTestStore(t)

	resp := &BrokerResponse{
		BrokerID:     "broker-a",
		BrokerName:   "Broker A",
		ResponseType: "pending",
		EmailFrom:    "privacy@broker-a.example.com",
		EmailSubject: "Re: Personal Data Removal Request",
		EmailBody:    "We have received your request and will respond within 30 days.",
		Confidence:   0.9,
	}
	if err := s.AddBrokerResponse(resp); err != nil {
		t.Fatalf("AddBrokerResponse: %v", err)
	}
	if resp.ID == 0 {
		t.Fatal("AddBrokerResponse did not set an ID")
	}

	all, err := s.GetAllBrokerResponses()
	if err != nil {
		t.Fatalf("GetAllBrokerResponses: %v", err)
	}
	if len(all) != 1 || all[0].EmailBody != resp.EmailBody {
		t.Fatalf("expected the stored email_body to round-trip, got %+v", all)
	}

	if err := s.UpdateBrokerResponseBody(resp.ID, resp.ProfileID, "updated body"); err != nil {
		t.Fatalf("UpdateBrokerResponseBody: %v", err)
	}
}

// ==================== Multi-Profile Tests ====================

func addRecordForProfile(t *testing.T, s *Store, profileID, brokerID string, status Status, sentAt time.Time) {
	t.Helper()
	rec := &Record{
		ProfileID:  profileID,
		BrokerID:   brokerID,
		BrokerName: brokerID,
		Email:      brokerID + "@example.com",
		Template:   "gdpr",
		Status:     status,
		SentAt:     sentAt,
	}
	if err := s.Add(rec); err != nil {
		t.Fatalf("Add: %v", err)
	}
}

func TestProfileIsolation_RecentRequestsAndStats(t *testing.T) {
	s := newTestStore(t)
	now := time.Now()

	addRecordForProfile(t, s, "maris", "broker-a", StatusSent, now)
	addRecordForProfile(t, s, "maris", "broker-b", StatusSent, now)
	addRecordForProfile(t, s, "spouse", "broker-a", StatusSent, now)

	marisRecords, err := s.GetRecentRequests("maris", 10)
	if err != nil {
		t.Fatalf("GetRecentRequests(maris): %v", err)
	}
	if len(marisRecords) != 2 {
		t.Errorf("expected 2 records for maris, got %d", len(marisRecords))
	}

	spouseRecords, err := s.GetRecentRequests("spouse", 10)
	if err != nil {
		t.Fatalf("GetRecentRequests(spouse): %v", err)
	}
	if len(spouseRecords) != 1 {
		t.Errorf("expected 1 record for spouse, got %d", len(spouseRecords))
	}

	total, sent, _, err := s.GetStats("maris")
	if err != nil {
		t.Fatalf("GetStats(maris): %v", err)
	}
	if total != 2 || sent != 2 {
		t.Errorf("expected maris stats total=2 sent=2, got total=%d sent=%d", total, sent)
	}

	total, _, _, err = s.GetStats("spouse")
	if err != nil {
		t.Fatalf("GetStats(spouse): %v", err)
	}
	if total != 1 {
		t.Errorf("expected spouse stats total=1, got total=%d", total)
	}
}

func TestProfileIsolation_EmptyProfileIDDefaultsConsistently(t *testing.T) {
	s := newTestStore(t)
	now := time.Now()

	// A record added with no ProfileID (as every pre-multi-profile caller
	// does) should be readable back under both "" and DefaultProfileID -
	// normalizeProfileID has to make those equivalent everywhere.
	addRecordForProfile(t, s, "", "broker-a", StatusSent, now)

	byEmpty, err := s.GetRecentRequests("", 10)
	if err != nil {
		t.Fatalf("GetRecentRequests(\"\"): %v", err)
	}
	if len(byEmpty) != 1 {
		t.Fatalf("expected 1 record via empty profileID, got %d", len(byEmpty))
	}
	if byEmpty[0].ProfileID != DefaultProfileID {
		t.Errorf("expected stored record to be normalized to %q, got %q", DefaultProfileID, byEmpty[0].ProfileID)
	}

	byDefault, err := s.GetRecentRequests(DefaultProfileID, 10)
	if err != nil {
		t.Fatalf("GetRecentRequests(DefaultProfileID): %v", err)
	}
	if len(byDefault) != 1 {
		t.Errorf("expected 1 record via DefaultProfileID, got %d", len(byDefault))
	}
}

func TestMigrationBackfillsExistingRowsToDefaultProfile(t *testing.T) {
	// Simulate a pre-multi-profile database: create the removal_requests
	// table without a profile_id column (as it existed before this
	// feature), insert a row the old way, then run migrate() and confirm
	// the ALTER TABLE ... DEFAULT backfill makes the row show up under
	// DefaultProfileID rather than vanishing behind the new filter.
	dbPath := filepath.Join(t.TempDir(), "legacy.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	_, err = db.Exec(`
		CREATE TABLE removal_requests (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			broker_id TEXT NOT NULL,
			broker_name TEXT NOT NULL,
			email TEXT NOT NULL,
			template TEXT NOT NULL,
			status TEXT NOT NULL,
			message_id TEXT,
			error TEXT,
			sent_at DATETIME,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP
		)`)
	if err != nil {
		t.Fatalf("create legacy table: %v", err)
	}
	_, err = db.Exec(`INSERT INTO removal_requests (broker_id, broker_name, email, template, status, sent_at)
		VALUES ('legacy-broker', 'Legacy Broker', 'legacy@example.com', 'gdpr', 'sent', ?)`, time.Now())
	if err != nil {
		t.Fatalf("insert legacy row: %v", err)
	}
	_ = db.Close()

	// NewStore runs migrate() on open, which should ALTER TABLE ADD COLUMN
	// profile_id with the DefaultProfileID default, backfilling this row.
	s, err := NewStore(dbPath)
	if err != nil {
		t.Fatalf("NewStore on legacy db: %v", err)
	}
	defer func() { _ = s.Close() }()

	records, err := s.GetRecentRequests(DefaultProfileID, 10)
	if err != nil {
		t.Fatalf("GetRecentRequests: %v", err)
	}
	if len(records) != 1 || records[0].BrokerID != "legacy-broker" {
		t.Fatalf("expected the pre-existing legacy row to be visible under DefaultProfileID, got %+v", records)
	}
}

func TestResolveProfileForBroker(t *testing.T) {
	s := newTestStore(t)
	now := time.Now()

	addRecordForProfile(t, s, "maris", "broker-a", StatusSent, now.Add(-2*time.Hour))
	addRecordForProfile(t, s, "spouse", "broker-b", StatusSent, now.Add(-1*time.Hour))

	got, err := s.ResolveProfileForBroker("broker-a")
	if err != nil {
		t.Fatalf("ResolveProfileForBroker(broker-a): %v", err)
	}
	if got != "maris" {
		t.Errorf("expected broker-a to resolve to maris, got %q", got)
	}

	got, err = s.ResolveProfileForBroker("broker-b")
	if err != nil {
		t.Fatalf("ResolveProfileForBroker(broker-b): %v", err)
	}
	if got != "spouse" {
		t.Errorf("expected broker-b to resolve to spouse, got %q", got)
	}

	// Never emailed by anyone - falls back to DefaultProfileID rather than erroring.
	got, err = s.ResolveProfileForBroker("never-sent")
	if err != nil {
		t.Fatalf("ResolveProfileForBroker(never-sent): %v", err)
	}
	if got != DefaultProfileID {
		t.Errorf("expected never-sent broker to fall back to %q, got %q", DefaultProfileID, got)
	}
}

// ==================== Profile-Scoped Task/Response Isolation ====================
//
// Regression coverage for a fix that added "AND profile_id = ?" to
// GetPendingTaskByID, CompletePendingTask, MarkTaskOpened,
// UpdateBrokerResponseClassification and UpdateBrokerResponseBody, so a
// caller in one profile's session can no longer read or mutate a task/
// response that belongs to another profile just by guessing/reusing its
// numeric ID. Each test below confirms both halves: the cross-profile call
// is a no-op (getter returns nil,nil; updater returns no error but leaves
// the row untouched), and the same call with the correct profile still
// works as before.

func addPendingTaskForProfile(t *testing.T, s *Store, profileID, brokerID string) *PendingTask {
	t.Helper()
	task := &PendingTask{
		ProfileID:  profileID,
		BrokerID:   brokerID,
		BrokerName: brokerID,
		TaskType:   TaskCaptcha,
		FormURL:    "https://" + brokerID + ".example.com/form",
		Notes:      "initial notes",
	}
	if err := s.AddPendingTask(task); err != nil {
		t.Fatalf("AddPendingTask: %v", err)
	}
	return task
}

func TestProfileIsolation_GetPendingTaskByID(t *testing.T) {
	s := newTestStore(t)
	task := addPendingTaskForProfile(t, s, "profile-a", "broker-a")

	// Wrong profile: treated as not found, same as a nonexistent ID.
	got, err := s.GetPendingTaskByID(task.ID, "profile-b")
	if err != nil {
		t.Fatalf("GetPendingTaskByID(profile-b): %v", err)
	}
	if got != nil {
		t.Errorf("expected nil for a task belonging to another profile, got %+v", got)
	}

	// Correct profile: found, with fields intact.
	got, err = s.GetPendingTaskByID(task.ID, "profile-a")
	if err != nil {
		t.Fatalf("GetPendingTaskByID(profile-a): %v", err)
	}
	if got == nil {
		t.Fatal("expected task to be found under its own profile")
	}
	if got.BrokerID != "broker-a" || got.Status != "pending" {
		t.Errorf("unexpected task contents: %+v", got)
	}
}

func TestProfileIsolation_CompletePendingTask(t *testing.T) {
	s := newTestStore(t)
	task := addPendingTaskForProfile(t, s, "profile-a", "broker-a")

	// Wrong profile: CompletePendingTask returns no error (SQL UPDATE
	// matching zero rows isn't an error condition in database/sql - Exec
	// only errors on things like a broken connection or bad SQL, not on
	// "the WHERE clause matched nothing"), but the row must be unchanged.
	if err := s.CompletePendingTask(task.ID, "profile-b", "completed"); err != nil {
		t.Fatalf("CompletePendingTask(profile-b) returned an error for a cross-profile no-op: %v", err)
	}
	got, err := s.GetPendingTaskByID(task.ID, "profile-a")
	if err != nil {
		t.Fatalf("GetPendingTaskByID: %v", err)
	}
	if got == nil {
		t.Fatal("expected task to still exist under profile-a")
	}
	if got.Status != "pending" {
		t.Errorf("cross-profile CompletePendingTask must not mutate the row - expected status still 'pending', got %q", got.Status)
	}
	if got.CompletedAt.Valid {
		t.Errorf("cross-profile CompletePendingTask must not set completed_at, got %v", got.CompletedAt)
	}

	// Correct profile: the update takes effect.
	if err := s.CompletePendingTask(task.ID, "profile-a", "completed"); err != nil {
		t.Fatalf("CompletePendingTask(profile-a): %v", err)
	}
	got, err = s.GetPendingTaskByID(task.ID, "profile-a")
	if err != nil {
		t.Fatalf("GetPendingTaskByID: %v", err)
	}
	if got == nil || got.Status != "completed" {
		t.Errorf("expected status 'completed' after same-profile CompletePendingTask, got %+v", got)
	}
	if !got.CompletedAt.Valid {
		t.Errorf("expected completed_at to be set after same-profile CompletePendingTask")
	}
}

func TestProfileIsolation_MarkTaskOpened(t *testing.T) {
	s := newTestStore(t)
	task := addPendingTaskForProfile(t, s, "profile-a", "broker-a")

	// Wrong profile: no error, but opened_at must stay unset.
	if err := s.MarkTaskOpened(task.ID, "profile-b"); err != nil {
		t.Fatalf("MarkTaskOpened(profile-b) returned an error for a cross-profile no-op: %v", err)
	}
	got, err := s.GetPendingTaskByID(task.ID, "profile-a")
	if err != nil {
		t.Fatalf("GetPendingTaskByID: %v", err)
	}
	if got == nil {
		t.Fatal("expected task to still exist under profile-a")
	}
	if got.OpenedAt.Valid {
		t.Errorf("cross-profile MarkTaskOpened must not set opened_at, got %v", got.OpenedAt)
	}

	// Correct profile: opened_at gets set.
	if err := s.MarkTaskOpened(task.ID, "profile-a"); err != nil {
		t.Fatalf("MarkTaskOpened(profile-a): %v", err)
	}
	got, err = s.GetPendingTaskByID(task.ID, "profile-a")
	if err != nil {
		t.Fatalf("GetPendingTaskByID: %v", err)
	}
	if got == nil || !got.OpenedAt.Valid {
		t.Errorf("expected opened_at to be set after same-profile MarkTaskOpened, got %+v", got)
	}
}

func addBrokerResponseForProfile(t *testing.T, s *Store, profileID, brokerID string) *BrokerResponse {
	t.Helper()
	resp := &BrokerResponse{
		ProfileID:    profileID,
		BrokerID:     brokerID,
		BrokerName:   brokerID,
		ResponseType: "pending",
		EmailFrom:    "privacy@" + brokerID + ".example.com",
		EmailSubject: "Re: Personal Data Removal Request",
		EmailBody:    "original body",
		Confidence:   0.5,
	}
	if err := s.AddBrokerResponse(resp); err != nil {
		t.Fatalf("AddBrokerResponse: %v", err)
	}
	return resp
}

// findBrokerResponseByID is a small test helper - there's no production
// GetBrokerResponseByID, so this scans GetAllBrokerResponses (which spans
// every profile, matching what a full-inbox re-scan uses) for the row under
// test.
func findBrokerResponseByID(t *testing.T, s *Store, id int64) *BrokerResponse {
	t.Helper()
	all, err := s.GetAllBrokerResponses()
	if err != nil {
		t.Fatalf("GetAllBrokerResponses: %v", err)
	}
	for i := range all {
		if all[i].ID == id {
			return &all[i]
		}
	}
	return nil
}

func TestProfileIsolation_UpdateBrokerResponseClassification(t *testing.T) {
	s := newTestStore(t)
	resp := addBrokerResponseForProfile(t, s, "profile-a", "broker-a")

	// Wrong profile: no error, but the classification fields must stay as inserted.
	if err := s.UpdateBrokerResponseClassification(resp.ID, "profile-b", "success", "https://form.example.com", "https://confirm.example.com", 0.99, true); err != nil {
		t.Fatalf("UpdateBrokerResponseClassification(profile-b) returned an error for a cross-profile no-op: %v", err)
	}
	got := findBrokerResponseByID(t, s, resp.ID)
	if got == nil {
		t.Fatal("expected response to still exist")
	}
	if got.ResponseType != "pending" || got.FormURL != "" || got.NeedsReview {
		t.Errorf("cross-profile UpdateBrokerResponseClassification must not mutate the row, got %+v", got)
	}

	// Correct profile: the update takes effect.
	if err := s.UpdateBrokerResponseClassification(resp.ID, "profile-a", "success", "https://form.example.com", "https://confirm.example.com", 0.99, true); err != nil {
		t.Fatalf("UpdateBrokerResponseClassification(profile-a): %v", err)
	}
	got = findBrokerResponseByID(t, s, resp.ID)
	if got == nil {
		t.Fatal("expected response to still exist")
	}
	if got.ResponseType != "success" || got.FormURL != "https://form.example.com" || got.ConfirmURL != "https://confirm.example.com" || !got.NeedsReview {
		t.Errorf("expected classification fields updated after same-profile call, got %+v", got)
	}
}

func TestProfileIsolation_UpdateBrokerResponseBody(t *testing.T) {
	s := newTestStore(t)
	resp := addBrokerResponseForProfile(t, s, "profile-a", "broker-a")

	// Wrong profile: no error, but the body must stay as inserted.
	if err := s.UpdateBrokerResponseBody(resp.ID, "profile-b", "attacker-controlled body"); err != nil {
		t.Fatalf("UpdateBrokerResponseBody(profile-b) returned an error for a cross-profile no-op: %v", err)
	}
	got := findBrokerResponseByID(t, s, resp.ID)
	if got == nil {
		t.Fatal("expected response to still exist")
	}
	if got.EmailBody != "original body" {
		t.Errorf("cross-profile UpdateBrokerResponseBody must not mutate the row, got body %q", got.EmailBody)
	}

	// Correct profile: the update takes effect.
	if err := s.UpdateBrokerResponseBody(resp.ID, "profile-a", "updated body"); err != nil {
		t.Fatalf("UpdateBrokerResponseBody(profile-a): %v", err)
	}
	got = findBrokerResponseByID(t, s, resp.ID)
	if got == nil || got.EmailBody != "updated body" {
		t.Errorf("expected body updated after same-profile call, got %+v", got)
	}
}

// ContactedBrokerIDs gates inbox matching: a broker we never wrote to can't be
// replying to us. Without that gate, matching on sender domain alone recorded
// ordinary Google and Gmail correspondence as broker responses.
func TestContactedBrokerIDs(t *testing.T) {
	s := newTestStore(t)
	now := time.Now()

	addRecord(t, s, "broker-a", StatusSent, now.Add(-1*time.Hour))
	addRecord(t, s, "broker-b", StatusFailed, now.Add(-2*time.Hour))
	// Same broker twice - the set must not care.
	addRecord(t, s, "broker-a", StatusSent, now.Add(-3*time.Hour))

	contacted, err := s.ContactedBrokerIDs()
	if err != nil {
		t.Fatalf("ContactedBrokerIDs: %v", err)
	}

	if len(contacted) != 2 {
		t.Fatalf("expected 2 distinct brokers, got %d: %+v", len(contacted), contacted)
	}
	if !contacted["broker-a"] || !contacted["broker-b"] {
		t.Errorf("expected both brokers present, got %+v", contacted)
	}
	// A broker that only ever appeared as an inbox match, never as a sent
	// request - this is the case the gate exists to reject.
	if contacted["google-search-removal"] {
		t.Error("a broker never written to must not appear as contacted")
	}
}

func TestContactedBrokerIDsEmptyOnFreshStore(t *testing.T) {
	s := newTestStore(t)

	contacted, err := s.ContactedBrokerIDs()
	if err != nil {
		t.Fatalf("ContactedBrokerIDs: %v", err)
	}
	if len(contacted) != 0 {
		t.Errorf("expected an empty set on a fresh store, got %+v", contacted)
	}
}

// ==================== Broker Response Deduplication Tests ====================

func testBrokerResponse(messageID, subject string, receivedAt time.Time) *BrokerResponse {
	return &BrokerResponse{
		BrokerID:       "broker-a",
		BrokerName:     "Broker A",
		ResponseType:   "pending",
		EmailFrom:      "privacy@broker-a.example.com",
		EmailSubject:   subject,
		EmailMessageID: messageID,
		Confidence:     0.9,
		ReceivedAt:     receivedAt,
	}
}

func countBrokerResponses(t *testing.T, s *Store) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM broker_responses`).Scan(&n); err != nil {
		t.Fatalf("count broker_responses: %v", err)
	}
	return n
}

// Every `eraser monitor` run and every web inbox scan re-fetches the same
// trailing window of mail, so before message_key existed each scan inserted
// a fresh copy of every reply it had already classified - a real database
// held 401 rows for 88 distinct replies. Re-recording the same message must
// be a no-op on the row count.
func TestAddBrokerResponse_RescanDoesNotDuplicate(t *testing.T) {
	s := newTestStore(t)
	received := time.Date(2026, 8, 30, 12, 53, 20, 0, time.UTC)

	first := testBrokerResponse("<abc@broker-a.example.com>", "[Request received]", received)
	if err := s.AddBrokerResponse(first); err != nil {
		t.Fatalf("AddBrokerResponse: %v", err)
	}

	// Three more scans of the same window, as the two entry points would do.
	for i := 0; i < 3; i++ {
		again := testBrokerResponse("<abc@broker-a.example.com>", "[Request received]", received)
		if err := s.AddBrokerResponse(again); err != nil {
			t.Fatalf("AddBrokerResponse (rescan %d): %v", i, err)
		}
		if again.ID != first.ID {
			t.Errorf("rescan %d: expected the existing row's ID %d, got %d", i, first.ID, again.ID)
		}
	}

	if n := countBrokerResponses(t, s); n != 1 {
		t.Fatalf("expected 1 stored response after 4 scans of the same message, got %d", n)
	}
}

// A different message from the same broker is still a separate response,
// even when brokers reuse one boilerplate subject for every acknowledgement.
func TestAddBrokerResponse_DistinctMessagesAreSeparateRows(t *testing.T) {
	s := newTestStore(t)
	received := time.Date(2026, 8, 30, 12, 53, 20, 0, time.UTC)

	for _, id := range []string{"<one@broker-a.example.com>", "<two@broker-a.example.com>"} {
		if err := s.AddBrokerResponse(testBrokerResponse(id, "[Request received]", received)); err != nil {
			t.Fatalf("AddBrokerResponse: %v", err)
		}
	}
	// No Message-ID at all: these fall back to the derived identity, where
	// the arrival timestamp is what separates them.
	if err := s.AddBrokerResponse(testBrokerResponse("", "[Request received]", received)); err != nil {
		t.Fatalf("AddBrokerResponse: %v", err)
	}
	if err := s.AddBrokerResponse(testBrokerResponse("", "[Request received]", received.Add(time.Minute))); err != nil {
		t.Fatalf("AddBrokerResponse: %v", err)
	}
	// ...and re-seeing that last one is still not a new row.
	if err := s.AddBrokerResponse(testBrokerResponse("", "[Request received]", received.Add(time.Minute))); err != nil {
		t.Fatalf("AddBrokerResponse: %v", err)
	}

	if n := countBrokerResponses(t, s); n != 4 {
		t.Fatalf("expected 4 distinct responses, got %d", n)
	}
}

// `eraser monitor` never stores an email body; the web scan does. Re-seeing
// a message must fill in a body the stored row is missing rather than
// leaving the reply permanently unreadable in the UI - but must not clobber
// a body that is already there.
func TestAddBrokerResponse_RescanBackfillsMissingBody(t *testing.T) {
	s := newTestStore(t)
	received := time.Date(2026, 8, 30, 12, 53, 20, 0, time.UTC)

	bodyless := testBrokerResponse("<abc@broker-a.example.com>", "Re: erasure request", received)
	if err := s.AddBrokerResponse(bodyless); err != nil {
		t.Fatalf("AddBrokerResponse: %v", err)
	}

	withBody := testBrokerResponse("<abc@broker-a.example.com>", "Re: erasure request", received)
	withBody.EmailBody = "We have received your request."
	if err := s.AddBrokerResponse(withBody); err != nil {
		t.Fatalf("AddBrokerResponse (with body): %v", err)
	}

	later := testBrokerResponse("<abc@broker-a.example.com>", "Re: erasure request", received)
	later.EmailBody = ""
	if err := s.AddBrokerResponse(later); err != nil {
		t.Fatalf("AddBrokerResponse (body-less rescan): %v", err)
	}

	all, err := s.GetAllBrokerResponses()
	if err != nil {
		t.Fatalf("GetAllBrokerResponses: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("expected 1 stored response, got %d", len(all))
	}
	if all[0].EmailBody != "We have received your request." {
		t.Fatalf("expected the backfilled body to be kept, got %q", all[0].EmailBody)
	}
}

// The migration has to fix databases that already grew duplicates, and stay
// a no-op when re-run. Rows written before the column existed carry no
// Message-ID, so they are keyed on the derived (broker, subject,
// received_at) identity - including a received_at in the Go
// time.Time.String() layout the driver actually writes.
func TestMigrateBrokerResponses_DedupesAndBackfillsLegacyRows(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "history.db")
	s, err := NewStore(dbPath)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	// Simulate a pre-migration database: drop the uniqueness guard and the
	// key column's contents, then write the duplicates the old scan loop
	// would have.
	if _, err := s.db.Exec(`DROP INDEX idx_br_message_key`); err != nil {
		t.Fatalf("drop index: %v", err)
	}
	insert := `INSERT INTO broker_responses (profile_id, broker_id, broker_name, response_type,
		email_from, email_subject, email_body, confidence, needs_review, received_at, processed_at, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, 0, ?, ?, ?)`
	now := time.Now()
	for i := 0; i < 4; i++ {
		body := ""
		if i == 1 { // only one scan of the four stored a body
			body = "We have received your request."
		}
		if _, err := s.db.Exec(insert, DefaultProfileID, "broker-a", "Broker A", "pending",
			"privacy@broker-a.example.com", "[Request received]", body, 0.9,
			"2026-08-30 12:53:20 +0000 +0000", now, now); err != nil {
			t.Fatalf("legacy insert: %v", err)
		}
	}
	// A genuinely different reply, duplicated the same way.
	for i := 0; i < 2; i++ {
		if _, err := s.db.Exec(insert, DefaultProfileID, "broker-b", "Broker B", "success",
			"privacy@broker-b.example.com", "Deletion complete", "", 0.95,
			"2026-08-29 09:00:00 +0000 +0000", now, now); err != nil {
			t.Fatalf("legacy insert: %v", err)
		}
	}
	if n := countBrokerResponses(t, s); n != 6 {
		t.Fatalf("expected 6 legacy rows, got %d", n)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Reopening runs migrate().
	s2, err := NewStore(dbPath)
	if err != nil {
		t.Fatalf("NewStore (migrating): %v", err)
	}
	t.Cleanup(func() { _ = s2.Close() })

	if n := countBrokerResponses(t, s2); n != 2 {
		t.Fatalf("expected the 6 legacy rows to collapse to 2 distinct replies, got %d", n)
	}
	all, err := s2.GetAllBrokerResponses()
	if err != nil {
		t.Fatalf("GetAllBrokerResponses: %v", err)
	}
	for _, r := range all {
		if r.BrokerID == "broker-a" && r.EmailBody != "We have received your request." {
			t.Errorf("expected the surviving broker-a row to be the one with a body, got %q", r.EmailBody)
		}
	}

	// The first scan after migrating sees the same messages, now with the
	// Message-IDs that were never stored before. Those must re-key the
	// existing rows rather than insert a duplicate alongside each of them.
	rekeyed := testBrokerResponse("<abc@broker-a.example.com>", "[Request received]",
		time.Date(2026, 8, 30, 12, 53, 20, 0, time.UTC))
	if err := s2.AddBrokerResponse(rekeyed); err != nil {
		t.Fatalf("AddBrokerResponse (post-migration scan): %v", err)
	}
	// And the scan after that, matching on the Message-ID directly.
	if err := s2.AddBrokerResponse(testBrokerResponse("<abc@broker-a.example.com>", "[Request received]",
		time.Date(2026, 8, 30, 12, 53, 20, 0, time.UTC))); err != nil {
		t.Fatalf("AddBrokerResponse (second post-migration scan): %v", err)
	}
	if n := countBrokerResponses(t, s2); n != 2 {
		t.Fatalf("expected 2 responses after two post-migration scans, got %d", n)
	}

	// Re-running the migration on an already-migrated database changes
	// nothing.
	if err := s2.migrate(); err != nil {
		t.Fatalf("migrate (idempotency): %v", err)
	}
	if n := countBrokerResponses(t, s2); n != 2 {
		t.Fatalf("expected re-running migrate() to be a no-op, got %d rows", n)
	}
}

func TestParseStoredTime(t *testing.T) {
	want := time.Date(2026, 8, 30, 12, 53, 20, 0, time.UTC)
	for _, raw := range []string{
		"2026-08-30T12:53:20Z",
		"2026-08-30 12:53:20",
		"2026-08-30 12:53:20 +0000 +0000",
		"2026-08-30 12:53:20 +0000 UTC",
		"2026-08-30 05:53:20 -0700 -0700",
		"2026-08-30 12:53:20 +0000 UTC m=+562.394283501",
	} {
		got, ok := parseStoredTime(raw)
		if !ok {
			t.Errorf("parseStoredTime(%q): no layout matched", raw)
			continue
		}
		if !got.Equal(want) {
			t.Errorf("parseStoredTime(%q) = %v, want %v", raw, got.UTC(), want)
		}
	}
	if _, ok := parseStoredTime("not a timestamp"); ok {
		t.Error("expected an unparseable value to be reported as such")
	}
}
