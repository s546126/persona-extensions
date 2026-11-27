import { test } from "node:test";
import assert from "node:assert/strict";
import { budget, ReadError, Wikipedia } from "../src/service.ts";
import { callTool } from "../src/tools.ts";
import { Fixture, syntheticArticle } from "./fixtures.ts";
const make = (fixture = new Fixture(), options = {}) => new Wikipedia(fixture.fetch, {}, { now: () => fixture.clock, monotonic: () => fixture.clock, limits: { gap: 0 }, locale: "en", ...options });
test("feed, search and summary share bounded public requests and cached responses", async () => {
  const fixture = new Fixture(), service = make(fixture);
  assert.equal((await service.today()).featured?.heading, "Featured article");
  assert.equal((await service.search("synthetic"))[0].title, "Found_article");
  assert.equal((await service.article("Featured article")).extract, "A detailed synthetic introduction.");
  await service.today(); await service.search("synthetic"); await service.article("Featured_article");
  assert.equal(fixture.hits.get("/api/rest_v1/feed/featured/2026/11/09"), 1);
  assert.equal(fixture.hits.get("/w/rest.php/v1/search/page?q=synthetic&limit=30"), 1);
  assert.equal(fixture.hits.get("/api/rest_v1/page/summary/Featured_article"), 1);
  assert.equal(fixture.requests[0].init.redirect, "error");
  assert.equal(fixture.requests[0].init.headers && (fixture.requests[0].init.headers as Record<string, string>).Authorization, undefined);
  assert.equal(service.status().state, "ok"); assert.deepEqual(service.cacheStats().entries, 3);
});
test("failed refresh returns a stale validated answer instead of erasing it", async () => {
  const fixture = new Fixture(), service = make(fixture);
  await service.today();
  fixture.put("/api/rest_v1/feed/featured/2026/11/09", {}, { status: 503 });
  const result = await service.today(undefined, true);
  assert.equal(result.featured?.heading, "Featured article"); assert.equal(result.stale?.code, "http");
  assert.equal(service.status().state, "error");
});
test("malformed, oversized and rate-limited responses fail without caching", async () => {
  for (const response of [{ body: "{bad" }, { body: "null" }]) {
    const fixture = new Fixture(); fixture.answers.set("/api/rest_v1/feed/featured/2026/11/09", response);
    await assert.rejects(make(fixture).today(), ReadError);
  }
  const fixture = new Fixture(); fixture.answers.set("/api/rest_v1/feed/featured/2026/11/09", { body: " ".repeat(4 << 20) });
  await assert.rejects(make(fixture).today(), (error: unknown) => error instanceof ReadError && error.code === "bad_answer");
  const limited = new Fixture(); limited.put("/api/rest_v1/feed/featured/2026/11/09", {}, { status: 429 });
  await assert.rejects(make(limited).today(), (error: unknown) => error instanceof ReadError && error.code === "rate_limited");
});
test("cancellation and deadline propagate through fetch and remove pending work", async () => {
  const fixture = new Fixture(); fixture.put("/api/rest_v1/feed/featured/2026/11/09", { tfa: syntheticArticle() }, { delay: 1000 });
  const service = make(fixture), controller = new AbortController();
  const pending = service.today(undefined, false, controller.signal);
  await new Promise(resolve => setTimeout(resolve, 20));
  controller.abort();
  await assert.rejects(pending, (error: unknown) => error instanceof Error && error.name === "AbortError");
  assert.equal(fixture.cancelled, 1);
  const timed = budget(undefined, 1);
  await assert.rejects(make(new Fixture()).today(undefined, false, timed.signal), (error: unknown) => error instanceof ReadError && error.code === "slow");
  timed.dispose();
});
test("assistant tools retain content and reject invalid or unknown calls", async () => {
  const service = make();
  assert.match(await callTool(service, "wikipedia_today"), /Featured today[\s\S]*Popular article[\s\S]*1969/);
  assert.match(await callTool(service, "wikipedia_search", { query: "synthetic" }), /Found article/);
  assert.match(await callTool(service, "wikipedia_article", { title: "Featured_article" }), /detailed synthetic introduction/);
  for (const args of [{ query: "" }, { query: "x".repeat(201) }, { query: "ok", limit: 0 }, { query: "ok", limit: 31 }, { query: "ok", limit: 1.5 }, { query: 2 }]) await assert.rejects(callTool(service, "wikipedia_search", args), ReadError);
  await assert.rejects(callTool(service, "wikipedia_article", { title: "bad#target" }), ReadError);
  await assert.rejects(callTool(service, "unknown"), /unknown method/);
});
test("language config chooses only registered origins", async () => {
  for (const [locale, language] of [["zh-Hans", "zh"], ["ja", "ja"], ["en", "en"]] as const) {
    const fixture = new Fixture(), service = new Wikipedia(fixture.fetch, {}, { locale, limits: { gap: 0 }, now: () => fixture.clock });
    await service.today(); assert.equal(fixture.requests[0].url.hostname, `${language}.wikipedia.org`);
  }
  const fixture = new Fixture(), service = new Wikipedia(fixture.fetch, { language: "ja" }, { locale: "zh", limits: { gap: 0 }, now: () => fixture.clock });
  await service.today(); assert.equal(fixture.requests[0].url.hostname, "ja.wikipedia.org");
});
test("unvalidated HTTP 200 JSON does not poison cache or erase valid stale entries", async () => {
  const fixture = new Fixture(), service = make(fixture);
  fixture.answers.set("/api/rest_v1/feed/featured/2026/11/09", { body: "null" });
  await assert.rejects(service.today(), (error: unknown) => error instanceof ReadError && error.code === "bad_answer");
  assert.equal(service.cacheStats().entries, 0);
  assert.equal(service.status().state, "error");

  fixture.put("/api/rest_v1/feed/featured/2026/11/09", { tfa: syntheticArticle("Featured_article") });
  assert.equal((await service.today()).featured?.title, "Featured_article");
  assert.equal(service.status().state, "ok");

  fixture.answers.set("/api/rest_v1/feed/featured/2026/11/09", { body: "{}" });
  const stale = await service.today(undefined, true);
  assert.equal(stale.featured?.title, "Featured_article");
  assert.equal(stale.stale?.code, "bad_answer");
  assert.equal(service.status().state, "error");
});
test("response body stream timeout classifies as slow, records lastError, and falls back to stale cache", async () => {
  let stalled = false;
  const fixture = new Fixture();
  const service = new Wikipedia(async (input, init) => {
    if (!stalled) return fixture.fetch(input, init);
    const stream = new ReadableStream<Uint8Array>({
      start(controller) {
        const onAbort = () => controller.error(new DOMException("The operation was aborted.", "AbortError"));
        if (init.signal.aborted) onAbort(); else init.signal.addEventListener("abort", onAbort, { once: true });
      },
    });
    return new Response(stream, { status: 200 });
  }, {}, { now: () => fixture.clock, monotonic: () => fixture.clock, limits: { gap: 0, request: 25 }, locale: "en" });

  stalled = true;
  await assert.rejects(service.today(), (error: unknown) => error instanceof ReadError && error.code === "slow");
  assert.equal(service.status().state, "error");

  stalled = false;
  assert.equal((await service.today()).featured?.title, "Featured_article");
  assert.equal(service.status().state, "ok");

  stalled = true;
  const stale = await service.today(undefined, true);
  assert.equal(stale.featured?.title, "Featured_article");
  assert.equal(stale.stale?.code, "slow");
  assert.equal(service.status().state, "error");
});
test("refreshing oldest cache entry with a larger body keeps the refreshed entry", async () => {
  const fixture = new Fixture();
  const smallA = syntheticArticle("Article_A", { extract: "a".repeat(200) });
  const smallB = syntheticArticle("Article_B", { extract: "b".repeat(200) });
  const largeA = syntheticArticle("Article_A", { extract: "A".repeat(700) });
  fixture.put("/api/rest_v1/page/summary/Article_A", smallA);
  fixture.put("/api/rest_v1/page/summary/Article_B", smallB);
  const service = make(fixture, { limits: { gap: 0, cacheBytes: 1200 } });
  await service.article("Article_A");
  await service.article("Article_B");
  fixture.put("/api/rest_v1/page/summary/Article_A", largeA);
  await service.article("Article_A", true);
  const hitsBefore = fixture.hits.get("/api/rest_v1/page/summary/Article_A");
  assert.equal((await service.article("Article_A")).extract, "A".repeat(700));
  assert.equal(fixture.hits.get("/api/rest_v1/page/summary/Article_A"), hitsBefore);
});
test("aborted queued waiter does not resolve from sibling cache hit and 404 does not set connector error", async () => {
  const fixture = new Fixture();
  fixture.put("/api/rest_v1/page/summary/Featured_article", syntheticArticle("Featured_article"), { delay: 40 });
  const service = make(fixture);
  const first = service.article("Featured_article");
  const controller = new AbortController();
  const second = service.article("Featured_article", false, controller.signal);
  controller.abort();
  await assert.rejects(second, (error: unknown) => error instanceof Error && error.name === "AbortError");
  assert.equal((await first).title, "Featured_article");
  await assert.rejects(service.article("Missing_article"), (error: unknown) => error instanceof ReadError && error.code === "not_found");
  assert.equal(service.status().state, "ok");

  // An already-expired slow signal rejects even when cached, and queued budget expiry does not clobber healthy status.
  const expired = AbortSignal.abort(new ReadError("slow"));
  await assert.rejects(service.article("Featured_article", false, expired), (error: unknown) => error instanceof ReadError && error.code === "slow");
  assert.equal(service.status().state, "ok");

  fixture.put("/api/rest_v1/page/summary/Slow_ok", syntheticArticle("Slow_ok"), { delay: 40 });
  const healthy = service.article("Slow_ok", true);
  const tight = budget(undefined, 5);
  const queued = service.article("Featured_article", true, tight.signal);
  assert.equal((await healthy).title, "Slow_ok");
  // Waiter timed out in queue -> falls back to stale cache without clobbering status() to error.
  assert.equal((await queued).title, "Featured_article");
  assert.equal(service.status().state, "ok");
  tight.dispose();

  // HTTP status error formats its status code in status().error.
  fixture.put("/api/rest_v1/page/summary/Broken", {}, { status: 503 });
  await assert.rejects(service.article("Broken"), (error: unknown) => error instanceof ReadError && error.code === "http");
  assert.equal(service.status().error, "Wikipedia returned HTTP 503.");
});
