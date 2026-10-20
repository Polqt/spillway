import { render } from "preact";
import { useEffect, useRef, useState } from "preact/hooks";
import "./style.css";

const dashboardKeyName = "spillway-dashboard-key";

function Dashboard() {
  const [key, setKey] = useState("");
  const [entry, setEntry] = useState("");
  const [connected, setConnected] = useState(false);
  const [gateway, setGateway] = useState(null);
  const [historyAvailable, setHistoryAvailable] = useState(null);
  const [dashboardData, setDashboardData] = useState(null);
  const [providerStatus, setProviderStatus] = useState([]);
  const [models, setModels] = useState([]);
  const [prices, setPrices] = useState({});
  const [selectedModel, setSelectedModel] = useState("");
  const [prompt, setPrompt] = useState("");
  const [maxTokens, setMaxTokens] = useState(512);
  const [streaming, setStreaming] = useState(true);
  const [responseText, setResponseText] = useState("");
  const [playgroundRoute, setPlaygroundRoute] = useState(null);
  const [playgroundMessage, setPlaygroundMessage] = useState("");
  const [playgroundError, setPlaygroundError] = useState(false);
  const [playgroundBusy, setPlaygroundBusy] = useState(false);
  const [message, setMessage] = useState("");
  const [busy, setBusy] = useState(false);
  const activeRequest = useRef(null);
  const requestSequence = useRef(0);
  const playgroundRequest = useRef(null);

  useEffect(() => {
    try {
      const saved = sessionStorage.getItem(dashboardKeyName);
      if (saved) void connect(saved);
    } catch {
      setMessage("Session storage is unavailable. You can still connect for this page session.");
    }
  }, []);

  useEffect(() => {
    if (!connected || !key) return undefined;
    const refreshTimer = window.setInterval(() => void connect(key), 10000);
    return () => window.clearInterval(refreshTimer);
  }, [connected, key]);

  async function request(path, token, signal) {
    const response = await fetch(path, {
      headers: { Authorization: `Bearer ${token}` },
      cache: "no-store",
      credentials: "omit",
      signal,
    });
    const body = await response.json().catch(() => ({}));
    if (response.status === 401) {
      const error = new Error("That key was not accepted. Check it and try again.");
      error.status = response.status;
      throw error;
    }
    if (response.status === 403) {
      const error = new Error("This key needs dashboard: true in your Spillway config. Check the server config and reconnect.");
      error.status = response.status;
      throw error;
    }
    if (path.startsWith("/api/usage?") && response.status === 503 && body.error?.code === "store_disabled") return null;
    if (!response.ok) throw new Error(body.error?.message || `Dashboard request failed (${response.status}).`);
    return body;
  }

  async function connect(candidate) {
    const token = candidate.trim();
    if (!token) return;
    const sequence = ++requestSequence.current;
    if (activeRequest.current) activeRequest.current.abort();
    const controller = new AbortController();
    activeRequest.current = controller;
    setBusy(true);
    setMessage("");
    const timeout = window.setTimeout(() => controller.abort(), 8000);
    try {
      const [gatewayResponse, budget, providerResponse, modelResponse, pricingResponse] = await Promise.all([
        request("/api/gateway", token, controller.signal).catch(() => null),
        request("/api/budget", token, controller.signal),
        request("/api/providers", token, controller.signal),
        request("/v1/models", token, controller.signal),
        request("/api/pricing", token, controller.signal),
      ]);
      const now = new Date();
      const todayStart = new Date(Date.UTC(now.getUTCFullYear(), now.getUTCMonth(), now.getUTCDate()));
      const monthStart = new Date(Date.UTC(now.getUTCFullYear(), now.getUTCMonth(), 1));
      const range = (start) => `&since=${encodeURIComponent(start.toISOString())}&until=${encodeURIComponent(now.toISOString())}`;
      const history = await request(`/api/usage?group=day${range(todayStart)}`, token, controller.signal);
      let historyData = null;
      if (history !== null) {
        const [month, providers, models, decisions, quotas] = await Promise.all([
          request(`/api/usage?group=day${range(monthStart)}`, token, controller.signal),
          request(`/api/usage?group=provider${range(monthStart)}`, token, controller.signal),
          request(`/api/usage?group=model${range(monthStart)}`, token, controller.signal),
          request("/api/decisions?limit=20", token, controller.signal),
          request("/api/quotas", token, controller.signal),
        ]);
        historyData = { budget, todaySpend: sumCosts(history.data), monthSpend: sumCosts(month.data), providers: providers.data || [], models: models.data || [], decisions: decisions.data || [], candidates: quotas.candidates || [] };
      }
      if (sequence !== requestSequence.current) return;
      try {
        sessionStorage.setItem(dashboardKeyName, token);
      } catch {
        setMessage("The key is active for this page, but this browser could not save it for the tab.");
      }
      setGateway(gatewayResponse);
      setHistoryAvailable(history !== null);
      setDashboardData(historyData);
      setProviderStatus(providerResponse?.providers || []);
      const availableModels = modelResponse?.data || [];
      setModels(availableModels);
      setPrices(pricingResponse?.prices || {});
      setSelectedModel((current) => availableModels.some((model) => model.id === current)
        ? current
        : availableModels.find((model) => model.id === "auto")?.id || availableModels.find((model) => model.id.startsWith("spillway/"))?.id || availableModels[0]?.id || "");
      setKey(token);
      setConnected(true);
    } catch (error) {
      if (sequence !== requestSequence.current) return;
      if (error.status === 401 || error.status === 403) {
        try {
          sessionStorage.removeItem(dashboardKeyName);
        } catch {
          // The rejected key is not stored in memory.
        }
        setKey("");
        setConnected(false);
        setDashboardData(null);
        setHistoryAvailable(null);
        setProviderStatus([]);
        setGateway(null);
      }
      setMessage(error.name === "AbortError" ? "The dashboard request timed out. Check that Spillway is running and try again." : error.message);
    } finally {
      window.clearTimeout(timeout);
      if (activeRequest.current === controller) activeRequest.current = null;
      if (sequence === requestSequence.current) setBusy(false);
    }
  }

  function forget() {
    requestSequence.current++;
    if (activeRequest.current) activeRequest.current.abort();
    if (playgroundRequest.current) playgroundRequest.current.abort();
    playgroundRequest.current = null;
    setPlaygroundBusy(false);
    activeRequest.current = null;
    setKey("");
    setConnected(false);
    setGateway(null);
    setBusy(false);
    setHistoryAvailable(null);
    setDashboardData(null);
    setProviderStatus([]);
    setModels([]);
    setPrices({});
    setPrompt("");
    setResponseText("");
    setPlaygroundRoute(null);
    setPlaygroundMessage("");
    setPlaygroundError(false);
    setMessage("");
    try {
      sessionStorage.removeItem(dashboardKeyName);
    } catch {
      // The in-memory key is still cleared when storage is unavailable.
    }
  }

  async function submitPlayground(event) {
    event.preventDefault();
    if (!key || !selectedModel || !prompt.trim() || playgroundBusy) return;
    const controller = new AbortController();
    playgroundRequest.current = controller;
    setPlaygroundBusy(true);
    setResponseText("");
    setPlaygroundRoute(null);
    setPlaygroundMessage("Sending request…");
    setPlaygroundError(false);
    try {
      const response = await fetch("/v1/chat/completions", {
        method: "POST",
        headers: { Authorization: `Bearer ${key}`, "Content-Type": "application/json" },
        cache: "no-store",
        credentials: "omit",
        signal: controller.signal,
        body: JSON.stringify({ model: selectedModel, messages: [{ role: "user", content: prompt }], stream: streaming, max_tokens: Number(maxTokens) }),
      });
      setPlaygroundRoute({
        provider: response.headers.get("x-spillway-provider"),
        model: response.headers.get("x-spillway-model"),
        route: response.headers.get("x-spillway-route"),
        cache: response.headers.get("x-spillway-cache"),
        status: response.status,
      });
      if (!response.ok) {
        const body = await response.json().catch(() => ({}));
        throw new Error(body.error?.message || `Request failed (${response.status}).`);
      }
      if (streaming) {
        await readCompletionStream(response, (text) => setResponseText((current) => current + text));
      } else {
        const body = await response.json();
        setResponseText(body.choices?.[0]?.message?.content ?? "");
      }
      setPlaygroundMessage("Request complete.");
    } catch (error) {
      if (error.name === "AbortError") {
        setPlaygroundMessage("Request cancelled.");
      } else {
        setPlaygroundMessage(error.message || "The request failed.");
        setPlaygroundError(true);
      }
    } finally {
      if (playgroundRequest.current === controller) playgroundRequest.current = null;
      setPlaygroundBusy(false);
    }
  }

  function cancelPlayground() {
    playgroundRequest.current?.abort();
  }

  return (
    <div class="shell">
      <header class="topbar">
        <a class="brand" href="/dashboard">
          <span class="brand-mark" aria-hidden="true">S</span>
          <span>Spillway</span>
          <span class="brand-label">LOCAL GATEWAY</span>
        </a>
        <span class="connection" role="status" aria-live="polite">
          <span class={`connection-dot${connected ? " online" : ""}`} aria-hidden="true" />
          {connected ? gateway?.version ? `Connected to Spillway ${gateway.version}` : "Connected" : "Not connected"}
        </span>
      </header>

      <main class="page">
        <section class="intro" aria-labelledby="welcome-title">
          <p class="eyebrow">YOUR LOCAL GATEWAY</p>
          <h1 id="welcome-title">Welcome to Spillway.</h1>
          <p class="intro-copy">Connect your dashboard to see what is configured and choose a useful next step.</p>
        </section>

        {!connected ? (
          <section class="card access-card" aria-labelledby="access-title">
            <div>
              <p class="step-label">STEP 1 · DASHBOARD ACCESS</p>
              <h2 id="access-title">Connect with a dashboard key</h2>
              <p>Use a local key whose config entry has <code>dashboard: true</code>. The key is sent only to this Spillway instance and kept in this tab's session storage.</p>
            </div>
            <form onSubmit={(event) => { event.preventDefault(); void connect(entry); setEntry(""); }}>
              <label for="dashboard-key">Dashboard key</label>
              <div class="key-row">
                <input id="dashboard-key" type="password" autoComplete="current-password" value={entry} onInput={(event) => setEntry(event.currentTarget.value)} required />
                <button type="submit" disabled={busy}>{busy ? "Connecting…" : "Connect"}</button>
              </div>
            </form>
            {message && <p class="notice" role="alert">{message}</p>}
            <p class="privacy-note">Spillway does not send telemetry. Your key is never placed in a URL or saved as a cookie.</p>
          </section>
        ) : (
          <>
            <section class="card welcome-card" aria-labelledby="ready-title">
              <div class="ready-mark" aria-hidden="true">✓</div>
              <div>
                <p class="step-label">CONNECTED</p>
                <h2 id="ready-title">Your dashboard is ready.</h2>
                <p>Spillway runs locally. You choose when to send requests to configured providers.</p>
              </div>
            </section>

            <section class="next-grid" aria-label="Next steps">
              <ClientExamples />

              <article class="card next-card" aria-labelledby="history-title">
                <p class="step-label">OPTIONAL · USAGE HISTORY</p>
                <h2 id="history-title">{historyAvailable ? "History storage is enabled" : "Choose whether to keep history"}</h2>
                {historyAvailable ? (
                  <p>Request metadata is available in this dashboard. Prompts, responses and keys are not stored in usage history.</p>
                ) : (
                  <p>Onboarding and local requests work without <code>store:</code>. Add <code>store:</code> to your config only if you want local usage and routing history to persist.</p>
                )}
                <p class="helper">History contains request metadata such as model, route, token counts and estimated cost.</p>
              </article>
            </section>
            <ProviderStatus providers={providerStatus} />
            <RequestPlayground
              models={models} prices={prices} model={selectedModel} setModel={setSelectedModel}
              prompt={prompt} setPrompt={setPrompt} maxTokens={maxTokens} setMaxTokens={setMaxTokens}
              streaming={streaming} setStreaming={setStreaming} response={responseText}
              route={playgroundRoute}
              message={playgroundMessage} busy={playgroundBusy} onSubmit={submitPlayground} onCancel={cancelPlayground}
              error={playgroundError}
            />
            {dashboardData && <History data={dashboardData} />}
            {message && <p class="notice" role="status">{message}</p>}
            <div class="actions"><button class="secondary" type="button" onClick={forget}>Forget dashboard key</button></div>
          </>
        )}
      </main>

      <footer class="footer"><span>Spillway runs on your machine.</span><span>Private by default · No telemetry</span></footer>
    </div>
  );
}

async function readCompletionStream(response, append) {
  if (!response.body) throw new Error("The response stream is unavailable.");
  const reader = response.body.getReader();
  const decoder = new TextDecoder();
  let buffer = "";
  let complete = false;
  let receivedDone = false;
  try {
    while (true) {
      const { value, done } = await reader.read();
      buffer += decoder.decode(value || new Uint8Array(), { stream: !done });
      let boundary;
      while ((boundary = buffer.search(/\r?\n\r?\n/)) !== -1) {
        const frame = buffer.slice(0, boundary);
        buffer = buffer.slice(boundary).replace(/^\r?\n\r?\n/, "");
        const data = frame.split(/\r?\n/).filter((line) => line.startsWith("data:")).map((line) => line.slice(5).trimStart()).join("\n");
        if (!data) continue;
        if (data === "[DONE]") {
          receivedDone = true;
          continue;
        }
        const event = JSON.parse(data);
        if (event.error) throw new Error(event.error.message || "The provider returned a stream error.");
        const content = event.choices?.[0]?.delta?.content;
        if (typeof content === "string") append(content);
      }
      if (done) {
        complete = true;
        break;
      }
    }
    if (!receivedDone) throw new Error("The response stream ended before completion.");
  } finally {
    if (!complete) await reader.cancel().catch(() => {});
    reader.releaseLock();
  }
}

function clientExamples(origin) {
  const endpoint = `${origin}/v1`;
  return [
    { name: "curl (POSIX)", code: `curl ${endpoint}/chat/completions \\\n  -H 'Authorization: Bearer <LOCAL_APP_KEY>' \\\n  -H 'Content-Type: application/json' \\\n  -d '{"model":"auto","messages":[{"role":"user","content":"Hello from Spillway"}]}'` },
    { name: "curl (PowerShell)", code: `curl.exe ${endpoint}/chat/completions \`\n  -H 'Authorization: Bearer <LOCAL_APP_KEY>' \`\n  -H 'Content-Type: application/json' \`\n  -d '{"model":"auto","messages":[{"role":"user","content":"Hello from Spillway"}]}'` },
    { name: "Python", code: `from openai import OpenAI\n\nclient = OpenAI(base_url="${endpoint}", api_key="<LOCAL_APP_KEY>")\nresponse = client.chat.completions.create(\n    model="auto",\n    messages=[{"role": "user", "content": "Hello from Spillway"}],\n)\nprint(response.choices[0].message.content)` },
    { name: "Node.js", code: `import OpenAI from "openai";\n\nconst client = new OpenAI({ baseURL: "${endpoint}", apiKey: "<LOCAL_APP_KEY>" });\nconst response = await client.chat.completions.create({\n  model: "auto",\n  messages: [{ role: "user", content: "Hello from Spillway" }],\n});\nconsole.log(response.choices[0].message.content);` },
    { name: "Environment", code: `# Client requests use model auto.\nOPENAI_BASE_URL=${endpoint}\nOPENAI_API_KEY=<LOCAL_APP_KEY>` },
  ];
}

function ClientExamples() {
  const examples = clientExamples(window.location.origin);
  const [selected, setSelected] = useState(0);
  const [copyMessage, setCopyMessage] = useState("");
  const current = examples[selected];

  async function copyExample() {
    try {
      await navigator.clipboard.writeText(current.code);
      setCopyMessage(`Copied ${current.name} example.`);
    } catch {
      setCopyMessage("Could not copy the example. Select and copy the text instead.");
    }
  }

  function handleTabKey(event) {
    if (event.key !== "ArrowRight" && event.key !== "ArrowLeft") return;
    event.preventDefault();
    const next = (selected + (event.key === "ArrowRight" ? 1 : examples.length - 1)) % examples.length;
    setSelected(next);
    event.currentTarget.parentElement.children[next].focus();
  }

  return (
    <section class="card next-card client-examples" aria-labelledby="client-examples-title">
      <h2 id="client-examples-title">Connect a client app</h2>
      <p>Point an OpenAI-compatible client at this local endpoint with the <code>default</code> key from your config, not the dashboard key. Replace the placeholder when you run the example; requests to configured providers may incur cost.</p>
      <div class="client-tabs" role="tablist" aria-label="Client examples">
        {examples.map((example, index) => (
          <button id={`client-tab-${index}`} class="client-tab" type="button" role="tab" aria-selected={selected === index} aria-controls="client-example-panel" tabIndex={selected === index ? 0 : -1} onClick={() => { setSelected(index); setCopyMessage(""); }} onKeyDown={handleTabKey} key={example.name}>{example.name}</button>
        ))}
      </div>
      <div id="client-example-panel" role="tabpanel" aria-labelledby={`client-tab-${selected}`}>
        <pre tabIndex="0"><code>{current.code}</code></pre>
        <button class="secondary" type="button" onClick={copyExample}>Copy example</button>
      </div>
      <p class="copy-status" role="status" aria-live="polite">{copyMessage}</p>
    </section>
  );
}

function RequestPlayground({ models, prices, model, setModel, prompt, setPrompt, maxTokens, setMaxTokens, streaming, setStreaming, response, route, message, busy, onSubmit, onCancel, error }) {
  const [copyMessage, setCopyMessage] = useState("");
  const price = prices[model];
  const estimatedTokens = Math.ceil(Array.from(prompt).length / 4);
  const estimateUSD = price ? (estimatedTokens * price.input_micro_usd_per_million + Number(maxTokens) * price.output_micro_usd_per_million) / 1_000_000_000_000 : null;
  const failures = error ? exhaustionCandidates(message) : [];
  async function copyCurl() {
    const body = JSON.stringify({ model, messages: [{ role: "user", content: prompt }], stream: streaming, max_tokens: Number(maxTokens) });
    const windows = /Win/i.test(navigator.userAgentData?.platform || navigator.platform);
    const quote = windows ? (value) => `'${value.replaceAll("'", "''")}'` : (value) => `'${value.replaceAll("'", "'\\''")}'`;
    const curl = windows ? "curl.exe" : "curl";
    const command = `${curl} -sS ${quote(`${window.location.origin}/v1/chat/completions`)} -H ${quote("Authorization: Bearer <LOCAL_APP_KEY>")} -H ${quote("Content-Type: application/json")} --data ${quote(body)}`;
    try {
      await navigator.clipboard.writeText(command);
      setCopyMessage(windows ? "Copied PowerShell curl command." : "Copied curl command.");
    } catch {
      setCopyMessage("Could not copy the curl command. Check clipboard permissions and try again.");
    }
  }
  return (
    <section class="request-playground" aria-labelledby="playground-heading">
      <div class="history-title"><div><p class="step-label">EXPLICIT PROVIDER REQUEST</p><h2 id="playground-heading">Request playground</h2></div></div>
      <p class="helper">Your prompt will be sent to the configured provider for the selected model. Provider charges may apply.</p>
      <form class="playground-form" onSubmit={onSubmit}>
        <label for="playground-model">Model</label>
        <select id="playground-model" value={model} onChange={(event) => setModel(event.currentTarget.value)} required disabled={busy || models.length === 0}>
          {models.map((item) => <option value={item.id} key={item.id}>{item.id}</option>)}
        </select>
        <p class="helper">Direct model references skip spillover. Choose auto or spillway/&lt;tier&gt; to let Spillway try eligible candidates in order.</p>
        <label for="playground-prompt">Prompt</label>
        <textarea id="playground-prompt" value={prompt} onInput={(event) => setPrompt(event.currentTarget.value)} rows="5" required disabled={busy} />
        <div class="playground-options">
          <label class="stream-option"><input type="checkbox" checked={streaming} onChange={(event) => setStreaming(event.currentTarget.checked)} disabled={busy} /> Stream response</label>
          <label for="playground-max-tokens">Max response tokens</label>
          <input id="playground-max-tokens" type="number" min="1" max="32768" value={maxTokens} onInput={(event) => setMaxTokens(event.currentTarget.value)} disabled={busy} />
        </div>
        {estimateUSD !== null && <p class="estimate-note" aria-live="polite">Estimated cost: about ${estimateUSD.toFixed(6)}. Based on a rough prompt token estimate and the selected output limit; actual cost may differ.</p>}
        <div class="playground-actions">
          <button type="submit" disabled={busy || !models.length || !prompt.trim()}>{busy ? "Requesting…" : "Send request"}</button>
          <button class="secondary" type="button" onClick={copyCurl} disabled={!model}>Copy as curl</button>
          {busy && <button class="secondary" type="button" onClick={onCancel}>Cancel request</button>}
        </div>
        <p class="copy-status" role="status" aria-live="polite">{copyMessage}</p>
      </form>
      {!models.length && <p class="empty">No configured models are available.</p>}
      {message && <p class="playground-message" role={error ? "alert" : "status"} aria-live={error ? "assertive" : "polite"}>{message}</p>}
      {failures.length > 0 && <ul aria-label="Exhausted candidates">{failures.map((failure) => <li key={failure.model}><strong>{failure.model}</strong>: {failure.reason}</li>)}</ul>}
      {route && <div class="playground-route" aria-label="Routing explanation">
        <h3>Routing explanation</h3>
        <p><strong>Provider and model</strong><span>{[route.provider, route.model].filter(Boolean).join("/") || "Not reported"}</span></p>
        <p><strong>Route reason</strong><span>{route.route || "Not reported"}</span></p>
        <p><strong>What happened</strong><span>{explainRoute(route.route, route.cache, route.status)}</span></p>
        <p><strong>Cache</strong><span>{route.cache || "Not reported"}</span></p>
        <p><strong>HTTP status</strong><span>{route.status}</span></p>
        <p class="route-terms">Definitions: <a href="#cooldown-definition">Cooldown</a> · <a href="#circuit-breaker-definition">Circuit breaker</a> · <a href="#exhaustion-definition">Exhaustion</a></p>
      </div>}
      <details class="route-glossary"><summary>Routing term definitions</summary><dl>
        <dt id="cooldown-definition">Cooldown</dt><dd>A period during which a candidate is skipped after a rate limit or another temporary failure.</dd>
        <dt id="circuit-breaker-definition">Circuit breaker</dt><dd>A temporary stop on a candidate after repeated upstream failures.</dd>
        <dt id="exhaustion-definition">Exhaustion</dt><dd>The state where every candidate in a tier is unavailable or failed for a request.</dd>
      </dl></details>
      <div class="response-panel" aria-labelledby="response-heading"><h3 id="response-heading">Response</h3><pre aria-label="Assistant response" aria-live="polite">{response || "The response will appear here."}</pre></div>
    </section>
  );
}

function exhaustionCandidates(message) {
  const marker = " can serve this request: ";
  const start = message.indexOf(marker);
  if (!message.startsWith("No candidate in tier ") || start === -1) return [];
  return message.slice(start + marker.length).split("; ").flatMap((entry) => {
    const separator = entry.indexOf(" ");
    return separator > 0 ? [{ model: entry.slice(0, separator), reason: entry.slice(separator + 1) }] : [];
  });
}

function ProviderStatus({ providers }) {
  const [refreshedAt, setRefreshedAt] = useState(new Date());
  useEffect(() => setRefreshedAt(new Date()), [providers]);
  return (
    <section class="provider-status" aria-labelledby="provider-status-heading">
      <div class="history-title"><div><p class="step-label">LOCAL CONFIGURATION</p><h2 id="provider-status-heading">Provider status</h2></div></div>
      <p class="helper"><strong>Configured:</strong> provider and credential state from local config.</p>
      <p class="helper"><strong>Observed since this server started:</strong> request signals; these do not indicate current reachability. Spillway does not probe providers.</p>
      <p class="provider-refresh">Last refreshed: {refreshedAt.toLocaleTimeString()}</p>
      {providers.length ? (
        <div class="provider-status-grid">{providers.map((provider) => (
          <article class="card provider-status-card" key={provider.name}>
            <div class="provider-status-heading"><strong>{provider.name}</strong><span class="estimate-label">Configured</span></div>
            <p>{provider.credential_required ? provider.credential_present ? "Credential configured" : "Credential missing" : "No credential required"}</p>
            {Array.isArray(provider.signals) && provider.signals.length ? (
              <ul class="provider-signals">{provider.signals.map((signal) => {
                const failures = Number(signal.upstream_failures || 0);
                const count = failures ? `${failures === 7 ? "At least " : ""}${failures} upstream failure${failures === 1 ? "" : "s"} observed. ` : "";
                const reason = signal.reason ? statusReason(signal.reason) : "No active cooldown";
                const breaker = signal.breaker_state === "open" ? " · Circuit breaker open" : "";
                const until = signal.cooldown_until ? ` · ${cooldownTime(signal.cooldown_until)}` : "";
                return <li key={signal.model}><strong>{signal.model}</strong><span>{count}{reason}{breaker}{until}</span></li>;
              })}</ul>
            ) : <p class="empty">No active cooldown or failure signal observed.</p>}
          </article>
        ))}</div>
      ) : <p class="empty">No providers are configured yet.</p>}
    </section>
  );
}

function History({ data }) {
  const activeCooldowns = data.candidates.filter((candidate) => candidate.cooldown_until && Date.parse(candidate.cooldown_until) > Date.now());
  return (
    <section class="history" aria-labelledby="history-heading">
      <div class="history-title"><div><p class="step-label">LOCAL REQUEST METADATA</p><h2 id="history-heading">Usage and routing history</h2></div><span class="estimate-label">Cost estimates</span></div>
      <div class="metrics">
        <Metric label="Estimated spend today" value={dollars(data.budget.daily?.spent_micro_usd ?? data.todaySpend)} detail={data.budget.daily ? `of ${dollars(data.budget.daily.limit_micro_usd)} daily budget` : "No daily budget configured"} />
        <Metric label="Estimated spend this month" value={dollars(data.budget.monthly?.spent_micro_usd ?? data.monthSpend)} detail={data.budget.monthly ? `of ${dollars(data.budget.monthly.limit_micro_usd)} monthly budget` : "No monthly budget configured"} />
        <Metric label="Requests this month" value={data.models.reduce((sum, row) => sum + Number(row.requests || 0), 0)} />
        <Metric label="Active cooldowns" value={activeCooldowns.length} />
      </div>
      <div class="history-grid">
        <UsageTable title="Usage by provider" rows={data.providers} />
        <UsageTable title="Usage by model" rows={data.models} />
      </div>
      <section class="card history-panel" aria-labelledby="decisions-heading">
        <h3 id="decisions-heading">Recent routing decisions</h3>
        {data.decisions.length ? (
          <ul class="decision-list">{data.decisions.map((decision, index) => (
            <li key={`${decision.at}-${index}`}>
              <div><strong>{decision.model || decision.requested_model || "Model unavailable"}</strong><span>{decision.provider || "No provider recorded"} · {decision.status || "unknown status"}</span></div>
              <p>{decision.route || "No route reason recorded"}</p>
              <time dateTime={decision.at}>{decision.at ? new Date(decision.at).toLocaleString() : ""}</time>
            </li>
          ))}</ul>
        ) : <p class="empty">No requests have been recorded yet.</p>}
      </section>
      <section class="card history-panel" aria-labelledby="cooldowns-heading">
        <h3 id="cooldowns-heading">Observed cooldowns</h3>
        {activeCooldowns.length ? (
          <ul class="cooldown-list">{activeCooldowns.map((candidate) => (
            <li key={`${candidate.provider}/${candidate.model}`}>
              <strong>{candidate.provider}/{candidate.model}</strong>
              <span>{candidate.reason || "Temporarily unavailable"} · breaker {candidate.breaker_state || "closed"}</span>
              <time dateTime={candidate.cooldown_until}>Until {new Date(candidate.cooldown_until).toLocaleTimeString()}</time>
            </li>
          ))}</ul>
        ) : <p class="empty">No active cooldowns.</p>}
      </section>
      <p class="helper">Spend is estimated from provider usage or configured prices. Actual billing may differ.</p>
    </section>
  );
}

function explainRoute(route, cache, status) {
  if (cache === "hit") return "Spillway served the saved exact-cache response.";
  if (!route) return "No route reason was recorded.";
  const parts = route.split(";").map((part) => part.trim());
  const candidate = parts.find((part) => /^candidate \d+\/\d+$/.test(part));
  const candidateMatch = candidate?.match(/^candidate (\d+)\/(\d+)$/);
  const validPart = (part) => /^(direct|auto|rules|classifier|classifier fallback|default|rule \d+|difficulty [a-z]+|tier [a-z0-9-]+|fallback [a-z0-9-]+|needs (tools|vision|json_mode|structured_outputs)(, (tools|vision|json_mode|structured_outputs))*|candidate \d+\/\d+|exhausted \(\d+ eligible\)|no capable model)$/.test(part);
  if (parts.some((part) => !validPart(part))) return "No plain-language explanation is available; the route reason above is shown verbatim.";
  const tier = parts.find((part) => part.startsWith("tier "))?.slice(5);
  const selection = candidateMatch
    ? Number(candidateMatch[1]) > 1
      ? status < 400
        ? `Candidate ${candidateMatch[1]} of ${candidateMatch[2]} answered. Earlier candidates were skipped or failed.`
        : `Spillway tried candidate ${candidateMatch[1]} of ${candidateMatch[2]}; the request failed.`
      : status < 400
        ? `Candidate ${candidateMatch[1]} of ${candidateMatch[2]} answered.`
        : `Spillway tried candidate ${candidateMatch[1]} of ${candidateMatch[2]}; the request failed.`
    : "";
  if (parts[0] === "direct") return status >= 400 ? "Spillway handled this as a direct-model request, but it failed before a successful response." : "Spillway sent the requested model directly to its provider.";
  if (parts.some((part) => part.startsWith("exhausted ("))) return "Spillway had no eligible candidate that could serve this request.";
  if (parts.includes("no capable model")) return "Spillway could not resolve a usable model for this request.";
  const fallbackTier = parts.find((part) => part.startsWith("fallback "))?.slice(9);
  const difficulty = parts.find((part) => part.startsWith("difficulty "))?.slice(11) || "configured";
  if (parts[0] === "rules") {
    const lead = parts.includes("classifier fallback")
      ? "The local classifier could not choose a route, so deterministic routing used "
      : parts.includes("default")
        ? "No routing rule matched, so Spillway used the default "
        : "A routing rule selected the ";
    return `${lead}${difficulty} difficulty${tier ? ` in the ${tier} tier` : ""}${fallbackTier ? `, then spilled over to the ${fallbackTier} fallback tier` : ""}.${selection ? ` ${selection}` : ""}`;
  }
  if (parts[0] === "classifier") return `${parts.includes("classifier fallback") ? "Classifier fallback" : "The local classifier"} selected the ${difficulty} difficulty${tier ? ` in the ${tier} tier` : ""}${fallbackTier ? `, then spilled over to the ${fallbackTier} fallback tier` : ""}.${selection ? ` ${selection}` : ""}`;
  if (parts[0].startsWith("tier ")) return `Spillway used the requested ${parts[0].slice(5)} tier.${selection ? ` ${selection}` : ""}`;
  if (parts[0] === "auto") return `Spillway selected a candidate from the configured auto route${tier ? ` in the ${tier} tier` : ""}.${selection ? ` ${selection}` : ""}`;
  return "Spillway used the configured route shown above.";
}

function statusReason(reason) {
  return ({ rate_limit: "Rate limit", "rate limited": "Rate limit", unavailable: "Unavailable", "upstream error": "Upstream error" })[reason] || reason;
}

function cooldownTime(value) {
  const date = new Date(value);
  const seconds = Math.ceil((date.getTime() - Date.now()) / 1000);
  const minutes = seconds > 0 ? Math.ceil(seconds / 60) : Math.floor(-seconds / 60);
  const relative = seconds > 0
    ? seconds < 60 ? `in ${seconds} seconds` : `in ${minutes} minute${minutes === 1 ? "" : "s"}`
    : seconds > -60 ? "ended less than a minute ago" : `ended ${minutes} minute${minutes === 1 ? "" : "s"} ago`;
  return `Until ${date.toLocaleTimeString()} (${relative})`;
}

function Metric({ label, value, detail }) {
  return <article class="card metric"><span>{label}</span><strong>{value}</strong>{detail && <small>{detail}</small>}</article>;
}

function UsageTable({ title, rows }) {
  return (
    <section class="card history-panel" aria-label={title}>
      <h3>{title}</h3>
      {rows.length ? (
        <ul class="usage-list">{rows.map((row) => (
          <li key={row.key}><span>{row.key}</span><span>{row.requests} requests</span><strong>{dollars(row.cost_micro_usd)}</strong></li>
        ))}</ul>
      ) : <p class="empty">No usage has been recorded yet.</p>}
    </section>
  );
}

function dollars(microUSD) {
  return `est. $${(Number(microUSD || 0) / 1_000_000).toFixed(2)}`;
}

function sumCosts(rows = []) {
  return rows.reduce((sum, row) => sum + Number(row.cost_micro_usd || 0), 0);
}

render(<Dashboard />, document.getElementById("app"));
