package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestArtifactCommandPublishesArbitraryKindsAndDisplayTitles(t *testing.T) {
	const issueID = "11111111-1111-1111-1111-111111111111"
	const path = "/api/issues/" + issueID + "/artifacts"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/issues/"+issueID {
			_ = json.NewEncoder(w).Encode(map[string]any{"id": issueID, "identifier": "DEV-43", "title": "test"})
			return
		}
		if r.URL.Path != path+"/report" || r.Method != "PUT" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body["title"] != "压测报告" || body["url"] != "https://docs.example/report" || body["kind"] != "new-kind-v9" {
			t.Errorf("artifact fields were reinterpreted: %+v", body)
		}
		body["key"] = "report"
		_ = json.NewEncoder(w).Encode(map[string]any{"artifacts": []any{body}})
	}))
	defer server.Close()
	t.Setenv("MULTICA_SERVER_URL", server.URL)
	t.Setenv("MULTICA_WORKSPACE_ID", "ws-1")
	t.Setenv("MULTICA_TOKEN", "test-token")
	cmd := newIssueArtifactCmd()
	cmd.SetArgs([]string{"set", issueID, "--key", "report", "--title", "压测报告", "--url", "https://docs.example/report", "--kind", "new-kind-v9", "--output", "json"})
	output, err := captureStdout(t, cmd.Execute)
	if err != nil || !strings.Contains(output, "压测报告") || !strings.Contains(output, "new-kind-v9") {
		t.Fatalf("output=%s; error=%v", output, err)
	}
}

func TestArtifactCommandListAndDelete(t *testing.T) {
	const issueID = "11111111-1111-1111-1111-111111111111"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/issues/" + issueID:
			_ = json.NewEncoder(w).Encode(map[string]any{"id": issueID, "identifier": "DEV-43"})
		case "/api/issues/" + issueID + "/artifacts":
			if r.Method != "GET" {
				t.Errorf("list method = %s", r.Method)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"artifacts": []any{}})
		case "/api/issues/" + issueID + "/artifacts/report":
			if r.Method != "DELETE" {
				t.Errorf("delete method = %s", r.Method)
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	t.Setenv("MULTICA_SERVER_URL", server.URL)
	t.Setenv("MULTICA_WORKSPACE_ID", "ws-1")
	t.Setenv("MULTICA_TOKEN", "test-token")
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"list", issueID, "--output", "json"}, "[]"},
		{[]string{"delete", issueID, "--key", "report", "--output", "json"}, `"deleted": true`},
	} {
		cmd := newIssueArtifactCmd()
		cmd.SetArgs(tc.args)
		output, err := captureStdout(t, cmd.Execute)
		if err != nil || !strings.Contains(output, tc.want) {
			t.Fatalf("%v: output=%s; error=%v", tc.args, output, err)
		}
	}
}
