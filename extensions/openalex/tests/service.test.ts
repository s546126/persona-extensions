import { test } from "node:test";
import assert from "node:assert/strict";
import { budget, OpenAlex, ReadError } from "../src/service.ts";
import { callTool } from "../src/tools.ts";
import { Fixture, response, until } from "./fixtures.ts";
const make = (fixture = new Fixture(), options = {}) => new OpenAlex(fixture.fetch, {}, { now: () => fixture.clock, monotonic: () => fixture.clock, limits: { gap: 0 }, locale: "en", ...options });
const path = "/works?search=synthetic&per-page=20&select=id%2Cdisplay_name%2Cpublication_year%2Cdoi%2Cprimary_location%2Copen_access%2Ccited_by_count";

test("research search uses bounded public requests and a cached validated answer", async () => {
  const fixture = new Fixture(), service = make(fixture);
  assert.equal((await service.search("synthetic")).works[0].citations, 8095);
  await service.search("synthetic");
  assert.equal(fixture.hits.get(path), 1);
  assert.equal(fixture.requests[0].init.redirect, "error");
  assert.equal(fixture.requests[0].init.headers && (fixture.requests[0].init.headers as Record<string, string>).Authorization, undefined);
  assert.equal(service.status().state, "ok"); assert.deepEqual(service.cacheStats().entries, 1);
});

test("failed refresh returns a stale validated answer instead of erasing it", async () => {
  const fixture = new Fixture(), service = make(fixture);
  await service.search("synthetic");
  fixture.put(path, {}, { status: 503 });
  const result = await service.search("synthetic", 20, true);
  assert.equal(result.works[0].title, "A Survey of Large Language Models"); assert.equal(result.stale?.code, "http");
  assert.equal(service.status().state, "error");
});

test("malformed, oversized and rate-limited responses fail without caching", async () => {
  for (const answer of [{ body: "{bad" }, { body: "null" }]) {
    const fixture = new Fixture(); fixture.answers.set(path, answer);
    await assert.rejects(make(fixture).search("synthetic"), ReadError);
  }
  const fixture = new Fixture(); fixture.answers.set(path, { body: " ".repeat(2 << 20) });
  await assert.rejects(make(fixture).search("synthetic"), (error: unknown) => error instanceof ReadError && error.code === "bad_answer");
  const limited = new Fixture(); limited.put(path, {}, { status: 429 });
  await assert.rejects(make(limited).search("synthetic"), (error: unknown) => error instanceof ReadError && error.code === "rate_limited");
});

test("cancellation and deadline propagate through fetch and remove pending work", async () => {
  const fixture = new Fixture(); fixture.put(path, response, { delay: 1000 });
  const service = make(fixture), controller = new AbortController();
  const pending = service.search("synthetic", 20, false, controller.signal);
  await until(async () => fixture.requests.length, length => length > 0);
  controller.abort();
  await assert.rejects(pending, (error: unknown) => error instanceof Error && error.name === "AbortError");
  assert.equal(fixture.cancelled, 1);
  const timed = budget(undefined, 1);
  const slow = make(fixture);
  const request = slow.search("synthetic", 20, false, timed.signal);
  const rejected = assert.rejects(request, (error: unknown) => error instanceof ReadError && error.code === "slow");
  await until(async () => timed.signal.aborted, aborted => aborted);
  await rejected; timed.dispose();
});

test("schema-invalid responses are never cached and do not mark connector ok", async () => {
  const fixture = new Fixture();
  fixture.put(path, { meta: { count: 1 }, results: [{ ...response.results[0], id: "https://attacker.invalid/W1" }] });
  const service = make(fixture);
  await assert.rejects(service.search("synthetic"), (error: unknown) => error instanceof ReadError && error.code === "bad_answer");
  assert.equal(service.cacheStats().entries, 0);
  assert.equal(service.status().state, "error");
  fixture.put(path, {}, { status: 503 });
  await assert.rejects(service.search("synthetic"), (error: unknown) => error instanceof ReadError && error.code === "http");
  assert.equal(service.status().error, "OpenAlex returned HTTP 503.");
});

test("cache enforces LRU without extending TTL, evicts on oversized refresh, and handles timeouts and status codes", async () => {
  const fixture = new Fixture(), service = make(fixture, { limits: { gap: 0, cache: 2 } });
  const pathA = "/works?search=alpha&per-page=20&select=id%2Cdisplay_name%2Cpublication_year%2Cdoi%2Cprimary_location%2Copen_access%2Ccited_by_count";
  const pathB = "/works?search=beta&per-page=20&select=id%2Cdisplay_name%2Cpublication_year%2Cdoi%2Cprimary_location%2Copen_access%2Ccited_by_count";
  const pathC = "/works?search=gamma&per-page=20&select=id%2Cdisplay_name%2Cpublication_year%2Cdoi%2Cprimary_location%2Copen_access%2Ccited_by_count";
  fixture.put(pathA, response);
  fixture.put(pathB, response);
  fixture.put(pathC, response);

  await service.search("alpha");
  await service.search("beta");
  // Touch alpha at +30m -> alpha becomes MRU, so inserting gamma evicts beta (not alpha).
  fixture.clock += 30 * 60_000;
  await service.search("alpha");
  await service.search("gamma");
  assert.equal(fixture.hits.get(pathA), 1);
  await service.search("alpha");
  assert.equal(fixture.hits.get(pathA), 1, "alpha was promoted by LRU");

  // At +61m total from alpha's save time, alpha must expire (hit at +30m must NOT extend TTL).
  fixture.clock += 31 * 60_000;
  fixture.put(pathA, {}, { status: 503 });
  const stale = await service.search("alpha");
  assert.equal(stale.stale?.code, "http");
  assert.equal(fixture.hits.get(pathA), 2);
  // Stale fallback must NOT promote alpha back to fresh.
  const staleAgain = await service.search("alpha");
  assert.equal(staleAgain.stale?.code, "http");
  assert.equal(fixture.hits.get(pathA), 3);

  // Oversized refresh deletes the old cached entry.
  const tiny = make(fixture, { limits: { gap: 0, cacheBytes: 300 } });
  const small = { meta: { count: 1 }, results: [{ id: "https://openalex.org/W1", display_name: "Tiny", cited_by_count: 1, open_access: { is_oa: false } }] };
  const big = { meta: { count: 1 }, results: [{ id: "https://openalex.org/W1", display_name: "Big " + "x".repeat(350), cited_by_count: 2, open_access: { is_oa: false } }] };
  fixture.put(pathA, small);
  await tiny.search("alpha");
  assert.equal(tiny.cacheStats().entries, 1);
  fixture.put(pathA, big);
  fixture.clock += 61 * 60_000;
  const refreshed = await tiny.search("alpha");
  assert.equal(refreshed.works[0].citations, 2);
  assert.equal(tiny.cacheStats().entries, 0);

  // Timeout falls back to stale cache and records slow error.
  const timeoutFixture = new Fixture(), timeoutSvc = make(timeoutFixture, { limits: { gap: 0, request: 20 } });
  await timeoutSvc.search("synthetic");
  timeoutFixture.put(path, response, { delay: 200 });
  timeoutFixture.clock += 61 * 60_000;
  const timedOut = await timeoutSvc.search("synthetic");
  assert.equal(timedOut.stale?.code, "slow");
  assert.equal(timeoutSvc.status().state, "error");
  assert.equal(timeoutSvc.status().error, "OpenAlex took too long to respond.");

  // Bodyless 404 does not pollute status, non-UTF-8 503 classifies as http 503.
  const healthy = new OpenAlex(async () => new Response(null, { status: 404 }), {}, { limits: { gap: 0 }, locale: "en" });
  await assert.rejects(healthy.search("missing"), (error: unknown) => error instanceof ReadError && error.code === "not_found");
  assert.equal(healthy.status().state, "ok");

  const binary503 = new OpenAlex(async () => new Response(new Uint8Array([0xff, 0xfe]), { status: 503 }), {}, { limits: { gap: 0 }, locale: "en" });
  await assert.rejects(binary503.search("broken"), (error: unknown) => error instanceof ReadError && error.code === "http" && error.status === 503);
  assert.equal(binary503.status().error, "OpenAlex returned HTTP 503.");

  // Tool timeout normalizes to ReadError("slow").
  const slowToolFixture = new Fixture();
  slowToolFixture.put("/works?search=synthetic&per-page=10&select=id%2Cdisplay_name%2Cpublication_year%2Cdoi%2Cprimary_location%2Copen_access%2Ccited_by_count", response, { delay: 200 });
  const slowToolSvc = make(slowToolFixture, { limits: { gap: 0, tool: 20, request: 500 } });
  await assert.rejects(callTool(slowToolSvc, "openalex_search", { query: "synthetic" }), (error: unknown) => error instanceof ReadError && error.code === "slow");
});

test("assistant tools retain research metadata and reject invalid or unknown calls", async () => {
  const service = make();
  assert.match(await callTool(service, "openalex_search", { query: "synthetic" }), /Large Language Models[\s\S]*8095 citations[\s\S]*Open access/);
  for (const args of [{ query: "" }, { query: "x".repeat(201) }, { query: "ok", limit: 0 }, { query: "ok", limit: 21 }, { query: "ok", limit: 1.5 }, { query: 2 }]) await assert.rejects(callTool(service, "openalex_search", args), ReadError);
  await assert.rejects(callTool(service, "unknown"), /unknown method/);
});
