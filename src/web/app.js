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
const pages = ["overview", "keys", "sessions", "logs", "models", "cli", "play"];
initLanguage();
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
async function req(path, { method = "GET", data, key = admin } = {}) {
  return requestJSON(path, {
    method,
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
    root.append(el("p", msg("common.empty")));
    return;
  }
  const t = el("table"),
    tr = el("tr");
  for (const h of heads) tr.append(el("th", h));
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
  root.append(t);
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
async function render() {
  const current = ++epoch;
  notice("");
  writeText($("title"), msg(`nav.${page}`));
  writeText($("desc"), msg(`desc.${page}`));
  writeText($("section"), msg(`nav.${page}`));
  $("nav").replaceChildren(
    ...pages.map((k) =>
      button(
        msg(`nav.${k}`),
        () => {
          filter = "";
          return goto(k);
        },
        k === page ? "active" : "",
      ),
    ),
  );
  const root = el("div");
  $("content").replaceChildren(root);
  const status = el("p", msg("common.loading"));
  root.append(status);
  try {
    await views[page](root);
  } catch (e) {
    if (current === epoch) notice(e.localized || e.message);
  } finally {
    status.remove();
  }
}
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
      "status",
      (i) =>
        i.metered
          ? () => `${number(i.input)()} / ${number(i.output)()}`
          : msg("usage.unknown"),
      (i) => () => number(i.latency_ms)() + " ms",
    ],
  );
}
const views = {
  async overview(root) {
    const [u, l] = await Promise.all([
      req("/admin/usage"),
      req("/admin/requests"),
    ]);
    const grid = el("div", undefined, "grid");
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
      ),
    );
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
  async logs(root) {
    const l = await req(
        "/admin/requests?offset=" +
          offset +
          "&session=" +
          encodeURIComponent(filter),
      ),
      [s, b] = panel(
        root,
        filter ? msg("logs.filtered", { id: short(filter) }) : msg("logs.all"),
      );
    b.append(
      button(filter ? msg("logs.clear") : msg("common.refresh"), () => {
        filter = "";
        return goto("logs");
      }),
    );
    logTable(s, l.data);
    pager(s, l.data.length);
  },
  async models(root) {
    const [s, b] = panel(root, msg("models.available"));
    b.append(button(msg("models.refresh"), render));
    const [q, qb] = panel(root, msg("quota.title")),
      out = el("pre"),
      windows = el("div"),
      raw = el("details");
    raw.hidden = true;
    raw.append(el("summary", msg("quota.raw")), out);
    qb.append(
      button(msg("quota.query"), async () => {
        const data = await req("/admin/quota");
        out.textContent = JSON.stringify(data, null, 2);
        raw.hidden = false;
        windows.replaceChildren();
        const buckets = Object.keys(data.rateLimitsByLimitId || {}).length
          ? data.rateLimitsByLimitId
          : { codex: data.rateLimits };
        for (const [name, bucket] of Object.entries(buckets)) {
          if (!bucket) continue;
          for (const [index, slot] of [
            bucket.primary,
            bucket.secondary,
          ].entries()) {
            if (!slot) continue;
            const label = el(
              "label",
              msg("quota.used", {
                bucket: bucket.limitName || name,
                window: slot.windowDurationMins
                  ? msg("quota.minutes", { n: number(slot.windowDurationMins) })
                  : msg("quota.window", { n: number(index + 1) }),
                percent: percent(slot.usedPercent),
              }),
            );
            const meter = el("progress");
            meter.max = 100;
            meter.value = slot.usedPercent;
            label.append(meter);
            if (slot.resetsAt)
              label.append(
                el(
                  "small",
                  msg("quota.reset", { time: dateTime(slot.resetsAt) }),
                ),
              );
            windows.append(label);
          }
        }
      }),
    );
    q.append(el("p", msg("quota.hint")), windows, raw);
    const m = await req("/admin/models");
    table(
      s,
      [msg("col.modelId"), msg("col.displayName"), msg("common.default")],
      m.data,
      ["id", "name", (i) => (i.is_default ? msg("common.yes") : "—")],
    );
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
