package lark

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/multica-ai/multica/server/internal/integrations/channel"
	"github.com/multica-ai/multica/server/internal/integrations/channel/engine"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func TestFeishuLoginIdentityJoinsBotAfterWorkspaceMembership(t *testing.T) {
	pool := channelScopeTestDB(t)
	fx := testutil.New(pool, "", "")
	userID := fx.User(t, "Feishu-only member", "")
	workspaceID := fx.Workspace(t, "Feishu identity", "feishu-login-"+userID)
	fx = testutil.New(pool, workspaceID, userID)
	agentID := fx.Agent(t, "Feishu bot", "")
	appID := "cli_login_" + agentID
	installationID := fx.Insert(t, "channel_installation", testutil.Cols{
		"workspace_id": workspaceID, "agent_id": agentID, "installer_user_id": userID,
		"channel_type": "feishu", "status": "active",
		"config": testutil.Raw("jsonb_build_object('app_id', '" + appID + "', 'tenant_key', 'tenant')"),
	})
	fx.InsertNoID(t, "feishu_login_identity", testutil.Cols{
		"app_id": "cli_independent_" + userID, "open_id": "ou_login_member", "tenant_key": "tenant", "union_id": "on_" + userID, "user_id": userID,
	}, "app_id = $1", "cli_independent_"+userID)
	store := NewChannelStore(db.New(pool))
	inst, err := store.GetLarkInstallationByAppID(context.Background(), appID)
	if err != nil {
		t.Fatal(err)
	}
	resolver := &feishuIdentityResolver{store: store}
	resolved := engine.ResolvedInstallation{ID: inst.ID, WorkspaceID: inst.WorkspaceID, Platform: inst}
	raw, err := json.Marshal(InboundMessage{SenderTenantKey: "different-tenant"})
	if err != nil {
		t.Fatal(err)
	}
	msg := channel.InboundMessage{Source: channel.Source{SenderID: "ou_bot_member", SenderStableID: "on_" + userID}, Raw: raw}
	if _, err := resolver.ResolveSender(context.Background(), resolved, msg); !errors.Is(err, engine.ErrSenderUnbound) {
		t.Fatalf("identities from another tenant must not match: %v", err)
	}
	msg.Raw, err = json.Marshal(InboundMessage{SenderTenantKey: "tenant"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := resolver.ResolveSender(context.Background(), resolved, msg); !errors.Is(err, engine.ErrSenderNotMember) {
		t.Fatalf("OAuth sign-in alone must not allow bot use: %v", err)
	}
	fx.Member(t, workspaceID, userID, "member")
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM channel_user_binding WHERE installation_id = $1`, installationID)
	})
	identity, err := resolver.ResolveSender(context.Background(), resolved, msg)
	if err != nil || util.UUIDToString(identity.UserID) != userID {
		t.Fatalf("joined member must not need an email/bot linking step: identity=%+v err=%v", identity, err)
	}
	link, err := store.GetLarkUserBindingByOpenID(context.Background(), GetUserBindingByOpenIDParams{
		InstallationID: inst.ID, ChannelUserID: "ou_bot_member",
	})
	if err != nil || util.UUIDToString(link.MulticaUserID) != userID {
		t.Fatalf("bot account link was not materialized: link=%+v err=%v", link, err)
	}
}
