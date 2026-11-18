package handler

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/issueartifact"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// GetIssueArtifactAudience is consumed by the publishing CLI, which holds the
// document creator's credentials. The server never borrows the steward bot's
// identity to modify a document owned by a different user.
func (h *Handler) GetIssueArtifactAudience(w http.ResponseWriter, r *http.Request) {
	issue, ok := h.loadIssueForUser(w, r, chi.URLParam(r, "id"))
	if !ok {
		return
	}
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	actorType, _ := h.resolveActor(r, userID, uuidToString(issue.WorkspaceID))
	run := h.agentRunOnIssue(r, actorType, issue)
	audience, err := h.issueArtifactAudience(r.Context(), issue, run)
	if err != nil {
		writeError(w, http.StatusConflict, "document access is not ready: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, audience)
}

func (h *Handler) issueArtifactAudience(ctx context.Context, issue db.Issue, run *db.AgentTaskQueue) (issueartifact.Audience, error) {
	var chain []db.Issue
	seen := map[pgtype.UUID]bool{}
	for {
		if seen[issue.ID] {
			return issueartifact.Audience{}, errors.New("cyclic issue ancestry")
		}
		seen[issue.ID] = true
		chain = append(chain, issue)
		binding, err := h.Queries.GetChannelIssueChatByIssue(ctx, db.GetChannelIssueChatByIssueParams{IssueID: issue.ID, ChannelType: "feishu"})
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return issueartifact.Audience{}, err
		}
		if err == nil && binding.WorkspaceID == issue.WorkspaceID && binding.Status == "active" {
			a := issueartifact.Audience{Readers: []issueartifact.Reader{{MemberType: "openchat", MemberID: binding.ChatID}}}
			return a, a.Validate()
		}
		if !issue.ParentIssueID.Valid {
			break
		}
		issue, err = h.Queries.GetIssueInWorkspace(ctx, db.GetIssueInWorkspaceParams{ID: issue.ParentIssueID, WorkspaceID: issue.WorkspaceID})
		if err != nil {
			return issueartifact.Audience{}, errors.New("cannot resolve parent issue")
		}
	}
	users := map[pgtype.UUID]bool{}
	var squadID pgtype.UUID
	if run != nil {
		squadID = run.SquadID
		if run.OriginatorUserID.Valid {
			users[run.OriginatorUserID] = true
		}
	}
	for _, item := range chain {
		if item.CreatorType == "member" {
			users[item.CreatorID] = true
		}
		if item.AssigneeType.String == "member" {
			users[item.AssigneeID] = true
		}
		if !squadID.Valid && item.AssigneeType.String == "squad" {
			squadID = item.AssigneeID
		}
	}
	if squadID.Valid {
		if _, err := h.Queries.GetSquadInWorkspace(ctx, db.GetSquadInWorkspaceParams{ID: squadID, WorkspaceID: issue.WorkspaceID}); err != nil {
			return issueartifact.Audience{}, errors.New("cannot resolve task squad")
		}
		members, err := h.Queries.ListSquadMembers(ctx, squadID)
		if err != nil {
			return issueartifact.Audience{}, err
		}
		for _, member := range members {
			if member.MemberType == "member" {
				users[member.MemberID] = true
			}
		}
	}
	emails := map[string]bool{}
	for userID := range users {
		if _, err := h.Queries.GetMemberByUserAndWorkspace(ctx, db.GetMemberByUserAndWorkspaceParams{UserID: userID, WorkspaceID: issue.WorkspaceID}); err != nil {
			return issueartifact.Audience{}, fmt.Errorf("reader %s is not a current workspace member", uuidToString(userID))
		}
		user, err := h.Queries.GetUser(ctx, userID)
		if err != nil {
			return issueartifact.Audience{}, errors.New("cannot resolve document reader")
		}
		emails[strings.ToLower(strings.TrimSpace(user.Email))] = true
	}
	audience := issueartifact.Audience{Readers: []issueartifact.Reader{}}
	for email := range emails {
		audience.Readers = append(audience.Readers, issueartifact.Reader{MemberType: "email", MemberID: email})
	}
	sort.Slice(audience.Readers, func(i, j int) bool { return audience.Readers[i].MemberID < audience.Readers[j].MemberID })
	return audience, audience.Validate()
}
