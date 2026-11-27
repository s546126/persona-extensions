import { Button, Field, Link, Loading, Markdown, Section, Text, View, type CallbackAction, type NativeAction, type Node } from "@persona/extension/ui";
import { clip, clipMarkdown, oneLine, queryText, questionID, ReadError, selectedAnswer, siteID, sites, siteIDs, tagText, titleText, type Question, type SearchResult, type Site } from "./model.ts";
import { errorText, fmt, textsFor } from "./i18n.ts";
import { budget, checkSignal, type StackExchange } from "./service.ts";
export interface PageState { q: string; site: Site; tag: string; found: boolean; question?: Question }
export type Loaded = { kind: "loading" } | { kind: "empty" } | { kind: "results"; result: SearchResult } | { kind: "answer"; question: Question; body: string; accepted: boolean; id: number; url: string; author: string; license: string; stale?: ReadError; quotaRemaining?: number } | { kind: "no-answer" } | { kind: "error"; error: unknown };
export const initialState: PageState = { q: "", site: "stackoverflow", tag: "", found: false };
const STATE_MAX = 16_384;
const ITEMS_MAX = 300;
const utf8Bytes = (value: string) => new TextEncoder().encode(value).byteLength;
function hash64(value: string): string {
  let hash = 0xcbf29ce484222325n;
  const mask = (1n << 64n) - 1n;
  for (const byte of new TextEncoder().encode(value)) {
    hash ^= BigInt(byte);
    hash = (hash * 0x100000001b3n) & mask;
  }
  return hash.toString(16).padStart(16, "0");
}
function uniqueKey(used: Set<string>, prefix: string, value: string): string {
  const build = (suffix: string) => {
    const full = `${prefix}${suffix}`;
    if (full.length <= 128) return full;
    const h = `~${hash64(suffix)}`;
    return `${prefix}${suffix.slice(0, Math.max(0, 128 - prefix.length - h.length))}${h}`;
  };
  let candidate = build(value);
  let attempt = 1;
  while (used.has(candidate)) candidate = build(`${value}#${attempt++}`);
  used.add(candidate);
  return candidate;
}
function safeLinkURL(raw: string): string | undefined {
  try {
    const url = new URL(raw);
    if (url.protocol !== "https:" || !url.hostname || url.username || url.password || url.port) return undefined;
    return url.href.length <= 4096 ? url.href : undefined;
  } catch { return undefined; }
}
function questionState(value: unknown, site: Site): Question | undefined {
  if (value == null) return undefined;
  if (!value || typeof value !== "object" || Array.isArray(value)) throw new ReadError("invalid");
  const raw = value as Record<string, unknown>, id = questionID(raw.id);
  const candidate: Question = { id, title: titleText(raw.title), url: "", score: 0, answerCount: 0, tags: [], license: "CC BY-SA 4.0", author: "" };
  const url = new URL(candidate.url = typeof raw.url === "string" ? raw.url : "");
  if (url.protocol !== "https:" || url.hostname !== sites[site].host || url.username || url.password || url.port || url.href.length > 4096 || (url.pathname !== `/questions/${id}` && !url.pathname.startsWith(`/questions/${id}/`))) throw new ReadError("invalid");
  candidate.url = url.href;
  if (!Number.isSafeInteger(raw.score) || !Number.isSafeInteger(raw.answerCount) || (raw.answerCount as number) < 0 || (raw.score as number) < -1_000_000 || (raw.score as number) > 1_000_000 || (raw.answerCount as number) > 1_000_000) throw new ReadError("invalid");
  candidate.score = raw.score as number; candidate.answerCount = raw.answerCount as number;
  candidate.tags = Array.isArray(raw.tags) ? raw.tags.slice(0, 10).map(item => {
    const t = tagText(item);
    if (!t) throw new ReadError("invalid");
    return t;
  }) : [];
  if (new Set(candidate.tags).size !== candidate.tags.length) throw new ReadError("invalid");
  if (typeof raw.license !== "string" || !/^CC BY-SA (?:2\.5|3\.0|4\.0)$/.test(raw.license)) throw new ReadError("invalid");
  candidate.license = raw.license;
  candidate.author = clip(oneLine(typeof raw.author === "string" ? raw.author : ""), 200);
  if (!candidate.author) throw new ReadError("invalid");
  if (raw.authorURL != null) {
    if (typeof raw.authorURL !== "string") throw new ReadError("invalid");
    const link = new URL(raw.authorURL);
    if (link.protocol !== "https:" || link.hostname !== sites[site].host || link.href.length > 4096 || !/^\/users\/[1-9][0-9]*(?:\/|$)/.test(link.pathname) || link.username || link.password || link.port) throw new ReadError("invalid");
    candidate.authorURL = link.href;
  }
  if (raw.acceptedAnswerID != null) candidate.acceptedAnswerID = questionID(raw.acceptedAnswerID);
  return candidate;
}
function safeState(raw: unknown): PageState {
  if (!raw || typeof raw !== "object" || Array.isArray(raw)) return initialState;
  const value = raw as Record<string, unknown>;
  try {
    const site = siteID(value.site);
    const found = typeof value.found === "boolean" ? value.found : false;
    const tag = tagText(value.tag);
    if (!found) {
      const q = typeof value.q === "string" ? clip(oneLine(value.q), 200) : "";
      return { q, site, tag, found: false };
    }
    return { q: queryText(value.q), site, tag, found: true, ...(value.question != null ? { question: questionState(value.question, site) } : {}) };
  } catch { return initialState; }
}
export function readState(raw: unknown): PageState {
  if (typeof raw !== "string" || raw.length > 16_000) return initialState;
  try {
    const value = JSON.parse(raw) as Record<string, unknown>;
    if (!value || Array.isArray(value) || typeof value.found !== "boolean") return initialState;
    const site = siteID(value.site);
    const question = questionState(value.question, site);
    return { q: queryText(value.q), site, tag: tagText(value.tag), found: value.found, ...(question ? { question } : {}) };
  } catch { return initialState; }
}
export function encodeState(state: PageState): string {
  const clean = safeState(state);
  const full = JSON.stringify(clean);
  if (utf8Bytes(full) <= STATE_MAX) return full;
  const slim = JSON.stringify({ q: clean.q, site: clean.site, tag: clean.tag, found: clean.found });
  return utf8Bytes(slim) <= STATE_MAX ? slim : JSON.stringify(initialState);
}
export function questionTransition(state: PageState, results: SearchResult, id: string): PageState {
  if (!id.startsWith("question:")) return state;
  let idNumber: number;
  try { idNumber = questionID(id.slice("question:".length)); } catch { return state; }
  const question = results.questions.find(candidate => candidate.id === idNumber);
  return question ? { q: state.q, site: state.site, tag: state.tag, found: true, question } : state;
}
export function transition(state: PageState, id: string, values: unknown = {}): PageState {
  let fields: Readonly<Record<string, unknown>>;
  if (typeof values === "string") { try { values = JSON.parse(values); } catch { return state; } }
  if (!values || typeof values !== "object" || Array.isArray(values)) return state;
  fields = values as Readonly<Record<string, unknown>>;
  if (id === "search") {
    try {
      const next = { q: queryText(fields.query), site: siteID(fields.site ?? state.site), tag: tagText(fields.tag ?? ""), found: true };
      return next.q === state.q && next.site === state.site && next.tag === state.tag && state.found ? { ...next, question: state.question } : next;
    } catch { return state; }
  }
  if (id === "home") return initialState;
  if (id === "back" && state.found) return { q: state.q, site: state.site, tag: state.tag, found: true };
  return state;
}
export async function loadPage(service: StackExchange, state: PageState, refresh: boolean, signal?: AbortSignal): Promise<Loaded> {
  const request = budget(signal, service.limits.page);
  try {
    if (!state.found) return { kind: "empty" };
    if (!state.question) return { kind: "results", result: await service.search(state.q, state.site, state.tag, 12, refresh, request.signal) };
    if (state.question.answerCount === 0) return { kind: "no-answer" };
    const result = await service.answers(state.question.id, state.site, refresh, request.signal), answer = selectedAnswer(result.answers);
    return { kind: "answer", question: state.question, body: answer.body, accepted: answer.accepted, id: answer.id, url: answer.url, author: answer.author, license: answer.license, stale: result.stale, quotaRemaining: result.quotaRemaining };
  } catch (error) { checkSignal(signal); return { kind: "error", error: request.signal.aborted && request.signal.reason instanceof ReadError ? request.signal.reason : error }; }
  finally { request.dispose(); }
}
export interface PageProps { service: StackExchange; state: PageState; loaded: Loaded; locale?: string; uiVersion?: number; action?: (id: string, next?: PageState, title?: string) => NativeAction | CallbackAction }
export function page(props: PageProps): Node {
  const { service, loaded, uiVersion = 0 } = props, state = safeState(props.state), locale = props.locale ?? service.locale, t = textsFor(locale), siteName = sites[state.site].name;
  const act = (id: string, next?: PageState, title?: string) => props.action?.(id, next, title) ?? (next && uiVersion >= 3 ? { id, push: { ui: "answers", state: encodeState(next), ...(title ? { title: clip(title, 200) } : {}) } } : { id });
  const text = (key: string, value: string, style: "body" | "caption" | "secondary" = "secondary") => <Text key={key} text={clip(value || t.unknown, 2000)} style={style} />;
  const search = <Section key="stackexchange-search" title={clip(t.search, 200)}>
    <Field key="stackexchange-query" name="query" label={clip(t.search, 200)} kind="text" placeholder={clip(t.searchHint, 200)} value={state.q} />
    <Field key="stackexchange-site" name="site" label={clip(t.site, 200)} kind="choice" value={state.site} choices={siteIDs.map(id => ({ value: id, title: sites[id].name }))} />
    <Field key="stackexchange-tag" name="tag" label={clip(t.tag, 200)} kind="text" placeholder={clip(t.tagHint, 200)} value={state.tag} />
    <Button key="stackexchange-submit" title={clip(t.searchAction, 200)} action={act("search")} />
  </Section>;
  const attribution = <Section key="stackexchange-attribution">{text("stackexchange-license", t.attribution, "caption")}</Section>;
  if (loaded.kind === "loading") return <View key="stackexchange" title={clip(t.search, 200)} state={encodeState(state)}>{search}<Section key="stackexchange-loading"><Loading key="stackexchange-loading-indicator" title={clip(t.loading, 200)} /></Section>{attribution}</View>;
  if (loaded.kind === "error") return <View key="stackexchange" title={clip(t.search, 200)} state={encodeState(state)}>{search}<Section key="stackexchange-error">{text("stackexchange-error-message", errorText(loaded.error, locale))}<Button key="stackexchange-retry" title={clip(t.retry, 200)} action={act("refresh")} /></Section>{attribution}</View>;
  if (loaded.kind === "empty") return <View key="stackexchange" title={clip(t.search, 200)} state={encodeState(state)}>{search}<Section key="stackexchange-welcome">{text("stackexchange-welcome-text", t.welcome, "body")}</Section>{attribution}</View>;
  const footer = <Section key="stackexchange-footer"><Button key="stackexchange-refresh" title={clip(t.retry, 200)} action={act("refresh")} />{state.question ? <Button key="stackexchange-back" title={clip(t.back, 200)} action={act("back")} /> : null}<Button key="stackexchange-home" title={clip(t.home, 200)} action={act("home")} /></Section>;
  if (loaded.kind === "no-answer") {
    const qURL = state.question ? safeLinkURL(state.question.url) : undefined;
    return <View key="stackexchange" title={clip(state.question?.title ?? t.title, 200)} state={encodeState(state)}>{search}<Section key="stackexchange-no-answer">{text("stackexchange-no-answer-text", t.noAnswer, "body")}{qURL ? <Link key="stackexchange-question-link" title={clip(fmt(t.openQuestion, { site: siteName }), 200)} url={qURL} /> : null}</Section>{footer}{attribution}</View>;
  }
  if (loaded.kind === "answer") {
    const aURL = safeLinkURL(loaded.url), qURL = safeLinkURL(loaded.question.url);
    return <View key="stackexchange" title={clip(loaded.question.title, 200)} state={encodeState(state)}>
      <Section key="stackexchange-answer-heading" title={clip(loaded.accepted ? t.accepted : t.highest, 200)} footer={clip([fmt(t.score, { count: loaded.question.score }), fmt(t.answers, { count: loaded.question.answerCount })].join(" · "), 1000)}>
        <Markdown key="stackexchange-answer-body" text={clipMarkdown(loaded.body, 20_000)} />
        {text("stackexchange-answer-author", fmt(t.by, { author: loaded.author, license: loaded.license }), "caption")}
        {aURL ? <Link key="stackexchange-answer-link" title={clip(fmt(t.openAnswer, { site: siteName }), 200)} url={aURL} /> : null}
      </Section>
      <Section key="stackexchange-question">{text("stackexchange-question-title", loaded.question.title, "body")}{qURL ? <Link key="stackexchange-question-link" title={clip(fmt(t.openQuestion, { site: siteName }), 200)} url={qURL} /> : null}{loaded.question.tags.length ? text("stackexchange-question-tags", loaded.question.tags.map(tag => "#" + tag).join(" · "), "caption") : null}</Section>
      {loaded.stale ? <Section key="stackexchange-stale">{text("stackexchange-stale-text", errorText(loaded.stale, locale))}</Section> : null}
      {loaded.quotaRemaining !== undefined ? <Section key="stackexchange-quota">{text("stackexchange-quota-text", fmt(t.quota, { count: loaded.quotaRemaining }), "caption")}</Section> : null}
      {footer}{attribution}
    </View>;
  }
  const used = new Set<string>();
  const maxQuestions = Math.min(20, Math.floor((ITEMS_MAX - 16) / 2));
  const questions = loaded.result.questions.slice(0, maxQuestions);
  return <View key="stackexchange" title={clip(`${t.title}: ${clip(state.q, 100)}`, 200)} state={encodeState(state)}>{search}
    {questions.length ? questions.map(question => {
      const qURL = safeLinkURL(question.url);
      return <Section key={uniqueKey(used, "question-", String(question.id))} title={clip(question.title, 200)} footer={clip([fmt(t.score, { count: question.score }), fmt(t.answers, { count: question.answerCount }), question.tags.map(tag => "#" + tag).join(" · ")].filter(Boolean).join(" · "), 1000)}>
        <Button key={uniqueKey(used, "open-", String(question.id))} title={clip(t.readAnswer, 200)} action={act(`question:${question.id}`, { ...state, question }, clip(question.title, 200))} />
        {qURL ? <Link key={uniqueKey(used, "link-", String(question.id))} title={clip(fmt(t.openQuestion, { site: siteName }), 200)} url={qURL} /> : null}
      </Section>;
    }) : <Section key="stackexchange-empty-results">{text("stackexchange-empty", t.empty)}</Section>}
    {loaded.result.stale ? <Section key="stackexchange-stale">{text("stackexchange-stale-text", errorText(loaded.result.stale, locale))}</Section> : null}
    {loaded.result.quotaRemaining !== undefined ? <Section key="stackexchange-quota">{text("stackexchange-quota-text", fmt(t.quota, { count: loaded.result.quotaRemaining }), "caption")}</Section> : null}
    {footer}{attribution}
  </View>;
}
