package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNormalizeProxyTarget(t *testing.T) {
	scheme, host, port, err := normalizeProxyTarget("", "", 0)
	if err == nil || scheme != "" {
		t.Fatalf("empty target should fail: %q %q %d %v", scheme, host, port, err)
	}
	scheme, host, port, err = normalizeProxyTarget("SOCKS5", "[::1]", 7891)
	if err != nil || scheme != "socks5" || host != "::1" || port != 7891 {
		t.Fatalf("normalize: %q %q %d %v", scheme, host, port, err)
	}
	if _, _, _, err := normalizeProxyTarget("ftp", "127.0.0.1", 7890); err == nil {
		t.Fatal("ftp must be rejected")
	}
	if _, _, _, err := normalizeProxyTarget("http", "127.0.0.1", 70000); err == nil {
		t.Fatal("port must be bounded")
	}
}

func TestNormalizeProxyURL(t *testing.T) {
	raw, parsed, err := normalizeProxyURL("127.0.0.1:7890")
	if err != nil || raw != "http://127.0.0.1:7890" || parsed.Hostname() != "127.0.0.1" {
		t.Fatalf("short form: %q %#v %v", raw, parsed, err)
	}
	raw, _, err = normalizeProxyURL("socks5://127.0.0.1:7891")
	if err != nil || raw != "socks5://127.0.0.1:7891" {
		t.Fatalf("socks5: %q %v", raw, err)
	}
	if _, _, err := normalizeProxyURL("http://127.0.0.1:7890/path"); err == nil {
		t.Fatal("path must be rejected")
	}
	if _, _, err := normalizeProxyURL("ftp://127.0.0.1:7890"); err == nil {
		t.Fatal("scheme must be rejected")
	}
}

func TestGitProxyEnvReplacesInheritedProxy(t *testing.T) {
	base := []string{"PATH=/bin", "HTTP_PROXY=http://old:1", "NO_PROXY=github.com", "no_proxy=github.com"}
	env := gitProxyEnv(base, "http://127.0.0.1:7890")
	joined := strings.Join(env, "\n")
	if strings.Contains(joined, "old:1") || strings.Contains(joined, "NO_PROXY") || strings.Contains(joined, "no_proxy") {
		t.Fatalf("inherited proxy survived: %v", env)
	}
	for _, key := range []string{"HTTP_PROXY=http://127.0.0.1:7890", "HTTPS_PROXY=http://127.0.0.1:7890", "ALL_PROXY=http://127.0.0.1:7890", "http_proxy=http://127.0.0.1:7890"} {
		if !strings.Contains(joined, key) {
			t.Fatalf("missing %s in %v", key, env)
		}
	}
	if strings.Contains(strings.Join(gitProxyEnv(base, ""), "\n"), "7890") {
		t.Fatal("disabled proxy must not be injected")
	}
}

func TestProxySettingsPersist(t *testing.T) {
	privateRoot := t.TempDir()
	w := &workbenchServer{privateRoot: privateRoot, root: privateRoot, proxyScheme: "http", proxyHost: "127.0.0.1", proxyPort: 7890}
	w.proxyEnabled = true
	w.proxyScheme = "socks5"
	w.proxyHost = "127.0.0.1"
	w.proxyPort = 7891
	if err := w.saveSettingsLocked(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(privateRoot, "settings.json"))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("settings mode: %v %v", info, err)
	}
	reloaded := &workbenchServer{privateRoot: privateRoot, root: privateRoot, proxyScheme: "http", proxyHost: "127.0.0.1", proxyPort: 7890}
	if err := reloaded.loadSettings(); err != nil {
		t.Fatal(err)
	}
	if !reloaded.proxyEnabled || reloaded.proxyScheme != "socks5" || reloaded.proxyHost != "127.0.0.1" || reloaded.proxyPort != 7891 {
		t.Fatalf("proxy settings did not round-trip: %+v", reloaded)
	}
	if got := reloaded.proxyURLFromFieldsLocked(); got != "socks5://127.0.0.1:7891" {
		t.Fatalf("proxy url: %s", got)
	}
}

func TestCloneInjectsProxyEnv(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "env.txt")
	script := "#!/bin/sh\nprintf '%s\\n' \"$HTTPS_PROXY\" > \"$OUT\"\nprintf '%s\\n' \"$NO_PROXY\" >> \"$OUT\"\nexit 0\n"
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("OUT", out)
	t.Setenv("HTTPS_PROXY", "http://old:1")
	t.Setenv("NO_PROXY", "github.com")
	empty := filepath.Join(dir, "target")
	if err := clonePublicRepository(context.Background(), "https://github.com/a/b", empty, "http://127.0.0.1:7890"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(data), "\n")
	if len(lines) < 2 || lines[0] != "http://127.0.0.1:7890" || lines[1] != "" {
		t.Fatalf("proxy env not applied: %q", lines)
	}
	if err := clonePublicRepository(context.Background(), "https://github.com/a/b", filepath.Join(dir, "target2"), ""); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(out)
	lines = strings.Split(string(data), "\n")
	if len(lines) < 2 || lines[0] != "" || lines[1] != "" {
		t.Fatalf("disabled proxy must not be injected: %q", lines)
	}
}
