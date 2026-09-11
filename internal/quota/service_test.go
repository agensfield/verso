package quota

import (
	"context"
	"encoding/base64"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/agensfield/verso/internal/accounts"
	"github.com/agensfield/verso/internal/auth"
)

type fakeClient struct {
	refreshes, usages int
	next              []byte
	usageErr          error
}

func (f *fakeClient) Refresh(context.Context, []byte) ([]byte, error) {
	f.refreshes++
	return f.next, nil
}
func (f *fakeClient) Usage(context.Context, []byte) (auth.Quota, error) {
	f.usages++
	v := false
	return auth.Quota{Exhausted: &v}, f.usageErr
}
func credentials(exp int64, user, account string) []byte {
	claims := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"email":"same@example.test","https://api.openai.com/auth":{"chatgpt_user_id":%q,"chatgpt_account_id":%q}}`, user, account)))
	access := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d}`, exp)))
	return []byte(fmt.Sprintf(`{"tokens":{"id_token":"e30.%s.sig","access_token":"e30.%s.sig","refresh_token":"synthetic-only","account_id":%q}}`, claims, access, account))
}
func fixture(t *testing.T) (Service, *fakeClient, accounts.Account) {
	t.Helper()
	root := t.TempDir()
	store, err := accounts.Open(filepath.Join(root, "accounts"))
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := accounts.ParseNativeAuth(credentials(1, "u", "a"))
	if err != nil {
		t.Fatal(err)
	}
	acc, err := store.Save(parsed, "")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeClient{next: credentials(9999999999, "u", "a")}
	return Service{Root: root, Store: store, Client: f, Now: func() time.Time { return time.Unix(2000, 0) }, Active: accounts.ActiveIdentity{Known: true, UserID: "other", AccountID: "other"}}, f, acc
}
func TestInactiveRefreshPersistsAndCacheAvoidsRepeat(t *testing.T) {
	s, f, acc := fixture(t)
	e, err := s.Refresh(context.Background(), acc.ID, false)
	if err != nil || e.Quota == nil || f.refreshes != 1 || f.usages != 1 {
		t.Fatalf("%+v %v %+v", e, err, f)
	}
	raw, err := s.Store.Credentials(acc.ID)
	expired, expiryErr := auth.AccessTokenExpired(raw, s.now())
	if err != nil || expiryErr != nil || expired {
		t.Fatal("refreshed credentials not persisted")
	}
	if _, err = s.Refresh(context.Background(), acc.ID, false); err != nil {
		t.Fatal(err)
	}
	if f.refreshes != 1 || f.usages != 1 {
		t.Fatal("fresh cache made network requests")
	}
}
func TestActiveAndUnknownNeverUseInactiveRefresh(t *testing.T) {
	for _, known := range []bool{true, false} {
		s, f, acc := fixture(t)
		s.Active = accounts.ActiveIdentity{Known: known, UserID: acc.UserID, AccountID: acc.AccountID}
		calls := 0
		s.ActiveUsage = func(context.Context) (auth.Quota, error) { calls++; return auth.Quota{}, nil }
		_, err := s.Refresh(context.Background(), acc.ID, true)
		if err != nil || f.refreshes != 0 || f.usages != 0 {
			t.Fatalf("%v %+v", err, f)
		}
		if known && calls != 1 || !known && calls != 0 {
			t.Fatal("wrong active delegate behavior")
		}
	}
}
func TestSameEmailDifferentWorkspaceIsInactive(t *testing.T) {
	s, f, acc := fixture(t)
	s.Active = accounts.ActiveIdentity{Known: true, UserID: acc.UserID, AccountID: "different"}
	if _, err := s.Refresh(context.Background(), acc.ID, true); err != nil {
		t.Fatal(err)
	}
	if f.refreshes != 1 {
		t.Fatal("email collapsed distinct workspaces")
	}
}
func TestRefreshIdentityDriftCannotReplaceSavedAccount(t *testing.T) {
	s, f, acc := fixture(t)
	before, _ := s.Store.Credentials(acc.ID)
	f.next = credentials(9999999999, "u", "other")
	e, err := s.Refresh(context.Background(), acc.ID, true)
	if err != nil || e.Quota != nil || e.Warning == "" || f.usages != 0 {
		t.Fatalf("%+v %v", e, err)
	}
	after, _ := s.Store.Credentials(acc.ID)
	if string(before) != string(after) {
		t.Fatal("identity drift persisted")
	}
}
func TestFailureRetainsStaleQuotaAndLoginRequired(t *testing.T) {
	s, f, acc := fixture(t)
	first, err := s.Refresh(context.Background(), acc.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	f.usageErr = auth.ErrLoginRequired
	next, err := s.Refresh(context.Background(), acc.ID, true)
	if err != nil || !next.LoginRequired || next.Quota == nil || *next.Quota.Exhausted != *first.Quota.Exhausted {
		t.Fatalf("%+v %v", next, err)
	}
}

func TestPartialActiveIdentityNeverRefreshes(t *testing.T) {
	for _, identity := range []accounts.ActiveIdentity{{Known: true, UserID: "u"}, {Known: true, AccountID: "a"}} {
		s, f, acc := fixture(t)
		s.Active = identity
		e, err := s.Refresh(context.Background(), acc.ID, true)
		if err != nil || f.refreshes != 0 || f.usages != 0 || e.Warning == "" {
			t.Fatalf("%+v %v %+v", e, err, f)
		}
	}
}

func TestFailedFetchPreservesSuccessTimeAndMarksStale(t *testing.T) {
	s, f, acc := fixture(t)
	first, err := s.Refresh(context.Background(), acc.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	later := s.now().Add(2 * time.Minute)
	s.Now = func() time.Time { return later }
	f.usageErr = auth.ErrNetwork
	next, err := s.Refresh(context.Background(), acc.ID, true)
	if err != nil || !next.Stale || !next.CheckedAt.Equal(first.CheckedAt) || !next.AttemptedAt.Equal(later) {
		t.Fatalf("%+v %v", next, err)
	}
	cached, _, err := s.Cached(acc.ID)
	if err != nil || !cached.Stale || !cached.CheckedAt.Equal(first.CheckedAt) {
		t.Fatalf("%+v %v", cached, err)
	}
}
