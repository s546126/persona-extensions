import { test } from "node:test";
import assert from "node:assert/strict";
import { clip, parseImages, plain, queryText, ReadError, searchPath } from "../src/model.ts";
import { readTime } from "../src/i18n.ts";
import { response } from "./fixtures.ts";
import { encodeState, readState, transition } from "../src/page.tsx";

test("image search validates query and keeps only explicit license metadata", () => {
  const result = parseImages(response, 1000);
  assert.equal(result.images[0].title, "File:Tour Eiffel Wikimedia Commons.jpg");
  assert.equal(result.images[0].description, "The Eiffel Tower in Paris.");
  assert.equal(result.images[0].license, "Public domain");
  assert.equal(result.images[0].pageURL, "https://commons.wikimedia.org/wiki/File:Tour_Eiffel_Wikimedia_Commons.jpg");
  assert.equal(result.images[0].thumbnailURL, "https://upload.wikimedia.org/wikipedia/commons/thumb/a/a8/Tour_Eiffel_Wikimedia_Commons.jpg/640px-Tour_Eiffel_Wikimedia_Commons.jpg");
  assert.equal(result.images[0].width, 640);
  const original = response.query.pages[6926930].imageinfo[0];
  assert.throws(() => parseImages({ query: { pages: { 6926930: { ...response.query.pages[6926930], imageinfo: [{ ...original, thumburl: "https://attacker.invalid/image.jpg" }] } } } }, 0), ReadError);
  assert.throws(() => parseImages({ query: { pages: { 6926930: { ...response.query.pages[6926930], imageinfo: [{ ...original, descriptionurl: "https://attacker.invalid/wiki/File:x" }] } } } }, 0), ReadError);
  assert.throws(() => parseImages({ query: { pages: { 6926930: { ...response.query.pages[6926930], imageinfo: [{ ...original, thumburl: "https://upload.wikimedia.org/wikipedia/commons/" + "a".repeat(4100) + ".jpg" }] } } } }, 0), ReadError);
  const officialThumb = structuredClone(response);
  officialThumb.query.pages[6926930].imageinfo[0].thumburl = "https://thumb.wikimedia.org/wikipedia/commons/thumb/a/a8/Tour_Eiffel_Wikimedia_Commons.jpg/640px-Tour_Eiffel_Wikimedia_Commons.jpg";
  assert.equal(parseImages(officialThumb, 0).images[0].thumbnailURL?.startsWith("https://thumb.wikimedia.org/"), true);
  const noLicense = structuredClone(response);
  Reflect.deleteProperty(noLicense.query.pages[6926930].imageinfo[0].extmetadata, "LicenseShortName");
  assert.equal(parseImages(noLicense, 0).images[0].license, "See file page");

  // MediaWiki omits query on zero-result searches, returning {"batchcomplete":""}.
  assert.deepEqual(parseImages({ batchcomplete: "" }, 42), { images: [], at: 42 });
  assert.throws(() => parseImages({}, 0), ReadError);
  assert.throws(() => parseImages({ error: { code: "badquery" } }, 0), ReadError);

  // Fallback page URL percent-encodes ? and % instead of truncating the filename as a query string.
  const noDescURL = structuredClone(response);
  noDescURL.query.pages[6926930].title = "File:What? 100% pure.jpg";
  Reflect.deleteProperty(noDescURL.query.pages[6926930].imageinfo[0], "descriptionurl");
  assert.equal(parseImages(noDescURL, 0).images[0].pageURL, "https://commons.wikimedia.org/wiki/File:What%3F_100%25_pure.jpg");

  // Duplicate or zero pageid values are rejected.
  const page = response.query.pages[6926930];
  assert.throws(() => parseImages({ query: { pages: { 1: { ...page, pageid: 7 }, 2: { ...page, pageid: 7 } } } }, 0), ReadError);
  assert.throws(() => parseImages({ query: { pages: { 1: { ...page, pageid: 0 } } } }, 0), ReadError);
});

test("a file's http or unreadable license link is upgraded or left out, not a failed search", () => {
  const page = response.query.pages[6926930], info = page.imageinfo[0];
  const withLicense = (value: string, id: number) => ({ ...page, pageid: id, title: `File:Example ${id}.jpg`,
    imageinfo: [{ ...info, descriptionurl: `https://commons.wikimedia.org/wiki/File:Example_${id}.jpg`, extmetadata: { ...info.extmetadata, LicenseShortName: { value: "GFDL" }, LicenseUrl: { value } } }] });
  const result = parseImages({ query: { pages: {
    1: withLicense("http://www.gnu.org/copyleft/fdl.html", 1), // as File:Domestic cat felis catus.jpg links it
    2: withLicense("http://creativecommons.org/licenses/by-sa/3.0/", 2),
    3: withLicense("http://example.org/license", 3),
    4: withLicense("not a link", 4),
    5: withLicense("https://user:secret@creativecommons.org/licenses/by/4.0/", 5),
  } } }, 0);
  assert.deepEqual(result.images.map(image => image.licenseURL), ["https://www.gnu.org/copyleft/fdl.html", "https://creativecommons.org/licenses/by-sa/3.0/", undefined, undefined, undefined]);
  assert.deepEqual(result.images.map(image => image.license), ["GFDL", "GFDL", "GFDL", "GFDL", "GFDL"]);
});

test("clip enforces exact code-point limits and readTime tolerates invalid timezones", () => {
  assert.equal(Array.from(clip("a".repeat(20), 10)).length, 10);
  assert.equal(clip("a".repeat(20), 10), "aaaaaaaaa…");
  assert.equal(Array.from(clip("😀".repeat(10), 5)).length, 5);
  assert.equal(clip("😀".repeat(10), 5), "😀😀😀😀…");
  assert.equal(clip("hello", 1), "h");
  assert.equal(clip("hello", 0), "");
  assert.equal(clip("hello", -5), "");
  assert.equal(clip("hello", Number.NaN), "");
  assert.equal(Array.from(clip("hello", 3.9)).length, 3);
  assert.ok(readTime(Date.UTC(2026, 10, 10), "", "en").length > 0);
  assert.ok(readTime(Date.UTC(2026, 10, 10), "Invalid/Zone", "zh-Hans").includes("2026"));
  assert.ok(readTime(Date.UTC(2026, 10, 10), "UTC", "").length > 0);
  assert.ok(readTime(Date.UTC(2026, 10, 10), "UTC", "invalid_locale_tag!!!").includes("2026"));
  assert.equal(readTime(Number.NaN, "UTC", "en"), "");
  assert.equal(readTime(Number.POSITIVE_INFINITY, "UTC", "en"), "");
});

test("HTML metadata stays plain data and URLs cannot choose another host", () => {
  assert.equal(plain(" <b>Hello</b>&nbsp; world &unknown; &#x1F600; "), "Hello world &unknown; 😀");
  assert.equal(plain("unterminated <tag"), "unterminated");
  assert.equal(plain("&#0; &#xD800;"), "\uFFFD \uFFFD");
  assert.equal(plain("a\uD800b"), "ab");
  for (const query of ["", " ", "x".repeat(161), "bad\n", "\uD800", "\uDC00", "a\uD800b"]) assert.throws(() => searchPath(query), ReadError);
  assert.throws(() => searchPath("valid", 0), ReadError);
  assert.throws(() => searchPath("valid", 21), ReadError);
  assert.equal(queryText(" Eiffel Tower "), "Eiffel Tower");
  assert.equal(queryText("😀"), "😀");
});

test("state and legacy callback values preserve validated searches", () => {
  const initial = readState(undefined);
  const found = transition(initial, "search", '{"query":"Eiffel Tower"}');
  assert.equal(found.found, true);
  assert.equal(readState(encodeState(found)).q, "Eiffel Tower");
  for (const raw of [JSON.stringify({ q: "x".repeat(161), found: true }), JSON.stringify({ q: "bad\n", found: true }), JSON.stringify({ q: "\uD800", found: true })]) {
    assert.deepEqual(readState(raw), initial);
  }
  for (const values of ["Eiffel", "not-json", "null", "[]", 1, { query: ["Eiffel"] }, { query: "x".repeat(161) }, { query: "bad\n" }, { query: "\uD800" }]) {
    assert.equal(transition(initial, "search", values).found, false);
  }
  assert.equal(transition(found, "home").found, false);
});
