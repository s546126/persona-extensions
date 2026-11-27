import { test } from "node:test";
import assert from "node:assert/strict";
import { budget, ReadError, Wiktionary } from "../src/service.ts";
import { callTool } from "../src/tools.ts";
import { definitions, Fixture, until } from "./fixtures.ts";
const make = (fixture = new Fixture(), options = {}) => new Wiktionary(fixture.fetch, {}, { now: () => fixture.clock, monotonic: () => fixture.clock, limits: { gap: 0 }, locale: "en", ...options });

test("definition lookup uses bounded public requests and a cached validated answer", async () => {
  const fixture = new Fixture(), service = make(fixture);
  assert.equal((await service.lookup("hello")).groups[0].partOfSpeech, "Interjection");
  await service.lookup("hello");
  assert.equal(fixture.hits.get("/api/rest_v1/page/definition/hello"), 1);
  assert.equal(fixture.requests[0].init.redirect, "error");
  assert.equal(fixture.requests[0].init.headers && (fixture.requests[0].init.headers as Record<string, string>).Authorization, undefined);
  assert.equal(service.status().state, "ok"); assert.deepEqual(service.cacheStats().entries, 1);
});

test("failed refresh returns a stale validated answer instead of erasing it", async () => {
  const fixture = new Fixture(), service = make(fixture);
  await service.lookup("hello");
  fixture.put("/api/rest_v1/page/definition/hello", {}, { status: 503 });
  const result = await service.lookup("hello", true);
  assert.equal(result.groups[0].partOfSpeech, "Interjection"); assert.equal(result.stale?.code, "http");
  assert.equal(service.status().state, "error");
  assert.equal(service.status().error, "The dictionary returned HTTP 503.");

  // A malformed HTTP 200 payload during refresh must not overwrite the cached valid entry.
  fixture.answers.set("/api/rest_v1/page/definition/hello", { body: "null", status: 200 });
  const staleBad = await service.lookup("hello", true);
  assert.equal(staleBad.groups[0].partOfSpeech, "Interjection");
  assert.equal(staleBad.stale?.code, "bad_answer");
});

test("malformed, oversized and rate-limited responses fail without caching", async () => {
  for (const response of [{ body: "{bad" }, { body: "null" }, { body: "{}" }, { body: JSON.stringify({ en: "not-array" }) }]) {
    const fixture = new Fixture(); fixture.answers.set("/api/rest_v1/page/definition/hello", response);
    const service = make(fixture);
    await assert.rejects(service.lookup("hello"), ReadError);
    assert.equal(service.cacheStats().entries, 0);
  }
  const fixture = new Fixture(); fixture.answers.set("/api/rest_v1/page/definition/hello", { body: " ".repeat((2 << 20) + 1) });
  await assert.rejects(make(fixture).lookup("hello"), (error: unknown) => error instanceof ReadError && error.code === "bad_answer");
  const limited = new Fixture(); limited.put("/api/rest_v1/page/definition/hello", {}, { status: 429 });
  await assert.rejects(make(limited).lookup("hello"), (error: unknown) => error instanceof ReadError && error.code === "rate_limited");
  const missing = new Fixture();
  const missingService = make(missing);
  await assert.rejects(missingService.lookup("missingword"), (error: unknown) => error instanceof ReadError && error.code === "not_found");
  assert.equal(missingService.status().state, "ok");

  // Bodyless 404 classifies as not_found without poisoning connector status.
  const bodyless404 = new Wiktionary((async () => new Response(null, { status: 404 })) as unknown as Fixture["fetch"], {}, { limits: { gap: 0 }, locale: "en" });
  await assert.rejects(bodyless404.lookup("ghost"), (error: unknown) => error instanceof ReadError && error.code === "not_found");
  assert.equal(bodyless404.status().state, "ok");
});

test("cancellation and deadline propagate through fetch and remove pending work", async () => {
  const fixture = new Fixture(); fixture.put("/api/rest_v1/page/definition/hello", definitions, { delay: 1000 });
  const service = make(fixture), controller = new AbortController();
  const pending = service.lookup("hello", false, controller.signal);
  await until(async () => fixture.requests.length, length => length > 0);
  controller.abort();
  await assert.rejects(pending, (error: unknown) => error instanceof Error && error.name === "AbortError");
  assert.equal(fixture.cancelled, 1);

  const timed = budget(undefined, 1);
  const slow = make(fixture);
  const request = slow.lookup("hello", false, timed.signal);
  const rejected = assert.rejects(request, (error: unknown) => error instanceof ReadError && error.code === "slow");
  await until(async () => timed.signal.aborted, aborted => aborted);
  await rejected; timed.dispose();
  assert.equal(slow.status().state, "error");

  // Internal request timeout classifies as ReadError("slow") and falls back to stale cache.
  const staleFixture = new Fixture();
  const staleService = make(staleFixture, { limits: { gap: 0, request: 25 } });
  await staleService.lookup("hello");
  staleFixture.put("/api/rest_v1/page/definition/hello", definitions, { delay: 500 });
  const staleSlow = await staleService.lookup("hello", true);
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
  const slowService = new Wiktionary(streamFetch, {}, { limits: { gap: 0, request: 20 }, locale: "en" });
  await assert.rejects(slowService.lookup("hello"), (error: unknown) => error instanceof ReadError && error.code === "slow");

  const cancelService = new Wiktionary(streamFetch, {}, { limits: { gap: 0, request: 5000 }, locale: "en" });
  const controller = new AbortController();
  const pending = cancelService.lookup("hello", false, controller.signal);
  setTimeout(() => controller.abort(), 15);
  await assert.rejects(pending, (error: unknown) => error instanceof Error && error.name === "AbortError");
});

test("refreshing oldest cache entry updates LRU order and queued cancellation does not return cached entry", async () => {
  const fixture = new Fixture();
  const smallA = { en: [{ partOfSpeech: "Noun", language: "English", definitions: [{ definition: "a".repeat(20) }] }] };
  const smallB = { en: [{ partOfSpeech: "Noun", language: "English", definitions: [{ definition: "b".repeat(20) }] }] };
  const largerA = { en: [{ partOfSpeech: "Noun", language: "English", definitions: [{ definition: "a".repeat(90) }] }] };
  fixture.put("/api/rest_v1/page/definition/alpha", smallA);
  fixture.put("/api/rest_v1/page/definition/beta", smallB);
  const bytesA = new TextEncoder().encode(JSON.stringify(smallA)).byteLength;
  const bytesB = new TextEncoder().encode(JSON.stringify(smallB)).byteLength;
  const bytesLargerA = new TextEncoder().encode(JSON.stringify(largerA)).byteLength;
  const service = make(fixture, { limits: { gap: 0, cacheBytes: Math.max(bytesA + bytesB, bytesLargerA + 10) } });
  await service.lookup("alpha");
  await service.lookup("beta");
  fixture.put("/api/rest_v1/page/definition/alpha", largerA);
  await service.lookup("alpha", true);
  // Refreshing alpha must evict beta, NOT freshly updated alpha.
  await service.lookup("alpha");
  assert.equal(fixture.hits.get("/api/rest_v1/page/definition/alpha"), 2);

  // Refreshing with a valid payload exceeding cacheBytes evicts the outdated cached entry.
  const hugeA = { en: [{ partOfSpeech: "Noun", language: "English", definitions: [{ definition: "z".repeat(400) }] }] };
  fixture.put("/api/rest_v1/page/definition/alpha", hugeA);
  const refreshedHuge = await service.lookup("alpha", true);
  assert.equal(refreshedHuge.groups[0].definitions[0].text, "z".repeat(400));
  assert.equal(service.cacheStats().entries, 0);

  // Queued caller aborted while waiting behind an earlier slow request must reject with AbortError.
  const qFixture = new Fixture();
  qFixture.put("/api/rest_v1/page/definition/hello", definitions, { delay: 40 });
  const qService = make(qFixture);
  const first = qService.lookup("hello");
  const controller = new AbortController();
  const second = qService.lookup("hello", false, controller.signal);
  controller.abort();
  await assert.rejects(second, (error: unknown) => error instanceof Error && error.name === "AbortError");
  await first;
});

test("assistant tools retain content, normalize tool timeouts, and reject invalid or unknown calls", async () => {
  const service = make();
  assert.match(await callTool(service, "wiktionary_lookup", { word: "hello" }), /Interjection[\s\S]*greeting[\s\S]*Hello, everyone/);
  for (const word of ["", "x".repeat(121), "bad\n", "bad#word"]) await assert.rejects(callTool(service, "wiktionary_lookup", { word }), ReadError);
  await assert.rejects(callTool(service, "unknown"), /unknown method/);
  await assert.rejects(callTool(service, "unknown", []), /unknown method/);

  const slowFixture = new Fixture();
  slowFixture.put("/api/rest_v1/page/definition/hello", definitions, { delay: 500 });
  const slowService = make(slowFixture, { limits: { gap: 0, tool: 20, request: 5000 } });
  await assert.rejects(callTool(slowService, "wiktionary_lookup", { word: "hello" }), (error: unknown) => error instanceof ReadError && error.code === "slow");
});

test("every language reads definitions from en.wiktionary, the reader's senses first, linking the reader's wiki", async () => {
  // Only English Wiktionary serves /api/rest_v1/page/definition; zh.wiktionary and ja.wiktionary answer 501.
  const answer = { en: definitions.en, ja: [{ partOfSpeech: "感動詞", language: "日本語", definitions: [{ definition: "こんにちは。" }] }],
    zh: [{ partOfSpeech: "叹词", language: "汉语", definitions: [{ definition: "你好。" }] }] };
  for (const [locale, language] of [["zh-Hans", "zh"], ["ja", "ja"], ["en", "en"]] as const) {
    const fixture = new Fixture();
    fixture.put("/api/rest_v1/page/definition/hello", answer);
    const service = new Wiktionary(fixture.fetch, {}, { locale, limits: { gap: 0 }, now: () => fixture.clock });
    const entry = await service.lookup("hello");
    assert.equal(fixture.requests[0].url.hostname, "en.wiktionary.org");
    assert.equal(entry.groups[0].language, { en: "English", ja: "日本語", zh: "汉语" }[language]);
    assert.equal(entry.url, `https://${language}.wiktionary.org/wiki/hello`);
  }
  const fixture = new Fixture(), service = new Wiktionary(fixture.fetch, { language: "ja" }, { locale: "zh", limits: { gap: 0 }, now: () => fixture.clock });
  const entry = await service.lookup("hello");
  assert.equal(fixture.requests[0].url.hostname, "en.wiktionary.org");
  assert.equal(entry.url, "https://ja.wiktionary.org/wiki/hello");
});
