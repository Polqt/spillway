(() => {
  "use strict";

  const keyForm = document.querySelector("#key-form");
  const keyInput = document.querySelector("#dashboard-key");
  const accessPanel = document.querySelector("#access-panel");
  const content = document.querySelector("#dashboard-content");
  const message = document.querySelector("#access-message");
  const disconnect = document.querySelector("#disconnect");
  const keyName = "spillway-dashboard-key";
  let key = "";
  let refreshTimer = 0;
  let refreshController = null;
  let session = 0;

  function setText(selector, value) {
    const element = document.querySelector(selector);
    if (element) element.textContent = String(value);
  }

  function setConnection(connected) {
    document.querySelector("#connection-dot").classList.toggle("online", connected);
    setText("#connection-label", connected ? "Connected" : "Not connected");
  }

  function dollars(microUSD) {
    const value = Number(microUSD || 0) / 1000000;
    return `est. $${value.toFixed(2)}`;
  }

  function widthClass(percent) {
    return `bar-${Math.min(100, Math.max(0, Math.ceil(percent / 10) * 10))}`;
  }

  function dateRange(start, end) {
    return `since=${encodeURIComponent(start.toISOString())}&until=${encodeURIComponent(end.toISOString())}`;
  }

  async function request(path, signal) {
    const response = await fetch(path, {
      headers: { Authorization: `Bearer ${key}` },
      credentials: "omit",
      cache: "no-store",
      signal
    });
    const body = await response.json().catch(() => ({}));
    if (!response.ok) {
      const error = new Error(body.error?.message || `Dashboard request failed (${response.status}).`);
      error.status = response.status;
      throw error;
    }
    return body;
  }

  function setBudget(period, status, spend) {
    const value = status ? Number(status.spent_micro_usd || 0) : spend;
    const limit = status ? Number(status.limit_micro_usd || 0) : 0;
    setText(`#${period}-spend`, dollars(value));
    const meter = document.querySelector(`#${period}-meter`);
    const percent = limit > 0 ? Math.min(100, Math.max(0, (value / limit) * 100)) : 0;
    meter.className = widthClass(percent);
    meter.classList.toggle("over", limit > 0 && value >= limit);
    if (status) {
      setText(`#${period}-budget`, `of est. $${(limit / 1000000).toFixed(2)} budget · resets ${new Date(status.reset_at).toLocaleString()}`);
    } else {
      setText(`#${period}-budget`, `est. spend · no ${period === "today" ? "daily" : "monthly"} budget configured`);
    }
  }

  function renderTotals(selector, rows) {
    const list = document.querySelector(selector);
    list.replaceChildren();
    if (!rows.length) {
      const empty = document.createElement("p");
      empty.className = "empty";
      empty.textContent = "No usage in this period.";
      list.append(empty);
      return;
    }
    const max = Math.max(1, ...rows.map((row) => Number(row.cost_micro_usd || 0)));
    for (const row of rows) {
      const item = document.createElement("div");
      item.className = "total-row";
      const name = document.createElement("span");
      name.className = "total-name";
      name.textContent = row.key || "unknown";
      const cost = document.createElement("span");
      cost.className = "total-cost";
      cost.textContent = `est. $${Number(row.cost_usd || 0).toFixed(2)}`;
      const meta = document.createElement("span");
      meta.className = "total-meta";
      const bar = document.createElement("span");
      bar.className = `bar-fill ${widthClass(Math.max(2, (Number(row.cost_micro_usd || 0) / max) * 100))}`;
      meta.append(bar);
      item.append(name, cost, meta);
      list.append(item);
    }
  }

  function renderDecisions(rows) {
    const list = document.querySelector("#decisions");
    list.replaceChildren();
    setText("#decision-count", `${rows.length} recent ${rows.length === 1 ? "request" : "requests"}`);
    if (!rows.length) {
      const empty = document.createElement("p");
      empty.className = "empty";
      empty.textContent = "No recent decisions yet.";
      list.append(empty);
      return;
    }
    for (const row of rows) {
      const item = document.createElement("article");
      item.className = "decision";
      const identity = document.createElement("div");
      const model = document.createElement("div");
      model.className = "decision-model";
      model.textContent = row.model ? `${row.provider}/${row.model}` : row.requested_model || "No model selected";
      const detail = document.createElement("div");
      detail.className = "decision-detail";
      detail.textContent = `${row.client_app || "unknown app"} · ${row.status} · ${row.latency_ms || 0} ms · ${dollars(row.cost_micro_usd)}`;
      identity.append(model, detail);
      const route = document.createElement("div");
      route.className = "decision-route";
      route.textContent = row.route || "No route reason recorded";
      const status = document.createElement("span");
      status.className = `status-pill${Number(row.status) >= 400 ? " error" : ""}`;
      status.textContent = Number(row.status) >= 400 ? "Error" : row.cache === "hit" ? "Cache hit" : "Served";
      const time = document.createElement("time");
      time.className = "decision-time";
      time.dateTime = row.at || "";
      time.textContent = row.at ? new Date(row.at).toLocaleString() : "";
      const routeWrap = document.createElement("div");
      routeWrap.append(route);
      if (Array.isArray(row.skips) && row.skips.length) {
        const skips = document.createElement("div");
        skips.className = "decision-skips";
        skips.textContent = `Skipped: ${row.skips.map((skip) => `${skip.provider}/${skip.model} (${skip.reason})`).join(", ")}`;
        routeWrap.append(skips);
      }
      const timing = document.createElement("div");
      timing.append(status, time);
      item.append(identity, routeWrap, timing);
      list.append(item);
    }
  }

  function renderCooldowns(candidates) {
    const active = candidates.filter((candidate) => candidate.cooldown_until && new Date(candidate.cooldown_until).getTime() > Date.now());
    const list = document.querySelector("#cooldowns");
    list.replaceChildren();
    setText("#cooldown-count", active.length ? `${active.length} cooling down` : "All candidates ready");
    if (!active.length) {
      const empty = document.createElement("p");
      empty.className = "empty";
      empty.textContent = "No active cooldowns.";
      list.append(empty);
      return;
    }
    for (const candidate of active) {
      const item = document.createElement("div");
      item.className = "cooldown";
      const model = document.createElement("div");
      model.className = "decision-model";
      model.textContent = `${candidate.provider}/${candidate.model}`;
      const reason = document.createElement("div");
      reason.className = "cooldown-reason";
      reason.textContent = `${candidate.reason || "Unavailable"} · breaker ${candidate.breaker_state || "closed"}`;
      const until = document.createElement("time");
      until.className = "cooldown-until";
      until.dateTime = candidate.cooldown_until;
      until.textContent = `Until ${new Date(candidate.cooldown_until).toLocaleTimeString()}`;
      item.append(model, reason, until);
      list.append(item);
    }
  }

  async function refresh(currentSession) {
    const controller = new AbortController();
    refreshController = controller;
    const timeout = window.setTimeout(() => controller.abort(), 8000);
    const now = new Date();
    const todayStart = new Date(Date.UTC(now.getUTCFullYear(), now.getUTCMonth(), now.getUTCDate()));
    const monthStart = new Date(Date.UTC(now.getUTCFullYear(), now.getUTCMonth(), 1));
    try {
      const signal = controller.signal;
      const [budget, today, month, providers, models, decisions, quotas] = await Promise.all([
        request("/api/budget", signal),
        request(`/api/usage?${dateRange(todayStart, now)}&group=day`, signal),
        request(`/api/usage?${dateRange(monthStart, now)}&group=day`, signal),
        request(`/api/usage?${dateRange(monthStart, now)}&group=provider`, signal),
        request(`/api/usage?${dateRange(monthStart, now)}&group=model`, signal),
        request("/api/decisions?limit=20", signal),
        request("/api/quotas", signal)
      ]);
      if (currentSession !== session) return;
      const todaySpend = (today.data || []).reduce((sum, row) => sum + Number(row.cost_micro_usd || 0), 0);
      const monthSpend = (month.data || []).reduce((sum, row) => sum + Number(row.cost_micro_usd || 0), 0);
      setBudget("today", budget.daily, todaySpend);
      setBudget("month", budget.monthly, monthSpend);
      renderTotals("#provider-totals", providers.data || []);
      renderTotals("#model-totals", models.data || []);
      renderDecisions(decisions.data || []);
      renderCooldowns(quotas.candidates || []);
      const quality = budget.includes_estimated_costs ? " · includes estimated usage" : "";
      const unknown = budget.includes_unknown_costs ? " · some costs unknown" : "";
      setText("#updated-at", `Updated ${new Date().toLocaleTimeString()}${quality}${unknown}`);
      message.textContent = "";
      setConnection(true);
    } finally {
      window.clearTimeout(timeout);
      if (refreshController === controller) refreshController = null;
    }
  }

  function forgetKey() {
    session++;
    clearInterval(refreshTimer);
    if (refreshController) refreshController.abort();
    refreshController = null;
    key = "";
    try { sessionStorage.removeItem(keyName); } catch (_error) { /* Storage may be disabled. */ }
    content.hidden = true;
    accessPanel.hidden = false;
    disconnect.hidden = true;
    setConnection(false);
  }

  function showRefreshError(error) {
    if (error.status === 403) {
      forgetKey();
      message.textContent = "This key cannot read the dashboard. Use a local key marked dashboard: true in your Spillway config.";
      return;
    }
    if (error.status === 401) {
      forgetKey();
      message.textContent = "That key was not accepted. Check it and try again.";
      return;
    }
    setConnection(false);
    message.textContent = error.message;
  }

  function connect(candidate) {
    const currentSession = ++session;
    if (refreshController) refreshController.abort();
    refreshController = null;
    key = candidate;
    try {
      sessionStorage.setItem(keyName, key);
    } catch (_error) {
      message.textContent = "This browser could not keep the key for this tab. Check its storage settings and try again.";
      key = "";
      return;
    }
    accessPanel.hidden = true;
    content.hidden = false;
    disconnect.hidden = false;
    clearInterval(refreshTimer);
    refresh(currentSession).catch((error) => {
      if (currentSession === session) showRefreshError(error);
    });
    refreshTimer = window.setInterval(() => {
      if (!refreshController && currentSession === session) {
        refresh(currentSession).catch((error) => {
          if (currentSession === session) showRefreshError(error);
        });
      }
    }, 10000);
  }

  keyForm.addEventListener("submit", (event) => {
    event.preventDefault();
    message.textContent = "";
    connect(keyInput.value.trim());
    keyInput.value = "";
  });

  disconnect.addEventListener("click", () => {
    forgetKey();
    setText("#updated-at", "Waiting for a dashboard key");
  });

  try {
    const savedKey = sessionStorage.getItem(keyName);
    if (savedKey) connect(savedKey);
  } catch (_error) {
    // The user can still enter a key when session storage is disabled.
  }
})();
