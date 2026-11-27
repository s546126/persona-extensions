import { test } from "node:test";
import assert from "node:assert/strict";
import { budget, CurrencyService, ReadError } from "../src/service.ts";
import { callTool } from "../src/tools.ts";
import { Fixture, ratePath, response, until } from "./fixtures.ts";
const make = (fixture = new Fixture(), options = {}) => new CurrencyService(fixture.fetch, {}, { now: () => fixture.clock, monotonic: () => fixture.clock, limits: { gap: 0 }, locale: "en", ...options });

test("currency conversion uses only ECB reference rates and caches the validated answer", async () => {
  const fixture = new Fixture(), service = make(fixture);
  assert.equal((await service.rate("USD", "CNY")).date, "2026-11-10");
  await service.rate("USD", "CNY");
  assert.equal(fixture.hits.get(ratePath), 1);
  assert.equal(fixture.requests[0].init.redirect, "error");
  assert.equal(fixture.requests[0].init.headers && (fixture.requests[0].init.headers as Record<string, string>).Authorization, undefined);
  assert.equal(service.status().state, "ok"); assert.deepEqual(service.cacheStats().entries, 1);
});

test("a failed refresh retains the old dated answer and marks it stale", async () => {
  const fixture = new Fixture(), service = make(fixture);
  await service.rate("USD", "CNY");
  fixture.put(ratePath, {}, { status: 503 });
  const result = await service.rate("USD", "CNY", true);
  assert.equal(result.rate, "6.6703"); assert.equal(result.stale?.code, "http");
  assert.equal(service.status().state, "error");
});

test("malformed, oversized and rate-limited responses fail without caching", async () => {
  for (const answer of [{ body: "{bad" }, { body: "null" }]) {
    const fixture = new Fixture(); fixture.answers.set(ratePath, answer);
    await assert.rejects(make(fixture).rate("USD", "CNY"), ReadError);
  }
  const fixture = new Fixture(); fixture.answers.set(ratePath, { body: " ".repeat(256 << 10) });
  await assert.rejects(make(fixture).rate("USD", "CNY"), (error: unknown) => error instanceof ReadError && error.code === "bad_answer");
  const limited = new Fixture(); limited.put(ratePath, {}, { status: 429 });
  await assert.rejects(make(limited).rate("USD", "CNY"), (error: unknown) => error instanceof ReadError && error.code === "rate_limited");
});

test("cancellation and deadlines propagate through fetch", async () => {
  const fixture = new Fixture(); fixture.put(ratePath, response, { delay: 1000 });
  const service = make(fixture), controller = new AbortController();
  const pending = service.rate("USD", "CNY", false, controller.signal);
  await until(async () => fixture.requests.length, length => length > 0);
  controller.abort();
  await assert.rejects(pending, (error: unknown) => error instanceof Error && error.name === "AbortError");
  assert.equal(fixture.cancelled, 1);
  const timed = budget(undefined, 1);
  const slow = make(fixture);
  const request = slow.rate("USD", "CNY", false, timed.signal);
  const rejected = assert.rejects(request, (error: unknown) => error instanceof ReadError && error.code === "slow");
  await until(async () => timed.signal.aborted, aborted => aborted);
  await rejected; timed.dispose();
});

test("semantically invalid 200 rates do not poison cache or overwrite stale valid rate", async () => {
  // Invalid 200 response on cold cache must neither cache nor report ok.
  const badFixture = new Fixture();
  badFixture.put(ratePath, [{ date: "2026-11-10", base: "USD", quote: "CNY", rate: "0.00" }]);
  const badService = make(badFixture);
  await assert.rejects(badService.rate("USD", "CNY"), (error: unknown) => error instanceof ReadError && error.code === "bad_answer");
  assert.equal(badService.cacheStats().entries, 0);
  assert.equal(badService.status().state, "error");

  // Invalid 200 response on refresh must preserve valid cached rate as stale.
  const fixture = new Fixture(), service = make(fixture);
  await service.rate("USD", "CNY");
  fixture.put(ratePath, [{ date: "2026-11-10", base: "USD", quote: "CNY", rate: 0 }]);
  const stale = await service.rate("USD", "CNY", true);
  assert.equal(stale.rate, "6.6703");
  assert.equal(stale.stale?.code, "bad_answer");
  assert.equal((await service.rate("USD", "CNY")).stale, undefined);
});

test("timeouts fall back to stale cache, HTTP status classifies first, not_found skips status, and oversized refresh evicts cache", async () => {
  // Internal request timeout surfaces ReadError(slow) and falls back to stale cache.
  const slowFixture = new Fixture();
  const slowService = make(slowFixture, { limits: { gap: 0, request: 20 } });
  await slowService.rate("USD", "CNY");
  slowFixture.put(ratePath, response, { delay: 500 });
  const stale = await slowService.rate("USD", "CNY", true);
  assert.equal(stale.rate, "6.6703");
  assert.equal(stale.stale?.code, "slow");
  assert.equal(slowService.status().error, "The exchange-rate service took too long to respond.");

  // Bodyless 404 classifies as not_found and does not pollute status().
  const nfService = new CurrencyService(async () => new Response(null, { status: 404 }), {}, { limits: { gap: 0 }, locale: "en" });
  await assert.rejects(nfService.rate("USD", "CNY"), (error: unknown) => error instanceof ReadError && error.code === "not_found");
  assert.equal(nfService.status().state, "ok");

  // Non-UTF-8 503 classifies as http(503) and formats {0} in status().error.
  const errService = new CurrencyService(async () => new Response(new Uint8Array([0xff, 0xfe]), { status: 503 }), {}, { limits: { gap: 0 }, locale: "en" });
  await assert.rejects(errService.rate("USD", "CNY"), (error: unknown) => error instanceof ReadError && error.code === "http" && error.status === 503);
  assert.equal(errService.status().error, "The exchange-rate service returned HTTP 503.");

  // Oversized refresh evicts old cached entry.
  const evictFixture = new Fixture();
  const baseBytes = new TextEncoder().encode(JSON.stringify(response)).byteLength;
  const evictService = make(evictFixture, { limits: { gap: 0, cacheBytes: baseBytes + 20 } });
  await evictService.rate("USD", "CNY");
  assert.equal(evictService.cacheStats().entries, 1);
  evictFixture.put(ratePath, [{ date: "2026-11-10", base: "USD", quote: "CNY", rate: 6.6703, extra: "x".repeat(baseBytes + 50) }]);
  await evictService.rate("USD", "CNY", true);
  assert.equal(evictService.cacheStats().entries, 0);

  // Unknown tool must return unknown_method even with invalid args.
  await assert.rejects(callTool(slowService, "unknown", { amount: {} }), /unknown method/);
});

test("assistant tools retain the date and perform unit conversion without network", async () => {
  const fixture = new Fixture(), service = make(fixture);
  assert.match(await callTool(service, "currency_convert", { amount: "100", from: "USD", to: "CNY" }), /100 USD = 667.03 CNY[\s\S]*2026-11-10[\s\S]*reference rates/);
  const hits = fixture.requests.length;
  assert.equal(await callTool(service, "unit_convert", { amount: "100", from: "C", to: "F" }), "100 °C = 212 °F\nLocal conversion · no network request");
  assert.equal(fixture.requests.length, hits);
  for (const args of [{ amount: "-1", from: "USD", to: "CNY" }, { amount: "1", from: "BTC", to: "CNY" }, { amount: "1e3", from: "USD", to: "CNY" }, { amount: "1", from: "USD", to: 2 }]) await assert.rejects(callTool(service, "currency_convert", args), ReadError);
  await assert.rejects(callTool(service, "unknown"), /unknown method/);
});
