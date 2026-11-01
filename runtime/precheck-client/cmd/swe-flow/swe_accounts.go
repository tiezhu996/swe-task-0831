package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	client "example.com/swe-precheck-client"
)

// A single platform identity. Password/session values live only in the private
// 0600 config file; the HTTP API exposes username, real_name and role only.
type sweAccount struct {
	Username string `json:"username"`
	Password string `json:"password,omitempty"`
	Session  string `json:"session,omitempty"`
	CSRF     string `json:"csrf,omitempty"`
	RealName string `json:"real_name,omitempty"`
	Role     string `json:"role,omitempty"`
}

type sweAccountStore struct {
	Path     string
	Accounts []sweAccount
	Active   string
}

// sweAccountMu serialises read-modify-write of the account list (add, switch,
// remove, session refresh). Authentication itself happens outside the lock.
var sweAccountMu sync.Mutex

func sweConfigPath() string {
	if path := strings.TrimSpace(os.Getenv("SWE_TASK_CONFIG")); path != "" {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "swe-task", "config.json")
}

func sweEnvOverride() bool {
	username := strings.TrimSpace(os.Getenv("SWE_USERNAME"))
	password := os.Getenv("SWE_PASSWORD")
	session := strings.TrimSpace(os.Getenv("SWE_SESSION"))
	csrf := strings.TrimSpace(os.Getenv("SWE_CSRF"))
	return (username != "" && password != "") || (session != "" && csrf != "")
}

func activeSWEUsername() string {
	credentials, err := localCredentials()
	if err != nil {
		return ""
	}
	return credentials.Username
}

func (s *sweAccountStore) account(username string) *sweAccount {
	for index := range s.Accounts {
		if s.Accounts[index].Username == username {
			return &s.Accounts[index]
		}
	}
	return nil
}

func (s *sweAccountStore) activeAccount() *sweAccount {
	if s.Active != "" {
		if account := s.account(s.Active); account != nil {
			return account
		}
	}
	if len(s.Accounts) > 0 {
		return &s.Accounts[0]
	}
	return nil
}

func (a sweAccount) credentials() platformCredentials {
	return platformCredentials{Username: a.Username, Password: a.Password, Session: a.Session, CSRF: a.CSRF}
}

// readSWEConfigRaw preserves unrelated keys (notably github) when the account
// list is rewritten.
func readSWEConfigRaw(path string) (map[string]json.RawMessage, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, os.ErrNotExist
	}
	if err != nil || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0077 != 0 {
		return nil, fmt.Errorf("私有认证配置不存在或权限不安全（目录0700、文件0600）")
	}
	dirInfo, err := os.Lstat(filepath.Dir(path))
	if err != nil || dirInfo.Mode()&os.ModeSymlink != 0 || dirInfo.Mode().Perm()&0077 != 0 {
		return nil, fmt.Errorf("私有认证配置目录权限不安全（需要0700）")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("私有认证配置不可读")
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("私有认证配置格式不合法")
	}
	var schema int
	if err := json.Unmarshal(raw["schema_version"], &schema); err != nil || schema != 1 {
		return nil, fmt.Errorf("私有认证配置版本不支持")
	}
	return raw, nil
}

func readSWEAccountStore() (sweAccountStore, error) {
	store := sweAccountStore{Path: sweConfigPath()}
	raw, err := readSWEConfigRaw(store.Path)
	if errors.Is(err, os.ErrNotExist) {
		return store, nil
	}
	if err != nil {
		return store, err
	}
	rawAccounts, accountsPresent := raw["swe_accounts"]
	if accountsPresent {
		if err := json.Unmarshal(rawAccounts, &store.Accounts); err != nil {
			return store, fmt.Errorf("私有认证配置的账号列表格式不合法")
		}
	}
	if rawActive, ok := raw["swe_active"]; ok {
		_ = json.Unmarshal(rawActive, &store.Active)
	}
	if !accountsPresent {
		// Migrate the pre-multi-account schema in memory. It is written back the
		// first time the operator adds, switches or removes an account.
		var legacy platformCredentials
		if rawSwe, ok := raw["swe"]; ok {
			_ = json.Unmarshal(rawSwe, &legacy)
		}
		if legacy.Username != "" {
			store.Accounts = append(store.Accounts, sweAccount{
				Username: legacy.Username, Password: legacy.Password,
				Session: legacy.Session, CSRF: legacy.CSRF,
			})
			if store.Active == "" {
				store.Active = legacy.Username
			}
		}
	}
	seen := map[string]bool{}
	clean := make([]sweAccount, 0, len(store.Accounts))
	for _, account := range store.Accounts {
		account.Username = strings.TrimSpace(account.Username)
		if account.Username == "" || seen[account.Username] {
			continue
		}
		seen[account.Username] = true
		clean = append(clean, account)
	}
	store.Accounts = clean
	if store.Active != "" && store.account(store.Active) == nil {
		store.Active = ""
	}
	if store.Active == "" && len(store.Accounts) > 0 {
		store.Active = store.Accounts[0].Username
	}
	return store, nil
}

func writeSWEAccountStore(store sweAccountStore) error {
	path := store.Path
	if path == "" {
		path = sweConfigPath()
	}
	if path == "" {
		return fmt.Errorf("无法定位本机私有认证配置")
	}
	dir := filepath.Dir(path)
	if info, statErr := os.Stat(dir); errors.Is(statErr, os.ErrNotExist) {
		if err := os.MkdirAll(dir, 0700); err != nil {
			return fmt.Errorf("无法建立私有认证配置目录")
		}
		if err := os.Chmod(dir, 0700); err != nil {
			return fmt.Errorf("私有认证配置目录权限设置失败")
		}
	} else if statErr != nil || !info.IsDir() {
		return fmt.Errorf("私有认证配置目录不可用")
	}
	raw, err := readSWEConfigRaw(path)
	if errors.Is(err, os.ErrNotExist) {
		raw = map[string]json.RawMessage{}
	} else if err != nil {
		return err
	}
	if raw == nil {
		raw = map[string]json.RawMessage{}
	}
	raw["schema_version"] = json.RawMessage("1")
	accounts, err := json.Marshal(store.Accounts)
	if err != nil {
		return err
	}
	active, err := json.Marshal(store.Active)
	if err != nil {
		return err
	}
	raw["swe_accounts"] = accounts
	raw["swe_active"] = active
	var mirror platformCredentials
	if account := store.activeAccount(); account != nil {
		mirror = account.credentials()
	}
	mirrorJSON, err := json.Marshal(mirror)
	if err != nil {
		return err
	}
	raw["swe"] = mirrorJSON
	if err := client.SaveLocalJSON(path, raw); err != nil {
		return fmt.Errorf("私有认证配置保存失败")
	}
	if err := os.Chmod(path, 0600); err != nil {
		return fmt.Errorf("私有认证配置权限修正失败")
	}
	return nil
}

// updateSWEAccountSession caches a session pair after a successful login. It is
// best effort: a failure to write the cache must not fail the platform request.
func updateSWEAccountSession(username, session, csrf string) {
	if username == "" || session == "" || csrf == "" || sweEnvOverride() {
		return
	}
	sweAccountMu.Lock()
	defer sweAccountMu.Unlock()
	store, err := readSWEAccountStore()
	if err != nil {
		return
	}
	account := store.account(username)
	if account == nil {
		return
	}
	account.Session, account.CSRF = session, csrf
	_ = writeSWEAccountStore(store)
}

func platformLogin(ctx context.Context, username, password string) (client.User, string, string, error) {
	c, err := client.New("", nil)
	if err != nil {
		return client.User{}, "", "", fmt.Errorf("无法建立平台客户端")
	}
	user, err := c.Login(ctx, username, password)
	if err != nil || user.Username != username {
		return client.User{}, "", "", fmt.Errorf("登录失败，请检查账号或密码")
	}
	session, csrf, _ := c.SessionCookies()
	return user, session, csrf, nil
}

// authenticateSWEAccount prefers the cached session, then falls back to the
// stored password. It is used when switching so a stale session cannot silently
// become the active identity.
func authenticateSWEAccount(ctx context.Context, account sweAccount) (client.User, string, string, error) {
	if account.Session != "" && account.CSRF != "" {
		c, err := client.New("", nil)
		if err == nil {
			c.SetSessionCookies(account.Session, account.CSRF)
			if user, meErr := c.Me(ctx); meErr == nil && user.Username == account.Username {
				session, csrf, _ := c.SessionCookies()
				return user, session, csrf, nil
			}
		}
	}
	if account.Password == "" {
		return client.User{}, "", "", fmt.Errorf("该账号没有可用的密码或会话")
	}
	return platformLogin(ctx, account.Username, account.Password)
}
