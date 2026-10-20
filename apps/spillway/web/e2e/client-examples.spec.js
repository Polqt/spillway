import { createRequire } from "node:module";
import { expect, test } from "@playwright/test";
import { readFile } from "node:fs/promises";

const require = createRequire(new URL("../package.json", import.meta.url));
const AxeBuilder = require("@axe-core/playwright");

const fallbackHTML = await readFile(new URL("../dashboard.html", import.meta.url), "utf8");
const fallbackCSS = await readFile(new URL("../dashboard.css", import.meta.url), "utf8");
const fallbackJS = await readFile(new URL("../dashboard_fallback.js", import.meta.url), "utf8");
const dashboardKey = "sk-test-dashboard-canary";

async function connect(page, assets) {
  await page.route("**/api/**", async (route) => {
    const request = route.request();
    expect(request.headers().authorization).toBe(`Bearer ${dashboardKey}`);
    const path = new URL(request.url()).pathname;
    const response = path === "/api/gateway" ? { version: "v1.1.0", dashboard_assets: assets }
      : path === "/api/providers" ? { providers: [] }
        : path === "/api/pricing" ? { prices: {} }
          : path === "/api/quotas" ? { candidates: [] }
            : path === "/api/budget" ? {}
              : { error: { code: "store_disabled", message: "Usage storage is not configured." } };
    await route.fulfill({ status: path.startsWith("/api/usage") ? 503 : 200, contentType: "application/json", body: JSON.stringify(response) });
  });
  await page.route("**/v1/models", (route) => route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ data: [{ id: "mock/echo" }] }) }));
}

async function enterKey(page) {
  await page.getByRole("textbox", { name: "Dashboard key" }).fill(dashboardKey);
  await page.getByRole("button", { name: "Connect" }).click();
  await expect(page.getByText("Connected to Spillway v1.1.0")).toBeVisible();
}

test("gateway version and client examples use the page origin and placeholder key", async ({ page, context }) => {
  await connect(page, "embedded");
  await context.grantPermissions(["clipboard-read", "clipboard-write"]);
  await page.goto("/dashboard");
  await enterKey(page);

  const examples = page.getByRole("region", { name: "Connect a client app" });
  await expect(examples).toBeVisible();
  await expect(examples).toContainText("default");
  await expect(examples).not.toContainText(dashboardKey);
  expect(await page.locator("body").innerText()).not.toContain(dashboardKey);
  const origin = new URL(page.url()).origin;
  const tabNames = ["curl (POSIX)", "curl (PowerShell)", "Python", "Node.js", "Environment"];
  for (const name of tabNames) {
    const tab = examples.getByRole("tab", { name });
    await expect(tab).toBeVisible();
    await tab.click();
    const panel = examples.getByRole("tabpanel");
    await expect(panel).toContainText(`${origin}/v1`);
    await expect(panel).toContainText("<LOCAL_APP_KEY>");
    await expect(panel).toContainText("auto");
  }
  await examples.getByRole("tab", { name: "curl (POSIX)" }).click();
  await examples.getByRole("button", { name: "Copy example" }).click();
  await expect(page.getByRole("status").filter({ hasText: "Copied" })).toBeVisible();
  const clipboard = await page.evaluate(() => navigator.clipboard.readText());
  expect(clipboard).toContain(`${origin}/v1`);
  expect(clipboard).toContain("<LOCAL_APP_KEY>");
  expect(clipboard).not.toContain(dashboardKey);
});

test("fallback gateway identity and static client examples use the page origin without keys", async ({ page }) => {
  await page.route("**/dashboard", (route) => route.fulfill({ contentType: "text/html", body: fallbackHTML }));
  await page.route("**/dashboard.css", (route) => route.fulfill({ contentType: "text/css", body: fallbackCSS }));
  await page.route("**/dashboard.js", (route) => route.fulfill({ contentType: "text/javascript", body: fallbackJS }));
  await connect(page, "fallback");
  await page.goto("/dashboard");
  await enterKey(page);

  const examples = page.getByRole("region", { name: "Connect a client app" });
  await expect(examples).toBeVisible();
  const details = examples.locator("details");
  await expect(details).toHaveCount(5);
  await expect(examples).toContainText("default");
  await expect(examples).not.toContainText(dashboardKey);
  expect(await page.locator("body").innerText()).not.toContain(dashboardKey);
  const origin = new URL(page.url()).origin;
  for (let index = 0; index < await details.count(); index++) {
    const item = details.nth(index);
    await item.locator("summary").click();
    await expect(item).toContainText(`${origin}/v1`);
    await expect(item).toContainText("<LOCAL_APP_KEY>");
    await expect(item).toContainText("auto");
  }
  for (const [width, colorScheme] of [[1280, "light"], [1280, "dark"], [320, "dark"]]) {
    await page.setViewportSize({ width, height: 900 });
    await page.emulateMedia({ colorScheme });
    const results = await new AxeBuilder({ page }).include(".client-examples").withTags(["wcag2a", "wcag2aa", "wcag21a", "wcag21aa", "wcag22aa"]).analyze();
    expect(results.violations).toEqual([]);
  }
});
