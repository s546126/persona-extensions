import { test } from "node:test";
import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import { resolve } from "node:path";
import { pathToFileURL } from "node:url";
import { createHash } from "node:crypto";
import { connect, type Program } from "@persona/extension";
import { Renderer, type Snapshot } from "@persona/extension/ui";
import { Commons } from "../src/service.ts";
import { page } from "../src/page.tsx";
import { Fixture, interaction, until } from "./fixtures.ts";
async function setup(initialize = true) {
  const bundle = await import(pathToFileURL(resolve("build/commons.mjs")).href);
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
  assert.equal(manifest.id, "commons"); assert.equal(manifest.runtime.mode, "deno"); assert.equal(manifest.runtime.command, undefined);
  assert.equal(manifest.runtime.source.sha256, createHash("sha256").update(bytes).digest("hex"));
  assert.equal(manifest.contributes.ui[0].id, "search"); assert.equal(manifest.contributes.connectors[0].capabilities.includes("actions"), false);
  assert.deepEqual(manifest.permissions.network, ["commons.wikimedia.org", "upload.wikimedia.org", "thumb.wikimedia.org"]);
  assert.ok(bytes.length > 0 && bytes.length < (8 << 20));
  const bundle = await import(pathToFileURL(resolve("build/commons.mjs")).href);
  assert.equal(bundle.default.fetch, undefined); assert.equal(bundle.default.id, "commons");
});

test("protocol initialization, localized tools and unknown contributions are SDK-owned", async () => {
  const h = await setup(false);
  try {
    assert.equal((await h.call("connector.status")).error.code, "not_initialized");
    assert.equal((await h.call("initialize", { protocol: 2 })).error.code, "wrong_protocol");
    assert.equal((await h.call("initialize", { protocol: 1, config: {}, hub: { locale: "ja" } })).result.id, "commons");
    assert.deepEqual((await h.call("ping")).result, {});
    assert.equal((await h.call("connector.status", { contribution: "missing" })).error.code, "no_contribution");
    assert.equal((await h.call("connector.unsupported", {})).error.code, "unknown_method");
    assert.ok((await h.call("connector.tools.list", {})).result.tools[0].description.includes("検索"));
    assert.ok((await h.call("connector.tools.call", { name: "commons_search", args: { query: "bad\n" } })).error);
  } finally { h.p.close(); }
});

test("image search renders real thumbnails, attribution and license links with native hooks", async () => {
  const h = await setup();
  try {
    for (const version of [1, 3, 5]) {
      const session = `commons-${version}`;
      await h.call("ui.page.mount", { session, params: { contribution: "search", locale: "en", ui_version: version } });
      let snapshot = await settled(h, session, "commons-welcome-text");
      const button = snapshot.view.sections[0].items.find(item => item.key === "commons-submit");
      assert.ok(button && "action" in button && button.action?.submit === true, "search submits current field values");
      await h.call("ui.page.event", { session, ticket: "host-ticket", interaction: interaction(snapshot, "commons-submit", { query: "Eiffel Tower" }) });
      snapshot = await settled(h, session, "Example Photographer");
      assert.ok(body(snapshot).includes("Public domain")); assert.ok(body(snapshot).includes("The Eiffel Tower in Paris."));
      assert.ok(body(snapshot).includes("https://upload.wikimedia.org/")); assert.ok(body(snapshot).includes("https://creativecommons.org/"));
      const rendered: any = JSON.parse(body(snapshot));
      const images = rendered.sections.flatMap((section: any) => section.items).filter((item: any) => item.type === "image");
      assert.ok(images.every((image: any) => image.alt.length <= 200));
      assert.ok(!body(snapshot).includes("attacker.invalid"));
      await h.call("ui.page.dispose", { session });
    }
  } finally { h.p.close(); }
});

test("shared connector tools and cache, trusted tickets and replay controls", async () => {
  const h = await setup();
  try {
    await h.call("ui.page.mount", { session: "owned", params: { contribution: "search", locale: "en", ui_version: 5 } });
    const snapshot = await settled(h, "owned", "commons-welcome-text"), event = interaction(snapshot, "commons-submit", { query: "Eiffel Tower" });
    assert.ok((await h.call("ui.page.event", { session: "owned", interaction: event })).error);
    const accepted = (await h.call("ui.page.event", { session: "owned", ticket: "host-ticket", interaction: event })).result;
    if (accepted?.snapshot) {
      const replay = (await h.call("ui.page.event", { session: "owned", ticket: "host-ticket", interaction: event })).result;
      assert.equal(replay.snapshot.version, accepted.snapshot.version);
    } else assert.equal(accepted.status, "accepted");
    assert.equal((await h.call("connector.tools.call", { name: "commons_search", args: { query: "Eiffel Tower" } })).result.text.includes("Example Photographer"), true);
    assert.equal(h.fixture.hits.size > 0, true);
    await h.call("ui.page.dispose", { session: "owned" });
    const remount: Snapshot = (await h.call("ui.page.mount", { session: "new", params: { contribution: "search", ui_version: 5 } })).result;
    assert.notEqual(remount.generation, snapshot.generation);
    assert.equal((await h.call("ui.page.event", { session: "new", ticket: "host-ticket", interaction: event })).result.status, "ambiguous");
  } finally { h.p.close(); }
});

test("invalid state cannot trigger arbitrary requests; pure snapshots are supported", async () => {
  const h = await setup();
  try {
    await h.call("ui.page.mount", { session: "bad", params: { contribution: "search", locale: "zh-Hans", ui_version: 1, state: "not-json" } });
    await settled(h, "bad", "图片检索");
    const local = new h.bundle.Commons(h.fixture.fetch, {}, { locale: "en", now: () => h.fixture.clock, limits: { gap: 0 } });
    const pure = h.bundle.snapshotPage(local);
    const view = await pure.render({ contribution: "search", locale: "en", ui_version: 1, hub: {}, state: h.bundle.encodeState({ q: "Eiffel Tower", found: true }) });
    assert.ok(JSON.stringify(view).includes("Example Photographer"));
  } finally { h.p.close(); }
});

test("long metadata, out-of-range thumbnail dimensions, empty searches, and large direct renders stay within CheckView budgets", async () => {
  const h = await setup();
  try {
    const longPath = "/w/api.php?action=query&generator=search&gsrsearch=File%3Alongcheck&gsrnamespace=6&gsrlimit=20&prop=imageinfo&iiprop=url%7Cextmetadata%7Csize&iiurlwidth=640&format=json";
    h.fixture.put(longPath, {
      query: {
        pages: {
          101: {
            pageid: 101,
            ns: 6,
            title: "File:Tall Panorama.jpg",
            imageinfo: [{
              descriptionurl: "https://commons.wikimedia.org/wiki/File:Tall_Panorama.jpg",
              thumburl: "https://upload.wikimedia.org/wikipedia/commons/thumb/a/a8/Tall_Panorama.jpg/640px-Tall_Panorama.jpg",
              thumbwidth: 640,
              thumbheight: 12000,
              extmetadata: {
                ImageDescription: { value: "😀".repeat(250) },
                Artist: { value: "A".repeat(3500) },
                LicenseShortName: { value: "L".repeat(3500) },
              },
            }],
          },
          102: {
            pageid: 102,
            ns: 6,
            title: "File:Zero Dim.svg",
            imageinfo: [{
              descriptionurl: "https://commons.wikimedia.org/wiki/File:Zero_Dim.svg",
              thumburl: "https://upload.wikimedia.org/wikipedia/commons/thumb/a/a8/Zero_Dim.svg/640px-Zero_Dim.svg.png",
              thumbwidth: 0,
              thumbheight: 0,
              extmetadata: {
                ImageDescription: { value: "Zero dim SVG" },
                Artist: { value: "Author" },
                LicenseShortName: { value: "CC0" },
              },
            }],
          },
        },
      },
    });
    await h.call("ui.page.mount", { session: "long", params: { contribution: "search", locale: "en", tz: "", ui_version: 7, state: h.bundle.encodeState({ q: "longcheck", found: true }) } });
    const snapshot = await settled(h, "long", "Zero Dim.svg");
    const keys = new Set<string>();
    const totalItems = snapshot.view.sections.reduce((sum, s) => sum + s.items.length, 0);
    assert.ok(totalItems <= 300, `expected <= 300 items, got ${totalItems}`);
    for (const section of snapshot.view.sections) {
      assert.ok(section.items.length > 0, `empty section ${section.key}`);
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
        if (item.type === "text") {
          assert.ok(Array.from(item.text).length >= 1 && Array.from(item.text).length <= 2000, `text runes out of range: ${Array.from(item.text).length}`);
          assert.ok(!/[\ud800-\udfff]/u.test(item.text), "text contains lone surrogate");
        }
        if (item.type === "image") {
          assert.ok(Array.from(item.alt).length >= 1 && Array.from(item.alt).length <= 200, `alt runes out of range: ${Array.from(item.alt).length}`);
          assert.ok(!/[\ud800-\udfff]/u.test(item.alt), "alt contains lone surrogate");
          if (item.caption) assert.ok(Array.from(item.caption).length <= 2000);
          if (item.src.width !== undefined) assert.ok(item.src.width >= 1 && item.src.width <= 8192, `invalid width: ${item.src.width}`);
          if (item.src.height !== undefined) assert.ok(item.src.height >= 1 && item.src.height <= 8192, `invalid height: ${item.src.height}`);
        }
      }
    }

    // MediaWiki zero-result search renders empty state cleanly.
    const emptyPath = "/w/api.php?action=query&generator=search&gsrsearch=File%3Anomatches&gsrnamespace=6&gsrlimit=20&prop=imageinfo&iiprop=url%7Cextmetadata%7Csize&iiurlwidth=640&format=json";
    h.fixture.put(emptyPath, { batchcomplete: "" });
    await h.call("ui.page.mount", { session: "zero", params: { contribution: "search", locale: "en", ui_version: 7, state: h.bundle.encodeState({ q: "nomatches", found: true }) } });
    const zeroSnap = await settled(h, "zero", "commons-empty-text");
    assert.ok(body(zeroSnap).includes("No images found"));

    // Direct page() render with > 50 images, duplicate IDs, oversized state.q, and > 2000-rune fields stays within CheckView limits.
    const local = new Commons(h.fixture.fetch, {}, { locale: "en", now: () => h.fixture.clock, limits: { gap: 0 } });
    const manyImages = Array.from({ length: 80 }, (_, i) => ({
      id: i < 2 ? 7 : i + 10,
      title: `File:Image_${i}_${"t".repeat(250)}.jpg`,
      description: "😀".repeat(250),
      artist: "A".repeat(3500),
      license: "L".repeat(3500),
      licenseURL: "https://creativecommons.org/publicdomain/mark/1.0/",
      pageURL: `https://commons.wikimedia.org/wiki/File:Image_${i}.jpg`,
      thumbnailURL: `https://upload.wikimedia.org/wikipedia/commons/thumb/a/a8/Image_${i}.jpg/640px-Image_${i}.jpg`,
      width: i === 0 ? 0 : 640,
      height: i === 0 ? 0 : 12000,
    }));
    const renderer = new Renderer({ authorize: () => false });
    try {
      const rendered = renderer.mount(page({
        service: local,
        state: { q: "q".repeat(17000), found: true },
        loaded: { kind: "result", result: { images: manyImages, at: h.fixture.clock } },
        locale: "en",
        tz: "Invalid/Zone",
        uiVersion: 7,
      })).view;
      const directItems = rendered.sections.reduce((sum, s) => sum + s.items.length, 0);
      const directImages = rendered.sections.flatMap(s => s.items).filter(item => item.type === "image").length;
      assert.ok(directItems <= 300, `expected <= 300 items, got ${directItems}`);
      assert.ok(directImages <= 50, `expected <= 50 images, got ${directImages}`);
      assert.ok(new TextEncoder().encode(rendered.state ?? "").byteLength <= 16384);
      const directKeys = new Set<string>();
      for (const section of rendered.sections) {
        if (section.key) {
          assert.ok(Array.from(section.key).length <= 128);
          assert.ok(!directKeys.has(section.key), `duplicate direct section key ${section.key}`);
          directKeys.add(section.key);
        }
        if (section.title) assert.ok(Array.from(section.title).length <= 2000);
        if (section.footer) assert.ok(Array.from(section.footer).length <= 2000);
        for (const item of section.items) {
          if (item.key) {
            assert.ok(Array.from(item.key).length <= 128);
            assert.ok(!directKeys.has(item.key), `duplicate direct item key ${item.key}`);
            directKeys.add(item.key);
          }
          if (item.type === "text") assert.ok(Array.from(item.text).length <= 2000);
          if (item.type === "field" && item.value) assert.ok(Array.from(item.value).length <= 2000);
          if (item.type === "image") {
            assert.ok(Array.from(item.alt).length >= 1 && Array.from(item.alt).length <= 200);
            assert.ok(!/[\ud800-\udfff]/u.test(item.alt));
            if (item.src.width !== undefined) assert.ok(item.src.width >= 1 && item.src.width <= 8192);
            if (item.src.height !== undefined) assert.ok(item.src.height >= 1 && item.src.height <= 8192);
          }
        }
      }
    } finally { renderer.dispose(); }
  } finally { h.p.close(); }
});

test("manual refresh resets after navigating to a new search and RPC rejects explicit limit: 0", async () => {
  const h = await setup();
  try {
    const eiffelPath = "/w/api.php?action=query&generator=search&gsrsearch=File%3AEiffel+Tower&gsrnamespace=6&gsrlimit=20&prop=imageinfo&iiprop=url%7Cextmetadata%7Csize&iiurlwidth=640&format=json";
    const altPath = "/w/api.php?action=query&generator=search&gsrsearch=File%3Aaltquery&gsrnamespace=6&gsrlimit=20&prop=imageinfo&iiprop=url%7Cextmetadata%7Csize&iiurlwidth=640&format=json";
    h.fixture.put(altPath, {
      query: { pages: { 9001: { pageid: 9001, ns: 6, title: "File:Alt Image.jpg", imageinfo: [{ descriptionurl: "https://commons.wikimedia.org/wiki/File:Alt_Image.jpg", extmetadata: { Artist: { value: "Alt Artist" }, LicenseShortName: { value: "CC0" } } }] } } },
    });
    await h.call("ui.page.mount", { session: "nav", params: { contribution: "search", locale: "en", ui_version: 5 } });
    let snapshot = await settled(h, "nav", "commons-welcome-text");

    await h.call("ui.page.event", { session: "nav", ticket: "host-ticket", interaction: interaction(snapshot, "commons-submit", { query: "Eiffel Tower" }) });
    snapshot = await settled(h, "nav", "Example Photographer");
    assert.equal(h.fixture.hits.get(eiffelPath), 1);

    await h.call("ui.page.event", { session: "nav", ticket: "host-ticket", interaction: interaction(snapshot, "commons-submit", { query: "altquery" }) });
    snapshot = await settled(h, "nav", "Alt Artist");
    assert.equal(h.fixture.hits.get(altPath), 1);

    await h.call("ui.page.event", { session: "nav", ticket: "host-ticket", interaction: interaction(snapshot, "commons-refresh") });
    await until(async () => h.fixture.hits.get(altPath) ?? 0, count => count === 2);
    snapshot = await settled(h, "nav", "Alt Artist");

    // Navigating back to cached "Eiffel Tower" must reuse cache (still hit 1).
    await h.call("ui.page.event", { session: "nav", ticket: "host-ticket", interaction: interaction(snapshot, "commons-submit", { query: "Eiffel Tower" }) });
    snapshot = await settled(h, "nav", "Example Photographer");
    assert.equal(h.fixture.hits.get(eiffelPath), 1);

    // End-to-end RPC ExactJSON: explicit limit: 0 must be rejected as invalid.
    const zeroLimit = await h.call("connector.tools.call", { name: "commons_search", args: { query: "Eiffel Tower", limit: 0 } });
    assert.equal(zeroLimit.error?.code, "invalid");
  } finally { h.p.close(); }
});
