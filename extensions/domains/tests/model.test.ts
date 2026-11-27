import { test } from "node:test";
import assert from "node:assert/strict";
import { clip, domainName, lookupURL, parseRegistration, rdapService, ReadError, statusURL } from "../src/model.ts";
import { readTime } from "../src/i18n.ts";
import { bootstrap, registration } from "./fixtures.ts";
import { decodeHistory, encodeHistory, MemoryHistory } from "../src/store.ts";
import { Domains } from "../src/service.ts";
import { encodeState, page, readState, transition } from "../src/page.tsx";
import { Renderer } from "@persona/extension/ui";

test("one domain is normalized and IDNs are kept separate from URLs", () => {
  assert.deepEqual(domainName("EXAMPLE.com."), { ascii: "example.com", unicode: "example.com" });
  assert.equal(domainName("例子.中国").ascii, "xn--fsqu00a.xn--fiqs8s");
  assert.equal(domainName("bücher.de").ascii, "xn--bcher-kva.de");
  for (const value of ["", "example", "https://example.com", "example.com/path", "user@example.com", "*.example.com", "127.0.0.1", "::1", "bad_foo.example.com", "bad-.example.com", "bad\n.example.com", "-bad.example.com", "a..com", "example.com."]) {
    if (value === "example.com.") continue;
    assert.throws(() => domainName(value), ReadError, value);
  }
});
test("IANA selects exactly one HTTPS RDAP base without arbitrary redirect", () => {
  const service = rdapService(bootstrap, "example.com");
  assert.equal(service.base, "https://rdap.verisign.com/com/v1/");
  assert.equal(lookupURL(service, "example.com"), "https://rdap.verisign.com/com/v1/domain/example.com");
  assert.throws(() => rdapService(bootstrap, "name.unsupported"), ReadError);
  assert.throws(() => rdapService({ services: [[["com"], ["http://registry.example.com/"]]] }, "example.com"), ReadError);
  assert.throws(() => rdapService({ services: [[["com"], ["https://one.example.com/", "https://two.example.com/"]]] }, "example.com"), ReadError);
  assert.equal(rdapService(bootstrap, "example.net").base, "https://rdap.verisign.com/net/v1/");
  assert.throws(() => rdapService({ services: [[["com"], ["https://user:password@registry.example.com/"]]] }, "example.com"), ReadError);
});
test("public registration records retain dates and statuses without inventing availability", () => {
  const result = parseRegistration(registration, "example.com", 1000);
  assert.equal(result.registrar, "Example Registrar");
  assert.equal(result.statuses.length, 2);
  assert.equal(result.events[1].action, "expiration");
  assert.equal(result.url, "https://rdap.verisign.com/com/v1/domain/example.com");
  assert.equal(statusURL, "https://www.iana.org/assignments/rdap-status-values/rdap-status-values.xhtml");
  assert.throws(() => parseRegistration({ ...registration, ldhName: "attacker.example" }, "example.com", 0), ReadError);
  assert.throws(() => parseRegistration({ ...registration, links: [{ rel: "self", href: "http://localhost/" }] }, "example.com", 0), ReadError);
  assert.throws(() => parseRegistration({ ...registration, events: [{ eventAction: "registration", eventDate: "not-a-date" }] }, "example.com", 0), ReadError);
  // Date.parse would roll this into March; an RDAP date must be a real calendar day.
  assert.throws(() => parseRegistration({ ...registration, events: [{ eventAction: "registration", eventDate: "2026-02-30T00:00:00Z" }] }, "example.com", 0), ReadError);
  for (const raw of [null, [], {}, { objectClassName: "entity" }]) assert.throws(() => parseRegistration(raw, "example.com", 0), ReadError);
});
test("history is bounded, normalized and independent of external accounts", async () => {
  const history = new MemoryHistory();
  await history.add("EXAMPLE.com");
  await history.add("example.com");
  assert.deepEqual((await history.load()).domains, ["example.com"]);
  const stored = decodeHistory(encodeHistory(await history.load()));
  assert.deepEqual(stored, { schema_version: 1, domains: ["example.com"] });
  await history.clear();
  assert.deepEqual((await history.load()).domains, []);
  assert.throws(() => decodeHistory("bad"), ReadError);
});
test("navigation accepts only one valid domain and keeps history reachable", () => {
  const searched = transition(readState(undefined), "search", { domain: "EXAMPLE.com" });
  assert.deepEqual(searched, { v: "result", q: "example.com" });
  assert.equal(readState(encodeState(searched)).v, "result");
  assert.equal(transition(searched, "back").v, "lookup");
  assert.equal(transition(searched, "history").v, "history");
  for (const value of ["", "example", "https://example.com", "bad\n.com", 2, null, { domain: ["example.com"] }]) assert.equal(transition(readState(undefined), "search", value).v, "lookup");
  assert.equal(transition(readState(undefined), "domain:example.com").v, "result");
  assert.equal(transition(readState(undefined), "domain:https://attacker.invalid").v, "lookup");
});

test("RDAP timezone offsets, upstream ldhName errors, clip, readTime, and history normalization handle edge cases", () => {
  // Valid RFC 3339 timestamp with non-zero offset crossing UTC midnight must be accepted.
  const tzResult = parseRegistration({
    ...registration,
    events: [{ eventAction: "registration", eventDate: "2026-01-01T23:30:00-05:00" }],
  }, "example.com", 1000);
  assert.equal(tzResult.events[0].at, "2026-01-02T04:30:00.000Z");

  // Impossible local calendar day with offset must still fail.
  assert.throws(
    () => parseRegistration({ ...registration, events: [{ eventAction: "registration", eventDate: "2026-02-30T23:30:00-05:00" }] }, "example.com", 0),
    (error: unknown) => error instanceof ReadError && error.code === "bad_answer",
  );

  // Malformed upstream ldhName must report bad_answer, not user-input invalid.
  assert.throws(
    () => parseRegistration({ ...registration, ldhName: "not a domain!!" }, "example.com", 0),
    (error: unknown) => error instanceof ReadError && error.code === "bad_answer",
  );

  // Overlong self URL (> 4096 chars) must be rejected as bad_answer.
  assert.throws(
    () => parseRegistration({ ...registration, links: [{ rel: "self", href: "https://rdap.verisign.com/com/v1/domain/" + "a".repeat(4100) }] }, "example.com", 0),
    (error: unknown) => error instanceof ReadError && error.code === "bad_answer",
  );

  // clip must strictly respect rune budget and non-positive bounds.
  assert.equal(clip("abcdef", 0), "");
  assert.equal(clip("abcdef", 1), "a");
  assert.equal(clip("abcdef", 4), "abc…");
  assert.equal(Array.from(clip("😀😀😀😀😀", 4)).length, 4);

  // Lone surrogates must be rejected in domainName.
  assert.throws(() => domainName("bad\ud800.com"), (error: unknown) => error instanceof ReadError && error.code === "invalid");

  // readTime must never throw on invalid timezone, locale, or timestamp.
  assert.doesNotThrow(() => readTime(Date.UTC(2026, 0, 2), "Not/A_Zone", "bad_locale_!!!"));
  assert.equal(readTime(Number.NaN, "UTC", "en"), "");

  // Case-duplicate domains in encoded history must be rejected after ASCII normalization.
  const dup = Buffer.from(JSON.stringify({ schema_version: 1, domains: ["EXAMPLE.com", "example.com"] }), "utf8").toString("base64");
  assert.throws(() => decodeHistory(dup), (error: unknown) => error instanceof ReadError && error.code === "storage");
});

test("page bounds 253-char domain keys, action IDs, titles and CheckView limits", () => {
  const longDomain = `${"a".repeat(60)}.${"b".repeat(60)}.${"c".repeat(60)}.${"d".repeat(60)}.com`; // 247 chars
  const service = new Domains(async () => new Response("{}", { status: 200 }));
  const history = new MemoryHistory();
  const render = (node: ReturnType<typeof page>) => {
    const renderer = new Renderer({ authorize: () => false });
    try { return renderer.render(node).view; }
    finally { renderer.dispose(); }
  };

  // History view with 247-char domain must keep keys and action IDs <= 128 chars.
  const histView = render(page({ service, history, state: { v: "history", q: "" }, loaded: { kind: "history", domains: [longDomain, longDomain] }, locale: "en", uiVersion: 5 }));
  const histItems = histView.sections.flatMap(s => s.items);
  const seenKeys = new Set<string>();
  for (const item of histItems) {
    if (typeof item.key === "string") {
      assert.ok(item.key.length > 0 && item.key.length <= 128, `key too long: ${item.key.length}`);
      assert.equal(seenKeys.has(item.key), false, `duplicate key: ${item.key}`);
      seenKeys.add(item.key);
    }
    if ("action" in item && item.action) {
      assert.ok(item.action.id.length <= 128, `action.id too long: ${item.action.id.length}`);
    }
  }

  // Result view with 247-char domain must keep View.title <= 200 chars and Section.title <= 200 chars.
  const resultView = render(page({
    service,
    history,
    state: { v: "result", q: longDomain },
    loaded: {
      kind: "result",
      result: {
        domain: { ascii: longDomain, unicode: longDomain },
        name: longDomain,
        statuses: ["ok"],
        events: [{ action: "registration", at: "2026-01-01T00:00:00.000Z" }],
        url: "https://rdap.verisign.com/com/v1/domain/example.com",
        source: "https://rdap.verisign.com",
        at: Date.UTC(2026, 0, 2),
      },
    },
    locale: "en",
  }));
  assert.ok(Array.from(resultView.title ?? "").length <= 200, `view title length ${Array.from(resultView.title ?? "").length} > 200`);
  for (const section of resultView.sections) {
    assert.ok(section.items.length > 0, `empty section: ${section.key}`);
    if (section.title) assert.ok(Array.from(section.title).length <= 200, `section title too long: ${section.title.length}`);
  }

  // Invalid direct state passed to page() must sanitize safely.
  const badView = render(page({ service, history, state: { v: "result", q: "not-a-domain" }, loaded: { kind: "empty" }, locale: "en" }));
  assert.equal(JSON.parse(badView.state ?? "{}").v, "lookup");
});
