import { test } from "node:test";
import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import { resolve } from "node:path";
import { pathToFileURL } from "node:url";
import { createHash } from "node:crypto";
import { connect, type Program } from "@persona/extension";
import { Renderer, type Snapshot } from "@persona/extension/ui";
import { DockerHub } from "../src/service.ts";
import { page } from "../src/page.tsx";
import { repositoryPath, searchPath, tagsPath } from "../src/model.ts";
import { detailResponse, Fixture, ingress, interaction, tag, until } from "./fixtures.ts";
async function setup(initialize = true) {
  const bundle = await import(pathToFileURL(resolve("build/dockerhub.mjs")).href);
  const fixture = new Fixture(), frames: any[] = [];
  const p = connect(bundle.createProgram(fixture.fetch, { now: () => fixture.clock, monotonic: () => fixture.clock, limits: { gap: 0 } }) as Program, line => { const frame = JSON.parse(line); frames.push(frame); if (frame.method === "ui.error" || frame.method === "ui.page.error") console.error("UI diagnostic", JSON.stringify(frame)); });
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
  assert.equal(manifest.id, "dockerhub"); assert.equal(manifest.runtime.mode, "deno"); assert.equal(manifest.runtime.command, undefined);
  assert.equal(manifest.runtime.source.sha256, createHash("sha256").update(bytes).digest("hex"));
  assert.equal(manifest.contributes.ui[0].id, "dockerhub"); assert.equal(manifest.contributes.connectors[0].capabilities.includes("actions"), false);
  assert.deepEqual(manifest.permissions.network, ["hub.docker.com"]);
  assert.ok(bytes.length > 0 && bytes.length < (8 << 20));
  const bundle = await import(pathToFileURL(resolve("build/dockerhub.mjs")).href);
  assert.equal(bundle.default.fetch, undefined); assert.equal(bundle.default.id, "dockerhub");
});

test("protocol initialization, localized tools and unknown contributions are SDK-owned", async () => {
  const h = await setup(false);
  try {
    assert.equal((await h.call("connector.status")).error.code, "not_initialized");
    assert.equal((await h.call("initialize", { protocol: 2 })).error.code, "wrong_protocol");
    assert.equal((await h.call("initialize", { protocol: 1, config: {}, hub: { locale: "ja" } })).result.id, "dockerhub");
    assert.deepEqual((await h.call("ping")).result, {});
    assert.equal((await h.call("connector.status", { contribution: "missing" })).error.code, "no_contribution");
    assert.equal((await h.call("connector.unsupported", {})).error.code, "unknown_method");
    assert.ok((await h.call("connector.tools.list", {})).result.tools[0].description.includes("検索"));
    assert.ok((await h.call("connector.tools.call", { name: "dockerhub_search", args: { query: "bad\n" } })).error);
  } finally { h.p.close(); }
});

test("search and detail navigation retain official status, digests and platforms", async () => {
  const h = await setup();
  try {
    for (const version of [1, 3, 5]) {
      const session = `dockerhub-${version}`;
      await h.call("ui.page.mount", { session, params: { contribution: "dockerhub", locale: "en", ui_version: version } });
      let snapshot = await settled(h, session, "dockerhub-welcome-text");
      const button = snapshot.view.sections[0].items.find(item => item.key === "dockerhub-submit");
      assert.ok(button && "action" in button && button.action?.submit === true, "search submits current field values");
      await h.call("ui.page.event", { session, ticket: "host-ticket", interaction: interaction(snapshot, "dockerhub-submit", { query: "nginx" }) });
      snapshot = await settled(h, session, "open-library/nginx");
      const navigation = (await h.call("ui.page.event", { session, ticket: "host-ticket", interaction: interaction(snapshot, "open-library/nginx") })).result;
      if (navigation.result.push) {
        const mounted = await h.call("ui.page.mount", { session: session + "-detail", params: { contribution: "dockerhub", locale: "en", ui_version: version, state: navigation.result.push.state } });
        assert.equal(mounted.error, undefined, JSON.stringify({ navigation, mounted }));
        assert.equal((await h.call("ui.page.snapshot", { session: session + "-detail" })).error, undefined, "new navigation mount is immediately readable");
      }
      snapshot = await settled(h, navigation.result.push ? session + "-detail" : session, "docker pull library/nginx").catch(async error => {
        const snapshot = await h.call("ui.page.snapshot", { session });
        throw new Error(JSON.stringify({ version, navigation, session, snapshot }) + ": " + error);
      });
      assert.ok(body(snapshot).includes("https://hub.docker.com/r/library/nginx"));
      assert.ok(body(snapshot).includes("sha256:e2355aa95529a7999dad94ddcb45a8768abf6fb0b204c93b1a3ffce477d7b893"));
      assert.ok(body(snapshot).includes("linux/arm64/v8"));
      assert.ok(!body(snapshot).includes("unknown/unknown"));
      await h.call("ui.page.dispose", { session });
    }
  } finally { h.p.close(); }
});

test("shared connector tools, trusted tickets and replay controls", async () => {
  const h = await setup();
  try {
    await h.call("ui.page.mount", { session: "owned", params: { contribution: "dockerhub", locale: "en", ui_version: 5 } });
    const snapshot = await settled(h, "owned", "dockerhub-welcome-text"), event = interaction(snapshot, "dockerhub-submit", { query: "nginx" });
    assert.ok((await h.call("ui.page.event", { session: "owned", interaction: event })).error);
    const accepted = (await h.call("ui.page.event", { session: "owned", ticket: "host-ticket", interaction: event })).result;
    if (accepted?.snapshot) {
      const replay = (await h.call("ui.page.event", { session: "owned", ticket: "host-ticket", interaction: event })).result;
      assert.equal(replay.snapshot.version, accepted.snapshot.version);
    } else assert.equal(accepted.status, "accepted");
    assert.equal((await h.call("connector.tools.call", { name: "dockerhub_search", args: { query: "nginx" } })).result.text.includes("library/nginx"), true);
    assert.equal(h.fixture.hits.size > 0, true);
    await h.call("ui.page.dispose", { session: "owned" });
    const remount: Snapshot = (await h.call("ui.page.mount", { session: "new", params: { contribution: "dockerhub", ui_version: 5 } })).result;
    assert.notEqual(remount.generation, snapshot.generation);
    assert.equal((await h.call("ui.page.event", { session: "new", ticket: "host-ticket", interaction: event })).result.status, "ambiguous");
  } finally { h.p.close(); }
});

test("modern navigation mounts the validated selected repository detail", async () => {
  const h = await setup();
  try {
    await h.call("ui.page.mount", { session: "search", params: { contribution: "dockerhub", locale: "en", ui_version: 5 } });
    const initial = await settled(h, "search", "dockerhub-welcome-text");
    await h.call("ui.page.event", { session: "search", ticket: "host-ticket", interaction: interaction(initial, "dockerhub-submit", { query: "nginx" }) });
    const results = await settled(h, "search", "open-nginx/nginx-ingress");
    const navigation = (await h.call("ui.page.event", { session: "search", ticket: "host-ticket", interaction: interaction(results, "open-nginx/nginx-ingress") })).result;
    assert.equal(navigation.result.push.ui, "dockerhub");
    await h.call("ui.page.mount", { session: "detail", params: { contribution: "dockerhub", locale: "en", ui_version: 5, state: navigation.result.push.state } });
    const detail = await settled(h, "detail", "Namespace nginx");
    assert.ok(body(detail).includes("Community") || body(detail).includes("Namespace nginx"));
  } finally { h.p.close(); }
});

test("snapshot transport mounts the initial editor without a pending load", async () => {
  const h = await setup();
  try {
    const mounted = (await h.call("ui.page.mount", { session: "snapshot-editor", transport: "snapshot", params: { contribution: "dockerhub", locale: "zh-Hans", ui_version: 5 } })).result;
    assert.ok(JSON.stringify(mounted).includes("镜像名称或关键词"));
    assert.equal(mounted.view.sections.some((section: { key: string }) => section.key === "dockerhub-loading"), false);
  } finally { h.p.close(); }
});

test("invalid state cannot trigger arbitrary requests; pure snapshots are supported", async () => {
  const h = await setup();
  try {
    await h.call("ui.page.mount", { session: "bad", params: { contribution: "dockerhub", locale: "ja", ui_version: 1, state: "not-json" } });
    await settled(h, "bad", "Docker Hub を検索");
    const local = new h.bundle.DockerHub(h.fixture.fetch, {}, { locale: "en", now: () => h.fixture.clock, limits: { gap: 0 } });
    const pure = h.bundle.snapshotPage(local);
    const view = await pure.render({ contribution: "dockerhub", locale: "en", ui_version: 1, hub: {}, state: h.bundle.encodeState({ q: "nginx", found: true }) });
    assert.ok(JSON.stringify(view).includes("library/nginx"));
  } finally { h.p.close(); }
});

test("long repository and tag names, duplicate items, and large direct renders stay within CheckView budgets", async () => {
  const h = await setup();
  try {
    const longNs = "n".repeat(64);
    const longName = "r".repeat(100);
    const longRepo = `${longNs}/${longName}`;
    const longTag = "t".repeat(128);
    h.fixture.put(searchPath("longrepo", 12), { count: 1, results: [{ ...ingress, repo_name: longRepo, short_description: "Long repo desc" }] });
    h.fixture.put(repositoryPath(longRepo), { ...detailResponse, namespace: longNs, name: longName, description: "Long detail desc" });
    h.fixture.put(tagsPath(longRepo), { count: 1, results: [{ ...tag, name: longTag }] });

    await h.call("ui.page.mount", { session: "long-v1", params: { contribution: "dockerhub", locale: "en", ui_version: 1, state: h.bundle.encodeState({ q: "longrepo", found: true }) } });
    const listSnap = await settled(h, "long-v1", "Long repo desc");
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
    const detailSnap = await settled(h, "long-v1", `docker pull ${longRepo}`);
    const detailKeys = new Set<string>();
    for (const section of detailSnap.view.sections) {
      if (section.key) {
        assert.ok(Array.from(section.key).length <= 128, `detail section key too long: ${Array.from(section.key).length}`);
        assert.ok(!detailKeys.has(section.key), `duplicate detail section key ${section.key}`);
        detailKeys.add(section.key);
      }
      for (const item of section.items) {
        if (item.key) {
          assert.ok(Array.from(item.key).length <= 128, `detail item key too long: ${Array.from(item.key).length}`);
          assert.ok(!detailKeys.has(item.key), `duplicate detail item key ${item.key}`);
          detailKeys.add(item.key);
        }
      }
    }

    // Direct page() renders with > 120 images and > 120 tags (with duplicates, > 2000-rune descriptions, invalid dates, oversized state.q) stay within CheckView budgets.
    const local = new DockerHub(h.fixture.fetch, {}, { locale: "en", now: () => h.fixture.clock, limits: { gap: 0 } });
    const manyImages = Array.from({ length: 120 }, (_, i) => ({
      name: i < 2 ? longRepo : `${"a".repeat(60)}/repo-${i}-${"b".repeat(80)}`,
      description: "D".repeat(2500),
      pulls: i,
      stars: i,
      official: false,
      url: `https://hub.docker.com/r/org/repo-${i}`,
    }));
    const manyTags = Array.from({ length: 120 }, (_, i) => ({
      name: i < 2 ? longTag : `tag-${i}-${"x".repeat(120)}`,
      digest: "sha256:" + "a".repeat(64),
      updated: "not-a-valid-date",
      platforms: ["linux/amd64"],
    }));
    for (const loaded of [
      { kind: "results" as const, result: { images: manyImages, total: manyImages.length, at: h.fixture.clock } },
      { kind: "detail" as const, result: { image: { name: longRepo, namespace: longNs, description: "D".repeat(2500), pulls: 1, stars: 1, official: false, url: `https://hub.docker.com/r/${longRepo}`, updated: "not-a-valid-date", pull: `docker pull ${longRepo}`, tags: manyTags }, at: h.fixture.clock } },
    ]) {
      const renderer = new Renderer({ authorize: () => false });
      try {
        const rendered = renderer.mount(page({
          service: local,
          state: { q: "q".repeat(17000), found: true },
          loaded,
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
    }
  } finally { h.p.close(); }
});

test("manual refresh resets after navigating to a new search and RPC rejects explicit limit: 0", async () => {
  const h = await setup();
  try {
    const nginxPath = searchPath("nginx", 12);
    const redisPath = searchPath("redis", 12);
    h.fixture.put(redisPath, { count: 1, results: [{ ...ingress, repo_name: "redis", short_description: "Redis in-memory store", is_official: true }] });
    await h.call("ui.page.mount", { session: "nav", params: { contribution: "dockerhub", locale: "en", ui_version: 5 } });
    let snapshot = await settled(h, "nav", "dockerhub-welcome-text");

    await h.call("ui.page.event", { session: "nav", ticket: "host-ticket", interaction: interaction(snapshot, "dockerhub-submit", { query: "nginx" }) });
    snapshot = await settled(h, "nav", "library/nginx");
    assert.equal(h.fixture.hits.get(nginxPath), 1);

    await h.call("ui.page.event", { session: "nav", ticket: "host-ticket", interaction: interaction(snapshot, "dockerhub-submit", { query: "redis" }) });
    snapshot = await settled(h, "nav", "library/redis");
    assert.equal(h.fixture.hits.get(redisPath), 1);

    await h.call("ui.page.event", { session: "nav", ticket: "host-ticket", interaction: interaction(snapshot, "dockerhub-refresh") });
    await until(async () => h.fixture.hits.get(redisPath) ?? 0, count => count === 2);
    snapshot = await settled(h, "nav", "library/redis");

    // Navigating back to cached "nginx" must reuse cache (still hit 1).
    await h.call("ui.page.event", { session: "nav", ticket: "host-ticket", interaction: interaction(snapshot, "dockerhub-submit", { query: "nginx" }) });
    snapshot = await settled(h, "nav", "library/nginx");
    assert.equal(h.fixture.hits.get(nginxPath), 1);

    // End-to-end RPC ExactJSON: explicit limit: 0 must be rejected as invalid.
    const zeroLimit = await h.call("connector.tools.call", { name: "dockerhub_search", args: { query: "nginx", limit: 0 } });
    assert.equal(zeroLimit.error?.code, "invalid");
  } finally { h.p.close(); }
});
