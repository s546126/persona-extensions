import { test } from "node:test";
import assert from "node:assert/strict";
import { Renderer } from "@persona/extension/ui";
import { clip, packageID, parseCatalog, parseLeaf, parseNuspec, parseSearch, queryText, ReadError, registrationPath, searchPath } from "../src/model.ts";
import { dateText } from "../src/i18n.ts";
import { encodeState, initialState, packageTransition, page, readState, transition } from "../src/page.tsx";
import { Nuget } from "../src/service.ts";
import { catalogResponse, Fixture, leafResponse, nuspecResponse, stable } from "./fixtures.ts";

test("official search and registration paths validate IDs, queries and stable versions", () => {
  const url = new URL("https://azuresearch-usnc.nuget.org" + searchPath("Newtonsoft.Json", 12));
  assert.equal(url.searchParams.get("q"), "Newtonsoft.Json");
  assert.equal(url.searchParams.get("take"), "12");
  assert.equal(registrationPath("Newtonsoft.Json", "13.0.4"), "/v3/registration5-semver1/newtonsoft.json/13.0.4.json");
  for (const value of ["", "a/b", "a b", "a..b", "../secret", "a\n", 42]) assert.throws(() => packageID(value), ReadError);
  for (const value of ["", "x\n", 42, "x".repeat(101)]) assert.throws(() => queryText(value), ReadError);
});

test("search keeps the reported stable version, not the highest prerelease", () => {
  const result = parseSearch({ data: [stable], totalHits: 1 }, 123);
  assert.equal(result.packages[0].version, "13.0.4");
  assert.equal(result.packages[0].license, "See license link");
  assert.equal(result.packages[0].totalDownloads, 9780413829);
  assert.equal(result.at, 123);
  for (const change of [{ id: "bad/id" }, { totalDownloads: -1 }, { version: "1.0.0\n" }]) {
    assert.throws(() => parseSearch({ data: [{ ...stable, ...change }], totalHits: 1 }, 0), ReadError);
  }
  assert.equal(parseSearch({ data: [{ ...stable, projectUrl: "http://www.newtonsoft.com/json" }], totalHits: 1 }, 0).packages[0].projectURL, undefined);
  assert.equal(parseSearch({ data: [{ ...stable, projectUrl: "javascript:alert(1)" }], totalHits: 1 }, 0).packages[0].projectURL, undefined);
  assert.equal(parseSearch({ data: [{ ...stable, projectUrl: "https://www.newtonsoft.com/json" }], totalHits: 1 }, 0).packages[0].projectURL, "https://www.newtonsoft.com/json");
  assert.throws(() => parseSearch({ data: [stable, stable], totalHits: 2 }, 0), ReadError);
});

test("registration leaf validates package identity and listed state", () => {
  assert.deepEqual(parseLeaf(leafResponse, "Newtonsoft.Json", "13.0.4"), { listed: true });
  assert.throws(() => parseLeaf({ ...leafResponse, registration: "https://api.nuget.org/v3/registration5-semver1/other/index.json" }, "Newtonsoft.Json", "13.0.4"), ReadError);
});

test("nuspec metadata retains license, target frameworks and dependency counts", () => {
  const result = parseNuspec(nuspecResponse, "Newtonsoft.Json", "13.0.4", true);
  assert.equal(result.install, "dotnet add package Newtonsoft.Json --version 13.0.4");
  assert.equal(result.license, "MIT");
  assert.equal(result.dependencies, 10);
  assert.deepEqual(result.frameworks, [".NETFramework2.0", ".NETFramework3.5", ".NETFramework4.0", ".NETFramework4.5", ".NETStandard1.0", ".NETStandard1.3", "net6.0", ".NETStandard2.0"]);
  assert.equal(result.listed, true);
  assert.throws(() => parseNuspec(nuspecResponse.replace("13.0.4", "14.0.1-beta2"), "Newtonsoft.Json", "13.0.4", true), ReadError);
  assert.throws(() => parseNuspec(nuspecResponse.replace("Newtonsoft.Json", "Other"), "Newtonsoft.Json", "13.0.4", true), ReadError);
  assert.equal(parseNuspec(nuspecResponse, "Newtonsoft.Json", "13.0.4", false).listed, false);
});

test("page state accepts only validated search results and HTTPS metadata", () => {
  const state = { q: "Newtonsoft.Json", found: true };
  const result = parseSearch({ data: [stable], totalHits: 1 }, 0);
  const detail = packageTransition(state, result, "package:Newtonsoft.Json");
  assert.equal(detail.package?.id, "Newtonsoft.Json");
  assert.equal(packageTransition(state, result, "package:Other"), state);
  assert.equal(transition(state, "package:Newtonsoft.Json"), state);
  assert.deepEqual(readState(encodeState(detail)), detail);
  assert.deepEqual(readState(encodeState({ ...detail, package: { ...detail.package!, projectURL: "javascript:alert(1)" } })), initialState);
  assert.equal(transition(state, "search", { query: "Newtonsoft.Json" }).found, true);
});

test("clip handles rune limits, surrogate pairs, non-positive bounds and dateText handles invalid dates", () => {
  assert.equal(clip("abcdef", 4), "abc…");
  assert.equal(Array.from(clip("abcdef", 4)).length, 4);
  assert.equal(clip("😀😀😀😀", 3), "😀😀…");
  assert.equal(clip("abcdef", 0), "");
  assert.equal(clip("abcdef", -5), "");
  assert.equal(clip("abcdef", Number.NaN), "");
  assert.equal(dateText("not-a-date"), "");
  assert.throws(() => queryText("bad\ud800query"), ReadError);
});

test("parseNuspec handles case-insensitive closing group tags and rejects overlong URLs", () => {
  const upperClose = nuspecResponse.replace("</group>", "</GROUP>");
  const parsed = parseNuspec(upperClose, "Newtonsoft.Json", "13.0.4", true);
  assert.equal(parsed.dependencies, 10);
  const longURL = "https://example.com/" + "a".repeat(4100);
  assert.throws(() => parseSearch({ data: [{ ...stable, projectUrl: longURL }], totalHits: 1 }, 0), ReadError);
});

test("page enforces CheckView key, action.id, text, item, state and frame budgets", () => {
  const service = new Nuget(new Fixture().fetch, {}, { locale: "en", limits: { gap: 0 } });
  const render = (node: ReturnType<typeof page>) => {
    const renderer = new Renderer({ authorize: () => false });
    try { return renderer.mount(node).view; }
    finally { renderer.dispose(); }
  };

  // 128-char NuGet package ID must keep all section/item keys and action.id <= 128 runes, and navigate cleanly.
  const longID = "A." + "x".repeat(126);
  const longPkg = parseSearch({ data: [{ ...stable, id: longID }], totalHits: 1 }, 0).packages[0];
  const longSearch = { packages: [longPkg, { ...longPkg }], total: 2, at: 0 };
  const resultsView = render(page({ service, state: { q: "longpkg", found: true }, loaded: { kind: "results", result: longSearch }, uiVersion: 5 }));
  const seenKeys = new Set<string>();
  let openActionID = "";
  for (const section of resultsView.sections) {
    assert.ok(Array.from(section.key).length <= 128, `section key too long: ${section.key.length}`);
    assert.equal(seenKeys.has(section.key), false, `duplicate section key: ${section.key}`);
    seenKeys.add(section.key);
    for (const item of section.items) {
      assert.ok(typeof item.key === "string" && Array.from(item.key).length <= 128, `item key too long: ${item.key?.length}`);
      assert.equal(seenKeys.has(item.key), false, `duplicate item key: ${item.key}`);
      seenKeys.add(item.key);
      if ("action" in item && item.action) {
        assert.ok(Array.from(item.action.id).length <= 128, `action id too long: ${item.action.id.length}`);
        if (item.action.id.startsWith("package:")) openActionID = item.action.id;
      }
    }
  }
  assert.ok(openActionID.startsWith("package:"));
  const transitioned = packageTransition({ q: "longpkg", found: true }, longSearch, openActionID);
  assert.equal(transitioned.package?.id, longID);

  // Detail with 40 long target frameworks must still respect StringMost = 2000 runes.
  const longFrameworks = Array.from({ length: 40 }, (_, i) => `net8.0-${String(i).padStart(2, "0")}-${"f".repeat(85)}`);
  const detailView = render(page({
    service,
    state: { q: "Newtonsoft.Json", found: true, package: parseSearch({ data: [stable], totalHits: 1 }, 0).packages[0] },
    loaded: { kind: "detail", result: { package: { ...parseNuspec(nuspecResponse, "Newtonsoft.Json", "13.0.4", true), frameworks: longFrameworks }, at: 0 } },
    uiVersion: 5,
  }));
  for (const section of detailView.sections) {
    for (const item of section.items) {
      if (item.type === "text") assert.ok(Array.from(item.text).length <= 2000, `text exceeds 2000 runes: ${Array.from(item.text).length}`);
    }
  }

  // Many packages passed directly to page() must respect ItemsMost = 300 and 512 KiB frame budget.
  const many = Array.from({ length: 120 }, (_, i) => ({
    ...parseSearch({ data: [{ ...stable, id: `Pkg.${i}` }], totalHits: 1 }, 0).packages[0],
    description: "d".repeat(500),
  }));
  const largeView = render(page({ service, state: { q: "Pkg", found: true }, loaded: { kind: "results", result: { packages: many, total: 120, at: 0 } }, uiVersion: 5 }));
  const totalItems = largeView.sections.reduce((sum, s) => sum + s.items.length, 0);
  assert.ok(totalItems <= 300, `items exceeded 300: ${totalItems}`);
  assert.ok(new TextEncoder().encode(JSON.stringify(largeView)).byteLength <= 512 * 1024);

  // Unvalidated state passed directly to page() must not leak raw invalid query/state into View.
  const badStateView = render(page({ service, state: { q: "bad\nquery", found: true }, loaded: { kind: "empty" }, uiVersion: 5 }));
  assert.equal(badStateView.state, encodeState(initialState));
});
