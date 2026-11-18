package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/issueartifact"
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

func TestGeneratedArtifactPublishesOnlyAfterVerifiedSharing(t *testing.T) {
	for _, scenario := range []string{"success", "denied", "invalid audience", "reference"} {
		t.Run(scenario, func(t *testing.T) {
			const id = "11111111-1111-1111-1111-111111111111"
			granted, verified, published, larkCalls := false, false, false, 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/issues/" + id:
					_ = json.NewEncoder(w).Encode(map[string]string{"id": id, "identifier": "DEV-43"})
				case "/api/issues/" + id + "/artifact-audience":
					if scenario == "invalid audience" {
						_, _ = w.Write([]byte(`{"readers":null}`))
						return
					}
					_ = json.NewEncoder(w).Encode(issueartifact.Audience{Readers: []issueartifact.Reader{{MemberType: "openchat", MemberID: "oc_task"}}})
				case "/api/issues/" + id + "/artifacts/report":
					if scenario != "reference" && !verified {
						t.Error("published before verification")
					}
					published = true
					_, _ = w.Write([]byte(`{"artifacts":[]}`))
				default:
					t.Errorf("unexpected request: %s", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			t.Setenv("MULTICA_SERVER_URL", server.URL)
			t.Setenv("MULTICA_WORKSPACE_ID", "ws-1")
			t.Setenv("MULTICA_TOKEN", "test-token")
			run := func(_ context.Context, args ...string) ([]byte, error) {
				larkCalls++
				switch args[1] {
				case "+inspect":
					return []byte(`{"ok":true,"data":{"token":"doc","type":"docx"}}`), nil
				case "+member-add":
					if scenario == "denied" {
						return []byte(`{"ok":false,"error":{"message":"permission denied"}}`), nil
					}
					granted = true
					return []byte(`{"ok":true,"data":{}}`), nil
				case "GET":
					if granted {
						verified = true
						return []byte(`{"ok":true,"data":{"items":[{"member_type":"openchat","member_id":"oc_task","perm":"view"}]}}`), nil
					}
					return []byte(`{"ok":true,"data":{"items":[]}}`), nil
				default:
					t.Fatalf("unexpected lark invocation: %v", args)
					return nil, nil
				}
			}
			cmd := newIssueArtifactCmdWithLark(run)
			args := []string{"set", id, "--key", "report", "--title", "Report", "--url", "https://example.feishu.cn/docx/doc", "--output", "json"}
			if scenario != "reference" {
				args = append(args, "--generated")
			} else {
				args = append(args, "--reference")
			}
			cmd.SetArgs(args)
			_, err := captureStdout(t, cmd.Execute)
			wantSuccess := scenario == "success" || scenario == "reference"
			if (err == nil) != wantSuccess || published != wantSuccess {
				t.Fatalf("published=%v err=%v", published, err)
			}
			if (scenario == "reference" || scenario == "invalid audience") && larkCalls != 0 {
				t.Fatalf("unexpected Lark calls: %d", larkCalls)
			}
		})
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

func TestArtifactDocumentRequiresExplicitProvenance(t *testing.T) {
	for _, flags := range [][]string{nil, {"--generated", "--reference"}} {
		cmd := newIssueArtifactCmdWithLark(func(context.Context, ...string) ([]byte, error) {
			t.Fatal("must reject before calling Lark")
			return nil, nil
		})
		args := []string{"set", "DEV-43", "--key", "report", "--title", "Report", "--url", "https://example.feishu.cn/docx/doc"}
		cmd.SetArgs(append(args, flags...))
		if err := cmd.Execute(); err == nil {
			t.Fatal("document provenance was not required")
		}
	}
}
