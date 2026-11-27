import { test } from "node:test";
import assert from "node:assert/strict";
import { Renderer } from "@persona/extension/ui";
import { clip, parseBook, parseSearch, parseWork, queryText, ReadError, searchPath, workPath, workKey } from "../src/model.ts";
import { readTime } from "../src/i18n.ts";
import { detail, Fixture, publicBook, sample } from "./fixtures.ts";
import { encodeState, initialState, page, readState, transition } from "../src/page.tsx";
import { OpenLibrary } from "../src/service.ts";

test("search and work paths validate identifiers and query bounds", () => {
  assert.equal(workKey("OL59800W"), "OL59800W");
  assert.equal(workPath("OL59800W"), "/works/OL59800W.json");
  for (const key of ["https://attacker.invalid", "OL1W/../../x", "OL0W", "", "OL1A"]) assert.throws(() => workPath(key), ReadError);
  for (const query of ["", " ", "x".repeat(201), "bad\n"]) assert.throws(() => searchPath(query), ReadError);
  assert.match(searchPath("Pride and Prejudice"), /^\/search\.json\?q=Pride\+and\+Prejudice&limit=20&fields=/);
  assert.throws(() => searchPath("valid", 0), ReadError);
  assert.throws(() => searchPath("valid", 31), ReadError);
});

test("book metadata produces only canonical catalog and validated public edition links", () => {
  const restricted = parseBook(sample);
  assert.equal(restricted.url, "https://openlibrary.org/works/OL59800W");
  assert.equal(restricted.readingURL, undefined);
  assert.equal(restricted.access, "printdisabled");
  const open = parseBook(publicBook);
  assert.equal(open.readingURL, "https://openlibrary.org/books/ia:synapseml_gutenberg_pride_and_prejudice_by_jane_austen");
  assert.equal(parseBook({ ...publicBook, ia: ["https://attacker.invalid"] }).readingURL, undefined);
  assert.equal(parseBook({ ...publicBook, ia: ["javascript:alert(1)"] }).readingURL, undefined);
  assert.equal(parseBook({ ...publicBook, public_scan_b: false }).readingURL, undefined);
  const result = parseSearch({ numFound: 2, docs: [sample, sample, publicBook] }, 1000);
  assert.equal(result.books.length, 2);
  const work = parseWork(detail, restricted, 1000);
  assert.equal(work.description, "A synthetic description of a novel.");
  assert.deepEqual(work.subjects, ["Science fiction", "Human nature"]);
  assert.equal(parseWork({ ...detail, description: "<script>alert(1)</script>" }, restricted, 1).description, "<script>alert(1)</script>");
  assert.throws(() => parseWork({ ...detail, title: "Different book" }, restricted, 1), ReadError);
  assert.throws(() => parseWork({ ...detail, key: "/works/OL66554W" }, restricted, 1), ReadError);
});

test("service responses cannot substitute URLs, types or excessive metadata", () => {
  for (const raw of [null, [], {}, { numFound: 1 }, { numFound: 1, docs: [null] },
    { numFound: 1, docs: [{ ...sample, title: null }] }, { numFound: -1, docs: [] }]) assert.throws(() => parseSearch(raw, 0), ReadError);
  assert.throws(() => parseWork({ description: { type: "/type/html", value: "unsafe" } }, parseBook(sample), 0), ReadError);
});

test("state round-trip preserves only validated server-owned book navigation", () => {
  const initial = readState(undefined);
  assert.equal(initial.v, "discover");
  const searched = transition(initial, "search", '{"query":"Pride and Prejudice"}');
  assert.equal(searched.v, "search");
  const selected = parseBook(publicBook);
  const work = transition(searched, `work:${selected.key}`);
  const withBook = { ...work, b: selected };
  const opened = transition(withBook, `work:${selected.key}`);
  assert.equal(opened.v, "work");
  const state = readState(encodeState(opened));
  assert.equal(state.b?.url, selected.url);
  assert.equal(transition(opened, "back").v, "search");
  for (const values of ["Earth", "not-json", "null", "[]", 1, { query: ["Earth"] }]) assert.equal(transition(initial, "search", values).v, "discover");
  const bad = { ...opened, b: { ...selected, url: "https://attacker.invalid" } };
  assert.equal(readState(JSON.stringify(bad)).v, "discover");
  const external = { ...opened, b: { ...selected, readingURL: "https://attacker.invalid/x" } };
  assert.equal(readState(JSON.stringify(external)).v, "discover");
});

test("clip, readTime, upstream key errors, and state validation handle edge cases safely", () => {
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

  // Upstream malformed work key must throw bad_answer, not invalid.
  assert.throws(() => parseBook({ ...sample, key: "/works/BAD" }), (e: unknown) => e instanceof ReadError && e.code === "bad_answer");
  assert.throws(() => parseWork({ ...detail, key: "/works/BAD" }, parseBook(sample), 0), (e: unknown) => e instanceof ReadError && e.code === "bad_answer");

  // Overlong or multi-line UI search input produces a valid queryText() or rejects control chars.
  const longSearch = transition(initialState, "search", { query: "a".repeat(250) });
  assert.equal(longSearch.v, "search");
  assert.equal(queryText(longSearch.q), longSearch.q);
  const multiline = transition(initialState, "search", { query: "Pride\nand Prejudice" });
  assert.equal(multiline.v, "search");
  assert.equal(queryText(multiline.q), "Pride and Prejudice");
  assert.equal(transition(initialState, "search", { query: "bad\u0000query" }).v, "discover");
  assert.deepEqual(readState(JSON.stringify({ v: "search", q: "bad\nquery", k: "" })), initialState);
  assert.deepEqual(readState(JSON.stringify({ v: "search", q: "a".repeat(250), k: "" })), initialState);

  // Work state with mismatched k vs b.key must reset to initialState.
  const selected = parseBook(publicBook);
  assert.deepEqual(readState(JSON.stringify({ v: "work", q: "Pride", k: "OL59800W", b: selected })), initialState);
});

test("page enforces CheckView text (<=2000), item, key, state and frame budgets", () => {
  const service = new OpenLibrary(new Fixture().fetch, {}, { locale: "en", limits: { gap: 0 } });
  const render = (node: ReturnType<typeof page>) => {
    const renderer = new Renderer({ authorize: () => false });
    try { return renderer.mount(node).view; }
    finally { renderer.dispose(); }
  };

  // Work detail with >2000-char description (up to 8000 in model) must clip <Text> to <= 2000 runes.
  const base = parseBook(sample);
  const longWork = parseWork({ ...detail, description: "D".repeat(5000) }, base, Date.UTC(2026, 10, 10));
  const detailView = render(page({
    service,
    state: { v: "work", q: "synthetic", k: base.key, b: base },
    loaded: { kind: "work", work: longWork },
    uiVersion: 5,
  }));
  for (const section of detailView.sections) {
    for (const item of section.items) {
      if (item.type === "text") assert.ok(Array.from(item.text).length <= 2000, `text exceeds 2000 runes: ${Array.from(item.text).length}`);
    }
  }

  // Duplicate books & many books passed directly to page() must stay within ItemsMost = 300 and 512 KiB.
  const dupView = render(page({
    service,
    state: { v: "search", q: "synthetic", k: "" },
    loaded: { kind: "search", result: { books: [base, { ...base }], total: 2, at: 0 } },
    uiVersion: 5,
  }));
  const seenKeys = new Set<string>();
  for (const section of dupView.sections) {
    assert.equal(seenKeys.has(section.key), false);
    seenKeys.add(section.key);
    for (const item of section.items) {
      assert.ok(typeof item.key === "string" && Array.from(item.key).length <= 128);
      assert.equal(seenKeys.has(item.key), false);
      seenKeys.add(item.key);
    }
  }

  const manyBooks = Array.from({ length: 320 }, (_, i) => ({
    ...base,
    key: `OL${i + 1}W`,
    title: `Book ${i} ` + "x".repeat(300),
    url: `https://openlibrary.org/works/OL${i + 1}W`,
  }));
  const largeView = render(page({
    service,
    state: { v: "search", q: "synthetic", k: "" },
    loaded: { kind: "search", result: { books: manyBooks, total: 320, at: 0 } },
    uiVersion: 5,
  }));
  const totalItems = largeView.sections.reduce((sum, s) => sum + s.items.length, 0);
  assert.ok(totalItems <= 300, `items exceeded 300: ${totalItems}`);
  assert.ok(new TextEncoder().encode(JSON.stringify(largeView)).byteLength <= 512 * 1024);

  // Invalid state passed directly to page() must be normalized.
  const badStateView = render(page({ service, state: { v: "search", q: "bad\nquery", k: "" }, loaded: { kind: "empty" }, uiVersion: 5 }));
  assert.equal(badStateView.state, encodeState(initialState));
});
