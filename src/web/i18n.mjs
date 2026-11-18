import zh from "./locales/zh-CN.mjs";
import en from "./locales/en.mjs";

const locales = new Map([
  ["zh-CN", { name: "简体中文", messages: zh }],
  ["en", { name: "English", messages: en }],
]);
const fallback = "zh-CN";
const preferenceKey = "desk-tunnel.locale";
let language = fallback;

export function chooseLanguage(saved, preferred = []) {
  if (locales.has(saved)) return saved;
  for (const tag of preferred) {
    const base = tag.toLowerCase().split("-")[0];
    for (const key of locales.keys()) {
      if (key.toLowerCase() === tag.toLowerCase()) return key;
    }
    for (const key of locales.keys()) {
      if (key.toLowerCase().split("-")[0] === base) return key;
    }
  }
  return fallback;
}
const resolve = (value) => (typeof value === "function" ? value() : value);
export const hasMessage = (key) =>
  Object.hasOwn(locales.get(fallback).messages, key);
export function text(key, params = {}) {
  const template =
    locales.get(language).messages[key] ?? locales.get(fallback).messages[key];
  if (typeof template !== "string") return key;
  return template.replace(/\{(\w+)\}/g, (match, name) =>
    Object.hasOwn(params, name) ? resolve(params[name]) : match,
  );
}
// A lazy label is re-evaluated on language change; raw server/user strings never go through translation.
export const msg = (key, params) => () => text(key, params);
export const number = (value) => () => Number(value).toLocaleString(language);
export const percent = (value) => () =>
  (value / 100).toLocaleString(language, {
    style: "percent",
    maximumFractionDigits: 2,
  });
export const dateTime = (unix) => () =>
  new Date(unix * 1000).toLocaleString(language);
export function uiError(label, status) {
  const localized = () => (status ? `${status} · ` : "") + resolve(label);
  return Object.assign(new Error(localized()), { localized });
}

// Keep the text node, not the whole element, as the update target: labels may also
// contain live inputs, progress meters, etc. Removed elements are garbage-collected.
const bindings = new WeakMap();
export function writeText(element, value) {
  if (typeof value !== "function") {
    bindings.delete(element);
    delete element.dataset.localized;
    element.textContent = value;
    return;
  }
  const node = document.createTextNode(value());
  element.replaceChildren(node);
  element.dataset.localized = "";
  bindings.set(element, () => {
    node.data = value();
  });
}
function applyLanguage() {
  document.documentElement.lang = language;
  for (const el of document.querySelectorAll("[data-i18n]"))
    el.textContent = text(el.dataset.i18n);
  for (const el of document.querySelectorAll("[data-i18n-alt]"))
    el.alt = text(el.dataset.i18nAlt);
  for (const el of document.querySelectorAll("[data-localized]"))
    bindings.get(el)?.();
  for (const el of document.querySelectorAll("[data-language]"))
    el.value = language;
}
export function initLanguage() {
  let saved;
  try {
    saved = localStorage.getItem(preferenceKey);
  } catch {
    /* Storage may be blocked. */
  }
  language = chooseLanguage(
    saved,
    navigator.languages?.length ? navigator.languages : [navigator.language],
  );
  for (const selector of document.querySelectorAll("[data-language]")) {
    for (const [key, { name }] of locales) {
      const option = new Option(name, key);
      option.lang = key;
      selector.add(option);
    }
    selector.addEventListener("change", () => {
      if (!locales.has(selector.value)) return;
      language = selector.value;
      try {
        localStorage.setItem(preferenceKey, language);
      } catch {
        /* Use an in-memory preference. */
      }
      applyLanguage();
    });
  }
  applyLanguage();
}
