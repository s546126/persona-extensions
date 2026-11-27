import { test } from "node:test";
import assert from "node:assert/strict";
import { clip, configFrom, parseItems, queryText, ReadError, searchPath } from "../src/model.ts";
import { readTime } from "../src/i18n.ts";
import { response } from "./fixtures.ts";
import { encodeState, readState, transition } from "../src/page.tsx";

test("configuration and media filters are bounded", () => {
  assert.deepEqual(configFrom(undefined), { media: "all" });
  assert.equal(configFrom({ media: "texts" }).media, "texts");
  for (const value of [{ media: "everything" }, { media: 1 }, [], "texts"]) assert.throws(() => configFrom(value), ReadError);
  assert.match(searchPath("Pride and Prejudice", "texts"), /AND\+mediatype%3Atexts/);
  assert.ok(!new URL("https://archive.org" + searchPath("Pride and Prejudice", "all")).searchParams.get("q")!.includes("mediatype"));
  assert.throws(() => searchPath("valid", "all", 0), ReadError);
  assert.throws(() => searchPath("valid", "all", 21), ReadError);
});

test("catalog items retain metadata and only canonical archive links", () => {
  const result = parseItems(response, 1000);
  assert.equal(result.items[0].title, "Pride and Prejudice");
  assert.equal(result.items[0].year, 1813);
  assert.equal(result.items[0].url, "https://archive.org/details/prideprejudice_0000");
  assert.equal(result.items[1].media, "audio");
  assert.equal(result.items[1].downloads, 42);
  assert.equal(parseItems({ response: { numFound: 1, docs: [{ ...response.response.docs[0], description: ["First paragraph", "Second paragraph"] }] } }, 0).items[0].description, "First paragraph · Second paragraph");
  const original = response.response.docs[0];
  assert.throws(() => parseItems({ response: { numFound: 1, docs: [{ ...original, identifier: "https://attacker.invalid" }] } }, 0), ReadError);
  assert.throws(() => parseItems({ response: { numFound: 1, docs: [{ ...original, mediatype: "unknown" }] } }, 0), ReadError);
  assert.throws(() => parseItems({ response: { numFound: 1, docs: [original, original] } }, 0), ReadError);
});

test("clip enforces exact code-point limits and readTime tolerates invalid timezones", () => {
  assert.equal(Array.from(clip("a".repeat(20), 10)).length, 10);
  assert.equal(clip("a".repeat(20), 10), "aaaaaaaaa…");
  assert.equal(Array.from(clip("😀".repeat(10), 5)).length, 5);
  assert.equal(clip("😀".repeat(10), 5), "😀😀😀😀…");
  assert.equal(clip("hello", 1), "h");
  assert.equal(clip("hello", 0), "");
  assert.equal(clip("hello", -5), "");
  assert.equal(clip("hello", Number.NaN), "");
  assert.equal(Array.from(clip("hello", 3.9)).length, 3);
  assert.ok(readTime(Date.UTC(2026, 10, 10), "", "en").length > 0);
  assert.ok(readTime(Date.UTC(2026, 10, 10), "Invalid/Zone", "zh-Hans").includes("2026"));
  assert.ok(readTime(Date.UTC(2026, 10, 10), "UTC", "").length > 0);
  assert.ok(readTime(Date.UTC(2026, 10, 10), "UTC", "invalid_locale_tag!!!").includes("2026"));
  assert.equal(readTime(Number.NaN, "UTC", "en"), "");
  assert.equal(readTime(Number.POSITIVE_INFINITY, "UTC", "en"), "");
});

test("query validation and state preserve only valid user input", () => {
  for (const query of ["", " ", "x".repeat(161), "bad\n", "\uD800", "\uDC00", "a\uD800b"]) assert.throws(() => queryText(query), ReadError);
  assert.equal(queryText(" Jane Austen "), "Jane Austen");
  assert.equal(queryText("😀"), "😀");
  const initial = readState(undefined);
  const found = transition(initial, "search", '{"query":"Jane Austen"}');
  assert.equal(found.found, true);
  assert.equal(readState(encodeState(found)).q, "Jane Austen");
  for (const raw of [JSON.stringify({ q: "x".repeat(161), found: true }), JSON.stringify({ q: "bad\n", found: true }), JSON.stringify({ q: "\uD800", found: true })]) {
    assert.deepEqual(readState(raw), initial);
  }
  for (const values of ["Jane", "not-json", "null", "[]", 1, { query: ["Jane"] }, { query: "x".repeat(161) }, { query: "bad\n" }, { query: "\uD800" }]) {
    assert.equal(transition(initial, "search", values).found, false);
  }
  assert.equal(transition(found, "home").found, false);
});
