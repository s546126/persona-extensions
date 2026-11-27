import { test } from "node:test";
import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import { resolve } from "node:path";
import { pathToFileURL } from "node:url";
import { createHash } from "node:crypto";
import { connect, type Program } from "@persona/extension";
import { Renderer, type Snapshot } from "@persona/extension/ui";
import { Archive } from "../src/service.ts";
import { page } from "../src/page.tsx";
import { Fixture, interaction, until } from "./fixtures.ts";
async function setup(initialize = true) {
  const bundle = await import(pathToFileURL(resolve("build/archive.mjs")).href);
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
  assert.equal(manifest.id, "archive"); assert.equal(manifest.runtime.mode, "deno"); assert.equal(manifest.runtime.command, undefined);
  assert.equal(manifest.runtime.source.sha256, createHash("sha256").update(bytes).digest("hex"));
  assert.equal(manifest.contributes.ui[0].id, "collections"); assert.equal(manifest.contributes.connectors[0].capabilities.includes("actions"), false);
  assert.deepEqual(manifest.permissions.network, ["archive.org"]);
  assert.ok(bytes.length > 0 && bytes.length < (8 << 20));
  const bundle = await import(pathToFileURL(resolve("build/archive.mjs")).href);
  assert.equal(bundle.default.fetch, undefined); assert.equal(bundle.default.id, "archive");
});

test("protocol initialization, localized tools and unknown contributions are SDK-owned", async () => {
  const h = await setup(false);
  try {
    assert.equal((await h.call("connector.status")).error.code, "not_initialized");
    assert.equal((await h.call("initialize", { protocol: 2 })).error.code, "wrong_protocol");
    assert.equal((await h.call("initialize", { protocol: 1, config: {}, hub: { locale: "ja" } })).result.id, "archive");
    assert.deepEqual((await h.call("ping")).result, {});
    assert.equal((await h.call("connector.status", { contribution: "missing" })).error.code, "no_contribution");
    assert.equal((await h.call("connector.unsupported", {})).error.code, "unknown_method");
    assert.ok((await h.call("connector.tools.list", {})).result.tools[0].description.includes("検索"));
    assert.ok((await h.call("connector.tools.call", { name: "archive_search", args: { query: "bad\n" } })).error);
  } finally { h.p.close(); }
});

test("archive search renders item metadata and canonical links with native hooks", async () => {
  const h = await setup();
  try {
    for (const version of [1, 3, 5]) {
      const session = `archive-${version}`;
      await h.call("ui.page.mount", { session, params: { contribution: "collections", locale: "en", ui_version: version } });
      let snapshot = await settled(h, session, "archive-welcome-text");
      const button = snapshot.view.sections[0].items.find(item => item.key === "archive-submit");
      assert.ok(button && "action" in button && button.action?.submit === true, "search submits current field values");
      await h.call("ui.page.event", { session, ticket: "host-ticket", interaction: interaction(snapshot, "archive-submit", { query: "synthetic" }) });
      snapshot = await settled(h, session, "Pride and Prejudice");
      assert.ok(body(snapshot).includes("Jane Austen")); assert.ok(body(snapshot).includes("1813"));
      const rendered: any = JSON.parse(body(snapshot));
      assert.ok(rendered.sections.every((section: any) => section.items.every((item: any) => item.text !== "")));
      assert.ok(body(snapshot).includes("https://archive.org/details/prideprejudice_0000"));
      assert.ok(!body(snapshot).includes("attacker.invalid"));
      await h.call("ui.page.dispose", { session });
    }
  } finally { h.p.close(); }
});

test("shared connector tools and cache, trusted tickets and replay controls", async () => {
  const h = await setup();
  try {
    await h.call("ui.page.mount", { session: "owned", params: { contribution: "collections", locale: "en", ui_version: 5 } });
    const snapshot = await settled(h, "owned", "archive-welcome-text"), event = interaction(snapshot, "archive-submit", { query: "synthetic" });
    assert.ok((await h.call("ui.page.event", { session: "owned", interaction: event })).error);
    const accepted = (await h.call("ui.page.event", { session: "owned", ticket: "host-ticket", interaction: event })).result;
    if (accepted?.snapshot) {
      const replay = (await h.call("ui.page.event", { session: "owned", ticket: "host-ticket", interaction: event })).result;
      assert.equal(replay.snapshot.version, accepted.snapshot.version);
    } else assert.equal(accepted.status, "accepted");
    assert.equal((await h.call("connector.tools.call", { name: "archive_search", args: { query: "synthetic" } })).result.text.includes("Pride and Prejudice"), true);
    assert.equal(h.fixture.hits.size > 0, true);
    await h.call("ui.page.dispose", { session: "owned" });
    const remount: Snapshot = (await h.call("ui.page.mount", { session: "new", params: { contribution: "collections", ui_version: 5 } })).result;
    assert.notEqual(remount.generation, snapshot.generation);
    assert.equal((await h.call("ui.page.event", { session: "new", ticket: "host-ticket", interaction: event })).result.status, "ambiguous");
  } finally { h.p.close(); }
});

test("invalid state cannot trigger arbitrary requests; pure snapshots are supported", async () => {
  const h = await setup();
  try {
    await h.call("ui.page.mount", { session: "bad", params: { contribution: "collections", locale: "zh-Hans", ui_version: 1, state: "not-json" } });
    await settled(h, "bad", "馆藏检索");
    const local = new h.bundle.Archive(h.fixture.fetch, {}, { locale: "en", now: () => h.fixture.clock, limits: { gap: 0 } });
    const pure = h.bundle.snapshotPage(local);
    const view = await pure.render({ contribution: "collections", locale: "en", ui_version: 1, hub: {}, state: h.bundle.encodeState({ q: "synthetic", found: true }) });
    assert.ok(JSON.stringify(view).includes("Pride and Prejudice"));
  } finally { h.p.close(); }
});

test("long identifiers, creators, large result sets, and empty tz stay within CheckView budgets", async () => {
  const h = await setup();
  try {
    const longId = "a".repeat(200);
    const secondLongId = "a".repeat(199) + "b";
    let fnv = 0x811c9dc5;
    for (let i = 0; i < longId.length; i++) { fnv ^= longId.charCodeAt(i); fnv = Math.imul(fnv, 0x01000193); }
    const longHash = (fnv >>> 0).toString(16).padStart(8, "0");
    const hyphenCollisionId = "a".repeat(107) + "-" + longHash;
    const bigPath = "/advancedsearch.php?q=%28longcheck%29&fl=identifier%2Ctitle%2Ccreator%2Cyear%2Cmediatype%2Cdownloads%2Cdescription&rows=20&page=1&output=json";
    h.fixture.put(bigPath, {
      response: {
        numFound: 3,
        docs: [
          {
            identifier: longId,
            title: "T".repeat(600),
            creator: Array.from({ length: 8 }, (_, i) => `Creator-${i}-${"c".repeat(280)}`),
            year: 2024,
            mediatype: "texts",
            downloads: 999,
            description: "D".repeat(3000),
          },
          {
            identifier: secondLongId,
            title: "Second item",
            creator: ["Author"],
            year: 2025,
            mediatype: "audio",
            downloads: 12,
            description: "Second description",
          },
          {
            identifier: hyphenCollisionId,
            title: "Third item",
            creator: ["Author"],
            year: 2025,
            mediatype: "texts",
            downloads: 5,
            description: "Third description",
          },
        ],
      },
    });
    await h.call("ui.page.mount", { session: "huge", params: { contribution: "collections", locale: "en", tz: "", ui_version: 7, state: h.bundle.encodeState({ q: "longcheck", found: true }) } });
    const snapshot = await settled(h, "huge", "Third item");
    const keys = new Set<string>();
    const totalItems = snapshot.view.sections.reduce((sum, s) => sum + s.items.length, 0);
    assert.ok(totalItems <= 300, `expected <= 300 items, got ${totalItems}`);
    for (const section of snapshot.view.sections) {
      assert.ok(section.items.length > 0, `empty section ${section.key}`);
      if (section.key) {
        assert.ok(Array.from(section.key).length <= 128, `section key too long: ${Array.from(section.key).length}`);
        assert.ok(!keys.has(section.key), `duplicate section key ${section.key}`);
        keys.add(section.key);
      }
      if (section.title) assert.ok(Array.from(section.title).length <= 2000, `section title too long`);
      if (section.footer) assert.ok(Array.from(section.footer).length <= 2000, `section footer too long: ${Array.from(section.footer).length}`);
      for (const item of section.items) {
        if (item.key) {
          assert.ok(Array.from(item.key).length <= 128, `item key too long: ${Array.from(item.key).length}`);
          assert.ok(!keys.has(item.key), `duplicate item key ${item.key}`);
          keys.add(item.key);
        }
        if (item.type === "text") assert.ok(Array.from(item.text).length <= 2000, `text too long: ${Array.from(item.text).length}`);
        if (item.type === "link") assert.ok(Array.from(item.url).length <= 2000, `url too long`);
      }
    }
    assert.ok(new TextEncoder().encode(JSON.stringify(snapshot)).byteLength <= 1048576);

    // Direct page() render with > 150 items, duplicate identifiers, oversized state.q, and > 2000-char description stays within CheckView limits.
    const local = new Archive(h.fixture.fetch, {}, { locale: "en", now: () => h.fixture.clock, limits: { gap: 0 } });
    const manyItems = Array.from({ length: 180 }, (_, i) => ({
      identifier: i < 2 ? `dup-${"x".repeat(160)}` : `item-${i}-${"x".repeat(160)}`,
      title: `Title ${i}`,
      creators: Array.from({ length: 8 }, (_, c) => `C${c}-${"y".repeat(280)}`),
      year: 2024,
      media: "texts" as const,
      downloads: i,
      description: "Z".repeat(3500),
      url: `https://archive.org/details/item-${i}`,
    }));
    const renderer = new Renderer({ authorize: () => false });
    try {
      const rendered = renderer.mount(page({
        service: local,
        state: { q: "q".repeat(17000), found: true },
        loaded: { kind: "result", result: { items: manyItems, total: manyItems.length, at: h.fixture.clock } },
        locale: "en",
        tz: "Invalid/Zone",
        uiVersion: 7,
      })).view;
      const directItems = rendered.sections.reduce((sum, s) => sum + s.items.length, 0);
      assert.ok(directItems <= 300, `expected <= 300 items in direct render, got ${directItems}`);
      assert.ok(new TextEncoder().encode(rendered.state ?? "").byteLength <= 16384);
      const directKeys = new Set<string>();
      for (const section of rendered.sections) {
        if (section.key) {
          assert.ok(Array.from(section.key).length <= 128);
          assert.ok(!directKeys.has(section.key), `duplicate direct section key ${section.key}`);
          directKeys.add(section.key);
        }
        if (section.footer) assert.ok(Array.from(section.footer).length <= 2000);
        for (const item of section.items) {
          if (item.key) {
            assert.ok(Array.from(item.key).length <= 128);
            assert.ok(!directKeys.has(item.key), `duplicate direct item key ${item.key}`);
            directKeys.add(item.key);
          }
          if (item.type === "text") assert.ok(Array.from(item.text).length <= 2000, `direct text too long: ${Array.from(item.text).length}`);
          if (item.type === "field" && item.value) assert.ok(Array.from(item.value).length <= 2000, `direct field value too long`);
        }
      }
    } finally { renderer.dispose(); }
  } finally { h.p.close(); }
});

test("manual refresh resets after navigating to a new search and RPC rejects explicit limit: 0", async () => {
  const h = await setup();
  try {
    const altPath = "/advancedsearch.php?q=%28altquery%29&fl=identifier%2Ctitle%2Ccreator%2Cyear%2Cmediatype%2Cdownloads%2Cdescription&rows=20&page=1&output=json";
    const synthPath = "/advancedsearch.php?q=%28synthetic%29&fl=identifier%2Ctitle%2Ccreator%2Cyear%2Cmediatype%2Cdownloads%2Cdescription&rows=20&page=1&output=json";
    h.fixture.put(altPath, {
      response: { numFound: 1, docs: [{ identifier: "alt_doc", title: "Alt Document", mediatype: "texts", downloads: 7, description: "Alt desc." }] },
    });
    await h.call("ui.page.mount", { session: "nav", params: { contribution: "collections", locale: "en", ui_version: 5 } });
    let snapshot = await settled(h, "nav", "archive-welcome-text");
    // Search "synthetic" (hit 1), then "altquery" (hit 1).
    await h.call("ui.page.event", { session: "nav", ticket: "host-ticket", interaction: interaction(snapshot, "archive-submit", { query: "synthetic" }) });
    snapshot = await settled(h, "nav", "Pride and Prejudice");
    assert.equal(h.fixture.hits.get(synthPath), 1);

    await h.call("ui.page.event", { session: "nav", ticket: "host-ticket", interaction: interaction(snapshot, "archive-submit", { query: "altquery" }) });
    snapshot = await settled(h, "nav", "Alt Document");
    assert.equal(h.fixture.hits.get(altPath), 1);

    // Manually refresh "altquery" (hit 2).
    await h.call("ui.page.event", { session: "nav", ticket: "host-ticket", interaction: interaction(snapshot, "archive-refresh") });
    await until(async () => h.fixture.hits.get(altPath) ?? 0, count => count === 2);
    snapshot = await settled(h, "nav", "Alt Document");

    // Navigate back to cached "synthetic": refresh must have reset to 0 so cache is reused (still hit 1).
    await h.call("ui.page.event", { session: "nav", ticket: "host-ticket", interaction: interaction(snapshot, "archive-submit", { query: "synthetic" }) });
    snapshot = await settled(h, "nav", "Pride and Prejudice");
    assert.equal(h.fixture.hits.get(synthPath), 1);

    // End-to-end RPC ExactJSON: explicit limit: 0 must be rejected as invalid.
    const zeroLimit = await h.call("connector.tools.call", { name: "archive_search", args: { query: "synthetic", limit: 0 } });
    assert.equal(zeroLimit.error?.code, "invalid");
  } finally { h.p.close(); }
});
