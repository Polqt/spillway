import { render } from "preact";
import { useEffect, useRef, useState } from "preact/hooks";
import "./style.css";

const dashboardKeyName = "spillway-dashboard-key";

function Dashboard() {
  const [key, setKey] = useState("");
  const [entry, setEntry] = useState("");
  const [connected, setConnected] = useState(false);
  const [historyAvailable, setHistoryAvailable] = useState(null);
  const [dashboardData, setDashboardData] = useState(null);
  const [message, setMessage] = useState("");
  const [busy, setBusy] = useState(false);
  const activeRequest = useRef(null);
  const requestSequence = useRef(0);

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
    if (response.status === 503) return null;
    if (!response.ok) throw new Error(`Dashboard request failed (${response.status}).`);
    return response.json();
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
      const budget = await request("/api/budget", token, controller.signal);
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
      setHistoryAvailable(history !== null);
      setDashboardData(historyData);
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
    activeRequest.current = null;
    setKey("");
    setConnected(false);
    setBusy(false);
    setHistoryAvailable(null);
    setDashboardData(null);
    setMessage("");
    try {
      sessionStorage.removeItem(dashboardKeyName);
    } catch {
      // The in-memory key is still cleared when storage is unavailable.
    }
  }

  return (
    <div class="shell">
      <header class="topbar">
        <a class="brand" href="/dashboard" aria-label="Spillway dashboard home">
          <span class="brand-mark" aria-hidden="true">S</span>
          <span>Spillway</span>
          <span class="brand-label">LOCAL GATEWAY</span>
        </a>
        <span class="connection" role="status" aria-live="polite">
          <span class={`connection-dot${connected ? " online" : ""}`} aria-hidden="true" />
          {connected ? "Connected" : "Not connected"}
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
              <article class="card next-card">
                <p class="step-label">NEXT · TRY A REQUEST</p>
                <h2>Use Spillway from your app</h2>
                <p>Point an OpenAI-compatible client at <code>http://127.0.0.1:8080/v1</code>. Requests go to the configured provider and may incur cost.</p>
                <pre><code>curl http://127.0.0.1:8080/v1/models \
  -H 'Authorization: Bearer YOUR_LOCAL_KEY'</code></pre>
                <p class="helper">Replace the placeholder in your terminal. Never paste a real key into a shared document.</p>
              </article>

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
