package accounts

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const SchemaVersion = 1

var (
	ErrNotFound         = errors.New("account not found")
	ErrAmbiguous        = errors.New("account lookup is ambiguous")
	ErrUnsafePath       = errors.New("unsafe account store path")
	ErrUnknownActive    = errors.New("active account identity is unknown")
	ErrActiveAccount    = errors.New("cannot remove the active account")
	ErrIdentityMismatch = errors.New("credential identity does not match account")
	ErrInvalidSchema    = errors.New("unsupported account schema")
	ErrInvalidAlias     = errors.New("invalid account alias")
	ErrAliasConflict    = errors.New("account alias conflicts with existing lookup")
	ErrReadOnly         = errors.New("account store is read-only")
)

// Account contains list-safe account metadata. It intentionally has no
// credential-bearing fields.
type Account struct {
	ID        string `json:"id"`
	Alias     string `json:"alias,omitempty"`
	Email     string `json:"email,omitempty"`
	UserID    string `json:"user_id"`
	AccountID string `json:"account_id"`
}

// AccountIssue is a sanitized problem with one saved account entry. It never
// contains credential data or filesystem paths.
type AccountIssue struct {
	ID      string `json:"id,omitempty"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// ActiveIdentity is supplied by the caller from an authoritative live probe.
// Known with empty IDs means the caller authoritatively observed no active
// native ChatGPT identity. A partial pair is treated as unknown.
type ActiveIdentity struct {
	Known     bool
	UserID    string
	AccountID string
}

type Store struct {
	root     string
	readOnly bool
}

type envelope struct {
	SchemaVersion int             `json:"schema_version"`
	Account       Account         `json:"account"`
	Credentials   json.RawMessage `json:"credentials"`
}

func Open(root string) (*Store, error) {
	clean, err := cleanRoot(root)
	if err != nil {
		return nil, err
	}
	info, err := os.Lstat(clean)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if err := os.MkdirAll(clean, 0o700); err != nil {
			return nil, fmt.Errorf("create account store: %w", err)
		}
		info, err = os.Lstat(clean)
	case err != nil:
		return nil, fmt.Errorf("inspect account store: %w", err)
	}
	if err != nil {
		return nil, fmt.Errorf("inspect account store: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return nil, ErrUnsafePath
	}
	if err := os.Chmod(clean, 0o700); err != nil {
		return nil, fmt.Errorf("secure account store: %w", err)
	}
	return &Store{root: clean}, nil
}

// OpenReadOnly opens an existing store without changing its directory or file
// permissions. A missing store is represented as empty and is not created.
func OpenReadOnly(root string) (*Store, error) {
	clean, err := cleanRoot(root)
	if err != nil {
		return nil, err
	}
	info, err := os.Lstat(clean)
	if errors.Is(err, fs.ErrNotExist) {
		return &Store{root: clean, readOnly: true}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("inspect account store: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || info.Mode().Perm() != 0o700 {
		return nil, ErrUnsafePath
	}
	return &Store{root: clean, readOnly: true}, nil
}

func (s *Store) List() ([]Account, error) {
	entries, err := os.ReadDir(s.root)
	if s.readOnly && errors.Is(err, fs.ErrNotExist) {
		return []Account{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list accounts: %w", err)
	}
	accounts := make([]Account, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".json") {
			continue
		}
		id := strings.TrimSuffix(name, ".json")
		if !validUUID(id) || entry.Type()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("%w: invalid account entry", ErrUnsafePath)
		}
		env, err := s.load(id)
		if err != nil {
			return nil, err
		}
		accounts = append(accounts, env.Account)
	}
	sort.Slice(accounts, func(i, j int) bool {
		if accounts[i].Alias != accounts[j].Alias {
			return accounts[i].Alias < accounts[j].Alias
		}
		return accounts[i].ID < accounts[j].ID
	})
	return accounts, nil
}

// ListPartial returns every healthy account it can inspect and sanitized issues
// for malformed entries. Mutations continue to use strict List and fail closed.
func (s *Store) ListPartial() ([]Account, []AccountIssue, error) {
	entries, err := os.ReadDir(s.root)
	if s.readOnly && errors.Is(err, fs.ErrNotExist) {
		return []Account{}, []AccountIssue{}, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("list accounts: %w", err)
	}
	listed := make([]Account, 0, len(entries))
	issues := make([]AccountIssue, 0)
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".json") {
			continue
		}
		id := strings.TrimSuffix(name, ".json")
		if !validUUID(id) || entry.Type()&os.ModeSymlink != 0 {
			issueID := ""
			if validUUID(id) {
				issueID = id
			}
			issues = append(issues, AccountIssue{ID: issueID, Code: "unsafe_entry", Message: "account entry has an invalid name or type"})
			continue
		}
		env, loadErr := s.load(id)
		if loadErr != nil {
			issues = append(issues, classifyAccountIssue(id, loadErr))
			continue
		}
		listed = append(listed, env.Account)
	}
	sort.Slice(listed, func(i, j int) bool {
		if listed[i].Alias != listed[j].Alias {
			return listed[i].Alias < listed[j].Alias
		}
		return listed[i].ID < listed[j].ID
	})
	sort.Slice(issues, func(i, j int) bool {
		if issues[i].ID != issues[j].ID {
			return issues[i].ID < issues[j].ID
		}
		return issues[i].Code < issues[j].Code
	})
	return listed, issues, nil
}

func (s *Store) Find(query string) (Account, error) {
	query = strings.TrimSpace(query)
	if unsafeQuery(query) {
		return Account{}, ErrUnsafePath
	}
	if validUUID(query) {
		env, err := s.load(query)
		if err != nil {
			return Account{}, err
		}
		return env.Account, nil
	}
	accounts, err := s.List()
	if err != nil {
		return Account{}, err
	}
	var matches []Account
	for _, account := range accounts {
		if account.Alias == query || account.Email == query {
			matches = append(matches, account)
		}
	}
	switch len(matches) {
	case 0:
		return Account{}, ErrNotFound
	case 1:
		return matches[0], nil
	default:
		return Account{}, ErrAmbiguous
	}
}

func (s *Store) Save(auth NativeAuth, alias string) (Account, error) {
	if s.readOnly {
		return Account{}, ErrReadOnly
	}
	if err := validateNativeAuth(auth); err != nil {
		return Account{}, err
	}
	accounts, err := s.List()
	if err != nil {
		return Account{}, err
	}
	explicitAlias := alias != ""
	if explicitAlias {
		if err := ValidateAlias(alias); err != nil {
			return Account{}, err
		}
	}
	for _, account := range accounts {
		if sameIdentity(account.UserID, account.AccountID, auth.UserID, auth.AccountID) {
			if explicitAlias {
				if err := aliasAvailable(alias, account.ID, accounts); err != nil {
					return Account{}, err
				}
				account.Alias = alias
			}
			account.Email = auth.Email
			if err := s.write(envelope{SchemaVersion: SchemaVersion, Account: account, Credentials: auth.raw}); err != nil {
				return Account{}, err
			}
			return account, nil
		}
	}

	if !explicitAlias {
		alias = auth.Email
	}
	if err := ValidateAlias(alias); err != nil {
		return Account{}, err
	}
	if explicitAlias {
		if err := aliasAvailable(alias, "", accounts); err != nil {
			return Account{}, err
		}
	}
	id, err := s.unusedID()
	if err != nil {
		return Account{}, err
	}
	account := Account{ID: id, Alias: alias, Email: auth.Email, UserID: auth.UserID, AccountID: auth.AccountID}
	if err := s.write(envelope{SchemaVersion: SchemaVersion, Account: account, Credentials: auth.raw}); err != nil {
		return Account{}, err
	}
	return account, nil
}

// Rename changes list-safe metadata only and preserves the stored credential
// document for the resolved account.
func (s *Store) Rename(query, alias string) (Account, error) {
	if s.readOnly {
		return Account{}, ErrReadOnly
	}
	if err := ValidateAlias(alias); err != nil {
		return Account{}, err
	}
	account, err := s.Find(query)
	if err != nil {
		return Account{}, err
	}
	accounts, err := s.List()
	if err != nil {
		return Account{}, err
	}
	if err := aliasAvailable(alias, account.ID, accounts); err != nil {
		return Account{}, err
	}
	env, err := s.load(account.ID)
	if err != nil {
		return Account{}, err
	}
	env.Account.Alias = alias
	if err := s.write(env); err != nil {
		return Account{}, err
	}
	return env.Account, nil
}

func (s *Store) Credentials(query string) ([]byte, error) {
	account, err := s.Find(query)
	if err != nil {
		return nil, err
	}
	env, err := s.load(account.ID)
	if err != nil {
		return nil, err
	}
	return append([]byte(nil), env.Credentials...), nil
}

func (s *Store) UpdateCredentials(query string, auth NativeAuth) (Account, error) {
	if s.readOnly {
		return Account{}, ErrReadOnly
	}
	if err := validateNativeAuth(auth); err != nil {
		return Account{}, err
	}
	account, err := s.Find(query)
	if err != nil {
		return Account{}, err
	}
	if !sameIdentity(account.UserID, account.AccountID, auth.UserID, auth.AccountID) {
		return Account{}, ErrIdentityMismatch
	}
	account.Email = auth.Email
	if err := s.write(envelope{SchemaVersion: SchemaVersion, Account: account, Credentials: auth.raw}); err != nil {
		return Account{}, err
	}
	return account, nil
}

func (s *Store) Remove(query string, active ActiveIdentity) error {
	if s.readOnly {
		return ErrReadOnly
	}
	account, err := s.Find(query)
	if err != nil {
		return err
	}
	active.UserID = strings.TrimSpace(active.UserID)
	active.AccountID = strings.TrimSpace(active.AccountID)
	if !active.Known || (active.UserID == "") != (active.AccountID == "") {
		return ErrUnknownActive
	}
	if sameIdentity(account.UserID, account.AccountID, active.UserID, active.AccountID) {
		return ErrActiveAccount
	}
	path := s.accountPath(account.ID)
	if err := rejectSymlink(path); err != nil {
		return err
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("remove account: %w", err)
	}
	return syncDir(s.root)
}

func (s *Store) load(id string) (envelope, error) {
	if !validUUID(id) {
		return envelope{}, ErrUnsafePath
	}
	path := s.accountPath(id)
	if err := rejectSymlink(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return envelope{}, ErrNotFound
		}
		return envelope{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return envelope{}, ErrNotFound
		}
		return envelope{}, fmt.Errorf("read account: %w", err)
	}
	var env envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return envelope{}, fmt.Errorf("parse account %s: %w", id, err)
	}
	if env.SchemaVersion != SchemaVersion {
		return envelope{}, fmt.Errorf("%w: %d", ErrInvalidSchema, env.SchemaVersion)
	}
	if env.Account.ID != id || !validUUID(env.Account.ID) {
		return envelope{}, fmt.Errorf("%w: account ID does not match filename", ErrUnsafePath)
	}
	if invalidAlias(env.Account.Alias) {
		return envelope{}, ErrInvalidAlias
	}
	if strings.TrimSpace(env.Account.UserID) == "" || strings.TrimSpace(env.Account.AccountID) == "" || !json.Valid(env.Credentials) {
		return envelope{}, errors.New("invalid account envelope")
	}
	auth, err := ParseNativeAuth(env.Credentials)
	if err != nil {
		return envelope{}, fmt.Errorf("validate account credentials: %w", err)
	}
	if !sameIdentity(env.Account.UserID, env.Account.AccountID, auth.UserID, auth.AccountID) {
		return envelope{}, ErrIdentityMismatch
	}
	return env, nil
}

func (s *Store) write(env envelope) error {
	if !validUUID(env.Account.ID) || env.SchemaVersion != SchemaVersion || !json.Valid(env.Credentials) {
		return errors.New("invalid account envelope")
	}
	path := s.accountPath(env.Account.ID)
	if err := rejectSymlinkUnlessMissing(path); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(s.root, ".account-*.tmp")
	if err != nil {
		return fmt.Errorf("create account temporary file: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	encoder := json.NewEncoder(tmp)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(env); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("encode account: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync account: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close account: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("commit account: %w", err)
	}
	return syncDir(s.root)
}

func (s *Store) unusedID() (string, error) {
	for range 8 {
		id, err := newUUID()
		if err != nil {
			return "", err
		}
		_, err = os.Lstat(s.accountPath(id))
		if errors.Is(err, fs.ErrNotExist) {
			return id, nil
		}
		if err != nil {
			return "", err
		}
	}
	return "", errors.New("could not allocate unique account ID")
}

func newUUID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("generate account ID: %w", err)
	}
	value[6] = value[6]&0x0f | 0x40
	value[8] = value[8]&0x3f | 0x80
	encoded := make([]byte, 36)
	hex.Encode(encoded[0:8], value[0:4])
	encoded[8] = '-'
	hex.Encode(encoded[9:13], value[4:6])
	encoded[13] = '-'
	hex.Encode(encoded[14:18], value[6:8])
	encoded[18] = '-'
	hex.Encode(encoded[19:23], value[8:10])
	encoded[23] = '-'
	hex.Encode(encoded[24:36], value[10:16])
	return string(encoded), nil
}

func validUUID(value string) bool {
	if len(value) != 36 || value[8] != '-' || value[13] != '-' || value[18] != '-' || value[23] != '-' || value[14] != '4' {
		return false
	}
	if value[19] != '8' && value[19] != '9' && value[19] != 'a' && value[19] != 'b' {
		return false
	}
	for i, char := range value {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			continue
		}
		if !strings.ContainsRune("0123456789abcdef", char) {
			return false
		}
	}
	return true
}

func cleanRoot(root string) (string, error) {
	if strings.TrimSpace(root) == "" {
		return "", ErrUnsafePath
	}
	for _, part := range strings.FieldsFunc(root, func(r rune) bool { return r == '/' || r == '\\' }) {
		if part == ".." {
			return "", ErrUnsafePath
		}
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("resolve account store: %w", err)
	}
	return filepath.Clean(abs), nil
}

func unsafeQuery(query string) bool {
	return query == "" || query == "." || query == ".." || strings.ContainsAny(query, `/\\`)
}

// ValidateAlias checks persisted alias syntax without reading or mutating a
// store. An empty alias is valid and means no explicit alias was supplied.
func ValidateAlias(alias string) error {
	if invalidAlias(alias) {
		return ErrInvalidAlias
	}
	return nil
}

func aliasAvailable(alias, ownID string, accounts []Account) error {
	if alias == "" {
		return nil
	}
	for _, account := range accounts {
		if account.ID == ownID {
			continue
		}
		if account.Alias == alias || account.Email == alias {
			return ErrAliasConflict
		}
	}
	return nil
}

func classifyAccountIssue(id string, err error) AccountIssue {
	issue := AccountIssue{ID: id, Code: "invalid_account", Message: "account entry could not be validated"}
	switch {
	case errors.Is(err, ErrUnsafePath):
		issue.Code, issue.Message = "unsafe_entry", "account entry is not a protected regular file"
	case errors.Is(err, ErrInvalidSchema):
		issue.Code, issue.Message = "invalid_schema", "account entry uses an unsupported schema"
	case errors.Is(err, ErrInvalidAlias):
		issue.Code, issue.Message = "invalid_alias", "account entry has an invalid alias"
	case errors.Is(err, ErrIdentityMismatch), errors.Is(err, ErrNativeIdentity), errors.Is(err, ErrConflictingIdentity):
		issue.Code, issue.Message = "identity_mismatch", "account credential identity is invalid"
	}
	return issue
}

func invalidAlias(alias string) bool {
	return alias != "" && (alias != strings.TrimSpace(alias) || unsafeQuery(alias) || validUUID(alias))
}

func (s *Store) accountPath(id string) string { return filepath.Join(s.root, id+".json") }

func rejectSymlink(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return ErrUnsafePath
	}
	if info.Mode().Perm() != 0o600 {
		return fmt.Errorf("%w: account file permissions are %04o, want 0600", ErrUnsafePath, info.Mode().Perm())
	}
	return nil
}

func rejectSymlinkUnlessMissing(path string) error {
	err := rejectSymlink(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

func validateNativeAuth(auth NativeAuth) error {
	if strings.TrimSpace(auth.UserID) == "" || strings.TrimSpace(auth.AccountID) == "" || !json.Valid(auth.raw) {
		return ErrNativeIdentity
	}
	parsed, err := ParseNativeAuth(auth.raw)
	if err != nil {
		return err
	}
	if parsed.Email != auth.Email || parsed.UserID != auth.UserID || parsed.AccountID != auth.AccountID {
		return ErrIdentityMismatch
	}
	return nil
}

func sameIdentity(leftUser, leftAccount, rightUser, rightAccount string) bool {
	return leftUser != "" && leftAccount != "" && leftUser == rightUser && leftAccount == rightAccount
}

func syncDir(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	if err := dir.Sync(); err != nil {
		return fmt.Errorf("sync account store: %w", err)
	}
	return nil
}
