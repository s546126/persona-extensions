import { test } from "node:test";
import assert from "node:assert/strict";
import { ExactJSON } from "@persona/extension";
import { budget, Commons, ReadError } from "../src/service.ts";
import { callTool } from "../src/tools.ts";
import { Fixture, response, until } from "./fixtures.ts";
const make = (fixture = new Fixture(), options = {}) => new Commons(fixture.fetch, {}, { now: () => fixture.clock, monotonic: () => fixture.clock, limits: { gap: 0 }, locale: "en", ...options });
const path = "/w/api.php?action=query&generator=search&gsrsearch=File%3AEiffel+Tower&gsrnamespace=6&gsrlimit=20&prop=imageinfo&iiprop=url%7Cextmetadata%7Csize&iiurlwidth=640&format=json";

test("image metadata uses bounded public requests and a cached validated answer", async () => {
  const fixture = new Fixture(), service = make(fixture);
  assert.equal((await service.search("Eiffel Tower")).images[0].artist, "Example Photographer");
  await service.search("Eiffel Tower");
  assert.equal(fixture.hits.get(path), 1);
  assert.equal(fixture.requests[0].init.redirect, "error");
  assert.equal(fixture.requests[0].init.headers && (fixture.requests[0].init.headers as Record<string, string>).Authorization, undefined);
  assert.equal(service.status().state, "ok"); assert.deepEqual(service.cacheStats().entries, 1);
});

test("failed refresh returns a stale validated answer instead of erasing it", async () => {
  const fixture = new Fixture(), service = make(fixture);
  await service.search("Eiffel Tower");
  fixture.put(path, {}, { status: 503 });
  const result = await service.search("Eiffel Tower", 20, true);
  assert.equal(result.images[0].artist, "Example Photographer"); assert.equal(result.stale?.code, "http");
  assert.equal(service.status().state, "error");
  assert.equal(service.status().error, "Commons returned HTTP 503.");

  // A malformed HTTP 200 payload during refresh must not overwrite the cached valid entry.
  fixture.answers.set(path, { body: "null", status: 200 });
  const staleBad = await service.search("Eiffel Tower", 20, true);
  assert.equal(staleBad.images[0].artist, "Example Photographer");
  assert.equal(staleBad.stale?.code, "bad_answer");
});

test("malformed, oversized and rate-limited responses fail without caching", async () => {
  for (const answer of [{ body: "{bad" }, { body: "null" }, { body: "{}" }, { body: JSON.stringify({ query: { pages: "bad" } }) }]) {
    const fixture = new Fixture(); fixture.answers.set(path, answer);
    const service = make(fixture);
    await assert.rejects(service.search("Eiffel Tower"), ReadError);
    assert.equal(service.cacheStats().entries, 0);
  }
  const fixture = new Fixture(); fixture.answers.set(path, { body: " ".repeat((2 << 20) + 1) });
  await assert.rejects(make(fixture).search("Eiffel Tower"), (error: unknown) => error instanceof ReadError && error.code === "bad_answer");

  // Oversized stream whose cancel() rejects must still classify as bad_answer, not network.
  const failingCancelFetch = (async () => new Response(new ReadableStream<Uint8Array>({
    pull(controller) { controller.enqueue(new Uint8Array(64)); },
    cancel() { return Promise.reject(new Error("cancel failed")); },
  }), { status: 200 })) as unknown as Fixture["fetch"];
  await assert.rejects(new Commons(failingCancelFetch, {}, { limits: { gap: 0, response: 32 }, locale: "en" }).search("Eiffel Tower"), (error: unknown) => error instanceof ReadError && error.code === "bad_answer");

  const limited = new Fixture(); limited.put(path, {}, { status: 429 });
  await assert.rejects(make(limited).search("Eiffel Tower"), (error: unknown) => error instanceof ReadError && error.code === "rate_limited");

  // Non-200 response with invalid UTF-8 body classifies by status, not bad_answer.
  const invalidUtf8Error = new Commons((async () => new Response(new Uint8Array([0xff, 0xfe]), { status: 503 })) as unknown as Fixture["fetch"], {}, { limits: { gap: 0 }, locale: "en" });
  await assert.rejects(invalidUtf8Error.search("Eiffel Tower"), (error: unknown) => error instanceof ReadError && error.code === "http" && error.status === 503);

  // Bodyless 404 classifies as not_found without poisoning connector status.
  const bodyless404 = new Commons((async () => new Response(null, { status: 404 })) as unknown as Fixture["fetch"], {}, { limits: { gap: 0 }, locale: "en" });
  await assert.rejects(bodyless404.search("Eiffel Tower"), (error: unknown) => error instanceof ReadError && error.code === "not_found");
  assert.equal(bodyless404.status().state, "ok");
});

test("cancellation and deadline propagate through fetch and remove pending work", async () => {
  const fixture = new Fixture(); fixture.put(path, response, { delay: 1000 });
  const service = make(fixture), controller = new AbortController();
  const pending = service.search("Eiffel Tower", 20, false, controller.signal);
  await until(async () => fixture.requests.length, length => length > 0);
  controller.abort();
  await assert.rejects(pending, (error: unknown) => error instanceof Error && error.name === "AbortError");
  assert.equal(fixture.cancelled, 1);

  const timed = budget(undefined, 1);
  const slow = make(fixture);
  const request = slow.search("Eiffel Tower", 20, false, timed.signal);
  const rejected = assert.rejects(request, (error: unknown) => error instanceof ReadError && error.code === "slow");
  await until(async () => timed.signal.aborted, aborted => aborted);
  await rejected; timed.dispose();
  assert.equal(slow.status().state, "error");

  // Internal request timeout classifies as ReadError("slow") and falls back to stale cache.
  const staleFixture = new Fixture();
  const staleService = make(staleFixture, { limits: { gap: 0, request: 25 } });
  await staleService.search("Eiffel Tower");
  staleFixture.put(path, response, { delay: 500 });
  const staleSlow = await staleService.search("Eiffel Tower", 20, true);
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
  const slowService = new Commons(streamFetch, {}, { limits: { gap: 0, request: 20 }, locale: "en" });
  await assert.rejects(slowService.search("Eiffel Tower"), (error: unknown) => error instanceof ReadError && error.code === "slow");

  const cancelService = new Commons(streamFetch, {}, { limits: { gap: 0, request: 5000 }, locale: "en" });
  const controller = new AbortController();
  const pending = cancelService.search("Eiffel Tower", 20, false, controller.signal);
  setTimeout(() => controller.abort(), 15);
  await assert.rejects(pending, (error: unknown) => error instanceof Error && error.name === "AbortError");
});

test("refreshing oldest cache entry updates LRU order and queued cancellation does not return cached entry", async () => {
  const fixture = new Fixture();
  const pathA = "/w/api.php?action=query&generator=search&gsrsearch=File%3Aalpha&gsrnamespace=6&gsrlimit=20&prop=imageinfo&iiprop=url%7Cextmetadata%7Csize&iiurlwidth=640&format=json";
  const pathB = "/w/api.php?action=query&generator=search&gsrsearch=File%3Abeta&gsrnamespace=6&gsrlimit=20&prop=imageinfo&iiprop=url%7Cextmetadata%7Csize&iiurlwidth=640&format=json";
  const makeResp = (id: number, title: string, desc: string) => ({
    query: { pages: { [id]: { pageid: id, ns: 6, title: `File:${title}.jpg`, imageinfo: [{ descriptionurl: `https://commons.wikimedia.org/wiki/File:${title}.jpg`, extmetadata: { ImageDescription: { value: desc } } }] } } },
  });
  const smallA = makeResp(1, "Alpha", "a".repeat(20));
  const smallB = makeResp(2, "Beta", "b".repeat(20));
  const largerA = makeResp(1, "Alpha", "a".repeat(120));
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
  const hugeA = makeResp(1, "Alpha", "z".repeat(600));
  fixture.put(pathA, hugeA);
  const refreshedHuge = await service.search("alpha", 20, true);
  assert.equal(refreshedHuge.images[0].description, "z".repeat(600));
  assert.equal(service.cacheStats().entries, 0);

  // Queued caller aborted while waiting behind an earlier slow request must reject with AbortError.
  const qFixture = new Fixture();
  qFixture.put(path, response, { delay: 40 });
  const qService = make(qFixture);
  const first = qService.search("Eiffel Tower");
  const controller = new AbortController();
  const second = qService.search("Eiffel Tower", 20, false, controller.signal);
  controller.abort();
  await assert.rejects(second, (error: unknown) => error instanceof Error && error.name === "AbortError");
  await first;
});

test("assistant tools retain attribution, validate ExactJSON limits, normalize timeouts, and reject invalid or unknown calls", async () => {
  const service = make();
  assert.match(await callTool(service, "commons_search", { query: "Eiffel Tower" }), /Example Photographer[\s\S]*Public domain[\s\S]*Wikimedia_Commons/);
  assert.match(await callTool(service, "commons_search", { query: "Eiffel Tower" }, undefined, ExactJSON.parse('{"query":"Eiffel Tower"}')), /Example Photographer/);
  assert.match(await callTool(service, "commons_search", { query: "Eiffel Tower", limit: null }, undefined, ExactJSON.parse('{"query":"Eiffel Tower","limit":null}')), /Example Photographer/);
  for (const raw of ['{"query":"Eiffel Tower","limit":0}', '{"query":"Eiffel Tower","limit":21}', '{"query":"Eiffel Tower","limit":1.5}', '{"query":"Eiffel Tower","limit":1e1}', '{"query":"Eiffel Tower","limit":"5"}', '{"query":"Eiffel Tower","limit":9223372036854775808}', '{"query":"Eiffel Tower","limit":9223372036854775807}', '{"query":null}', '[]']) {
    await assert.rejects(callTool(service, "commons_search", JSON.parse(raw), undefined, ExactJSON.parse(raw)), (error: unknown) => error instanceof ReadError && error.code === "invalid");
  }
  for (const args of [{ query: "" }, { query: "x".repeat(161) }, { query: "\uD800" }, { query: "ok", limit: 0 }, { query: "ok", limit: 21 }, { query: "ok", limit: 1.5 }, { query: 2 }]) {
    await assert.rejects(callTool(service, "commons_search", args), ReadError);
  }
  await assert.rejects(callTool(service, "unknown"), /unknown method/);
  await assert.rejects(callTool(service, "unknown", [], undefined, ExactJSON.parse("[]")), /unknown method/);

  const slowFixture = new Fixture();
  slowFixture.put("/w/api.php?action=query&generator=search&gsrsearch=File%3AEiffel+Tower&gsrnamespace=6&gsrlimit=10&prop=imageinfo&iiprop=url%7Cextmetadata%7Csize&iiurlwidth=640&format=json", response, { delay: 500 });
  const slowService = make(slowFixture, { limits: { gap: 0, tool: 20, request: 5000 } });
  await assert.rejects(callTool(slowService, "commons_search", { query: "Eiffel Tower" }), (error: unknown) => error instanceof ReadError && error.code === "slow");
});
