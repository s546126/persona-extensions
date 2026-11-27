import { test } from "node:test";
import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import { resolve } from "node:path";
import { pathToFileURL } from "node:url";
import { createHash } from "node:crypto";
import { connect, type Program } from "@persona/extension";
import { Renderer, type Snapshot } from "@persona/extension/ui";
import { NpmPackages } from "../src/service.ts";
import { page } from "../src/page.tsx";
import { downloadsPath, packagePath, searchPath } from "../src/model.ts";
import { Fixture, interaction, scoped, typescript, until } from "./fixtures.ts";
async function setup(initialize = true) {
  const bundle = await import(pathToFileURL(resolve("build/npmpackages.mjs")).href);
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
  assert.equal(manifest.id, "npmpackages"); assert.equal(manifest.runtime.mode, "deno"); assert.equal(manifest.runtime.command, undefined);
  assert.equal(manifest.runtime.source.sha256, createHash("sha256").update(bytes).digest("hex"));
  assert.equal(manifest.contributes.ui[0].id, "packages"); assert.equal(manifest.contributes.connectors[0].capabilities.includes("actions"), false);
  assert.deepEqual(manifest.permissions.network, ["registry.npmjs.org", "api.npmjs.org"]);
  assert.ok(bytes.length > 0 && bytes.length < (8 << 20));
  const bundle = await import(pathToFileURL(resolve("build/npmpackages.mjs")).href);
  assert.equal(bundle.default.fetch, undefined); assert.equal(bundle.default.id, "npmpackages");
});

test("protocol initialization, localized tools and unknown contributions are SDK-owned", async () => {
  const h = await setup(false);
  try {
    assert.equal((await h.call("connector.status")).error.code, "not_initialized");
    assert.equal((await h.call("initialize", { protocol: 2 })).error.code, "wrong_protocol");
    assert.equal((await h.call("initialize", { protocol: 1, config: {}, hub: { locale: "ja" } })).result.id, "npmpackages");
    assert.deepEqual((await h.call("ping")).result, {});
    assert.equal((await h.call("connector.status", { contribution: "missing" })).error.code, "no_contribution");
    assert.equal((await h.call("connector.unsupported", {})).error.code, "unknown_method");
    assert.ok((await h.call("connector.tools.list", {})).result.tools[0].description.includes("検索"));
    assert.ok((await h.call("connector.tools.call", { name: "npmpackages_search", args: { query: "bad\n" } })).error);
  } finally { h.p.close(); }
});

test("search and detail navigation retain scoped versions, links and install text", async () => {
  const h = await setup();
  try {
    for (const version of [1, 3, 5]) {
      const session = `npmpackages-${version}`;
      await h.call("ui.page.mount", { session, params: { contribution: "packages", locale: "en", ui_version: version } });
      let snapshot = await settled(h, session, "npmpackages-welcome-text");
      const button = snapshot.view.sections[0].items.find(item => item.key === "npmpackages-submit");
      assert.ok(button && "action" in button && button.action?.submit === true, "search submits current field values");
      await h.call("ui.page.event", { session, ticket: "host-ticket", interaction: interaction(snapshot, "npmpackages-submit", { query: "typescript" }) });
      snapshot = await settled(h, session, "typescript@7.0.2");
      const navigation = (await h.call("ui.page.event", { session, ticket: "host-ticket", interaction: interaction(snapshot, "open-@babel/preset-typescript") })).result;
      if (navigation.result.push) {
        await h.call("ui.page.mount", { session: session + "-detail", params: { contribution: "packages", locale: "en", ui_version: version, state: navigation.result.push.state } });
      }
      snapshot = await settled(h, navigation.result.push ? session + "-detail" : session, "npm install @babel/preset-typescript@8.0.7");
      assert.ok(body(snapshot).includes("https://www.npmjs.com/package/@babel/preset-typescript"));
      assert.ok(body(snapshot).includes("https://github.com/babel/babel"));
      assert.ok(body(snapshot).includes("73,675,116"));
      assert.ok(body(snapshot).includes("TypeScript declarations included"));
      assert.ok(!body(snapshot).includes("attacker.invalid"));
      await h.call("ui.page.dispose", { session });
    }
  } finally { h.p.close(); }
});

test("shared connector tools, trusted tickets and replay controls", async () => {
  const h = await setup();
  try {
    await h.call("ui.page.mount", { session: "owned", params: { contribution: "packages", locale: "en", ui_version: 5 } });
    const snapshot = await settled(h, "owned", "npmpackages-welcome-text"), event = interaction(snapshot, "npmpackages-submit", { query: "typescript" });
    assert.ok((await h.call("ui.page.event", { session: "owned", interaction: event })).error);
    const accepted = (await h.call("ui.page.event", { session: "owned", ticket: "host-ticket", interaction: event })).result;
    if (accepted?.snapshot) {
      const replay = (await h.call("ui.page.event", { session: "owned", ticket: "host-ticket", interaction: event })).result;
      assert.equal(replay.snapshot.version, accepted.snapshot.version);
    } else assert.equal(accepted.status, "accepted");
    assert.equal((await h.call("connector.tools.call", { name: "npmpackages_search", args: { query: "typescript" } })).result.text.includes("typescript@7.0.2"), true);
    assert.equal(h.fixture.hits.size > 0, true);
    await h.call("ui.page.dispose", { session: "owned" });
    const remount: Snapshot = (await h.call("ui.page.mount", { session: "new", params: { contribution: "packages", ui_version: 5 } })).result;
    assert.notEqual(remount.generation, snapshot.generation);
    assert.equal((await h.call("ui.page.event", { session: "new", ticket: "host-ticket", interaction: event })).result.status, "ambiguous");
  } finally { h.p.close(); }
});

test("modern navigation mounts the validated selected package detail", async () => {
  const h = await setup();
  try {
    await h.call("ui.page.mount", { session: "search", params: { contribution: "packages", locale: "en", ui_version: 5 } });
    const initial = await settled(h, "search", "npmpackages-welcome-text");
    await h.call("ui.page.event", { session: "search", ticket: "host-ticket", interaction: interaction(initial, "npmpackages-submit", { query: "typescript" }) });
    const results = await settled(h, "search", "open-@babel/preset-typescript");
    const navigation = (await h.call("ui.page.event", { session: "search", ticket: "host-ticket", interaction: interaction(results, "open-@babel/preset-typescript") })).result;
    assert.equal(navigation.result.push.ui, "packages");
    await h.call("ui.page.mount", { session: "detail", params: { contribution: "packages", locale: "en", ui_version: 5, state: navigation.result.push.state } });
    const detail = await settled(h, "detail", "npm install @babel/preset-typescript@8.0.7");
    assert.ok(body(detail).includes("73,675,116"));
  } finally { h.p.close(); }
});

test("snapshot transport mounts the initial editor without a pending load", async () => {
  const h = await setup();
  try {
    const mounted = (await h.call("ui.page.mount", { session: "snapshot-editor", transport: "snapshot", params: { contribution: "packages", locale: "zh-Hans", ui_version: 5 } })).result;
    assert.ok(JSON.stringify(mounted).includes("包名、作用域或关键词"));
    assert.equal(mounted.view.sections.some((section: { key: string }) => section.key === "npmpackages-loading"), false);
  } finally { h.p.close(); }
});

test("invalid state cannot trigger arbitrary requests; pure snapshots are supported", async () => {
  const h = await setup();
  try {
    await h.call("ui.page.mount", { session: "bad", params: { contribution: "packages", locale: "ja", ui_version: 1, state: "not-json" } });
    await settled(h, "bad", "npm パッケージを検索");
    const local = new h.bundle.NpmPackages(h.fixture.fetch, {}, { locale: "en", now: () => h.fixture.clock, limits: { gap: 0 } });
    const pure = h.bundle.snapshotPage(local);
    const view = await pure.render({ contribution: "packages", locale: "en", ui_version: 1, hub: {}, state: h.bundle.encodeState({ q: "typescript", found: true }) });
    assert.ok(JSON.stringify(view).includes("typescript@7.0.2"));
  } finally { h.p.close(); }
});

test("long scoped package names, duplicate items, and large direct renders stay within CheckView budgets", async () => {
  const h = await setup();
  try {
    const longPkg = `@${"s".repeat(50)}/${"p".repeat(162)}`; // 214 chars (max valid npm package name)
    const longEntry = { ...scoped, name: longPkg, description: "Long scoped package desc", links: { npm: `https://www.npmjs.com/package/${longPkg}` } };
    h.fixture.put(searchPath("longpkg", 12), { objects: [{ package: longEntry, downloads: { weekly: 100 } }], total: 1 });
    h.fixture.put(packagePath(longPkg), longEntry);
    h.fixture.put(downloadsPath(longPkg), { downloads: 100, start: "2026-11-03", end: "2026-11-09", package: longPkg });

    await h.call("ui.page.mount", { session: "long-v1", params: { contribution: "packages", locale: "en", ui_version: 1, state: h.bundle.encodeState({ q: "longpkg", found: true }) } });
    const listSnap = await settled(h, "long-v1", "Long scoped package desc");
    const openBtn = listSnap.view.sections.flatMap(s => s.items).find(item => item.key?.startsWith("open-"));
    assert.ok(openBtn && openBtn.key);
    assert.ok(Array.from(openBtn.key).length <= 128, `open key too long: ${Array.from(openBtn.key).length}`);
    for (const section of listSnap.view.sections) {
      if (section.key) assert.ok(Array.from(section.key).length <= 128, `section key too long: ${Array.from(section.key).length}`);
      for (const item of section.items) {
        if (item.key) assert.ok(Array.from(item.key).length <= 128, `item key too long: ${Array.from(item.key).length}`);
        if ("action" in item && item.action?.id) assert.ok(Array.from(item.action.id).length <= 128, `action id too long: ${Array.from(item.action.id).length}`);
      }
    }
    await h.call("ui.page.event", { session: "long-v1", ticket: "host-ticket", interaction: interaction(listSnap, openBtn.key) });
    const detailSnap = await settled(h, "long-v1", `npm install ${longPkg}@8.0.7`);
    assert.ok(body(detailSnap).includes("npm install " + longPkg));

    // Direct page() render with > 120 packages, duplicates, > 2500-rune descriptions, oversized state.q, and invalid date stays within CheckView limits.
    const local = new NpmPackages(h.fixture.fetch, {}, { locale: "en", now: () => h.fixture.clock, limits: { gap: 0 } });
    const manyPkgs = Array.from({ length: 120 }, (_, i) => ({
      name: i < 2 ? longPkg : `@${"s".repeat(40)}/pkg-${i}-${"p".repeat(120)}`,
      version: "1.0.0",
      description: "D".repeat(2500),
      license: "L".repeat(2500),
      url: `https://www.npmjs.com/package/pkg-${i}`,
      published: "not-a-valid-date",
      weeklyDownloads: i,
    }));
    const renderer = new Renderer({ authorize: () => false });
    try {
      const rendered = renderer.mount(page({
        service: local,
        state: { q: "q".repeat(17000), found: true },
        loaded: { kind: "results", result: { packages: manyPkgs, total: manyPkgs.length, at: h.fixture.clock } },
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
    const tsPath = "https://registry.npmjs.org" + searchPath("typescript", 12);
    const viteSearchPath = searchPath("vite", 12);
    const viteFullPath = "https://registry.npmjs.org" + viteSearchPath;
    h.fixture.put(viteSearchPath, { objects: [{ package: { ...typescript, name: "vite", version: "6.0.0", description: "Frontend tooling", links: { npm: "https://www.npmjs.com/package/vite" } } }], total: 1 });
    await h.call("ui.page.mount", { session: "nav", params: { contribution: "packages", locale: "en", ui_version: 5 } });
    let snapshot = await settled(h, "nav", "npmpackages-welcome-text");

    await h.call("ui.page.event", { session: "nav", ticket: "host-ticket", interaction: interaction(snapshot, "npmpackages-submit", { query: "typescript" }) });
    snapshot = await settled(h, "nav", "typescript@7.0.2");
    assert.equal(h.fixture.hits.get(tsPath), 1);

    await h.call("ui.page.event", { session: "nav", ticket: "host-ticket", interaction: interaction(snapshot, "npmpackages-submit", { query: "vite" }) });
    snapshot = await settled(h, "nav", "vite@6.0.0");
    assert.equal(h.fixture.hits.get(viteFullPath), 1);

    await h.call("ui.page.event", { session: "nav", ticket: "host-ticket", interaction: interaction(snapshot, "npmpackages-refresh") });
    await until(async () => h.fixture.hits.get(viteFullPath) ?? 0, count => count === 2);
    snapshot = await settled(h, "nav", "vite@6.0.0");

    // Navigating back to cached "typescript" must reuse cache (still hit 1).
    await h.call("ui.page.event", { session: "nav", ticket: "host-ticket", interaction: interaction(snapshot, "npmpackages-submit", { query: "typescript" }) });
    snapshot = await settled(h, "nav", "typescript@7.0.2");
    assert.equal(h.fixture.hits.get(tsPath), 1);

    // End-to-end RPC ExactJSON: explicit limit: 0 must be rejected as invalid.
    const zeroLimit = await h.call("connector.tools.call", { name: "npmpackages_search", args: { query: "typescript", limit: 0 } });
    assert.equal(zeroLimit.error?.code, "invalid");
  } finally { h.p.close(); }
});
