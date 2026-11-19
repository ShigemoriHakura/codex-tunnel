import { msg, text, number, percent, dateTime } from "./i18n.mjs";
import { metric } from "./monitor.mjs";
import { copyAddress, publicURLs } from "./site.mjs";

const surface =
  "min-w-0 rounded-xl border border-solid border-[var(--line)] bg-[var(--surface)] p-4";
const caption = "m-0 text-xs text-[var(--muted)]";
const sectionLabel = "m-0 mb-2 text-xs font-medium text-[var(--muted)]";
const periods = [
  ["24", "board.day"],
  ["168", "board.week"],
  ["720", "board.month"],
  ["all", "trend.allTime"],
];
const kinds = [
  ["", "board.allKinds"],
  ["chat", "board.chat"],
  ["image", "board.images"],
];

function trafficChart(parent, report, { el }, life) {
  const shell = el(
    "div",
    undefined,
    "rounded-[10px] border border-solid border-[var(--line)] bg-[var(--soft)] px-3 py-2",
  );
  const legend = el(
    "div",
    undefined,
    "flex flex-wrap items-center gap-x-4 gap-y-1 text-xs",
  );
  for (const [label, cls] of [
    ["board.completed", "bg-sky-600"],
    ["board.failed", "bg-rose-600"],
    ["board.avgLatency", "bg-[var(--ink)]"],
  ]) {
    const line = el("span", undefined, "inline-flex items-center gap-1.5");
    line.append(
      el("i", undefined, "inline-block h-2 w-2 rounded-sm " + cls),
      el("span", msg(label)),
    );
    legend.append(line);
  }
  const canvas = el("canvas", undefined, "block h-[184px] w-full");
  const detail = el("small", undefined, "block min-h-5 tabular-nums");
  canvas.tabIndex = 0;
  canvas.setAttribute("role", "img");
  shell.append(legend, canvas, detail);
  parent.append(shell);
  const { step, start, end, buckets } = report;
  const first = Math.floor(start / step) * step,
    last = Math.floor(end / step) * step;
  const length = 1 + Math.round((last - first) / step);

  const columnCount = Math.min(30, length);
  const columns = Array.from({ length: columnCount }, (_, index) => ({
    at: first + Math.ceil((index * length) / columnCount) * step,
    requests: 0,
    errors: 0,
    latencySum: 0,
  }));
  for (const b of buckets) {
    const index = Math.min(
      columnCount - 1,
      Math.floor((((b.bucket - first) / step) * columnCount) / length),
    );
    if (index < 0) continue;
    const col = columns[index];
    col.requests += b.requests;
    col.errors += b.errors;
    col.latencySum += b.requests * (b.latency_ms || 0);
  }
  const ceiling = Math.max(1, ...columns.map((b) => b.requests));
  const maxMs = Math.max(
    1,
    ...columns.map((b) => b.latencySum / (b.requests || 1)),
  );
  let selected = columns.findLastIndex((b) => b.requests);
  if (selected < 0) selected = columnCount - 1;
  const pad = 38;
  function draw() {
    const width = canvas.clientWidth,
      height = canvas.clientHeight;
    if (width < 90 || !height) return;
    const ratio = window.devicePixelRatio || 1;
    canvas.width = Math.round(width * ratio);
    canvas.height = Math.round(height * ratio);
    const c = canvas.getContext("2d");
    c.scale(ratio, ratio);
    const style = getComputedStyle(shell);
    const top = 18,
      baseline = height - 25,
      area = baseline - top;
    const space = (width - 2 * pad) / columnCount;
    const x = (i) => pad + (i + 0.5) * space;
    const fmt = (n) =>
      Intl.NumberFormat(document.documentElement.lang, {
        notation: "compact",
        maximumFractionDigits: 1,
      }).format(n);
    c.font = "11px system-ui";
    c.fillStyle = style.color;
    for (let i = 0; i <= 2; i++) {
      const y = baseline - (i * area) / 2;
      c.strokeStyle = style.borderTopColor;
      c.beginPath();
      c.moveTo(pad, y);
      c.lineTo(width - pad, y);
      c.stroke();
      c.fillStyle = style.color;
      c.textAlign = "right";
      c.fillText(fmt(Math.round((ceiling * i) / 2)), pad - 6, y + 3);
      c.textAlign = "left";
      c.fillText(fmt(Math.round((maxMs * i) / 2)), width - pad + 5, y + 3);
    }
    const barWidth = Math.max(2, Math.min(18, space * 0.65));
    columns.forEach((b, i) => {
      const h = (b.requests / ceiling) * area;
      c.fillStyle = "#0284c7";
      c.fillRect(x(i) - barWidth / 2, baseline - h, barWidth, h);
      if (b.errors) {
        c.fillStyle = "#e11d48";
        c.fillRect(
          x(i) - barWidth / 2,
          baseline - h,
          barWidth,
          (b.errors / ceiling) * area,
        );
      }
    });
    c.strokeStyle = style.color;
    c.lineWidth = 1.5;
    c.beginPath();
    let linked = false;
    columns.forEach((b, i) => {
      if (!b.requests) {
        linked = false;
        return;
      }
      const y = baseline - (b.latencySum / b.requests / maxMs) * area;
      if (linked) c.lineTo(x(i), y);
      else c.moveTo(x(i), y);
      linked = true;
    });
    c.stroke();
    columns.forEach((b, i) => {
      if (!b.requests) return;
      c.fillStyle = style.color;
      c.beginPath();
      c.arc(
        x(i),
        baseline - (b.latencySum / b.requests / maxMs) * area,
        2.5,
        0,
        Math.PI * 2,
      );
      c.fill();
    });
    c.beginPath();
    c.setLineDash([3, 4]);
    c.moveTo(x(selected), top);
    c.lineTo(x(selected), baseline);
    c.stroke();
    c.setLineDash([]);
    const stamp = (t) =>
      new Date(t * 1000).toLocaleString(
        document.documentElement.lang,
        step >= 86400
          ? { month: "short", day: "numeric" }
          : {
              hour: "2-digit",
              minute: "2-digit",
              month: "numeric",
              day: "numeric",
            },
      );
    c.textAlign = "left";
    c.fillText(stamp(first), pad, baseline + 18);
    if (first !== last) {
      c.textAlign = "right";
      c.fillText(stamp(last), width - pad, baseline + 18);
    }
    const col = columns[selected];
    const summary = text("board.chartPoint", {
      time: dateTime(col.at),
      n: number(col.requests),
      errors: number(col.errors),
      ms: col.requests
        ? number(Math.round(col.latencySum / col.requests))
        : "—",
    });
    detail.textContent = summary;
    canvas.setAttribute("aria-label", summary);
  }
  canvas.onpointermove = (e) => {
    selected = Math.max(
      0,
      Math.min(
        columnCount - 1,
        Math.floor(
          ((e.clientX - canvas.getBoundingClientRect().left - pad) /
            (canvas.clientWidth - 2 * pad)) *
            columnCount,
        ),
      ),
    );
    draw();
  };
  canvas.onkeydown = (e) => {
    if (!["ArrowLeft", "ArrowRight", "Home", "End"].includes(e.key)) return;
    e.preventDefault();
    selected =
      e.key === "Home"
        ? 0
        : e.key === "End"
          ? columnCount - 1
          : Math.max(
              0,
              Math.min(
                columnCount - 1,
                selected + (e.key === "ArrowLeft" ? -1 : 1),
              ),
            );
    draw();
  };
  const resize = new ResizeObserver(draw);
  resize.observe(canvas);
  const changes = new MutationObserver(draw);
  changes.observe(document.documentElement, {
    attributes: true,
    attributeFilter: ["lang", "data-theme"],
  });
  matchMedia("(prefers-color-scheme: dark)").addEventListener("change", draw, {
    signal: life,
  });
  life.addEventListener(
    "abort",
    () => {
      resize.disconnect();
      changes.disconnect();
    },
    { once: true },
  );
  draw();
}

export async function dashboardView(root, ui, signal) {
  const { el, req, button, navigate, logTable, notice } = ui;
  let hours = "24",
    kind = "",
    model = "",
    models = [],
    serial = 0,
    charts,
    railOpen = matchMedia("(min-width: 1440px)").matches;
  signal.addEventListener("abort", () => charts?.abort(), { once: true });
  let snapshot;
  const group = (rail, title) => {
    const g = el(
      "div",
      undefined,
      "border-0 border-t border-solid border-[var(--line)] py-3",
    );
    g.append(el("h3", msg(title), sectionLabel));
    rail.append(g);
    return g;
  };
  function choice(box, label, active, fn) {
    const b = button(
      label,
      fn,
      "w-full border-transparent bg-transparent text-left hover:bg-[var(--soft)]",
    );
    b.setAttribute("aria-pressed", String(active));
    box.append(b);
  }
  async function refresh(updateStatus = false) {
    const turn = ++serial;
    const step = hours === "24" ? 3600 : 86400;
    const q = new URLSearchParams({ hours, step, kind, model, recent: "1" });
    const [data, status] = await Promise.all([
      req("/admin/analytics?" + q, { signal }),
      updateStatus || !snapshot
        ? Promise.all([
            req("/admin/site", { signal }),
            req("/admin/diagnostics/settings", { signal }),
            req("/admin/diagnostics/errors", { signal }),
            req("/admin/memory", { signal }),
          ])
        : snapshot,
    ]);
    if (turn !== serial || signal.aborted) return;
    snapshot = status;
    const [settings, logConfig, errorList, memory] = status;
    const errors = errorList.data.reduce((n, g) => n + g.unread, 0);
    if (!model) models = data.models.filter((m) => m.model).slice(0, 4);
    charts?.abort();
    charts = new AbortController();
    root.replaceChildren();
    root.className =
      "grid items-start gap-4 min-[1440px]:grid-cols-[240px_minmax(0,1fr)]";
    const rail = el("details", undefined, surface + " m-0");
    rail.open = railOpen;
    rail.ontoggle = () => {
      railOpen = rail.open;
    };
    const railTitle = el(
      "summary",
      msg("board.scope"),
      "text-sm font-medium text-[var(--ink)]",
    );
    rail.append(railTitle);
    const workloads = group(rail, "board.workload");
    for (const [key, title] of kinds)
      choice(workloads, msg(title), kind === key, () => {
        kind = key;
        model = "";
        return refresh();
      });
    const selection = group(rail, "board.modelScope");
    choice(selection, msg("board.allModels"), !model, () => {
      model = "";
      return refresh();
    });
    for (const row of models)
      choice(selection, row.model, model === row.model, () => {
        model = row.model;
        return refresh();
      });
    const queues = group(rail, "board.inspect");
    for (const [page, title, count] of [
      ["logs", "nav.logs", logConfig.size],
      ["errors", "nav.errors", errors],
    ]) {
      const b = button(
        msg(title),
        () => navigate(page),
        "flex w-full items-center justify-between border-transparent bg-transparent text-left hover:bg-[var(--soft)]",
      );
      b.append(el("span", number(count), "text-xs tabular-nums"));
      queues.append(b);
    }
    const board = el("div", undefined, "grid min-w-0 gap-4");
    root.append(rail, board);
    const heading = el("div"),
      strip = el("div", undefined, "mb-3 flex flex-wrap gap-1.5");
    for (const [key, title] of periods) {
      const b = button(
        msg(title),
        () => {
          hours = key;
          model = "";
          return refresh();
        },
        "px-3",
      );
      b.setAttribute("aria-pressed", String(hours === key));
      strip.append(b);
    }
    heading.append(
      strip,
      el("h1", msg("nav.overview")),
      el("p", msg("board.subtitle"), "m-0 mt-1 text-sm"),
    );
    board.append(heading);
    const s = data.summary;
    const displayTokens = (n) =>
      s.requests && s.unmetered === s.requests
        ? msg("usage.unknown")
        : number(n);
    const value = (v, unit = "") =>
      v == null ? "—" : () => number(Math.round(v))() + unit;
    const metrics = [
      [
        "usage.requests",
        number(s.requests),
        msg("board.activeKeys", { n: number(s.active_keys) }),
      ],
      [
        "usage.input",
        displayTokens(s.input),
        msg("board.cached", { n: value(s.cached) }),
      ],
      [
        "usage.output",
        displayTokens(s.output),
        msg("board.reasoning", { n: value(s.reasoning) }),
      ],
      [
        "trend.avgTps",
        metric(s.avg_tps, "tok/s"),
        msg("board.tpsSamples", { n: number(s.tps_samples) }),
      ],
      [
        "monitor.success",
        s.requests ? percent(100 * (1 - s.errors / s.requests)) : "—",
        msg("board.failedCount", { n: number(s.errors) }),
      ],
      [
        "board.avgLatency",
        value(s.latency_ms, " ms"),
        msg("board.ttft", { n: value(s.ttft_ms, " ms") }),
      ],
    ];
    const stripKpi = el(
      "div",
      undefined,
      "grid grid-cols-2 gap-px overflow-hidden rounded-xl border border-solid border-[var(--line)] bg-[var(--line)] sm:grid-cols-3 xl:grid-cols-6",
    );
    for (const [label, v, hint] of metrics) {
      const k = el(
        "div",
        undefined,
        "grid min-h-[110px] min-w-0 content-center gap-1 bg-[var(--surface)] p-4",
      );
      k.append(
        el("small", msg(label), caption),
        el(
          "strong",
          v,
          "break-words text-[24px] font-medium leading-tight tracking-tight tabular-nums",
        ),
        el("small", hint, caption),
      );
      stripKpi.append(k);
    }
    board.append(stripKpi);
    const analysis = el(
      "div",
      undefined,
      "grid min-w-0 items-start gap-4 lg:grid-cols-[minmax(0,1.85fr)_minmax(0,.95fr)]",
    );
    board.append(analysis);
    const traffic = el("div", undefined, surface + " grid gap-3");
    traffic.append(
      el("h2", msg("board.traffic")),
      el("small", msg("board.chartHint"), caption),
    );
    if (s.requests) trafficChart(traffic, data, ui, charts.signal);
    else traffic.append(el("p", msg("common.empty"), "empty"));
    const byModel = el("div", undefined, "grid gap-2");
    byModel.append(el("h3", msg("trend.models"), "m-0 text-sm font-medium"));
    for (const row of data.models.slice(0, 4)) {
      const line = el(
        "div",
        undefined,
        "grid grid-cols-[minmax(0,1fr)_auto] items-center gap-2 rounded-[10px] border border-solid border-[var(--line)] px-3 py-2",
      );
      line.append(
        el("span", row.model || msg("usage.unknown"), "truncate text-sm"),
        el(
          "small",
          msg("board.share", {
            n: number(row.requests),
            share: percent((100 * row.requests) / s.requests),
          }),
          caption,
        ),
      );
      byModel.append(line);
    }
    if (data.models.length) traffic.append(byModel);
    const info = el("div", undefined, surface + " grid gap-3");
    const statusHeading = el("div", undefined, "bar");
    statusHeading.append(
      el("h2", msg("board.status")),
      button(msg("common.refresh"), () => refresh(true)),
    );
    info.append(statusHeading);
    const lines = [
      [
        "board.collection",
        msg(
          logConfig.enabled
            ? logConfig.paused
              ? "board.paused"
              : "board.running"
            : "monitor.disabled",
        ),
      ],
      [
        "board.retained",
        () => number(logConfig.size)() + " / " + number(logConfig.capacity)(),
      ],
      ["board.unread", number(errors)],
      [
        "board.rss",
        memory.rssBytes == null
          ? "—"
          : () =>
              number(Math.round((memory.rssBytes / 1048576) * 10) / 10)() +
              " MiB",
      ],
      [
        "board.heap",
        memory.heapBytes == null
          ? "—"
          : () =>
              number(Math.round((memory.heapBytes / 1048576) * 10) / 10)() +
              " MiB",
      ],
      [
        "board.coverage",
        s.requests ? percent(100 * (1 - s.unmetered / s.requests)) : "—",
      ],
    ];
    const dl = el("dl", undefined, "m-0 grid gap-0");
    for (const [label, v] of lines) {
      const line = el(
        "div",
        undefined,
        "flex min-h-9 items-center justify-between gap-3 border-0 border-b border-solid border-[var(--line)] text-sm",
      );
      line.append(
        el("dt", msg(label)),
        el("dd", v, "m-0 font-medium tabular-nums"),
      );
      dl.append(line);
    }
    info.append(dl);
    const url = publicURLs(settings).apiURL;
    const address = el("code", url, "endpoint");
    info.append(
      el("h3", msg("overview.endpoint"), "m-0 text-sm font-medium"),
      address,
    );
    const actions = el("div", undefined, "flex flex-wrap gap-2");
    actions.append(
      button(msg("overview.copy"), async () =>
        notice(
          msg(
            (await copyAddress(url, address))
              ? "overview.copied"
              : "overview.selected",
          ),
        ),
      ),
      button(msg("nav.site"), () => navigate("site")),
    );
    info.append(actions);
    analysis.append(traffic, info);
    const events = el("div", undefined, surface),
      bar = el("div", undefined, "bar");
    bar.append(
      el("h2", msg("usage.recent")),
      button(msg("usage.all"), () => navigate("logs")),
    );
    events.append(bar);
    logTable(events, data.recent);
    board.append(events);
  }
  await refresh();
}
