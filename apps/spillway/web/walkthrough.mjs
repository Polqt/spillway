import { spawn, spawnSync } from "node:child_process";
import { mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";
import readline from "node:readline/promises";
import { stdin, stdout } from "node:process";
import { chromium, expect } from "@playwright/test";

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../../..");
const port = process.env.SPILLWAY_WALKTHROUGH_PORT || "18080";
const baseURL = `http://127.0.0.1:${port}`;
const tempDir = await mkdtemp(path.join(os.tmpdir(), "spillway-walkthrough-"));
const sourceConfig = await readFile(path.join(root, "examples/config.mock.yaml"), "utf8");
const config = sourceConfig
  .replace('listen: "127.0.0.1:8080"', `listen: "127.0.0.1:${port}"`)
  .replace("path: ./spillway-mock.db", `path: ${path.join(tempDir, "spillway.db").replaceAll("\\", "/")}`);
const configPath = path.join(tempDir, "config.yaml");
const binaryPath = path.join(tempDir, process.platform === "win32" ? "spillway.exe" : "spillway");
const curlConfigPath = path.join(tempDir, "request.curl");
await writeFile(configPath, config);
await writeFile(curlConfigPath, [
  'header = "Authorization: Bearer sk-test-spillway-mock"',
  'header = "Content-Type: application/json"',
].join("\n"));

function cli(args) {
  stdout.write(`\n$ go run ./apps/spillway ${args.join(" ")}\n`);
  const result = spawnSync("go", ["run", "./apps/spillway", ...args], {
    cwd: root,
    encoding: "utf8",
    env: process.env,
  });
  stdout.write(result.stdout || "");
  if (result.stderr) stdout.write(result.stderr);
  if (result.error) throw result.error;
  if (result.status !== 0) throw new Error(`CLI exited with status ${result.status}`);
}

async function waitForServer() {
  const deadline = Date.now() + 60_000;
  while (Date.now() < deadline) {
    if (serverError) throw serverError;
    if (server?.exitCode !== null && server?.exitCode !== undefined) {
      throw new Error(`Spillway exited with status ${server.exitCode}`);
    }
    try {
      const response = await fetch(`${baseURL}/healthz`);
      if (response.ok) return;
    } catch {
      // The Go process may still be starting.
    }
    await new Promise((resolve) => setTimeout(resolve, 250));
  }
  throw new Error(`Spillway did not become ready at ${baseURL}`);
}

let server;
let serverError;
let browser;
try {
  stdout.write("\n=== Spillway CLI ===\n");
  cli(["help"]);
  cli(["version"]);
  cli(["check", "-config", configPath]);
  stdout.write("\n$ go build -o <temporary>/spillway ./apps/spillway\n");
  const build = spawnSync("go", ["build", "-o", binaryPath, "./apps/spillway"], {
    cwd: root,
    encoding: "utf8",
    env: process.env,
  });
  if (build.stdout) stdout.write(build.stdout);
  if (build.stderr) stdout.write(build.stderr);
  if (build.error) throw build.error;
  if (build.status !== 0) throw new Error(`Go build exited with status ${build.status}`);

  stdout.write(`\n$ <temporary>/spillway serve -config ${configPath}\n`);
  server = spawn(binaryPath, ["serve", "-config", configPath], {
    cwd: root,
    stdio: "inherit",
    env: process.env,
  });
  server.on("error", (error) => { serverError = error; });
  await waitForServer();

  const curl = process.platform === "win32" ? "curl.exe" : "curl";
  const requestBody = JSON.stringify({ model: "auto", messages: [{ role: "user", content: "Reply with a short greeting." }] });
  stdout.write(`\n$ ${curl} --config <temporary>/request.curl --data-binary <request> ${baseURL}/v1/chat/completions\n`);
  const request = spawnSync(curl, [
    "--silent", "--show-error", "--fail-with-body", "--config", curlConfigPath,
    "--data-binary", requestBody, `${baseURL}/v1/chat/completions`,
  ], { encoding: "utf8" });
  if (request.error) throw request.error;
  if (request.status !== 0) throw new Error(`CLI API request exited with status ${request.status}: ${request.stderr}`);
  stdout.write(`${JSON.stringify(JSON.parse(request.stdout), null, 2)}\n`);

  stdout.write("\n=== Dashboard walkthrough ===\n");
  browser = await chromium.launch({ headless: false });
  const page = await browser.newPage({ viewport: { width: 1440, height: 1000 } });
  await page.goto(`${baseURL}/dashboard`);
  await expect(page.getByRole("heading", { name: "Connect your dashboard" })).toBeVisible();
  await page.getByLabel("Dashboard key").fill("sk-test-spillway-dashboard");
  await page.getByRole("button", { name: "Connect" }).click();
  await expect(page.locator("#dashboard-content")).toBeVisible();
  await expect(page.locator("#connection-label")).toHaveText("Connected");
  await expect(page.getByRole("heading", { name: "Request playground" })).toBeVisible();

  await page.locator("#playground-prompt").fill("Reply with a short greeting.");
  await page.getByRole("button", { name: "Send request" }).click();
  await expect(page.locator("#playground-message")).toHaveText("Request complete.", { timeout: 30_000 });
  await expect(page.locator("#playground-response")).not.toBeEmpty();
  await expect(page.locator("#playground-route")).toBeVisible();
  await page.reload();
  await expect(page.locator("#dashboard-content")).toBeVisible();
  await expect(page.locator("#decisions")).not.toContainText("No recent decisions yet.");

  stdout.write(`\nWalkthrough passed. Browser open at ${baseURL}/dashboard.\n`);
  if (stdin.isTTY) {
    const rl = readline.createInterface({ input: stdin, output: stdout });
    await rl.question("Press Enter to close the browser and stop Spillway. ");
    rl.close();
  } else {
    stdout.write("Close the browser window to stop Spillway.\n");
    await page.waitForEvent("close");
  }
} finally {
  await browser?.close();
  if (server && server.exitCode === null) {
    server.kill();
    await new Promise((resolve) => server.once("exit", resolve));
  }
  await rm(tempDir, { recursive: true, force: true });
}
