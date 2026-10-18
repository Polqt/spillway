import { expect, test } from "@playwright/test";
import { readFile } from "node:fs/promises";

const fallbackHTML = await readFile(new URL("../dashboard.html", import.meta.url), "utf8");
const fallbackCSS = await readFile(new URL("../dashboard.css", import.meta.url), "utf8");
const fallbackJS = await readFile(new URL("../dashboard_fallback.js", import.meta.url), "utf8");

test("dashboard key connects, explains optional history and can be forgotten", async ({ page }) => {
  const calls = [];
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
        body: JSON.stringify({ providers: [{ name: "mock", credential_required: false, credential_present: false, signals: [] }] }),
      });
      return;
    }
    await route.fulfill({ status: 503, contentType: "application/json", body: JSON.stringify({ error: { message: "Usage storage is not configured." } }) });
  });

  await page.clock.install({ time: new Date("2026-10-18T10:00:00Z") });
  await page.goto("/dashboard");
  await expect(page.getByRole("heading", { name: "Welcome to Spillway." })).toBeVisible();
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
  expect(calls).toHaveLength(3);
  expect(calls.map((call) => new URL(call.url()).pathname)).toContain("/api/providers");
  expect(calls.map((call) => new URL(call.url()).pathname)).toContain("/api/usage");
  expect(await page.evaluate(() => sessionStorage.getItem("spillway-dashboard-key"))).toBe("sk-test-dashboard");
  await page.clock.fastForward(10000);
  await expect.poll(() => calls.length).toBe(6);

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
  await page.route("**/api/providers", (route) => route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ providers: [] }) }));
  await page.route("**/api/usage**", (route) => route.fulfill({ status: 503, contentType: "application/json", body: JSON.stringify({ error: { code: "store_disabled", message: "Usage storage is not configured." } }) }));

  await page.setViewportSize({ width: 320, height: 900 });
  await page.goto("/dashboard");
  await page.getByLabel("Dashboard key").fill("sk-test-dashboard");
  await page.getByRole("button", { name: "Connect" }).click();
  await expect(page.locator("#dashboard-content")).toBeVisible();
  await expect(page.locator("#connection-label")).toHaveText("Connected");
  await expect(page.locator("#updated-at")).toHaveText("Connected · usage history is not stored");
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

test("stored history remains available and renders untrusted model names as text", async ({ page }) => {
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
      await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ data: [{ at: "2026-10-18T10:00:00Z", model: "<img src=x onerror=alert(1)>", provider: "mock", status: 200, route: "auto; candidate 1/1" }] }) });
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
  await expect(page.locator("img")).toHaveCount(0);
});
