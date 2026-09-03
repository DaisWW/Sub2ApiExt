package store

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"

	"github.com/DaisWW/Sub2ApiExt/monitoring/internal/model"
)

// Bump this when the source identity inputs or normalization rules change.
// Prefixing the digest lets an upgrade rebaseline old watermarks once instead
// of treating an implementation change as a real upstream source update.
//
// v3 also rebaselines the first fingerprint rollout. Its timestamp watermarks
// were migration bookkeeping rather than a confirmed source change, and would
// otherwise hide valid monitoring history until a new request arrived.
const sourceFingerprintVersion = "v3"

// accountSourceFingerprint captures fields that can change where or how an
// account is probed. Activity, probe results, update timestamps and billing
// multipliers are deliberately absent: they are evidence/bookkeeping rather
// than a change to the upstream source.
func accountSourceFingerprint(account model.Account) string {
	payload := struct {
		Platform    string         `json:"platform"`
		Type        string         `json:"type"`
		Priority    int            `json:"priority"`
		Status      string         `json:"status"`
		Schedulable bool           `json:"schedulable"`
		Credentials map[string]any `json:"credentials"`
		ProxyURL    string         `json:"proxy_url"`
		ProxyError  string         `json:"proxy_error"`
	}{
		Platform:    normalizedFingerprintString(account.Platform),
		Type:        normalizedFingerprintString(account.Type),
		Priority:    account.Priority,
		Status:      normalizedFingerprintString(account.Status),
		Schedulable: account.Schedulable,
		Credentials: fingerprintCredentials(account.Credentials),
		ProxyURL:    strings.TrimSpace(account.ProxyURL),
		ProxyError:  strings.TrimSpace(account.ProxyError),
	}
	return versionedFingerprint(payload)
}

// groupSourceFingerprint captures routing inputs for a group. RequestCount is
// intentionally excluded because it changes with every request and is
// bookkeeping rather than a source/configuration change.
func groupSourceFingerprint(group model.Group, accounts map[int64]*model.Account) string {
	type memberFingerprint struct {
		AccountID          int64  `json:"account_id"`
		GroupPriority      int    `json:"group_priority"`
		AccountPriority    int    `json:"account_priority"`
		AccountStatus      string `json:"account_status"`
		AccountSchedulable bool   `json:"account_schedulable"`
		AccountSource      string `json:"account_source"`
	}
	payload := struct {
		Platform         string              `json:"platform"`
		Status           string              `json:"status"`
		HasActiveChannel bool                `json:"has_active_channel"`
		ProbeEnabled     bool                `json:"probe_enabled"`
		Members          []memberFingerprint `json:"members"`
	}{
		Platform:         normalizedFingerprintString(group.Platform),
		Status:           normalizedFingerprintString(group.Status),
		HasActiveChannel: group.HasActiveChannel,
		ProbeEnabled:     group.ProbeEnabled,
		Members:          make([]memberFingerprint, 0, len(group.AccountIDs)+len(group.Members)),
	}

	memberByID := make(map[int64]model.GroupMember, len(group.Members))
	for _, member := range group.Members {
		if _, exists := memberByID[member.AccountID]; !exists {
			memberByID[member.AccountID] = member
		}
	}
	ids := make(map[int64]struct{}, len(group.AccountIDs)+len(group.Members))
	for _, accountID := range group.AccountIDs {
		if accountID > 0 {
			ids[accountID] = struct{}{}
		}
	}
	for _, member := range group.Members {
		if member.AccountID > 0 {
			ids[member.AccountID] = struct{}{}
		}
	}
	sortedIDs := make([]int64, 0, len(ids))
	for accountID := range ids {
		sortedIDs = append(sortedIDs, accountID)
	}
	sort.Slice(sortedIDs, func(i, j int) bool { return sortedIDs[i] < sortedIDs[j] })
	for _, accountID := range sortedIDs {
		member := memberByID[accountID]
		account := accounts[accountID]
		entry := memberFingerprint{
			AccountID:       accountID,
			GroupPriority:   member.GroupPriority,
			AccountPriority: member.AccountPriority,
		}
		if account != nil {
			entry.AccountPriority = account.Priority
			entry.AccountStatus = normalizedFingerprintString(account.Status)
			entry.AccountSchedulable = account.Schedulable
			entry.AccountSource = account.SourceFingerprint
			if entry.AccountSource == "" {
				entry.AccountSource = accountSourceFingerprint(*account)
			}
		}
		payload.Members = append(payload.Members, entry)
	}
	return versionedFingerprint(payload)
}

func versionedFingerprint(value any) string {
	return sourceFingerprintVersion + ":" + digestFingerprint(value)
}

func currentSourceFingerprint(value string) bool {
	return strings.HasPrefix(strings.TrimSpace(value), sourceFingerprintVersion+":")
}

func normalizedFingerprintString(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

// fingerprintCredentials removes values that are refreshed as part of normal
// OAuth/token bookkeeping. API keys, endpoints, model mappings and unknown
// provider-specific settings remain part of the source identity.
func fingerprintCredentials(credentials map[string]any) map[string]any {
	value, ok := normalizeFingerprintValue(credentials)
	if !ok {
		return map[string]any{}
	}
	result, ok := value.(map[string]any)
	if !ok {
		return map[string]any{}
	}
	return result
}

func normalizeFingerprintValue(value any) (any, bool) {
	switch typed := value.(type) {
	case map[string]any:
		result := make(map[string]any, len(typed))
		for key, child := range typed {
			if ephemeralCredentialKey(key) {
				continue
			}
			normalized, ok := normalizeFingerprintValue(child)
			if ok {
				result[key] = normalized
			}
		}
		return result, true
	case []any:
		result := make([]any, 0, len(typed))
		for _, child := range typed {
			normalized, ok := normalizeFingerprintValue(child)
			if ok {
				result = append(result, normalized)
			}
		}
		return result, true
	default:
		return value, true
	}
}

func ephemeralCredentialKey(key string) bool {
	switch strings.ToLower(strings.TrimSpace(key)) {
	case "access_token", "refresh_token", "id_token", "expires_at", "expires_in",
		"token_expires_at", "last_refresh_at", "access_token_expires_at":
		return true
	default:
		return false
	}
}

func digestFingerprint(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		// Values originate in PostgreSQL JSON and should always be marshalable.
		// Keep a non-empty deterministic marker if a future provider adds an
		// unsupported value type, so it cannot silently disable watermarking.
		encoded = []byte("fingerprint-error")
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}
