// @vitest-environment node
import { expect, it } from "vitest";
import { SquadSchema } from "./schemas";

const squad = {
  id: "s", workspace_id: "w", name: "Squad", leader_id: "a", creator_id: "u",
  created_at: "", updated_at: "",
};

it("treats an older server's squad as auto-opening Feishu groups", () => {
  expect(SquadSchema.parse(squad).auto_create_channel_chat).toBe(true);
});

it("keeps a squad that switched automatic groups off", () => {
  expect(SquadSchema.parse({ ...squad, auto_create_channel_chat: false }).auto_create_channel_chat).toBe(false);
});

it("reads a malformed switch as the server default", () => {
  expect(SquadSchema.parse({ ...squad, auto_create_channel_chat: "no" }).auto_create_channel_chat).toBe(true);
});
