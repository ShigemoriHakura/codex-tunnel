import { msg, text, number, percent, dateTime, writeText } from "./i18n.mjs";

const unknown = () => text("usage.unknown");
export function metric(value, unit = "") {
  if (value == null) return unknown;
  return () => {
    if (unit === "%") return percent(value)();
    if (unit === "$")
      return new Intl.NumberFormat(document.documentElement.lang, {
        style: "currency",
        currency: "USD",
        currencyDisplay: "narrowSymbol",
        maximumFractionDigits: 6,
      }).format(value);
    return (
      number(Number(Number(value).toFixed(1)))() + (unit ? " " + unit : "")
    );
  };
}
export function tiles(root, entries, el) {
  const grid = el("div", undefined, "metric-grid");
  for (const [label, value] of entries) {
    const cell = el("div", undefined, "metric");
    cell.append(el("small", msg(label)), el("strong", value));
    grid.append(cell);
  }
  root.append(grid);
}
export function chooser(title, choices, value, el) {
  const label = el("label", msg(title)),
    select = el("select");
  for (const [key, name] of choices) {
    const option = el("option", name);
    option.value = key;
    select.append(option);
  }
  select.value = value;
  label.append(select);
  return [label, select];
}
function download(data) {
  const url = URL.createObjectURL(
    new Blob([JSON.stringify(data, null, 2)], { type: "application/json" }),
  );
  const link = document.createElement("a");
  link.href = url;
  link.download = "gateway-logs.json";
  link.click();
  setTimeout(() => URL.revokeObjectURL(url), 1000);
}
async function copyJSON(data) {
  const value = JSON.stringify(data, null, 2);
  if (navigator.clipboard && window.isSecureContext) {
    try {
      await navigator.clipboard.writeText(value);
      return;
    } catch {
      /* HTTP fallback below. */
    }
  }
  const previous = document.activeElement;
  const field = document.createElement("textarea");
  field.value = value;
  document.body.append(field);
  field.select();
  const copied = document.execCommand("copy");
  field.remove();
  previous?.focus();
  if (!copied) throw new Error(text("monitor.copyFailed"));
}
function direction(value) {
  return msg(value === "ingress" ? "monitor.ingress" : "monitor.egress");
}
function badge(el, status) {
  return el("span", status, status >= 400 ? "status problem" : "status");
}
function pageMetrics(records) {
  // Prefer the final client result when both sides of a request share this page.
  const byID = new Map();
  for (const row of records) {
    if (!byID.has(row.request_id) || row.direction === "ingress")
      byID.set(row.request_id, row);
  }
  const values = [...byID.values()];
  const sum = (key, average = false) => {
    const reported = values.map((v) => v[key]).filter((v) => v != null);
    if (!reported.length) return null;
    return (
      reported.reduce((s, n) => s + n, 0) / (average ? reported.length : 1)
    );
  };
  return [
    ["monitor.ttft", metric(sum("ttft_ms", true), "ms")],
    ["monitor.speed", metric(sum("speed", true), "tok/s")],
    ["monitor.latency", metric(sum("latency_ms", true), "ms")],
    [
      "monitor.success",
      metric(
        values.length
          ? (values.filter((v) => v.status < 400).length * 100) / values.length
          : null,
        "%",
      ),
    ],
    ["usage.input", metric(sum("input"))],
    ["usage.output", metric(sum("output"))],
    ["monitor.cached", metric(sum("cached"))],
    ["monitor.cost", metric(sum("cost"), "$")],
  ];
}
function drawDetail(root, row, { el, button }) {
  root.replaceChildren();
  const header = el("div", undefined, "bar");
  const heading = el("h2", msg("monitor.detail"));
  heading.id = "log-detail-title";
  root.setAttribute("aria-labelledby", heading.id);
  header.append(
    heading,
    button(msg("common.close"), () => root.close()),
  );
  root.append(header);
  const tabs = el("div", undefined, "toolbar");
  const body = el("div");
  function show(raw) {
    formatted.setAttribute("aria-pressed", String(!raw));
    json.setAttribute("aria-pressed", String(raw));
    body.replaceChildren();
    if (raw) {
      body.append(el("pre", JSON.stringify(row, null, 2)));
      return;
    }
    const properties = el("dl", undefined, "properties");
    const fields = [
      ["monitor.id", row.request_id],
      ["col.time", dateTime(row.created)],
      ["monitor.direction", direction(row.direction)],
      ["monitor.path", row.method + " " + row.path],
      ["col.status", badge(el, row.status)],
      ["col.model", row.model],
      ["monitor.account", row.account],
      [
        "monitor.provider",
        row.path.startsWith("registry.npmjs.org/") ||
        row.path.startsWith("npm ")
          ? "npm"
          : row.direction === "egress" || row.account || row.metered != null
            ? "Codex CLI"
            : null,
      ],
      ["col.owner", row.key_name || row.owner],
      ["col.session", row.session],
      [
        "monitor.totalTokens",
        metric(
          row.input == null || row.output == null
            ? null
            : row.input + row.output,
        ),
      ],
      [
        "monitor.streaming",
        row.stream == null
          ? null
          : msg(row.stream ? "monitor.yes" : "monitor.no"),
      ],
      ["col.latency", metric(row.latency_ms, "ms")],
      ["monitor.ttft", metric(row.ttft_ms, "ms")],
      ["monitor.speed", metric(row.speed, "tok/s")],
      ["monitor.cost", metric(row.cost, "$")],
      ["usage.input", metric(row.input)],
      ["usage.output", metric(row.output)],
      ["monitor.cached", metric(row.cached)],
      ["monitor.reasoning", metric(row.reasoning)],
      [
        "monitor.error",
        [row.error_code, row.message].filter(Boolean).join(" · "),
      ],
    ];
    for (const [label, v] of fields) {
      const dd = el("dd");
      if (v instanceof Node) dd.append(v);
      else writeText(dd, v || "—");
      properties.append(el("dt", msg(label)), dd);
    }
    body.append(properties);
    for (const [label, key] of [
      ["monitor.requestBody", "request_body"],
      ["monitor.responseBody", "response_body"],
    ]) {
      body.append(el("h3", msg(label)));
      let value = row[key];
      if (value == null) {
        body.append(el("small", msg("monitor.noBody")));
        continue;
      }
      try {
        value = JSON.stringify(JSON.parse(value), null, 2);
      } catch {
        /* Streaming or truncated preview. */
      }
      body.append(el("pre", value));
    }
  }
  const formatted = button(msg("monitor.formatted"), () => show(false));
  const json = button(msg("monitor.raw"), () => show(true));
  const copy = button(msg("monitor.copy"), async () => {
    await copyJSON(row);
    writeText(copy, msg("monitor.copied"));
  });
  tabs.append(formatted, json, copy);
  root.append(tabs, body);
  show(false);
  if (!root.open) root.showModal();
}
export async function logsView(root, ui, session, signal) {
  const { el, req, panel, button, table } = ui;
  const [controls, bar] = panel(root, msg("monitor.capture"));
  const stateText = el("small"),
    states = el("div", undefined, "toolbar");
  let config,
    index = 0,
    selected = 0,
    revision = 0,
    busy = false,
    selectionVersion = 0,
    currentRows = [];
  const apply = async (patch) => {
    config = await req("/admin/diagnostics/settings", {
      method: "PATCH",
      data: patch,
      signal,
    });
    syncConfig();
    await refresh();
  };
  const enabled = button("", () =>
    apply({ enabled: !config.enabled, paused: false }),
  );
  const paused = button("", () => apply({ paused: !config.paused }));
  const mode = button("", () => apply({ llm_only: !config.llm_only }));
  const settings = el("details"),
    capLabel = el("label", msg("monitor.capacity")),
    capacity = el("input");
  capacity.type = "number";
  capacity.min = "1";
  capacity.max = "10000";
  capLabel.append(capacity);
  const bodyLabel = el("label", msg("monitor.captureBodies"), "check"),
    capture = el("input");
  capture.type = "checkbox";
  bodyLabel.prepend(capture);
  const save = button(msg("monitor.save"), async () => {
    if (!capacity.reportValidity() || !Number.isInteger(capacity.valueAsNumber))
      return;
    if (
      capacity.valueAsNumber < config.capacity &&
      !confirm(text("monitor.confirmCapacity"))
    )
      return;
    await apply({
      capture_body: capture.checked,
      capacity: capacity.valueAsNumber,
    });
  });
  const setup = el("div", undefined, "toolbar");
  setup.append(capLabel, bodyLabel, save);
  settings.append(
    el("summary", msg("monitor.settings")),
    setup,
    el("small", msg("monitor.bodyHint")),
  );
  function syncConfig() {
    enabled.disabled = mode.disabled = save.disabled = false;
    writeText(
      enabled,
      msg(config.enabled ? "monitor.enabled" : "monitor.disabled"),
    );
    enabled.setAttribute("aria-pressed", String(!!config.enabled));
    writeText(paused, msg(config.paused ? "monitor.resume" : "monitor.pause"));
    paused.disabled = !config.enabled;
    writeText(
      mode,
      msg(config.llm_only ? "monitor.llmOnly" : "monitor.allApi"),
    );
    writeText(
      stateText,
      msg("monitor.retention", {
        size: number(config.size),
        capacity: number(config.capacity),
        dropped: number(config.dropped),
      }),
    );
    capacity.value = config.capacity;
    capture.checked = !!config.capture_body;
  }
  bar.append(stateText);
  states.append(enabled, paused, mode);
  controls.append(states, settings);
  const [list, tools] = panel(root, msg("logs.all"));
  list.classList.add("log-list");
  const searchLabel = el("label", msg("monitor.search")),
    search = el("input");
  search.type = "search";
  searchLabel.append(search);
  const [dirLabel, dir] = chooser(
    "monitor.direction",
    ["all", "ingress", "egress"].map((k) => [k, msg("monitor." + k)]),
    "all",
    el,
  );
  const [statusLabel, status] = chooser(
    "col.status",
    [
      ["", msg("monitor.all")],
      ["1", msg("monitor.failures")],
    ],
    "",
    el,
  );
  const filters = el("div", undefined, "toolbar filters");
  filters.append(searchLabel, dirLabel, statusLabel);
  if (session) {
    const chip = button(msg("logs.filtered", { id: session }), () => {
      session = "";
      chip.remove();
      return reset();
    });
    filters.append(chip);
  }
  const msgBox = el("p"),
    summary = el("div"),
    listBody = el("div"),
    paging = el("div", undefined, "toolbar");
  const detail = el("dialog", undefined, "log-detail");
  detail.addEventListener("close", () => {
    selected = 0;
    selectionVersion++;
  });
  const select = async (row) => {
    const version = ++selectionVersion;
    selected = row.seq;
    const data = await req("/admin/diagnostics/logs/" + row.seq, { signal });
    if (version !== selectionVersion || signal.aborted) return;
    drawDetail(detail, data, ui);
  };
  let timer, searchTimer;
  function resetDetail() {
    selected = 0;
    selectionVersion++;
    if (detail.open) detail.close();
  }
  async function refresh() {
    const version = ++revision;
    busy = true;
    const params = new URLSearchParams({
      offset: String(index * 50),
      session,
      direction: dir.value,
      errors: status.value,
      search: search.value,
    });
    try {
      const [result, latest] = await Promise.all([
        req("/admin/diagnostics/logs?" + params, { signal }),
        req("/admin/diagnostics/settings", { signal }),
      ]);
      if (version !== revision || signal.aborted) return;
      // Keep edits in the settings form while live rows are updating.
      const draftCapacity = capacity.value,
        draftBody = capture.checked;
      const preserveDraft =
        settings.open &&
        config &&
        (draftCapacity !== String(config.capacity) ||
          draftBody !== !!config.capture_body);
      config = latest;
      syncConfig();
      if (preserveDraft) {
        capacity.value = draftCapacity;
        capture.checked = draftBody;
      }
      if (index && !result.data.length) {
        index = Math.max(0, Math.ceil(result.total / 50) - 1);
        return refresh();
      }
      currentRows = result.data;
      listBody.replaceChildren();
      summary.replaceChildren();
      paging.replaceChildren();
      msgBox.replaceChildren();
      tiles(summary, pageMetrics(currentRows), el);
      table(
        listBody,
        [
          "col.time",
          "col.status",
          "monitor.direction",
          "monitor.account",
          "monitor.path",
          "col.model",
          "monitor.ttft",
          "monitor.speed",
          "monitor.cost",
          "col.latency",
          "col.actions",
        ].map((k) => msg(k)),
        currentRows,
        [
          (r) => dateTime(r.created),
          (r) => badge(el, r.status),
          (r) => direction(r.direction),
          (r) => r.account || "—",
          (r) => r.method + " " + r.path,
          "model",
          (r) => metric(r.ttft_ms, "ms"),
          (r) => metric(r.speed, "tok/s"),
          (r) => metric(r.cost, "$"),
          (r) => metric(r.latency_ms, "ms"),
          (r) => button(msg("monitor.detail"), () => select(r)),
        ],
      );
      const prev = button(msg("common.previous"), () => {
        index--;
        resetDetail();
        return refresh();
      });
      prev.disabled = !index;
      const next = button(msg("common.next"), () => {
        index++;
        resetDetail();
        return refresh();
      });
      next.disabled = (index + 1) * 50 >= result.total;
      paging.append(
        prev,
        el(
          "small",
          msg("monitor.range", {
            from: number(result.total ? index * 50 + 1 : 0),
            to: number(index * 50 + currentRows.length),
            n: number(result.total),
          }),
        ),
        next,
      );
      if (selected && !currentRows.some((r) => r.seq === selected))
        resetDetail();
    } catch (e) {
      if (!signal.aborted && version === revision)
        writeText(msgBox, e.localized || e.message);
    } finally {
      if (version === revision) busy = false;
    }
  }
  function reset() {
    index = 0;
    resetDetail();
    return refresh();
  }
  search.oninput = () => {
    clearTimeout(searchTimer);
    searchTimer = setTimeout(reset, 200);
  };
  dir.onchange = status.onchange = reset;
  tools.append(
    button(msg("common.refresh"), refresh),
    button(msg("monitor.export"), () => download(currentRows)),
    button(
      msg("monitor.clear"),
      async () => {
        if (confirm(text("monitor.confirmClear"))) {
          await req("/admin/diagnostics/logs", { method: "DELETE", signal });
          await reset();
        }
      },
      "warn",
    ),
  );
  list.append(
    filters,
    el("h3", msg("monitor.pageSummary")),
    summary,
    msgBox,
    listBody,
    paging,
  );
  root.append(detail);
  enabled.disabled = paused.disabled = mode.disabled = save.disabled = true;
  signal.addEventListener(
    "abort",
    () => {
      clearInterval(timer);
      clearTimeout(searchTimer);
    },
    { once: true },
  );
  await refresh();
  if (signal.aborted) return;
  timer = setInterval(() => {
    if (!document.hidden && !busy && !signal.aborted && !detail.open) refresh();
  }, 3000);
}
export async function errorsView(root, { el, button, panel, req }, signal) {
  const [box, bar] = panel(root, msg("monitor.errorGroups")),
    summary = el("p"),
    list = el("div");
  box.append(summary, list);
  bar.append(
    button(msg("common.refresh"), load),
    button(msg("monitor.seen"), async () => {
      await req("/admin/diagnostics/errors", { method: "POST", signal });
      await load();
    }),
    button(
      msg("monitor.clear"),
      async () => {
        if (confirm(text("monitor.confirmErrors"))) {
          await req("/admin/diagnostics/errors", { method: "DELETE", signal });
          await load();
        }
      },
      "warn",
    ),
  );
  let version = 0;
  const warning = el("p");
  box.append(warning);
  async function load() {
    const seq = ++version;
    const { data } = await req("/admin/diagnostics/errors", { signal });
    if (signal.aborted || seq !== version) return;
    warning.replaceChildren();
    const open = new Set(
      [...list.querySelectorAll("details[open]")].map(
        (node) => node.dataset.group,
      ),
    );
    const unread = data.reduce((n, g) => n + g.unread, 0);
    writeText(
      summary,
      msg("monitor.unread", { n: number(unread), groups: number(data.length) }),
    );
    list.replaceChildren();
    if (!data.length) list.append(el("p", msg("monitor.noErrors"), "empty"));
    for (const g of data) {
      const details = el("details"),
        title = el("summary");
      details.dataset.group = JSON.stringify([g.source, g.code, g.status]);
      details.open = open.has(details.dataset.group);
      title.append(
        badge(el, g.status),
        el("strong", g.code),
        el("span", g.source + " · ×" + g.occurrences),
      );
      details.append(
        title,
        el("p", g.message || g.code),
        el(
          "p",
          msg("monitor.errorTime", {
            first: dateTime(g.first_seen),
            last: dateTime(g.last_seen),
          }),
        ),
        el(
          "pre",
          JSON.stringify(
            {
              request_id: g.request_id,
              path: g.path,
              model: g.model,
              unread: g.unread,
            },
            null,
            2,
          ),
        ),
      );
      list.append(details);
    }
  }
  const timer = setInterval(() => {
    if (!document.hidden && !signal.aborted)
      load().catch((e) => {
        if (!signal.aborted) writeText(warning, e.localized || e.message);
      });
  }, 30000);
  signal.addEventListener("abort", () => clearInterval(timer), { once: true });
  await load();
}
