import { test } from "node:test";
import assert from "node:assert/strict";
import { budget, FoodFacts, ReadError } from "../src/service.ts";
import { loadPage } from "../src/page.tsx";
import { callTool } from "../src/tools.ts";
import { Fixture, productPath, productResponse, searchPath, searchResponse, until } from "./fixtures.ts";
const make = (fixture = new Fixture(), options = {}) => new FoodFacts(fixture.fetch, {}, { now: () => fixture.clock, monotonic: () => fixture.clock, limits: { gap: 0 }, locale: "en", ...options });

test("food search uses bounded public requests and a cached validated answer", async () => {
  const fixture = new Fixture(), service = make(fixture);
  assert.equal((await service.search("chocolate")).products[0].name, "Nutella");
  await service.search("chocolate");
  assert.equal(fixture.hits.get(searchPath), 1);
  assert.equal(fixture.requests[0].init.redirect, "error");
  assert.equal(fixture.requests[0].init.headers && (fixture.requests[0].init.headers as Record<string, string>).Authorization, undefined);
  assert.equal(service.status().state, "ok"); assert.deepEqual(service.cacheStats().entries, 1);
});

test("failed refresh returns a stale validated answer instead of erasing it", async () => {
  const fixture = new Fixture(), service = make(fixture);
  await service.search("chocolate");
  fixture.put(searchPath, {}, { status: 503 });
  const result = await service.search("chocolate", 12, true);
  assert.equal(result.products[0].name, "Nutella"); assert.equal(result.stale?.code, "http");
  assert.equal(service.status().state, "error");
});

test("malformed, oversized and rate-limited responses fail without caching", async () => {
  for (const answer of [{ body: "{bad" }, { body: "null" }]) {
    const fixture = new Fixture(); fixture.answers.set(searchPath, answer);
    const service = make(fixture);
    await assert.rejects(service.search("chocolate"), ReadError);
    assert.equal(service.cacheStats().entries, 0);
    assert.equal(service.status().state, "error");
  }
  const missingFixture = new Fixture();
  missingFixture.put(productPath, { status: 0, status_verbose: "product not found" });
  const missingService = make(missingFixture);
  await assert.rejects(missingService.product("3017620422003"), (error: unknown) => error instanceof ReadError && error.code === "not_found");
  assert.equal(missingService.cacheStats().entries, 0);
  assert.equal(missingService.status().state, "ok");
  const invalidRefresh = new Fixture(), staleService = make(invalidRefresh);
  await staleService.search("chocolate");
  invalidRefresh.answers.set(searchPath, { body: "null" });
  const stale = await staleService.search("chocolate", 12, true);
  assert.equal(stale.products[0].name, "Nutella");
  assert.equal(stale.stale?.code, "bad_answer");
  assert.equal(staleService.status().state, "error");
  const fixture = new Fixture(); fixture.answers.set(searchPath, { body: " ".repeat(1 << 20) });
  await assert.rejects(make(fixture).search("chocolate"), (error: unknown) => error instanceof ReadError && error.code === "bad_answer");
  const limited = new Fixture(); limited.put(searchPath, {}, { status: 429 });
  await assert.rejects(make(limited).search("chocolate"), (error: unknown) => error instanceof ReadError && error.code === "rate_limited");
});

test("cancellation and deadline propagate through fetch and remove pending work", async () => {
  const fixture = new Fixture(); fixture.put(searchPath, searchResponse, { delay: 1000 });
  const service = make(fixture), controller = new AbortController();
  const pending = service.search("chocolate", 12, false, controller.signal);
  await until(async () => fixture.requests.length, length => length > 0);
  controller.abort();
  await assert.rejects(pending, (error: unknown) => error instanceof Error && error.name === "AbortError");
  assert.equal(fixture.cancelled, 1);
  const timed = budget(undefined, 1);
  const slow = make(fixture);
  const request = slow.search("chocolate", 12, false, timed.signal);
  const rejected = assert.rejects(request, (error: unknown) => error instanceof ReadError && error.code === "slow");
  await until(async () => timed.signal.aborted, aborted => aborted);
  await rejected; timed.dispose();

  // An aborted queued request must reject rather than resolving from a sibling request's cache.
  const queuedFixture = new Fixture();
  queuedFixture.put(searchPath, searchResponse, { delay: 40 });
  const queuedService = make(queuedFixture);
  const first = queuedService.search("chocolate");
  const secondController = new AbortController();
  const second = queuedService.search("chocolate", 12, false, secondController.signal);
  secondController.abort();
  await assert.rejects(second, (error: unknown) => error instanceof Error && error.name === "AbortError");
  assert.equal((await first).products[0].name, "Nutella");
});

test("assistant tools retain product metadata and reject invalid or unknown calls", async () => {
  const service = make();
  assert.match(await callTool(service, "foodfacts_search", { query: "chocolate" }), /Nutella[\s\S]*milk, nuts, soybeans[\s\S]*ODbL/);
  assert.match(await callTool(service, "foodfacts_product", { code: "3017620422003" }), /Nutella[\s\S]*world.openfoodfacts.org\/product\/3017620422003/);
  for (const args of [{ query: "" }, { query: "x".repeat(121) }, { query: "ok", limit: 0 }, { query: "ok", limit: 21 }, { query: "ok", limit: 1.5 }, { query: 2 }]) await assert.rejects(callTool(service, "foodfacts_search", args), ReadError);
  await assert.rejects(callTool(service, "foodfacts_product", { code: "bad" }), ReadError);
  await assert.rejects(callTool(service, "unknown"), /unknown method/);
});

test("internal request, page and tool timeouts surface ReadError(slow) and allow stale fallback", async () => {
  const fixture = new Fixture();
  const service = make(fixture, { limits: { gap: 0, request: 20 } });
  await service.search("chocolate");
  fixture.put(searchPath, searchResponse, { delay: 500 });
  const stale = await service.search("chocolate", 12, true);
  assert.equal(stale.products[0].name, "Nutella");
  assert.equal(stale.stale?.code, "slow");
  assert.equal(service.status().state, "error");

  const uncachedFixture = new Fixture();
  uncachedFixture.put(searchPath, searchResponse, { delay: 500 });
  const uncached = make(uncachedFixture, { limits: { gap: 0, request: 20 } });
  await assert.rejects(uncached.search("chocolate"), (error: unknown) => error instanceof ReadError && error.code === "slow");
  assert.equal(uncached.status().state, "error");

  const pageFixture = new Fixture();
  pageFixture.put(searchPath, searchResponse, { delay: 500 });
  const pageService = make(pageFixture, { limits: { gap: 0, request: 500, page: 20 } });
  const loaded = await loadPage(pageService, { q: "chocolate", found: true }, false);
  assert.equal(loaded.kind, "error");
  assert.ok(loaded.error instanceof ReadError && loaded.error.code === "slow");

  // Cached refresh timing out on limits.page returns stale cached content rather than an error view.
  const pageStaleFixture = new Fixture();
  const pageStaleService = make(pageStaleFixture, { limits: { gap: 0, request: 500, page: 20 } });
  await pageStaleService.search("chocolate");
  pageStaleFixture.put(searchPath, searchResponse, { delay: 500 });
  const stalePage = await loadPage(pageStaleService, { q: "chocolate", found: true }, true);
  assert.equal(stalePage.kind, "result");
  assert.equal(stalePage.result.products[0].name, "Nutella");
  assert.equal(stalePage.result.stale?.code, "slow");

  // Refreshing the oldest cached key with a larger payload must evict another key, not itself.
  const evictFixture = new Fixture();
  const firstBytes = new TextEncoder().encode(JSON.stringify(searchResponse)).byteLength;
  const secondBytes = new TextEncoder().encode(JSON.stringify(productResponse)).byteLength;
  const biggerSearch = { ...searchResponse, products: [{ ...searchResponse.products[0], ingredients_text: "sugar, ".repeat(30) }] };
  const evictService = make(evictFixture, { limits: { gap: 0, cacheBytes: firstBytes + secondBytes + 80 } });
  await evictService.search("chocolate");
  await evictService.product("3017620422003");
  evictFixture.put(searchPath, biggerSearch);
  await evictService.search("chocolate", 12, true);
  const hitsBefore = evictFixture.hits.get(searchPath);
  await evictService.search("chocolate");
  assert.equal(evictFixture.hits.get(searchPath), hitsBefore);

  const toolFixture = new Fixture();
  toolFixture.put(productPath, productResponse, { delay: 500 });
  const toolService = make(toolFixture, { limits: { gap: 0, request: 500, tool: 20 } });
  await assert.rejects(callTool(toolService, "foodfacts_product", { code: "3017620422003" }), (error: unknown) => error instanceof ReadError && error.code === "slow");
});

test("non-200 HTTP errors with null or oversized bodies preserve status classification", async () => {
  const null429 = new FoodFacts(async () => new Response(null, { status: 429 }), {}, { limits: { gap: 0 } });
  await assert.rejects(null429.search("chocolate"), (error: unknown) => error instanceof ReadError && error.code === "rate_limited");
  assert.equal(null429.status().state, "rate_limited");

  const huge503 = new FoodFacts(async () => new Response("x".repeat((1 << 20) + 128), { status: 503 }), {}, { limits: { gap: 0 } });
  await assert.rejects(huge503.search("chocolate"), (error: unknown) => error instanceof ReadError && error.code === "http" && error.status === 503);
});

test("cache separates LRU recency from TTL freshness", async () => {
  const fixture = new Fixture();
  const oatPath = searchPath.replace("chocolate", "oat");
  const milkPath = searchPath.replace("chocolate", "milk");
  fixture.put(oatPath, searchResponse);
  fixture.put(milkPath, searchResponse);
  const service = make(fixture, { limits: { gap: 0, cache: 2, fresh: 1000 } });

  await service.search("chocolate"); // cached at t=0
  fixture.clock += 200;
  await service.search("oat"); // cached at t=200
  fixture.clock += 200;
  await service.search("chocolate"); // cache hit at t=400 -> promotes chocolate as MRU, keeps TTL anchored at t=0
  assert.equal(fixture.hits.get(searchPath), 1);

  await service.search("milk"); // capacity 2 -> must evict cold "oat", NOT hot "chocolate"
  await service.search("chocolate"); // still cached at t=400 < 1000
  assert.equal(fixture.hits.get(searchPath), 1, "hot entry must not be evicted before colder entry");

  fixture.clock += 700; // t=1100 > savedAt(0) + fresh(1000) -> TTL must expire even though hit happened at t=400
  await service.search("chocolate");
  assert.equal(fixture.hits.get(searchPath), 2, "LRU read must not extend TTL beyond fresh window");
});
