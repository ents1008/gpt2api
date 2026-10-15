package account

import (
	"context"
	"database/sql/driver"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/jmoiron/sqlx"

	"github.com/432539/gpt2api/pkg/crypto"
)

// TestRecordRefreshErrorMarkDeadPreservesExistingDeadAccount verifies that a
// manual refresh failure cannot revive an account already proven dead by a
// previous runtime auth failure. Expected: the UPDATE keeps existing dead
// accounts dead instead of trusting a stale future token_expires_at value.
func TestRecordRefreshErrorMarkDeadPreservesExistingDeadAccount(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("create sqlmock: %v", err)
	}
	defer db.Close()

	dao := NewDAO(sqlx.NewDb(db, "sqlmock"))
	mock.ExpectExec(`(?s)UPDATE oai_accounts.*WHEN status = 'dead' THEN 'dead'.*WHEN token_expires_at IS NOT NULL AND token_expires_at > NOW\(\) THEN 'warned'.*WHERE id = \? AND deleted_at IS NULL`).
		WithArgs(sqlmock.AnyArg(), RefreshSourceRT, "RT 已失效(401)", uint64(938)).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := dao.RecordRefreshError(context.Background(), 938, RefreshSourceRT, "RT 已失效(401)", true); err != nil {
		t.Fatalf("RecordRefreshError returned error: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("SQL expectation was not met: %v", err)
	}
}

// TestImportBatchAdmitsCompleteBrowserRuntimeIdentity verifies that the bulk
// JSON import path writes the coherent browser/runtime identity captured at
// registration. Expected: browser-owned credentials are production eligible
// immediately even if an obsolete caller still requests quarantine.
func TestImportBatchAdmitsCompleteBrowserRuntimeIdentity(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("create sqlmock: %v", err)
	}
	defer db.Close()

	cipher, err := crypto.NewAESGCM("00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff")
	if err != nil {
		t.Fatalf("create cipher: %v", err)
	}
	svc := NewService(NewDAO(sqlx.NewDb(db, "sqlmock")), cipher)

	mock.ExpectQuery(`SELECT \* FROM oai_accounts WHERE email = \? AND deleted_at IS NULL LIMIT 1`).
		WithArgs("user@example.com").
		WillReturnRows(sqlmock.NewRows(accountColumns()))
	mock.ExpectExec(`(?s)INSERT INTO oai_accounts.*oai_session_id, oai_device_id, client_id, refresh_client_id, credential_provenance, browser_profile.*admission_state.*VALUES`).
		WithArgs(
			"user@example.com",
			sqlmock.AnyArg(),
			sqlmock.AnyArg(),
			sqlmock.AnyArg(),
			sqlmock.AnyArg(),
			"session-456",
			"device-123",
			"app_web",
			"",
			CredentialProvenanceBrowserWeb,
			testBrowserProfileJSON,
			"acct_123",
			"chatgpt",
			"free",
			100,
			StatusHealthy,
			AdmissionStateAdmitted,
			AdmissionStageAdmitted,
			sqlmock.AnyArg(),
			sqlmock.AnyArg(),
			sqlmock.AnyArg(),
			sqlmock.AnyArg(),
			"",
			"",
		).
		WillReturnResult(sqlmock.NewResult(42, 1))
	mock.ExpectExec(`INSERT INTO oai_account_cookies \(account_id, cookie_json_enc\)`).
		WithArgs(uint64(42), encryptedCookieJSONArg{
			cipher: cipher,
			want:   `[{"name":"oai-did","value":"device-123","domain":"chatgpt.com","path":"/"}]`,
		}).
		WillReturnResult(sqlmock.NewResult(0, 1))

	summary := svc.ImportBatch(context.Background(), []ImportSource{{
		Email:                "user@example.com",
		AccessToken:          "at",
		SessionToken:         "st",
		ClientID:             "app_web",
		CredentialProvenance: CredentialProvenanceBrowserWeb,
		BrowserProfile:       testBrowserProfileJSON,
		ChatGPTAccountID:     "acct_123",
		AccountType:          "chatgpt",
		DeviceID:             "device-123",
		SessionID:            "session-456",
		Cookies:              `[{"name":"oai-did","value":"device-123","domain":".chatgpt.com"}]`,
	}}, ImportOptions{UpdateExisting: true, DefaultClientID: "app_web", Quarantine: true})

	if summary.Created != 1 || summary.Failed != 0 {
		t.Fatalf("summary = %+v, want one created account", summary)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("SQL expectation was not met: %v", err)
	}
}

// TestImportBatchRejectsIncompleteBrowserIdentityWithoutQuarantine verifies
// that immediate admission does not weaken the server-side credential
// boundary. Expected: a browser provenance record without the complete ST,
// device, and cookie identity fails before any database access.
func TestImportBatchRejectsIncompleteBrowserIdentityWithoutQuarantine(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("create sqlmock: %v", err)
	}
	defer db.Close()

	cipher, err := crypto.NewAESGCM("00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff")
	if err != nil {
		t.Fatalf("create cipher: %v", err)
	}
	svc := NewService(NewDAO(sqlx.NewDb(db, "sqlmock")), cipher)

	summary := svc.ImportBatch(context.Background(), []ImportSource{{
		Email:                "user@example.com",
		AccessToken:          "at",
		CredentialProvenance: CredentialProvenanceBrowserWeb,
	}}, ImportOptions{UpdateExisting: true, Quarantine: false})

	if summary.Failed != 1 || summary.Created != 0 {
		t.Fatalf("summary = %+v, want one rejected account", summary)
	}
	if !strings.Contains(summary.Results[0].Reason, "Browser Web") {
		t.Fatalf("reason = %q, want browser identity rejection", summary.Results[0].Reason)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unexpected database access: %v", err)
	}
}

// TestApplyImportedAdmissionStateRestoresBrowserAccount verifies the update
// path for an account left behind by the obsolete one-hour gate. Expected: a
// complete browser re-import clears every quarantine lease field and restores
// immediate production admission.
func TestApplyImportedAdmissionStateRestoresBrowserAccount(t *testing.T) {
	now := time.Date(2026, 8, 11, 16, 30, 0, 0, time.FixedZone("HKT", 8*60*60))
	a := &Account{
		AdmissionState:        AdmissionStateQuarantined,
		AdmissionStage:        AdmissionStageInitial,
		AdmissionStartedAt:    nullTime(now.Add(-30 * time.Minute)),
		AdmissionNextCheckAt:  nullTime(now.Add(30 * time.Minute)),
		AdmissionLastCheckAt:  nullTime(now.Add(-time.Minute)),
		AdmissionClaimedUntil: nullTime(now.Add(time.Minute)),
		AdmissionReason:       AdmissionReasonPending1h,
	}

	applyImportedAdmissionState(a, CredentialProvenanceBrowserWeb, true, now)

	if a.AdmissionState != AdmissionStateAdmitted || a.AdmissionStage != AdmissionStageAdmitted {
		t.Fatalf("admission = %s/%d, want admitted/%d", a.AdmissionState, a.AdmissionStage, AdmissionStageAdmitted)
	}
	if a.AdmissionStartedAt.Valid || a.AdmissionNextCheckAt.Valid || a.AdmissionLastCheckAt.Valid || a.AdmissionClaimedUntil.Valid {
		t.Fatalf("obsolete admission lease was not cleared: %+v", a)
	}
	if a.AdmissionReason != "" {
		t.Fatalf("admission reason = %q, want empty", a.AdmissionReason)
	}
}

// TestImportBatchRejectsMalformedCookiesBeforeDatabaseAccess protects account
// import atomicity. Expected: malformed JSON-looking cookie input is rejected
// before lookup/create, so a failed browser-session import cannot leave a
// credential row without its required cookie state.
func TestImportBatchRejectsMalformedCookiesBeforeDatabaseAccess(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("create sqlmock: %v", err)
	}
	defer db.Close()

	cipher, err := crypto.NewAESGCM("00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff")
	if err != nil {
		t.Fatalf("create cipher: %v", err)
	}
	svc := NewService(NewDAO(sqlx.NewDb(db, "sqlmock")), cipher)
	summary := svc.ImportBatch(context.Background(), []ImportSource{{
		Email:       "invalid-cookie@example.com",
		AccessToken: "at",
		Cookies:     `[{"name":`,
	}}, ImportOptions{UpdateExisting: true})

	if summary.Failed != 1 || len(summary.Results) != 1 || !strings.Contains(summary.Results[0].Reason, "Cookies 格式错误") {
		t.Fatalf("summary = %+v, want pre-database cookie validation failure", summary)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unexpected database access: %v", err)
	}
}

type encryptedCookieJSONArg struct {
	cipher *crypto.AESGCM
	want   string
}

func (m encryptedCookieJSONArg) Match(value driver.Value) bool {
	raw, ok := value.(string)
	if !ok || m.cipher == nil {
		return false
	}
	plain, err := m.cipher.DecryptString(raw)
	return err == nil && plain == m.want
}

// TestListDispatchableUsesRecentImageSuccessPriority verifies image dispatch
// does not let old suspicious failures dominate the small scheduler candidate
// window. Expected: the SQL joins recent image_tasks successes and failures,
// and only treats an account as recently good when its last success is newer
// than its last failure.
func TestListDispatchableUsesRecentImageSuccessPriority(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("create sqlmock: %v", err)
	}
	defer db.Close()

	dao := NewDAO(sqlx.NewDb(db, "sqlmock"))
	rows := sqlmock.NewRows(accountColumns()).
		AddRow(dispatchableAccountRow(958, StatusSuspicious, "upstream_network_transient", 0, time.Now().Add(-80*time.Minute))...).
		AddRow(dispatchableAccountRow(836, StatusSuspicious, "turnstile_stalled", 3, time.Now().Add(-2*time.Hour))...)

	mock.ExpectQuery(`(?s)SELECT a\.\* FROM oai_accounts a.*LEFT JOIN.*recent_successes.*last_success_at.*last_failure_at.*image_tasks.*WHERE.*a\.admission_state = 'admitted'.*ORDER BY.*last_success_at > recent\.last_failure_at.*recent_successes.*last_success_at.*DESC.*suspicious.*refresh_error.*LIMIT \?`).
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), 30).
		WillReturnRows(rows)

	got, err := dao.ListDispatchable(context.Background(), 30)
	if err != nil {
		t.Fatalf("ListDispatchable returned error: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("dispatchable accounts = %d, want 2", len(got))
	}
	if got[0].ID != 958 || got[1].ID != 836 {
		t.Fatalf("dispatchable order = [%s], want [958 836]", strings.TrimSpace(dispatchableIDs(got)))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("SQL expectation was not met: %v", err)
	}
}

// TestListDispatchableRetriesExpiredThrottledAccounts verifies that an
// account-local rate-limit cooldown is temporary rather than a permanent pool
// removal. Expected: throttled accounts are candidates only when they carry an
// explicit cooldown, the shared cooldown predicate has expired, and they sort
// behind healthy, warned, and suspicious accounts.
func TestListDispatchableRetriesExpiredThrottledAccounts(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("create sqlmock: %v", err)
	}
	defer db.Close()

	dao := NewDAO(sqlx.NewDb(db, "sqlmock"))
	row := dispatchableAccountRow(1498, StatusThrottled, "", 0, time.Now().Add(-24*time.Hour))
	row[18] = time.Now().Add(-time.Hour)
	rows := sqlmock.NewRows(accountColumns()).AddRow(row...)

	mock.ExpectQuery(`(?s)SELECT a\.\* FROM oai_accounts a.*WHERE.*a\.admission_state = 'admitted'.*\(a\.status IN \('healthy', 'warned', 'suspicious'\).*OR \(a\.status='throttled' AND a\.cooldown_until IS NOT NULL\)\).*a\.cooldown_until IS NULL OR a\.cooldown_until <= \?.*CASE a\.status WHEN 'healthy' THEN 0 WHEN 'warned' THEN 1 WHEN 'suspicious' THEN 2 WHEN 'throttled' THEN 3 ELSE 4 END.*LIMIT \?`).
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), 30).
		WillReturnRows(rows)

	got, err := dao.ListDispatchable(context.Background(), 30)
	if err != nil {
		t.Fatalf("ListDispatchable returned error: %v", err)
	}
	if len(got) != 1 || got[0].ID != 1498 {
		t.Fatalf("dispatchable accounts = [%s], want [1498]", strings.TrimSpace(dispatchableIDs(got)))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("SQL expectation was not met: %v", err)
	}
}

// TestListNeedProbeQuotaExcludesQuarantinedAccounts ensures admission checks
// are the only background traffic applied to a new credential. Expected: the
// quota probe query requires admitted state even when operational health is
// healthy, preventing an immediate extra upstream request after import.
func TestListNeedProbeQuotaExcludesQuarantinedAccounts(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("create sqlmock: %v", err)
	}
	defer db.Close()

	dao := NewDAO(sqlx.NewDb(db, "sqlmock"))
	mock.ExpectQuery(`(?s)SELECT \* FROM oai_accounts.*status = 'healthy'.*admission_state = 'admitted'.*image_quota_updated_at.*LIMIT \?`).
		WithArgs(sqlmock.AnyArg(), 30).
		WillReturnRows(sqlmock.NewRows(accountColumns()))

	rows, err := dao.ListNeedProbeQuota(context.Background(), 18000, 30)
	if err != nil {
		t.Fatalf("ListNeedProbeQuota returned error: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("quota probe candidates = %d, want 0", len(rows))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("SQL expectation was not met: %v", err)
	}
}

// TestListNeedRefreshExcludesImmutableBrowserQuarantine ensures the generic
// refresh scanner cannot repeatedly select a browser registration that is under
// its one-hour observation. Expected: the SQL boundary excludes both immutable
// Web provenance values before any credential decrypt or network work occurs.
func TestListNeedRefreshExcludesImmutableBrowserQuarantine(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("create sqlmock: %v", err)
	}
	defer db.Close()

	dao := NewDAO(sqlx.NewDb(db, "sqlmock"))
	mock.ExpectQuery(`(?s)SELECT \* FROM oai_accounts.*NOT \(admission_state = \? AND credential_provenance IN \(\?, \?\)\).*token_expires_at <= \?.*LIMIT \?`).
		WithArgs(
			AdmissionStateQuarantined,
			CredentialProvenanceWebOnly,
			CredentialProvenanceBrowserWeb,
			sqlmock.AnyArg(),
			30,
		).
		WillReturnRows(sqlmock.NewRows(accountColumns()))

	rows, err := dao.ListNeedRefresh(context.Background(), 1800, 30)
	if err != nil {
		t.Fatalf("ListNeedRefresh returned error: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("refresh candidates = %d, want 0", len(rows))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("SQL expectation was not met: %v", err)
	}
}

// TestRestoreHealthyRestoresSuccessfulThrottledAccount verifies that a real
// successful image is authoritative evidence that an expired account-local
// throttle has recovered. Expected: both suspicious and throttled transient
// states return to healthy, while dead and operator-disabled states remain out.
func TestRestoreHealthyRestoresSuccessfulThrottledAccount(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("create sqlmock: %v", err)
	}
	defer db.Close()

	dao := NewDAO(sqlx.NewDb(db, "sqlmock"))
	mock.ExpectExec(`(?s)UPDATE oai_accounts.*SET status='healthy', cooldown_until=NULL, refresh_error=''.*WHERE id=\? AND deleted_at IS NULL AND status IN \('suspicious', 'throttled'\)`).
		WithArgs(uint64(1498)).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := dao.RestoreHealthy(context.Background(), 1498); err != nil {
		t.Fatalf("RestoreHealthy returned error: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("SQL expectation was not met: %v", err)
	}
}

// TestApplyRefreshResultPreservesThrottledCooldown verifies that rotating an
// authentication token does not erase an independent image-quota cooldown.
// Expected: refresh still heals auth-related states, but a throttled account
// retains its cooldown until real generation is allowed to retry it.
func TestApplyRefreshResultPreservesThrottledCooldown(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("create sqlmock: %v", err)
	}
	defer db.Close()

	dao := NewDAO(sqlx.NewDb(db, "sqlmock"))
	expiresAt := time.Now().Add(24 * time.Hour)
	mock.ExpectExec(`(?s)UPDATE oai_accounts.*cooldown_until = CASE WHEN status='throttled' THEN cooldown_until ELSE NULL END.*status = CASE WHEN status IN \('dead','suspicious','warned'\) THEN 'healthy' ELSE status END.*WHERE id = \? AND deleted_at IS NULL`).
		WithArgs("new-at", expiresAt, sqlmock.AnyArg(), RefreshSourceST, uint64(1552)).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := dao.ApplyRefreshResult(context.Background(), 1552, "", "new-at", "", expiresAt, RefreshSourceST); err != nil {
		t.Fatalf("ApplyRefreshResult returned error: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("SQL expectation was not met: %v", err)
	}
}

// TestApplyRefreshResultRejectsStaleRotatedRT protects rotating refresh tokens
// from a cross-process lost update. Expected trajectory: the write compares
// the encrypted RT snapshot, observes zero affected rows, and returns a typed
// conflict instead of overwriting the winner's newer AT/RT pair.
func TestApplyRefreshResultRejectsStaleRotatedRT(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("create sqlmock: %v", err)
	}
	defer db.Close()

	dao := NewDAO(sqlx.NewDb(db, "sqlmock"))
	expiresAt := time.Now().Add(24 * time.Hour)
	mock.ExpectExec(`(?s)UPDATE oai_accounts.*refresh_token_enc = \?.*WHERE id = \? AND deleted_at IS NULL AND refresh_token_enc = \?`).
		WithArgs("new-at", "new-rt", expiresAt, sqlmock.AnyArg(), RefreshSourceRT, uint64(1889), "old-rt").
		WillReturnResult(sqlmock.NewResult(0, 0))

	err = dao.ApplyRefreshResult(
		context.Background(),
		1889,
		"old-rt",
		"new-at",
		"new-rt",
		expiresAt,
		RefreshSourceRT,
	)
	if !errors.Is(err, ErrRefreshConflict) {
		t.Fatalf("ApplyRefreshResult error = %v, want ErrRefreshConflict", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("SQL expectation was not met: %v", err)
	}
}

// TestRotateRefreshTokenCASPreservesRotationAfterRejectedAT verifies an OAuth
// server's RT rotation is not discarded when the returned AT later fails Web
// verification. Expected: only the RT is advanced under the old encrypted RT
// predicate, preserving recovery without promoting the unverified AT.
func TestRotateRefreshTokenCASPreservesRotationAfterRejectedAT(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("create sqlmock: %v", err)
	}
	defer db.Close()

	dao := NewDAO(sqlx.NewDb(db, "sqlmock"))
	mock.ExpectExec(`(?s)UPDATE oai_accounts.*SET refresh_token_enc = \?.*last_refresh_source = \?.*refresh_error = \?.*WHERE id = \? AND deleted_at IS NULL AND refresh_token_enc = \?`).
		WithArgs("new-rt", sqlmock.AnyArg(), RefreshSourceRT, "rt_at_unverified", uint64(1889), "old-rt").
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := dao.RotateRefreshTokenCAS(context.Background(), 1889, "old-rt", "new-rt", "rt_at_unverified"); err != nil {
		t.Fatalf("RotateRefreshTokenCAS returned error: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("SQL expectation was not met: %v", err)
	}
}

// TestApplyAdmissionTransitionRejectsStaleObservation prevents an old probe
// from advancing credentials that were re-imported while the probe was in
// flight. Expected: admission_started_at acts as the observation generation,
// zero rows becomes ErrAdmissionConflict, and the new quarantine clock wins.
func TestApplyAdmissionTransitionRejectsStaleObservation(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("create sqlmock: %v", err)
	}
	defer db.Close()

	dao := NewDAO(sqlx.NewDb(db, "sqlmock"))
	startedAt := time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC)
	checkedAt := startedAt.Add(time.Hour)
	transition := admissionTransition{
		State:     AdmissionStateAdmitted,
		Stage:     AdmissionStageAdmitted,
		StartedAt: nullTime(startedAt),
		Reason:    AdmissionReasonChecksPassed,
	}
	mock.ExpectExec(`(?s)UPDATE oai_accounts.*admission_started_at = \?.*WHERE id = \?.*admission_stage = \?.*admission_started_at = \?`).
		WithArgs(
			AdmissionStateAdmitted,
			AdmissionStageAdmitted,
			transition.StartedAt,
			transition.NextCheckAt,
			checkedAt,
			AdmissionReasonChecksPassed,
			uint64(1940),
			AdmissionStageInitial,
			startedAt,
		).
		WillReturnResult(sqlmock.NewResult(0, 0))

	err = dao.ApplyAdmissionTransition(
		context.Background(),
		1940,
		AdmissionStageInitial,
		startedAt,
		transition,
		checkedAt,
	)
	if !errors.Is(err, ErrAdmissionConflict) {
		t.Fatalf("ApplyAdmissionTransition error = %v, want ErrAdmissionConflict", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("SQL expectation was not met: %v", err)
	}
}

func accountColumns() []string {
	return []string{
		"id", "email", "auth_token_enc", "refresh_token_enc", "session_token_enc", "token_expires_at",
		"oai_session_id", "oai_device_id", "client_id", "refresh_client_id", "credential_provenance", "browser_profile", "chatgpt_account_id", "account_type",
		"plan_type", "daily_image_quota", "status", "warned_at", "cooldown_until", "last_used_at",
		"today_used_count", "today_used_date", "last_refresh_at", "last_refresh_source", "refresh_error",
		"image_quota_remaining", "image_quota_total", "image_quota_reset_at", "image_quota_updated_at",
		"admission_state", "admission_stage", "admission_started_at", "admission_next_check_at",
		"admission_last_check_at", "admission_claimed_until", "admission_reason",
		"notes", "created_at", "updated_at", "deleted_at", "active_email",
	}
}

func dispatchableAccountRow(id uint64, status, reason string, quota int, lastUsed time.Time) []driver.Value {
	now := time.Now()
	email := "user@example.com"
	return []driver.Value{
		int64(id), email, "at", nil, nil, now.Add(24 * time.Hour),
		"session", "device", "client", "refresh-client", "", testBrowserProfileJSON, "acct", "codex",
		"free", 100, status, nil, nil, lastUsed,
		0, nil, nil, "", reason,
		quota, -1, nil, nil,
		AdmissionStateAdmitted, AdmissionStageAdmitted, nil, nil, nil, nil, "",
		"", now, now, nil, email,
	}
}

func dispatchableIDs(rows []*Account) string {
	var b strings.Builder
	for _, row := range rows {
		b.WriteString(" ")
		b.WriteString(strconv.FormatUint(row.ID, 10))
	}
	return b.String()
}
