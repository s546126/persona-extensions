import { Button, Field, Link, Loading, Section, Text, View, type CallbackAction, type NativeAction, type Node } from "@persona/extension/ui";
import { clip, queryText, ReadError, repositoryName, type DetailResult, type ImageSummary, type SearchResult } from "./model.ts";
import { dateText, errorText, fmt, number, textsFor } from "./i18n.ts";
import { budget, checkSignal, type DockerHub } from "./service.ts";
export interface PageState { q: string; found: boolean; image?: ImageSummary }
export type Loaded = { kind: "loading" } | { kind: "empty" } | { kind: "results"; result: SearchResult } | { kind: "detail"; result: DetailResult } | { kind: "error"; error: unknown };
export const initialState: PageState = { q: "", found: false };
const MAX_VIEW_ITEMS = 300;
const MAX_RESULT_CHROME_ITEMS = 8;
const MAX_DETAIL_CHROME_ITEMS = 13;
const MAX_RESULT_ITEMS = MAX_VIEW_ITEMS - MAX_RESULT_CHROME_ITEMS;
const MAX_DETAIL_TAG_ITEMS = MAX_VIEW_ITEMS - MAX_DETAIL_CHROME_ITEMS;
const MAX_RESULT_BYTES = 512 * 1024;
const MAX_STATE_BYTES = 16 * 1024;
const encoder = new TextEncoder();
function shortHash(value: string): string {
  let hash = 0x811c9dc5;
  for (let i = 0; i < value.length; i++) {
    hash ^= value.charCodeAt(i);
    hash = Math.imul(hash, 0x01000193);
  }
  return (hash >>> 0).toString(16).padStart(8, "0");
}
function boundedID(prefix: string, value: string, max = 128): string {
  const full = prefix + value;
  if (Array.from(full).length <= max) return full;
  const suffix = "~" + shortHash(value);
  const keep = max - Array.from(prefix).length - suffix.length;
  return prefix + Array.from(value).slice(0, Math.max(0, keep)).join("") + suffix;
}
export const imageActionID = (name: string) => boundedID("image:", name, 128);
function validHTTPS(raw: string | undefined, maxLen = 4096): string | undefined {
  if (!raw || raw.length > maxLen) return undefined;
  try {
    const url = new URL(raw);
    return url.protocol === "https:" && url.hostname && !url.username && !url.password && !url.port ? url.href : undefined;
  } catch { return undefined; }
}
function summaryState(value: unknown): ImageSummary | undefined {
  if (value == null) return undefined;
  if (!value || typeof value !== "object" || Array.isArray(value)) throw new ReadError("invalid");
  const raw = value as Record<string, unknown>;
  if (typeof raw.description !== "string" || /[\ud800-\udfff]/u.test(raw.description) || typeof raw.official !== "boolean" || typeof raw.pulls !== "number" || !Number.isSafeInteger(raw.pulls) || raw.pulls < 0 || raw.pulls > 100_000_000_000_000 || typeof raw.stars !== "number" || !Number.isSafeInteger(raw.stars) || raw.stars < 0 || raw.stars > 100_000_000_000_000) throw new ReadError("invalid");
  const name = repositoryName(raw.name);
  return { name, description: clip(raw.description, 500), official: raw.official, pulls: raw.pulls, stars: raw.stars, url: `https://hub.docker.com/r/${name}` };
}
export function readState(raw: unknown): PageState {
  if (typeof raw !== "string" || raw.length > 10_000) return initialState;
  try {
    const value = JSON.parse(raw) as Record<string, unknown>;
    if (!value || Array.isArray(value) || typeof value.found !== "boolean") return initialState;
    return { q: queryText(value.q), found: value.found, image: summaryState(value.image) };
  } catch { return initialState; }
}
export const encodeState = (state: PageState) => JSON.stringify(state);
function boundState(state: PageState): PageState {
  const base: PageState = { q: clip(state.q, 100), found: Boolean(state.found) };
  if (!state.image) return base;
  const candidate: PageState = { ...base, image: { ...state.image, description: clip(state.image.description, 500) } };
  return encoder.encode(encodeState(candidate)).byteLength <= MAX_STATE_BYTES ? candidate : base;
}
export function imageTransition(state: PageState, result: SearchResult, id: string): PageState {
  if (!id.startsWith("image:")) return state;
  const image = result.images.find(candidate => id === `image:${candidate.name}` || id === imageActionID(candidate.name));
  return image ? { q: state.q, found: true, image } : state;
}
export function transition(state: PageState, id: string, values: unknown = {}): PageState {
  let fields: Readonly<Record<string, unknown>>;
  if (typeof values === "string") { try { values = JSON.parse(values); } catch { return state; } }
  if (!values || typeof values !== "object" || Array.isArray(values)) return state;
  fields = values as Readonly<Record<string, unknown>>;
  if (id === "search") {
    try {
      const next = { q: queryText(fields.query), found: true };
      return next.q === state.q && state.found ? { ...next, image: state.image } : next;
    } catch { return state; }
  }
  if (id === "home") return initialState;
  if (id === "back" && state.found) return { q: state.q, found: true };
  return state;
}
export async function loadPage(service: DockerHub, state: PageState, refresh: boolean, signal?: AbortSignal): Promise<Loaded> {
  const request = budget(signal, service.limits.page);
  try {
    if (!state.found) return { kind: "empty" };
    if (state.image) return { kind: "detail", result: await service.repository(state.image.name, refresh, request.signal) };
    return { kind: "results", result: await service.search(state.q, 12, refresh, request.signal) };
  } catch (error) {
    checkSignal(signal);
    return { kind: "error", error: !(error instanceof ReadError) && request.signal.aborted && request.signal.reason instanceof ReadError ? request.signal.reason : error };
  }
  finally { request.dispose(); }
}
export interface PageProps { service: DockerHub; state: PageState; loaded: Loaded; locale?: string; uiVersion?: number; action?: (id: string, next?: PageState, title?: string) => NativeAction | CallbackAction }
export function page(props: PageProps): Node {
  const { service, state, loaded, uiVersion = 0 } = props, locale = props.locale || service.locale, t = textsFor(locale);
  const safeState = boundState(state);
  const encodedState = encodeState(safeState);
  const act = (id: string, next?: PageState, title?: string) => {
    const safeNext = next ? boundState(next) : undefined;
    const safeTitle = title ? clip(title, 2000) : undefined;
    return props.action?.(id, safeNext, safeTitle) ?? (safeNext && uiVersion >= 3 ? { id, push: { ui: "dockerhub", state: encodeState(safeNext), ...(safeTitle ? { title: safeTitle } : {}) } } : { id });
  };
  const text = (key: string, value: string, style: "body" | "caption" | "secondary" = "secondary") => <Text key={key} text={clip(value || t.unknown, 2000)} style={style} />;
  const search = <Section key="dockerhub-search" title={t.search}><Field key="dockerhub-query" name="query" label={t.search} kind="text" placeholder={t.searchHint} value={safeState.q} /><Button key="dockerhub-submit" title={t.searchAction} action={act("search")} /></Section>;
  const attribution = <Section key="dockerhub-attribution">{text("dockerhub-license", t.attribution, "caption")}</Section>;
  const footer = <Section key="dockerhub-footer"><Button key="dockerhub-refresh" title={t.retry} action={act("refresh")} />{safeState.image ? <Button key="dockerhub-back" title={t.back} action={act("back")} /> : null}<Button key="dockerhub-home" title={t.home} action={act("home")} /></Section>;
  const stale = (error?: ReadError) => error ? <Section key="dockerhub-stale">{text("dockerhub-stale-text", errorText(error, locale))}</Section> : null;
  if (loaded.kind === "loading") return <View key="dockerhub" title={t.search} state={encodedState}>{search}<Section key="dockerhub-loading"><Loading key="dockerhub-loading-indicator" title={t.loading} /></Section>{attribution}</View>;
  if (loaded.kind === "error") return <View key="dockerhub" title={t.search} state={encodedState}>{search}<Section key="dockerhub-error">{text("dockerhub-error-message", errorText(loaded.error, locale))}<Button key="dockerhub-retry" title={t.retry} action={act("refresh")} /></Section>{attribution}</View>;
  if (loaded.kind === "empty") return <View key="dockerhub" title={t.search} state={encodedState}>{search}<Section key="dockerhub-welcome">{text("dockerhub-welcome-text", t.welcome, "body")}</Section>{attribution}</View>;
  const usedKeys = new Set<string>();
  const uniqueID = (prefix: string, value: string): string => {
    let key = boundedID(prefix, value);
    for (let n = 1; usedKeys.has(key); n++) key = boundedID(prefix, `${value}~${n}`);
    usedKeys.add(key);
    return key;
  };
  if (loaded.kind === "detail") {
    const image = loaded.result.image;
    const imageURL = validHTTPS(image.url) ?? `https://hub.docker.com/r/${image.name}`;
    const detailFooter = clip([image.official ? t.official : fmt(t.namespace, { namespace: image.namespace }), dateText(image.updated)].filter(Boolean).join(" · "), 2000);
    const tagCards: Node[] = [];
    let remainingItems = MAX_DETAIL_TAG_ITEMS;
    let remainingBytes = MAX_RESULT_BYTES;
    for (const tag of image.tags) {
      if (remainingItems < 3) break;
      const tagTitle = clip(tag.name, 2000);
      const tagFooter = clip(dateText(tag.updated), 2000);
      const digestText = clip(tag.digest, 2000);
      const platformsText = clip(tag.platforms.join(" · ") || t.noPlatforms, 2000);
      const tagURL = `${imageURL}/tags?name=${encodeURIComponent(tag.name)}`;
      const sectionKey = uniqueID("tag-", tag.name);
      const digestKey = uniqueID("digest-", tag.name);
      const platformsKey = uniqueID("platforms-", tag.name);
      const linkKey = uniqueID("tag-link-", tag.name);
      const cardBytes = encoder.encode(JSON.stringify([sectionKey, tagTitle, tagFooter, digestKey, digestText, platformsKey, platformsText, linkKey, tagURL])).byteLength;
      if (cardBytes > remainingBytes) break;
      tagCards.push(<Section key={sectionKey} title={tagTitle} footer={tagFooter}>
        {text(digestKey, digestText)}
        {text(platformsKey, platformsText)}
        <Link key={linkKey} title={t.openTag} url={tagURL} />
      </Section>);
      remainingItems -= 3;
      remainingBytes -= cardBytes;
    }
    return <View key="dockerhub" title={clip(image.name, 2000)} state={encodedState}>{search}
      <Section key="dockerhub-detail" title={clip(image.name, 2000)} footer={detailFooter}>
        {text("dockerhub-description", image.description, "body")}
        {text("dockerhub-total", fmt(t.total, { count: number(image.pulls) }))}
        {text("dockerhub-stars", fmt(t.stars, { count: number(image.stars) }))}
      </Section>
      <Section key="dockerhub-install" title={t.install}>{text("dockerhub-install-command", image.pull, "body")}{text("dockerhub-install-hint", t.installHint, "caption")}</Section>
      <Section key="dockerhub-tags" title={clip(fmt(t.tags, { count: number(image.tags.length) }), 2000)} />
      {tagCards}
      <Section key="dockerhub-links"><Link key="dockerhub-image-link" title={t.openImage} url={imageURL} /></Section>
      {stale(loaded.result.stale)}{footer}{attribution}
    </View>;
  }
  const cards: Node[] = [];
  let remainingItems = MAX_RESULT_ITEMS;
  let remainingBytes = MAX_RESULT_BYTES;
  for (const image of loaded.result.images) {
    if (remainingItems < 3) break;
    const title = clip(image.name, 2000);
    const cardFooter = clip([image.official ? t.official : t.community, fmt(t.total, { count: number(image.pulls) })].join(" · "), 2000);
    const desc = clip(image.description || t.unknown, 2000);
    const imageURL = validHTTPS(image.url) ?? `https://hub.docker.com/r/${image.name}`;
    const sectionKey = uniqueID("image-", image.name);
    const descKey = uniqueID("description-", image.name);
    const openKey = uniqueID("open-", image.name);
    const linkKey = uniqueID("link-", image.name);
    const nextState = boundState({ ...safeState, image });
    const actionID = imageActionID(image.name);
    const cardBytes = encoder.encode(JSON.stringify([sectionKey, title, cardFooter, descKey, desc, openKey, actionID, linkKey, imageURL, nextState])).byteLength;
    if (cardBytes > remainingBytes) break;
    cards.push(<Section key={sectionKey} title={title} footer={cardFooter}>
      {text(descKey, desc, "body")}
      <Button key={openKey} title={t.details} action={act(actionID, nextState, image.name)} />
      <Link key={linkKey} title={t.openImage} url={imageURL} />
    </Section>);
    remainingItems -= 3;
    remainingBytes -= cardBytes;
  }
  return <View key="dockerhub" title={clip(`${t.title}: ${clip(safeState.q, 100)}`, 2000)} state={encodedState}>{search}
    <Section key="dockerhub-result-count">{text("dockerhub-total-results", fmt(t.results, { total: number(loaded.result.total) }), "caption")}</Section>
    {cards.length ? cards : <Section key="dockerhub-empty-results">{text("dockerhub-empty", t.empty)}</Section>}
    {stale(loaded.result.stale)}{footer}{attribution}
  </View>;
}
