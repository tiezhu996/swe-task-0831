package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPrivateCredentialsPermissionsAndSchema(t *testing.T) {
	dir := t.TempDir()
	_ = os.Chmod(dir, 0700)
	path := filepath.Join(dir, "config.json")
	_ = os.WriteFile(path, []byte(`{"schema_version":1,"swe":{"username":"test","password":"private"}}`), 0600)
	c, err := readPrivateCredentials(path)
	if err != nil || c.Username != "test" {
		t.Fatal("valid private config not loaded")
	}
	_ = os.Chmod(path, 0644)
	if _, err := readPrivateCredentials(path); err == nil {
		t.Fatal("public credentials accepted")
	}
	_ = os.Chmod(path, 0600)
	link := filepath.Join(dir, "linked.json")
	_ = os.Symlink(path, link)
	if _, err := readPrivateCredentials(link); err == nil {
		t.Fatal("credential symlink accepted")
	}
	_ = os.WriteFile(path, []byte(`{"schema_version":2,"swe":{"username":"test","password":"private"}}`), 0600)
	if _, err := readPrivateCredentials(path); err == nil {
		t.Fatal("unknown schema accepted")
	}
}
