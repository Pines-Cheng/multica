package lark

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

type duplicateTabCleanup struct {
	ChatAdmin
	err error
}

func (c duplicateTabCleanup) SyncChatURLTabs(_ context.Context, _ InstallationCredentials, _ ChatID, tabs []ChatURLTab) error {
	for i := range tabs {
		tabs[i].ID = ""
	}
	return c.err
}

func TestIssueChatTabs_PersistsDuplicateCleanup(t *testing.T) {
	for _, partialFailure := range []bool{false, true} {
		name := "success"
		if partialFailure {
			name = "later write fails"
		}
		t.Run(name, func(t *testing.T) {
			f := newActivityFixture(t)
			root := f.fx.Issue(t, "Root", testutil.Cols{
				"assignee_type": "squad", "assignee_id": f.squad,
				"metadata": `{"progress":"https://docs.example/progress"}`,
			})
			f.publishIssue(protocol.EventIssueCreated, root, nil)
			binding, ok := f.binding(t, root)
			if !ok {
				t.Fatal("missing chat binding")
			}
			ctx := context.Background()
			if err := f.store.SetChannelIssueArtifactTabs(ctx, db.SetChannelIssueArtifactTabsParams{
				ID: binding.ID, WorkspaceID: binding.WorkspaceID,
				ArtifactTabs: []byte(`{"metadata:progress":"duplicate","artifact:removed":"keep"}`),
			}); err != nil {
				t.Fatal(err)
			}
			var syncErr error
			if partialFailure {
				syncErr = errors.New("another tab failed after cleanup")
			}
			f.svc.cfg.Chats = duplicateTabCleanup{ChatAdmin: f.chats, err: syncErr}
			if err := f.svc.updateArtifactTabs(ctx, binding); !errors.Is(err, syncErr) {
				t.Fatalf("sync error = %v, want %v", err, syncErr)
			}
			binding, _ = f.binding(t, root)
			var owned map[string]string
			if err := json.Unmarshal(binding.ArtifactTabs, &owned); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(owned, map[string]string{"artifact:removed": "keep"}) {
				t.Fatalf("must forget the deleted duplicate but retain unrelated ownership: %+v", owned)
			}
		})
	}
}
