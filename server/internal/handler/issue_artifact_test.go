package handler

import (
	"net/http"
	"net/url"
	"sync/atomic"
	"testing"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/issueartifact"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

func artifactRequest(method, issue, key string, body any) *http.Request {
	return testutil.WithURLParams(newRequest(method, "/api/issues/"+issue+"/artifacts/"+url.PathEscape(key), body), "id", issue, "key", key)
}

func TestIssueArtifactsRoundTripAndNoop(t *testing.T) {
	issueID := dbfx.Issue(t, "Generic artifact lifecycle")
	var count atomic.Int64
	h := *testHandler
	h.Bus = events.New()
	h.Bus.Subscribe(protocol.EventIssueArtifactsChanged, func(events.Event) { count.Add(1) })
	type response struct {
		Artifacts []issueartifact.Record `json:"artifacts"`
		Revision  int64                  `json:"issue_revision"`
	}
	var empty response
	testutil.Call(t, h.ListIssueArtifacts, artifactRequest("GET", issueID, "", nil)).Want(200).JSON(&empty)
	if empty.Artifacts == nil || len(empty.Artifacts) != 0 {
		t.Fatalf("empty issue must return an array: %+v", empty)
	}
	body := map[string]any{"title": "压测报告", "url": "https://docs.example/report", "kind": "custom-type-v9"}
	var first response
	testutil.Call(t, h.SetIssueArtifact, artifactRequest("PUT", issueID, "report", body)).Want(200).JSON(&first)
	if len(first.Artifacts) != 1 || first.Artifacts[0].Title != "压测报告" || first.Artifacts[0].Kind != "custom-type-v9" || first.Revision <= empty.Revision {
		t.Fatalf("create lost artifact data: %+v", first)
	}
	var noop response
	testutil.Call(t, h.SetIssueArtifact, artifactRequest("PUT", issueID, "report", body)).Want(200).JSON(&noop)
	if noop.Revision != first.Revision || count.Load() != 1 {
		t.Fatalf("unchanged artifact must not refresh cards: %+v; events=%d", noop, count.Load())
	}
	body["title"], body["url"] = "最新压测报告", "https://docs.example/revised"
	var revised response
	testutil.Call(t, h.SetIssueArtifact, artifactRequest("PUT", issueID, "report", body)).Want(200).JSON(&revised)
	if len(revised.Artifacts) != 1 || revised.Artifacts[0].Key != "report" || revised.Artifacts[0].URL != body["url"] || revised.Artifacts[0].Title != body["title"] {
		t.Fatalf("update must replace the same artifact: %+v", revised)
	}
	var removed response
	testutil.Call(t, h.DeleteIssueArtifact, artifactRequest("DELETE", issueID, "report", nil)).Want(200).JSON(&removed)
	testutil.Call(t, h.DeleteIssueArtifact, artifactRequest("DELETE", issueID, "report", nil)).Want(200)
	if len(removed.Artifacts) != 0 || count.Load() != 3 {
		t.Fatalf("delete/no-op events=%d; response=%+v", count.Load(), removed)
	}
}

func TestIssueArtifactRejectsInvalidInputAndForeignWorkspace(t *testing.T) {
	issueID := dbfx.Issue(t, "Artifact input validation")
	for _, tc := range []struct {
		key  string
		body any
	}{
		{"bad key", map[string]any{"title": "report", "url": "https://docs.example"}},
		{"report", map[string]any{"url": "https://docs.example"}},
		{"report", map[string]any{"title": "report", "url": "javascript:alert(1)"}},
		{"report", map[string]any{"title": 32, "url": "https://docs.example"}},
		{"report", map[string]any{"title": "report", "url": "https://docs.example", "kind": 32}},
	} {
		testutil.Call(t, testHandler.SetIssueArtifact, artifactRequest("PUT", issueID, tc.key, tc.body)).Want(400)
	}
	foreignWorkspace := dbfx.Workspace(t, "Artifact isolation", "artifact-isolation")
	foreignIssue := dbfx.Issue(t, "Foreign artifact", testutil.Cols{"workspace_id": foreignWorkspace})
	for _, method := range []string{"GET", "PUT", "DELETE"} {
		handler := testHandler.ListIssueArtifacts
		if method == "PUT" {
			handler = testHandler.SetIssueArtifact
		}
		if method == "DELETE" {
			handler = testHandler.DeleteIssueArtifact
		}
		testutil.Call(t, handler, artifactRequest(method, foreignIssue, "report", map[string]any{"title": "report", "url": "https://docs.example"})).Want(404)
	}
}
