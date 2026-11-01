package main

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"time"
	"unicode"
)

const maxSWEAccounts = 8

type sweAccountView struct {
	Username string `json:"username"`
	RealName string `json:"real_name,omitempty"`
	Role     string `json:"role,omitempty"`
	Active   bool   `json:"active"`
}

func validSWEUsername(value string) bool {
	if len(value) < 1 || len(value) > 64 {
		return false
	}
	for _, r := range value {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '.' && r != '_' && r != '@' && r != '-' {
			return false
		}
	}
	return true
}

func (w *workbenchServer) sweAccountsPayload(store sweAccountStore) map[string]any {
	views := make([]sweAccountView, 0, len(store.Accounts))
	for _, account := range store.Accounts {
		views = append(views, sweAccountView{
			Username: account.Username,
			RealName: account.RealName,
			Role:     account.Role,
			Active:   account.Username == store.Active,
		})
	}
	sort.SliceStable(views, func(i, j int) bool { return views[i].Active && !views[j].Active })
	return map[string]any{
		"accounts":     views,
		"active":       store.Active,
		"env_override": sweEnvOverride(),
	}
}

func (w *workbenchServer) resetPlatformCaches() {
	w.mu.Lock()
	w.prechecks = nil
	w.prechecksAt = time.Time{}
	w.precheckHashes = map[int]cachedPrecheckBinding{}
	w.prechecksRefreshing = false
	w.prechecksError = false
	w.prechecksAttemptAt = time.Time{}
	w.gate = platformGate{}
	w.mu.Unlock()
}

func (w *workbenchServer) listSWEAccounts(out http.ResponseWriter, _ *http.Request) {
	store, err := readSWEAccountStore()
	if err != nil {
		workbenchError(out, 500, "账号配置不可读，请检查私有认证文件权限")
		return
	}
	writeWorkbenchJSON(out, 200, w.sweAccountsPayload(store))
}

func (w *workbenchServer) addSWEAccount(out http.ResponseWriter, r *http.Request) {
	if sweEnvOverride() {
		workbenchError(out, 409, "当前由环境变量提供平台凭据；先在启动环境移除 SWE_* 后再切换账号")
		return
	}
	var input struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if decodeWorkbenchJSON(r, &input) != nil {
		workbenchError(out, 400, "账号格式不正确")
		return
	}
	username := strings.TrimSpace(input.Username)
	if !validSWEUsername(username) {
		workbenchError(out, 400, "账号只能包含字母、数字和 . _ @ -，长度 1–64")
		return
	}
	if len(input.Password) < 1 || len(input.Password) > 256 {
		workbenchError(out, 400, "密码长度必须在 1–256 之间")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()
	user, session, csrf, err := platformLogin(ctx, username, input.Password)
	if err != nil {
		workbenchError(out, 409, "登录失败，请检查账号或密码，或稍后重试")
		return
	}
	sweAccountMu.Lock()
	store, err := readSWEAccountStore()
	if err != nil {
		sweAccountMu.Unlock()
		workbenchError(out, 500, "账号配置不可读，请检查私有认证文件权限")
		return
	}
	if len(store.Accounts) >= maxSWEAccounts && store.account(username) == nil {
		sweAccountMu.Unlock()
		workbenchError(out, 409, "最多保存 8 个账号，请先移除不用的账号")
		return
	}
	account := store.account(username)
	if account == nil {
		store.Accounts = append(store.Accounts, sweAccount{Username: username})
		account = &store.Accounts[len(store.Accounts)-1]
	}
	account.Password = input.Password
	account.RealName = user.RealName
	account.Role = user.Role
	account.Session = session
	account.CSRF = csrf
	store.Active = username
	if err := writeSWEAccountStore(store); err != nil {
		sweAccountMu.Unlock()
		workbenchError(out, 500, "账号配置保存失败")
		return
	}
	sweAccountMu.Unlock()
	w.resetPlatformCaches()
	writeWorkbenchJSON(out, 200, w.sweAccountsPayload(store))
}

func (w *workbenchServer) switchSWEAccount(out http.ResponseWriter, r *http.Request) {
	if sweEnvOverride() {
		workbenchError(out, 409, "当前由环境变量提供平台凭据；先在启动环境移除 SWE_* 后再切换账号")
		return
	}
	var input struct {
		Username string `json:"username"`
	}
	if decodeWorkbenchJSON(r, &input) != nil {
		workbenchError(out, 400, "账号格式不正确")
		return
	}
	username := strings.TrimSpace(input.Username)
	if !validSWEUsername(username) {
		workbenchError(out, 400, "账号格式不正确")
		return
	}
	sweAccountMu.Lock()
	store, err := readSWEAccountStore()
	if err != nil {
		sweAccountMu.Unlock()
		workbenchError(out, 500, "账号配置不可读，请检查私有认证文件权限")
		return
	}
	account := store.account(username)
	if account == nil {
		sweAccountMu.Unlock()
		workbenchError(out, 404, "该账号未保存")
		return
	}
	candidate := *account
	sweAccountMu.Unlock()

	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()
	user, session, csrf, err := authenticateSWEAccount(ctx, candidate)
	if err != nil {
		workbenchError(out, 409, "该账号登录失败，未切换")
		return
	}
	sweAccountMu.Lock()
	store, err = readSWEAccountStore()
	if err != nil {
		sweAccountMu.Unlock()
		workbenchError(out, 500, "账号配置不可读")
		return
	}
	account = store.account(username)
	if account == nil {
		sweAccountMu.Unlock()
		workbenchError(out, 404, "该账号未保存")
		return
	}
	account.RealName = user.RealName
	account.Role = user.Role
	account.Session = session
	account.CSRF = csrf
	store.Active = username
	if err := writeSWEAccountStore(store); err != nil {
		sweAccountMu.Unlock()
		workbenchError(out, 500, "账号切换保存失败")
		return
	}
	sweAccountMu.Unlock()
	w.resetPlatformCaches()
	writeWorkbenchJSON(out, 200, w.sweAccountsPayload(store))
}

func (w *workbenchServer) removeSWEAccount(out http.ResponseWriter, r *http.Request) {
	if sweEnvOverride() {
		workbenchError(out, 409, "当前由环境变量提供平台凭据；先在启动环境移除 SWE_* 后再管理账号")
		return
	}
	var input struct {
		Username string `json:"username"`
	}
	if decodeWorkbenchJSON(r, &input) != nil {
		workbenchError(out, 400, "账号格式不正确")
		return
	}
	username := strings.TrimSpace(input.Username)
	sweAccountMu.Lock()
	store, err := readSWEAccountStore()
	if err != nil {
		sweAccountMu.Unlock()
		workbenchError(out, 500, "账号配置不可读")
		return
	}
	index := -1
	for i := range store.Accounts {
		if store.Accounts[i].Username == username {
			index = i
			break
		}
	}
	if index < 0 {
		sweAccountMu.Unlock()
		workbenchError(out, 404, "该账号未保存")
		return
	}
	wasActive := store.Active == username
	store.Accounts = append(store.Accounts[:index], store.Accounts[index+1:]...)
	if wasActive {
		store.Active = ""
		if len(store.Accounts) > 0 {
			store.Active = store.Accounts[0].Username
		}
	}
	if err := writeSWEAccountStore(store); err != nil {
		sweAccountMu.Unlock()
		workbenchError(out, 500, "账号移除保存失败")
		return
	}
	sweAccountMu.Unlock()
	w.resetPlatformCaches()
	writeWorkbenchJSON(out, 200, w.sweAccountsPayload(store))
}
