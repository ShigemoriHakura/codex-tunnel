import { logsView, errorsView } from "./monitor.mjs";
import { usageView } from "./trends.mjs";
import {
  msg,
  text,
  writeText,
  initLanguage,
  number,
  dateTime,
  percent,
  hasMessage,
  uiError,
} from "./i18n.mjs";

const $ = (id) => document.getElementById(id);
const pages = ["overview", "keys", "sessions", "stats", "logs", "errors", "models", "cli", "play"];
initLanguage();
const themePicker = $("theme");
function applyTheme(value) {
  const theme = ["light", "dark"].includes(value) ? value : "system";
  document.documentElement.dataset.theme = theme;
  themePicker.value = theme;
}
try { applyTheme(localStorage.getItem("desk-tunnel.theme")); } catch { applyTheme("system"); }
themePicker.onchange = () => {
  applyTheme(themePicker.value);
  try { localStorage.setItem("desk-tunnel.theme", themePicker.value); } catch { /* Preference lasts for this visit. */ }
};

let admin = "",
  page = "overview",
  offset = 0,
  filter = "",
  epoch = 0;
function el(tag, text, cls) {
  const n = document.createElement(tag);
  if (text !== undefined) writeText(n, text);
  if (cls) n.className = cls;
  return n;
}
function notice(s) {
  writeText($("notice"), s);
}
function button(s, fn, cls) {
  const b = el("button", s, cls);
  b.type = "button";
  b.onclick = async () => {
    const at = epoch;
    b.disabled = true;
    try {
      await fn();
    } catch (e) {
      if (at === epoch) notice(e.localized || e.message);
    } finally {
      b.disabled = false;
    }
  };
  return b;
}
async function req(path, { method = "GET", data, key = admin, signal } = {}) {
  return requestJSON(path, {
    method,
    signal,
    headers: {
      Authorization: "Bearer " + key,
      ...(data ? { "Content-Type": "application/json" } : {}),
    },
    body: data ? JSON.stringify(data) : undefined,
  });
}
async function requestJSON(path, options) {
  let r;
  try {
    r = await fetch(path, options);
  } catch {
    throw uiError(msg("error.network"));
  }
  let j;
  try {
    j = await r.json();
  } catch {
    throw uiError(msg("error.response"), r.status);
  }
  if (!r.ok) {
    const key = `error.${j?.error?.code}`;
    throw uiError(
      hasMessage(key) ? msg(key) : j?.error?.message || msg("error.request"),
      r.status,
    );
  }
  return j;
}
function field(label, type = "text", initial = "") {
  const wrap = el("label", label),
    input = el(type === "textarea" ? "textarea" : "input");
  if (type !== "textarea") input.type = type;
  input.value = initial;
  wrap.append(input);
  return [wrap, input];
}
function panel(root, title) {
  const s = el("section"),
    bar = el("div", undefined, "bar");
  bar.append(el("h2", title));
  s.append(bar);
  root.append(s);
  return [s, bar];
}
function table(root, heads, items, cols) {
  if (!items.length) {
    root.append(el("p", msg("common.empty"), "empty"));
    return;
  }
  const t = el("table"),
    tr = el("tr");
  for (const h of heads) {
    const cell = el("th", h);
    cell.scope = "col";
    tr.append(cell);
  }
  t.createTHead().append(tr);
  const body = t.createTBody();
  for (const i of items) {
    const r = body.insertRow();
    for (const c of cols) {
      const v = typeof c === "function" ? c(i) : i[c],
        td = r.insertCell();
      if (v instanceof Node) td.append(v);
      else writeText(td, v ?? "—");
    }
  }
  const scroll = el("div", undefined, "table-scroll");
  scroll.append(t);
  root.append(scroll);
}
const short = (s) => (s ? s.slice(0, 13) + "…" : "—");
function pager(root, len) {
  const p = el("div", undefined, "toolbar");
  if (offset > 0)
    p.append(
      button(msg("common.previous"), () => {
        offset -= 100;
        return render();
      }),
    );
  p.append(el("small", msg("common.page", { page: number(offset / 100 + 1) })));
  if (len === 100)
    p.append(
      button(msg("common.next"), () => {
        offset += 100;
        return render();
      }),
    );
  root.append(p);
}
function goto(p) {
  page = p;
  offset = 0;
  if (p !== "logs") filter = "";
  return render();
}
let viewLife;
async function render() {
  viewLife?.abort();
  viewLife = new AbortController();
  const current = ++epoch;
  notice("");
  writeText($("title"), msg(`nav.${page}`));
  writeText($("desc"), msg(`desc.${page}`));
  $("pagePicker").value = page;
  for (const entry of $("nav").children) {
    const selected = entry.dataset.page === page;
    entry.classList.toggle("active", selected);
    if (selected) entry.setAttribute("aria-current", "page");
    else entry.removeAttribute("aria-current");
  }
  const root = el("div");
  $("content").replaceChildren(root);
  const status = el("p", msg("common.loading"));
  root.append(status);
  try {
    await views[page](root, viewLife.signal);
  } catch (e) {
    if (current === epoch) notice(e.localized || e.message);
  } finally {
    status.remove();
  }
}
const navSymbols = {overview: "▦", stats: "▥", errors: "!", keys: "⚿", sessions: "▤", logs: "≡", models: "◫", cli: "›_", play: "▷"};
function navigate(page) {
  filter = "";
  return goto(page);
}
for (const key of pages) {
  const entry = button(msg(`nav.${key}`), () => navigate(key));
  const icon = el("span", navSymbols[key], "nav-icon");
  icon.setAttribute("aria-hidden", "true");
  entry.prepend(icon);
  entry.dataset.page = key;
  $("nav").append(entry);
  const option = el("option", msg(`nav.${key}`));
  option.value = key;
  $("pagePicker").append(option);
}
$("pagePicker").onchange = () => navigate($("pagePicker").value);

function logTable(root, items) {
  table(
    root,
    [
      msg("col.time"),
      msg("col.keyKind"),
      msg("col.model"),
      msg("col.status"),
      msg("col.usage"),
      msg("col.latency"),
    ],
    items,
    [
      (i) => dateTime(i.created),
      (i) => (i.key_name || short(i.owner)) + " / " + i.kind,
      "model",
      (i) => el("span", i.status, i.status >= 400 ? "status problem" : "status"),
      (i) =>
        i.metered
          ? () => `${number(i.input)()} / ${number(i.output)()}`
          : msg("usage.unknown"),
      (i) => () => number(i.latency_ms)() + " ms",
    ],
  );
}
async function loadPanel(target, load) {
  target.replaceChildren(el("p", msg("common.loading")));
  try {
    const result = await load();
    target.replaceChildren(result);
  } catch (e) {
    target.replaceChildren(el("p", e.localized || e.message));
  }
}

function quotaWindow(minutes, index) {
  if (!minutes) return msg("quota.window", { n: number(index + 1) });
  if (minutes % 1440 === 0) return msg("quota.days", { n: number(minutes / 1440) });
  if (minutes % 60 === 0) return msg("quota.hours", { n: number(minutes / 60) });
  return msg("quota.minutes", { n: number(minutes) });
}
function renderQuota(data) {
  const result = document.createDocumentFragment();
  const buckets = { ...data.rateLimitsByLimitId };
  const main = data.rateLimits;
  if (main) buckets[main.limitId || "codex"] ??= main;
  for (const [name, bucket] of Object.entries(buckets)) {
    if (!bucket) continue;
    for (const [index, slot] of [bucket.primary, bucket.secondary].entries()) {
      if (!slot) continue;
      const label = el("label", msg("quota.used", {
        bucket: bucket.limitName || name,
        window: quotaWindow(slot.windowDurationMins, index),
        percent: percent(slot.usedPercent),
      }));
      label.className = "quota-window";
      const meter = el("progress");
      meter.max = 100;
      meter.value = slot.usedPercent;
      label.append(meter);
      if (slot.resetsAt != null)
        label.append(el("small", msg("quota.reset", { time: dateTime(slot.resetsAt) })));
      result.append(label);
    }
  }
  if (!result.childElementCount) result.append(el("p", msg("quota.noWindows")));
  const cards = data.rateLimitResetCredits;
  const cardHeader = el("div", undefined, "card-heading");
  cardHeader.append(el("h3", msg("quota.cards")));
  if (Number.isInteger(cards?.availableCount)) {
    cardHeader.append(el("span", msg("quota.cardCount", { n: number(cards.availableCount) }), "status"));
  } else {
    cardHeader.append(el("small", msg("quota.cardsUnknown")));
  }
  result.append(cardHeader);
  if (cards && Array.isArray(cards.credits) && cards.credits.length) {
    table(result, [msg("col.name"), msg("col.status"), msg("quota.granted"), msg("quota.expires")],
      cards.credits, [
        (c) => c.title || c.description || c.id,
        (c) => hasMessage(`card.${c.status}`) ? msg(`card.${c.status}`) : c.status,
        (c) => c.grantedAt != null ? dateTime(c.grantedAt) : "—",
        (c) => c.expiresAt != null ? dateTime(c.expiresAt) : "—",
      ]);
  } else if (cards?.availableCount > 0) {
    result.append(el("small", msg("quota.noCardDetails")));
  }
  const raw = el("details");
  raw.append(el("summary", msg("quota.raw")), el("pre", JSON.stringify(data, null, 2)));
  result.append(raw);
  return result;
}

const monitorUI = { el, button, req, panel, table };
const views = {
  async overview(root) {
    const [u, l] = await Promise.all([
      req("/admin/usage"),
      req("/admin/requests"),
    ]);
    const grid = el("div", undefined, "stats");
    for (const [k, label] of Object.entries({
      requests: msg("usage.requests"),
      input_tokens: msg("usage.input"),
      output_tokens: msg("usage.output"),
      images: msg("usage.images"),
    })) {
      const box = el("div", undefined, "stat");
      box.append(el("small", label), el("b", number(u[k])));
      grid.append(box);
    }
    root.append(
      grid,
      el(
        "p",
        msg("usage.summary", {
          errors: number(u.errors),
          unmetered: number(u.unmetered_requests),
        }),
        "summary",
      ),
    );
    const tools = el("div", undefined, "overview-tools");
    root.append(tools);
    const [latency, latencyBar] = panel(tools, msg("overview.latency"));
    latencyBar.append(button(msg("nav.stats"), () => goto("stats")));
    const recent = l.data.slice(0, 24).reverse();
    if (recent.length) {
      const durations = recent.map(row => row.latency_ms);
      const peak = Math.max(...durations);
      const average = Math.round(durations.reduce((sum, ms) => sum + ms, 0) / durations.length);
      const chart = el("div", undefined, "latency-bars");
      chart.setAttribute("aria-hidden", "true");
      for (const ms of durations) {
        const bar = el("span");
        bar.style.height = `${ms / Math.max(peak, 1) * 100}%`;
        bar.title = `${ms} ms`;
        chart.append(bar);
      }
      latency.append(chart, el("p", msg("overview.sample", {
        n: number(recent.length), average: number(average), peak: number(peak),
      })));
    } else {
      latency.append(el("p", msg("overview.noLatency"), "empty"));
    }
    const [api, apiBar] = panel(tools, msg("overview.endpoint"));
    const baseURL = new URL("/v1", location.origin).href;
    const address = el("code", baseURL, "endpoint");
    apiBar.append(button(msg("overview.copy"), async () => {
      try {
        await navigator.clipboard.writeText(baseURL);
        notice(msg("overview.copied"));
      } catch {
        const range = document.createRange();
        range.selectNodeContents(address);
        const selection = window.getSelection();
        selection.removeAllRanges();
        selection.addRange(range);
        notice(msg("overview.selected"));
      }
    }));
    api.append(el("p", msg("overview.endpointHint")), address);
    const [s, bar] = panel(root, msg("usage.recent"));
    bar.append(button(msg("usage.all"), () => goto("logs")));
    logTable(s, l.data.slice(0, 8));
  },
  async keys(root) {
    const [s] = panel(root, msg("keys.issue")),
      [lab, name] = field(msg("keys.name")),
      line = el("div", undefined, "toolbar");
    line.append(
      lab,
      button(
        msg("keys.create"),
        async () => {
          const j = await req("/admin/keys", {
            method: "POST",
            data: { name: name.value },
          });
          await render();
          const out = el("section");
          out.append(
            el("strong", msg("keys.copy")),
            el("div", j.key, "secret"),
          );
          $("content").prepend(out);
        },
        "primary",
      ),
    );
    s.append(line);
    const l = await req("/admin/keys?offset=" + offset),
      [list] = panel(root, msg("keys.list"));
    table(
      list,
      [
        msg("col.name"),
        msg("col.id"),
        msg("col.created"),
        msg("col.status"),
        msg("col.actions"),
      ],
      l.data,
      [
        "name",
        "id",
        (i) => dateTime(i.created),
        (i) => (i.revoked ? msg("keys.revoked") : msg("keys.active")),
        (i) =>
          i.revoked
            ? "—"
            : button(
                msg("keys.revoke"),
                async () => {
                  if (confirm(text("keys.confirm", { name: i.name }))) {
                    await req("/admin/keys/" + i.id, { method: "DELETE" });
                    await render();
                  }
                },
                "warn",
              ),
      ],
    );
    pager(list, l.data.length);
  },
  async sessions(root) {
    const l = await req("/admin/sessions?offset=" + offset),
      [s] = panel(root, msg("sessions.list"));
    table(
      s,
      [
        msg("col.owner"),
        msg("col.session"),
        msg("col.model"),
        msg("usage.recent"),
        msg("col.actions"),
      ],
      l.data,
      [
        (i) => i.name + " / " + i.key_name,
        "id",
        (i) => i.model || msg("common.default"),
        (i) => dateTime(i.updated),
        (i) => {
          const n = el("div", undefined, "toolbar");
          n.append(
            button(msg("sessions.history"), async () => {
              const [box, bar] = panel(
                root,
                msg("sessions.content", { name: i.name }),
              );
              bar.append(button(msg("common.close"), () => box.remove()));
              box.scrollIntoView();
              const load = async (cursor = "") => {
                const j = await req(
                  "/admin/sessions/" +
                    i.id +
                    "/history?cursor=" +
                    encodeURIComponent(cursor),
                );
                if (!j.data.length) box.append(el("p", msg("sessions.empty")));
                for (const t of j.data) {
                  for (const m of t.messages)
                    box.append(
                      el(
                        "small",
                        hasMessage(`role.${m.role}`)
                          ? msg(`role.${m.role}`)
                          : m.role,
                      ),
                      el("pre", m.content),
                    );
                }
                if (j.next_cursor) {
                  const more = button(msg("sessions.older"), async () => {
                    await load(j.next_cursor);
                    more.remove();
                  });
                  box.append(more);
                }
              };
              await load();
            }),
            button(msg("sessions.logs"), () => {
              filter = i.id;
              return goto("logs");
            }),
            button(
              msg("sessions.remove"),
              async () => {
                if (confirm(text("sessions.confirm"))) {
                  await req("/admin/sessions/" + i.id, { method: "DELETE" });
                  await render();
                }
              },
              "warn",
            ),
          );
          return n;
        },
      ],
    );
    pager(s, l.data.length);
  },
  logs(root, signal) {
    return logsView(root, monitorUI, filter, signal);
  },
  stats(root, signal) {
    return usageView(root, monitorUI, signal);
  },
  errors(root, signal) {
    return errorsView(root, monitorUI, signal);
  },
  async models(root) {
    const [account, bar] = panel(root, msg("account.title")),
      accountBody = el("div"),
      [quota] = panel(root, msg("quota.title")),
      quotaBody = el("div"),
      [models, modelBar] = panel(root, msg("models.available")),
      modelBody = el("div");
    account.append(accountBody);
    quota.append(quotaBody);
    models.append(modelBody);
    const loadAccount = async () => {
      let skipQuota;
      quotaBody.replaceChildren(el("p", msg("common.loading")));
      await loadPanel(accountBody, async () => {
        const { account, requiresOpenaiAuth } = await req("/admin/account");
        if (!account) {
          skipQuota = msg(requiresOpenaiAuth ? "account.loginHint" : "quota.unsupported");
          return el("p", msg(requiresOpenaiAuth ? "account.signedOut" : "account.external"));
        }
        if (account.type !== "chatgpt") skipQuota = msg("quota.unsupported");
        return el("p", msg("account.signedIn", {
          identity: [account.type, account.email, account.workspaceName, account.planType]
            .filter(Boolean).join(" · "),
        }));
      });
      if (!root.isConnected) return;
      if (skipQuota) quotaBody.replaceChildren(el("p", skipQuota));
      else await loadPanel(quotaBody, async () => renderQuota(await req("/admin/quota")));
    };
    const loadModels = () => loadPanel(modelBody, async () => {
      const list = document.createDocumentFragment();
      const m = await req("/admin/models");
      table(list, [msg("col.modelId"), msg("col.displayName"), msg("common.default")],
        m.data, ["id", "name", (i) => (i.is_default ? msg("common.yes") : "—")]);
      return list;
    });
    const refresh = button(msg("quota.query"), loadAccount);
    bar.append(refresh);
    refresh.disabled = true;
    await loadAccount();
    refresh.disabled = false;
    if (!root.isConnected) return;
    await loadModels();
    modelBar.append(button(msg("models.refresh"), loadModels));
  },
  async cli(root) {
    const [s, b] = panel(root, msg("cli.title"));
    let installed = await req("/admin/cli");
    s.append(
      el(
        "p",
        msg("cli.current", {
          version: installed.installed,
          mode: msg(installed.managed ? "cli.managed" : "cli.external"),
        }),
      ),
    );
    b.append(
      button(msg("cli.check"), async () => {
        const j = await req("/admin/cli?check=1");
        if (!j.latest) throw uiError(msg("error.registry_error"));
        details.replaceChildren(
          el("p", msg("cli.latest", { version: j.latest })),
        );
        if (j.managed && j.latest !== j.installed)
          details.append(
            button(
              msg("cli.install", { version: j.latest }),
              async () => {
                if (confirm(text("cli.confirm"))) {
                  notice(msg("cli.installing"));
                  await req("/admin/cli/update", {
                    method: "POST",
                    data: { version: j.latest },
                  });
                  await render();
                }
              },
              "primary",
            ),
          );
      }),
    );
    const details = el("div");
    s.append(details);
    if (installed.managed)
      s.append(
        button(msg("cli.restore"), async () => {
          if (confirm(text("cli.confirmRestore"))) {
            await req("/admin/cli/update", {
              method: "POST",
              data: { reset: true },
            });
            await render();
          }
        }),
      );
  },
  async play(root) {
    const [memory, memoryBar] = panel(root, msg("memory.title")),
      reading = el("div");
    memory.append(reading);
    memoryBar.append(button(msg("memory.measure"), () => loadPanel(reading, async () => {
      const sample = await req("/admin/memory");
      const values = document.createDocumentFragment();
      values.append(el("small", msg("memory.sample", { time: dateTime(sample.sampledAt) })));
      for (const [field, label] of [
        ["heapBytes", "memory.heap"], ["rssBytes", "memory.rss"], ["peakRssBytes", "memory.peak"],
      ]) {
        if (sample[field] != null)
          values.append(el("p", msg(label, { mib: number(Math.round(sample[field] / 1048576 * 100) / 100) })));
      }
      return values;
    })));
    const [s] = panel(root, msg("play.send")),
      [lk, key] = field(msg("play.key"), "password"),
      [lm, model] = field(msg("play.model")),
      [ls, sid] = field(msg("play.session")),
      [lp, prompt] = field(msg("play.prompt"), "textarea");
    key.autocomplete = "off";
    const tools = el("div", undefined, "toolbar"),
      out = el("pre"),
      image = el("img");
    image.dataset.i18nAlt = "play.result";
    image.alt = text("play.result");
    image.hidden = true;
    const call = (path, data, headers = {}) =>
      requestJSON(path, {
        method: "POST",
        headers: {
          "Content-Type": "application/json",
          Authorization: "Bearer " + key.value,
          ...headers,
        },
        body: JSON.stringify(data),
      });
    tools.append(
      button(msg("play.new"), async () => {
        const j = await call("/v1/sessions", {
          name: text("play.name"),
          model: model.value,
        });
        sid.value = j.id;
      }),
      button(
        msg("play.chat"),
        async () => {
          image.hidden = true;
          writeText(out, msg("play.generating"));
          try {
            const j = await call(
              "/v1/chat/completions",
              {
                model: model.value,
                messages: [{ role: "user", content: prompt.value }],
              },
              sid.value ? { "X-Session-ID": sid.value } : {},
            );
            writeText(out, j.choices[0].message.content);
          } catch (e) {
            writeText(out, "");
            throw e;
          }
        },
        "primary",
      ),
      button(msg("play.image"), async () => {
        image.hidden = true;
        writeText(out, msg("play.drawing"));
        try {
          const j = await call("/v1/images/generations", {
            model: model.value,
            prompt: prompt.value,
          });
          image.src = "data:image/png;base64," + j.data[0].b64_json;
          image.hidden = false;
        } finally {
          writeText(out, "");
        }
      }),
    );
    s.append(lk, lm, ls, lp, tools, out, image);
  },
};
$("signin").onsubmit = async (e) => {
  e.preventDefault();
  admin = $("token").value;
  try {
    await req("/admin/usage");
    $("token").value = "";
    writeText($("loginError"), "");
    $("login").hidden = true;
    $("shell").hidden = false;
    await goto("overview");
  } catch (err) {
    admin = "";
    writeText($("loginError"), err.localized || err.message);
  }
};
$("logout").onclick = () => location.reload();
