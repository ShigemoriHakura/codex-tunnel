import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import zh from "../web/locales/zh-CN.mjs";
import en from "../web/locales/en.mjs";
import { chooseLanguage, text } from "../web/i18n.mjs";

test("saved preference, ordered browser languages, region matching, and fallback", () => {
  assert.equal(chooseLanguage("en", ["zh-CN"]), "en");
  assert.equal(chooseLanguage("invalid", ["fr", "en-GB", "zh"]), "en");
  assert.equal(chooseLanguage(null, ["zh-Hans", "en"]), "zh-CN");
  assert.equal(chooseLanguage(null, ["de"]), "zh-CN");
});
test("catalogs agree on keys and interpolation parameters", () => {
  assert.deepEqual(Object.keys(en).sort(), Object.keys(zh).sort());
  for (const key of Object.keys(zh)) {
    const params = (s) => (s.match(/\{\w+\}/g) ?? []).sort();
    assert.ok(en[key] && zh[key], key);
    assert.deepEqual(params(en[key]), params(zh[key]), key);
  }
  assert.equal(
    text("keys.confirm", { name: "{name}<b>$1" }),
    "撤销 {name}<b>$1 的访问权？",
  );
});
test("static markup and literal application references exist in the catalogs", () => {
  const read = (file) => readFileSync(new URL(file, import.meta.url), "utf8");
  const html = read("../web/index.html");
  const app = read("../web/app.js");
  for (const [, key] of [
    ...html.matchAll(/data-i18n="([^"]+)"/g),
    ...app.matchAll(/(?:text|msg)\(\s*"([^"]+)"/g),
  ])
    assert.ok(Object.hasOwn(en, key), key);
  for (const page of [
    "overview",
    "keys",
    "sessions",
    "logs",
    "models",
    "cli",
    "play",
  ])
    for (const prefix of ["nav", "desc"])
      assert.ok(Object.hasOwn(en, `${prefix}.${page}`));
});
