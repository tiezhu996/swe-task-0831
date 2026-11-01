package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Loon's local HTTP proxy defaults to this address. The user can change it in
// Settings; a disabled proxy is never injected into a pull.
const (
	defaultProxyScheme = "http"
	defaultProxyHost   = "127.0.0.1"
	defaultProxyPort   = 7890
)

func normalizeProxyTarget(scheme, host string, port int) (string, string, int, error) {
	scheme = strings.ToLower(strings.TrimSpace(scheme))
	if scheme == "" {
		scheme = defaultProxyScheme
	}
	switch scheme {
	case "http", "https", "socks5":
	default:
		return "", "", 0, fmt.Errorf("代理类型必须是 HTTP、HTTPS 或 SOCKS5")
	}
	host = strings.TrimSpace(host)
	if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	}
	if host == "" {
		return "", "", 0, fmt.Errorf("请填写代理主机")
	}
	if strings.ContainsAny(host, " \t\r\n/") {
		return "", "", 0, fmt.Errorf("代理主机格式不正确")
	}
	if port < 1 || port > 65535 {
		return "", "", 0, fmt.Errorf("代理端口必须在 1–65535 之间")
	}
	return scheme, host, port, nil
}

// normalizeProxyURL accepts the short "127.0.0.1:7890" form and returns a
// canonical URL for both the HTTP test client and git's proxy environment.
func normalizeProxyURL(raw string) (string, *url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil, fmt.Errorf("未配置代理")
	}
	if !strings.Contains(raw, "://") {
		raw = defaultProxyScheme + "://" + raw
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Opaque != "" {
		return "", nil, fmt.Errorf("代理地址格式不正确")
	}
	scheme := strings.ToLower(parsed.Scheme)
	switch scheme {
	case "http", "https", "socks5":
	default:
		return "", nil, fmt.Errorf("代理类型必须是 HTTP、HTTPS 或 SOCKS5")
	}
	host := parsed.Hostname()
	if host == "" {
		return "", nil, fmt.Errorf("代理地址缺少主机")
	}
	if strings.ContainsAny(host, " \t\r\n/") {
		return "", nil, fmt.Errorf("代理主机格式不正确")
	}
	port := parsed.Port()
	if port == "" {
		port = strconv.Itoa(defaultProxyPort)
	}
	number, err := strconv.Atoi(port)
	if err != nil || number < 1 || number > 65535 {
		return "", nil, fmt.Errorf("代理端口必须在 1–65535 之间")
	}
	if parsed.Path != "" && parsed.Path != "/" {
		return "", nil, fmt.Errorf("代理地址不能带路径")
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", nil, fmt.Errorf("代理地址不能带查询或片段")
	}
	parsed.Scheme = scheme
	parsed.Host = net.JoinHostPort(host, strconv.Itoa(number))
	parsed.Path, parsed.RawPath = "", ""
	return parsed.String(), parsed, nil
}

func (w *workbenchServer) proxyURLFromFieldsLocked() string {
	scheme, host, port := w.proxyScheme, w.proxyHost, w.proxyPort
	if scheme == "" {
		scheme = defaultProxyScheme
	}
	if host == "" {
		host = defaultProxyHost
	}
	if port == 0 {
		port = defaultProxyPort
	}
	return scheme + "://" + net.JoinHostPort(host, strconv.Itoa(port))
}

func (w *workbenchServer) proxySummary() (bool, string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.proxyEnabled, w.proxyURLFromFieldsLocked()
}

// gitProxyURL returns an empty string unless the operator enabled the proxy,
// so a configured-but-off Loon never changes how git behaves.
func (w *workbenchServer) gitProxyURL() string {
	enabled, raw := w.proxySummary()
	if !enabled {
		return ""
	}
	return raw
}

// gitProxyEnv replaces every inherited proxy variable, including NO_PROXY, so
// the setting is authoritative instead of silently losing to a shell export.
func gitProxyEnv(base []string, proxyURL string) []string {
	drop := map[string]bool{
		"HTTP_PROXY": true, "HTTPS_PROXY": true, "ALL_PROXY": true, "NO_PROXY": true,
		"http_proxy": true, "https_proxy": true, "all_proxy": true, "no_proxy": true,
	}
	env := make([]string, 0, len(base)+8)
	for _, entry := range base {
		key := entry
		if index := strings.IndexByte(entry, '='); index >= 0 {
			key = entry[:index]
		}
		if drop[strings.ToUpper(key)] {
			continue
		}
		env = append(env, entry)
	}
	if proxyURL == "" {
		return env
	}
	for _, key := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy"} {
		env = append(env, key+"="+proxyURL)
	}
	return env
}

func httpClientForProxy(proxyURL string, timeout time.Duration) (*http.Client, error) {
	transport := &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: timeout,
		ExpectContinueTimeout: time.Second,
		ForceAttemptHTTP2:     true,
	}
	if proxyURL != "" {
		_, parsed, err := normalizeProxyURL(proxyURL)
		if err != nil {
			return nil, err
		}
		transport.Proxy = http.ProxyURL(parsed)
	}
	return &http.Client{Transport: transport, Timeout: timeout}, nil
}

func (w *workbenchServer) gitFetch(ctx context.Context, source, remote, commit string) error {
	env := append(gitProxyEnv(os.Environ(), w.gitProxyURL()), "GIT_TERMINAL_PROMPT=0")
	return gitRun(ctx, env, "-C", source, "fetch", remote, commit)
}

type networkLatency struct {
	OK     bool   `json:"ok"`
	Status int    `json:"status,omitempty"`
	MS     int64  `json:"ms"`
	Error  string `json:"error,omitempty"`
}

type networkTestResult struct {
	Target   string         `json:"target"`
	TestedAt string         `json:"tested_at"`
	ProxyURL string         `json:"proxy_url,omitempty"`
	Direct   networkLatency `json:"direct"`
	Proxy    networkLatency `json:"proxy"`
}

func measureGitHub(ctx context.Context, client *http.Client, target string) networkLatency {
	start := time.Now()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return networkLatency{Error: err.Error()}
	}
	request.Header.Set("User-Agent", "SWE-Workbench/1.0")
	request.Header.Set("Range", "bytes=0-0")
	response, err := client.Do(request)
	elapsed := time.Since(start).Milliseconds()
	if err != nil {
		message := strings.TrimSpace(err.Error())
		if runes := []rune(message); len(runes) > 240 {
			message = string(runes[:240])
		}
		return networkLatency{MS: elapsed, Error: message}
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	return networkLatency{OK: true, Status: response.StatusCode, MS: elapsed}
}

func testGitHubNetwork(ctx context.Context, proxyURL string) networkTestResult {
	target := "https://github.com/"
	result := networkTestResult{Target: target, TestedAt: time.Now().UTC().Format(time.RFC3339), ProxyURL: proxyURL}
	var directClient, proxyClient *http.Client
	var directErr, proxyErr error
	directClient, directErr = httpClientForProxy("", 8*time.Second)
	proxyClient, proxyErr = httpClientForProxy(proxyURL, 8*time.Second)
	var wait sync.WaitGroup
	wait.Add(2)
	go func() {
		defer wait.Done()
		if directErr != nil {
			result.Direct = networkLatency{Error: directErr.Error()}
			return
		}
		result.Direct = measureGitHub(ctx, directClient, target)
	}()
	go func() {
		defer wait.Done()
		if proxyErr != nil {
			result.Proxy = networkLatency{Error: proxyErr.Error()}
			return
		}
		result.Proxy = measureGitHub(ctx, proxyClient, target)
	}()
	wait.Wait()
	return result
}

func (w *workbenchServer) getSettings(out http.ResponseWriter, _ *http.Request) {
	writeWorkbenchJSON(out, http.StatusOK, w.settingsView())
}

func (w *workbenchServer) settingsView() map[string]any {
	w.mu.Lock()
	defer w.mu.Unlock()
	return map[string]any{
		"work_root":     w.workRoot,
		"proxy_enabled": w.proxyEnabled,
		"proxy_scheme":  w.proxyScheme,
		"proxy_host":    w.proxyHost,
		"proxy_port":    w.proxyPort,
		"proxy_url":     w.proxyURLFromFieldsLocked(),
	}
}

func (w *workbenchServer) testNetwork(out http.ResponseWriter, r *http.Request) {
	var input struct {
		ProxyURL string `json:"proxy_url"`
	}
	if decodeWorkbenchJSON(r, &input) != nil {
		workbenchError(out, 400, "网络测试参数格式不正确")
		return
	}
	if strings.TrimSpace(input.ProxyURL) == "" {
		_, input.ProxyURL = w.proxySummary()
	}
	normalized, parsed, err := normalizeProxyURL(input.ProxyURL)
	if err != nil {
		workbenchError(out, 400, err.Error())
		return
	}
	display := parsed.Redacted()
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	result := testGitHubNetwork(ctx, normalized)
	result.ProxyURL = display
	writeWorkbenchJSON(out, http.StatusOK, result)
}
