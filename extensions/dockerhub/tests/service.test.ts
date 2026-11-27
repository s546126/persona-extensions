import { test } from "node:test";
import assert from "node:assert/strict";
import { ExactJSON } from "@persona/extension";
import { budget, DockerHub, ReadError } from "../src/service.ts";
import { callTool } from "../src/tools.ts";
import { detailPath, detailResponse, Fixture, nginx, searchPathFixture, searchResponse, tagPath, until } from "./fixtures.ts";
const make = (fixture = new Fixture(), options = {}) => new DockerHub(fixture.fetch, {}, { now: () => fixture.clock, monotonic: () => fixture.clock, limits: { gap: 0 }, locale: "en", ...options });

test("anonymous Docker Hub search and detail use validated bounded requests", async () => {
  const fixture = new Fixture(), service = make(fixture);
  assert.equal((await service.search("nginx")).images[0].name, "library/nginx");
  assert.equal((await service.repository("library/nginx")).image.pull, "docker pull library/nginx");
  assert.equal((await service.search("nginx", 1)).images.length, 1);
  await service.search("nginx");
  assert.equal(fixture.hits.get(detailPath), 1);
  assert.equal(fixture.hits.get(tagPath), 1);
  for (const request of fixture.requests) {
    assert.equal(request.init.method, "GET");
    assert.equal(request.init.redirect, "error");
    assert.equal(request.url.searchParams.has("access_token"), false);
    assert.match((request.init.headers as Record<string, string>)["User-Agent"], /persona-dockerhub/);
  }
  assert.equal(service.status().state, "ok");
});

test("cache freshness expires from save time and stale fallback does not promote expired data back to fresh", async () => {
  const fixture = new Fixture(), service = make(fixture);
  await service.search("nginx");
  assert.equal(fixture.hits.get(searchPathFixture), 1);
  // Repeated cache hits within TTL must NOT extend freshness indefinitely.
  fixture.clock += 10 * 60_000;
  await service.search("nginx");
  assert.equal(fixture.hits.get(searchPathFixture), 1);
  fixture.clock += 6 * 60_000; // 16 min after initial save (> 15 min fresh)
  await service.search("nginx");
  assert.equal(fixture.hits.get(searchPathFixture), 2);

  // Failed refresh returning stale data must NOT mark the stale entry fresh again.
  fixture.put(searchPathFixture, {}, { status: 503 });
  fixture.clock += 16 * 60_000;
  const stale1 = await service.search("nginx");
  assert.equal(stale1.stale?.code, "http");
  assert.equal(fixture.hits.get(searchPathFixture), 3);
  // Subsequent read with refresh=false must still hit upstream once recovered.
  fixture.put(searchPathFixture, searchResponse);
  fixture.clock += 1000;
  const recovered = await service.search("nginx");
  assert.equal(recovered.stale, undefined);
  assert.equal(fixture.hits.get(searchPathFixture), 4);
});

test("transient failures return only previously validated stale data", async () => {
  const fixture = new Fixture(), service = make(fixture);
  await service.search("nginx");
  fixture.put(searchPathFixture, {}, { status: 503 });
  fixture.clock += 16 * 60_000;
  const result = await service.search("nginx", 12, true);
  assert.equal(result.images[0].name, "library/nginx");
  assert.equal(result.stale?.code, "http");
  assert.equal(service.status().state, "error");
  assert.equal(service.status().error, "Docker Hub returned HTTP 503.");
});

test("malformed responses never enter the stale cache", async () => {
  const fixture = new Fixture(); fixture.put(detailPath, { ...detailResponse, namespace: "attacker" });
  const service = make(fixture);
  await assert.rejects(service.repository("library/nginx"), ReadError);
  assert.equal(service.cacheStats().entries, 0);
  fixture.put(detailPath, {}, { status: 503 });
  await assert.rejects(service.repository("library/nginx"), ReadError);
  assert.equal(service.cacheStats().entries, 0);
});

test("oversized, malformed and rate-limited responses fail closed", async () => {
  for (const response of [{ body: "{bad" }, { body: "null" }]) {
    const fixture = new Fixture(); fixture.responses.set(searchPathFixture, response);
    await assert.rejects(make(fixture).search("nginx"), ReadError);
  }
  const oversized = new Fixture(); oversized.responses.set(searchPathFixture, { body: " ".repeat((1 << 20) + 1) });
  await assert.rejects(make(oversized).search("nginx"), (error: unknown) => error instanceof ReadError && error.code === "bad_answer");

  // Oversized stream whose cancel() rejects must still classify as bad_answer, not network.
  const failingCancelFetch = (async () => new Response(new ReadableStream<Uint8Array>({
    pull(controller) { controller.enqueue(new Uint8Array(64)); },
    cancel() { return Promise.reject(new Error("cancel failed")); },
  }), { status: 200 })) as unknown as Fixture["fetch"];
  await assert.rejects(new DockerHub(failingCancelFetch, {}, { limits: { gap: 0, response: 32 }, locale: "en" }).search("nginx"), (error: unknown) => error instanceof ReadError && error.code === "bad_answer");

  const limited = new Fixture(); limited.put(searchPathFixture, {}, { status: 429 });
  await assert.rejects(make(limited).search("nginx"), (error: unknown) => error instanceof ReadError && error.code === "rate_limited");

  // Non-200 response with invalid UTF-8 body classifies by HTTP status, not bad_answer.
  const invalidUtf8Error = new DockerHub((async () => new Response(new Uint8Array([0xff, 0xfe]), { status: 503 })) as unknown as Fixture["fetch"], {}, { limits: { gap: 0 }, locale: "en" });
  await assert.rejects(invalidUtf8Error.search("nginx"), (error: unknown) => error instanceof ReadError && error.code === "http" && error.status === 503);

  // Bodyless 404 classifies as not_found without poisoning connector status.
  const bodyless404 = new DockerHub((async () => new Response(null, { status: 404 })) as unknown as Fixture["fetch"], {}, { limits: { gap: 0 }, locale: "en" });
  await assert.rejects(bodyless404.repository("library/nginx"), (error: unknown) => error instanceof ReadError && error.code === "not_found");
  assert.equal(bodyless404.status().state, "ok");

  // Refreshing with a valid payload exceeding cacheBytes evicts the outdated cached entry.
  const evictFixture = new Fixture();
  const smallResp = { count: 1, results: [{ ...nginx, short_description: "short" }] };
  const hugeResp = { count: 1, results: [{ ...nginx, short_description: "z".repeat(450) }] };
  evictFixture.put(searchPathFixture, smallResp);
  const smallBytes = new TextEncoder().encode(JSON.stringify(smallResp)).byteLength;
  const evictService = make(evictFixture, { limits: { gap: 0, cacheBytes: smallBytes + 20 } });
  await evictService.search("nginx");
  assert.equal(evictService.cacheStats().entries, 1);
  evictFixture.put(searchPathFixture, hugeResp);
  const refreshedHuge = await evictService.search("nginx", 12, true);
  assert.equal(refreshedHuge.images[0].description, "z".repeat(450));
  assert.equal(evictService.cacheStats().entries, 0);
});

test("cancellation, deadlines, stream aborts, and queued aborts propagate cleanly", async () => {
  const fixture = new Fixture(); fixture.put(searchPathFixture, { count: 0, results: [] }, { delay: 1000 });
  const service = make(fixture), controller = new AbortController();
  const pending = service.search("nginx", 12, false, controller.signal);
  await until(async () => fixture.requests.length, length => length > 0);
  controller.abort();
  await assert.rejects(pending, (error: unknown) => error instanceof Error && error.name === "AbortError");
  assert.equal(fixture.cancelled, 1);

  const timed = budget(undefined, 1), slow = make(fixture);
  const request = slow.search("nginx", 12, false, timed.signal);
  const rejected = assert.rejects(request, (error: unknown) => error instanceof ReadError && error.code === "slow");
  await until(async () => timed.signal.aborted, aborted => aborted);
  await rejected; timed.dispose();
  assert.equal(slow.status().state, "error");

  // Internal request timeout classifies as ReadError("slow") and falls back to stale cache.
  const staleFixture = new Fixture();
  const staleService = make(staleFixture, { limits: { gap: 0, request: 25 } });
  await staleService.search("nginx");
  staleFixture.put(searchPathFixture, searchResponse, { delay: 500 });
  const staleSlow = await staleService.search("nginx", 12, true);
  assert.equal(staleSlow.stale?.code, "slow");
  assert.equal(staleService.status().state, "error");

  // Stream body abort on timeout classifies as ReadError("slow") while preserving caller AbortError.
  const streamFetch = (async (_input: string, init: RequestInit) => {
    const signal = init.signal!;
    const body = new ReadableStream<Uint8Array>({
      start(ctrl) { signal.addEventListener("abort", () => ctrl.error(new DOMException("aborted", "AbortError")), { once: true }); },
    });
    return new Response(body, { status: 200 });
  }) as unknown as Fixture["fetch"];
  await assert.rejects(new DockerHub(streamFetch, {}, { limits: { gap: 0, request: 20 }, locale: "en" }).search("nginx"), (error: unknown) => error instanceof ReadError && error.code === "slow");

  const cancelController = new AbortController();
  const cancelPending = new DockerHub(streamFetch, {}, { limits: { gap: 0, request: 5000 }, locale: "en" }).search("nginx", 12, false, cancelController.signal);
  setTimeout(() => cancelController.abort(), 15);
  await assert.rejects(cancelPending, (error: unknown) => error instanceof Error && error.name === "AbortError");

  // Queued caller aborted while waiting behind an earlier slow request must reject with AbortError.
  const qFixture = new Fixture();
  qFixture.put(searchPathFixture, searchResponse, { delay: 40 });
  const qService = make(qFixture);
  const first = qService.search("nginx");
  const qCtrl = new AbortController();
  const second = qService.search("nginx", 12, false, qCtrl.signal);
  qCtrl.abort();
  await assert.rejects(second, (error: unknown) => error instanceof Error && error.name === "AbortError");
  await first;
});

test("assistant tools preserve official status, validate ExactJSON limits, normalize timeouts, and reject invalid input", async () => {
  const service = make();
  assert.match(await callTool(service, "dockerhub_search", { query: "nginx" }), /library\/nginx[\s\S]*Official image[\s\S]*hub.docker.com\/r\/library\/nginx/);
  assert.match(await callTool(service, "dockerhub_search", { query: "nginx" }, undefined, ExactJSON.parse('{"query":"nginx"}')), /library\/nginx/);
  assert.match(await callTool(service, "dockerhub_search", { query: "nginx", limit: null }, undefined, ExactJSON.parse('{"query":"nginx","limit":null}')), /library\/nginx/);
  assert.match(await callTool(service, "dockerhub_repository", { name: "library/nginx" }), /docker pull library\/nginx[\s\S]*sha256:e2355aa[\s\S]*linux\/arm64\/v8/);
  for (const raw of ['{"query":"nginx","limit":0}', '{"query":"nginx","limit":21}', '{"query":"nginx","limit":1.5}', '{"query":"nginx","limit":1e1}', '{"query":"nginx","limit":"5"}', '{"query":"nginx","limit":9223372036854775808}', '{"query":"nginx","limit":9223372036854775807}', '{"query":null}', '[]']) {
    await assert.rejects(callTool(service, "dockerhub_search", JSON.parse(raw), undefined, ExactJSON.parse(raw)), (error: unknown) => error instanceof ReadError && error.code === "invalid");
  }
  for (const args of [{ query: "" }, { query: "x".repeat(101) }, { query: "\uD800" }, { query: "ok", limit: 0 }, { query: "ok", limit: 21 }, { query: "ok", limit: 1.5 }, { query: 2 }]) await assert.rejects(callTool(service, "dockerhub_search", args), ReadError);
  for (const name of ["../secret", "nginx", "a\n/b"]) await assert.rejects(callTool(service, "dockerhub_repository", { name }), ReadError);
  await assert.rejects(callTool(service, "unknown"), /unknown method/);
  await assert.rejects(callTool(service, "unknown", [], undefined, ExactJSON.parse("[]")), /unknown method/);

  const slowFixture = new Fixture();
  slowFixture.put("/v2/search/repositories/?query=nginx&page_size=10", searchResponse, { delay: 500 });
  const slowService = make(slowFixture, { limits: { gap: 0, tool: 20, request: 5000 } });
  await assert.rejects(callTool(slowService, "dockerhub_search", { query: "nginx" }), (error: unknown) => error instanceof ReadError && error.code === "slow");
});
