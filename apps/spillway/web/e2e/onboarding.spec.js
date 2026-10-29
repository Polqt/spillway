import { expect, test } from "@playwright/test";
import { readFile } from "node:fs/promises";

const fallbackHTML = await readFile(new URL("../dashboard.html", import.meta.url), "utf8");
const fallbackCSS = await readFile(new URL("../dashboard.css", import.meta.url), "utf8");
const fallbackJS = await readFile(new URL("../dashboard_fallback.js", import.meta.url), "utf8");

test("dashboard key connects, explains optional history and can be forgotten", async ({ page }) => {
  const calls = [];
  await page.setViewportSize({ width: 320, height: 900 });
  await page.route("**/api/**", async (route) => {
    calls.push(route.request());
    expect(route.request().headers().authorization).toBe("Bearer sk-test-dashboard");
    expect(route.request().headers().cookie).toBeUndefined();
    if (route.request().url().includes("/api/budget")) {
      await route.fulfill({ status: 200, contentType: "application/json", body: "{}" });
      return;
    }
    if (route.request().url().includes("/api/providers")) {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({ providers: [{ name: "mock", credential_required: false, credential_present: false, signals: [{ model: "mock-model-with-a-long-unbroken-name-for-mobile-layouts", reason: "rate_limit", breaker_state: "open", upstream_failures: 7, cooldown_until: "2026-10-18T10:01:00Z" }] }] }),
      });
      return;
    }
    if (route.request().url().includes("/api/pricing")) {
      await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ prices: {} }) });
      return;
    }
    await route.fulfill({ status: 503, contentType: "application/json", body: JSON.stringify({ error: { code: "store_disabled", message: "Usage storage is not configured." } }) });
  });
  await page.route("**/v1/models", (route) => route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ data: [{ id: "mock/echo" }] }) }));
  await page.route("**/v1/chat/completions", (route) => route.fulfill({
    status: 200,
    contentType: "text/event-stream",
    headers: { "x-spillway-provider": "mock", "x-spillway-model": "echo", "x-spillway-route": "direct; unknown marker", "x-spillway-cache": "hit" },
    body: 'data: {"choices":[{"delta":{"content":"fallback answer"}}]}\n\ndata: [DONE]\n\n',
  }));

  await page.clock.install({ time: new Date("2026-10-18T10:00:00Z") });
  await page.goto("/dashboard");
  await expect(page.getByRole("heading", { name: "Your gateway, in view." })).toBeVisible();
  await expect(page.getByRole("heading", { name: "Your dashboard is ready." })).toHaveCount(0);
  expect(calls).toHaveLength(0);

  await page.keyboard.press("Tab");
  await page.keyboard.press("Tab");
  await expect(page.getByRole("textbox", { name: "Dashboard key" })).toBeFocused();
  await page.getByRole("textbox", { name: "Dashboard key" }).fill("sk-test-dashboard");
  await page.keyboard.press("Tab");
  await expect(page.getByRole("button", { name: "Connect" })).toBeFocused();
  await page.keyboard.press("Enter");

  await expect(page.getByRole("heading", { name: "Your dashboard is ready." })).toBeVisible();
  await expect(page.getByText(/work without store:/)).toBeVisible();
  await expect(page.getByRole("button", { name: "Forget dashboard key" })).toBeVisible();
  await expect(page.getByRole("heading", { name: "Provider status" })).toBeVisible();
  await expect(page.locator(".provider-status")).toContainText("mock");
  await expect(page.locator(".provider-status")).toContainText("No credential required");
  await expect(page.locator(".provider-status")).toContainText("At least 7 upstream failures observed.");
  await expect(page.locator(".provider-status")).toContainText("Rate limit · Circuit breaker open");
  await expect(page.locator(".provider-status")).toContainText("Observed since this server started");
  await expect(page.locator(".provider-status")).toContainText("Last refreshed");
  await expect(page.locator(".provider-status")).toContainText(/in (?:\d+ seconds|1 minute)/);
  await expect(page.locator(".provider-status")).toContainText("mock-model-with-a-long-unbroken-name-for-mobile-layouts");
  const dimensions = await page.evaluate(() => ({ viewport: document.documentElement.clientWidth, content: document.documentElement.scrollWidth }));
  expect(dimensions.content).toBeLessThanOrEqual(dimensions.viewport);
  expect(calls).toHaveLength(5);
  expect(calls.map((call) => new URL(call.url()).pathname)).toContain("/api/providers");
  expect(calls.map((call) => new URL(call.url()).pathname)).toContain("/api/usage");
  expect(await page.evaluate(() => sessionStorage.getItem("spillway-dashboard-key"))).toBe("sk-test-dashboard");
  await page.clock.fastForward(10000);
  await expect.poll(() => calls.length).toBe(10);

  await page.getByRole("button", { name: "Forget dashboard key" }).click();
  await expect(page.getByRole("heading", { name: "Connect with a dashboard key" })).toBeVisible();
  expect(await page.evaluate(() => sessionStorage.getItem("spillway-dashboard-key"))).toBeNull();
});

test("dashboard onboarding reflows at narrow and 200 percent equivalent widths", async ({ page }) => {
  for (const width of [320, 640]) {
    await page.setViewportSize({ width, height: 900 });
    await page.goto("/dashboard");
    await expect(page.getByRole("textbox", { name: "Dashboard key" })).toBeVisible();
    const dimensions = await page.evaluate(() => ({
      viewport: document.documentElement.clientWidth,
      content: document.documentElement.scrollWidth,
    }));
    expect(dimensions.content).toBeLessThanOrEqual(dimensions.viewport);
  }
});

test("Go install fallback connects and stays usable without usage storage", async ({ page }) => {
  await page.route("**/dashboard", (route) => route.fulfill({ contentType: "text/html", body: fallbackHTML }));
  await page.route("**/dashboard.css", (route) => route.fulfill({ contentType: "text/css", body: fallbackCSS }));
  await page.route("**/dashboard.js", (route) => route.fulfill({ contentType: "text/javascript", body: fallbackJS }));
  await page.route("**/api/budget", (route) => route.fulfill({ status: 200, contentType: "application/json", body: "{}" }));
  await page.route("**/api/quotas", (route) => route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ candidates: [] }) }));
  await page.route("**/api/providers", (route) => route.fulfill({
    status: 200,
    contentType: "application/json",
    body: JSON.stringify({ providers: [{ name: "mock", credential_required: false, credential_present: false, signals: [{ model: "mock-model-with-a-long-unbroken-name-for-mobile-layouts", reason: "rate_limit", breaker_state: "open", upstream_failures: 7, cooldown_until: "2026-10-18T10:01:00Z" }] }] }),
  }));
  await page.route("**/api/pricing", (route) => route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ prices: {} }) }));
  await page.route("**/v1/models", (route) => route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ data: [{ id: "mock/echo" }] }) }));
  let fallbackRoute = "direct; unknown marker";
  let fallbackCache = "hit";
  await page.route("**/v1/chat/completions", (route) => route.fulfill({
    status: 200,
    contentType: "application/json",
    headers: { "x-spillway-provider": "mock", "x-spillway-model": "echo", "x-spillway-route": fallbackRoute, "x-spillway-cache": fallbackCache },
    body: JSON.stringify({ choices: [{ message: { content: "fallback answer" } }] }),
  }));
  await page.route("**/api/usage**", (route) => route.fulfill({ status: 503, contentType: "application/json", body: JSON.stringify({ error: { code: "store_disabled", message: "Usage storage is not configured." } }) }));

  await page.setViewportSize({ width: 320, height: 900 });
  await page.clock.install({ time: new Date("2026-10-18T10:00:00Z") });
  await page.goto("/dashboard");
  await page.getByRole("textbox", { name: "Dashboard key" }).fill("sk-test-dashboard");
  await page.getByRole("button", { name: "Connect" }).click();
  await expect(page.locator("#dashboard-content")).toBeVisible();
  await expect(page.locator("#connection-label")).toHaveText("Connected");
  await expect(page.locator("#updated-at")).toHaveText("Connected · usage history is not stored");
  await expect(page.locator("#provider-status")).toContainText("At least 7 upstream failures observed.");
  await expect(page.locator("#provider-status")).toContainText("Rate limit · Circuit breaker open");
  await expect(page.locator("[aria-labelledby='provider-status-heading']")).toContainText("Observed since this server started");
  await expect(page.locator("#provider-status-refreshed")).toContainText("Last refreshed");
  await expect(page.locator("#provider-status")).toContainText(/in (?:\d+ seconds|1 minute)/);
  await expect(page.locator("#provider-status")).toContainText("mock-model-with-a-long-unbroken-name-for-mobile-layouts");
  await page.getByLabel("Stream response").uncheck();
  await page.getByLabel("Prompt").fill("fallback private prompt");
  await page.getByRole("button", { name: "Send request" }).click();
  await expect(page.locator("#playground-route")).toContainText("direct; unknown marker");
  await expect(page.locator("#playground-route")).toContainText("Spillway served the saved exact-cache response.");
  await expect(page.locator("#playground-route")).toContainText("hit");
  await expect(page.locator("#playground-response")).toContainText("fallback answer");
  await expect(page.locator("#playground-message")).toHaveText("Request complete.");
  await expect(page.locator("body")).not.toContainText("fallback private prompt");
  fallbackRoute = "rules; rule 1; difficulty hard; tier free; fallback strong; candidate 1/1";
  fallbackCache = "miss";
  await page.getByRole("button", { name: "Send request" }).click();
  await expect(page.locator("#playground-route")).toContainText("A routing rule selected the hard difficulty in the free tier, then spilled over to the strong fallback tier.");
  fallbackRoute = "tier demo; candidate 2/3";
  await page.getByRole("button", { name: "Send request" }).click();
  await expect(page.locator("#playground-route")).toContainText("Spillway used the requested demo tier. Spillway spilled over to candidate 2 of 3 after earlier candidates were unavailable.");
  await expect(page.locator("#playground-route").getByRole("link", { name: "Circuit breaker" })).toHaveAttribute("href", "#circuit-breaker-definition");
  await expect(page.locator("#cooldown-definition + dd")).toContainText("A period during which a candidate is skipped after a rate limit or another temporary failure.");
  const dimensions = await page.evaluate(() => ({ viewport: document.documentElement.clientWidth, content: document.documentElement.scrollWidth }));
  expect(dimensions.content).toBeLessThanOrEqual(dimensions.viewport);
  expect(await page.evaluate(() => sessionStorage.getItem("spillway-dashboard-key"))).toBe("sk-test-dashboard");
  await page.getByRole("button", { name: "Disconnect" }).click();
  await expect(page.locator("#dashboard-content")).toBeHidden();
  expect(await page.evaluate(() => sessionStorage.getItem("spillway-dashboard-key"))).toBeNull();
});

test("invalid dashboard access gives the config hint without retaining the key", async ({ page }) => {
  await page.route("**/api/providers", (route) => route.fulfill({
    status: 200,
    contentType: "application/json",
    body: JSON.stringify({ providers: [] }),
  }));
  await page.route("**/api/budget", (route) => route.fulfill({
    status: 403,
    contentType: "application/json",
    body: JSON.stringify({ error: { message: "This API key does not have dashboard access." } }),
  }));
  await page.goto("/dashboard");
  await page.getByRole("textbox", { name: "Dashboard key" }).fill("sk-test-chat-only");
  await page.getByRole("button", { name: "Connect" }).click();
  await expect(page.getByRole("alert")).toContainText("dashboard: true");
  expect(await page.evaluate(() => sessionStorage.getItem("spillway-dashboard-key"))).toBeNull();
});

test("provider status API failures remain visible instead of looking like an empty configuration", async ({ page }) => {
  await page.route("**/v1/models", (route) => route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ data: [{ id: "mock/echo" }] }) }));
  await page.route("**/api/pricing", (route) => route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ prices: {} }) }));
  await page.route("**/api/budget", (route) => route.fulfill({ status: 200, contentType: "application/json", body: "{}" }));
  await page.route("**/api/providers", (route) => route.fulfill({
    status: 503,
    contentType: "application/json",
    body: JSON.stringify({ error: { code: "provider_status_unavailable", message: "Provider status is temporarily unavailable." } }),
  }));
  await page.goto("/dashboard");
  await page.getByRole("textbox", { name: "Dashboard key" }).fill("sk-test-dashboard");
  await page.getByRole("button", { name: "Connect" }).click();
  await expect(page.getByRole("alert")).toHaveText("Provider status is temporarily unavailable.");
  await expect(page.getByRole("heading", { name: "Your dashboard is ready." })).toHaveCount(0);
});

test("stored history remains available and renders untrusted model names as text", async ({ page }) => {
  await page.route("**/v1/models", (route) => route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ data: [{ id: "mock/echo" }] }) }));
  await page.route("**/api/**", async (route) => {
    const url = new URL(route.request().url());
    expect(route.request().headers().authorization).toBe("Bearer sk-test-dashboard");
    if (url.pathname === "/api/budget") {
      await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ daily: { spent_micro_usd: 2500, limit_micro_usd: 1000000 } }) });
    } else if (url.pathname === "/api/usage" && url.searchParams.get("group") === "day") {
      await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ data: [{ cost_micro_usd: 2500 }] }) });
    } else if (url.pathname === "/api/usage") {
      await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ data: [{ key: "mock/echo", requests: 1, cost_micro_usd: 2500 }] }) });
    } else if (url.pathname === "/api/decisions") {
      await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ data: [
        { at: "2026-10-18T10:00:00Z", model: "<img src=x onerror=alert(1)>", provider: "mock", status: 200, route: "vendor route reason; new marker", cache: "miss", skips: [{ provider: "other", model: "small", reason: "capability not declared" }], attempts: [{ provider: "mock", model: "echo", outcome: "success" }], prompt: "private prompt must never render" },
        { model: "plain", provider: "mock", status: 200, route: "direct", cache: "miss" },
        { model: "tiered", provider: "mock", status: 200, route: "tier demo; candidate 1/2", cache: "miss" },
        { model: "automatic", provider: "mock", status: 200, route: "auto; needs tools; tier smart; candidate 1/2", cache: "miss" },
        { model: "ruled", provider: "mock", status: 200, route: "rules; rule 1; difficulty hard; tier free; fallback strong; candidate 1/1", cache: "miss" },
        { model: "spilled", provider: "mock", status: 200, route: "auto; tier free; candidate 2/3", cache: "miss", skips: [{ provider: "other", model: "small", reason: "rate limited" }] },
        { model: "cached", provider: "mock", status: 200, route: "tier demo; candidate 1/2", cache: "hit" },
        { model: "failed", status: 503, route: "auto; exhausted (1 eligible)", cache: "miss", attempts: [{ provider: "mock", model: "echo", outcome: "unavailable" }] },
        { model: "rejected", status: 400, route: "direct", cache: "miss" },
        { model: "unconfigured", status: 404, route: "auto; no capable model", cache: "miss" },
        { model: "defaulted", provider: "mock", status: 200, route: "rules; default; difficulty moderate; tier balanced; candidate 1/1", cache: "miss" },
        { model: "classifier-fallback", provider: "mock", status: 200, route: "rules; classifier fallback; rule 1; difficulty hard; tier free; fallback strong; candidate 1/1", cache: "miss" },
      ] }) });
    } else if (url.pathname === "/api/providers") {
      await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ providers: [{ name: "mock", credential_required: false, credential_present: false, signals: [] }] }) });
    } else {
      await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ candidates: [] }) });
    }
  });

  await page.goto("/dashboard");
  await page.getByRole("textbox", { name: "Dashboard key" }).fill("sk-test-dashboard");
  await page.getByRole("button", { name: "Connect" }).click();
  await expect(page.getByRole("heading", { name: "Usage and routing history" })).toBeVisible();
  await expect(page.getByRole("heading", { name: "History storage is enabled" })).toBeVisible();
  await expect(page.getByText("<img src=x onerror=alert(1)>")).toBeVisible();
  const decisions = page.locator(".decision-details");
  await expect(decisions).toHaveCount(12);
  await expect(decisions.nth(0)).toContainText("vendor route reason; new marker");
  await expect(decisions.nth(0)).toContainText("miss");
  await expect(decisions.nth(0)).toContainText("other/small · capability not declared");
  await expect(decisions.nth(0)).toContainText("mock/echo · success");
  await expect(decisions.nth(0)).not.toContainText("private prompt");
  await expect(decisions.nth(0)).toContainText("No plain-language explanation is available; the route reason above is shown verbatim.");
  await expect(decisions.nth(1)).toContainText("Spillway sent the requested model directly to its provider.");
  await expect(decisions.nth(2)).toContainText("Spillway used the requested demo tier. Spillway selected candidate 1 of 2.");
  await expect(decisions.nth(3)).toContainText("Spillway selected a candidate from the configured auto route in the smart tier. Spillway selected candidate 1 of 2.");
  await expect(decisions.nth(4)).toContainText("A routing rule selected the hard difficulty in the free tier, then spilled over to the strong fallback tier.");
  await expect(decisions.nth(5)).toContainText("Spillway spilled over to candidate 2 of 3 after earlier candidates were unavailable.");
  await expect(decisions.nth(6)).toContainText("Spillway served the saved exact-cache response.");
  await expect(decisions.nth(7)).toContainText("Spillway had no eligible candidate that could serve this request.");
  await expect(decisions.nth(8)).toContainText("Spillway handled this as a direct-model request, but it failed before a successful response.");
  await expect(decisions.nth(9)).toContainText("Spillway could not resolve a usable model for this request.");
  await expect(decisions.nth(10)).toContainText("No routing rule matched, so Spillway used the default moderate difficulty in the balanced tier.");
  await expect(decisions.nth(11)).toContainText("The local classifier could not choose a route, so deterministic routing used hard difficulty in the free tier, then spilled over to the strong fallback tier.");
  await expect(page.locator("img")).toHaveCount(0);
});
