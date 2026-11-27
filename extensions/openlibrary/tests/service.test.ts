import { test } from "node:test";
import assert from "node:assert/strict";
import { budget, OpenLibrary, ReadError } from "../src/service.ts";
import { callTool } from "../src/tools.ts";
import { Fixture, sample, until } from "./fixtures.ts";
const make = (fixture = new Fixture(), options = {}) => new OpenLibrary(fixture.fetch, {}, { now: () => fixture.clock, monotonic: () => fixture.clock, limits: { gap: 0 }, locale: "en", ...options });

test("search and work details share bounded public requests and cached responses", async () => {
  const fixture = new Fixture(), service = make(fixture);
  const result = await service.search("synthetic");
  assert.equal(result.books[0].title, sample.title);
  const work = await service.work("OL59800W", result.books[0]);
  assert.equal(work.description, "A synthetic description of a novel.");
  await service.search("synthetic"); await service.work("OL59800W", result.books[0]);
  assert.equal(fixture.hits.get("/search.json?q=synthetic&limit=20&fields=key%2Ctitle%2Cauthor_name%2Cfirst_publish_year%2Cedition_count%2Cebook_access%2Cpublic_scan_b%2Cia"), 1);
  assert.equal(fixture.hits.get("/works/OL59800W.json"), 1);
  assert.equal(fixture.requests[0].init.redirect, "error");
  assert.equal(fixture.requests[0].init.headers && (fixture.requests[0].init.headers as Record<string, string>).Authorization, undefined);
  assert.equal(service.status().state, "ok"); assert.deepEqual(service.cacheStats().entries, 2);
});

test("failed refresh returns a stale validated answer instead of erasing it", async () => {
  const fixture = new Fixture(), service = make(fixture);
  await service.search("synthetic");
  fixture.put("/search.json?q=synthetic&limit=20&fields=key%2Ctitle%2Cauthor_name%2Cfirst_publish_year%2Cedition_count%2Cebook_access%2Cpublic_scan_b%2Cia", {}, { status: 503 });
  const result = await service.search("synthetic", 20, true);
  assert.equal(result.books[0].title, sample.title); assert.equal(result.stale?.code, "http");
  assert.equal(service.status().state, "error");
});

test("malformed, oversized and rate-limited responses fail without caching", async () => {
  const path = "/search.json?q=synthetic&limit=20&fields=key%2Ctitle%2Cauthor_name%2Cfirst_publish_year%2Cedition_count%2Cebook_access%2Cpublic_scan_b%2Cia";
  for (const response of [{ body: "{bad" }, { body: "null" }]) {
    const fixture = new Fixture(); fixture.answers.set(path, response);
    await assert.rejects(make(fixture).search("synthetic"), ReadError);
  }
  const fixture = new Fixture(); fixture.answers.set(path, { body: " ".repeat(2 << 20) });
  await assert.rejects(make(fixture).search("synthetic"), (error: unknown) => error instanceof ReadError && error.code === "bad_answer");
  const limited = new Fixture(); limited.put(path, {}, { status: 429 });
  await assert.rejects(make(limited).search("synthetic"), (error: unknown) => error instanceof ReadError && error.code === "rate_limited");
});

test("cancellation and deadline propagate through fetch and remove pending work", async () => {
  const fixture = new Fixture(); fixture.put("/works/OL59800W.json", { ...sample, title: sample.title }, { delay: 1000 });
  const service = make(fixture), controller = new AbortController();
  const pending = service.work("OL59800W", { key: "OL59800W", title: sample.title, authors: [], access: "no_ebook", url: "https://openlibrary.org/works/OL59800W" }, false, controller.signal);
  await until(async () => fixture.requests.length, length => length > 0);
  controller.abort();
  await assert.rejects(pending, (error: unknown) => error instanceof Error && error.name === "AbortError");
  assert.equal(fixture.cancelled, 1);
  fixture.put("/search.json?q=synthetic&limit=20&fields=key%2Ctitle%2Cauthor_name%2Cfirst_publish_year%2Cedition_count%2Cebook_access%2Cpublic_scan_b%2Cia", { numFound: 1, docs: [sample] }, { delay: 1000 });
  const timed = budget(undefined, 1);
  const slow = make(fixture);
  const timedRequest = slow.search("synthetic", 20, false, timed.signal);
  const rejected = assert.rejects(timedRequest, (error: unknown) => error instanceof ReadError && error.code === "slow");
  await until(async () => timed.signal.aborted, aborted => aborted);
  await rejected;
  timed.dispose();
});

test("schema-invalid responses never enter cache, openlibrary_work validates workKey before fetch", async () => {
  const path = "/search.json?q=synthetic&limit=20&fields=key%2Ctitle%2Cauthor_name%2Cfirst_publish_year%2Cedition_count%2Cebook_access%2Cpublic_scan_b%2Cia";
  const fixture = new Fixture();
  fixture.put(path, { numFound: 1, docs: [{ ...sample, key: "/works/BAD" }] });
  const service = make(fixture);
  await assert.rejects(service.search("synthetic"), (error: unknown) => error instanceof ReadError && error.code === "bad_answer");
  assert.equal(service.cacheStats().entries, 0);
  assert.equal(service.status().state, "error");
  fixture.put(path, {}, { status: 503 });
  await assert.rejects(service.search("synthetic"), (error: unknown) => error instanceof ReadError && error.code === "http");
  assert.equal(service.status().error, "The catalog returned HTTP 503.");

  // Invalid work IDs must fail locally with ReadError("invalid") without issuing any HTTP request.
  const noFetch = new Fixture();
  const svc = make(noFetch);
  await assert.rejects(callTool(svc, "openlibrary_work", { work: "bad" }), (error: unknown) => error instanceof ReadError && error.code === "invalid");
  assert.equal(noFetch.requests.length, 0);
});

test("cache enforces LRU without extending TTL, evicts oversized refresh, and handles timeouts and status codes", async () => {
  const fixture = new Fixture(), service = make(fixture, { limits: { gap: 0, cache: 2 } });
  const p = (q: string) => `/search.json?q=${q}&limit=20&fields=key%2Ctitle%2Cauthor_name%2Cfirst_publish_year%2Cedition_count%2Cebook_access%2Cpublic_scan_b%2Cia`;
  fixture.put(p("alpha"), { numFound: 1, docs: [sample] });
  fixture.put(p("beta"), { numFound: 1, docs: [sample] });
  fixture.put(p("gamma"), { numFound: 1, docs: [sample] });

  await service.search("alpha");
  await service.search("beta");
  fixture.clock += 30 * 60_000;
  await service.search("alpha");
  await service.search("gamma");
  await service.search("alpha");
  assert.equal(fixture.hits.get(p("alpha")), 1, "alpha was promoted by LRU");

  // At +61m from save, alpha expires (hit at +30m must not extend TTL).
  fixture.clock += 31 * 60_000;
  fixture.put(p("alpha"), {}, { status: 503 });
  const stale = await service.search("alpha");
  assert.equal(stale.stale?.code, "http");
  assert.equal(fixture.hits.get(p("alpha")), 2);
  // Stale fallback must not promote alpha back to fresh.
  const staleAgain = await service.search("alpha");
  assert.equal(staleAgain.stale?.code, "http");
  assert.equal(fixture.hits.get(p("alpha")), 3);

  // Oversized refresh deletes the old cached entry.
  const tiny = make(fixture, { limits: { gap: 0, cacheBytes: 250 } });
  const small = { numFound: 1, docs: [{ key: "/works/OL1W", title: "Small" }] };
  const big = { numFound: 1, docs: [{ key: "/works/OL1W", title: "Big " + "x".repeat(300) }] };
  fixture.put(p("alpha"), small);
  await tiny.search("alpha");
  assert.equal(tiny.cacheStats().entries, 1);
  fixture.put(p("alpha"), big);
  fixture.clock += 61 * 60_000;
  const refreshed = await tiny.search("alpha");
  assert.ok(refreshed.books[0].title.startsWith("Big "));
  assert.equal(tiny.cacheStats().entries, 0);

  // Request timeout falls back to stale cache and records slow error.
  const tFixture = new Fixture(), tSvc = make(tFixture, { limits: { gap: 0, request: 20 } });
  await tSvc.search("synthetic");
  tFixture.put(p("synthetic"), { numFound: 1, docs: [sample] }, { delay: 200 });
  tFixture.clock += 61 * 60_000;
  const fallback = await tSvc.search("synthetic");
  assert.equal(fallback.stale?.code, "slow");
  assert.equal(tSvc.status().state, "error");
  assert.equal(tSvc.status().error, "The catalog took too long to respond.");

  // Bodyless 404 does not pollute status; non-UTF-8 503 classifies as http 503.
  const healthy = new OpenLibrary(async () => new Response(null, { status: 404 }), {}, { limits: { gap: 0 }, locale: "en" });
  await assert.rejects(healthy.search("missing"), (error: unknown) => error instanceof ReadError && error.code === "not_found");
  assert.equal(healthy.status().state, "ok");

  const binary503 = new OpenLibrary(async () => new Response(new Uint8Array([0xff, 0xfe]), { status: 503 }), {}, { limits: { gap: 0 }, locale: "en" });
  await assert.rejects(binary503.search("broken"), (error: unknown) => error instanceof ReadError && error.code === "http" && error.status === 503);
  assert.equal(binary503.status().error, "The catalog returned HTTP 503.");

  // Tool timeout normalizes to ReadError("slow").
  const slowToolFixture = new Fixture();
  slowToolFixture.put("/search.json?q=synthetic&limit=10&fields=key%2Ctitle%2Cauthor_name%2Cfirst_publish_year%2Cedition_count%2Cebook_access%2Cpublic_scan_b%2Cia", { numFound: 1, docs: [sample] }, { delay: 200 });
  const slowToolSvc = make(slowToolFixture, { limits: { gap: 0, tool: 20, request: 500 } });
  await assert.rejects(callTool(slowToolSvc, "openlibrary_search", { query: "synthetic" }), (error: unknown) => error instanceof ReadError && error.code === "slow");
});

test("assistant tools retain book details and reject invalid or unknown calls", async () => {
  const service = make();
  assert.match(await callTool(service, "openlibrary_search", { query: "synthetic" }), /Left Hand of Darkness[\s\S]*Pride and Prejudice[\s\S]*Public edition/);
  assert.match(await callTool(service, "openlibrary_work", { work: "OL59800W" }), /synthetic description[\s\S]*Science fiction/);
  for (const args of [{ query: "" }, { query: "x".repeat(201) }, { query: "ok", limit: 0 }, { query: "ok", limit: 31 }, { query: "ok", limit: 1.5 }, { query: 2 }]) await assert.rejects(callTool(service, "openlibrary_search", args), ReadError);
  for (const work of ["bad", "https://attacker.invalid", "OL59800W/../../"]) await assert.rejects(callTool(service, "openlibrary_work", { work }), ReadError);
  await assert.rejects(callTool(service, "unknown"), /unknown method/);
});
