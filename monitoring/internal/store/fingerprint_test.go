package store

import (
	"testing"
	"time"

	"github.com/DaisWW/Sub2ApiExt/monitoring/internal/model"
)

func TestAccountSourceFingerprintIgnoresActivityAndTokenRefresh(t *testing.T) {
	account := model.Account{
		Platform: " OpenAI ", Type: "api_key", Priority: 3, Status: "active", Schedulable: true,
		Credentials: map[string]any{
			"api_key": "key", "access_token": "old", "refresh_token": "refresh-old",
			"expires_at": "2026-08-28T10:00:00Z", "model": "gpt-test",
		},
		ProxyURL: "http://proxy.example:8080",
	}
	first := accountSourceFingerprint(account)
	if !currentSourceFingerprint(first) {
		t.Fatalf("account fingerprint is missing current version prefix: %q", first)
	}
	activity := time.Date(2026, 8, 28, 10, 0, 0, 0, time.UTC)
	account.LastActivityAt = &activity
	account.UpdatedAt = &activity
	account.Credentials["access_token"] = "new"
	account.Credentials["refresh_token"] = "refresh-new"
	account.Credentials["expires_at"] = "2026-08-29T10:00:00Z"
	if got := accountSourceFingerprint(account); got != first {
		t.Fatalf("bookkeeping changed source fingerprint: %s != %s", got, first)
	}

	account.Credentials["model"] = "gpt-other"
	if got := accountSourceFingerprint(account); got == first {
		t.Fatal("source configuration change did not change fingerprint")
	}
}

func TestGroupSourceFingerprintIgnoresRequestCount(t *testing.T) {
	account := model.Account{ID: 1, Platform: "openai", Status: "active", Schedulable: true}
	group := model.Group{
		ID: 9, Platform: "openai", Status: "active", HasActiveChannel: true,
		AccountIDs: []int64{1}, Members: []model.GroupMember{{AccountID: 1, RequestCount: 1}},
	}
	accounts := map[int64]*model.Account{1: &account}
	first := groupSourceFingerprint(group, accounts)
	group.Members[0].RequestCount = 999
	if got := groupSourceFingerprint(group, accounts); got != first {
		t.Fatalf("request count changed group fingerprint: %s != %s", got, first)
	}
}
