import { msg, text, writeText } from "./i18n.mjs";

export async function editBinding(
  root,
  key,
  { el, req, button, field },
  refresh,
  signal,
) {
  const dialog = el("dialog", undefined, "log-detail"),
    title = el("h2", msg("keys.bindTitle", { name: key.name }));
  title.id = "binding-title";
  dialog.setAttribute("aria-labelledby", title.id);
  const bar = el("div", undefined, "bar");
  bar.append(
    title,
    button(msg("common.close"), () => dialog.close()),
  );
  const label = el("label", msg("keys.chooseSession")),
    select = el("select");
  label.append(select);
  const [newLabel, name] = field(msg("keys.newName"));
  const actionBar = el("div", undefined, "toolbar"),
    paging = el("div", undefined, "toolbar");
  const state = el("p");
  state.setAttribute("role", "status");
  let offset = 0,
    count = 0,
    selected = key.session_id || "",
    latest = 0;
  function sync() {
    newLabel.hidden = selected !== "new";
    save.disabled =
      selected === key.session_id ||
      (selected === "new" && !name.value.trim()) ||
      (!selected && !key.session_id);
  }
  function option(value, label) {
    const opt = el("option", label);
    opt.value = value;
    select.append(opt);
  }
  async function load() {
    const revision = ++latest;
    const { data } = await req("/admin/sessions?offset=" + offset, { signal });
    if (signal.aborted || revision !== latest) return;
    count = data.length;
    select.replaceChildren();
    option("", msg("keys.unbound"));
    option("new", msg("keys.createBind"));
    if (key.session_id && !data.some((s) => s.id === key.session_id))
      option(key.session_id, key.session_name + " · " + key.session_id);
    for (const s of data)
      option(s.id, s.name + " · " + s.key_name + " · " + s.id);
    select.value = selected;
    if (select.selectedIndex < 0) {
      selected = key.session_id || "";
      select.value = selected;
    }
    paging.replaceChildren();
    if (offset > 0)
      paging.append(
        button(msg("common.previous"), () => {
          offset -= 100;
          return load();
        }),
      );
    if (count === 100)
      paging.append(
        button(msg("common.next"), () => {
          offset += 100;
          return load();
        }),
      );
    sync();
  }
  const save = button(
    msg("keys.saveBind"),
    async () => {
      try {
        if (
          selected &&
          selected !== "new" &&
          !confirm(text("keys.shareConfirm", { name: key.name }))
        )
          return;
        await req("/admin/keys/" + key.id + "/binding", {
          method: "PUT",
          data:
            selected === "new"
              ? { create_name: name.value.trim() }
              : { session_id: selected },
          signal,
        });
        dialog.close();
        await refresh();
      } catch (e) {
        if (!signal.aborted) writeText(state, e.localized || e.message);
      }
    },
    "primary",
  );
  select.onchange = () => {
    selected = select.value;
    sync();
  };
  name.oninput = sync;
  actionBar.append(save);
  dialog.append(
    bar,
    el("p", msg("keys.bindHint")),
    label,
    paging,
    newLabel,
    actionBar,
    state,
  );
  root.append(dialog);
  dialog.addEventListener("close", () => dialog.remove(), { once: true });
  await load();
  if (!signal.aborted) dialog.showModal();
}
