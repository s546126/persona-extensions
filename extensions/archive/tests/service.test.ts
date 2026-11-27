import { test } from "node:test";
import assert from "node:assert/strict";
import { ExactJSON } from "@persona/extension";
import { Archive, budget, ReadError } from "../src/service.ts";
import { callTool } from "../src/tools.ts";
import { Fixture, response, until } from "./fixtures.ts";
const make = (fixture = new Fixture(), options = {}) => new Archive(fixture.fetch, {}, { now: () => fixture.clock, monotonic: () => fixture.clock, limits: { gap: 0 }, locale: "en", ...options });
const path = "/advancedsearch.php?q=%28synthetic%29&fl=identifier%2Ctitle%2Ccreator%2Cyear%2Cmediatype%2Cdownloads%2Cdescription&rows=20&page=1&output=json";

test("catalog search uses bounded public requests and a cached validated answer", async () => {
  const fixture = new Fixture(), service = make(fixture);
  assert.equal((await service.search("synthetic")).items[0].title, "Pride and Prejudice");
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
  assert.equal(result.items[0].title, "Pride and Prejudice"); assert.equal(result.stale?.code, "http");
  assert.equal(service.status().state, "error");
  assert.equal(service.status().error, "The archive returned HTTP 503.");

  // A malformed HTTP 200 payload during refresh must not overwrite the cached valid entry.
  fixture.answers.set(path, { body: "null", status: 200 });
  const staleBad = await service.search("synthetic", 20, true);
  assert.equal(staleBad.items[0].title, "Pride and Prejudice");
  assert.equal(staleBad.stale?.code, "bad_answer");
});

test("malformed, oversized and rate-limited responses fail without caching", async () => {
  for (const answer of [{ body: "{bad" }, { body: "null" }, { body: "{}" }, { body: JSON.stringify({ response: { numFound: 1, docs: "bad" } }) }]) {
    const fixture = new Fixture(); fixture.answers.set(path, answer);
    const service = make(fixture);
    await assert.rejects(service.search("synthetic"), ReadError);
    assert.equal(service.cacheStats().entries, 0);
  }
  const fixture = new Fixture(); fixture.answers.set(path, { body: " ".repeat((2 << 20) + 1) });
  await assert.rejects(make(fixture).search("synthetic"), (error: unknown) => error instanceof ReadError && error.code === "bad_answer");
  // Oversized stream whose cancel() rejects must still classify as bad_answer, not network.
  const failingCancelFetch = (async () => new Response(new ReadableStream<Uint8Array>({
    pull(controller) { controller.enqueue(new Uint8Array(64)); },
    cancel() { return Promise.reject(new Error("cancel failed")); },
  }), { status: 200 })) as unknown as Fixture["fetch"];
  await assert.rejects(new Archive(failingCancelFetch, {}, { limits: { gap: 0, response: 32 }, locale: "en" }).search("synthetic"), (error: unknown) => error instanceof ReadError && error.code === "bad_answer");
  const limited = new Fixture(); limited.put(path, {}, { status: 429 });
  await assert.rejects(make(limited).search("synthetic"), (error: unknown) => error instanceof ReadError && error.code === "rate_limited");

  // Bodyless 404 classifies as not_found without poisoning connector status.
  const bodyless404 = new Archive((async () => new Response(null, { status: 404 })) as unknown as Fixture["fetch"], {}, { limits: { gap: 0 }, locale: "en" });
  await assert.rejects(bodyless404.search("synthetic"), (error: unknown) => error instanceof ReadError && error.code === "not_found");
  assert.equal(bodyless404.status().state, "ok");
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
  assert.equal(slow.status().state, "error");

  // Internal request timeout classifies as ReadError("slow") and falls back to stale cache.
  const staleFixture = new Fixture();
  const staleService = make(staleFixture, { limits: { gap: 0, request: 25 } });
  await staleService.search("synthetic");
  staleFixture.put(path, response, { delay: 500 });
  const staleSlow = await staleService.search("synthetic", 20, true);
  assert.equal(staleSlow.stale?.code, "slow");
  assert.equal(staleService.status().state, "error");
});

test("stream body abort on timeout classifies as ReadError('slow') and preserves caller abort", async () => {
  const streamFetch = (async (_input: string, init: RequestInit) => {
    const signal = init.signal!;
    const body = new ReadableStream<Uint8Array>({
      start(controller) {
        signal.addEventListener("abort", () => {
          controller.error(new DOMException("aborted", "AbortError"));
        }, { once: true });
      },
    });
    return new Response(body, { status: 200 });
  }) as unknown as Fixture["fetch"];
  const slowService = new Archive(streamFetch, {}, { limits: { gap: 0, request: 20 }, locale: "en" });
  await assert.rejects(slowService.search("synthetic"), (error: unknown) => error instanceof ReadError && error.code === "slow");

  const cancelService = new Archive(streamFetch, {}, { limits: { gap: 0, request: 5000 }, locale: "en" });
  const controller = new AbortController();
  const pending = cancelService.search("synthetic", 20, false, controller.signal);
  setTimeout(() => controller.abort(), 15);
  await assert.rejects(pending, (error: unknown) => error instanceof Error && error.name === "AbortError");
});

test("refreshing oldest cache entry updates LRU order and queued cancellation does not return cached entry", async () => {
  const fixture = new Fixture();
  const pathA = "/advancedsearch.php?q=%28alpha%29&fl=identifier%2Ctitle%2Ccreator%2Cyear%2Cmediatype%2Cdownloads%2Cdescription&rows=20&page=1&output=json";
  const pathB = "/advancedsearch.php?q=%28beta%29&fl=identifier%2Ctitle%2Ccreator%2Cyear%2Cmediatype%2Cdownloads%2Cdescription&rows=20&page=1&output=json";
  const smallA = { response: { numFound: 1, docs: [{ identifier: "alpha_1", title: "a".repeat(20), mediatype: "texts", downloads: 1 }] } };
  const smallB = { response: { numFound: 1, docs: [{ identifier: "beta_1", title: "b".repeat(20), mediatype: "texts", downloads: 1 }] } };
  const largerA = { response: { numFound: 1, docs: [{ identifier: "alpha_1", title: "a".repeat(90), mediatype: "texts", downloads: 1 }] } };
  fixture.put(pathA, smallA);
  fixture.put(pathB, smallB);
  const bytesA = new TextEncoder().encode(JSON.stringify(smallA)).byteLength;
  const bytesB = new TextEncoder().encode(JSON.stringify(smallB)).byteLength;
  const bytesLargerA = new TextEncoder().encode(JSON.stringify(largerA)).byteLength;
  const service = make(fixture, { limits: { gap: 0, cacheBytes: Math.max(bytesA + bytesB, bytesLargerA + 10) } });
  await service.search("alpha");
  await service.search("beta");
  fixture.put(pathA, largerA);
  await service.search("alpha", 20, true);
  // Refreshing alpha must evict beta, NOT freshly updated alpha.
  await service.search("alpha");
  assert.equal(fixture.hits.get(pathA), 2);

  // Refreshing with a valid payload exceeding cacheBytes evicts the outdated cached entry.
  const hugeA = { response: { numFound: 1, docs: [{ identifier: "alpha_1", title: "z".repeat(400), mediatype: "texts", downloads: 1 }] } };
  fixture.put(pathA, hugeA);
  const refreshedHuge = await service.search("alpha", 20, true);
  assert.equal(refreshedHuge.items[0].title, "z".repeat(400));
  assert.equal(service.cacheStats().entries, 0);

  // Queued caller aborted while waiting behind an earlier slow request must reject with AbortError.
  const qFixture = new Fixture();
  qFixture.put(path, response, { delay: 40 });
  const qService = make(qFixture);
  const first = qService.search("synthetic");
  const controller = new AbortController();
  const second = qService.search("synthetic", 20, false, controller.signal);
  controller.abort();
  await assert.rejects(second, (error: unknown) => error instanceof Error && error.name === "AbortError");
  await first;
});

test("assistant tools retain metadata, validate ExactJSON limits, normalize timeouts, and reject invalid or unknown calls", async () => {
  const service = make();
  assert.match(await callTool(service, "archive_search", { query: "synthetic" }), /Pride and Prejudice[\s\S]*Jane Austen[\s\S]*archive.org\/details/);
  assert.match(await callTool(service, "archive_search", { query: "synthetic" }, undefined, ExactJSON.parse('{"query":"synthetic"}')), /Pride and Prejudice/);
  assert.match(await callTool(service, "archive_search", { query: "synthetic", limit: null }, undefined, ExactJSON.parse('{"query":"synthetic","limit":null}')), /Pride and Prejudice/);
  for (const raw of ['{"query":"synthetic","limit":0}', '{"query":"synthetic","limit":21}', '{"query":"synthetic","limit":1.5}', '{"query":"synthetic","limit":1e1}', '{"query":"synthetic","limit":"5"}', '{"query":"synthetic","limit":9223372036854775808}', '{"query":"synthetic","limit":9223372036854775807}', '{"query":null}', '[]']) {
    await assert.rejects(callTool(service, "archive_search", JSON.parse(raw), undefined, ExactJSON.parse(raw)), (error: unknown) => error instanceof ReadError && error.code === "invalid");
  }
  for (const args of [{ query: "" }, { query: "x".repeat(161) }, { query: "\uD800" }, { query: "ok", limit: 0 }, { query: "ok", limit: 21 }, { query: "ok", limit: 1.5 }, { query: 2 }]) {
    await assert.rejects(callTool(service, "archive_search", args), ReadError);
  }
  await assert.rejects(callTool(service, "unknown"), /unknown method/);
  await assert.rejects(callTool(service, "unknown", [], undefined, ExactJSON.parse("[]")), /unknown method/);

  const slowFixture = new Fixture();
  slowFixture.put("/advancedsearch.php?q=%28synthetic%29&fl=identifier%2Ctitle%2Ccreator%2Cyear%2Cmediatype%2Cdownloads%2Cdescription&rows=10&page=1&output=json", response, { delay: 500 });
  const slowService = make(slowFixture, { limits: { gap: 0, tool: 20, request: 5000 } });
  await assert.rejects(callTool(slowService, "archive_search", { query: "synthetic" }), (error: unknown) => error instanceof ReadError && error.code === "slow");
});

test("media configuration only changes the approved API filter", async () => {
  const fixture = new Fixture(), service = new Archive(fixture.fetch, { media: "texts" }, { locale: "en", limits: { gap: 0 }, now: () => fixture.clock });
  await service.search("synthetic");
  assert.equal(fixture.requests[0].url.searchParams.get("q"), "(synthetic) AND mediatype:texts");
});
