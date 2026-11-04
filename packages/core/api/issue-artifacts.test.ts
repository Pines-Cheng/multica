// @vitest-environment node
import { afterEach, expect, it, vi } from "vitest";
import { ApiClient } from "./client";

afterEach(() => vi.unstubAllGlobals());
const client = new ApiClient("https://api.example.test");

function respond(body: unknown) {
  const fetch = vi.fn().mockResolvedValue(new Response(JSON.stringify(body)));
  vi.stubGlobal("fetch", fetch);
  return fetch;
}

it("preserves new artifact kinds and uses a missing kind as an empty classification", async () => {
  respond({ artifacts: [
    { key: "design", title: "技术方案", url: "https://docs.example/design", kind: "any-new-type" },
    { key: "report", title: "压测报告", url: "https://docs.example/report" },
  ] });
  const result = await client.listIssueArtifacts("issue");
  expect(result.artifacts.map((item) => [item.title, item.kind])).toEqual([["技术方案", "any-new-type"], ["压测报告", ""]]);
});

it.each([{}, null, { artifacts: null }, { artifacts: [{ key: "broken", title: 42 }] },
  { artifacts: [{ key: "script", title: "Report", url: "javascript:alert(1)" }] },
])("degrades missing or malformed lists to an empty list: %j", async (body) => {
  respond(body);
  await expect(client.listIssueArtifacts("issue")).resolves.toEqual({ artifacts: [] });
});

it("writes the declared display title and free classification", async () => {
  const artifact = { title: "验收记录", url: "https://docs.example/review", kind: "review-v3" };
  const fetch = respond({ artifacts: [{ key: "review", ...artifact }], issue_revision: 4 });
  await expect(client.setIssueArtifact("DEV-43", "review", artifact)).resolves.toMatchObject({ issue_revision: 4 });
  expect(fetch).toHaveBeenCalledWith("https://api.example.test/api/issues/DEV-43/artifacts/review", expect.objectContaining({
    method: "PUT", body: JSON.stringify(artifact),
  }));
});

it("does not report malformed or missing saved artifacts as success", async () => {
  respond({ artifacts: [] });
  await expect(client.setIssueArtifact("issue", "report", { title: "Report", url: "https://docs.example/report" })).rejects.toThrow("Could not save");
});

it("deletes by stable key", async () => {
  const fetch = respond({ artifacts: [] });
  await client.deleteIssueArtifact("issue", "report");
  expect(fetch).toHaveBeenCalledWith("https://api.example.test/api/issues/issue/artifacts/report", expect.objectContaining({ method: "DELETE" }));
});
