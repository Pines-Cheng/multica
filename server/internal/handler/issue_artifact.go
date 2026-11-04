package handler

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/multica-ai/multica/server/internal/issueartifact"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

func (h *Handler) ListIssueArtifacts(w http.ResponseWriter, r *http.Request) {
	issue, ok := h.loadIssueForUser(w, r, chi.URLParam(r, "id"))
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"artifacts": issueartifact.Records(issue.Artifacts), "issue_revision": issue.Revision})
}

func (h *Handler) SetIssueArtifact(w http.ResponseWriter, r *http.Request) {
	var artifact issueartifact.Artifact
	if err := json.NewDecoder(r.Body).Decode(&artifact); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if err := artifact.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	value, err := json.Marshal(artifact)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid artifact")
		return
	}
	h.mutateIssueArtifact(w, r, value)
}

func (h *Handler) DeleteIssueArtifact(w http.ResponseWriter, r *http.Request) {
	h.mutateIssueArtifact(w, r, nil)
}

type artifactMutation struct {
	raw      []byte
	revision int64
}

func (h *Handler) mutateIssueArtifact(w http.ResponseWriter, r *http.Request, value []byte) {
	r = h.withWakeupActor(r)
	key := chi.URLParam(r, "key")
	if err := issueartifact.ValidateKey(key); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	issue, ok := h.loadIssueForUser(w, r, chi.URLParam(r, "id"))
	if !ok {
		return
	}
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	updated, err := wakeupWrite(h, r, func(q *db.Queries) (artifactMutation, error) {
		if value == nil {
			row, err := q.DeleteIssueArtifact(r.Context(), db.DeleteIssueArtifactParams{ID: issue.ID, WorkspaceID: issue.WorkspaceID, Key: key})
			return artifactMutation{row.Artifacts, row.Revision}, err
		}
		row, err := q.SetIssueArtifact(r.Context(), db.SetIssueArtifactParams{ID: issue.ID, WorkspaceID: issue.WorkspaceID, Key: key, Value: value})
		return artifactMutation{row.Artifacts, row.Revision}, err
	})
	changed := err == nil
	if errors.Is(err, pgx.ErrNoRows) {
		row, readErr := h.Queries.GetIssueArtifactsInWorkspace(r.Context(), db.GetIssueArtifactsInWorkspaceParams{ID: issue.ID, WorkspaceID: issue.WorkspaceID})
		updated, err = artifactMutation{row.Artifacts, row.Revision}, readErr
	}
	if err != nil {
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			writeError(w, http.StatusNotFound, "issue not found")
		case isCheckViolation(err):
			writeError(w, http.StatusBadRequest, "artifacts exceed the 16KB size limit")
		default:
			slog.Warn("issue artifact mutation failed", "issue_id", uuidToString(issue.ID), "error", err)
			writeError(w, http.StatusInternalServerError, "failed to update artifacts")
		}
		return
	}
	response := map[string]any{"artifacts": issueartifact.Records(updated.raw), "issue_revision": updated.revision}
	if changed {
		workspaceID := uuidToString(issue.WorkspaceID)
		actorType, actorID := h.resolveActor(r, userID, workspaceID)
		h.publish(protocol.EventIssueArtifactsChanged, workspaceID, actorType, actorID, map[string]any{
			"issue_id": uuidToString(issue.ID), "artifacts": response["artifacts"], "issue_revision": updated.revision,
		})
	}
	writeJSON(w, http.StatusOK, response)
}
