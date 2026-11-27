import { test } from "node:test";
import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import { resolve } from "node:path";
import { pathToFileURL } from "node:url";
import { createHash } from "node:crypto";
import { connect, type Program } from "@persona/extension";
import type { Snapshot } from "@persona/extension/ui";
import { Fixture, interaction, syntheticArticle, until } from "./fixtures.ts";
async function setup(initialize = true) {
  const bundle = await import(pathToFileURL(resolve("build/wikipedia.mjs")).href);
  const fixture = new Fixture(), frames: any[] = [];
  const p = connect(bundle.createProgram(fixture.fetch, { now: () => fixture.clock, monotonic: () => fixture.clock, limits: { gap: 0 } }) as Program, line => frames.push(JSON.parse(line)));
  let id = 0;
  const call = async (method: string, params: unknown = {}) => {
    const request = String(++id); p.line(JSON.stringify({ id: request, method, params })); await p.drain();
    const frame = frames.find(frame => frame.id === request); assert.ok(frame); return frame;
  };
  if (initialize) await call("initialize", { protocol: 1, config: {}, hub: { locale: "en", timezone: "UTC" } });
  return { bundle, fixture, frames, p, call };
}
const body = (snapshot: Snapshot) => JSON.stringify(snapshot.view);
const settled = async (h: Awaited<ReturnType<typeof setup>>, session: string, token: string): Promise<Snapshot> => {
  const frame = await until(() => h.call("ui.page.snapshot", { session }), frame => !!frame.error || JSON.stringify(frame.result).includes(token));
  assert.equal(frame.error, undefined, JSON.stringify(frame.error)); return frame.result;
};
test("source artifact and package have verified identity and no runtime imports", async () => {
  const manifest = JSON.parse(await readFile("build/package/persona-extension.json", "utf8"));
  const bytes = await readFile("build/package/" + manifest.runtime.source.bundle);
  assert.equal(manifest.id, "wikipedia"); assert.equal(manifest.runtime.mode, "deno"); assert.equal(manifest.runtime.command, undefined);
  assert.equal(manifest.runtime.source.sha256, createHash("sha256").update(bytes).digest("hex"));
  assert.equal(manifest.contributes.ui[0].id, "today"); assert.equal(manifest.contributes.connectors[0].capabilities.includes("actions"), false);
  assert.deepEqual(manifest.permissions.network, ["en.wikipedia.org", "zh.wikipedia.org", "ja.wikipedia.org"]);
  assert.ok(bytes.length > 0 && bytes.length < (8 << 20));
  const bundle = await import(pathToFileURL(resolve("build/wikipedia.mjs")).href);
  assert.equal(bundle.default.fetch, undefined); assert.equal(bundle.default.id, "wikipedia");
});
test("protocol initialization, localized tools and unknown contributions are SDK-owned", async () => {
  const h = await setup(false);
  try {
    assert.equal((await h.call("connector.status")).error.code, "not_initialized");
    assert.equal((await h.call("initialize", { protocol: 2 })).error.code, "wrong_protocol");
    assert.ok((await h.call("initialize", { protocol: 1, config: { language: "fr" } })).error);
    assert.equal((await h.call("initialize", { protocol: 1, config: {}, hub: { locale: "ja" } })).result.id, "wikipedia");
    assert.deepEqual((await h.call("ping")).result, {});
    assert.equal((await h.call("connector.status", { contribution: "missing" })).error.code, "no_contribution");
    assert.equal((await h.call("connector.unsupported", {})).error.code, "unknown_method");
    assert.ok((await h.call("connector.tools.list", {})).result.tools[0].description.includes("今日の"));
    assert.equal((await h.call("connector.tools.call", { name: "wikipedia_search", args: { query: "bad\n" } })).error.code, "invalid");
  } finally { h.p.close(); }
});
test("daily tabs, search and article navigation use native server hooks", async () => {
  const h = await setup();
  try {
    for (const version of [1, 2, 3, 5]) {
      const session = `daily-${version}`;
      await h.call("ui.page.mount", { session, params: { contribution: "today", locale: "en", ui_version: version } });
      let snapshot = await settled(h, session, "Featured article");
      assert.equal(snapshot.view.sections[1].items[0].type, version < 2 ? "row" : "segmented");
      await h.call("ui.page.event", { session, ticket: "host-ticket", interaction: version >= 2 ? interaction(snapshot, "tab", { tab: "popular" }) : interaction(snapshot, "tab-popular") });
      snapshot = await settled(h, session, "Popular article");
      assert.ok(!body(snapshot).includes("article-0-Featured_article"));
      const searchButton = snapshot.view.sections[0].items.find(item => item.key === "do-search");
      assert.ok(searchButton && "action" in searchButton && searchButton.action?.submit === true, "search submits the current field values");
      await h.call("ui.page.event", { session, ticket: "host-ticket", interaction: interaction(snapshot, "do-search", { query: "synthetic" }) });
      snapshot = await settled(h, session, "Found article");
      const result = await h.call("ui.page.event", { session, ticket: "host-ticket", interaction: interaction(snapshot, "article-0-Found_article") });
      assert.equal(result.result.status, "accepted");
      if (version >= 3) {
        assert.equal(result.result.result.push.ui, "today");
        const child = `article-${version}`;
        await h.call("ui.page.mount", { session: child, params: { contribution: "today", locale: "en", ui_version: version, state: result.result.result.push.state } });
        const detail = await settled(h, child, "A found synthetic introduction");
        assert.ok(body(detail).includes("A found synthetic introduction")); assert.ok(!body(detail).includes("attacker.invalid"));
        await h.call("ui.page.dispose", { session: child });
      } else {
        const detail = await settled(h, session, "A found synthetic introduction");
        await h.call("ui.page.event", { session, ticket: "host-ticket", interaction: interaction(detail, "back-button") });
        assert.ok(body(await settled(h, session, "Found article")).includes("Found article"));
      }
      await h.call("ui.page.dispose", { session });
    }
  } finally { h.p.close(); }
});
test("shared connector tools and cache, trusted tickets and replay controls", async () => {
  const h = await setup();
  try {
    await h.call("ui.page.mount", { session: "owned", params: { contribution: "today", locale: "en", ui_version: 5 } });
    const snapshot = await settled(h, "owned", "Featured article"), event = interaction(snapshot, "tab", { tab: "popular" });
    assert.ok((await h.call("ui.page.event", { session: "owned", interaction: event })).error);
    const accepted = (await h.call("ui.page.event", { session: "owned", ticket: "host-ticket", interaction: event })).result;
    const replay = (await h.call("ui.page.event", { session: "owned", ticket: "host-ticket", interaction: event })).result;
    assert.equal(replay.snapshot.version, accepted.snapshot.version);
    assert.equal((await h.call("connector.tools.call", { name: "wikipedia_today", args: {} })).result.text.includes("Featured article"), true);
    assert.equal(h.fixture.hits.get("/api/rest_v1/feed/featured/2026/11/09"), 1);
    await h.call("ui.page.dispose", { session: "owned" });
    const remount: Snapshot = (await h.call("ui.page.mount", { session: "new", params: { contribution: "today", ui_version: 5 } })).result;
    assert.notEqual(remount.generation, snapshot.generation);
    assert.equal((await h.call("ui.page.event", { session: "new", ticket: "host-ticket", interaction: event })).result.status, "ambiguous");
  } finally { h.p.close(); }
});
test("invalid state cannot trigger arbitrary requests; pure snapshots are supported", async () => {
  const h = await setup();
  try {
    await h.call("ui.page.mount", { session: "bad", params: { contribution: "today", locale: "zh-Hans", ui_version: 1, state: "not-json" } });
    await settled(h, "bad", "今日精选");
    const local = new h.bundle.Wikipedia(h.fixture.fetch, {}, { locale: "en", now: () => h.fixture.clock, limits: { gap: 0 } });
    const pure = h.bundle.snapshotPage(local);
    const view = await pure.render({ contribution: "today", locale: "en", ui_version: 1, hub: {} });
    assert.ok(JSON.stringify(view).includes("Featured article"));
    const searched = await pure.action({ contribution: "today", locale: "en", ui_version: 1, hub: {}, action: "search", values: JSON.stringify({ query: "synthetic" }) as unknown as Record<string, string> });
    assert.ok(JSON.stringify(searched).includes("Found article"));
  } finally { h.p.close(); }
});
test("connector.tools.call handles omitted limit, rejects invalid limit lexical forms, and dispatches unknown tools first", async () => {
  const h = await setup();
  try {
    const omitted = await h.call("connector.tools.call", { name: "wikipedia_search", args: { query: "synthetic" } });
    assert.equal(omitted.error, undefined, JSON.stringify(omitted.error));
    assert.match(omitted.result.text, /Found article/);
    assert.equal(h.fixture.hits.get("/w/rest.php/v1/search/page?q=synthetic&limit=10"), 1);

    assert.equal((await h.call("connector.tools.call", { name: "wikipedia_search", args: { query: "synthetic", limit: 0 } })).error.code, "invalid");
    assert.equal((await h.call("connector.tools.call", { name: "wikipedia_search", args: { query: "synthetic", limit: 1.5 } })).error.code, "invalid");
    assert.equal((await h.call("connector.tools.call", { name: "wikipedia_search", args: { query: "synthetic", limit: "5" } })).error.code, "invalid");
    assert.equal((await h.call("connector.tools.call", { name: "wikipedia_unknown", args: { limit: 1.5 } })).error.code, "unknown_method");

    // Raw JSON frames test lexical ExactJSON edge cases (1.0, 1e1, int64 overflow, duplicate keys, null, non-object args).
    const rawCall = async (id: string, json: string) => {
      h.p.line(json);
      await h.p.drain();
      const frame = h.frames.find(f => f.id === id);
      assert.ok(frame);
      return frame;
    };
    for (const [idx, argsJSON] of [
      `{"query":"synthetic","limit":1.0}`,
      `{"query":"synthetic","limit":1e1}`,
      `{"query":"synthetic","limit":9223372036854775808}`,
      `{"query":"synthetic","limit":1.5,"limit":5}`,
      `{"query":"synthetic","limit":5,"limit":0}`,
      `[]`,
      `"str"`,
    ].entries()) {
      const frame = await rawCall(`raw-bad-${idx}`, `{"id":"raw-bad-${idx}","method":"connector.tools.call","params":{"name":"wikipedia_search","args":${argsJSON}}}`);
      assert.equal(frame.error?.code, "invalid", `expected invalid for ${argsJSON}, got ${JSON.stringify(frame)}`);
    }
    const nullLimit = await rawCall("raw-null-limit", `{"id":"raw-null-limit","method":"connector.tools.call","params":{"name":"wikipedia_search","args":{"query":"synthetic","limit":null}}}`);
    assert.equal(nullLimit.error, undefined, JSON.stringify(nullLimit.error));
    const dupTrailingNull = await rawCall("raw-dup-null", `{"id":"raw-dup-null","method":"connector.tools.call","params":{"name":"wikipedia_search","args":{"query":"synthetic","limit":10,"limit":null}}}`);
    assert.equal(dupTrailingNull.error, undefined, JSON.stringify(dupTrailingNull.error));
    const nullArgs = await rawCall("raw-null-args", `{"id":"raw-null-args","method":"connector.tools.call","params":{"name":"wikipedia_today","args":null}}`);
    assert.equal(nullArgs.error, undefined, JSON.stringify(nullArgs.error));
  } finally { h.p.close(); }
});
test("long and colon-containing titles keep keys and action IDs within CheckView limits and navigate cleanly", async () => {
  const h = await setup();
  try {
    const prefix = "Very_long_article_title_with_colon:_";
    const longA = prefix + "A".repeat(170) + ":_Part_1";
    const longB = prefix + "A".repeat(170) + ":_Part_2";
    const colonTitle = "2001:_A_Space_Odyssey";
    h.fixture.put(`/w/rest.php/v1/search/page?q=odyssey&limit=30`, {
      pages: [
        { key: colonTitle, title: "2001: A Space Odyssey", description: "Film", excerpt: "Colon excerpt" },
        { key: longA, title: longA.replaceAll("_", " "), description: "First long", excerpt: "Long A" },
        { key: longB, title: longB.replaceAll("_", " "), description: "Second long", excerpt: "Long B" },
      ],
    });
    h.fixture.put(`/api/rest_v1/page/summary/${encodeURIComponent(colonTitle)}`, syntheticArticle(colonTitle, { extract: "Monolith introduction." }));
    h.fixture.put(`/api/rest_v1/page/summary/${encodeURIComponent(longA)}`, syntheticArticle(longA, { extract: "Long A full introduction." }));

    // Verify snapshotPage CheckView key/action.id bounds across both v1 and v5.
    const local = new h.bundle.Wikipedia(h.fixture.fetch, {}, { locale: "en", now: () => h.fixture.clock, limits: { gap: 0 } });
    const pure = h.bundle.snapshotPage(local);
    for (const ui_version of [1, 5]) {
      const view = await pure.render({ contribution: "today", locale: "en", ui_version, hub: {}, state: JSON.stringify({ v: "search", t: "featured", a: "", q: "odyssey", d: "" }) });
      const items = view.sections.flatMap((section: any) => section.items);
      const keys = new Set<string>();
      for (const item of items) {
        if (item.key) {
          assert.ok(Array.from(item.key).length <= 128, `key too long (${Array.from(item.key).length}): ${item.key}`);
          assert.ok(!keys.has(item.key), `duplicate key: ${item.key}`);
          keys.add(item.key);
        }
        if (item.action?.id) {
          assert.ok(Array.from(item.action.id).length <= 128, `action.id too long (${Array.from(item.action.id).length}): ${item.action.id}`);
        }
      }
      // Snapshot colon navigation works directly via action id, and long titles carry open-article values on v4+.
      const colonRow = items.find((item: any) => item.title === "2001: A Space Odyssey");
      assert.ok(colonRow);
      const opened = await pure.action({ contribution: "today", locale: "en", ui_version, hub: {}, state: view.state, action: colonRow.action.id, values: {} });
      assert.ok(JSON.stringify(opened.view).includes("Monolith introduction."));
      const longRow = items.find((item: any) => item.title === longA.replaceAll("_", " "));
      assert.ok(longRow && longRow.action.id.startsWith("open-article:"));
      if (ui_version >= 4) {
        assert.deepEqual(longRow.action.values, { article: longA });
        const openedLong = await pure.action({ contribution: "today", locale: "en", ui_version, hub: {}, state: view.state, action: longRow.action.id, values: longRow.action.values });
        assert.ok(JSON.stringify(openedLong.view).includes("Long A full introduction."));
      }
    }

    // Verify componentPage (Root) navigates to colon and 200+ char titles cleanly on ui_version 1 and 2, and identity avoids colon collisions.
    for (const ui_version of [1, 2]) {
      const session = `long-v${ui_version}`;
      await h.call("ui.page.mount", { session, params: { contribution: "today", locale: "en", ui_version, state: JSON.stringify({ v: "search", t: "featured", a: "", q: "odyssey", d: "" }) } });
      const searchSnap = await settled(h, session, "2001: A Space Odyssey");
      const longARow = searchSnap.view.sections.flatMap(section => section.items).find(item => "title" in item && item.title === longA.replaceAll("_", " "));
      assert.ok(longARow && longARow.key && Array.from(longARow.key).length <= 128);
      await h.call("ui.page.event", { session, ticket: "host-ticket", interaction: interaction(searchSnap, longARow.key) });
      const longADetail = await settled(h, session, "Long A full introduction.");
      assert.ok(body(longADetail).includes("Long A full introduction."));
      await h.call("ui.page.dispose", { session });
    }
  } finally { h.p.close(); }
});
test("loadPage and callTool report slow error on request, page, and tool budget timeouts", async () => {
  const bundle = await import(pathToFileURL(resolve("build/wikipedia.mjs")).href);
  const stalledFetch = async (_input: string, init: RequestInit) => {
    const stream = new ReadableStream<Uint8Array>({
      start(controller) {
        const onAbort = () => controller.error(new DOMException("The operation was aborted.", "AbortError"));
        if (init.signal?.aborted) onAbort(); else init.signal?.addEventListener("abort", onAbort, { once: true });
      },
    });
    return new Response(stream, { status: 200 });
  };
  // Request-level timeout
  const reqService = new bundle.Wikipedia(stalledFetch, {}, { locale: "en", limits: { gap: 0, request: 25, page: 200, tool: 200 } });
  const reqLoaded = await bundle.loadPage(reqService, bundle.readState(undefined), false);
  assert.equal(reqLoaded.kind, "error");
  assert.ok(reqLoaded.error instanceof bundle.ReadError && reqLoaded.error.code === "slow");
  // Page-level and tool-level budgets tighter than request timeout
  const outerService = new bundle.Wikipedia(stalledFetch, {}, { locale: "en", limits: { gap: 0, request: 500, page: 25, tool: 25 } });
  const pageLoaded = await bundle.loadPage(outerService, bundle.readState(undefined), false);
  assert.equal(pageLoaded.kind, "error");
  assert.ok(pageLoaded.error instanceof bundle.ReadError && pageLoaded.error.code === "slow");
  await assert.rejects(bundle.callTool(outerService, "wikipedia_today"), (error: unknown) => error instanceof bundle.ReadError && (error as { code?: string }).code === "slow");
});
