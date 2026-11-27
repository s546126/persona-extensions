import { test } from "node:test";
import assert from "node:assert/strict";
import { Renderer } from "@persona/extension/ui";
import { clip, gemName, gemPath, httpsURL, parseGem, parseSearch, queryText, ReadError, searchPath } from "../src/model.ts";
import { dateText } from "../src/i18n.ts";
import { gemTransition, encodeState, initialState, page, readState, transition } from "../src/page.tsx";
import { RubyGems } from "../src/service.ts";
import { detailResponse, Fixture, rack, rails } from "./fixtures.ts";

test("official JSON requests bound queries and gem names", () => {
  const url = new URL("https://rubygems.org" + searchPath("ruby rails"));
  assert.equal(url.searchParams.get("query"), "ruby rails");
  assert.equal(gemPath("rails"), "/api/v1/gems/rails.json");
  for (const value of ["", "a/b", "ruby rails", "a\n", "../secret", 42]) assert.throws(() => gemName(value), ReadError);
  for (const value of ["", "x\n", 42, "x".repeat(101)]) assert.throws(() => queryText(value), ReadError);
});

test("search keeps bounded metadata and rejects duplicate or invalid gems", () => {
  const result = parseSearch([rails, rack], 123);
  assert.equal(result.gems[0].name, "rails");
  assert.equal(result.gems[0].version, "8.1.4.1");
  assert.equal(result.gems[0].totalDownloads, 815065467);
  assert.equal(result.gems[0].repository, "https://github.com/rails/rails/tree/v8.1.4.1");
  assert.equal(result.gems[1].repository, undefined, "an optional HTTP link is omitted, not upgraded");
  for (const change of [{ name: "bad/name" }, { project_uri: "https://attacker.invalid/gem" }, { downloads: -1 }, { version: "1.0.0\n" }, { licenses: [42] }]) {
    assert.throws(() => parseSearch([{ ...rails, ...change }], 0), ReadError);
  }
  assert.throws(() => parseSearch([rails, rails], 0), ReadError);
  assert.equal(parseSearch([{ ...rails, source_code_uri: "javascript:alert(1)" }], 0).gems[0].repository, undefined);
});

test("current version carries its license, release date, yanked state and dependencies", () => {
  const result = parseGem(detailResponse, 0);
  assert.equal(result.install, "gem install rails -v 8.1.4.1");
  assert.equal(result.license, "MIT");
  assert.equal(result.yanked, false);
  assert.equal(result.versionDownloads, 1388621);
  assert.equal(result.published, "2026-11-04T02:47:52.512Z");
  assert.deepEqual(result.dependencies[0], { name: "actionpack", requirements: "= 8.1.4.1" });
});

test("invalid dependency and missing current metadata fail closed", () => {
  assert.throws(() => parseGem({ ...detailResponse, dependencies: { runtime: [{ name: "other" }] } }, 0), ReadError);
  assert.throws(() => parseGem({ ...detailResponse, dependencies: { runtime: [{ name: "../secret", requirements: ">= 1" }] } }, 0), ReadError);
  assert.throws(() => parseGem({ ...detailResponse, project_uri: "https://rubygems.org/gems/other" }, 0), ReadError);
  const yanked = parseGem({ ...detailResponse, yanked: true }, 0);
  assert.equal(yanked.yanked, true);
  assert.equal(parseGem({ ...detailResponse, licenses: [] }, 0).license, "Unspecified");
});

test("page state accepts only validated search results and HTTPS metadata", () => {
  const state = { q: "rails", found: true };
  const result = parseSearch([rails, rack], 0);
  const detail = gemTransition(state, result, "gem:rack");
  assert.equal(detail.gem?.name, "rack");
  assert.equal(gemTransition(state, result, "gem:other"), state);
  assert.equal(transition(state, "gem:rails"), state);
  assert.deepEqual(readState(encodeState(detail)), detail);
  assert.deepEqual(readState(encodeState({ ...detail, gem: { ...detail.gem!, repository: "javascript:alert(1)" } })), initialState);
  assert.equal(transition(state, "search", { query: "rails" }).found, true);
});

test("clip, dateText, URL length, lone surrogates and duplicate runtime dependencies are hardened", () => {
  assert.equal(clip("abcdef", 4), "abc…");
  assert.equal(Array.from(clip("abcdef", 4)).length, 4);
  assert.equal(clip("😀😀😀😀", 3), "😀😀…");
  assert.equal(clip("abcdef", 0), "");
  assert.equal(clip("abcdef", -5), "");
  assert.equal(clip("abcdef", Number.NaN), "");
  assert.equal(dateText("not-a-date"), "");
  assert.throws(() => queryText("bad\ud800query"), ReadError);
  assert.equal(httpsURL("https://example.com/" + "a".repeat(4100)), undefined);

  // Duplicate runtime dependency names must be rejected by parseGem so view keys never collide.
  assert.throws(() => parseGem({
    ...detailResponse,
    dependencies: {
      development: [],
      runtime: [
        { name: "actionpack", requirements: ">= 7.0" },
        { name: "actionpack", requirements: "< 9.0" },
      ],
    },
  }, 0), ReadError);
});

test("page enforces CheckView text (<=2000), item, key, state and frame budgets", () => {
  const service = new RubyGems(new Fixture().fetch, {}, { locale: "en", limits: { gap: 0 } });
  const render = (node: ReturnType<typeof page>) => {
    const renderer = new Renderer({ authorize: () => false });
    try { return renderer.mount(node).view; }
    finally { renderer.dispose(); }
  };

  // Detail with oversized description and duplicate dependencies passed directly to page() must stay valid.
  const baseDetail = parseGem(detailResponse, 0);
  const detailView = render(page({
    service,
    state: { q: "rails", found: true, gem: parseSearch([rails], 0).gems[0] },
    loaded: {
      kind: "detail",
      result: {
        gem: {
          ...baseDetail,
          description: "D".repeat(3000),
          dependencies: [
            { name: "actionpack", requirements: ">= 7.0" },
            { name: "actionpack", requirements: "< 9.0" },
          ],
        },
        at: 0,
      },
    },
    uiVersion: 5,
  }));
  const detailKeys = new Set<string>();
  for (const section of detailView.sections) {
    assert.ok(Array.from(section.key).length <= 128);
    assert.equal(detailKeys.has(section.key), false);
    detailKeys.add(section.key);
    for (const item of section.items) {
      assert.ok(typeof item.key === "string" && Array.from(item.key).length <= 128);
      assert.equal(detailKeys.has(item.key), false);
      detailKeys.add(item.key);
      if (item.type === "text") assert.ok(Array.from(item.text).length <= 2000, `text exceeds 2000 runes: ${Array.from(item.text).length}`);
    }
  }

  // Duplicate and many gems passed directly to page() must respect ItemsMost = 300 and 512 KiB frame budget.
  const oneGem = parseSearch([rails], 0).gems[0];
  const dupView = render(page({
    service,
    state: { q: "rails", found: true },
    loaded: { kind: "results", result: { gems: [oneGem, { ...oneGem }], at: 0 } },
    uiVersion: 5,
  }));
  assert.ok(dupView.sections.length > 0);

  const manyGems = Array.from({ length: 120 }, (_, i) => ({
    ...oneGem,
    name: `gem-${i}`,
    url: `https://rubygems.org/gems/gem-${i}`,
    description: "d".repeat(500),
  }));
  const largeView = render(page({
    service,
    state: { q: "gem", found: true },
    loaded: { kind: "results", result: { gems: manyGems, at: 0 } },
    uiVersion: 5,
  }));
  const totalItems = largeView.sections.reduce((sum, s) => sum + s.items.length, 0);
  assert.ok(totalItems <= 300, `items exceeded 300: ${totalItems}`);
  assert.ok(new TextEncoder().encode(JSON.stringify(largeView)).byteLength <= 512 * 1024);

  // Invalid state passed directly to page() must be normalized.
  const badStateView = render(page({ service, state: { q: "bad\nquery", found: true }, loaded: { kind: "empty" }, uiVersion: 5 }));
  assert.equal(badStateView.state, encodeState(initialState));
});
