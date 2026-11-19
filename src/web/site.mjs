import { msg, text, writeText } from "./i18n.mjs";

// import.meta.url points at the actual browser mount, including behind a reverse proxy.
export const mountURL = new URL("./", import.meta.url);
export function apiPath(path) {
  return new URL(path.slice(1), mountURL);
}
export function publicURLs({ origin, directory }) {
  const fallback = location.origin;
  let host = origin.trim();
  if (host && !host.includes("://")) host = "https://" + host;
  const url = new URL(host || fallback);
  if (
    !["http:", "https:"].includes(url.protocol) ||
    url.username ||
    url.password ||
    url.pathname !== "/" ||
    url.search ||
    url.hash
  )
    throw new Error(text("error.invalid_site_address"));
  const path = directory.trim().replace(/^\//, "").replace(/\/$/, "");
  if (
    path &&
    (!/^[A-Za-z0-9_-]+(?:\/[A-Za-z0-9_-]+)*$/.test(path) ||
      /^(admin|v1|healthz|locales)(\/|$)/.test(path))
  )
    throw new Error(text("error.invalid_site_address"));
  const consoleURL = new URL(path ? "/" + path + "/" : "/", url);
  return {
    consoleURL: consoleURL.href,
    apiURL: new URL("v1", consoleURL).href,
  };
}
export async function copyAddress(value, element) {
  try {
    await navigator.clipboard.writeText(value);
    return true;
  } catch {
    const range = document.createRange();
    range.selectNodeContents(element);
    const selection = window.getSelection();
    selection.removeAllRanges();
    selection.addRange(range);
    return false;
  }
}
export async function siteView(root, { el, req, panel, field }, signal) {
  const saved = await req("/admin/site", { signal });
  if (signal.aborted) return;
  const [box] = panel(root, msg("site.settings"));
  const [originLabel, origin] = field(msg("site.domain"), "text", saved.origin);
  const [pathLabel, path] = field(
    msg("site.directory"),
    "text",
    saved.directory,
  );
  origin.placeholder = "https://gateway.example.com";
  path.placeholder = "/codex";
  for (const input of [origin, path]) {
    input.spellcheck = false;
    input.autocomplete = "off";
  }
  const form = el("form", undefined, "grid max-w-3xl gap-4");
  const fields = el("div", undefined, "grid gap-4 md:grid-cols-2");
  originLabel.append(el("small", msg("site.originHint")));
  pathLabel.append(el("small", msg("site.pathHint")));
  fields.append(originLabel, pathLabel);
  form.append(fields);
  const preview = el(
    "div",
    undefined,
    "grid gap-2 rounded-[10px] border border-solid border-[var(--line)] bg-[var(--soft)] p-4",
  );
  const pageURL = el("code"),
    apiURL = el("code");
  preview.append(
    el("small", msg("site.consoleURL")),
    pageURL,
    el("small", msg("overview.endpoint")),
    apiURL,
  );
  const state = el("p");
  state.setAttribute("role", "status");
  const submit = el("button", msg("site.save"), "primary");
  submit.type = "submit";
  const currentLink = el("a", msg("site.open"));
  currentLink.target = "_blank";
  currentLink.rel = "noopener noreferrer";
  currentLink.href = publicURLs(saved).consoleURL;
  const actions = el("div", undefined, "flex flex-wrap items-center gap-4");
  actions.append(submit, currentLink);
  form.append(preview, actions, state);
  box.append(form, el("p", msg("site.dnsHint"), "hint"));
  function draft() {
    return { origin: origin.value, directory: path.value };
  }
  function update() {
    state.replaceChildren();
    try {
      const v = publicURLs(draft());
      pageURL.textContent = v.consoleURL;
      apiURL.textContent = v.apiURL;
      submit.disabled = false;
    } catch (e) {
      pageURL.textContent = apiURL.textContent = "—";
      writeText(state, msg("error.invalid_site_address"));
      submit.disabled = true;
    }
  }
  origin.oninput = path.oninput = update;
  form.onsubmit = async (e) => {
    e.preventDefault();
    submit.disabled = true;
    try {
      const input = draft();
      if (input.origin.trim())
        input.origin = new URL(publicURLs(input).consoleURL).origin;
      const next = await req("/admin/site", {
        method: "PUT",
        data: input,
        signal,
      });
      if (signal.aborted) return;
      origin.value = next.origin;
      path.value = next.directory;
      currentLink.href = publicURLs(next).consoleURL;
      update();
      writeText(state, msg("site.saved"));
      if (
        mountURL.pathname !== "/" &&
        mountURL.pathname !== next.directory + "/"
      ) {
        location.replace(new URL(next.directory + "/", location.origin).href);
      }
    } catch (err) {
      if (!signal.aborted) writeText(state, err.localized || err.message);
    } finally {
      submit.disabled = false;
    }
  };
  update();
}
