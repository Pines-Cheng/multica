package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/spf13/cobra"
)

func TestIssueCommentAssistanceCLI(t *testing.T) {
	const issueID = "11111111-1111-4111-8111-111111111111"
	var requested bool
	var actions []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/issues/"+issueID:
			_ = json.NewEncoder(w).Encode(map[string]any{"id": issueID, "identifier": "DEV-1"})
		case r.Method == http.MethodPost && r.URL.Path == "/api/issues/"+issueID+"/comments":
			var body struct {
				RequestAssistance bool `json:"request_assistance"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			requested = body.RequestAssistance
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "comment-1", "assistance_status": "open"})
		case r.Method == http.MethodPost:
			actions = append(actions, r.URL.Path)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "comment-1"})
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	setCLITestServerEnv(t, server.URL)
	t.Setenv("MULTICA_TOKEN", "mat_test-token")
	cmd := newIssueCommentAddTestCmd()
	_ = cmd.Flags().Set("content", "Please authenticate [@Ada](mention://member/22222222-2222-4222-8222-222222222222)")
	_ = cmd.Flags().Set("request-assistance", "true")
	_ = cmd.Flags().Set("output", "table")
	if err := runIssueCommentAdd(cmd, []string{issueID}); err != nil {
		t.Fatal(err)
	}
	if !requested {
		t.Fatal("CLI did not send an explicit assistance request")
	}
	for _, action := range []string{"close", "reopen"} {
		if err := runIssueCommentAssistance(&cobra.Command{}, "comment-1", action); err != nil {
			t.Fatal(err)
		}
	}
	if len(actions) != 2 || actions[0] != "/api/comments/comment-1/assistance/close" || actions[1] != "/api/comments/comment-1/assistance/reopen" {
		t.Fatalf("actions = %v", actions)
	}
}
