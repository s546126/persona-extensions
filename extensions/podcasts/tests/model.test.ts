import { test } from "node:test";
import assert from "node:assert/strict";
import { clip, canonicalEpisodeURL, lookupPath, parseEpisodes, parseSearch, parseShow, podcastID, publicURL, queryText, ReadError, searchPath } from "../src/model.ts";
import { readTime } from "../src/i18n.ts";
import { decodeList, encodeList, MemoryStore } from "../src/store.ts";
import { sampleEpisode, sampleShow } from "./fixtures.ts";
import { encodeState, initialState, page, readState, transition } from "../src/page.tsx";
import { Podcasts } from "../src/service.ts";
import { Renderer } from "@persona/extension/ui";

test("search and lookup paths validate identifiers and bounds", () => {
  assert.equal(podcastID(1860072717), 1860072717);
  assert.equal(podcastID("1860072717"), 1860072717);
  for (const id of [0, -1, 1.2, "https://attacker.invalid", "1&limit=200", 2_000_000_000_001, undefined]) assert.throws(() => podcastID(id), ReadError);
  for (const query of ["", " ", "x".repeat(201), "bad\n"]) assert.throws(() => searchPath(query), ReadError);
  assert.equal(searchPath("A show", 2), "/search?term=A+show&media=podcast&entity=podcast&limit=2");
  assert.throws(() => searchPath("valid", 31), ReadError);
  assert.equal(lookupPath(42, 3), "/lookup?id=42&entity=podcastEpisode&limit=3&sort=recent");
});

test("only canonical Apple links and HTTPS publisher feeds survive parsing", () => {
  const show = parseShow(sampleShow);
  assert.equal(show.url, "https://podcasts.apple.com/us/podcast/id1860072717");
  assert.equal(show.feedURL, "https://feeds.example.com/show.xml");
  assert.equal(canonicalEpisodeURL(show.id, sampleEpisode.trackId), "https://podcasts.apple.com/us/podcast/id1860072717?i=1000792066218");
  assert.throws(() => parseShow({ ...sampleShow, collectionViewUrl: "https://attacker.invalid/phishing" }), ReadError);
  assert.throws(() => parseShow({ ...sampleShow, feedUrl: "http://feeds.example.com/show.xml" }), ReadError);
  assert.throws(() => parseShow({ ...sampleShow, feedUrl: "https://user:password@feeds.example.com/show.xml" }), ReadError);
  assert.equal(publicURL(undefined, "feed"), undefined);
});

test("parent and episode records are strictly separated and bounded", () => {
  const search = parseSearch({ resultCount: 2, results: [sampleShow, sampleShow] }, 1000);
  assert.equal(search.shows.length, 1);
  const detail = parseEpisodes({ resultCount: 2, results: [sampleShow, sampleEpisode] }, sampleShow.collectionId, 1000);
  assert.equal(detail.episodes.length, 1);
  assert.equal(detail.episodes[0].minutes, 59);
  assert.equal(detail.episodes[0].description, "A synthetic description of an episode.");
  assert.ok(!("episodeUrl" in detail.episodes[0]));
  assert.throws(() => parseEpisodes({ resultCount: 2, results: [sampleShow, { ...sampleEpisode, kind: "podcastEpisode" }] }, sampleShow.collectionId, 0), ReadError);
  assert.throws(() => parseEpisodes({ resultCount: 1, results: [sampleEpisode] }, 42, 0), ReadError);
  assert.throws(() => parseEpisodes({ resultCount: 2, results: [sampleShow, { ...sampleEpisode, collectionId: 42 }] }, sampleShow.collectionId, 0), ReadError);
  // Date.parse would roll this into October; an episode date must be a real calendar day.
  assert.throws(() => parseEpisodes({ resultCount: 2, results: [sampleShow, { ...sampleEpisode, releaseDate: "2026-02-30T00:00:00Z" }] }, sampleShow.collectionId, 0), ReadError);
  for (const raw of [null, [], {}, { resultCount: -1, results: [] }, { resultCount: 1 }, { resultCount: 1, results: [null] }]) assert.throws(() => parseSearch(raw, 0), ReadError);
});

test("native episode descriptions respect the SDK's 2000-character text bound", () => {
  const show = parseShow(sampleShow), result = parseEpisodes({ resultCount: 2, results: [sampleShow, { ...sampleEpisode, description: "long ".repeat(1600) }] }, show.id, 1000);
  const state = { v: "show" as const, q: "engineering", k: show.id, b: show };
  const renderer = new Renderer({ authorize: () => false });
  try {
    const snapshot = renderer.mount(page({ service: new Podcasts(async () => new Response()), store: new MemoryStore(), state, loaded: { kind: "show", result } }));
    for (const section of snapshot.view.sections) for (const item of section.items) {
      if (item.type === "text") assert.ok(Array.from(item.text).length <= 2000);
      if (item.type === "row") assert.ok(Array.from(item.subtitle ?? "").length <= 2000);
    }
  } finally { renderer.dispose(); }
});
test("listening-list persistence validates untrusted data and does not manufacture writes", async () => {
  const show = parseShow(sampleShow), store = new MemoryStore();
  await store.save(show);
  assert.equal(store.has(show.id), true);
  const list = decodeList(encodeList(await store.load()));
  assert.deepEqual(list.shows, [show]);
  await store.save(otherShow());
  assert.equal((await store.load()).shows.length, 2);
  await store.remove(show.id);
  assert.equal(store.has(show.id), false);
  assert.throws(() => decodeList("not-base64"), ReadError);
  assert.throws(() => decodeList(encodeList({ schema_version: 1, shows: [] }) + "x"), ReadError);
});
function otherShow() { return parseShow({ ...sampleShow, collectionId: 42, collectionName: "Another Show", feedUrl: undefined }); }

test("navigation retains only validated show metadata", () => {
  const show = parseShow(sampleShow);
  const searched = transition(readState(undefined), "search", '{"query":"engineering"}');
  const opened = transition({ ...searched, b: show }, `show:${show.id}`);
  assert.equal(opened.v, "show");
  const roundTrip = readState(encodeState(opened));
  assert.equal(roundTrip.b?.url, show.url);
  assert.equal(transition(roundTrip, "back").v, "search");
  const fromLibrary = transition(readState(undefined), "library");
  const libraryShow = { ...fromLibrary, b: show, v: "show" as const, k: show.id, from: "library" as const };
  assert.equal(transition(libraryShow, "back").v, "library");
  assert.equal(transition(readState(encodeState(libraryShow)), "back").v, "library");
  assert.equal(transition(roundTrip, "library").v, "library");
  for (const values of ["show", "not-json", "null", "[]", 1, { query: ["bad"] }]) assert.equal(transition(readState(undefined), "search", values).v, "discover");
  assert.equal(readState(JSON.stringify({ ...opened, b: { ...show, url: "https://attacker.invalid" } })).v, "discover");
  assert.equal(readState(JSON.stringify({ ...opened, b: { ...show, feedURL: "http://attacker.invalid" } })).v, "discover");
});

test("clip, readTime, query normalization and show state consistency handle edge cases safely", () => {
  assert.equal(clip("abcdef", 4), "abc…");
  assert.equal(Array.from(clip("abcdef", 4)).length, 4);
  assert.equal(clip("😀😀😀😀", 3), "😀😀…");
  assert.equal(clip("abcdef", 0), "");
  assert.equal(clip("abcdef", -5), "");
  assert.equal(clip("abcdef", Number.NaN), "");
  assert.throws(() => queryText("bad\ud800query"), ReadError);
  assert.equal(readTime(Number.NaN, "UTC", "en"), "");
  assert.equal(readTime("not-a-date", "UTC", "en"), "");
  assert.ok(readTime(Date.UTC(2026, 10, 12), "Invalid/Timezone", "en").length > 0);
  assert.ok(readTime(Date.UTC(2026, 10, 12), "UTC", "not_a_valid_locale!!!").length > 0);

  const longSearch = transition(initialState, "search", { query: "a".repeat(250) });
  assert.equal(longSearch.v, "search");
  assert.equal(queryText(longSearch.q), longSearch.q);
  const multiline = transition(initialState, "search", { query: "Systems\nPodcast" });
  assert.equal(multiline.v, "search");
  assert.equal(queryText(multiline.q), "Systems Podcast");
  assert.equal(transition(initialState, "search", { query: "bad\u0000query" }).v, "discover");
  assert.deepEqual(readState(JSON.stringify({ v: "search", q: "bad\nquery", k: 0 })), initialState);
  assert.deepEqual(readState(JSON.stringify({ v: "search", q: "a".repeat(250), k: 0 })), initialState);

  // Mismatched show state (k !== b.id) must reset to initialState.
  const show = parseShow(sampleShow);
  assert.deepEqual(readState(JSON.stringify({ v: "show", q: "eng", k: 42, b: show })), initialState);
});

test("page omits empty episode-notes section and enforces CheckView item, key, state and frame budgets", () => {
  const service = new Podcasts(async () => new Response(), {}, { locale: "en", limits: { gap: 0 } });
  const store = new MemoryStore();
  const render = (node: ReturnType<typeof page>) => {
    const renderer = new Renderer({ authorize: () => false });
    try { return renderer.mount(node).view; }
    finally { renderer.dispose(); }
  };

  // Zero episodes must NOT render an empty Section (items: [] violates wire.CheckView).
  const show = parseShow(sampleShow);
  const emptyShowView = render(page({
    service,
    store,
    state: { v: "show", q: "eng", k: show.id, b: show },
    loaded: { kind: "show", result: { show, episodes: [], at: Date.UTC(2026, 10, 12) } },
    uiVersion: 5,
  }));
  for (const section of emptyShowView.sections) {
    assert.ok(section.items.length > 0, `empty section emitted: ${section.key}`);
  }

  // Duplicate shows and many shows passed directly to page() must respect keys, ItemsMost = 300, and 512 KiB.
  const dupView = render(page({
    service,
    store,
    state: { v: "search", q: "eng", k: 0 },
    loaded: { kind: "search", result: { shows: [show, { ...show }], at: 0 } },
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

  const manyShows = Array.from({ length: 320 }, (_, i) => ({
    ...show,
    id: i + 1,
    title: `Podcast ${i} ` + "x".repeat(300),
    url: `https://podcasts.apple.com/us/podcast/id${i + 1}`,
  }));
  const largeView = render(page({
    service,
    store,
    state: { v: "search", q: "eng", k: 0 },
    loaded: { kind: "search", result: { shows: manyShows, at: 0 } },
    uiVersion: 5,
  }));
  const totalItems = largeView.sections.reduce((sum, s) => sum + s.items.length, 0);
  assert.ok(totalItems <= 300, `items exceeded 300: ${totalItems}`);
  assert.ok(new TextEncoder().encode(JSON.stringify(largeView)).byteLength <= 512 * 1024);

  // Invalid state passed directly to page() must be normalized.
  const badStateView = render(page({ service, store, state: { v: "search", q: "bad\nquery", k: 0 }, loaded: { kind: "empty" }, uiVersion: 5 }));
  assert.equal(badStateView.state, encodeState(initialState));
});
