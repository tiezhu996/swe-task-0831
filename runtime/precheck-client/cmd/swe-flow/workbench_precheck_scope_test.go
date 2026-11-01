package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	client "example.com/swe-precheck-client"
)

func TestPrecheckFromAnotherAccountDoesNotMarkLocalTaskUnverified(t *testing.T) {
	root := t.TempDir()
	repo, prompt := "https://github.com/example/repo", "冻结的完整题面"
	path := setupWorkbenchTask(t, root, "old-account", repo, prompt)
	store := client.NewWorkflowStore(path)
	state, err := store.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.Update(context.Background(), state.Revision, client.WorkflowAudit{Action: "precheck_bind", Outcome: "success"}, func(s *client.WorkflowState) error {
		s.Precheck.ItemID = 31
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(out http.ResponseWriter, r *http.Request) {
		out.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/precheck/items":
			fmt.Fprint(out, `{"code":0,"data":{"items":[{"id":32,"repo_url":"https://github.com/example/repo","prompt_excerpt":"冻结的完整题面","status":"PRECHECK_PASSED"}],"page":1,"page_size":100,"total":1}}`)
		default:
			http.NotFound(out, r)
		}
	}))
	defer server.Close()

	w, err := newWorkbenchServer(root)
	if err != nil {
		t.Fatal(err)
	}
	w.newClient = func(context.Context) (*client.Client, error) { return client.New(server.URL, nil) }
	response := httptest.NewRecorder()
	w.handler().ServeHTTP(response, httptest.NewRequest("GET", "/api/prechecks?refresh=1", nil))
	if response.Code != 200 {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var data struct {
		Items []struct {
			ID              int      `json:"id"`
			MatchUnverified bool     `json:"match_unverified"`
			Matches         []string `json:"matches"`
		} `json:"items"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &data); err != nil {
		t.Fatal(err)
	}
	if len(data.Items) != 1 || data.Items[0].ID != 32 {
		t.Fatalf("items=%+v", data.Items)
	}
	if data.Items[0].MatchUnverified || len(data.Items[0].Matches) != 0 {
		t.Fatalf("precheck 32 must not use the task bound to precheck 31: %+v", data.Items[0])
	}
}
