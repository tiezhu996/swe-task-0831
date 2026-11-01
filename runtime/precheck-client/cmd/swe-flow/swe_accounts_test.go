package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func useTempSWEConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.json")
	t.Setenv("SWE_TASK_CONFIG", path)
	t.Setenv("SWE_USERNAME", "")
	t.Setenv("SWE_PASSWORD", "")
	t.Setenv("SWE_SESSION", "")
	t.Setenv("SWE_CSRF", "")
	return path
}

func TestSWEAccountsMigrateAndPersist(t *testing.T) {
	path := useTempSWEConfig(t)
	legacy := `{"schema_version":1,"swe":{"username":"one","password":"p1"},"github":{"token":"keep"}}`
	if err := os.WriteFile(path, []byte(legacy), 0600); err != nil {
		t.Fatal(err)
	}
	store, err := readSWEAccountStore()
	if err != nil || len(store.Accounts) != 1 || store.Active != "one" {
		t.Fatalf("legacy migration: %+v %v", store, err)
	}
	creds, err := localCredentials()
	if err != nil || creds.Username != "one" || creds.Password != "p1" {
		t.Fatalf("local credentials: %+v %v", creds, err)
	}
	store.Accounts = append(store.Accounts, sweAccount{Username: "two", Password: "p2", RealName: "Two", Role: "qa"})
	store.Active = "two"
	if err := writeSWEAccountStore(store); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("mode: %v %v", info, err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var config map[string]any
	if json.Unmarshal(raw, &config) != nil || config["github"] == nil {
		t.Fatalf("github key was lost: %s", raw)
	}
	reloaded, err := readSWEAccountStore()
	if err != nil || reloaded.Active != "two" || len(reloaded.Accounts) != 2 {
		t.Fatalf("reload: %+v %v", reloaded, err)
	}
	active, err := localCredentials()
	if err != nil || active.Username != "two" || active.Password != "p2" {
		t.Fatalf("active credentials: %+v %v", active, err)
	}
	mirror, err := readPrivateCredentials(path)
	if err != nil || mirror.Username != "two" {
		t.Fatalf("legacy mirror: %+v %v", mirror, err)
	}
}

func TestSWEAccountSessionAndPayloadDoNotLeakSecrets(t *testing.T) {
	path := useTempSWEConfig(t)
	if err := os.WriteFile(path, []byte(`{"schema_version":1,"swe":{"username":"one","password":"p1"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	updateSWEAccountSession("one", "session-secret", "csrf-secret")
	store, err := readSWEAccountStore()
	if err != nil {
		t.Fatal(err)
	}
	account := store.account("one")
	if account == nil || account.Session != "session-secret" || account.CSRF != "csrf-secret" {
		t.Fatalf("session not cached: %+v", account)
	}
	payload := (&workbenchServer{}).sweAccountsPayload(store)
	encoded, _ := json.Marshal(payload)
	for _, secret := range []string{"p1", "session-secret", "csrf-secret"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("payload leaked %q: %s", secret, encoded)
		}
	}
	if !strings.Contains(string(encoded), `"active":true`) {
		t.Fatalf("active account missing: %s", encoded)
	}
}
