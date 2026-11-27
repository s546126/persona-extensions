import { test } from "node:test";
import assert from "node:assert/strict";
import { budget, Domains, ReadError } from "../src/service.ts";
import { callTool } from "../src/tools.ts";
import { bootstrap, bootstrapURL, Fixture, registration, registryURL, until } from "./fixtures.ts";
const make = (fixture = new Fixture(), options = {}) => new Domains(fixture.fetch, {}, { now: () => fixture.clock, monotonic: () => fixture.clock, limits: { gap: 0 }, locale: "en", ...options });
test("bootstrap and registration reads share validated bounded cache", async () => {
  const fixture = new Fixture(), service = make(fixture);
  const result = await service.lookup("EXAMPLE.com");
  assert.equal(result.registrar, "Example Registrar");
  await service.lookup("example.com");
  assert.equal(fixture.hits.get(bootstrapURL), 1);
  assert.equal(fixture.hits.get(registryURL), 1);
  assert.equal(fixture.requests[0].init.redirect, "error");
  assert.equal(fixture.requests[0].init.headers && (fixture.requests[0].init.headers as Record<string, string>).Authorization, undefined);
  assert.equal(service.status().state, "ok");
});
test("transient failure serves a clearly marked old validated record", async () => {
  const fixture = new Fixture(), service = make(fixture);
  await service.lookup("example.com");
  fixture.put(registryURL, {}, { status: 503 });
  const result = await service.lookup("example.com", true);
  assert.equal(result.registrar, "Example Registrar");
  assert.equal(result.stale?.code, "http");
});
test("a semantically invalid 200 answer does not replace a validated cached record", async () => {
  const fixture = new Fixture(), service = make(fixture);
  await service.lookup("example.com");
  fixture.put(registryURL, {});
  const refreshed = await service.lookup("example.com", true);
  assert.equal(refreshed.registrar, "Example Registrar");
  assert.equal(refreshed.stale?.code, "bad_answer");
  // The invalid answer must not have replaced the valid record: a later lookup still reads it.
  const result = await service.lookup("example.com");
  assert.equal(result.registrar, "Example Registrar");
  assert.equal(result.stale, undefined);
});

test("malformed, oversized and rate-limited responses are never cached", async () => {
  for (const response of [{ body: "{bad" }, { body: "null" }]) {
    const fixture = new Fixture(); fixture.answers.set(registryURL, response);
    await assert.rejects(make(fixture).lookup("example.com"), ReadError);
  }
  const fixture = new Fixture(); fixture.answers.set(registryURL, { body: " ".repeat(1 << 20) });
  await assert.rejects(make(fixture).lookup("example.com"), (error: unknown) => error instanceof ReadError && error.code === "bad_answer");
  const limited = new Fixture(); limited.put(registryURL, {}, { status: 429 });
  await assert.rejects(make(limited).lookup("example.com"), (error: unknown) => error instanceof ReadError && error.code === "rate_limited");
});
test("explicit cancellation aborts a pending registry response", async () => {
  const fixture = new Fixture(); fixture.put(registryURL, registration, { delay: 1000 });
  const service = make(fixture), controller = new AbortController();
  const pending = service.lookup("example.com", false, controller.signal);
  await until(async () => fixture.requests.length, length => length > 1);
  controller.abort();
  await assert.rejects(pending, (error: unknown) => error instanceof Error);
  assert.ok(fixture.cancelled >= 1);
});
test("a request deadline aborts the fetch and carries the timeout reason", async () => {
  const service = new Domains(async (_input, init) => {
    const signal = init?.signal;
    if (!signal) throw new Error("request signal missing");
    await new Promise<void>((_resolve, reject) => {
      if (signal.aborted) reject(signal.reason);
      else signal.addEventListener("abort", () => reject(signal.reason), { once: true });
    });
    throw new Error("aborted response must not resolve");
  }, {}, { limits: { gap: 0, request: 5 } });
  await assert.rejects(service.lookup("example.com"), (error: unknown) => error instanceof ReadError && error.code === "slow");
  assert.equal(service.status().error, "The registry took too long to answer.");
});

test("timeouts fall back to stale cache, HTTP status classifies first, not_found skips status, and oversized refresh evicts cache", async () => {
  // Timeout falls back to stale cache instead of throwing AbortError.
  const slowFixture = new Fixture();
  const slowService = make(slowFixture, { limits: { gap: 0, request: 20 } });
  await slowService.lookup("example.com");
  slowFixture.put(registryURL, registration, { delay: 500 });
  const stale = await slowService.lookup("example.com", true);
  assert.equal(stale.registrar, "Example Registrar");
  assert.equal(stale.stale?.code, "slow");
  assert.equal(slowService.status().error, "The registry took too long to answer.");

  // Bodyless 404 classifies as not_found and does not pollute status().
  const okFixture = new Fixture();
  const okService = make(okFixture);
  await okService.lookup("example.com");
  const notFoundService = new Domains(async (input) => {
    if (String(input) === bootstrapURL) return new Response(JSON.stringify(bootstrap), { status: 200 });
    return new Response(null, { status: 404 });
  }, {}, { now: () => okFixture.clock, monotonic: () => okFixture.clock, limits: { gap: 0 }, locale: "en" });
  await assert.rejects(notFoundService.lookup("missing.com"), (error: unknown) => error instanceof ReadError && error.code === "not_found");
  assert.equal(notFoundService.status().state, "ok");

  // Non-UTF-8 503 body classifies as http(503) and formats {0} in status().error.
  const badUtf8Service = new Domains(async (input) => {
    if (String(input) === bootstrapURL) return new Response(JSON.stringify(bootstrap), { status: 200 });
    return new Response(new Uint8Array([0xff, 0xfe]), { status: 503 });
  }, {}, { now: () => okFixture.clock, monotonic: () => okFixture.clock, limits: { gap: 0 }, locale: "en" });
  await assert.rejects(badUtf8Service.lookup("example.com"), (error: unknown) => error instanceof ReadError && error.code === "http" && error.status === 503);
  assert.equal(badUtf8Service.status().error, "The registry returned HTTP 503.");

  // Oversized refresh evicts old cached entry when cacheBytes is exceeded.
  const evictFixture = new Fixture();
  const smallSize = new TextEncoder().encode(JSON.stringify(registration)).byteLength;
  const bootstrapSize = new TextEncoder().encode(JSON.stringify(bootstrap)).byteLength;
  const evictService = make(evictFixture, { limits: { gap: 0, cacheBytes: bootstrapSize + smallSize + 20 } });
  await evictService.lookup("example.com");
  assert.equal(evictService.cacheStats().entries, 2);
  evictFixture.put(registryURL, {
    ...registration,
    entities: [{ roles: ["registrar"], vcardArray: ["vcard", [["version", {}, "text", "4.0"], ["fn", {}, "text", "R".repeat(smallSize + bootstrapSize + 50)]]] }],
  });
  await evictService.lookup("example.com", true);
  assert.equal(evictService.cacheStats().entries, 1);

  // Unknown tool must return unknown_method even with invalid args.
  await assert.rejects(callTool(okService, "unknown", { domain: 123 }), /unknown method/);
});
test("assistant lookup returns lifecycle metadata and rejects unsafe inputs", async () => {
  const service = make();
  assert.match(await callTool(service, "domains_lookup", { domain: "example.com" }), /Example Registrar[\s\S]*client transfer prohibited[\s\S]*expiration/);
  for (const domain of ["", "https://example.com", "example", "user@example.com", "*.example.com", 42]) await assert.rejects(callTool(service, "domains_lookup", { domain }), ReadError);
  await assert.rejects(callTool(service, "unknown"), /unknown method/);
});
