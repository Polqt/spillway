(() => {
  "use strict";

  const keyForm = document.querySelector("#key-form");
  const keyInput = document.querySelector("#dashboard-key");
  const accessPanel = document.querySelector("#access-panel");
  const content = document.querySelector("#dashboard-content");
  const message = document.querySelector("#access-message");
  const disconnect = document.querySelector("#disconnect");
  const playgroundForm = document.querySelector("#playground-form");
  const playgroundModel = document.querySelector("#playground-model");
  const playgroundPrompt = document.querySelector("#playground-prompt");
  const playgroundStream = document.querySelector("#playground-stream");
  const playgroundMaxTokens = document.querySelector("#playground-max-tokens");
  const playgroundEstimate = document.querySelector("#playground-estimate");
  const playgroundResponse = document.querySelector("#playground-response");
  const playgroundRoute = document.querySelector("#playground-route");
  const playgroundMessage = document.querySelector("#playground-message");
  const playgroundSubmit = document.querySelector("#playground-submit");
  const playgroundCancel = document.querySelector("#playground-cancel");
  const keyName = "spillway-dashboard-key";
  let key = "";
  let refreshTimer = 0;
  let refreshController = null;
  let session = 0;
  let playgroundPrices = {};
  let playgroundController = null;

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
    if (response.status === 503 && body.error?.code === "store_disabled") return null;
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
      const explanation = document.createElement("div");
      explanation.className = "decision-skips";
      explanation.textContent = `What happened: ${explainRoute(row.route, row.cache, row.status)}`;
      const status = document.createElement("span");
      status.className = `status-pill${Number(row.status) >= 400 ? " error" : ""}`;
      status.textContent = Number(row.status) >= 400 ? "Error" : row.cache === "hit" ? "Cache hit" : "Served";
      const time = document.createElement("time");
      time.className = "decision-time";
      time.dateTime = row.at || "";
      time.textContent = row.at ? new Date(row.at).toLocaleString() : "";
      const routeWrap = document.createElement("div");
      routeWrap.append(route);
      routeWrap.append(explanation);
      const cache = document.createElement("div");
      cache.className = "decision-skips";
      cache.textContent = `Cache: ${row.cache || "not recorded"}`;
      routeWrap.append(cache);
      if (Array.isArray(row.skips) && row.skips.length) {
        const skips = document.createElement("div");
        skips.className = "decision-skips";
        skips.textContent = `Skipped: ${row.skips.map((skip) => `${skip.provider}/${skip.model} (${skip.reason})`).join(", ")}`;
        routeWrap.append(skips);
      }
      if (Array.isArray(row.attempts) && row.attempts.length) {
        const attempts = document.createElement("div");
        attempts.className = "decision-skips";
        attempts.textContent = `Attempts: ${row.attempts.map((attempt) => `${attempt.provider}/${attempt.model} (${attempt.outcome})`).join(", ")}`;
        routeWrap.append(attempts);
      }
      const timing = document.createElement("div");
      timing.append(status, time);
      item.append(identity, routeWrap, timing);
      list.append(item);
    }
  }

  function renderEvaluationRuns(rows) {
    const list = document.querySelector("#evaluation-runs");
    list.replaceChildren();
    if (!rows.length) {
      const empty = document.createElement("p");
      empty.className = "empty";
      empty.textContent = "No aggregate evaluation reports have been saved.";
      list.append(empty);
      return;
    }
    for (const run of rows) {
      const item = document.createElement("article");
      item.className = "decision";
      const title = document.createElement("div");
      title.className = "decision-model";
      title.textContent = run.run_id;
      const detail = document.createElement("div");
      detail.className = "decision-detail";
      detail.textContent = `${run.summary?.prompt_count || 0} prompts · corpus ${(run.corpus_sha256 || "").slice(0, 12)}`;
      item.append(title, detail);
      for (const gate of (run.summary?.results || []).filter((result) => result.difficulty === "all" && result.category === "all")) {
        const result = document.createElement("div");
        result.className = "decision-detail";
        result.textContent = `${gate.arm} · ${gate.gate}: ${gate.verdict}`;
        item.append(result);
      }
      const created = document.createElement("time");
      created.dateTime = run.created_at || "";
      created.textContent = run.created_at ? new Date(run.created_at).toLocaleString() : "";
      item.append(created);
      list.append(item);
    }
  }

  function explainRoute(route, cache, status) {
    if (cache === "hit") return "Spillway served the saved exact-cache response.";
    if (!route) return "No route reason was recorded.";
    const parts = route.split(";").map((part) => part.trim());
    const candidate = parts.find((part) => /^candidate \d+\/\d+$/.test(part));
    const candidateMatch = candidate && candidate.match(/^candidate (\d+)\/(\d+)$/);
    const validPart = (part) => /^(direct|auto|rules|classifier|classifier fallback|default|rule \d+|difficulty [a-z]+|tier [a-z0-9-]+|fallback [a-z0-9-]+|needs (tools|vision|json_mode|structured_outputs)(, (tools|vision|json_mode|structured_outputs))*|candidate \d+\/\d+|exhausted \(\d+ eligible\)|no capable model)$/.test(part);
    if (parts.some((part) => !validPart(part))) return "No plain-language explanation is available; the route reason above is shown verbatim.";
    const tier = parts.find((part) => part.startsWith("tier "))?.slice(5);
    const selection = candidateMatch
      ? Number(candidateMatch[1]) > 1
        ? `Spillway spilled over to candidate ${candidateMatch[1]} of ${candidateMatch[2]} after earlier candidates were unavailable.`
        : `Spillway selected candidate ${candidateMatch[1]} of ${candidateMatch[2]}.`
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

  function renderProviderStatus(providers) {
    const list = document.querySelector("#provider-status");
    list.replaceChildren();
    if (!providers.length) {
      const empty = document.createElement("p");
      empty.className = "empty";
      empty.textContent = "No providers are configured yet.";
      list.append(empty);
      return;
    }
    for (const provider of providers) {
      const item = document.createElement("article");
      item.className = "provider-status-card";
      const heading = document.createElement("div");
      heading.className = "provider-status-heading";
      const name = document.createElement("strong");
      name.textContent = provider.name;
      const configured = document.createElement("span");
      configured.className = "estimate-tag";
      configured.textContent = "Configured";
      heading.append(name, configured);
      const credential = document.createElement("p");
      credential.textContent = provider.credential_required
        ? provider.credential_present ? "Credential configured" : "Credential missing"
        : "No credential required";
      item.append(heading, credential);
      const providerSignals = Array.isArray(provider.signals) ? provider.signals : [];
      if (providerSignals.length) {
        const signals = document.createElement("ul");
        signals.className = "provider-signals";
        for (const signal of providerSignals) {
          const row = document.createElement("li");
          const model = document.createElement("strong");
          model.textContent = signal.model;
          const detail = document.createElement("span");
          const failures = Number(signal.upstream_failures || 0);
          const failureText = failures ? `${failures === 7 ? "At least " : ""}${failures} upstream failure${failures === 1 ? "" : "s"} observed. ` : "";
          const reasonText = signal.reason ? `Observed ${signal.reason}` : "No active cooldown";
          const breakerText = signal.breaker_state === "open" ? "; circuit open" : "";
          const untilText = signal.cooldown_until ? ` until ${new Date(signal.cooldown_until).toLocaleTimeString()}` : "";
          detail.textContent = `${failureText}${reasonText}${breakerText}${untilText}`;
          row.append(model, detail);
          signals.append(row);
        }
        item.append(signals);
      } else {
        const empty = document.createElement("p");
        empty.className = "empty";
        empty.textContent = "No active cooldown or failure signal observed.";
        item.append(empty);
      }
      list.append(item);
    }
  }

  function renderPlaygroundModels(models) {
    const selected = playgroundModel.value;
    playgroundModel.replaceChildren();
    for (const model of models) {
      const option = document.createElement("option");
      option.value = model.id;
      option.textContent = model.id;
      playgroundModel.append(option);
    }
    if (models.some((model) => model.id === selected)) playgroundModel.value = selected;
    updatePlaygroundEstimate();
  }

  function renderPlaygroundRoute(metadata) {
    playgroundRoute.replaceChildren();
    playgroundRoute.hidden = !metadata;
    if (!metadata) return;
    const heading = document.createElement("h3");
    heading.textContent = "Routing explanation";
    playgroundRoute.append(heading);
    const fields = [
      ["Provider and model", [metadata.provider, metadata.model].filter(Boolean).join("/") || "Not reported"],
      ["Route reason", metadata.route || "Not reported"],
      ["Cache", metadata.cache || "Not reported"],
      ["HTTP status", String(metadata.status)],
    ];
    for (const [label, value] of fields) {
      const row = document.createElement("p");
      const name = document.createElement("strong");
      name.textContent = label;
      const detail = document.createElement("span");
      detail.textContent = value;
      row.append(name, detail);
      playgroundRoute.append(row);
    }
    const summary = document.createElement("p");
    const summaryLabel = document.createElement("strong");
    summaryLabel.textContent = "What happened";
    const summaryText = document.createElement("span");
    summaryText.textContent = explainRoute(metadata.route, metadata.cache, metadata.status);
    summary.append(summaryLabel, summaryText);
    playgroundRoute.insertBefore(summary, playgroundRoute.children[3]);
  }

  function updatePlaygroundEstimate() {
    const price = playgroundPrices[playgroundModel.value];
    if (!price) {
      playgroundEstimate.hidden = true;
      playgroundEstimate.textContent = "";
      return;
    }
    const promptTokens = Math.ceil(Array.from(playgroundPrompt.value).length / 4);
    const outputTokens = Math.max(1, Number(playgroundMaxTokens.value) || 1);
    const usd = (promptTokens * price.input_micro_usd_per_million + outputTokens * price.output_micro_usd_per_million) / 1000000000000;
    playgroundEstimate.textContent = `Estimated cost: about $${usd.toFixed(6)}. Based on a rough prompt token estimate and the selected output limit; actual cost may differ.`;
    playgroundEstimate.hidden = false;
  }

  async function consumePlaygroundStream(response, append) {
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
          const text = event.choices?.[0]?.delta?.content;
          if (typeof text === "string") append(text);
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

  async function submitPlayground(event) {
    event.preventDefault();
    if (!playgroundModel.value || !playgroundPrompt.value.trim() || playgroundController) return;
    const controller = new AbortController();
    playgroundController = controller;
    playgroundResponse.textContent = "";
    renderPlaygroundRoute(null);
    playgroundMessage.textContent = "Sending request…";
    playgroundMessage.setAttribute("role", "status");
    playgroundCancel.hidden = false;
    playgroundSubmit.disabled = true;
    try {
      const response = await fetch("/v1/chat/completions", {
        method: "POST",
        headers: { Authorization: `Bearer ${key}`, "Content-Type": "application/json" },
        cache: "no-store",
        credentials: "omit",
        signal: controller.signal,
        body: JSON.stringify({ model: playgroundModel.value, messages: [{ role: "user", content: playgroundPrompt.value }], stream: playgroundStream.checked, max_tokens: Number(playgroundMaxTokens.value) }),
      });
      renderPlaygroundRoute({
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
      if (playgroundStream.checked) {
        await consumePlaygroundStream(response, (text) => { playgroundResponse.textContent += text; });
      } else {
        const body = await response.json();
        playgroundResponse.textContent = body.choices?.[0]?.message?.content ?? "";
      }
      playgroundMessage.textContent = "Request complete.";
    } catch (error) {
      if (error.name === "AbortError") {
        playgroundMessage.textContent = "Request cancelled.";
      } else {
        playgroundMessage.textContent = error.message || "The request failed.";
        playgroundMessage.setAttribute("role", "alert");
      }
    } finally {
      if (playgroundController === controller) playgroundController = null;
      playgroundCancel.hidden = true;
      playgroundSubmit.disabled = false;
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
      const [budget, today, quotas, providerStatus, modelResponse, pricing] = await Promise.all([
        request("/api/budget", signal),
        request(`/api/usage?${dateRange(todayStart, now)}&group=day`, signal),
        request("/api/quotas", signal),
        request("/api/providers", signal),
        request("/v1/models", signal),
        request("/api/pricing", signal)
      ]);
      if (currentSession !== session) return;
      renderProviderStatus(providerStatus.providers || []);
      playgroundPrices = pricing.prices || {};
      renderPlaygroundModels(modelResponse.data || []);
      if (today === null) {
        setBudget("today", budget.daily, 0);
        setBudget("month", budget.monthly, 0);
        renderTotals("#provider-totals", []);
        renderTotals("#model-totals", []);
        renderDecisions([]);
        renderEvaluationRuns([]);
        renderCooldowns(quotas.candidates || []);
        setText("#updated-at", "Connected · usage history is not stored");
        message.textContent = "Onboarding and local requests work without store:. Add store: only if you want usage and routing history to persist.";
        setConnection(true);
        return;
      }
      const [month, providers, models, decisions, evaluations] = await Promise.all([
        request(`/api/usage?${dateRange(monthStart, now)}&group=day`, signal),
        request(`/api/usage?${dateRange(monthStart, now)}&group=provider`, signal),
        request(`/api/usage?${dateRange(monthStart, now)}&group=model`, signal),
        request("/api/decisions?limit=20", signal),
        request("/api/evaluations?limit=20", signal)
      ]);
      if (currentSession !== session) return;
      const todaySpend = (today.data || []).reduce((sum, row) => sum + Number(row.cost_micro_usd || 0), 0);
      const monthSpend = (month.data || []).reduce((sum, row) => sum + Number(row.cost_micro_usd || 0), 0);
      setBudget("today", budget.daily, todaySpend);
      setBudget("month", budget.monthly, monthSpend);
      renderTotals("#provider-totals", providers.data || []);
      renderTotals("#model-totals", models.data || []);
      renderDecisions(decisions.data || []);
      renderEvaluationRuns(evaluations.data || []);
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
    if (playgroundController) playgroundController.abort();
    refreshController = null;
    renderPlaygroundRoute(null);
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

  playgroundForm.addEventListener("submit", submitPlayground);
  playgroundCancel.addEventListener("click", () => playgroundController?.abort());
  playgroundModel.addEventListener("change", updatePlaygroundEstimate);
  playgroundPrompt.addEventListener("input", updatePlaygroundEstimate);
  playgroundMaxTokens.addEventListener("input", updatePlaygroundEstimate);

  try {
    const savedKey = sessionStorage.getItem(keyName);
    if (savedKey) connect(savedKey);
  } catch (_error) {
    // The user can still enter a key when session storage is disabled.
  }
})();
