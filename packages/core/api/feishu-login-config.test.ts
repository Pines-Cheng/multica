// @vitest-environment node
import { describe, expect, it } from "vitest";
import { AppConfigSchema, EMPTY_APP_CONFIG } from "./schemas";

describe("Feishu sign-in config", () => {
  it("stays disabled for older and malformed responses", () => {
    expect(EMPTY_APP_CONFIG.feishu_login_enabled).toBe(false);
    for (const value of [undefined, null, "true", 1]) {
      expect(AppConfigSchema.parse({ feishu_login_enabled: value }).feishu_login_enabled).toBe(false);
    }
    expect(AppConfigSchema.parse({ feishu_login_enabled: true }).feishu_login_enabled).toBe(true);
  });
});
