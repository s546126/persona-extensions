import { test } from "node:test";
import assert from "node:assert/strict";
import { answerMarkdown, answersPath, clip, clipMarkdown, decodeEntities, parseAnswers, parseQuestions, queryText, questionID, ReadError, searchPath, selectedAnswer, siteID, tagText, titleText } from "../src/model.ts";
import { StackExchange } from "../src/service.ts";
import { encodeState, initialState, page, questionTransition, readState, transition } from "../src/page.tsx";
import { Renderer } from "@persona/extension/ui";
import { answer, question, questionPath } from "./fixtures.ts";

test("bounded public search and answer paths validate all user-controlled fields", () => {
  const path = searchPath(" TypeScript & JSX ", "stackoverflow", "c#", 12);
  const url = new URL("https://api.stackexchange.com" + path);
  assert.equal(url.searchParams.get("q"), "TypeScript & JSX");
  assert.equal(url.searchParams.get("tagged"), "c#");
  assert.equal(url.searchParams.get("filter"), "!*10eWkBRxImNF3.k5tm)KG1");
  assert.equal(new URL("https://api.stackexchange.com" + answersPath(70794241, "unix")).searchParams.get("site"), "unix");
  for (const value of ["", "x\n", 42, "x".repeat(201)]) assert.throws(() => queryText(value), ReadError);
  for (const value of ["__proto__", "meta", "stackoverflow.com", 42]) assert.throws(() => siteID(value), ReadError);
  for (const value of ["typescript;foo", "c++ ", "x".repeat(61), "-tag"]) assert.throws(() => tagText(value), ReadError);
  for (const value of [0, -1, 1.5, 1e20, "1\n", {}]) assert.throws(() => questionID(value), ReadError);
  assert.equal(questionPath.includes("access_token"), false);
});

test("question parsing decodes entities and keeps only validated same-site links", () => {
  const result = parseQuestions({ items: [{ ...question, title: "C&amp;C &quot;generals&quot;", owner: { display_name: "A &lt;B&gt;" } }], quota_remaining: 17 }, "stackoverflow", 123);
  assert.equal(result.questions[0].title, 'C&C "generals"');
  assert.equal(result.questions[0].author, "A <B>");
  assert.equal(result.quotaRemaining, 17);
  assert.equal(result.at, 123);
  for (const change of [
    { link: "https://attacker.invalid/questions/70794241" },
    { link: "https://stackoverflow.com.evil.invalid/questions/70794241" },
    { question_id: -1 },
    { tags: ["typescript", "typescript"] },
    { content_license: "public domain" },
    { owner: { display_name: "Name", link: "https://attacker.invalid/users/123" } }
  ]) assert.throws(() => parseQuestions({ items: [{ ...question, ...change }] }, "stackoverflow", 0), ReadError);
  assert.throws(() => parseQuestions({ items: [question, question] }, "stackoverflow", 0), ReadError);
});

test("answer HTML becomes safe Markdown with code, entities and HTTPS references", () => {
  const text = answerMarkdown(answer.body);
  assert.match(text, /Use `<T>` & a spread/);
  assert.match(text, /```\nconst value = \{ \.\.\.input \};\nconsole\.log\(value\);\n```/);
  assert.match(text, /\[TypeScript docs\]\(https:\/\/www\.typescriptlang\.org\/docs\/\) > guide/);
  assert.doesNotMatch(text, /<(?:script|pre|code|a)\b/i);
  assert.equal(decodeEntities("&#x1F600; &#65; &unknown;"), "😀 A &unknown;");
  assert.match(answerMarkdown('<script>alert(1)</script><p>Safe</p>'), /Safe/);
  assert.doesNotMatch(answerMarkdown('<script>alert(1)</script><p>Safe</p>'), /alert/);
  assert.equal(answerMarkdown('<a href="javascript:alert(1)">label</a>'), "label");
  const code = answerMarkdown('<pre><code>```\n[bad](javascript:alert(1))\n```</code></pre>');
  assert.equal(code, "```\n'''\n[bad](javascript:alert(1))\n'''\n```");
  assertValidWireMarkdown(code);
  assert.doesNotMatch(answerMarkdown('<a href="https://example.com"><strong>nested</strong></a>'), /<a|<strong/);
  assert.equal(answerMarkdown('<a href="https://example.com">[x](bad)</a>'), '[\\[x\\]\\(bad)](https://example.com/)');
  assertValidWireMarkdown(answerMarkdown('<a href="https://example.com">[x](bad)</a>'));
});

// Faithful mirror of sdk/go/wire/viewcheck.go:381-413 (including ASCII RE2 \s and Go url.Parse %-escape rejection).
function assertValidWireMarkdown(md: string) {
  assert.ok(md.length > 0, "markdown must be non-empty");
  assert.ok(Array.from(md).length <= 20_000, `markdown length ${Array.from(md).length} exceeds 20000`);
  const stripped = md.replace(/```[\s\S]*?(?:```|$)|`[^`\n]*`/g, "");
  assert.equal(/!\[[^\]]*\]/.test(stripped), false, `markdown image found in: ${stripped}`);
  assert.equal(/^ {0,3}\[[^\]\n]+\]:/m.test(stripped), false, `markdown reference link found in: ${stripped}`);
  for (const match of [
    ...stripped.matchAll(/\]\([ \t\r\n\f]*<?([^) \t\r\n\f>]*)/g),
    ...stripped.matchAll(/<([A-Za-z][A-Za-z0-9+.-]*:[^<> \t\r\n\f]*)>/g),
  ]) {
    const raw = match[1];
    assert.equal(/%(?![0-9a-fA-F]{2})/.test(raw), false, `invalid URL percent-escape in: ${raw}`);
    const parsed = new URL(raw);
    assert.equal(parsed.protocol, "https:", `non-https URL: ${raw}`);
    assert.ok(parsed.hostname.length > 0, `empty hostname: ${raw}`);
    assert.equal(parsed.username, "", `userinfo username in: ${raw}`);
    assert.equal(parsed.password, "", `userinfo password in: ${raw}`);
  }
}

test("answerMarkdown preserves headings, lists, bold, italics and satisfies wire CheckView Markdown rules", () => {
  const html = [
    "<h2>Overview</h2>",
    "<p>Use <strong>strict mode</strong> and <em>type guards</em>.</p>",
    "<ul><li>First item</li><li><p>Second item with <code>const x = 1</code></p></li></ul>",
    "<blockquote><p>Quoted guidance</p></blockquote>",
    "<p>Literal &lt;http://localhost:3000&gt; and arr[0](http://evil.invalid) and ![img](https://evil.invalid/a.png)</p>",
    '<p><a href="https://user:pass@example.com/secret">creds</a> and <a href="https://en.wikipedia.org/wiki/Closure_(computer_programming)">wiki</a></p>',
  ].join("\n");
  const md = answerMarkdown(html);
  assert.match(md, /^### Overview$/m);
  assert.doesNotMatch(md, /^\\# Overview/m);
  assert.match(md, /\*\*strict mode\*\*/);
  assert.doesNotMatch(md, /\\\*\\\*strict mode\\\*\\\*/);
  assert.match(md, /\*type guards\*/);
  assert.match(md, /^- First item$/m);
  assert.match(md, /^- Second item with `const x = 1`$/m);
  assert.doesNotMatch(md, /^\\- First item/m);
  assert.match(md, /^> Quoted guidance$/m);
  assert.match(md, /\[wiki\]\(https:\/\/en\.wikipedia\.org\/wiki\/Closure_%28computer_programming%29\)/);
  assert.doesNotMatch(md, /user:pass@example\.com/);
  assertValidWireMarkdown(md);

  // Adversarial CheckView bypass regressions: every case must pass Go wire.CheckView's checkMarkdown.
  const adversarial = [
    '<pre><code>use ``` here</code></pre><pre><code>curl &lt;http://localhost:8080&gt;</code></pre>',
    '<p>A backtick ` in text and <code>![img](x)</code></p>',
    '<p>Warning!<a href="https://example.com">docs</a> and alert!<code>x</code><a href="https://example.com">more</a></p>',
    '<p><a href="https://example.com">foo]: bar</a></p>',
    '<p><a href="http://example.com">arr[0]</a>(param) and arr[0]<a href="http://example.com">(param)</a></p>',
    '<p>&lt;<a href="http://localhost:3000">http://localhost:3000</a>&gt;</p>',
    '<p><a href="https://example.com/100%">100%</a> <a href="https://example.com/?q=![x]">imgq</a> <a href="https://example.com/?q=`">tickq</a> <code>![y](z)</code></p>',
    '<p>&lt;http://example.com&#160;foo&gt;</p>',
    '<ul><li><pre><code>const a = 1;</code></pre></li></ul>',
    '<p>arr[0]<code>x</code>(http://evil.invalid) and &lt;http<code>x</code>://evil.invalid&gt;</p>',
    '<p>&lt;h<code>x</code>2://evil.invalid&gt; and <code><pre>x</pre></code><code>![img](y)</code></p>',
  ];
  for (const snippet of adversarial) {
    assertValidWireMarkdown(answerMarkdown(snippet));
  }
});

test("clip and clipMarkdown bound Unicode code points and keep Markdown valid at cut boundaries", () => {
  assert.equal(clip("hello", 0), "");
  assert.equal(clip("hello", 1), "h");
  assert.equal(clip("hello", 5), "hello");
  assert.equal(clip("hello", 4), "hel…");
  assert.equal(clip("😀😀😀😀", 3), "😀😀…");
  assert.equal(Array.from(clip("😀".repeat(2500), 2000)).length, 2000);

  // Cutting mid-link, mid-autolink, mid-inline-code, and mid-fenced-code-block must remain valid Markdown <= most.
  const withLink = "a".repeat(40) + " [docs](https://example.com/some/very/long/path)";
  const cutLink = clipMarkdown(withLink, 70);
  assert.ok(Array.from(cutLink).length <= 70);
  assertValidWireMarkdown(cutLink);

  const withInline = "a".repeat(40) + " `const x = ![bad](http://evil)`";
  const cutInline = clipMarkdown(withInline, 70);
  assert.ok(Array.from(cutInline).length <= 70);
  assertValidWireMarkdown(cutInline);

  const withFence = "```\n" + "x".repeat(40) + "\n![bad](http://evil)\n```";
  const cutFence = clipMarkdown(withFence, 60);
  assert.ok(Array.from(cutFence).length <= 60);
  assert.match(cutFence, /```\n…$/);
  assertValidWireMarkdown(cutFence);

  const fromListPre = answerMarkdown("<ul><li><pre><code>" + "line\n".repeat(30) + "</code></pre></li></ul>");
  const cutListPre = clipMarkdown(fromListPre, 80);
  assert.ok(Array.from(cutListPre).length <= 80);
  assertValidWireMarkdown(cutListPre);
});

test("readState preserves questions with titles between 201 and 500 characters", () => {
  const longTitle = "How ".repeat(70).trim(); // 279 chars, valid in parseQuestions (<= 500)
  const results = parseQuestions({ items: [{ ...question, title: longTitle }] }, "stackoverflow", 0);
  const state = { q: "typescript", site: "stackoverflow" as const, tag: "", found: true };
  const detail = questionTransition(state, results, "question:70794241");
  assert.deepEqual(readState(encodeState(detail)), detail);
});

test("accepted answer wins, then highest votes; empty answers are distinct from API errors", () => {
  const parsed = parseAnswers({ items: [answer], quota_remaining: 4 }, 70794241, "stackoverflow", 100);
  assert.equal(selectedAnswer(parsed.answers).accepted, true);
  const alternative = { ...answer, answer_id: 70794815, is_accepted: false, score: 99, link: "https://stackoverflow.com/questions/70794241/optional-properties/70794815#70794815" };
  assert.equal(selectedAnswer(parseAnswers({ items: [alternative] }, 70794241, "stackoverflow", 0).answers).accepted, false);
  assert.throws(() => selectedAnswer([]), (error: unknown) => error instanceof ReadError && error.code === "not_found");
  assert.equal(parseAnswers({ items: [{ ...answer, owner: null }] }, 70794241, "stackoverflow", 0).answers[0].author, "Anonymous");
  for (const change of [{ question_id: 1 }, { body: "" }, { answer_id: 0 }, { is_accepted: "yes" }]) {
    assert.throws(() => parseAnswers({ items: [{ ...answer, ...change }] }, 70794241, "stackoverflow", 0), ReadError);
  }
  assert.throws(() => parseAnswers({ items: [answer, answer] }, 70794241, "stackoverflow", 0), ReadError);
});

test("page state and question actions reject forged IDs, hosts and fields", () => {
  const state = { q: "typescript", site: "stackoverflow" as const, tag: "typescript", found: true };
  const results = parseQuestions({ items: [question] }, "stackoverflow", 0);
  assert.equal(questionTransition(state, results, "question:70794241").question?.id, 70794241);
  assert.equal(questionTransition(state, results, "question:1"), state);
  assert.equal(transition(state, "question:70794241"), state);
  const detail = questionTransition(state, results, "question:70794241");
  assert.deepEqual(readState(encodeState(detail)), detail);
  assert.deepEqual(readState(encodeState({ ...detail, question: { ...detail.question!, url: "https://attacker.invalid/questions/70794241" } })), initialState);
  assert.deepEqual(readState(encodeState({ ...detail, site: "unix" })), initialState);
  assert.deepEqual(readState('{"q":"bad\\n"}'), initialState);
  assert.equal(transition(state, "search", { query: "typescript", site: "__proto__" }), state);
  assert.equal(transition(state, "search", { query: "typescript", site: "unix", tag: "bash" }).site, "unix");
});

test("upstream malformed tags throw bad_answer, URLs and lone surrogates are bounded, and direct page() handles invalid site safely", () => {
  // Unexpected upstream tag must throw bad_answer (not user-input invalid).
  assert.throws(
    () => parseQuestions({ items: [{ ...question, tags: ["bad tag!"] }] }, "stackoverflow", 0),
    (error: unknown) => error instanceof ReadError && error.code === "bad_answer",
  );
  assert.throws(
    () => parseQuestions({ items: [{ ...question, tags: [""] }] }, "stackoverflow", 0),
    (error: unknown) => error instanceof ReadError && error.code === "bad_answer",
  );

  // Overlong canonical URL (> 4096 chars) must be rejected as bad_answer.
  assert.throws(
    () => parseQuestions({ items: [{ ...question, link: "https://stackoverflow.com/questions/70794241/" + "a".repeat(4100) }] }, "stackoverflow", 0),
    (error: unknown) => error instanceof ReadError && error.code === "bad_answer",
  );

  // Lone surrogates must be rejected in queryText and titleText.
  assert.throws(() => queryText("bad\ud800query"), (error: unknown) => error instanceof ReadError && error.code === "invalid");
  assert.throws(() => titleText("bad\udc00title"), (error: unknown) => error instanceof ReadError && error.code === "invalid");

  // Direct page() with invalid state.site must not crash with TypeError.
  const service = new StackExchange(async () => new Response("{}", { status: 200 }));
  const renderer = new Renderer({ authorize: () => false });
  try {
    const view = renderer.render(page({
      service,
      state: { q: "typescript", site: "nonexistent" as any, tag: "", found: false },
      loaded: { kind: "empty" },
      locale: "en",
    })).view;
    assert.equal(JSON.parse(view.state ?? "{}").site, "stackoverflow");
  } finally { renderer.dispose(); }

  // Direct page() with long titles and duplicate question IDs must satisfy CheckView limits.
  const longTitle = "Long title ".repeat(40);
  const dupQ = {
    id: 70794241, title: longTitle, url: "https://stackoverflow.com/questions/70794241/x",
    score: 1, answerCount: 1, tags: ["typescript"], license: "CC BY-SA 4.0", author: "Author",
  };
  const r2 = new Renderer({ authorize: () => false });
  try {
    const view = r2.render(page({
      service,
      state: { q: "typescript", site: "stackoverflow", tag: "", found: true },
      loaded: { kind: "results", result: { questions: [dupQ, dupQ], at: 0 } },
      locale: "en",
    })).view;
    const seen = new Set<string>();
    for (const section of view.sections) {
      assert.ok(section.items.length > 0);
      if (section.title) assert.ok(Array.from(section.title).length <= 200, `section title too long: ${section.title.length}`);
      for (const item of section.items) {
        if (typeof item.key === "string") {
          assert.equal(seen.has(item.key), false, `duplicate key: ${item.key}`);
          seen.add(item.key);
        }
      }
    }
  } finally { r2.dispose(); }
});
