package handler

import (
	"net/http"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

func TestCommentAssistance_ExplicitRequestAndIndependentClose(t *testing.T) {
	fx := testutil.New(testPool, testWorkspaceID, testUserID)
	issue := fx.Issue(t, "Assistance lifecycle")
	content := "Please authenticate [@Owner](mention://member/" + testUserID + ")"
	create := func(body map[string]any, status int) CommentResponse {
		t.Helper()
		r := withURLParam(newRequest(http.MethodPost, "/api/issues/"+issue+"/comments", body), "id", issue)
		response := testutil.Call(t, testHandler.CreateComment, r).Want(status)
		var c CommentResponse
		if status == http.StatusCreated {
			response.JSON(&c)
		}
		return c
	}
	ordinary := create(map[string]any{"content": content}, http.StatusCreated)
	if ordinary.AssistanceStatus != "none" {
		t.Fatalf("a mention alone must not request assistance: %+v", ordinary)
	}
	for _, text := range []string{"Please help", "`" + content + "`", "[@Nobody](mention://member/00000000-0000-4000-8000-000000000099)"} {
		create(map[string]any{"content": text, "request_assistance": true}, http.StatusBadRequest)
	}
	request := create(map[string]any{"content": content, "request_assistance": true}, http.StatusCreated)
	if request.AssistanceStatus != "open" {
		t.Fatalf("explicit request = %+v", request)
	}
	change := func(id string, h http.HandlerFunc, status int) CommentResponse {
		t.Helper()
		r := withURLParam(newRequest(http.MethodPost, "/api/comments/"+id+"/assistance", nil), "commentId", id)
		response := testutil.Call(t, h, r).Want(status)
		var c CommentResponse
		if status == http.StatusOK {
			response.JSON(&c)
		}
		return c
	}
	change(ordinary.ID, testHandler.CloseCommentAssistance, http.StatusConflict)
	closed := change(request.ID, testHandler.CloseCommentAssistance, http.StatusOK)
	if closed.AssistanceStatus != "closed" || closed.ResolvedAt != nil {
		t.Fatalf("closing assistance must not resolve the discussion: %+v", closed)
	}
	if again := change(request.ID, testHandler.CloseCommentAssistance, http.StatusOK); again.Revision != closed.Revision {
		t.Fatal("closing twice must be idempotent")
	}
	change(request.ID, testHandler.ResolveComment, http.StatusOK)
	create(map[string]any{"content": "Thanks", "parent_id": request.ID}, http.StatusCreated)
	var state string
	if err := testPool.QueryRow(t.Context(), "SELECT assistance_status FROM comment WHERE id = $1", request.ID).Scan(&state); err != nil || state != "closed" {
		t.Fatalf("ordinary replies must not reopen assistance: %s (%v)", state, err)
	}
	if reopened := change(request.ID, testHandler.ReopenCommentAssistance, http.StatusOK); reopened.AssistanceStatus != "open" {
		t.Fatalf("explicit reopen = %+v", reopened)
	}
	// Every list projection, including thread and root modes, must carry status.
	for _, query := range []string{"", "?roots_only=true", "?thread=" + request.ID, "?threads=10"} {
		r := withURLParam(newRequest(http.MethodGet, "/api/issues/"+issue+"/comments"+query, nil), "id", issue)
		var comments []CommentResponse
		testutil.Call(t, testHandler.ListComments, r).Want(http.StatusOK).JSON(&comments)
		found := false
		for _, c := range comments {
			if c.ID == request.ID {
				found = c.AssistanceStatus == "open"
			}
		}
		if !found {
			t.Fatalf("list %s lost assistance status: %+v", query, comments)
		}
	}
}
