import { test } from "node:test";
import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import { resolve } from "node:path";
import { pathToFileURL } from "node:url";
import { createHash } from "node:crypto";
import { connect, type Program } from "@persona/extension";
import type { Snapshot } from "@persona/extension/ui";
import { Fixture, interaction, until } from "./fixtures.ts";
async function setup(initialize = true) {
  const bundle = await import(pathToFileURL(resolve("build/nuget.mjs")).href);
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
  assert.equal(manifest.id, "nuget"); assert.equal(manifest.runtime.mode, "deno"); assert.equal(manifest.runtime.command, undefined);
  assert.equal(manifest.runtime.source.sha256, createHash("sha256").update(bytes).digest("hex"));
  assert.equal(manifest.contributes.ui[0].id, "nuget"); assert.equal(manifest.contributes.connectors[0].capabilities.includes("actions"), false);
  assert.deepEqual(manifest.permissions.network, ["azuresearch-usnc.nuget.org", "api.nuget.org", "nuget.azure.cn"]);
  assert.ok(bytes.length > 0 && bytes.length < (8 << 20));
  const bundle = await import(pathToFileURL(resolve("build/nuget.mjs")).href);
  assert.equal(bundle.default.fetch, undefined); assert.equal(bundle.default.id, "nuget");
});

test("protocol initialization, localized tools and unknown contributions are SDK-owned", async () => {
  const h = await setup(false);
  try {
    assert.equal((await h.call("connector.status")).error.code, "not_initialized");
    assert.equal((await h.call("initialize", { protocol: 2 })).error.code, "wrong_protocol");
    assert.equal((await h.call("initialize", { protocol: 1, config: {}, hub: { locale: "ja" } })).result.id, "nuget");
    assert.deepEqual((await h.call("ping")).result, {});
    assert.equal((await h.call("connector.status", { contribution: "missing" })).error.code, "no_contribution");
    assert.equal((await h.call("connector.unsupported", {})).error.code, "unknown_method");
    assert.ok((await h.call("connector.tools.list", {})).result.tools[0].description.includes("検索"));
    assert.ok((await h.call("connector.tools.call", { name: "nuget_search", args: { query: "bad\n" } })).error);
  } finally { h.p.close(); }
});

test("search and detail navigation retain stable version, license, dependencies and install text", async () => {
  const h = await setup();
  try {
    for (const version of [1, 3, 5]) {
      const session = `nuget-${version}`;
      await h.call("ui.page.mount", { session, params: { contribution: "nuget", locale: "en", ui_version: version } });
      let snapshot = await settled(h, session, "nuget-welcome-text");
      const button = snapshot.view.sections[0].items.find(item => item.key === "nuget-submit");
      assert.ok(button && "action" in button && button.action?.submit === true, "search submits current field values");
      await h.call("ui.page.event", { session, ticket: "host-ticket", interaction: interaction(snapshot, "nuget-submit", { query: "Newtonsoft.Json" }) });
      snapshot = await settled(h, session, "Newtonsoft.Json@13.0.4");
      const navigation = (await h.call("ui.page.event", { session, ticket: "host-ticket", interaction: interaction(snapshot, "open-Newtonsoft.Json") })).result;
      if (navigation.result.push) {
        await h.call("ui.page.mount", { session: session + "-detail", params: { contribution: "nuget", locale: "en", ui_version: version, state: navigation.result.push.state } });
      }
      snapshot = await settled(h, navigation.result.push ? session + "-detail" : session, "dotnet add package Newtonsoft.Json --version 13.0.4");
      assert.ok(body(snapshot).includes("MIT"));
      assert.ok(body(snapshot).includes(".NETStandard1.0"));
      assert.ok(body(snapshot).includes("https://www.nuget.org/packages/Newtonsoft.Json/13.0.4"));
      assert.ok(body(snapshot).includes("https://www.newtonsoft.com/json"));
      assert.ok(!body(snapshot).includes("attacker.invalid"));
      await h.call("ui.page.dispose", { session });
    }
  } finally { h.p.close(); }
});

test("shared connector tools, trusted tickets and replay controls", async () => {
  const h = await setup();
  try {
    await h.call("ui.page.mount", { session: "owned", params: { contribution: "nuget", locale: "en", ui_version: 5 } });
    const snapshot = await settled(h, "owned", "nuget-welcome-text"), event = interaction(snapshot, "nuget-submit", { query: "Newtonsoft.Json" });
    assert.ok((await h.call("ui.page.event", { session: "owned", interaction: event })).error);
    const accepted = (await h.call("ui.page.event", { session: "owned", ticket: "host-ticket", interaction: event })).result;
    if (accepted?.snapshot) {
      const replay = (await h.call("ui.page.event", { session: "owned", ticket: "host-ticket", interaction: event })).result;
      assert.equal(replay.snapshot.version, accepted.snapshot.version);
    } else assert.equal(accepted.status, "accepted");
    assert.equal((await h.call("connector.tools.call", { name: "nuget_search", args: { query: "Newtonsoft.Json" } })).result.text.includes("Newtonsoft.Json@13.0.4"), true);
    assert.equal(h.fixture.hits.size > 0, true);
    await h.call("ui.page.dispose", { session: "owned" });
    const remount: Snapshot = (await h.call("ui.page.mount", { session: "new", params: { contribution: "nuget", ui_version: 5 } })).result;
    assert.notEqual(remount.generation, snapshot.generation);
    assert.equal((await h.call("ui.page.event", { session: "new", ticket: "host-ticket", interaction: event })).result.status, "ambiguous");
  } finally { h.p.close(); }
});

test("modern navigation mounts the validated selected package detail", async () => {
  const h = await setup();
  try {
    await h.call("ui.page.mount", { session: "search", params: { contribution: "nuget", locale: "en", ui_version: 5 } });
    const initial = await settled(h, "search", "nuget-welcome-text");
    await h.call("ui.page.event", { session: "search", ticket: "host-ticket", interaction: interaction(initial, "nuget-submit", { query: "Newtonsoft.Json" }) });
    const results = await settled(h, "search", "open-Newtonsoft.Json");
    const navigation = (await h.call("ui.page.event", { session: "search", ticket: "host-ticket", interaction: interaction(results, "open-Newtonsoft.Json") })).result;
    assert.equal(navigation.result.push.ui, "nuget");
    await h.call("ui.page.mount", { session: "detail", params: { contribution: "nuget", locale: "en", ui_version: 5, state: navigation.result.push.state } });
    const detail = await settled(h, "detail", "dotnet add package Newtonsoft.Json --version 13.0.4");
    assert.ok(body(detail).includes("MIT"));
  } finally { h.p.close(); }
});

test("snapshot transport mounts the initial editor without a pending load", async () => {
  const h = await setup();
  try {
    const mounted = (await h.call("ui.page.mount", { session: "snapshot-editor", transport: "snapshot", params: { contribution: "nuget", locale: "zh-Hans", ui_version: 5 } })).result;
    assert.ok(JSON.stringify(mounted).includes("包 ID 或关键词"));
    assert.equal(mounted.view.sections.some((section: { key: string }) => section.key === "nuget-loading"), false);
  } finally { h.p.close(); }
});

test("invalid state cannot trigger arbitrary requests; pure snapshots are supported", async () => {
  const h = await setup();
  try {
    await h.call("ui.page.mount", { session: "bad", params: { contribution: "nuget", locale: "ja", ui_version: 1, state: "not-json" } });
    await settled(h, "bad", "NuGet パッケージを検索");
    const local = new h.bundle.Nuget(h.fixture.fetch, {}, { locale: "en", now: () => h.fixture.clock, limits: { gap: 0 } });
    const pure = h.bundle.snapshotPage(local);
    const view = await pure.render({ contribution: "nuget", locale: "en", ui_version: 1, hub: {}, state: h.bundle.encodeState({ q: "Newtonsoft.Json", found: true }) });
    assert.ok(JSON.stringify(view).includes("Newtonsoft.Json@13.0.4"));
  } finally { h.p.close(); }
});

test("ExactJSON rejects limit: 0 and checks unknown tool before args validation", async () => {
  const h = await setup();
  try {
    const zeroLimit = await h.call("connector.tools.call", { name: "nuget_search", args: { query: "Newtonsoft.Json", limit: 0 } });
    assert.equal(zeroLimit.error?.code, "invalid");
    const unknownTool = await h.call("connector.tools.call", { name: "nuget_unknown", args: { limit: "bad" } });
    assert.equal(unknownTool.error?.code, "unknown_method");
  } finally { h.p.close(); }
});

test("128-char package IDs navigate cleanly within key/action limits and refresh resets after navigation", async () => {
  const h = await setup();
  try {
    const longID = "A." + "x".repeat(126);
    const longPkg = {
      id: longID, version: "1.0.0", description: "Long package ID",
      projectUrl: "https://example.com/long", licenseUrl: "https://licenses.nuget.org/MIT",
      totalDownloads: 42, published: "2025-09-16T08:13:09.313Z",
    };
    const longSearch12 = `/query?${new URLSearchParams({ q: "longpkg", take: "12" })}`;
    const longSearch20 = `/query?${new URLSearchParams({ q: longID, take: "20" })}`;
    const longLeaf = `/v3/registration5-semver1/${longID.toLowerCase()}/1.0.0.json`;
    const longNuspec = `/v3-flatcontainer/${longID.toLowerCase()}/1.0.0/${longID.toLowerCase()}.nuspec`;
    h.fixture.put(longSearch12, { totalHits: 1, data: [longPkg] });
    h.fixture.put(longSearch20, { totalHits: 1, data: [longPkg] });
    h.fixture.put(longLeaf, { listed: true, registration: `https://api.nuget.org/v3/registration5-semver1/${longID.toLowerCase()}/index.json` });
    h.fixture.put(longNuspec, `<?xml version="1.0" encoding="utf-8"?><package><metadata><id>${longID}</id><version>1.0.0</version><license type="expression">MIT</license><description>Long package ID</description></metadata></package>`);

    await h.call("ui.page.mount", { session: "long-v1", params: { contribution: "nuget", locale: "en", ui_version: 1 } });
    let snap = await settled(h, "long-v1", "nuget-welcome-text");
    await h.call("ui.page.event", { session: "long-v1", ticket: "host-ticket", interaction: interaction(snap, "nuget-submit", { query: "longpkg" }) });
    snap = await settled(h, "long-v1", `${longID}@1.0.0`);
    const openItem = snap.view.sections.flatMap(s => s.items).find(item => typeof item.key === "string" && item.key.startsWith("open-"));
    assert.ok(openItem && typeof openItem.key === "string" && "action" in openItem && openItem.action);
    assert.ok(Array.from(openItem.key).length <= 128);
    assert.ok(Array.from(openItem.action.id).length <= 128);
    await h.call("ui.page.event", { session: "long-v1", ticket: "host-ticket", interaction: interaction(snap, openItem.key) });
    snap = await settled(h, "long-v1", `dotnet add package ${longID} --version 1.0.0`);

    // Refresh once, then navigate back: cached search should NOT be bypassed.
    await h.call("ui.page.event", { session: "long-v1", ticket: "host-ticket", interaction: interaction(snap, "nuget-refresh") });
    snap = await settled(h, "long-v1", `dotnet add package ${longID} --version 1.0.0`);
    const hitsBeforeBack = h.fixture.hits.get("https://azuresearch-usnc.nuget.org" + longSearch12) ?? 0;
    await h.call("ui.page.event", { session: "long-v1", ticket: "host-ticket", interaction: interaction(snap, "nuget-back") });
    await settled(h, "long-v1", `${longID}@1.0.0`);
    assert.equal(h.fixture.hits.get("https://azuresearch-usnc.nuget.org" + longSearch12), hitsBeforeBack, "refresh must reset after navigation");

    // Modern ui_version: 5 also keeps bounded keys/action.id and pushes validated detail state.
    await h.call("ui.page.mount", { session: "long-v5", params: { contribution: "nuget", locale: "en", ui_version: 5 } });
    let snap5 = await settled(h, "long-v5", "nuget-welcome-text");
    await h.call("ui.page.event", { session: "long-v5", ticket: "host-ticket", interaction: interaction(snap5, "nuget-submit", { query: "longpkg" }) });
    snap5 = await settled(h, "long-v5", `${longID}@1.0.0`);
    const open5 = snap5.view.sections.flatMap(s => s.items).find(item => typeof item.key === "string" && item.key.startsWith("open-"));
    assert.ok(open5 && typeof open5.key === "string" && "action" in open5 && open5.action);
    assert.ok(Array.from(open5.key).length <= 128);
    assert.ok(Array.from(open5.action.id).length <= 128);
    const nav5 = (await h.call("ui.page.event", { session: "long-v5", ticket: "host-ticket", interaction: interaction(snap5, open5.key) })).result;
    assert.equal(nav5.result.push.ui, "nuget");
    await h.call("ui.page.mount", { session: "long-v5-detail", params: { contribution: "nuget", locale: "en", ui_version: 5, state: nav5.result.push.state } });
    await settled(h, "long-v5-detail", `dotnet add package ${longID} --version 1.0.0`);
  } finally { h.p.close(); }
});
