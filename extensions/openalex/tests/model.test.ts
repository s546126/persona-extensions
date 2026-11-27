import { test } from "node:test";
import assert from "node:assert/strict";
import { Renderer } from "@persona/extension/ui";
import { clip, parseWorks, queryText, ReadError, searchPath } from "../src/model.ts";
import { readTime } from "../src/i18n.ts";
import { Fixture, response } from "./fixtures.ts";
import { encodeState, initialState, page, readState, transition } from "../src/page.tsx";
import { OpenAlex } from "../src/service.ts";

test("research metadata retains citation, open access and canonical work links", () => {
  const result = parseWorks(response, 1000);
  assert.equal(result.works[0].title, "A Survey of Large Language Models");
  assert.equal(result.works[0].citations, 8095);
  assert.equal(result.works[0].openAccess, true);
  assert.equal(result.works[0].id, "https://openalex.org/W4362515116");
  assert.equal(result.works[0].license, "cc-by");
  assert.equal(result.works[1].oaURL, undefined);
  assert.equal(result.works[1].openAccess, false);
  const original = response.results[0];
  assert.throws(() => parseWorks({ meta: { count: 1 }, results: [{ ...original, id: "https://attacker.invalid/W1" }] }, 0), ReadError);
  assert.throws(() => parseWorks({ meta: { count: 1 }, results: [{ ...original, doi: "http://attacker.invalid" }] }, 0), ReadError);
  assert.throws(() => parseWorks({ meta: { count: 1 }, results: [original, original] }, 0), ReadError);
  // A publisher's plain-http open-access link (the first CRISPR result's is http://www.cell.com/…) is left out, not a failure.
  const cell = { ...original, open_access: { ...original.open_access, oa_url: "http://www.cell.com/article/S0092867415017055/pdf" } };
  const listed = parseWorks({ meta: { count: 2 }, results: [cell, response.results[1]] }, 0);
  assert.equal(listed.works.length, 2);
  assert.equal(listed.works[0].oaURL, undefined);
  assert.equal(listed.works[0].doi, original.doi);
  for (const oa_url of ["https://user:secret@example.com/paper.pdf", "not a url", "ftp://example.com/paper.pdf"]) {
    assert.throws(() => parseWorks({ meta: { count: 1 }, results: [{ ...original, open_access: { ...original.open_access, oa_url } }] }, 0), ReadError);
  }
});

test("queries and results are bounded and control characters are rejected", () => {
  for (const query of ["", " ", "x".repeat(201), "bad\n"]) assert.throws(() => queryText(query), ReadError);
  assert.equal(queryText(" large language models "), "large language models");
  assert.throws(() => searchPath("valid", 0), ReadError);
  assert.throws(() => searchPath("valid", 21), ReadError);
});

test("state and legacy callback values preserve validated research searches", () => {
  const initial = readState(undefined);
  const found = transition(initial, "search", '{"query":"large language models"}');
  assert.equal(found.found, true);
  assert.equal(readState(encodeState(found)).q, "large language models");
  for (const values of ["models", "not-json", "null", "[]", 1, { query: ["models"] }]) assert.equal(transition(initial, "search", values).found, false);
  assert.equal(transition(found, "home").found, false);
});

test("clip, queryText, readTime and URL validation handle edge cases safely", () => {
  assert.equal(clip("abcdef", 4), "abc…");
  assert.equal(Array.from(clip("abcdef", 4)).length, 4);
  assert.equal(clip("😀😀😀😀", 3), "😀😀…");
  assert.equal(clip("abcdef", 0), "");
  assert.equal(clip("abcdef", -5), "");
  assert.equal(clip("abcdef", Number.NaN), "");
  assert.throws(() => queryText("bad\ud800query"), ReadError);
  assert.equal(readTime(Number.NaN, "UTC", "en"), "");
  assert.ok(readTime(Date.UTC(2026, 10, 10), "Invalid/Timezone", "en").length > 0);
  assert.ok(readTime(Date.UTC(2026, 10, 10), "UTC", "not_a_valid_locale!!!").length > 0);

  const original = response.results[0];
  assert.throws(() => parseWorks({ meta: { count: 1 }, results: [{ ...original, doi: "https://doi.org:8443/10.1000/x" }] }, 0), ReadError);
  assert.throws(() => parseWorks({ meta: { count: 1 }, results: [{ ...original, doi: "https://doi.org/" + "a".repeat(4100) }] }, 0), ReadError);

  // Overlong or multi-line UI search input must always produce a valid queryText() or reject safely.
  const longQuery = transition(initialState, "search", { query: "a".repeat(250) });
  assert.equal(longQuery.found, true);
  assert.equal(queryText(longQuery.q), longQuery.q);
  const multiline = transition(initialState, "search", { query: "Large\nLanguage Models" });
  assert.equal(multiline.found, true);
  assert.equal(queryText(multiline.q), "Large Language Models");
  assert.equal(transition(initialState, "search", { query: "bad\u0000query" }).found, false);
  assert.deepEqual(readState(JSON.stringify({ q: "bad\nquery", found: true })), initialState);
  assert.deepEqual(readState(JSON.stringify({ q: "a".repeat(250), found: true })), initialState);
});

test("page enforces CheckView text, item, key, state and frame budgets", () => {
  const service = new OpenAlex(new Fixture().fetch, {}, { locale: "en", limits: { gap: 0 } });
  const render = (node: ReturnType<typeof page>) => {
    const renderer = new Renderer({ authorize: () => false });
    try { return renderer.mount(node).view; }
    finally { renderer.dispose(); }
  };

  // Duplicate works and huge text fields passed directly to page() must not break CheckView.
  const parsed = parseWorks(response, Date.UTC(2026, 10, 10));
  const oversizedWork = { ...parsed.works[0], title: "T".repeat(3000), license: "L".repeat(3000), source: "S".repeat(3000) };
  const dupView = render(page({
    service,
    state: { q: "synthetic", found: true },
    loaded: { kind: "result", result: { works: [oversizedWork, { ...oversizedWork }], total: 2, at: Date.UTC(2026, 10, 10) } },
    uiVersion: 5,
  }));
  const seenKeys = new Set<string>();
  for (const section of dupView.sections) {
    assert.ok(Array.from(section.key).length <= 128);
    assert.equal(seenKeys.has(section.key), false);
    seenKeys.add(section.key);
    if (section.title) assert.ok(Array.from(section.title).length <= 2000);
    if (section.footer) assert.ok(Array.from(section.footer).length <= 2000);
    for (const item of section.items) {
      assert.ok(typeof item.key === "string" && Array.from(item.key).length <= 128);
      assert.equal(seenKeys.has(item.key), false);
      seenKeys.add(item.key);
      if (item.type === "text") assert.ok(Array.from(item.text).length <= 2000);
    }
  }

  // Many works passed directly must stay within ItemsMost = 300 and 512 KiB frame budget.
  const manyWorks = Array.from({ length: 120 }, (_, i) => ({
    ...parsed.works[0],
    id: `https://openalex.org/W${i + 1}`,
    title: `Paper ${i} ` + "x".repeat(400),
  }));
  const largeView = render(page({
    service,
    state: { q: "synthetic", found: true },
    loaded: { kind: "result", result: { works: manyWorks, total: 120, at: Date.UTC(2026, 10, 10) } },
    uiVersion: 5,
  }));
  const totalItems = largeView.sections.reduce((sum, s) => sum + s.items.length, 0);
  assert.ok(totalItems <= 300, `items exceeded 300: ${totalItems}`);
  assert.ok(new TextEncoder().encode(JSON.stringify(largeView)).byteLength <= 512 * 1024);

  // Unvalidated state passed directly to page() must be normalized.
  const badStateView = render(page({ service, state: { q: "bad\nquery", found: true }, loaded: { kind: "empty" }, uiVersion: 5 }));
  assert.equal(badStateView.state, encodeState(initialState));
});
