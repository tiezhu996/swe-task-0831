package precheckclient

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	DefaultBaseURL = "https://swe.jzxhnh.com/api/v1"
	sessionCookie  = "swe_qa_session"
	csrfCookie     = "swe_qa_csrf"
)

type Client struct {
	baseURL *url.URL
	http    *http.Client
	jar     http.CookieJar
	retry   RetryPolicy
	ua      string
}

// RetryPolicy only applies to idempotent read requests. Mutating requests are
// deliberately never retried because a timeout cannot tell us whether the
// server already accepted the write.
type RetryPolicy struct {
	MaxAttempts  int
	InitialDelay time.Duration
	MaxDelay     time.Duration
	// RequestTimeout bounds one HTTP attempt. The parent context still bounds
	// the whole operation, including retries and polling.
	RequestTimeout time.Duration
}

var defaultRetryPolicy = RetryPolicy{
	MaxAttempts:    3,
	InitialDelay:   250 * time.Millisecond,
	MaxDelay:       3 * time.Second,
	RequestTimeout: 30 * time.Second,
}

const defaultUserAgent = "swe-precheck-go-client/1"

type User struct {
	Username     string `json:"username"`
	RealName     string `json:"real_name"`
	Role         string `json:"role"`
	IsFirstLogin bool   `json:"is_first_login"`
	Impersonated bool   `json:"impersonated"`
}

type Row struct {
	RepoURL string `json:"repo_url"`
	Prompt  string `json:"prompt"`
}

type Quota struct {
	Limit         int `json:"limit"`
	Inflight      int `json:"inflight"`
	Allowance     int `json:"allowance"`
	MaxBatchItems int `json:"max_batch_items"`
}

type Batch struct {
	BatchID     string `json:"batch_id"`
	Status      string `json:"status"`
	StatusLabel string `json:"status_label"`
	Total       int    `json:"total"`
	Done        int    `json:"done"`
	Passed      int    `json:"passed"`
	Rejected    int    `json:"rejected"`
	Failed      int    `json:"failed"`
	Canceled    int    `json:"canceled"`
	Pending     int    `json:"pending"`
	Running     int    `json:"running"`
	Error       string `json:"error"`
	Finished    bool   `json:"finished"`
	Resumable   bool   `json:"resumable"`
	CreatedAt   string `json:"created_at"`
	FinishedAt  string `json:"finished_at"`
}

type BatchItem struct {
	ID              int    `json:"id"`
	BatchID         string `json:"batch_id"`
	RowNo           int    `json:"row_no"`
	RepoURL         string `json:"repo_url"`
	PromptExcerpt   string `json:"prompt_excerpt"`
	Status          string `json:"status"`
	StatusLabel     string `json:"status_label"`
	Difficulty      string `json:"difficulty"`
	DifficultyTotal int    `json:"difficulty_total"`
	RejectKind      string `json:"reject_kind"`
	RejectLabel     string `json:"reject_label"`
	RejectDetail    string `json:"reject_detail"`
	Rerunnable      bool   `json:"rerunnable"`
	Appealable      bool   `json:"appealable"`
	Deletable       bool   `json:"deletable"`
	Cancelable      bool   `json:"cancelable"`
	CreatedAt       string `json:"created_at"`
	UpdatedAt       string `json:"updated_at"`
}

type BatchProgress struct {
	Batch Batch       `json:"batch"`
	Items []BatchItem `json:"items"`
}

// HTTPError preserves the status code and response detail so callers can
// distinguish authentication, rate limiting, and server failures without
// parsing an error string.
type HTTPError struct {
	Method     string
	Path       string
	StatusCode int
	Detail     string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("swe API %s %s: HTTP %d: %s", e.Method, e.Path, e.StatusCode, e.Detail)
}

// WaitOptions controls batch polling. A zero value uses conservative defaults.
// Set MaxInterval equal to InitialInterval and BackoffFactor to 1 for the
// fixed-interval behavior of WaitBatch.
type WaitOptions struct {
	InitialInterval time.Duration
	MaxInterval     time.Duration
	BackoffFactor   float64
	OnProgress      func(BatchProgress)
}

type ItemDetail struct {
	BatchItem
	RequirementPrompt string          `json:"requirement_prompt"`
	LocalDup          json.RawMessage `json:"local_dup"`
	IssueDup          json.RawMessage `json:"issue_dup"`
	DifficultyScores  json.RawMessage `json:"difficulty_scores"`
	Appeals           json.RawMessage `json:"appeals"`
}

type ItemQuery struct {
	Page     int
	PageSize int
	Status   string
	Keyword  string
	DateFrom string
	DateTo   string
}

type ItemList struct {
	Items    []BatchItem `json:"items"`
	Page     int         `json:"page"`
	PageSize int         `json:"page_size"`
	Total    int         `json:"total"`
}

type BatchList struct {
	Items    []Batch `json:"items"`
	Page     int     `json:"page"`
	PageSize int     `json:"page_size"`
	Total    int     `json:"total"`
}

type apiEnvelope[T any] struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    T      `json:"data"`
}

type apiError struct {
	Detail string `json:"detail"`
}

func New(baseURL string, client *http.Client) (*Client, error) {
	return NewWithRetry(baseURL, client, defaultRetryPolicy)
}

// NewWithRetry constructs a client with bounded retries for GET/HEAD/OPTIONS
// requests. Set MaxAttempts to 1 to disable retries for a controlled test.
func NewWithRetry(baseURL string, client *http.Client, retry RetryPolicy) (*Client, error) {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	u, err := url.Parse(strings.TrimRight(baseURL, "/"))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("invalid base URL: %q", baseURL)
	}
	if client == nil {
		jar, jarErr := cookiejar.New(nil)
		if jarErr != nil {
			return nil, jarErr
		}
		client = &http.Client{Jar: jar}
	}
	if client.Jar == nil {
		jar, jarErr := cookiejar.New(nil)
		if jarErr != nil {
			return nil, jarErr
		}
		client.Jar = jar
	}
	if retry.MaxAttempts < 1 {
		retry.MaxAttempts = 1
	}
	if retry.InitialDelay <= 0 {
		retry.InitialDelay = 100 * time.Millisecond
	}
	if retry.MaxDelay < retry.InitialDelay {
		retry.MaxDelay = retry.InitialDelay
	}
	if retry.RequestTimeout <= 0 {
		retry.RequestTimeout = defaultRetryPolicy.RequestTimeout
	}
	return &Client{baseURL: u, http: client, jar: client.Jar, retry: retry, ua: defaultUserAgent}, nil
}

// SetSessionCookies is for a session obtained outside this process. Do not log
// or commit these values. Login is preferable because the jar receives cookies
// directly from Set-Cookie.
func (c *Client) SetSessionCookies(session, csrf string) {
	c.jar.SetCookies(c.baseURL, []*http.Cookie{
		{Name: sessionCookie, Value: session, Path: "/"},
		{Name: csrfCookie, Value: csrf, Path: "/"},
	})
}

// SessionCookies returns the current platform session pair so the local
// credential store can reuse it. Callers must treat both values as secrets and
// never include them in a UI response or log line.
func (c *Client) SessionCookies() (session, csrf string, ok bool) {
	if c == nil || c.jar == nil || c.baseURL == nil {
		return "", "", false
	}
	for _, cookie := range c.jar.Cookies(c.baseURL) {
		switch cookie.Name {
		case sessionCookie:
			session = cookie.Value
		case csrfCookie:
			csrf = cookie.Value
		}
	}
	return session, csrf, session != "" && csrf != ""
}

func (c *Client) Login(ctx context.Context, username, password string) (User, error) {
	return request[User](c, ctx, http.MethodPost, "/auth/login", map[string]string{
		"username": username,
		"password": password,
	})
}

func (c *Client) Me(ctx context.Context) (User, error) {
	return request[User](c, ctx, http.MethodGet, "/auth/me", nil)
}

func (c *Client) Quota(ctx context.Context) (Quota, error) {
	return request[Quota](c, ctx, http.MethodGet, "/precheck/quota", nil)
}

type SubmitResult struct {
	BatchID      string `json:"batch_id"`
	Accepted     int    `json:"accepted"`
	RejectedRows []int  `json:"rejected_rows"`
	QuotaNotice  string `json:"quota_notice"`
}

// UploadResult is returned by the platform attachment endpoint. URL is the
// server-side reference that must be put in a later submission payload when
// the form schema asks for an uploaded artifact.
type UploadResult struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

// SubmissionResult is the small stable part of the formal submission result.
// The platform may add fields later; callers that need them should use the
// detail/list endpoints rather than sending internal spreadsheet columns.
type SubmissionResult struct {
	ID     int    `json:"id"`
	Status string `json:"status"`
}

type FormField struct {
	Key          string   `json:"field_key"`
	Label        string   `json:"label"`
	Type         string   `json:"type"`
	Required     bool     `json:"required"`
	Auto         bool     `json:"auto"`
	MaxLength    int      `json:"max_length"`
	Options      []string `json:"options"`
	RequiredWhen struct {
		Field  string   `json:"field"`
		Values []string `json:"values"`
	} `json:"required_when"`
}

type FormSchema struct {
	Fields     []FormField `json:"fields"`
	Version    string      `json:"schema_version"`
	Frozen     bool        `json:"submit_frozen"`
	Limited    bool        `json:"submission_limited"`
	Attachment struct {
		Configured bool   `json:"configured"`
		MaxMB      int    `json:"max_mb"`
		Accept     string `json:"accept"`
	} `json:"attachment"`
}

func (c *Client) SubmissionFormSchema(ctx context.Context) (FormSchema, error) {
	return request[FormSchema](c, ctx, http.MethodGet, "/submissions/form-schema", nil)
}

func (c *Client) SubmissionDetail(ctx context.Context, id int) (SubmissionSummary, error) {
	if id <= 0 {
		return SubmissionSummary{}, fmt.Errorf("submission ID must be positive")
	}
	return request[SubmissionSummary](c, ctx, http.MethodGet, "/submissions/"+strconv.Itoa(id), nil)
}

// DeleteSubmission is an explicit single-attempt write. The platform retains
// history for auditing. Call only after checking the record and user intent.
func (c *Client) DeleteSubmission(ctx context.Context, id int) (map[string]any, error) {
	if id <= 0 {
		return nil, fmt.Errorf("submission ID must be positive")
	}
	return request[map[string]any](c, ctx, http.MethodDelete, "/submissions/"+strconv.Itoa(id), nil)
}

var forbiddenSubmissionKeys = map[string]struct{}{
	"备注":         {},
	"remark":     {},
	"remarks":    {},
	"note":       {},
	"notes":      {},
	"patch_file": {},
}

// BatchState is local recovery metadata. It deliberately contains no session,
// credentials, platform remarks, or diagnostic paths.
type BatchState struct {
	BatchID    string    `json:"batch_id"`
	RowsSHA256 string    `json:"rows_sha256"`
	CreatedAt  time.Time `json:"created_at"`
}

func NewBatchState(result SubmitResult, rows []Row, now time.Time) (BatchState, error) {
	if strings.TrimSpace(result.BatchID) == "" {
		return BatchState{}, errors.New("batch ID must not be empty")
	}
	digest, err := RowsSHA256(rows)
	if err != nil {
		return BatchState{}, err
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	return BatchState{BatchID: result.BatchID, RowsSHA256: digest, CreatedAt: now.UTC()}, nil
}

// RowsSHA256 provides a stable fingerprint for checking that a resumed batch
// belongs to the same local rows before polling it.
func RowsSHA256(rows []Row) (string, error) {
	data, err := json.Marshal(rows)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return fmt.Sprintf("%x", sum[:]), nil
}

// SaveBatchState writes state with mode 0600 and atomically replaces the
// destination, so an interrupted process cannot leave a partial JSON file.
func SaveBatchState(path string, state BatchState) error {
	if strings.TrimSpace(path) == "" {
		return errors.New("state path must not be empty")
	}
	if strings.TrimSpace(state.BatchID) == "" {
		return errors.New("batch ID must not be empty")
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	tmp, err := os.CreateTemp(dir, ".swe-batch-state-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

func LoadBatchState(path string) (BatchState, error) {
	var state BatchState
	if strings.TrimSpace(path) == "" {
		return state, errors.New("state path must not be empty")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return state, err
	}
	if err := json.Unmarshal(data, &state); err != nil {
		return state, fmt.Errorf("invalid batch state: %w", err)
	}
	if strings.TrimSpace(state.BatchID) == "" {
		return BatchState{}, errors.New("invalid batch state: batch ID is empty")
	}
	return state, nil
}

func (c *Client) SubmitBatch(ctx context.Context, rows []Row) (SubmitResult, error) {
	if len(rows) == 0 {
		return SubmitResult{}, errors.New("rows must not be empty")
	}
	return request[SubmitResult](c, ctx, http.MethodPost, "/precheck/batches", map[string]any{"rows": rows})
}

func (c *Client) ListBatches(ctx context.Context, page, pageSize int) (BatchList, error) {
	q := url.Values{}
	setPage(q, page, pageSize)
	return request[BatchList](c, ctx, http.MethodGet, "/precheck/batches?"+q.Encode(), nil)
}

func (c *Client) BatchProgress(ctx context.Context, batchID string) (BatchProgress, error) {
	if batchID == "" {
		return BatchProgress{}, errors.New("batch ID must not be empty")
	}
	return request[BatchProgress](c, ctx, http.MethodGet, "/precheck/batches/"+url.PathEscape(batchID), nil)
}

// WaitBatch polls a submitted batch until the service reports a terminal
// state. It honors cancellation/deadlines from ctx.
func (c *Client) WaitBatch(ctx context.Context, batchID string, interval time.Duration) (BatchProgress, error) {
	if interval <= 0 {
		interval = 2 * time.Second
	}
	return c.WaitBatchWithOptions(ctx, batchID, WaitOptions{
		InitialInterval: interval,
		MaxInterval:     interval,
		BackoffFactor:   1,
	})
}

// WaitBatchWithOptions polls a batch with bounded backoff. The callback is
// invoked after each successful read, which lets a CLI show progress without
// scraping the web UI.
func (c *Client) WaitBatchWithOptions(ctx context.Context, batchID string, options WaitOptions) (BatchProgress, error) {
	if batchID == "" {
		return BatchProgress{}, errors.New("batch ID must not be empty")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	interval := options.InitialInterval
	if interval <= 0 {
		interval = 2 * time.Second
	}
	maxInterval := options.MaxInterval
	if maxInterval <= 0 {
		maxInterval = 15 * time.Second
	}
	if maxInterval < interval {
		maxInterval = interval
	}
	factor := options.BackoffFactor
	if factor < 1 {
		factor = 1.5
	}
	for {
		progress, err := c.BatchProgress(ctx, batchID)
		if err != nil {
			return BatchProgress{}, err
		}
		if options.OnProgress != nil {
			options.OnProgress(progress)
		}
		if progress.Batch.Finished || isTerminalBatchStatus(progress.Batch.Status) {
			return progress, nil
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return BatchProgress{}, ctx.Err()
		case <-timer.C:
		}
		next := time.Duration(float64(interval) * factor)
		if next < interval || next > maxInterval {
			next = maxInterval
		}
		interval = next
	}
}

func isTerminalBatchStatus(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "completed", "complete", "finished", "failed", "rejected", "canceled", "cancelled":
		return true
	default:
		return false
	}
}

func (c *Client) ListItems(ctx context.Context, q ItemQuery) (ItemList, error) {
	v := url.Values{}
	setPage(v, q.Page, q.PageSize)
	if q.Status != "" {
		v.Set("status", q.Status)
	}
	if q.Keyword != "" {
		v.Set("keyword", q.Keyword)
	}
	if q.DateFrom != "" {
		v.Set("date_from", q.DateFrom)
	}
	if q.DateTo != "" {
		v.Set("date_to", q.DateTo)
	}
	return request[ItemList](c, ctx, http.MethodGet, "/precheck/items?"+v.Encode(), nil)
}

// ListAllItems reads every page through the API. It is useful for local
// filtering and reporting without scraping the web UI.
func (c *Client) ListAllItems(ctx context.Context, q ItemQuery) ([]BatchItem, error) {
	page := q.Page
	if page <= 0 {
		page = 1
	}
	pageSize := q.PageSize
	if pageSize <= 0 {
		pageSize = 100
	}
	var all []BatchItem
	var previousPageKey string
	for {
		q.Page, q.PageSize = page, pageSize
		result, err := c.ListItems(ctx, q)
		if err != nil {
			return nil, err
		}
		pageKey := itemPageKey(result.Items)
		if pageKey != "" && pageKey == previousPageKey {
			return nil, fmt.Errorf("precheck item pagination did not advance at page %d", page)
		}
		previousPageKey = pageKey
		all = append(all, result.Items...)
		if len(result.Items) == 0 || len(all) >= result.Total || len(result.Items) < pageSize {
			return all, nil
		}
		page++
	}
}

func itemPageKey(items []BatchItem) string {
	if len(items) == 0 {
		return ""
	}
	return fmt.Sprintf("%d:%d:%d", len(items), items[0].ID, items[len(items)-1].ID)
}

func (c *Client) ItemDetail(ctx context.Context, itemID int) (ItemDetail, error) {
	return request[ItemDetail](c, ctx, http.MethodGet, "/precheck/items/"+strconv.Itoa(itemID), nil)
}

// UpdateItem changes the repository/prompt and queues a fresh full precheck.
func (c *Client) UpdateItem(ctx context.Context, itemID int, repoURL, prompt string) (BatchItem, error) {
	return request[BatchItem](c, ctx, http.MethodPut, "/precheck/items/"+strconv.Itoa(itemID), map[string]string{
		"repo_url": repoURL,
		"prompt":   prompt,
	})
}

// Rerun queues the existing content again without editing it.
func (c *Client) Rerun(ctx context.Context, itemID int) (BatchItem, error) {
	return request[BatchItem](c, ctx, http.MethodPost, "/precheck/items/"+strconv.Itoa(itemID)+"/rerun", map[string]any{})
}

func (c *Client) Appeal(ctx context.Context, itemID int, reason string) (BatchItem, error) {
	if len([]rune(strings.TrimSpace(reason))) < 10 {
		return BatchItem{}, errors.New("appeal reason must contain at least 10 characters")
	}
	return request[BatchItem](c, ctx, http.MethodPost, "/precheck/items/"+strconv.Itoa(itemID)+"/appeal", map[string]string{"reason": reason})
}

// UploadAttachment uploads one local artifact to the platform's attachment
// endpoint. It is deliberately a single write attempt: retrying a multipart
// upload after an ambiguous network failure could create duplicate artifacts.
func (c *Client) UploadAttachment(ctx context.Context, path string) (UploadResult, error) {
	file, err := os.Open(path)
	if err != nil {
		return UploadResult{}, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return UploadResult{}, err
	}
	if !info.Mode().IsRegular() {
		return UploadResult{}, fmt.Errorf("attachment is not a regular file")
	}

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("file", filepath.Base(path))
	if err != nil {
		return UploadResult{}, err
	}
	if _, err := io.Copy(part, file); err != nil {
		return UploadResult{}, err
	}
	if err := writer.Close(); err != nil {
		return UploadResult{}, err
	}
	return multipartRequest[UploadResult](c, ctx, http.MethodPost, "/uploads/attachment", body.Bytes(), writer.FormDataContentType())
}

// UploadVerifiedAttachment uploads the same bytes that matched the audit hash.
func (c *Client) UploadVerifiedAttachment(ctx context.Context, path, expectedSHA256 string) (UploadResult, error) {
	data, err := os.ReadFile(path)
	if err != nil || expectedSHA256 == "" || TextSHA256(string(data)) != expectedSHA256 {
		return UploadResult{}, fmt.Errorf("attachment changed after audit; upload blocked")
	}
	return c.uploadAttachmentBytes(ctx, filepath.Base(path), data)
}

func (c *Client) uploadAttachmentBytes(ctx context.Context, name string, data []byte) (UploadResult, error) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("file", name)
	if err != nil {
		return UploadResult{}, err
	}
	if _, err = part.Write(data); err != nil {
		return UploadResult{}, err
	}
	if err = writer.Close(); err != nil {
		return UploadResult{}, err
	}
	return multipartRequest[UploadResult](c, ctx, http.MethodPost, "/uploads/attachment", body.Bytes(), writer.FormDataContentType())
}

// CreateSubmission creates a formal platform submission. The payload is
// intentionally caller-owned: delivery-sheet-only columns such as 备注 must
// never be copied into this request implicitly.
func (c *Client) CreateSubmission(ctx context.Context, payload map[string]any, patchFilePath, schemaVersion string) (SubmissionResult, error) {
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return SubmissionResult{}, err
	}
	// Validate the exact serialized JSON, including typed maps, structs and
	// RawMessage values nested in the caller's payload.
	var normalized map[string]any
	if err := json.Unmarshal(payloadBytes, &normalized); err != nil {
		return SubmissionResult{}, err
	}
	if err := validateSubmissionPayload(normalized); err != nil {
		return SubmissionResult{}, err
	}

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("payload", string(payloadBytes)); err != nil {
		return SubmissionResult{}, err
	}
	if strings.TrimSpace(schemaVersion) != "" {
		if err := writer.WriteField("schema_version", schemaVersion); err != nil {
			return SubmissionResult{}, err
		}
	}
	if strings.TrimSpace(patchFilePath) != "" {
		file, err := os.Open(patchFilePath)
		if err != nil {
			return SubmissionResult{}, err
		}
		info, statErr := file.Stat()
		if statErr != nil {
			_ = file.Close()
			return SubmissionResult{}, statErr
		}
		if !info.Mode().IsRegular() {
			_ = file.Close()
			return SubmissionResult{}, fmt.Errorf("patch file is not a regular file")
		}
		part, err := writer.CreateFormFile("patch_file", filepath.Base(patchFilePath))
		if err == nil {
			_, err = io.Copy(part, file)
		}
		closeErr := file.Close()
		if err != nil {
			return SubmissionResult{}, err
		}
		if closeErr != nil {
			return SubmissionResult{}, closeErr
		}
	}
	if err := writer.Close(); err != nil {
		return SubmissionResult{}, err
	}
	return multipartRequest[SubmissionResult](c, ctx, http.MethodPost, "/submissions/create", body.Bytes(), writer.FormDataContentType())
}

func validateSubmissionPayload(payload map[string]any) error {
	if payload == nil {
		return errors.New("submission payload must not be nil")
	}
	return validateSubmissionValue(payload)
}

func validateSubmissionValue(value any) error {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			if _, forbidden := forbiddenSubmissionKeys[strings.ToLower(strings.TrimSpace(key))]; forbidden {
				return fmt.Errorf("submission payload contains forbidden internal field %q", key)
			}
			if err := validateSubmissionValue(child); err != nil {
				return err
			}
		}
	case []any:
		for _, child := range typed {
			if err := validateSubmissionValue(child); err != nil {
				return err
			}
		}
	}
	return nil
}

func multipartRequest[T any](c *Client, ctx context.Context, method, path string, body []byte, contentType string) (T, error) {
	return requestBytes[T](c, ctx, method, path, body, contentType)
}

func setPage(q url.Values, page, pageSize int) {
	if page > 0 {
		q.Set("page", strconv.Itoa(page))
	}
	if pageSize > 0 {
		q.Set("page_size", strconv.Itoa(pageSize))
	}
}

func request[T any](c *Client, ctx context.Context, method, path string, body any) (T, error) {
	var zero T
	var bodyBytes []byte
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return zero, err
		}
		bodyBytes = b
	}
	contentType := ""
	if body != nil {
		contentType = "application/json"
	}
	return requestBytes[T](c, ctx, method, path, bodyBytes, contentType)
}

func requestBytes[T any](c *Client, ctx context.Context, method, path string, bodyBytes []byte, contentType string) (T, error) {
	var zero T
	if ctx == nil {
		ctx = context.Background()
	}
	attempts := 1
	if isReadMethod(method) {
		attempts = c.retry.MaxAttempts
	}
	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		var r io.Reader
		if bodyBytes != nil {
			r = bytes.NewReader(bodyBytes)
		}
		attemptCtx := ctx
		cancel := func() {}
		if c.retry.RequestTimeout > 0 {
			attemptCtx, cancel = context.WithTimeout(ctx, c.retry.RequestTimeout)
		} else {
			attemptCtx, cancel = context.WithCancel(ctx)
		}
		req, err := http.NewRequestWithContext(attemptCtx, method, c.baseURL.String()+path, r)
		if err != nil {
			cancel()
			return zero, err
		}
		req.Header.Set("Accept", "application/json")
		if c.ua != "" {
			req.Header.Set("User-Agent", c.ua)
		}
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		if !isReadMethod(method) {
			for _, cookie := range c.jar.Cookies(c.baseURL) {
				if cookie.Name == csrfCookie {
					req.Header.Set("X-CSRF-Token", cookie.Value)
					break
				}
			}
		}
		resp, err := c.http.Do(req)
		if err != nil {
			cancel()
			lastErr = err
			if attempt < attempts {
				if err := waitRetry(ctx, c.retry, attempt, 0); err != nil {
					return zero, err
				}
				continue
			}
			return zero, err
		}
		data, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		cancel()
		if readErr != nil {
			lastErr = readErr
			if attempt < attempts {
				if err := waitRetry(ctx, c.retry, attempt, 0); err != nil {
					return zero, err
				}
				continue
			}
			return zero, readErr
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			var detail apiError
			_ = json.Unmarshal(data, &detail)
			if detail.Detail == "" {
				detail.Detail = strings.TrimSpace(string(data))
			}
			lastErr = &HTTPError{Method: method, Path: path, StatusCode: resp.StatusCode, Detail: detail.Detail}
			if attempt < attempts && retryableStatus(resp.StatusCode) {
				if err := waitRetry(ctx, c.retry, attempt, retryAfterDelay(resp.Header.Get("Retry-After"), time.Now())); err != nil {
					return zero, err
				}
				continue
			}
			return zero, lastErr
		}
		var envelope apiEnvelope[T]
		if err := json.Unmarshal(data, &envelope); err != nil {
			return zero, err
		}
		if envelope.Code != 0 {
			return zero, fmt.Errorf("swe API %s %s: code=%d message=%s", method, path, envelope.Code, envelope.Message)
		}
		return envelope.Data, nil
	}
	return zero, lastErr
}

func isReadMethod(method string) bool {
	return method == http.MethodGet || method == http.MethodHead || method == http.MethodOptions
}

func retryableStatus(status int) bool {
	return status == http.StatusTooManyRequests || status >= 500
}

func retryAfterDelay(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(value); err == nil && seconds >= 0 {
		return time.Duration(seconds) * time.Second
	}
	if when, err := http.ParseTime(value); err == nil && when.After(now) {
		return when.Sub(now)
	}
	return 0
}

func waitRetry(ctx context.Context, policy RetryPolicy, attempt int, retryAfter time.Duration) error {
	delay := policy.InitialDelay
	for i := 1; i < attempt; i++ {
		delay *= 2
		if delay >= policy.MaxDelay {
			delay = policy.MaxDelay
			break
		}
	}
	if retryAfter > 0 {
		delay = retryAfter
		if delay > policy.MaxDelay {
			delay = policy.MaxDelay
		}
	}
	timer := time.NewTimer(delay)
	select {
	case <-ctx.Done():
		if !timer.Stop() {
			<-timer.C
		}
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
