import { test } from "node:test";
import assert from "node:assert/strict";
import { clip, configFrom, definitionPath, entryURL, languageFor, parseEntry, plain, ReadError, word } from "../src/model.ts";
import { readTime } from "../src/i18n.ts";
import { definitions } from "./fixtures.ts";
import { encodeState, readState, transition } from "../src/page.tsx";

test("configuration and language selection are bounded", () => {
  assert.deepEqual(configFrom(undefined), {}); assert.deepEqual(configFrom({ language: "auto" }), { language: "auto" });
  assert.equal(languageFor({}, "ja-JP"), "ja"); assert.equal(languageFor({}, "zh-Hans"), "zh"); assert.equal(languageFor({}, "en-US"), "en");
  assert.equal(languageFor({ language: "en" }, "ja"), "en");
  for (const value of [{ language: "fr" }, { language: 1 }, [], "en"]) assert.throws(() => configFrom(value), ReadError);
});

test("definition parser retains parts of speech and plain-text examples", () => {
  const entry = parseEntry(definitions, "en", "hello", 1000);
  assert.equal(entry.url, "https://en.wiktionary.org/wiki/hello");
  assert.equal(entry.groups[0].partOfSpeech, "Interjection");
  assert.equal(entry.groups[0].definitions[0].text, "A greeting said when meeting someone.");
  assert.equal(entry.groups[0].definitions[0].examples[0].text, "Hello, everyone.");
  assert.equal(entry.groups[1].partOfSpeech, "Noun");
  assert.ok(!entry.url.includes("attacker"));
  assert.throws(() => parseEntry({}, "en", "missing", 0), ReadError);
});

test("an empty or markup-only definition line is skipped, not the word", () => {
  const raw = { en: [
    { partOfSpeech: "Noun", language: "English", definitions: [{ definition: "" }, { definition: "<span class=\"usage-label-sense\"></span> " }, { definition: "A clear liquid." }] },
    { partOfSpeech: "Verb", language: "English", definitions: [{ definition: "" }] },
  ] };
  const entry = parseEntry(raw, "en", "water", 0);
  assert.deepEqual(entry.groups.map(group => [group.partOfSpeech, group.definitions.map(definition => definition.text)]), [["Noun", ["A clear liquid."]]]);
  assert.throws(() => parseEntry({ en: [raw.en[1]] }, "en", "water", 0), (error: unknown) => error instanceof ReadError && error.code === "not_found");
  assert.throws(() => parseEntry({ en: [{ partOfSpeech: "Noun", definitions: [{ definition: 1 }] }] }, "en", "water", 0), (error: unknown) => error instanceof ReadError && error.code === "bad_answer");
});

test("HTML snippets stay plain data with bounded entities", () => {
  assert.equal(plain(" <b>Hello</b>&nbsp; world &unknown; &#x1F600; "), "Hello world &unknown; 😀");
  assert.equal(plain("unterminated <tag"), "unterminated");
  assert.equal(plain("&#0; &#xD800;"), "� �");
});

test("words cannot select another URL authority and entry URLs are percent-encoded", () => {
  assert.equal(word("hello world"), "hello world");
  assert.equal(word("😀"), "😀");
  assert.equal(entryURL("en", "hello world"), "https://en.wiktionary.org/wiki/hello_world");
  assert.equal(entryURL("en", "100%"), "https://en.wiktionary.org/wiki/100%25");
  assert.equal(entryURL("zh", "猫"), "https://zh.wiktionary.org/wiki/%E7%8C%AB");
  assert.equal(entryURL("en", "foo/bar"), "https://en.wiktionary.org/wiki/foo/bar");
  for (const value of ["", " ", "a#fragment", "a?query", "a<b", "x".repeat(121), "bad\n", "../outside", "bad\\path", "\uD800", "\uDC00", "a\uD800b"]) assert.throws(() => definitionPath(value), ReadError);
  assert.equal(entryURL("en", "https://evil.invalid"), "https://en.wiktionary.org/wiki/https%3A//evil.invalid");
});

test("clip enforces exact code-point limits and readTime tolerates invalid timezones", () => {
  assert.equal(Array.from(clip("a".repeat(20), 10)).length, 10);
  assert.equal(clip("a".repeat(20), 10), "aaaaaaaaa…");
  assert.equal(Array.from(clip("😀".repeat(10), 5)).length, 5);
  assert.equal(clip("hello", 1), "h");
  assert.equal(clip("hello", 0), "");
  assert.equal(clip("hello", -5), "");
  assert.equal(clip("hello", Number.NaN), "");
  assert.equal(Array.from(clip("hello", 3.9)).length, 3);
  assert.ok(readTime(Date.UTC(2026, 10, 10), "", "en").length > 0);
  assert.ok(readTime(Date.UTC(2026, 10, 10), "Invalid/Zone", "zh-Hans").includes("2026"));
  assert.ok(readTime(Date.UTC(2026, 10, 10), "UTC", "").length > 0);
  assert.equal(readTime(Number.NaN, "UTC", "en"), "");
  assert.equal(readTime(Number.POSITIVE_INFINITY, "UTC", "en"), "");
});

test("state and legacy callback values preserve validated queries", () => {
  const initial = readState(undefined);
  const found = transition(initial, "lookup", '{"word":"hello"}');
  assert.equal(found.found, true);
  assert.equal(readState(encodeState(found)).q, "hello");
  for (const raw of [JSON.stringify({ q: "../outside", found: true }), JSON.stringify({ q: "a#b", found: true }), JSON.stringify({ q: "x".repeat(121), found: true }), JSON.stringify({ q: "hello\n", found: true }), JSON.stringify({ q: "\uD800", found: true })]) {
    assert.deepEqual(readState(raw), initial);
  }
  for (const values of ["hello", "not-json", "null", "[]", 1, { word: ["hello"] }, { word: "x".repeat(121) }, { word: "\uD800" }]) assert.equal(transition(initial, "lookup", values).found, false);
  assert.equal(transition(found, "home").found, false);
});
