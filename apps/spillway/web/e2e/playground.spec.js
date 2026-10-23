import { expect, test } from "@playwright/test";
import { execFileSync, spawn } from "node:child_process";
import { mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import { createServer } from "node:net";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";

const fallbackHTML = await readFile(new URL("../dashboard.html", import.meta.url), "utf8");
const fallbackCSS = await readFile(new URL("../dashboard.css", import.meta.url), "utf8");
const fallbackJS = await readFile(new URL("../dashboard_fallback.js", import.meta.url), "utf8");

async function startMockSpillway() {
  const root = fileURLToPath(new URL("../../../../", import.meta.url));
  const tempDir = await mkdtemp(join(tmpdir(), "spillway-playground-"));
  const portListener = createServer();
  await new Promise((resolve, reject) => {
    portListener.once("error", reject);
    portListener.listen(0, "127.0.0.1", resolve);
  });
  const { port } = portListener.address();
  await new Promise((resolve, reject) => portListener.close((error) => error ? reject(error) : resolve()));

  const example = await readFile(join(root, "examples", "config.mock.yaml"), "utf8");
  const databasePath = join(tempDir, "usage.db").replaceAll("\\", "/");
  const configPath = join(tempDir, "config.yaml");
  await writeFile(configPath, example
    .replace('listen: "127.0.0.1:8080"', `listen: "127.0.0.1:${port}"`)
    .replace("path: ./spillway-mock.db", `path: ${databasePath}`), "utf8");

  const binaryPath = join(tempDir, process.platform === "win32" ? "spillway.exe" : "spillway");
  execFileSync("go", ["build", "-o", binaryPath, "./apps/spillway"], {
    cwd: root,
    env: { ...process.env, CGO_ENABLED: "0" },
    stdio: "pipe",
  });
  const processHandle = spawn(binaryPath, ["serve", "-config", configPath], { cwd: tempDir, stdio: "ignore" });
  const baseURL = `http://127.0.0.1:${port}`;
  await expect.poll(async () => {
    try {
      return (await fetch(`${baseURL}/healthz`)).status;
    } catch {
      return 0;
    }
  }, { timeout: 30000 }).toBe(200);

  return {
    baseURL,
    async close() {
      if (processHandle.exitCode === null) {
        await new Promise((resolve) => {
          processHandle.once("exit", resolve);
          processHandle.kill();
        });
      }
      await rm(tempDir, { recursive: true, force: true });
    },
  };
}

async function connectDashboard(page, calls, { build = "embedded", models = [{ id: "mock/paid" }, { id: "mock/echo" }], dashboardKey = "sk-test-dashboard" } = {}) {
  if (build === "fallback") {
    await page.route("**/dashboard", (route) => route.fulfill({ contentType: "text/html", body: fallbackHTML }));
    await page.route("**/dashboard.css", (route) => route.fulfill({ contentType: "text/css", body: fallbackCSS }));
    await page.route("**/dashboard.js", (route) => route.fulfill({ contentType: "text/javascript", body: fallbackJS }));
  }
  await page.route("**/api/**", async (route) => {
    const request = route.request();
    calls.push(request);
    expect(request.headers().authorization).toBe(`Bearer ${dashboardKey}`);
    expect(request.headers().cookie).toBeUndefined();
    const path = new URL(request.url()).pathname;
    if (path === "/api/budget") {
      await route.fulfill({ status: 200, contentType: "application/json", body: "{}" });
    } else if (path === "/api/providers") {
      await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ providers: [] }) });
    } else if (path === "/api/pricing") {
      await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ prices: { "mock/paid": { input_micro_usd_per_million: 500000, output_micro_usd_per_million: 1000000 } } }) });
    } else {
      await route.fulfill({ status: 503, contentType: "application/json", body: JSON.stringify({ error: { code: "store_disabled", message: "Usage storage is not configured." } }) });
    }
  });
  if (models) {
    await page.route("**/v1/models", async (route) => {
      calls.push(route.request());
      expect(route.request().headers().authorization).toBe(`Bearer ${dashboardKey}`);
      await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ data: models }) });
    });
  }
  await page.goto("/dashboard");
  await page.getByRole("textbox", { name: "Dashboard key" }).fill(dashboardKey);
  await page.getByRole("button", { name: "Connect" }).click();
  await expect(page.getByRole("heading", { name: "Request playground" })).toBeVisible();
}

for (const build of ["embedded", "fallback"]) {
  test(`${build} playground defaults to a virtual model and explains direct references`, async ({ page }) => {
    await connectDashboard(page, [], {
      build,
      models: [{ id: "mock/echo" }, { id: "spillway/dead" }, { id: "auto" }],
    });

    await expect(page.getByLabel("Model")).toHaveValue("auto");
    await expect(page.getByText("Direct model references skip spillover. Choose auto or spillway/<tier> to let Spillway try eligible candidates in order.")).toBeVisible();
  });

  test(`${build} copy as curl keeps the placeholder key and request fields`, async ({ page }) => {
    await page.addInitScript(() => {
      window.__clipboardText = "";
      Object.defineProperty(navigator, "clipboard", {
        configurable: true,
        value: { writeText: async (text) => { window.__clipboardText = text; } },
      });
    });
    await connectDashboard(page, [], { build, models: [{ id: "auto" }, { id: "mock/echo" }] });
    await page.getByLabel("Prompt").fill("say 'hello' with spillover");
    await page.getByLabel("Stream response").uncheck();
    await page.getByRole("button", { name: "Copy as curl" }).click();

    const curl = await page.evaluate(() => window.__clipboardText);
    expect(curl).toContain("http://127.0.0.1:4173/v1/chat/completions");
    expect(curl).toContain("Authorization: Bearer <LOCAL_APP_KEY>");
    expect(curl).toMatch(process.platform === "win32" ? /^curl\.exe -sS / : /^curl -sS /);
    expect(curl).toContain('"model":"auto"');
    expect(curl).toContain(process.platform === "win32" ? "say ''hello'' with spillover" : "say '\\''hello'\\'' with spillover");
    expect(curl).toContain('"stream":false');
    expect(curl).toContain('"max_tokens":512');
    expect(curl).not.toContain("sk-test-dashboard");
  });
}

test("mock dead tier exhaustion reaches both playground builds with candidate reasons", async ({ browser }) => {
  const mock = await startMockSpillway();
  try {
    for (const build of ["embedded", "fallback"]) {
      const page = await browser.newPage();
      await page.route("**/v1/**", async (route) => {
        const response = await route.fetch({
          url: `${mock.baseURL}${new URL(route.request().url()).pathname}`,
        });
        await route.fulfill({ response });
      });
      await connectDashboard(page, [], {
        build,
        models: null,
        dashboardKey: "sk-test-spillway-dashboard",
      });
      await page.getByLabel("Model").selectOption("spillway/dead");
      await page.getByLabel("Prompt").fill("real mock dead tier exhaustion");
      await page.getByRole("button", { name: "Send request" }).click();
      await expect(page.getByRole("alert")).toContainText('No candidate in tier "dead" can serve this request');
      const candidates = page.getByRole("list", { name: "Exhausted candidates" });
      await expect(candidates.getByRole("listitem").filter({ hasText: "mock/rate-limited" })).toContainText("rate limited");
      await expect(candidates.getByRole("listitem").filter({ hasText: "mock/unavailable" })).toContainText("unavailable");
      await expect(page.getByLabel("Routing explanation")).toContainText("tier dead; exhausted (2 eligible)");
      await page.close();
    }
  } finally {
    await mock.close();
  }
});

for (const build of ["embedded", "fallback"]) test(`${build} request playground sends an explicit non-streaming mock request and keeps its text private`, async ({ page }) => {
  const calls = [];
  await connectDashboard(page, calls, { build });
  await expect(page.getByText(/Estimated cost: about/)).toBeVisible();
  await page.getByLabel("Model").selectOption("mock/echo");
  await expect(page.getByText(/Estimated cost: about/)).toHaveCount(0);
  await page.getByLabel("Model").selectOption("mock/paid");
  await page.getByLabel("Stream response").uncheck();
  await page.getByLabel("Prompt").fill("playground-private-prompt-9c41");

  let chatRequest;
  await page.route("**/v1/chat/completions", async (route) => {
    chatRequest = route.request();
    expect(chatRequest.headers().authorization).toBe("Bearer sk-test-dashboard");
    expect(new URL(chatRequest.url()).origin).toBe(new URL(page.url()).origin);
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      headers: { "x-spillway-provider": "mock", "x-spillway-model": "paid", "x-spillway-route": "tier demo; candidate 2/3", "x-spillway-cache": "miss" },
      body: JSON.stringify({ choices: [{ message: { role: "assistant", content: "mock response text" } }] }),
    });
  });
  await page.getByRole("button", { name: "Send request" }).focus();
  await page.keyboard.press("Enter");
  await expect(page.getByLabel("Assistant response")).toContainText("mock response text");
  await expect(page.getByLabel("Routing explanation")).toContainText("mock/paid");
  await expect(page.getByLabel("Routing explanation")).toContainText("tier demo; candidate 2/3");
  await expect(page.getByLabel("Routing explanation")).toContainText("Spillway used the requested demo tier. Candidate 2 of 3 answered. Earlier candidates were skipped or failed.");
  await expect(page.getByLabel("Routing explanation").getByRole("link", { name: "Cooldown" })).toHaveAttribute("href", "#cooldown-definition");
  await expect(page.getByLabel("Routing explanation")).toContainText("miss");
  expect(chatRequest.postDataJSON()).toEqual({
    model: "mock/paid",
    messages: [{ role: "user", content: "playground-private-prompt-9c41" }],
    stream: false,
    max_tokens: 512,
  });
  expect(await page.evaluate(() => document.body.innerText)).not.toContain("sk-test-dashboard");
  expect(await page.evaluate(() => document.body.innerText)).not.toContain("playground-private-prompt-9c41");
  expect(await page.evaluate(() => JSON.stringify(localStorage))).not.toContain("playground-private-prompt-9c41");
  expect(await page.evaluate(() => JSON.stringify(sessionStorage))).not.toContain("playground-private-prompt-9c41");
  expect(calls.map((request) => request.url()).join("\n")).not.toContain("playground-private-prompt-9c41");
  expect(calls.map((request) => new URL(request.url()).pathname)).toContain("/api/pricing");
});

for (const build of ["embedded", "fallback"]) test(`${build} request playground renders streamed content, reports errors and cancels in flight work`, async ({ page }) => {
  await connectDashboard(page, [], { build });
  await page.getByLabel("Prompt").fill("stream this");
  let mode = "stream";
  let release;
  await page.route("**/v1/chat/completions", async (route) => {
    if (mode === "stream") {
      await route.fulfill({
        status: 200,
        contentType: "text/event-stream",
        headers: { "x-spillway-provider": "mock", "x-spillway-model": "echo", "x-spillway-route": "direct", "x-spillway-cache": "bypass" },
        body: 'data: {"choices":[{"delta":{"content":"hello "}}]}\n\ndata: {"choices":[{"delta":{"content":"world"}}]}\n\ndata: [DONE]\n\n',
      });
    } else if (mode === "error") {
      await route.fulfill({ status: 429, contentType: "application/json", headers: { "x-spillway-route": "auto; exhausted (1 eligible)" }, body: JSON.stringify({ error: { message: "Mock provider is rate limited." } }) });
    } else if (mode === "drop") {
      await route.fulfill({ status: 200, contentType: "text/event-stream", body: 'data: {"choices":[{"delta":{"content":"partial"}}]}\n\n' });
    } else {
      await new Promise((resolve) => { release = resolve; });
      await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ choices: [{ message: { content: "late" } }] }) }).catch(() => {});
    }
  });
  await page.getByRole("button", { name: "Send request" }).click();
  await expect(page.getByLabel("Assistant response")).toContainText("hello world");
  await expect(page.getByRole("status").filter({ hasText: "Request complete." })).toBeVisible();

  mode = "error";
  await page.getByRole("button", { name: "Send request" }).click();
  await expect(page.getByRole("alert")).toHaveText("Mock provider is rate limited.");
  await expect(page.getByLabel("Routing explanation")).toContainText("auto; exhausted (1 eligible)");
  await expect(page.getByLabel("Routing explanation")).toContainText("Spillway had no eligible candidate that could serve this request.");

  mode = "drop";
  await page.getByRole("button", { name: "Send request" }).click();
  await expect(page.getByLabel("Assistant response")).toContainText("partial");
  await expect(page.getByRole("alert")).toHaveText("The response stream ended before completion.");

  mode = "pending";
  await page.getByRole("button", { name: "Send request" }).click();
  await page.getByRole("button", { name: "Cancel request" }).focus();
  await page.keyboard.press("Enter");
  await expect(page.getByText("Request cancelled.")).toBeVisible();
  release?.();
});

test("fallback playground reports a dropped stream as incomplete", async ({ page }) => {
  await page.route("**/dashboard", (route) => route.fulfill({ contentType: "text/html", body: fallbackHTML }));
  await page.route("**/dashboard.css", (route) => route.fulfill({ contentType: "text/css", body: fallbackCSS }));
  await page.route("**/dashboard.js", (route) => route.fulfill({ contentType: "text/javascript", body: fallbackJS }));
  await page.route("**/api/**", async (route) => {
    const path = new URL(route.request().url()).pathname;
    const body = path === "/api/pricing" ? { prices: {} }
      : path === "/api/providers" ? { providers: [] }
        : path === "/api/quotas" ? { candidates: [] }
          : { error: { code: "store_disabled", message: "Usage storage is not configured." } };
    await route.fulfill({ status: path === "/api/usage" ? 503 : 200, contentType: "application/json", body: JSON.stringify(body) });
  });
  await page.route("**/v1/models", (route) => route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ data: [{ id: "mock/echo" }] }) }));
  let dropStream = false;
  await page.route("**/v1/chat/completions", (route) => route.fulfill({
    status: 200,
    contentType: "text/event-stream",
    body: dropStream
      ? 'data: {"choices":[{"delta":{"content":"partial fallback"}}]}\n\n'
      : 'data: {"choices":[{"delta":{"content":"fallback answer"}}]}\n\ndata: [DONE]\n\n',
  }));

  await page.goto("/dashboard");
  await page.getByRole("textbox", { name: "Dashboard key" }).fill("sk-test-dashboard");
  await page.getByRole("button", { name: "Connect" }).click();
  await page.getByLabel("Prompt").fill("test incomplete stream");
  await page.getByRole("button", { name: "Send request" }).click();
  await expect(page.getByLabel("Assistant response")).toContainText("fallback answer");
  await expect(page.getByRole("status").filter({ hasText: "Request complete." })).toBeVisible();

  dropStream = true;
  await page.getByRole("button", { name: "Send request" }).click();
  await expect(page.getByLabel("Assistant response")).toContainText("partial fallback");
  await expect(page.getByRole("alert")).toHaveText("The response stream ended before completion.");
});

for (const build of ["embedded", "fallback"]) test(`${build} explains the final candidate and preserves unknown route reasons`, async ({ page }) => {
  await connectDashboard(page, [], { build });
  await page.getByLabel("Prompt").fill("explain this route");
  let routeReason = "tier demo; candidate 3/3";
  await page.route("**/v1/chat/completions", (route) => route.fulfill({
    status: 200,
    contentType: "application/json",
    headers: { "x-spillway-provider": "mock", "x-spillway-model": "echo", "x-spillway-route": routeReason, "x-spillway-cache": "miss" },
    body: JSON.stringify({ choices: [{ message: { content: "mock response" } }] }),
  }));

  await page.getByRole("button", { name: "Send request" }).click();
  await expect(page.getByLabel("Routing explanation")).toContainText("tier demo; candidate 3/3");
  await expect(page.getByLabel("Routing explanation")).toContainText("Candidate 3 of 3 answered. Earlier candidates were skipped or failed.");

  routeReason = "provider supplied unknown marker";
  await page.getByRole("button", { name: "Send request" }).click();
  await expect(page.getByLabel("Routing explanation")).toContainText("provider supplied unknown marker");
  await expect(page.getByLabel("Routing explanation")).toContainText("No plain-language explanation is available; the route reason above is shown verbatim.");
});
