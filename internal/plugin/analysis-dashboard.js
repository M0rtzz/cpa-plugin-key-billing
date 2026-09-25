// Admin analysis additions share the host UI's transport, identity and theme helpers.
// Model aggregates come only from persisted usage.handle records.
const analysisDashboard = (() => {
  const PAGE = 20,
    LIMIT = 1048575;
  const text = (key, args) => m("dashboard." + key, args);
  const state = {
    tab: "events",
    model: "",
    type: "",
    failed: "",
    error: "",
    page: 0,
    snapshot: "",
    view: null,
    rankSort: "cost_usd",
    dimension: "billing",
    metric: "total_tokens",
    granularity: "auto",
    revision: 0,
    charts: [],
    exportTask: null,
  };
  let root = null,
    data = null,
    query = null,
    signature = "",
    scopeKey = "",
    busy = false,
    analysisBusy = false;
  const label = (name) => String(text(name));
  function element(tag, value, cls = "") {
    return el(tag, { class: cls, text: value });
  }
  function glyph(name) {
    const paths = {
      input: "M12 3v18m-7-7 7 7 7-7",
      output: "M12 21V3m-7 7 7-7 7 7",
      cache: "M4 8h16v12H4zM3 4h18v4H3zM9 12h6",
      write: "M4 8h16v12H4zM3 4h18v4H3zM9 14h6m-3-3v6",
      image: "M3 3h18v18H3zM3 16l5-5 5 5 3-3 5 5M15 7h.01",
      info: "M12 11v6m0-10h.01M22 12a10 10 0 1 1-20 0 10 10 0 0 1 20 0",
      lightning: "M13 2 3 14h8l-1 8 11-12h-8l1-8Z",
    };
    const ns = "http://www.w3.org/2000/svg",
      svg = document.createElementNS(ns, "svg"),
      path = document.createElementNS(ns, "path");
    for (const [key, value] of Object.entries({
      viewBox: "0 0 24 24",
      fill: name === "lightning" ? "currentColor" : "none",
      stroke: name === "lightning" ? "none" : "currentColor",
      "stroke-width": "1.6",
      "aria-hidden": "true",
      class: "dashboard-glyph",
    }))
      svg.setAttribute(key, value);
    path.setAttribute("d", paths[name]);
    path.setAttribute("stroke-linecap", "round");
    path.setAttribute("stroke-linejoin", "round");
    svg.append(path);
    return svg;
  }
  function button(name, run) {
    return el("button", { type: "button", onclick: run, text: text(name) });
  }
  function select(id, choices, value, change) {
    const node = el(
      "select",
      { id, "aria-label": text(id), onchange: (event) => change(event.target.value) },
      choices.map(([key, name]) => el("option", { value: key, text: name })),
    );
    node.value = value;
    return el("span", { class: "native-select" }, node);
  }
  const usdExact = (value) => (value == null ? "—" : "$" + Number(value).toLocaleString(locale(), { maximumFractionDigits: 8 }));
  const count = (value) => (value == null ? "—" : Number(value).toLocaleString(locale()));
  function chartNumber(value, unit) {
    if (value == null || !Number.isFinite(Number(value))) return "—";
    const number = Number(value),
      scale = unit === "percent" ? 1 : unit === "millions" || Math.abs(number) >= 1e6 ? 1e6 : Math.abs(number) >= 1e3 ? 1e3 : 1,
      suffix = unit === "percent" ? "%" : scale === 1e6 ? "M" : scale === 1e3 ? "K" : "";
    return (number / scale).toLocaleString(locale(), { minimumFractionDigits: 2, maximumFractionDigits: 2 }) + suffix;
  }
  function identity(row) {
    if (privacyMasked()) return label("masked");
    const key = keyDirectory.resolve(row),
      name = key.label || key.preview || label("unnamed");
    return key.label && key.preview ? name + " · " + key.preview : name;
  }
  function reset() {
    state.revision++;
    state.exportTask && (state.exportTask.cancelled = true);
    state.charts.forEach((chart) => chart.destroy());
    state.charts = [];
    root?.remove();
    $("analysis-body")
      .querySelectorAll(":scope > .dashboard-toolbar")
      .forEach((n) => n.remove());
    root = null;
    data = null;
    signature = "";
    query = null;
    busy = false;
    Object.assign(state, { view: null, snapshot: "", page: 0, tab: "events", model: "", type: "", failed: "", error: "" });
  }
  function chart(canvas, type, labels, datasets, { percent = false, unit = "" } = {}) {
    if (typeof Chart !== "function") {
      canvas.replaceWith(element("p", label("chart_unavailable"), "muted"));
      return;
    }
    const theme = { text: themeColor("--text-secondary", "#888"), grid: themeColor("--analysis-grid-color", "#8882") };
    const options = {
      responsive: true,
      maintainAspectRatio: false,
      animation: false,
      plugins: { legend: { display: type !== "doughnut", labels: { color: theme.text } }, tooltip: { mode: "index", intersect: false } },
    };
    if (type !== "doughnut")
      options.scales = {
        x: { ticks: { color: theme.text, maxTicksLimit: 12 }, grid: { color: theme.grid } },
        y: { beginAtZero: true, ticks: { color: theme.text }, grid: { color: theme.grid } },
      };
    if (unit) {
      options.scales.y.ticks.callback = (value) => chartNumber(value, unit);
      options.plugins.tooltip.callbacks = {
        label: (context) =>
          context.dataset.label + ": " + chartNumber(context.parsed.y, context.dataset.yAxisID === "percent" ? "percent" : unit),
      };
    }
    if (percent)
      options.scales.percent = {
        position: "right",
        min: 0,
        max: 100,
        ticks: { color: theme.text, callback: (value) => chartNumber(value, "percent") },
        grid: { drawOnChartArea: false },
      };
    state.charts.push(new Chart(canvas, { type, data: { labels, datasets }, options }));
  }
  function chartBox(title, id) {
    const canvas = el("canvas", { id, role: "img", "aria-label": title });
    return { box: el("div", { class: "dashboard-chart" }, canvas), canvas };
  }
  function colors(index) {
    return [
      "#3478f6",
      "#10b981",
      "#f59e0b",
      "#8b5cf6",
      "#ef6351",
      "#06b6d4",
      "#ec4899",
      "#84cc16",
      "#a78bfa",
      "#f97316",
      "#14b8a6",
      "#eab308",
    ][index % 12];
  }
  function render(value) {
    if (!value || currentRole !== "admin") return;
    const key = $("analysis-key").value;
    if (value._dashboardQuery && (value._dashboardQuery.api_key || "") !== key) return;
    const next = JSON.stringify([value, key, locale(), privacyMasked(), window.billingTheme.current(), typeof Chart]);
    if (signature === next && root?.isConnected) return;
    const changedData = data !== value,
      changedKey = scopeKey !== key;
    signature = next;
    scopeKey = key;
    data = value;
    query = {
      ...(value._dashboardQuery || selectedRange(false)),
      from: value.from || value._dashboardQuery?.from,
      to: value.to || value._dashboardQuery?.to,
    };
    if (!query.from || !query.to) Object.assign(query, selectedRange(false));
    if (key) query.api_key = key;
    else delete query.api_key;
    state.charts.forEach((c) => c.destroy());
    state.charts = [];
    if (!root?.isConnected) {
      root = el("div", { id: "analysis-dashboard" });
      $("analysis-body").append(root);
    }
    root.replaceChildren();
    if (changedKey) Object.assign(state, { model: "", type: "", failed: "", error: "", page: 0, snapshot: "", view: null });
    if (key && state.tab === "ranking") state.tab = "events";
    const toolbar = el("div", { class: "card dashboard-toolbar" });
    const rangeHours = (new Date(query.to) - new Date(query.from)) / 3600000;
    const picker = select(
      "granularity",
      [
        ["auto", text("auto")],
        ["hour", text("hour")],
        ["day", text("day")],
      ],
      state.granularity,
      (mode) => {
        state.granularity = mode;
        void guard(() => loadPage(false, { reload: true }));
      },
    );
    picker.querySelector('option[value="hour"]').disabled =
      Math.ceil(rangeHours + (new Date(query.from).getMinutes() * 60 + new Date(query.from).getSeconds()) / 3600) > 1000;
    if (Math.ceil(rangeHours + (new Date(query.from).getMinutes() * 60 + new Date(query.from).getSeconds()) / 3600) > 1000)
      picker.title = label("bucket_limit");
    toolbar.append(
      element("span", text("granularity")),
      picker,
      element("span", text("effective", { value: label(value.granularity || "day") }), "muted"),
    );
    toolbar.hidden = !!key;
    // The granularity control precedes the summary, next to the shared date controls.
    root.append(toolbar);
    const content = $("analysis-body").querySelector(".analysis-content");
    if (content) content.before(toolbar);
    const staleToolbar = $("analysis-body").querySelectorAll(":scope > .dashboard-toolbar");
    for (const node of staleToolbar) if (node !== toolbar) node.remove();
    const oldDistribution = $("usage-distribution");
    if (oldDistribution) oldDistribution.hidden = !key;
    if (!key) {
      renderModels();
      renderEndpoint();
      renderTokenTrend();
      renderKeyTrend();
    }
    renderTableShell();
    if (changedData || changedKey || !state.view) {
      state.snapshot = String(value.snapshot_id ?? "");
      state.page = 0;
      void loadRecords();
    } else renderRecords();
  }
  function modelFields() {
    return state.dimension === "billing"
      ? ["billing_model"]
      : state.dimension === "request"
        ? ["requested_model"]
        : state.dimension === "upstream"
          ? ["reported_model"]
          : ["requested_model", "reported_model"];
  }
  function groupedModels() {
    const groups = new Map();
    const fields = modelFields(),
      missing = { billing_model: "billing_unknown", requested_model: "request_unknown", reported_model: "upstream_unknown" };
    for (const row of data.model_groups || []) {
      const name = fields.map((field) => row[field] || label(missing[field])).join(" → "),
        id = JSON.stringify(fields.map((field) => row[field] || ""));
      if (!groups.has(id))
        groups.set(id, { name, requests: 0, total_tokens: 0, cost_usd: 0, before_global_usd: 0, unconvertible: 0, keys: new Map() });
      const group = groups.get(id);
      for (const field of ["requests", "total_tokens", "cost_usd", "before_global_usd", "unconvertible"])
        group[field] += Number(row[field] || 0);
      if (!group.keys.has(row.key))
        group.keys.set(row.key, { ...row, requests: 0, total_tokens: 0, cost_usd: 0, before_global_usd: 0, unconvertible: 0 });
      const sub = group.keys.get(row.key);
      for (const field of ["requests", "total_tokens", "cost_usd", "before_global_usd", "unconvertible"])
        sub[field] += Number(row[field] || 0);
    }
    return [...groups.values()].sort((a, b) => b[state.metric] - a[state.metric] || a.name.localeCompare(b.name));
  }
  function modelCells(row, name) {
    const before = row.unconvertible
      ? el("span", {
          title: text("conversion_detail", { value: usdExact(row.before_global_usd), count: row.unconvertible }),
          text: text("incomplete_conversion"),
        })
      : usdExact(row.before_global_usd);
    return [name, count(row.requests), count(row.total_tokens), usdExact(row.cost_usd), before];
  }
  function table(headers, rows) {
    return el(
      "div",
      { class: "table-scroll" },
      el(
        "table",
        { class: "dashboard-table" },
        el(
          "thead",
          {},
          el(
            "tr",
            {},
            headers.map((h) => el("th", { text: h })),
          ),
        ),
        el("tbody", {}, rows),
      ),
    );
  }
  function tableRow(values) {
    return el(
      "tr",
      {},
      values.map((v) => el("td", {}, v ?? "—")),
    );
  }
  function renderModels() {
    const card = el("section", { class: "card", id: "dashboard-models" });
    const controls = el(
      "div",
      { class: "dashboard-toolbar" },
      element("h2", text("model_distribution")),
      select(
        "dimension",
        [
          ["billing", text("billing")],
          ["request", text("request")],
          ["upstream", text("upstream")],
          ["mapping", text("mapping")],
        ],
        state.dimension,
        (v) => {
          state.dimension = v;
          replaceModels();
        },
      ),
      select(
        "metric",
        [
          ["total_tokens", text("by_tokens")],
          ["cost_usd", text("by_cost")],
        ],
        state.metric,
        (v) => {
          state.metric = v;
          replaceModels();
        },
      ),
    );
    const groups = groupedModels();
    const fields = modelFields(),
      rows = data.model_groups || [],
      total = rows.reduce((sum, row) => sum + row.requests, 0),
      complete = rows.reduce((sum, row) => sum + (fields.every((field) => row[field]) ? row.requests : 0), 0);
    card.append(
      controls,
      element("p", text("model_note"), "muted dashboard-note"),
      element("p", text("model_coverage", { complete: count(complete), total: count(total) }), "muted dashboard-model-coverage"),
    );
    if (state.dimension !== "billing" && complete < total) card.append(element("p", text("model_missing_note"), "muted dashboard-note"));
    if (!groups.length) card.append(element("p", text("empty"), "empty"));
    else {
      const plot = chartBox(label("model_distribution"), "dashboard-model-chart");
      const body = [];
      for (const row of groups) {
        const expand = el("button", { type: "button", class: "link dashboard-expand", "aria-expanded": "false", text: "› " + row.name });
        const detail = table(
          [text("key"), text("requests"), text("tokens"), text("actual"), text("before")],
          [...row.keys.values()]
            .sort((a, b) => b[state.metric] - a[state.metric] || a.key.localeCompare(b.key))
            .map((k) => tableRow(modelCells(k, identity(k)))),
        );
        const detailRow = el("tr", {}, el("td", { colspan: 5 }, detail));
        detailRow.hidden = true;
        expand.onclick = () => {
          detailRow.hidden = !detailRow.hidden;
          expand.setAttribute("aria-expanded", String(!detailRow.hidden));
        };
        body.push(tableRow(modelCells(row, expand)), detailRow);
      }
      const layout = el(
        "div",
        { class: "dashboard-distribution" },
        plot.box,
        table([text("model"), text("requests"), text("tokens"), text("actual"), text("before")], body),
      );
      card.append(layout);
      root.append(card);
      const shown = groups.slice(0, 12).map((r) => ({ name: r.name, value: r[state.metric] }));
      if (groups.length > 12) shown.push({ name: label("other"), value: groups.slice(12).reduce((sum, r) => sum + r[state.metric], 0) });
      if (shown.some((r) => r.value > 0))
        chart(
          plot.canvas,
          "doughnut",
          shown.map((r) => r.name),
          [{ data: shown.map((r) => r.value), backgroundColor: shown.map((_, i) => colors(i)), borderWidth: 0 }],
        );
      else plot.box.replaceChildren(element("p", text("no_metric"), "empty"));
      return;
    }
    root.append(card);
  }
  function replaceModels() {
    state.charts = state.charts.filter((c) => {
      if (c.canvas?.id === "dashboard-model-chart") {
        c.destroy();
        return false;
      }
      return true;
    });
    const old = $("dashboard-models"),
      next = old.nextSibling;
    old.remove();
    renderModels();
    root.insertBefore($("dashboard-models"), next);
  }
  function renderEndpoint() {
    root.append(
      el(
        "section",
        { class: "card" },
        el(
          "div",
          { class: "dashboard-toolbar" },
          element("h2", text("endpoint_distribution")),
          ...["inbound", "upstream", "path", "by_tokens", "by_cost"].map((key) =>
            el("button", { type: "button", disabled: true, text: text(key) }),
          ),
        ),
        element("p", text("endpoint_unavailable"), "empty"),
      ),
    );
  }
  function renderTokenTrend() {
    const plot = chartBox(label("token_trend"), "dashboard-token-chart"),
      trends = data.trends;
    const note = data.missing_usage ? text("missing_usage", { count: data.missing_usage }) : text("token_note");
    root.append(el("section", { class: "card" }, element("h2", text("token_trend")), element("p", note, "muted"), plot.box));
    const fields = ["input_tokens", "output_tokens", "cache_write_tokens", "cache_read_tokens", "cache_rate"];
    chart(
      plot.canvas,
      "line",
      trends.requests.map((p) => new Date(p.time).toLocaleString(locale())),
      fields.map((field, i) => ({
        label: label(field),
        data: (trends[field] || []).map((p, index) =>
          field === "cache_rate" &&
          !(trends.input_tokens[index]?.value + trends.cache_read_tokens[index]?.value + trends.cache_write_tokens[index]?.value)
            ? null
            : p.value,
        ),
        borderColor: colors(i),
        backgroundColor: colors(i),
        borderWidth: 2,
        pointRadius: 1,
        tension: 0.15,
        yAxisID: field === "cache_rate" ? "percent" : "y",
        borderDash: field === "cache_rate" ? [5, 5] : [],
      })),
      { percent: true, unit: "tokens" },
    );
  }
  function renderKeyTrend() {
    const series = data.key_trends || [],
      plot = chartBox(label("recent_top10"), "dashboard-key-chart");
    const card = el("section", { class: "card" }, element("h2", text("recent_top10")), element("p", text("top10_note"), "muted"));
    root.append(card);
    if (!series.length) {
      card.append(element("p", text("empty"), "empty"));
      return;
    }
    card.append(plot.box);
    const aliases = series.map((row) => {
      const key = keyDirectory.resolve(row),
        name = String(key.label || "").trim();
      return name && name !== key.preview && name !== row.key && !/^sk-/i.test(name) ? name : "";
    });
    // Assign distinct styles within the displayed roster, independent of order
    // and labels. Hashing individual keys can give adjacent or identical hues.
    const palette = ["#0072b2", "#e69f00", "#009e73", "#cc79a7", "#d55e00", "#56b4e9", "#b59f00", "#9b6ef3", "#24a6a6", "#878787"],
      styles = new Map(
        [...series]
          .sort((a, b) => a.key.localeCompare(b.key))
          .map((s, i) => [
            s.key,
            { borderColor: palette[i % palette.length], backgroundColor: palette[i % palette.length], borderDash: i < 5 ? [] : [6, 3] },
          ]),
      );
    chart(
      plot.canvas,
      "line",
      series[0].points.map((p) => new Date(p.time).toLocaleString(locale())),
      series.map((s, index) => ({
        label: privacyMasked()
          ? label("masked")
          : aliases[index]
            ? aliases[index] + (aliases.filter((name) => name === aliases[index]).length > 1 ? " (" + (index + 1) + ")" : "")
            : label("unnamed") + " " + (index + 1),
        data: s.points.map((p) => p.value),
        ...styles.get(s.key),
        borderWidth: 2,
        pointRadius: 1,
        tension: 0.15,
      })),
      { unit: "millions" },
    );
  }
  function params(page = state.page, limit = PAGE) {
    const p = new URLSearchParams({ from: query.from, to: query.to, offset: page * PAGE, limit });
    if (query.api_key) p.set("api_key", query.api_key);
    if (state.snapshot) p.set("snapshot_id", state.snapshot);
    if (state.model) p.set("model", state.model);
    if (state.tab === "events") {
      if (state.type) p.set("request_type", state.type);
      if (state.failed) p.set("failed", state.failed);
    } else if (state.error) p.set("error_type", state.error);
    return p;
  }
  function rankings() {
    return (data.usage_distribution?.api_keys || [])
      .slice()
      .sort((a, b) => b[state.rankSort] - a[state.rankSort] || a.key.localeCompare(b.key));
  }
  function renderTableShell() {
    const card = el("section", { class: "card", id: "dashboard-records" });
    const tabs = el("div", { class: "tabs", role: "tablist", "aria-label": text("records") });
    for (const tab of scopeKey ? ["events", "errors"] : ["events", "errors", "ranking"])
      tabs.append(
        el("button", {
          type: "button",
          role: "tab",
          "aria-selected": String(state.tab === tab),
          class: state.tab === tab ? "active" : "",
          text: text(tab),
          onclick: () => {
            state.tab = tab;
            state.page = 0;
            state.view = null;
            state.snapshot = String(data.snapshot_id ?? "");
            rerenderShell();
            void loadRecords();
          },
        }),
      );
    const refresh = button("refresh", () => {
      state.snapshot = "";
      state.page = 0;
      void loadRecords(true);
    });
    refresh.id = "dashboard-refresh";
    const exp = button("export", () => void guard(exportExcel));
    exp.id = "dashboard-export";
    const cancel = button("cancel", () => {
      if (state.exportTask) state.exportTask.cancelled = true;
    });
    cancel.id = "dashboard-export-cancel";
    cancel.hidden = !state.exportTask;
    card.append(el("div", { class: "dashboard-toolbar" }, tabs, refresh, exp, cancel), element("p", "", "muted dashboard-export-progress"));
    const filters = el("div", { class: "dashboard-toolbar dashboard-filters" });
    if (state.tab !== "ranking") {
      const model = el("input", {
        id: "dashboard-model",
        list: "dashboard-model-list",
        placeholder: text("all_models"),
        "aria-label": text("model"),
      });
      model.value = state.model;
      model.onchange = () => changeFilter("model", model.value.trim());
      filters.append(
        model,
        el(
          "datalist",
          { id: "dashboard-model-list" },
          (state.view?.filter_options?.models || []).map((v) => el("option", { value: v })),
        ),
      );
      if (state.tab === "events")
        filters.append(
          select("type", [["", text("all_types")], ...["stream", "sync", "ws", "unknown"].map((v) => [v, text(v)])], state.type, (v) =>
            changeFilter("type", v),
          ),
          select(
            "status",
            [
              ["", text("all_status")],
              ["false", text("success")],
              ["true", text("failed")],
            ],
            state.failed,
            (v) => changeFilter("failed", v),
          ),
        );
      else {
        const error = el("input", { id: "dashboard-error", placeholder: text("error_type"), "aria-label": text("error_type") });
        error.value = state.error;
        error.onchange = () => changeFilter("error", error.value.trim());
        filters.append(error);
      }
      filters.append(
        button("clear", () => {
          Object.assign(state, { model: "", type: "", failed: "", error: "", page: 0, snapshot: "" });
          rerenderShell();
          void loadRecords();
        }),
        element("small", text("filter_note"), "muted"),
      );
    } else
      filters.append(
        select(
          "rank_sort",
          [
            ["cost_usd", text("by_cost")],
            ["total_tokens", text("by_tokens")],
            ["requests", text("requests")],
          ],
          state.rankSort,
          (v) => {
            state.rankSort = v;
            state.page = 0;
            renderRecords();
          },
        ),
      );
    card.append(
      filters,
      el("div", { id: "dashboard-table", "aria-live": "polite" }),
      el("div", { id: "dashboard-pagination", class: "dashboard-toolbar" }),
    );
    root.append(card);
  }
  function rerenderShell() {
    $("dashboard-records")?.remove();
    renderTableShell();
    renderRecords();
  }
  function changeFilter(key, value) {
    state[key] = value;
    state.page = 0;
    state.snapshot = "";
    void loadRecords();
  }
  async function loadRecords(refresh = false) {
    if (!query) return;
    if (refresh && state.tab !== "ranking") query = { ...query, ...selectedRange(false) };
    const revision = ++state.revision,
      generation = sessionGeneration;
    busy = true;
    renderRecords();
    try {
      if (state.tab === "ranking") {
        if (refresh) await loadPage(false, { reload: true });
        return;
      }
      const view = await plugin("GET", "/" + state.tab + "?" + params());
      if (revision !== state.revision || generation !== sessionGeneration || (query.api_key || "") !== $("analysis-key").value) return;
      state.view = view;
      state.snapshot = String(view.snapshot_id ?? state.snapshot);
      if (state.page && state.page * PAGE >= view.total) {
        state.page = Math.max(0, Math.ceil(view.total / PAGE) - 1);
        void loadRecords();
        return;
      }
      const list = $("dashboard-model-list");
      if (list && view.filter_options?.models) list.replaceChildren(...view.filter_options.models.map((v) => el("option", { value: v })));
    } catch (error) {
      if (revision === state.revision && generation === sessionGeneration) {
        state.view = null;
        notify(error.message || String(error), "err");
      }
    } finally {
      if (revision === state.revision) {
        busy = false;
        renderRecords();
      }
    }
  }
  function tokenValues(entry) {
    const t = entry.token_usage,
      c = entry.cost || {};
    if (t && t.quality !== "inconsistent")
      return {
        input: t.input?.uncached_tokens,
        output: t.output?.total_tokens,
        read: t.input?.cache_read_tokens,
        write: t.input?.cache_write_tokens,
        reasoning: t.output?.reasoning_tokens,
        total: t.total_tokens,
      };
    if (entry.accounting_quality === "complete")
      return {
        input: c.uncached_input_tokens,
        output: c.billed_output_tokens,
        read: c.cache_read_tokens,
        write: c.cache_write_tokens,
        reasoning: entry.reasoning_tokens,
        total: c.uncached_input_tokens + c.billed_output_tokens + c.cache_read_tokens + c.cache_write_tokens,
      };
    return { total: t?.total_tokens };
  }
  function info(title, lines) {
    const popup = el(
      "div",
      { class: "dashboard-detail", role: "tooltip" },
      element("strong", title),
      ...lines.map(([key, value]) => el("div", { class: "dashboard-detail-row" }, element("span", key), element("span", value))),
    );
    popup.hidden = true;
    const trigger = el("button", { type: "button", class: "dashboard-info", "aria-label": title, "aria-expanded": "false" }, glyph("info"));
    const wrap = el("span", { class: "dashboard-detail-wrap" }, trigger, popup);
    const show = () => {
      popup.hidden = false;
      trigger.setAttribute("aria-expanded", "true");
      const r = trigger.getBoundingClientRect();
      popup.style.left = Math.max(8, Math.min(innerWidth - 348, r.right + 8)) + "px";
      popup.style.top = Math.max(8, Math.min(innerHeight - popup.offsetHeight - 8, r.top)) + "px";
    };
    const hide = () => {
      popup.hidden = true;
      trigger.setAttribute("aria-expanded", "false");
    };
    wrap.onmouseenter = show;
    wrap.onmouseleave = () => {
      if (document.activeElement !== trigger) hide();
    };
    trigger.onfocus = show;
    trigger.onblur = hide;
    trigger.onclick = () => (popup.hidden ? show() : hide());
    trigger.onkeydown = (e) => {
      if (e.key === "Escape") hide();
    };
    return wrap;
  }
  function tokensCell(entry) {
    const v = tokenValues(entry),
      lines = el("div", { class: "dashboard-token-lines" });
    function part(kind, value, name, cls) {
      return el("span", { class: "dashboard-token-part " + cls, title: label(name) }, glyph(kind), element("span", count(value)));
    }
    if (v.input != null || v.output != null)
      lines.append(
        el(
          "div",
          { class: "dashboard-token-line" },
          part("input", v.input, "input_tokens", "dashboard-token-input"),
          part("output", v.output, "output_tokens", "dashboard-token-output"),
        ),
      );
    const cache = el("div", { class: "dashboard-token-line" });
    if (v.read > 0) cache.append(part("cache", v.read, "cache_read_tokens", "dashboard-token-cache"));
    if (v.write > 0) cache.append(part("write", v.write, "cache_write_tokens", "dashboard-token-cache"));
    if (cache.childNodes.length) lines.append(cache);
    lines.append(element("small", label("total") + " " + count(v.total), "muted"));
    if (/^(gpt-image-|dall-e-)/i.test((entry.response_model || entry.upstream_model || entry.billing_model || "").split("/").pop()))
      lines.append(
        el("span", { class: "dashboard-token-part dashboard-token-image" }, glyph("image"), element("small", text("image_note"))),
      );
    const node = el(
      "div",
      { class: "dashboard-metric" },
      lines,
      info(label("token_detail"), [
        [label("input_tokens"), count(v.input)],
        [label("output_tokens"), count(v.output)],
        [label("cache_read_tokens"), count(v.read)],
        [label("cache_write_tokens"), count(v.write)],
        [label("reasoning"), count(v.reasoning)],
        [label("total"), count(v.total)],
      ]),
    );
    return node;
  }
  function costCell(entry) {
    const c = entry.cost || {},
      p = c.pricing || {},
      node = el("div", { class: "dashboard-metric" }, element("strong", usdExact(c.total_usd), "dashboard-billed-amount"));
    if (Number.isFinite(c.service_tier_multiplier) && c.service_tier_multiplier !== 1)
      node.append(
        el(
          "span",
          { class: "dashboard-multiplier", title: label("tier_multiplier") + ": ×" + count(c.service_tier_multiplier) },
          glyph("lightning"),
          element("span", "×" + count(c.service_tier_multiplier)),
        ),
      );
    if (c.long_context) node.append(element("small", text("long_context"), "muted"));
    node.append(
      info(label("cost_detail"), [
        [label("input_tokens"), usdExact(c.uncached_input_usd)],
        [label("output_tokens"), usdExact(c.output_usd)],
        [label("cache_read_tokens"), usdExact(c.cache_read_usd)],
        [label("cache_write_tokens"), usdExact(c.cache_write_usd)],
        [label("global_multiplier"), count(c.billing_multiplier)],
        [label("tier_multiplier"), count(c.service_tier_multiplier)],
        [label("request_tier"), entry.service_tier || "—"],
        [label("response_tier"), entry.response_service_tier || "—"],
        [label("billing_tier"), p.service_tier || "—"],
        [label("tier_source"), p.tier_source || "—"],
        [label("unit_input"), usdExact(c.applied_input_per_1m)],
        [label("unit_output"), usdExact(c.applied_output_per_1m)],
        [label("unit_read"), usdExact(c.applied_cache_read_per_1m)],
        [label("unit_write"), usdExact(c.applied_cache_write_per_1m)],
        [label("before"), c.billing_multiplier > 0 ? usdExact(c.total_usd / c.billing_multiplier) : "—"],
        [label("actual"), usdExact(c.total_usd)],
      ]),
    );
    return node;
  }
  function tokenValues(entry) {
    const t = entry.token_usage,
      c = entry.cost || {};
    if (t && t.quality !== "inconsistent")
      return {
        quality: t.quality,
        input: t.input?.uncached_tokens,
        output: t.output?.total_tokens,
        read: t.input?.cache_read_tokens,
        write: t.input?.cache_write_tokens,
        reasoning: t.output?.reasoning_tokens,
        unclassified: t.unclassified_tokens,
        total: t.total_tokens,
      };
    if (entry.accounting_quality === "complete")
      return {
        quality: "complete",
        input: c.uncached_input_tokens,
        output: c.billed_output_tokens,
        read: c.cache_read_tokens,
        write: c.cache_write_tokens,
        reasoning: entry.reasoning_tokens,
        unclassified: 0,
        total:
          c.uncached_input_tokens +
          c.billed_output_tokens +
          c.cache_read_tokens +
          c.cache_write_tokens,
        historical: true,
      };
    return {
      quality: t?.quality || entry.accounting_quality || "",
      unclassified: t?.unclassified_tokens,
      total: t?.total_tokens,
    };
  }
  function info(title, sections, notes = []) {
    const popup = el(
      "div",
      { class: "dashboard-detail", role: "tooltip" },
      element("h3", title),
    );
    for (const note of notes)
      popup.append(element("small", note, "dashboard-detail-note"));
    for (const section of sections) {
      const group = el("div", { class: "dashboard-detail-section" });
      for (const [key, value] of section.rows || [])
        group.append(
          el(
            "div",
            { class: "dashboard-detail-row" },
            element("span", key),
            element("strong", value),
          ),
        );
      if (section.total)
        group.append(
          el(
            "div",
            { class: "dashboard-detail-row dashboard-detail-total" },
            element("span", section.total[0]),
            element("strong", section.total[1]),
          ),
        );
      popup.append(group);
    }
    popup.hidden = true;
    const trigger = el(
      "button",
      {
        type: "button",
        class: "dashboard-info",
        "aria-label": title,
        "aria-expanded": "false",
      },
      glyph("info"),
    );
    const wrap = el("span", { class: "dashboard-detail-wrap" }, trigger, popup);
    const show = () => {
      popup.hidden = false;
      trigger.setAttribute("aria-expanded", "true");
      const r = trigger.getBoundingClientRect();
      popup.style.left =
        Math.max(8, Math.min(innerWidth - 348, r.right + 8)) + "px";
      popup.style.top =
        Math.max(8, Math.min(innerHeight - popup.offsetHeight - 8, r.top)) +
        "px";
    };
    const hide = () => {
      popup.hidden = true;
      trigger.setAttribute("aria-expanded", "false");
    };
    wrap.onmouseenter = show;
    wrap.onmouseleave = () => {
      if (document.activeElement !== trigger) hide();
    };
    trigger.onfocus = show;
    trigger.onblur = hide;
    trigger.onclick = () => (popup.hidden ? show() : hide());
    trigger.onkeydown = (e) => {
      if (e.key === "Escape") hide();
    };
    return wrap;
  }
  function tokensCell(entry) {
    const v = tokenValues(entry),
      lines = el("div", { class: "dashboard-token-lines" });
    function part(kind, value, name, cls) {
      return el(
        "span",
        { class: "dashboard-token-part " + cls, title: label(name) },
        glyph(kind),
        element("span", count(value)),
      );
    }
    if (v.input != null || v.output != null)
      lines.append(
        el(
          "div",
          { class: "dashboard-token-line" },
          part("input", v.input, "input_tokens", "dashboard-token-input"),
          part("output", v.output, "output_tokens", "dashboard-token-output"),
        ),
      );
    const cache = el("div", { class: "dashboard-token-line" });
    if (v.read > 0)
      cache.append(
        part("cache", v.read, "cache_read_tokens", "dashboard-token-cache"),
      );
    if (v.write > 0)
      cache.append(
        part("write", v.write, "cache_write_tokens", "dashboard-token-cache"),
      );
    if (cache.childNodes.length) lines.append(cache);
    lines.append(
      element("small", label("total") + " " + count(v.total), "muted"),
    );
    if (
      /^(gpt-image-|dall-e-)/i.test(
        (
          entry.response_model ||
          entry.upstream_model ||
          entry.billing_model ||
          ""
        )
          .split("/")
          .pop(),
      )
    )
      lines.append(
        el(
          "span",
          { class: "dashboard-token-part dashboard-token-image" },
          glyph("image"),
          element("small", text("image_note")),
        ),
      );
    const node = el(
      "div",
      { class: "dashboard-metric" },
      lines,
      info(
        label("token_detail"),
        [
          {
            rows:
              v.quality !== "inconsistent"
                ? [
                    ["input_tokens", v.input],
                    ["output_tokens", v.output],
                    ["cache_read_tokens", v.read],
                    ["cache_write_tokens", v.write],
                  ]
                    .filter(
                      ([, value]) =>
                        value != null &&
                        (v.quality === "complete" || value > 0),
                    )
                    .map(([key, value]) => [
                      label("token_" + key),
                      count(value),
                    ])
                    .concat(
                      v.reasoning > 0
                        ? [[label("reasoning"), count(v.reasoning)]]
                        : [],
                    )
                    .concat(
                      v.unclassified > 0
                        ? [
                            [
                              label("unclassified_tokens"),
                              count(v.unclassified),
                            ],
                          ]
                        : [],
                    )
                : [],
            total: [
              label("total_tokens"),
              v.total == null ? "—" : count(v.total),
            ],
          },
        ],
        [
          ...(v.quality !== "complete"
            ? [
                label(
                  v.quality === "inconsistent"
                    ? "token_inconsistent"
                    : "token_incomplete",
                ),
              ]
            : []),
          ...(isImageModel(entry) ? [label("image_detail_note")] : []),
          ...(v.historical ? [label("historical_tokens_note")] : []),
        ],
      ),
    );
    return node;
  }
  function isImageModel(entry) {
    return /^(gpt-image-|dall-e-)/i.test(
      (
        entry.response_model ||
        entry.upstream_model ||
        entry.billing_model ||
        ""
      )
        .split("/")
        .pop(),
    );
  }
  function tierLabel(value) {
    const key = String(value || "")
      .trim()
      .toLowerCase();
    const known = {
      priority: label("tier_priority"),
      fast: label("tier_fast"),
      flex: label("tier_flex"),
      default: label("tier_default"),
      standard: label("tier_standard"),
      auto: label("tier_auto"),
    };
    return known[key] || String(value || label("not_reported"));
  }
  function pricingMethod(value) {
    const key = String(value || "")
      .trim()
      .toLowerCase();
    const known = {
      base: label("method_base"),
      explicit_tier: label("method_explicit_tier"),
      model_ratio: label("method_model_ratio"),
      base_fallback: label("method_base_fallback"),
      oauth_multiplier: label("method_oauth_multiplier"),
    };
    return known[key] || String(value || label("not_reported"));
  }
  function tierSource(value) {
    const key = String(value || "")
      .trim()
      .toLowerCase();
    const known = {
      response: label("source_response"),
      request: label("source_request"),
      request_policy: label("source_request_policy"),
      default: label("source_default"),
    };
    return known[key] || String(value || label("not_reported"));
  }
  function costCell(entry) {
    const c = entry.cost || {},
      p = c.pricing || {},
      node = el(
        "div",
        { class: "dashboard-metric" },
        element("strong", usdExact(c.total_usd), "dashboard-billed-amount"),
      );
    if (
      Number.isFinite(c.service_tier_multiplier) &&
      c.service_tier_multiplier !== 1
    )
      node.append(
        el(
          "span",
          {
            class: "dashboard-multiplier",
            title:
              label("tier_multiplier") +
              ": ×" +
              count(c.service_tier_multiplier),
          },
          glyph("lightning"),
          element("span", "×" + count(c.service_tier_multiplier)),
        ),
      );
    if (c.long_context)
      node.append(element("small", text("long_context"), "muted"));
    const multiplier = Number.isFinite(Number(c.multiplier))
      ? Number(c.multiplier)
      : Number(c.billing_multiplier) * Number(c.service_tier_multiplier);
    const multiplierText = (value) =>
      value == null ? "—" : count(value) + "×";
    node.append(
      info(
        label("cost_detail"),
        [
          {
            rows: [
              ["input", c.uncached_input_usd, c.applied_input_per_1m],
              ["output", c.output_usd, c.applied_output_per_1m],
              ["read", c.cache_read_usd, c.applied_cache_read_per_1m],
              ["write", c.cache_write_usd, c.applied_cache_write_per_1m],
            ].flatMap(([kind, amount, rate]) => [
              [label(kind + "_cost"), usdExact(amount)],
              [
                label(kind + "_rate"),
                rate == null ? "—" : usdExact(rate) + label("rate_per_million"),
              ],
            ]),
          },
          {
            rows: [
              [label("request_tier"), tierLabel(entry.service_tier)],
              [label("response_tier"), tierLabel(entry.response_service_tier)],
              [label("billing_tier"), tierLabel(p.service_tier)],
              [label("tier_source"), tierSource(p.tier_source)],
              ...(p.method
                ? [[label("pricing_method"), pricingMethod(p.method)]]
                : []),
              [
                label("global_multiplier"),
                multiplierText(c.billing_multiplier),
              ],
              [
                label("tier_multiplier"),
                multiplierText(c.service_tier_multiplier),
              ],
              [label("final_multiplier"), multiplierText(multiplier)],
              [
                label("before"),
                c.billing_multiplier > 0
                  ? usdExact(c.total_usd / c.billing_multiplier)
                  : "—",
              ],
            ],
          },
          { total: [label("actual_charge"), usdExact(c.total_usd)] },
        ],
        [
          label("cost_detail_note"),
          ...(c.long_context ? [label("long_context_note")] : []),
          ...(isImageModel(entry) ? [label("image_cost_note")] : []),
        ],
      ),
    );
    return node;
  }
  function renderRecords() {
    const loading = busy || analysisBusy;
    const parent = $("dashboard-table"),
      pager = $("dashboard-pagination");
    if (!parent || !pager) return;
    const ranking = state.tab === "ranking",
      all = ranking ? rankings() : [],
      entries = ranking ? all.slice(state.page * PAGE, (state.page + 1) * PAGE) : state.view?.entries || [],
      total = ranking ? all.length : Number(state.view?.total || 0);
    parent.setAttribute("aria-busy", String(loading));
    if (loading) parent.replaceChildren(element("p", text("loading"), "empty"));
    else if (!entries.length) parent.replaceChildren(element("p", text("empty"), "empty"));
    else {
      let headers = ranking
        ? [text("rank"), text("key"), text("requests"), text("tokens"), text("actual")]
        : state.tab === "errors"
          ? [text("time"), text("key"), text("model"), text("http"), text("error_type"), text("error_body")]
          : [
              text("time"),
              text("key"),
              text("model"),
              text("type"),
              text("status"),
              text("reasoning_effort"),
              text("tokens"),
              text("actual"),
              text("timing"),
            ];
      if (scopeKey && !ranking) headers.splice(1, 1);
      const rows = entries.map((e, i) => {
        if (ranking) {
          const name = el("button", {
            type: "button",
            class: "link",
            text: identity(e),
            onclick: () => {
              $("analysis-key").value = e.key;
              $("analysis-key").dispatchEvent(new Event("change"));
            },
          });
          return tableRow([state.page * PAGE + i + 1, name, count(e.requests), count(e.total_tokens), usdExact(e.cost_usd)]);
        }
        let values = [new Date(e.at).toLocaleString(locale()), identity(e), e.billing_model || e.upstream_model || "—"];
        if (state.tab === "errors")
          values.push(
            e.status_code || "—",
            e.error_type || "—",
            el("details", {}, element("summary", text("error_body")), element("pre", e.body || e.reason || "—", "dashboard-error-body")),
          );
        else
          values.push(
            label(["stream", "sync", "ws"].includes(e.request_type) ? e.request_type : "unknown"),
            label(e.failed ? "failed" : "success"),
            e.reasoning_effort || "—",
            tokensCell(e),
            costCell(e),
            `${label("ttft")} ${e.ttft_ms == null ? "—" : count(e.ttft_ms) + " ms"} / ${label("latency")} ${e.latency_ms == null ? "—" : count(e.latency_ms) + " ms"}`,
          );
        if (scopeKey) values.splice(1, 1);
        return tableRow(values);
      });
      parent.replaceChildren(table(headers, rows));
    }
    const pages = Math.max(1, Math.ceil(total / PAGE)),
      go = (page) => {
        state.page = page;
        ranking ? renderRecords() : void loadRecords();
      };
    const first = button("first", () => go(0)),
      prev = button("previous", () => go(state.page - 1)),
      next = button("next", () => go(state.page + 1)),
      last = button("last", () => go(pages - 1));
    first.disabled = prev.disabled = loading || state.page === 0;
    next.disabled = last.disabled = loading || state.page >= pages - 1;
    const page = select(
      "page",
      Array.from({ length: pages }, (_, i) => [String(i), String(i + 1)]),
      String(state.page),
      (v) => go(Number(v)),
    );
    page.querySelector("select").disabled = loading || pages === 1;
    pager.replaceChildren(element("span", text("records_total", { count: total })), first, prev, page, next, last);
    $("dashboard-refresh").disabled = loading;
    $("dashboard-export").disabled = loading || !!state.exportTask;
  }
  let libraryPromise;
  function excelLibrary() {
    if (window.XLSX) return Promise.resolve(window.XLSX);
    if (!libraryPromise)
      libraryPromise = new Promise((resolve, reject) => {
        const script = document.createElement("script");
        script.src = "/v0/resource/plugins/cpa-key-billing/xlsx.js";
        script.onload = () => (window.XLSX ? resolve(window.XLSX) : reject(new Error(label("export_failed"))));
        script.onerror = () => {
          libraryPromise = null;
          script.remove();
          reject(new Error(label("export_failed")));
        };
        document.head.append(script);
      });
    return libraryPromise;
  }
  function exportRows(entries, kind, masked, sort) {
    if (kind === "ranking")
      return [
        [label("rank"), label("key"), label("requests"), label("tokens"), label("actual")],
        ...entries.map((r, i) => [i + 1, masked ? label("masked") : identity(r), r.requests, r.total_tokens, r.cost_usd]),
      ];
    const keys =
      kind === "errors"
        ? ["at", "label", "billing_model", "status_code", "error_type", "body"]
        : [
            "at",
            "label",
            "billing_model",
            "requested_model",
            "reported_model",
            "response_model",
            "request_type",
            "failed",
            "reasoning_effort",
            "input_tokens",
            "output_tokens",
            "total_tokens",
            "reasoning_tokens",
            "cache_read_tokens",
            "cache_write_tokens",
            "total_usd",
            "uncached_input_usd",
            "output_usd",
            "cache_read_usd",
            "cache_write_usd",
            "billing_multiplier",
            "service_tier_multiplier",
            "service_tier",
            "response_service_tier",
            "billed_service_tier",
            "tier_source",
            "pricing_method",
            "pricing_rule_version",
            "tier_fallback",
            "price_fallback",
            "applied_input_per_1m",
            "applied_output_per_1m",
            "applied_cache_read_per_1m",
            "applied_cache_write_per_1m",
            "ttft_ms",
            "latency_ms",
          ];
    return [
      keys,
      ...entries.map((e) => {
        const c = e.cost || {},
          v = tokenValues(e),
          p = c.pricing || {},
          row = {
            ...e,
            ...c,
            label: masked ? label("masked") : identity(e),
            input_tokens: v.input,
            output_tokens: v.output,
            total_tokens: v.total,
            reasoning_tokens: v.reasoning,
            cache_read_tokens: v.read,
            cache_write_tokens: v.write,
            billed_service_tier: p.service_tier,
            tier_source: p.tier_source,
            pricing_method: p.method,
            pricing_rule_version: p.rule_version,
            tier_fallback: p.tier_fallback,
            price_fallback: p.price_fallback,
          };
        return keys.map((key) => {
          const value = row[key];
          return value == null ? null : typeof value === "object" ? JSON.stringify(value) : value;
        });
      }),
    ];
  }
  async function exportExcel() {
    if (state.exportTask) return;
    const task = { cancelled: false },
      kind = state.tab,
      masked = privacyMasked(),
      generation = sessionGeneration,
      p = params(0, 1000),
      sorted = rankings();
    state.exportTask = task;
    const progress = root.querySelector(".dashboard-export-progress"),
      cancel = $("dashboard-export-cancel");
    cancel.hidden = false;
    renderRecords();
    const check = () => {
      if (task.cancelled || generation !== sessionGeneration) throw new Error(label("export_cancelled"));
    };
    try {
      const XLSX = await excelLibrary();
      check();
      let entries = [];
      if (kind === "ranking") entries = sorted;
      else {
        let offset = 0,
          total = 0;
        do {
          check();
          p.set("offset", String(offset));
          const result = await plugin("GET", "/" + kind + "?" + p);
          check();
          total = Number(result.total);
          if (total > LIMIT) throw new Error(label("excel_limit"));
          p.set("snapshot_id", String(result.snapshot_id));
          if (!result.entries.length && offset < total) throw new Error(label("export_failed"));
          entries.push(...result.entries);
          offset += result.entries.length;
          setText(progress, text("export_progress", { count: offset, total }));
          await new Promise((resolve) => setTimeout(resolve, 0));
        } while (offset < total);
      }
      if (entries.length > LIMIT) throw new Error(label("excel_limit"));
      check();
      const sheet = XLSX.utils.aoa_to_sheet(exportRows(entries, kind, masked, state.rankSort));
      const workbook = XLSX.utils.book_new();
      XLSX.utils.book_append_sheet(workbook, sheet, label(kind).slice(0, 31));
      const bytes = XLSX.write(workbook, { bookType: "xlsx", type: "array", compression: true });
      check();
      const url = URL.createObjectURL(new Blob([bytes], { type: "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet" }));
      const anchor = el("a", { href: url, download: "cpa-" + kind + "-" + new Date().toISOString().replaceAll(":", "-") + ".xlsx" });
      document.body.append(anchor);
      anchor.click();
      anchor.remove();
      setTimeout(() => URL.revokeObjectURL(url), 1000);
      setText(progress, text("export_progress", { count: entries.length, total: entries.length }));
    } finally {
      if (state.exportTask === task) state.exportTask = null;
      cancel.hidden = true;
      renderRecords();
    }
  }
  function invalidate() {
    state.revision++;
    busy = false;
    state.view = null;
    state.snapshot = "";
    signature = "";
  }
  return {
    render,
    reset,
    invalidate,
    beginLoad() {
      state.revision++;
      busy = false;
      analysisBusy = true;
      renderRecords();
    },
    endLoad() {
      analysisBusy = false;
      renderRecords();
    },
    get granularity() {
      return state.granularity;
    },
  };
})();
