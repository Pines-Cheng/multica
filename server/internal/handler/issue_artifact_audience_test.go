package handler

import (
	"testing"

	"github.com/multica-ai/multica/server/internal/issueartifact"
	"github.com/multica-ai/multica/server/internal/testutil"
)

func TestArtifactAudienceSquadAndParentParticipants(t *testing.T) {
	leader := dbfx.Agent(t, "Document leader", testRuntimeID)
	squad := dbfx.Squad(t, "Document squad", leader)
	reader := dbfx.User(t, "Reader", "artifact-reader@example.com")
	dbfx.Member(t, testWorkspaceID, reader, "member")
	dbfx.SquadMember(t, squad, "member", reader)
	dbfx.SquadMember(t, squad, "member", testUserID)
	dbfx.SquadMember(t, squad, "agent", leader)
	parent := dbfx.Issue(t, "Parent", testutil.Cols{"assignee_type": "squad", "assignee_id": squad})
	child := dbfx.Issue(t, "Child", testutil.Cols{"parent_issue_id": parent, "creator_type": "agent", "creator_id": leader, "assignee_type": "member", "assignee_id": reader})
	var got issueartifact.Audience
	testutil.Call(t, testHandler.GetIssueArtifactAudience, artifactRequest("GET", child, "", nil)).Want(200).JSON(&got)
	if len(got.Readers) != 2 || got.Readers[0].MemberID != "artifact-reader@example.com" || got.Readers[1].MemberID != handlerTestEmail {
		t.Fatalf("wrong audience: %+v", got)
	}
	dbfx.Exec(t, `DELETE FROM member WHERE workspace_id = $1 AND user_id = $2`, testWorkspaceID, reader)
	testutil.Call(t, testHandler.GetIssueArtifactAudience, artifactRequest("GET", child, "", nil)).Want(409)
}

func TestArtifactAudiencePrefersActiveAncestorChat(t *testing.T) {
	parent := dbfx.Issue(t, "Parent group")
	child := dbfx.Issue(t, "Child without topic", testutil.Cols{"parent_issue_id": parent})
	// No installation lookup is needed to return the portable chat identity.
	dbfx.Insert(t, "channel_issue_chat", testutil.Cols{"workspace_id": testWorkspaceID, "issue_id": parent, "root_issue_id": parent, "channel_type": "feishu", "chat_id": "oc_parent", "steward_installation_id": parent, "created_by": testUserID})
	var got issueartifact.Audience
	testutil.Call(t, testHandler.GetIssueArtifactAudience, artifactRequest("GET", child, "", nil)).Want(200).JSON(&got)
	if len(got.Readers) != 1 || got.Readers[0] != (issueartifact.Reader{MemberType: "openchat", MemberID: "oc_parent"}) {
		t.Fatalf("wrong group: %+v", got)
	}
	dbfx.Exec(t, `UPDATE channel_issue_chat SET status = 'detached' WHERE issue_id = $1`, parent)
	testutil.Call(t, testHandler.GetIssueArtifactAudience, artifactRequest("GET", child, "", nil)).Want(200).JSON(&got)
	if len(got.Readers) != 1 || got.Readers[0].MemberType != "email" {
		t.Fatalf("used inactive group: %+v", got)
	}
}

func TestArtifactAudienceRejectsMissingReadersAndCrossWorkspace(t *testing.T) {
	leader := dbfx.Agent(t, "Only agent", testRuntimeID)
	issue := dbfx.Issue(t, "No humans", testutil.Cols{"creator_type": "agent", "creator_id": leader})
	testutil.Call(t, testHandler.GetIssueArtifactAudience, artifactRequest("GET", issue, "", nil)).Want(409)
	ws := dbfx.Workspace(t, "Foreign readers", "foreign-readers")
	foreign := dbfx.Issue(t, "Foreign document", testutil.Cols{"workspace_id": ws})
	testutil.Call(t, testHandler.GetIssueArtifactAudience, artifactRequest("GET", foreign, "", nil)).Want(404)
	dbfx.Exec(t, `UPDATE issue SET parent_issue_id = $2 WHERE id = $1`, issue, foreign)
	testutil.Call(t, testHandler.GetIssueArtifactAudience, artifactRequest("GET", issue, "", nil)).Want(409)
}

func TestArtifactAudienceUsesExecutingSquadAndHumanOriginator(t *testing.T) {
	leader := dbfx.Agent(t, "Mentioned squad leader", testRuntimeID)
	squad := dbfx.Squad(t, "Mentioned squad", leader)
	reader := dbfx.User(t, "Squad reader", "squad-reader@example.com")
	originator := dbfx.User(t, "Requester", "requester@example.com")
	for _, user := range []string{reader, originator} {
		dbfx.Member(t, testWorkspaceID, user, "member")
	}
	dbfx.SquadMember(t, squad, "member", reader)
	issue := dbfx.Issue(t, "Agent-created task", testutil.Cols{"creator_type": "agent", "creator_id": leader, "assignee_type": "agent", "assignee_id": leader})
	run := dbfx.Task(t, leader, testutil.Cols{"issue_id": issue, "squad_id": squad, "runtime_id": testRuntimeID, "originator_user_id": originator, "accountable_user_id": originator})
	req := artifactRequest("GET", issue, "", nil)
	req.Header.Set("X-Agent-ID", leader)
	req.Header.Set("X-Task-ID", run)
	var got issueartifact.Audience
	testutil.Call(t, testHandler.GetIssueArtifactAudience, req).Want(200).JSON(&got)
	if len(got.Readers) != 2 || got.Readers[0].MemberID != "requester@example.com" || got.Readers[1].MemberID != "squad-reader@example.com" {
		t.Fatalf("lost execution audience: %+v", got)
	}
	other := dbfx.Issue(t, "Unrelated task", testutil.Cols{"creator_type": "agent", "creator_id": leader})
	req = artifactRequest("GET", other, "", nil)
	req.Header.Set("X-Agent-ID", leader)
	req.Header.Set("X-Task-ID", run)
	testutil.Call(t, testHandler.GetIssueArtifactAudience, req).Want(409)
}
