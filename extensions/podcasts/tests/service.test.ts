import { test } from "node:test";
import assert from "node:assert/strict";
import { budget, Podcasts, ReadError } from "../src/service.ts";
import { callTool } from "../src/tools.ts";
import { Fixture, lookupToolURL, lookupURL, sampleShow, searchToolURL, searchURL, showURL, until } from "./fixtures.ts";
const make = (fixture = new Fixture(), options = {}) => new Podcasts(fixture.fetch, {}, { now: () => fixture.clock, monotonic: () => fixture.clock, limits: { gap: 0 }, locale: "en", ...options });

test("search and episode reads share bounded requests and validated cache entries", async () => {
  const fixture = new Fixture(), service = make(fixture);
  const result = await service.search("synthetic");
  assert.equal(result.shows[0].title, "Example Engineering");
  const detail = await service.episodes(result.shows[0].id);
  assert.equal(detail.episodes[0].title, "A useful conversation");
  await service.search("synthetic"); await service.episodes(result.shows[0].id);
  assert.equal(fixture.hits.get(searchURL), 1);
  assert.equal(fixture.hits.get(lookupURL), 1);
  assert.equal(fixture.requests[0].init.redirect, "error");
  assert.equal(fixture.requests[0].init.headers && (fixture.requests[0].init.headers as Record<string, string>).Authorization, undefined);
  assert.equal(service.status().state, "ok"); assert.deepEqual(service.cacheStats().entries, 2);
});

test("a frequently used cached answer still expires after its freshness window", async () => {
  const fixture = new Fixture(), service = make(fixture);
  await service.search("synthetic");
  fixture.put(searchURL, { resultCount: 1, results: [{ ...sampleShow, collectionName: "Updated Engineering" }] });
  fixture.clock += 10 * 60 * 1000;
  await service.search("synthetic"); // a hit at 10 minutes must not reset the 15-minute window
  fixture.clock += 10 * 60 * 1000;
  const result = await service.search("synthetic");
  assert.equal(result.shows[0].title, "Updated Engineering");
  assert.equal(fixture.hits.get(searchURL), 2);
});

test("show refuses a record whose collection id is not the requested show", async () => {
  const fixture = new Fixture(), service = make(fixture);
  const result = await service.search("synthetic");
  const id = result.shows[0].id;
  fixture.put(showURL, { resultCount: 1, results: [{ ...sampleShow, collectionId: 42 }] });
  await assert.rejects(service.show(id), (error: unknown) => error instanceof ReadError && error.code === "not_found");
});

test("failed refresh returns a stale validated answer", async () => {
  const fixture = new Fixture(), service = make(fixture);
  await service.search("synthetic");
  fixture.put(searchURL, {}, { status: 503 });
  const result = await service.search("synthetic", 20, true);
  assert.equal(result.shows[0].title, "Example Engineering"); assert.equal(result.stale?.code, "http");
  assert.equal(service.status().state, "error");
});

test("malformed, oversized and rate-limited responses are not cached", async () => {
  for (const response of [{ body: "{bad" }, { body: "null" }]) {
    const fixture = new Fixture(); fixture.answers.set(searchURL, response);
    await assert.rejects(make(fixture).search("synthetic"), ReadError);
  }
  const fixture = new Fixture(); fixture.answers.set(searchURL, { body: " ".repeat(2 << 20) });
  await assert.rejects(make(fixture).search("synthetic"), (error: unknown) => error instanceof ReadError && error.code === "bad_answer");
  const limited = new Fixture(); limited.put(searchURL, {}, { status: 429 });
  await assert.rejects(make(limited).search("synthetic"), (error: unknown) => error instanceof ReadError && error.code === "rate_limited");
});

test("cancellation and deadlines propagate to the response", async () => {
  const fixture = new Fixture(); fixture.put(lookupURL, { resultCount: 2, results: [] }, { delay: 1000 });
  fixture.put(searchURL, { resultCount: 2, results: [] }, { delay: 1000 });
  const service = make(fixture), controller = new AbortController();
  const pending = service.episodes(1860072717, 20, false, controller.signal);
  await until(async () => fixture.requests.length, length => length > 0);
  controller.abort();
  await assert.rejects(pending, (error: unknown) => error instanceof Error && error.name === "AbortError");
  assert.equal(fixture.cancelled, 1);
  const timed = budget(undefined, 1);
  const slow = make(fixture);
  const timedRequest = slow.search("synthetic", 20, false, timed.signal);
  const rejected = assert.rejects(timedRequest, (error: unknown) => error instanceof ReadError && error.code === "slow");
  await until(async () => timed.signal.aborted, aborted => aborted);
  await rejected;
  timed.dispose();
});

test("cache expiration and refresh use monotonic clock separately from epoch now()", async () => {
  const fixture = new Fixture();
  let elapsed = 0;
  const epochBase = Date.UTC(2026, 10, 12);
  const monoBase = 500; // realistic performance.now() uptime << epoch ms!
  const service = new Podcasts(fixture.fetch, {}, {
    now: () => epochBase + elapsed,
    monotonic: () => monoBase + elapsed,
    limits: { gap: 0 },
    locale: "en",
  });

  const first = await service.search("synthetic");
  assert.equal(first.shows[0].title, "Example Engineering");
  assert.equal(fixture.hits.get(searchURL), 1);

  // Manual refresh (fresh = 0) MUST refetch even when monotonic() << now().
  fixture.put(searchURL, { resultCount: 1, results: [{ ...sampleShow, collectionName: "Refreshed Engineering" }] });
  const refreshed = await service.search("synthetic", 20, true);
  assert.equal(refreshed.shows[0].title, "Refreshed Engineering");
  assert.equal(fixture.hits.get(searchURL), 2);

  // After 16 minutes (> 15m TTL), cache MUST expire and refetch on normal read.
  elapsed += 16 * 60_000;
  fixture.put(searchURL, { resultCount: 1, results: [{ ...sampleShow, collectionName: "Expired Engineering" }] });
  const expired = await service.search("synthetic");
  assert.equal(expired.shows[0].title, "Expired Engineering");
  assert.equal(fixture.hits.get(searchURL), 3);
});

test("oversized refresh evicts old cache, timeouts fall back to stale, HTTP status classifies first, and show() handles non-array results", async () => {
  const fixture = new Fixture();
  // Oversized refresh deletes the old cached entry instead of leaving stale data behind.
  const tiny = make(fixture, { limits: { gap: 0, cacheBytes: 350 } });
  const small = { resultCount: 1, results: [{ wrapperType: "track", kind: "podcast", collectionId: 1, collectionName: "Tiny" }] };
  const big = { resultCount: 1, results: [{ wrapperType: "track", kind: "podcast", collectionId: 1, collectionName: "Big " + "x".repeat(400) }] };
  fixture.put(searchURL, small);
  await tiny.search("synthetic");
  assert.equal(tiny.cacheStats().entries, 1);
  fixture.put(searchURL, big);
  fixture.clock += 16 * 60_000;
  const got = await tiny.search("synthetic");
  assert.ok(got.shows[0].title.startsWith("Big "));
  assert.equal(tiny.cacheStats().entries, 0);

  // Request timeout falls back to stale cache and records slow error.
  const tFixture = new Fixture(), tSvc = make(tFixture, { limits: { gap: 0, request: 20 } });
  await tSvc.search("synthetic");
  tFixture.put(searchURL, { resultCount: 1, results: [sampleShow] }, { delay: 200 });
  tFixture.clock += 16 * 60_000;
  const fallback = await tSvc.search("synthetic");
  assert.equal(fallback.stale?.code, "slow");
  assert.equal(tSvc.status().state, "error");
  assert.equal(tSvc.status().error, "The directory took too long to answer.");

  // Bodyless 404 does not pollute status; non-UTF-8 503 classifies as http 503.
  const healthy = new Podcasts(async () => new Response(null, { status: 404 }), {}, { limits: { gap: 0 }, locale: "en" });
  await assert.rejects(healthy.search("missing"), (error: unknown) => error instanceof ReadError && error.code === "not_found");
  assert.equal(healthy.status().state, "ok");

  const binary503 = new Podcasts(async () => new Response(new Uint8Array([0xff, 0xfe]), { status: 503 }), {}, { limits: { gap: 0 }, locale: "en" });
  await assert.rejects(binary503.search("broken"), (error: unknown) => error instanceof ReadError && error.code === "http" && error.status === 503);
  assert.equal(binary503.status().error, "The directory returned HTTP 503.");

  // show() with non-array results must throw bad_answer, not network TypeError.
  const badShowFixture = new Fixture();
  badShowFixture.put(showURL, { resultCount: 1, results: "not-an-array" });
  await assert.rejects(make(badShowFixture).show(1860072717), (error: unknown) => error instanceof ReadError && error.code === "bad_answer");

  // Tool timeout normalizes to ReadError("slow").
  const slowToolFixture = new Fixture();
  slowToolFixture.put(searchToolURL, { resultCount: 1, results: [sampleShow] }, { delay: 200 });
  const slowToolSvc = make(slowToolFixture, { limits: { gap: 0, tool: 20, request: 500 } });
  await assert.rejects(callTool(slowToolSvc, "podcasts_search", { query: "synthetic" }), (error: unknown) => error instanceof ReadError && error.code === "slow");
});

test("assistant tools retain show and episode details and validate numeric IDs", async () => {
  const service = make();
  assert.match(await callTool(service, "podcasts_search", { query: "synthetic" }), /Example Engineering[\s\S]*Publisher RSS[\s\S]*Another Show/);
  assert.match(await callTool(service, "podcasts_episodes", { show: 1860072717 }), /useful conversation[\s\S]*A synthetic description/);
  for (const args of [{ query: "" }, { query: "x".repeat(201) }, { query: "ok", limit: 0 }, { query: "ok", limit: 31 }, { query: "ok", limit: 1.5 }, { query: 2 }]) await assert.rejects(callTool(service, "podcasts_search", args), ReadError);
  for (const show of [0, -2, 1.2, "https://attacker.invalid", "1&limit=200"]) await assert.rejects(callTool(service, "podcasts_episodes", { show }), ReadError);
  await assert.rejects(callTool(service, "unknown"), /unknown method/);
});
