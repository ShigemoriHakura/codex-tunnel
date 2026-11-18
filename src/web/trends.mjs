import { msg, text, number, dateTime, writeText } from "./i18n.mjs";
import { chooser, metric, tiles } from "./monitor.mjs";

const periods = [1, 6, 24, 72, 168, 720, 2160].map((hours) => [
  String(hours),
  hours <= 24
    ? msg("quota.hours", { n: hours })
    : msg("quota.days", { n: hours / 24 }),
]);
periods.push(["all", msg("trend.allTime")]);
const colors = ["#428ce1", "#12a780", "#b47ce3"];
const tokenSeries = [
  ["input", "usage.input"],
  ["output", "usage.output"],
  ["cached", "monitor.cached"],
];
function plot(root, data, series, label, ui, signal) {
  const { el } = ui;
  const wrap = el("div", undefined, "trend-chart"),
    legends = el("div", undefined, "chart-legend");
  for (const [i, [, key]] of series.entries()) {
    const title = el("span", msg(key));
    title.style.borderColor = colors[i];
    legends.append(title);
  }
  const canvas = el("canvas"),
    readout = el("small");
  canvas.tabIndex = 0;
  canvas.setAttribute("role", "img");
  wrap.append(el("h3", msg(label)), legends, canvas, readout);
  root.append(wrap);
  const first = Math.floor(data.start / data.step) * data.step;
  const last = Math.floor(data.end / data.step) * data.step;
  const slots = Math.max(1, Math.round((last - first) / data.step) + 1);
  const pts = new Map(data.buckets.map((v) => [v.bucket, v]));
  function sample(index, key) {
    const p = pts.get(first + index * data.step);
    if (!p) return key === "hit_rate" ? null : 0;
    if ((key === "input" || key === "output") && p.requests === p.unmetered)
      return null;
    return p[key];
  }
  let index = slots - 1;
  const max =
    series[0][0] === "hit_rate"
      ? 100
      : Math.max(
          1,
          ...series.map(([k]) =>
            data.buckets.reduce((v, b) => Math.max(v, b[k] || 0), 0),
          ),
        );
  const left = 66,
    right = 16,
    top = 12,
    bottom = 30;
  function draw() {
    const width = canvas.clientWidth,
      height = canvas.clientHeight;
    if (!width || !height) return;
    const ratio = window.devicePixelRatio || 1;
    canvas.width = Math.round(width * ratio);
    canvas.height = Math.round(height * ratio);
    const c = canvas.getContext("2d");
    c.scale(ratio, ratio);
    const css = getComputedStyle(wrap);
    const x = (i) =>
      left + (slots === 1 ? 0.5 : i / (slots - 1)) * (width - left - right);
    const y = (v) => height - bottom - (v / max) * (height - top - bottom);
    c.lineWidth = 1;
    c.font = "11px system-ui";
    c.textBaseline = "middle";
    c.textAlign = "right";
    for (let i = 0; i < 4; i++) {
      const v = (max * i) / 3;
      // Computed 'color' resolves CSS light-dark(), including on canvas.
      c.strokeStyle = getComputedStyle(canvas).borderTopColor;
      c.beginPath();
      c.moveTo(left, y(v));
      c.lineTo(width - right, y(v));
      c.stroke();
      c.fillStyle = css.color;
      const tick =
        series[0][0] === "hit_rate"
          ? Math.round(v) + "%"
          : new Intl.NumberFormat(document.documentElement.lang, {
              notation: "compact",
              maximumFractionDigits: 1,
            }).format(v);
      c.fillText(tick, left - 6, y(v));
    }
    c.fillStyle = css.color;
    c.textAlign = "left";
    c.fillText(
      new Date(first * 1000).toLocaleString(document.documentElement.lang, {
        month: "short",
        day: "numeric",
        hour: "2-digit",
        minute: "2-digit",
      }),
      left,
      height - 10,
    );
    c.textAlign = "right";
    c.fillText(
      new Date(last * 1000).toLocaleString(document.documentElement.lang, {
        month: "short",
        day: "numeric",
        hour: "2-digit",
        minute: "2-digit",
      }),
      width - right,
      height - 10,
    );
    series.forEach(([key], n) => {
      c.strokeStyle = colors[n];
      c.lineWidth = 2;
      c.lineCap = "round";
      c.beginPath();
      let connected = false;
      // Draw a min/max envelope at pixel resolution even for multi-year history.
      const columns = Math.min(
        slots,
        Math.max(2, Math.floor(width - left - right)),
      );
      for (let col = 0; col < columns; col++) {
        const from = Math.floor((col * slots) / columns),
          end = Math.floor(((col + 1) * slots) / columns);
        let lo = Infinity,
          hi = -Infinity;
        for (let k = from; k < end; k++) {
          const v = sample(k, key);
          if (v != null) {
            lo = Math.min(lo, v);
            hi = Math.max(hi, v);
          }
        }
        if (hi === -Infinity) {
          connected = false;
          continue;
        }
        const at = x(from);
        if (!connected) c.moveTo(at, y(lo));
        else c.lineTo(at, y(lo));
        c.lineTo(at + 0.25, y(hi));
        connected = true;
      }
      c.stroke();
      const value = sample(index, key);
      if (value != null) {
        c.fillStyle = colors[n];
        c.beginPath();
        c.arc(x(index), y(value), 3.5, 0, 2 * Math.PI);
        c.fill();
      }
    });
    c.strokeStyle = css.color;
    c.lineWidth = 1;
    c.setLineDash([2, 4]);
    c.beginPath();
    c.moveTo(x(index), top);
    c.lineTo(x(index), height - bottom);
    c.stroke();
    const stamp = first + index * data.step;
    const description =
      dateTime(stamp)() +
      " · " +
      series
        .map(
          ([k, title]) =>
            text(title) +
            " " +
            metric(sample(index, k), k === "hit_rate" ? "%" : "")(),
        )
        .join(" / ");
    readout.textContent = description;
    canvas.setAttribute("aria-label", text(label) + " · " + description);
  }
  canvas.onpointermove = (e) => {
    const rect = canvas.getBoundingClientRect();
    index = Math.max(
      0,
      Math.min(
        slots - 1,
        Math.round(
          ((e.clientX - rect.left - left) / (rect.width - left - right)) *
            (slots - 1),
        ),
      ),
    );
    draw();
  };
  canvas.onkeydown = (e) => {
    if (!["ArrowLeft", "ArrowRight", "Home", "End"].includes(e.key)) return;
    e.preventDefault();
    index =
      e.key === "Home"
        ? 0
        : e.key === "End"
          ? slots - 1
          : Math.max(
              0,
              Math.min(slots - 1, index + (e.key === "ArrowLeft" ? -1 : 1)),
            );
    draw();
  };
  const resize = new ResizeObserver(draw);
  resize.observe(canvas);
  const observer = new MutationObserver(draw);
  observer.observe(document.documentElement, {
    attributes: true,
    attributeFilter: ["lang", "data-theme"],
  });
  const scheme = matchMedia("(prefers-color-scheme: dark)");
  scheme.addEventListener("change", draw, { signal });
  signal.addEventListener(
    "abort",
    () => {
      resize.disconnect();
      observer.disconnect();
    },
    { once: true },
  );
  draw();
}
export async function usageView(root, ui, signal) {
  const { el, button, panel, table, req } = ui;
  const [controls, bar] = panel(root, msg("trend.range"));
  const fields = el("div", undefined, "toolbar filters");
  const [periodLabel, period] = chooser("trend.period", periods, "24", el);
  const [stepLabel, step] = chooser(
    "trend.granularity",
    [
      ["300", msg("trend.5min")],
      ["3600", msg("trend.hourly")],
      ["86400", msg("trend.daily")],
    ],
    "3600",
    el,
  );
  const scope = el("div", undefined, "toolbar");
  fields.append(periodLabel, stepLabel);
  controls.append(fields, scope);
  const results = el("div"),
    notice = el("p");
  root.append(notice, results);
  let model = "",
    owner = "",
    ownerName = "",
    version = 0,
    chartLife;
  signal.addEventListener("abort", () => chartLife?.abort(), { once: true });
  let loading = false;
  async function refresh(background = false) {
    loading = true;
    const current = ++version;
    const q = new URLSearchParams({
      hours: period.value,
      step: step.value,
      model,
      owner,
    });
    if (!background) writeText(notice, msg("common.loading"));
    try {
      const data = await req("/admin/analytics?" + q, { signal });
      if (signal.aborted || current !== version) return;
      chartLife?.abort();
      chartLife = new AbortController();
      notice.replaceChildren();
      scope.replaceChildren();
      results.replaceChildren();
      if (model)
        scope.append(
          button(model + " ×", () => {
            model = "";
            return refresh();
          }),
        );
      if (owner)
        scope.append(
          button((ownerName || owner) + " ×", () => {
            owner = "";
            return refresh();
          }),
        );
      const s = data.summary;
      const [summary] = panel(results, msg("trend.totals"));
      tiles(
        summary,
        [
          ["usage.requests", metric(s.requests)],
          [
            "monitor.success",
            metric(s.requests ? 100 * (1 - s.errors / s.requests) : null, "%"),
          ],
          ["trend.errors", metric(s.errors)],
          [
            "usage.input",
            metric(s.requests && s.unmetered === s.requests ? null : s.input),
          ],
          [
            "usage.output",
            metric(s.requests && s.unmetered === s.requests ? null : s.output),
          ],
          ["monitor.cached", metric(s.cached)],
          ["monitor.reasoning", metric(s.reasoning)],
          ["trend.rangeHit", metric(s.hit_rate, "%")],
          ["trend.lifeHit", metric(data.lifetime.hit_rate, "%")],
          ["monitor.cost", metric(s.cost, "$")],
          ["trend.keys", metric(s.active_keys)],
          [
            "trend.accounts",
            metric(s.requests && !s.active_accounts ? null : s.active_accounts),
          ],
          [
            "trend.imageReq",
            () => number(s.image_ok)() + " / " + number(s.image_failed)(),
          ],
          [
            "trend.imageTurn",
            () =>
              metric(s.image_ok + s.image_failed ? s.image_turn_input : 0)() +
              " / " +
              metric(s.image_ok + s.image_failed ? s.image_turn_output : 0)(),
          ],
        ],
        el,
      );
      summary.append(
        el(
          "small",
          msg("trend.coverage", {
            n: number(s.priced),
            all: number(s.requests),
            unknown: number(s.unmetered),
          }),
        ),
        el("p", msg("trend.priceHint"), "hint"),
      );
      const [chart] = panel(results, msg("trend.history"));
      if (s.requests) {
        plot(
          chart,
          data,
          tokenSeries,
          "trend.tokenHistory",
          ui,
          chartLife.signal,
        );
        plot(
          chart,
          data,
          [
            ["requests", "usage.requests"],
            ["errors", "trend.errors"],
          ],
          "trend.requestHistory",
          ui,
          chartLife.signal,
        );
        plot(
          chart,
          data,
          [["hit_rate", "trend.rangeHit"]],
          "trend.cacheHistory",
          ui,
          chartLife.signal,
        );
      } else chart.append(el("p", msg("common.empty"), "empty"));
      for (const [title, entries, byModel] of [
        ["trend.models", data.models, true],
        ["trend.byKey", data.keys, false],
      ]) {
        const [group] = panel(results, msg(title));
        table(
          group,
          [
            msg(byModel ? "col.model" : "col.owner"),
            msg("usage.requests"),
            msg("usage.input"),
            msg("usage.output"),
            msg("monitor.cached"),
            msg("trend.errors"),
            msg("monitor.cost"),
          ],
          entries,
          [
            (r) => {
              const label = byModel ? r.model : r.name || r.owner;
              if (!label) return "—";
              return button(label, () => {
                if (byModel) model = r.model;
                else {
                  owner = r.owner;
                  ownerName = r.name;
                }
                return refresh();
              });
            },
            "requests",
            (r) => metric(r.unmetered === r.requests ? null : r.input),
            (r) => metric(r.unmetered === r.requests ? null : r.output),
            (r) => metric(r.cached),
            "errors",
            (r) => metric(r.cost, "$"),
          ],
        );
      }
    } catch (e) {
      if (!signal.aborted && current === version)
        writeText(notice, e.localized || e.message);
    } finally {
      if (current === version) loading = false;
    }
  }
  step.onchange = () => {
    if (step.value === "300" && (period.value === "all" || +period.value > 24))
      period.value = "24";
    if (step.value === "86400" && period.value !== "all" && +period.value <= 24)
      period.value = "72";
    if (step.value === "3600" && period.value === "1") period.value = "24";
    return refresh();
  };
  period.onchange = () => {
    if (period.value === "1") step.value = "300";
    else if (
      (period.value === "all" || +period.value > 24) &&
      step.value === "300"
    )
      step.value = "3600";
    else if (
      period.value !== "all" &&
      +period.value <= 24 &&
      step.value === "86400"
    )
      step.value = "3600";
    return refresh();
  };
  bar.append(button(msg("common.refresh"), refresh));
  const timer = setInterval(() => {
    if (
      !document.hidden &&
      !signal.aborted &&
      !loading &&
      !results.contains(document.activeElement)
    )
      refresh(true);
  }, 60000);
  signal.addEventListener("abort", () => clearInterval(timer), { once: true });
  await refresh();
}
