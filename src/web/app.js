"use strict";
const $ = (id) => document.getElementById(id),
  pages = {
    overview: [
      "运行概览",
      "OVERVIEW",
      "查看此网关的推理请求和已报告的 Token 用量。",
    ],
    keys: [
      "访问密钥",
      "ACCESS KEYS",
      "创建独立的客户端身份；撤销后无法发起新请求。",
    ],
    sessions: [
      "会话管理",
      "SESSIONS",
      "查看会话归属、模型和活动记录，或移除会话绑定。",
    ],
    logs: [
      "请求日志",
      "REQUESTS",
      "记录推理请求的元数据，不记录提示词或响应正文。",
    ],
    models: [
      "模型与配额",
      "UPSTREAM",
      "查询当前 Codex 登录身份可见的模型和上游配额。",
    ],
    cli: [
      "CLI 维护",
      "RUNTIME",
      "检查官方稳定版本；安装验证成功后，新请求才切换版本。",
    ],
    play: [
      "接口调试",
      "PLAYGROUND",
      "使用客户端 API 密钥测试对话、续聊和生图。",
    ],
  };
let admin = "",
  page = "overview",
  offset = 0,
  filter = "",
  epoch = 0;
function el(tag, text, cls) {
  const n = document.createElement(tag);
  if (text !== undefined) n.textContent = text;
  if (cls) n.className = cls;
  return n;
}
function notice(s) {
  $("notice").textContent = s;
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
      if (at === epoch) notice(e.message);
    } finally {
      b.disabled = false;
    }
  };
  return b;
}
async function req(path, { method = "GET", data, key = admin } = {}) {
  const r = await fetch(path, {
    method,
    headers: {
      Authorization: "Bearer " + key,
      ...(data ? { "Content-Type": "application/json" } : {}),
    },
    body: data ? JSON.stringify(data) : undefined,
  });
  const j = await r.json();
  if (!r.ok) throw Error(`${r.status} · ${j.error?.message || "请求失败"}`);
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
    root.append(el("p", "暂无记录。"));
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
      else td.textContent = v ?? "—";
    }
  }
  root.append(t);
}
const time = (s) => new Date(s * 1000).toLocaleString(),
  num = (n) => Number(n).toLocaleString(),
  short = (s) => (s ? s.slice(0, 13) + "…" : "—");
function pager(root, len) {
  const p = el("div", undefined, "toolbar");
  if (offset > 0)
    p.append(
      button("← 上一页", () => {
        offset -= 100;
        return render();
      }),
    );
  p.append(el("small", `第 ${offset / 100 + 1} 页`));
  if (len === 100)
    p.append(
      button("下一页 →", () => {
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
  $("title").textContent = pages[page][0];
  $("desc").textContent = pages[page][2];
  $("section").textContent = pages[page][1];
  $("nav").replaceChildren(
    ...Object.keys(pages).map((k) =>
      button(
        pages[k][0],
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
  const status = el("p", "正在读取…");
  root.append(status);
  try {
    await views[page](root);
  } catch (e) {
    if (current === epoch) notice(e.message);
  } finally {
    status.remove();
  }
}
function logTable(root, items) {
  table(
    root,
    ["时间", "密钥 / 类型", "模型", "状态", "输入 / 输出", "耗时"],
    items,
    [
      (i) => time(i.created),
      (i) => (i.key_name || short(i.owner)) + " / " + i.kind,
      "model",
      "status",
      (i) => (i.metered ? `${i.input} / ${i.output}` : "未报告"),
      (i) => i.latency_ms + " ms",
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
      requests: "推理请求",
      input_tokens: "输入 Tokens",
      output_tokens: "输出 Tokens",
      images: "成功生图",
    })) {
      const box = el("div", undefined, "stat");
      box.append(el("small", label), el("b", num(u[k])));
      grid.append(box);
    }
    root.append(
      grid,
      el(
        "p",
        `失败 ${u.errors} 次 · ${u.unmetered_requests} 次上游未报告 Token。统计不是账单。`,
      ),
    );
    const [s, bar] = panel(root, "最近活动");
    bar.append(button("查看全部 →", () => goto("logs")));
    logTable(s, l.data.slice(0, 8));
  },
  async keys(root) {
    const [s] = panel(root, "签发密钥"),
      [lab, name] = field("用途 / 名称"),
      line = el("div", undefined, "toolbar");
    line.append(
      lab,
      button(
        "创建密钥",
        async () => {
          const j = await req("/admin/keys", {
            method: "POST",
            data: { name: name.value },
          });
          await render();
          const out = el("section");
          out.append(
            el("strong", "请立即复制，完整密钥仅显示一次。"),
            el("div", j.key, "secret"),
          );
          $("content").prepend(out);
        },
        "primary",
      ),
    );
    s.append(line);
    const l = await req("/admin/keys?offset=" + offset),
      [list] = panel(root, "客户端密钥");
    table(
      list,
      ["名称", "标识（非密钥）", "创建时间", "状态", "操作"],
      l.data,
      [
        "name",
        "id",
        (i) => time(i.created),
        (i) => (i.revoked ? "已撤销" : "有效"),
        (i) =>
          i.revoked
            ? "—"
            : button(
                "撤销",
                async () => {
                  if (confirm("撤销 " + i.name + " 的访问权？")) {
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
      [s] = panel(root, "持久会话");
    table(s, ["名称 / 归属", "会话 ID", "模型", "最近活动", "操作"], l.data, [
      (i) => i.name + " / " + i.key_name,
      "id",
      (i) => i.model || "默认",
      (i) => time(i.updated),
      (i) => {
        const n = el("div", undefined, "toolbar");
        n.append(
          button("历史", async () => {
            const [box, bar] = panel(root, "会话内容 · " + i.name);
            bar.append(button("关闭", () => box.remove()));
            box.scrollIntoView();
            const load = async (cursor = "") => {
              const j = await req(
                "/admin/sessions/" +
                  i.id +
                  "/history?cursor=" +
                  encodeURIComponent(cursor),
              );
              if (!j.data.length) box.append(el("p", "尚无对话。"));
              for (const t of j.data) {
                for (const m of t.messages)
                  box.append(el("small", m.role), el("pre", m.content));
              }
              if (j.next_cursor) {
                const more = button("更早内容", async () => {
                  await load(j.next_cursor);
                  more.remove();
                });
                box.append(more);
              }
            };
            await load();
          }),
          button("记录", () => {
            filter = i.id;
            return goto("logs");
          }),
          button(
            "移除",
            async () => {
              if (confirm("移除绑定后客户端无法继续该会话。确认？")) {
                await req("/admin/sessions/" + i.id, { method: "DELETE" });
                await render();
              }
            },
            "warn",
          ),
        );
        return n;
      },
    ]);
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
        filter ? "会话活动 · " + short(filter) : "全部推理请求",
      );
    b.append(
      button(filter ? "清除筛选" : "刷新", () => {
        filter = "";
        return goto("logs");
      }),
    );
    logTable(s, l.data);
    pager(s, l.data.length);
  },
  async models(root) {
    const [s, b] = panel(root, "可用模型");
    b.append(button("重新查询", render));
    const [q, qb] = panel(root, "账户限额"),
      out = el("pre"),
      windows = el("div"),
      raw = el("details");
    raw.hidden = true;
    raw.append(el("summary", "查看上游原始数据"), out);
    qb.append(
      button("查询上游配额", async () => {
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
              `${bucket.limitName || name} · ${slot.windowDurationMins ? slot.windowDurationMins + " 分钟" : "窗口 " + (index + 1)} · 已用 ${slot.usedPercent}%`,
            );
            const meter = el("progress");
            meter.max = 100;
            meter.value = slot.usedPercent;
            label.append(meter);
            if (slot.resetsAt)
              label.append(el("small", "重置：" + time(slot.resetsAt)));
            windows.append(label);
          }
        }
      }),
    );
    q.append(el("p", "不同登录方式可能不提供限额数据。"), windows, raw);
    const m = await req("/admin/models");
    table(s, ["模型 ID", "显示名称", "默认"], m.data, [
      "id",
      "name",
      (i) => (i.is_default ? "是" : "—"),
    ]);
  },
  async cli(root) {
    const [s, b] = panel(root, "运行时版本");
    let installed = await req("/admin/cli");
    s.append(
      el(
        "p",
        `当前版本：${installed.installed} · ${installed.managed ? "托管安装" : "外部管理，仅可检查"}`,
      ),
    );
    b.append(
      button("检查更新", async () => {
        const j = await req("/admin/cli?check=1");
        if (!j.latest) throw Error(j.check_error);
        details.replaceChildren(el("p", "npm 稳定版本：" + j.latest));
        if (j.managed && j.latest !== j.installed)
          details.append(
            button(
              "安装 " + j.latest,
              async () => {
                if (confirm("安装该官方版本并切换新请求？")) {
                  notice("正在安装和验证，请稍候…");
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
        button("恢复镜像内置版本", async () => {
          if (confirm("新请求将使用内置版本。继续？")) {
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
    const [s] = panel(root, "发送请求"),
      [lk, key] = field("客户端 API 密钥", "password"),
      [lm, model] = field("模型（留空使用默认）"),
      [ls, sid] = field("会话 ID（可选）"),
      [lp, prompt] = field("提示词", "textarea");
    key.autocomplete = "off";
    const tools = el("div", undefined, "toolbar"),
      out = el("pre"),
      image = el("img");
    image.alt = "生成结果";
    image.hidden = true;
    const call = (path, data, headers = {}) =>
      fetch(path, {
        method: "POST",
        headers: {
          "Content-Type": "application/json",
          Authorization: "Bearer " + key.value,
          ...headers,
        },
        body: JSON.stringify(data),
      }).then(async (r) => {
        const j = await r.json();
        if (!r.ok) throw Error(j.error?.message || "失败");
        return j;
      });
    tools.append(
      button("新建会话", async () => {
        const j = await call("/v1/sessions", {
          name: "面板调试",
          model: model.value,
        });
        sid.value = j.id;
      }),
      button(
        "对话",
        async () => {
          image.hidden = true;
          out.textContent = "正在生成…";
          try {
            const j = await call(
              "/v1/chat/completions",
              {
                model: model.value,
                messages: [{ role: "user", content: prompt.value }],
              },
              sid.value ? { "X-Session-ID": sid.value } : {},
            );
            out.textContent = j.choices[0].message.content;
          } catch (e) {
            out.textContent = "";
            throw e;
          }
        },
        "primary",
      ),
      button("生成图片", async () => {
        image.hidden = true;
        out.textContent = "正在生图…";
        try {
          const j = await call("/v1/images/generations", {
            model: model.value,
            prompt: prompt.value,
          });
          image.src = "data:image/png;base64," + j.data[0].b64_json;
          image.hidden = false;
        } finally {
          out.textContent = "";
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
    $("loginError").textContent = "";
    $("login").hidden = true;
    $("shell").hidden = false;
    await goto("overview");
  } catch (err) {
    admin = "";
    $("loginError").textContent = err.message;
  }
};
$("logout").onclick = () => location.reload();
