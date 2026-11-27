import { test } from "node:test";
import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import { resolve } from "node:path";
import { pathToFileURL } from "node:url";
import { createHash } from "node:crypto";
import { connect, type Program } from "@persona/extension";
import { Renderer, type Snapshot } from "@persona/extension/ui";
import { Crates } from "../src/service.ts";
import { page } from "../src/page.tsx";
import { searchPath } from "../src/model.ts";
import { Fixture, interaction, serde, until } from "./fixtures.ts";
async function setup(initialize = true) {
  const bundle = await import(pathToFileURL(resolve("build/crates.mjs")).href);
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
  assert.equal(manifest.id, "crates"); assert.equal(manifest.runtime.mode, "deno"); assert.equal(manifest.runtime.command, undefined);
  assert.equal(manifest.runtime.source.sha256, createHash("sha256").update(bytes).digest("hex"));
  assert.equal(manifest.contributes.ui[0].id, "crates"); assert.equal(manifest.contributes.connectors[0].capabilities.includes("actions"), false);
  assert.deepEqual(manifest.permissions.network, ["crates.io"]);
  assert.ok(bytes.length > 0 && bytes.length < (8 << 20));
  const bundle = await import(pathToFileURL(resolve("build/crates.mjs")).href);
  assert.equal(bundle.default.fetch, undefined); assert.equal(bundle.default.id, "crates");
});

test("protocol initialization, localized tools and unknown contributions are SDK-owned", async () => {
  const h = await setup(false);
  try {
    assert.equal((await h.call("connector.status")).error.code, "not_initialized");
    assert.equal((await h.call("initialize", { protocol: 2 })).error.code, "wrong_protocol");
    assert.equal((await h.call("initialize", { protocol: 1, config: {}, hub: { locale: "ja" } })).result.id, "crates");
    assert.deepEqual((await h.call("ping")).result, {});
    assert.equal((await h.call("connector.status", { contribution: "missing" })).error.code, "no_contribution");
    assert.equal((await h.call("connector.unsupported", {})).error.code, "unknown_method");
    assert.ok((await h.call("connector.tools.list", {})).result.tools[0].description.includes("検索"));
    assert.ok((await h.call("connector.tools.call", { name: "crates_search", args: { query: "bad\n" } })).error);
  } finally { h.p.close(); }
});

test("search and detail navigation retain version, license, docs and install text", async () => {
  const h = await setup();
  try {
    for (const version of [1, 3, 5]) {
      const session = `crates-${version}`;
      await h.call("ui.page.mount", { session, params: { contribution: "crates", locale: "en", ui_version: version } });
      let snapshot = await settled(h, session, "crates-welcome-text");
      const button = snapshot.view.sections[0].items.find(item => item.key === "crates-submit");
      assert.ok(button && "action" in button && button.action?.submit === true, "search submits current field values");
      await h.call("ui.page.event", { session, ticket: "host-ticket", interaction: interaction(snapshot, "crates-submit", { query: "serde" }) });
      snapshot = await settled(h, session, "serde@1.0.229");
      const navigation = (await h.call("ui.page.event", { session, ticket: "host-ticket", interaction: interaction(snapshot, "open-serde") })).result;
      if (navigation.result.push) {
        await h.call("ui.page.mount", { session: session + "-detail", params: { contribution: "crates", locale: "en", ui_version: version, state: navigation.result.push.state } });
      }
      snapshot = await settled(h, navigation.result.push ? session + "-detail" : session, "cargo add serde@1.0.229");
      assert.ok(body(snapshot).includes("https://crates.io/crates/serde"));
      assert.ok(body(snapshot).includes("https://docs.rs/serde"));
      assert.ok(body(snapshot).includes("MIT OR Apache-2.0"));
      assert.ok(body(snapshot).includes("236,925,653"));
      assert.ok(!body(snapshot).includes("attacker.invalid"));
      await h.call("ui.page.dispose", { session });
    }
  } finally { h.p.close(); }
});

test("shared connector tools, trusted tickets and replay controls", async () => {
  const h = await setup();
  try {
    await h.call("ui.page.mount", { session: "owned", params: { contribution: "crates", locale: "en", ui_version: 5 } });
    const snapshot = await settled(h, "owned", "crates-welcome-text"), event = interaction(snapshot, "crates-submit", { query: "serde" });
    assert.ok((await h.call("ui.page.event", { session: "owned", interaction: event })).error);
    const accepted = (await h.call("ui.page.event", { session: "owned", ticket: "host-ticket", interaction: event })).result;
    if (accepted?.snapshot) {
      const replay = (await h.call("ui.page.event", { session: "owned", ticket: "host-ticket", interaction: event })).result;
      assert.equal(replay.snapshot.version, accepted.snapshot.version);
    } else assert.equal(accepted.status, "accepted");
    assert.equal((await h.call("connector.tools.call", { name: "crates_search", args: { query: "serde" } })).result.text.includes("serde@1.0.229"), true);
    assert.equal(h.fixture.hits.size > 0, true);
    await h.call("ui.page.dispose", { session: "owned" });
    const remount: Snapshot = (await h.call("ui.page.mount", { session: "new", params: { contribution: "crates", ui_version: 5 } })).result;
    assert.notEqual(remount.generation, snapshot.generation);
    assert.equal((await h.call("ui.page.event", { session: "new", ticket: "host-ticket", interaction: event })).result.status, "ambiguous");
  } finally { h.p.close(); }
});

test("modern navigation mounts the validated selected crate detail", async () => {
  const h = await setup();
  try {
    await h.call("ui.page.mount", { session: "search", params: { contribution: "crates", locale: "en", ui_version: 5 } });
    const initial = await settled(h, "search", "crates-welcome-text");
    await h.call("ui.page.event", { session: "search", ticket: "host-ticket", interaction: interaction(initial, "crates-submit", { query: "serde" }) });
    const results = await settled(h, "search", "open-serde-saphyr");
    const navigation = (await h.call("ui.page.event", { session: "search", ticket: "host-ticket", interaction: interaction(results, "open-serde-saphyr") })).result;
    assert.equal(navigation.result.push.ui, "crates");
    await h.call("ui.page.mount", { session: "detail", params: { contribution: "crates", locale: "en", ui_version: 5, state: navigation.result.push.state } });
    const detail = await settled(h, "detail", "serde-saphyr");
    assert.ok(body(detail).includes("crates-saphyr") === false);
  } finally { h.p.close(); }
});

test("snapshot transport mounts the initial editor without a pending load", async () => {
  const h = await setup();
  try {
    const mounted = (await h.call("ui.page.mount", { session: "snapshot-editor", transport: "snapshot", params: { contribution: "crates", locale: "zh-Hans", ui_version: 5 } })).result;
    assert.ok(JSON.stringify(mounted).includes("crate 名称或关键词"));
    assert.equal(mounted.view.sections.some((section: { key: string }) => section.key === "crates-loading"), false);
  } finally { h.p.close(); }
});

test("invalid state cannot trigger arbitrary requests; pure snapshots are supported", async () => {
  const h = await setup();
  try {
    await h.call("ui.page.mount", { session: "bad", params: { contribution: "crates", locale: "ja", ui_version: 1, state: "not-json" } });
    await settled(h, "bad", "Rust クレートを検索");
    const local = new h.bundle.Crates(h.fixture.fetch, {}, { locale: "en", now: () => h.fixture.clock, limits: { gap: 0 } });
    const pure = h.bundle.snapshotPage(local);
    const view = await pure.render({ contribution: "crates", locale: "en", ui_version: 1, hub: {}, state: h.bundle.encodeState({ q: "serde", found: true }) });
    assert.ok(JSON.stringify(view).includes("serde@1.0.229"));
  } finally { h.p.close(); }
});

test("large direct renders, oversized text/state, duplicate crates, and invalid dates stay within CheckView budgets", async () => {
  const h = await setup();
  try {
    const local = new Crates(h.fixture.fetch, {}, { locale: "en", now: () => h.fixture.clock, limits: { gap: 0 } });
    const manyCrates = Array.from({ length: 120 }, (_, i) => ({
      name: i < 2 ? "dup_crate" : `crate_${i}_${"c".repeat(48)}`,
      version: "1.0.0",
      description: "D".repeat(2500),
      license: "L".repeat(2500),
      url: `https://crates.io/crates/crate_${i}`,
      updated: "not-a-valid-date",
      recentDownloads: i,
    }));
    const renderer = new Renderer({ authorize: () => false });
    try {
      const rendered = renderer.mount(page({
        service: local,
        state: { q: "q".repeat(17000), found: true },
        loaded: { kind: "results", result: { crates: manyCrates, total: manyCrates.length, at: h.fixture.clock } },
        locale: "en",
        uiVersion: 7,
      })).view;
      const totalItems = rendered.sections.reduce((sum, s) => sum + s.items.length, 0);
      assert.ok(totalItems <= 300, `expected <= 300 items, got ${totalItems}`);
      assert.ok(new TextEncoder().encode(rendered.state ?? "").byteLength <= 16384);
      const keys = new Set<string>();
      for (const section of rendered.sections) {
        if (section.key) {
          assert.ok(Array.from(section.key).length <= 128);
          assert.ok(!keys.has(section.key), `duplicate section key ${section.key}`);
          keys.add(section.key);
        }
        if (section.title) assert.ok(Array.from(section.title).length <= 2000);
        if (section.footer) assert.ok(Array.from(section.footer).length <= 2000);
        for (const item of section.items) {
          if (item.key) {
            assert.ok(Array.from(item.key).length <= 128);
            assert.ok(!keys.has(item.key), `duplicate item key ${item.key}`);
            keys.add(item.key);
          }
          if (item.type === "text") assert.ok(Array.from(item.text).length >= 1 && Array.from(item.text).length <= 2000);
          if (item.type === "field" && item.value) assert.ok(Array.from(item.value).length <= 2000);
          if ("action" in item && item.action?.id) assert.ok(Array.from(item.action.id).length <= 128);
          if ("action" in item && item.action?.push?.state) assert.ok(new TextEncoder().encode(item.action.push.state).byteLength <= 16384);
        }
      }
    } finally { renderer.dispose(); }
  } finally { h.p.close(); }
});

test("manual refresh resets after navigating to a new search and RPC rejects explicit limit: 0", async () => {
  const h = await setup();
  try {
    const serdePath = searchPath("serde", 12);
    const tokioPath = searchPath("tokio", 12);
    h.fixture.put(tokioPath, { crates: [{ ...serde, id: "tokio", name: "tokio", default_version: "1.40.0", description: "Async runtime" }], meta: { total: 1 } });
    await h.call("ui.page.mount", { session: "nav", params: { contribution: "crates", locale: "en", ui_version: 5 } });
    let snapshot = await settled(h, "nav", "crates-welcome-text");

    await h.call("ui.page.event", { session: "nav", ticket: "host-ticket", interaction: interaction(snapshot, "crates-submit", { query: "serde" }) });
    snapshot = await settled(h, "nav", "serde@1.0.229");
    assert.equal(h.fixture.hits.get(serdePath), 1);

    await h.call("ui.page.event", { session: "nav", ticket: "host-ticket", interaction: interaction(snapshot, "crates-submit", { query: "tokio" }) });
    snapshot = await settled(h, "nav", "tokio@1.40.0");
    assert.equal(h.fixture.hits.get(tokioPath), 1);

    await h.call("ui.page.event", { session: "nav", ticket: "host-ticket", interaction: interaction(snapshot, "crates-refresh") });
    await until(async () => h.fixture.hits.get(tokioPath) ?? 0, count => count === 2);
    snapshot = await settled(h, "nav", "tokio@1.40.0");

    // Navigating back to cached "serde" must reuse cache (still hit 1).
    await h.call("ui.page.event", { session: "nav", ticket: "host-ticket", interaction: interaction(snapshot, "crates-submit", { query: "serde" }) });
    snapshot = await settled(h, "nav", "serde@1.0.229");
    assert.equal(h.fixture.hits.get(serdePath), 1);

    // End-to-end RPC ExactJSON: explicit limit: 0 must be rejected as invalid.
    const zeroLimit = await h.call("connector.tools.call", { name: "crates_search", args: { query: "serde", limit: 0 } });
    assert.equal(zeroLimit.error?.code, "invalid");
  } finally { h.p.close(); }
});
