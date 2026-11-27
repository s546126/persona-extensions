import { Button, Field, Link, Loading, Row, Section, Text, View, type CallbackAction, type NativeAction, type Node } from "@persona/extension/ui";
import { boundedID, clip, parseStoredShow, plain, podcastID, queryText, ReadError, type Episode, type EpisodesResult, type SearchResult, type Show } from "./model.ts";
import { errorText, fmt, readTime, textsFor } from "./i18n.ts";
import { budget, checkSignal, type Podcasts } from "./service.ts";
import type { Storage } from "./store.ts";
const TEXT_MAX = 2000;
const KEY_MAX = 128;
const ITEMS_MAX = 300;
const STATE_MAX = 16_384;
const FRAME_MAX = 512 * 1024;
const URL_MAX = 4096;
const utf8 = new TextEncoder();
const byteLength = (text: string) => utf8.encode(text).byteLength;
function safeLinkURL(value: unknown): string | undefined {
  if (typeof value !== "string" || !value) return undefined;
  let link: URL;
  try { link = new URL(value); } catch { return undefined; }
  if (link.protocol !== "https:" || link.username || link.password || link.port || link.href.length > URL_MAX) return undefined;
  return link.href;
}
function uniqueKey(used: Set<string>, prefix: string, raw: string): string {
  let key = boundedID(prefix, raw, KEY_MAX);
  let attempt = 2;
  while (used.has(key)) {
    key = boundedID(prefix, `${raw}#${attempt}`, KEY_MAX);
    attempt++;
  }
  used.add(key);
  return key;
}
export type ViewKind = "discover" | "search" | "show" | "library";
export interface PageState { v: ViewKind; q: string; k: number; from?: "library"; b?: Show; message?: string }
export type Loaded = { kind: "loading" } | { kind: "empty" } | { kind: "search"; result: SearchResult } | { kind: "show"; result: EpisodesResult } | { kind: "library"; shows: Show[] } | { kind: "error"; error: unknown };
export const initialState: PageState = { v: "discover", q: "", k: 0 };
export function readState(raw: unknown): PageState {
  if (typeof raw !== "string" || byteLength(raw) > STATE_MAX) return initialState;
  try {
    const value = JSON.parse(raw) as Record<string, unknown>;
    if (!value || Array.isArray(value)) return initialState;
    const k = value.k === 0 ? 0 : podcastID(value.k);
    const v: ViewKind = ["search", "show", "library"].includes(String(value.v)) ? value.v as ViewKind : "discover";
    const q = typeof value.q === "string" && value.q !== "" ? queryText(value.q) : "";
    const b = value.b === undefined ? undefined : parseStoredShow(value.b);
    if (value.b !== undefined && !b) return initialState;
    if (v === "show" && (!k || !b || k !== b.id) || v === "search" && !q) return initialState;
    return { v, q, k,
      ...(value.from === "library" ? { from: "library" as const } : {}),
      ...(b === undefined ? {} : { b }),
      ...(typeof value.message === "string" && ["saved", "removed"].includes(value.message) ? { message: value.message } : {}) };
  } catch { return initialState; }
}
export const encodeState = (state: PageState) => {
  const encoded = JSON.stringify(state);
  if (byteLength(encoded) <= STATE_MAX) return encoded;
  const slim = JSON.stringify(state.q ? { v: "search", q: state.q, k: 0 } : initialState);
  return byteLength(slim) <= STATE_MAX ? slim : JSON.stringify(initialState);
};
function safeState(state: PageState): PageState {
  try { return readState(JSON.stringify(state)); }
  catch { return initialState; }
}
export function transition(state: PageState, id: string, values: unknown = {}): PageState {
  const [kind, argument] = id.split(":", 2);
  if (kind === "show" && argument && state.b?.id === Number(argument)) return { ...state, v: "show", k: state.b.id, message: undefined };
  if (kind === "saved" && argument && state.b?.id === Number(argument)) return { ...state, message: "saved" };
  if (kind === "removed" && argument && state.b?.id === Number(argument)) return { ...state, message: "removed" };
  let fields: Readonly<Record<string, unknown>>;
  if (typeof values === "string") {
    try { values = JSON.parse(values); } catch { return state; }
  }
  if (!values || typeof values !== "object" || Array.isArray(values)) return state;
  fields = values as Readonly<Record<string, unknown>>;
  if (kind === "search") {
    const raw = typeof fields.query === "string" ? fields.query : undefined;
    if (raw === undefined || /[\u0000-\u0008\u000b\u000c\u000e-\u001f\u007f\ud800-\udfff]/u.test(raw)) return state;
    try {
      const query = queryText(clip(plain(raw), 200));
      return { v: "search", q: query, k: 0 };
    } catch { return state; }
  }
  if (kind === "library") return { ...state, v: "library", k: 0, from: undefined, b: undefined };
  if (kind === "back") return state.from === "library" ? { v: "library", q: "", k: 0 } : state.q ? { v: "search", q: state.q, k: 0 } : initialState;
  if (kind === "home") return initialState;
  return state;
}
export async function loadPage(service: Podcasts, store: Storage, state: PageState, refresh: boolean, signal?: AbortSignal): Promise<Loaded> {
  const request = budget(signal, service.limits.page);
  try {
    if (state.v === "show") return { kind: "show", result: await service.episodes(state.k, 20, refresh, request.signal) };
    if (state.v === "search") return { kind: "search", result: await service.search(state.q, 20, refresh, request.signal) };
    if (state.v === "library") return { kind: "library", shows: (await store.load()).shows };
    return { kind: "empty" };
  } catch (error) { checkSignal(signal); return { kind: "error", error }; }
  finally { request.dispose(); }
}
export interface PageProps { service: Podcasts; store: Storage; state: PageState; loaded: Loaded; locale?: string; tz?: string; uiVersion?: number; action?: (id: string, next?: PageState, title?: string) => NativeAction | CallbackAction }
export function page(props: PageProps): Node {
  const { service, loaded, uiVersion = 0 } = props, state = safeState(props.state), stateJSON = encodeState(state), t = textsFor(props.locale ?? service.locale), tz = props.tz ?? "Local";
  const act = (id: string, next?: PageState, title?: string) => {
    const cleanTitle = title ? clip(title, TEXT_MAX) : undefined;
    return props.action?.(id, next, cleanTitle) ?? (next && uiVersion >= 3 ? { id, push: { ui: "podcasts", state: encodeState(next), ...(cleanTitle ? { title: cleanTitle } : {}) } } : { id });
  };
  const text = (key: string, value: string, style: "body" | "caption" | "secondary" = "secondary") => <Text key={key} text={clip(value, 1800)} style={style} />;
  const search = <Section key="podcast-search" title={clip(t.search, TEXT_MAX)}><Field key="podcast-query" name="query" label={clip(t.search, TEXT_MAX)} kind="text" placeholder={clip(t.searchHint, TEXT_MAX)} value={state.q} /><Button key="podcast-search-submit" title={clip(t.searchAction, TEXT_MAX)} action={act("search")} /></Section>;
  const navigation = <Section key="podcast-navigation"><Button key="podcast-home" title={clip(t.discover, TEXT_MAX)} action={act("home")} /><Button key="podcast-library" title={clip(t.library, TEXT_MAX)} action={act("library")} /></Section>;
  const message = state.message ? <Section key="podcast-message">{text("podcast-message-text", state.message === "saved" ? t.savedMessage : t.removedMessage, "body")}</Section> : null;
  if (loaded.kind === "loading") return <View key="podcasts" title={clip(t.discover, TEXT_MAX)} state={stateJSON}>{search}{navigation}<Section key="podcast-loading"><Loading key="podcast-loading-indicator" title={clip(t.loading, TEXT_MAX)} /></Section></View>;
  if (loaded.kind === "error") return <View key="podcasts" title={clip(t.discover, TEXT_MAX)} state={stateJSON}>{search}{navigation}<Section key="podcast-error">{text("podcast-error-message", errorText(loaded.error, props.locale ?? service.locale))}<Button key="podcast-retry" title={clip(t.retry, TEXT_MAX)} action={act("refresh")} /></Section></View>;
  if (state.v === "show") {
    if (loaded.kind !== "show") throw new ReadError("invalid");
    const result = loaded.result, show = result.show, saved = props.store.has(show.id);
    const showURL = safeLinkURL(show.url), feedURL = safeLinkURL(show.feedURL);
    const usedShowKeys = new Set(["show-details", "show-publisher", "show-actions", "show-save", "show-remove", "show-open", "show-feed", "show-back", "podcast-message", "podcast-message-text", "show-episodes", "show-no-episodes", "show-no-episodes-text", "show-episode-notes", "show-footer", "show-refresh", "show-feed-note", "show-feed-note-text", "show-attribution", "show-license"]);
    const reservedItems = 1 + 4 + (message ? 1 : 0) + 3;
    let remainingItems = Math.max(0, ITEMS_MAX - reservedItems);
    let remainingBytes = FRAME_MAX - byteLength(stateJSON) - 8192;
    const episodeLinks: Node[] = [];
    const episodeNotes: Node[] = [];
    for (const episode of result.episodes) {
      const epURL = safeLinkURL(episode.url);
      const needed = epURL ? 2 : 1;
      if (needed > remainingItems) break;
      const noteText = clip(`${episode.title}\n${[episode.at ? readTime(episode.at, tz, props.locale ?? service.locale) || t.unknownDate : t.unknownDate, episode.minutes === undefined ? "" : fmt(t.minutes, { count: episode.minutes })].filter(Boolean).join(" · ")}\n${episode.description}`, 1800);
      const epTitle = clip(episode.title, TEXT_MAX);
      const estimatedBytes = byteLength(epTitle) + byteLength(noteText) + (epURL ? byteLength(epURL) : 0) + 256;
      if (estimatedBytes > remainingBytes) break;
      remainingItems -= needed;
      remainingBytes -= estimatedBytes;
      if (epURL) episodeLinks.push(<Link key={uniqueKey(usedShowKeys, "episode-", String(episode.id))} title={epTitle} url={epURL} />);
      episodeNotes.push(text(uniqueKey(usedShowKeys, "episode-note-", String(episode.id)), noteText, "body"));
    }
    const readAt = readTime(result.at, tz, props.locale ?? service.locale);
    const footerText = clip(`${readAt ? fmt(t.readAt, { at: readAt }) : ""}${result.stale ? `${readAt ? " · " : ""}${errorText(result.stale, props.locale ?? service.locale)}` : ""}`, TEXT_MAX);
    return <View key="podcasts" title={clip(show.title, TEXT_MAX)} state={stateJSON}>
      <Section key="show-details" title={clip(show.title, TEXT_MAX)} footer={clip([show.publisher, show.genre, show.episodes === undefined ? "" : fmt(t.episodeCount, { count: show.episodes })].filter(Boolean).join(" · "), TEXT_MAX)}>
        {text("show-publisher", show.publisher || t.account, "body")}
      </Section>
      <Section key="show-actions"><Button key="show-save" title={clip(saved ? t.saved : t.save, TEXT_MAX)} action={act(`save:${show.id}`)} />{saved ? <Button key="show-remove" title={clip(t.remove, TEXT_MAX)} action={act(`remove:${show.id}`)} /> : null}{showURL ? <Link key="show-open" title={clip(t.openShow, TEXT_MAX)} url={showURL} /> : null}{feedURL ? <Link key="show-feed" title={clip(t.openFeed, TEXT_MAX)} url={feedURL} /> : null}<Button key="show-back" title={clip(t.back, TEXT_MAX)} action={act("back")} /></Section>
      {message}
      {episodeLinks.length ? <Section key="show-episodes" title={clip(t.episodes, TEXT_MAX)}>{episodeLinks}</Section> : <Section key="show-no-episodes">{text("show-no-episodes-text", t.noEpisodes)}</Section>}
      {episodeNotes.length ? <Section key="show-episode-notes" title={clip(t.episodes, TEXT_MAX)}>{episodeNotes}</Section> : null}
      <Section key="show-footer" footer={footerText}><Button key="show-refresh" title={clip(t.refresh, TEXT_MAX)} action={act("refresh")} /></Section>
      <Section key="show-feed-note">{text("show-feed-note-text", t.feedHint, "caption")}</Section>
      <Section key="show-attribution">{text("show-license", t.attribution, "caption")}</Section>
    </View>;
  }
  if (state.v === "library") {
    if (loaded.kind !== "library") throw new ReadError("invalid");
    const usedKeys = new Set(["podcast-navigation", "podcast-home", "podcast-library", "library-shows", "library-empty", "library-note", "library-note-text"]);
    let remainingItems = Math.max(0, ITEMS_MAX - 3);
    let remainingBytes = FRAME_MAX - byteLength(stateJSON) - 8192;
    const rows: Node[] = [];
    for (const show of loaded.shows) {
      if (remainingItems < 1) break;
      const rowKey = uniqueKey(usedKeys, "library-", String(show.id));
      const title = clip(show.title, TEXT_MAX);
      const subtitle = clip([show.publisher, show.genre].filter(Boolean).join(" · "), TEXT_MAX);
      const nextState: PageState = { v: "show", q: "", k: show.id, from: "library" as const, b: show };
      const nextStateJSON = uiVersion >= 3 ? encodeState(nextState) : "";
      const estimatedBytes = byteLength(rowKey) + byteLength(title) + byteLength(subtitle) + byteLength(nextStateJSON) + 384;
      if (estimatedBytes > remainingBytes) break;
      remainingItems -= 1;
      remainingBytes -= estimatedBytes;
      rows.push(<Row key={rowKey} title={title} subtitle={subtitle} action={act(`show:${show.id}`, nextState, title)} />);
    }
    return <View key="podcasts" title={clip(t.library, TEXT_MAX)} state={stateJSON}>{navigation}
      <Section key="library-shows" title={clip(t.library, TEXT_MAX)}>{rows.length ? rows : text("library-empty", t.libraryEmpty)}</Section>
      <Section key="library-note">{text("library-note-text", t.libraryHint, "caption")}</Section>
    </View>;
  }
  const usedKeys = new Set(["podcast-search", "podcast-query", "podcast-search-submit", "podcast-navigation", "podcast-home", "podcast-library", "podcast-results", "podcast-empty", "podcast-welcome", "podcast-welcome-text", "podcast-attribution", "podcast-license"]);
  let remainingItems = Math.max(0, ITEMS_MAX - 5);
  let remainingBytes = FRAME_MAX - byteLength(stateJSON) - 8192;
  const rows: Node[] = [];
  if (state.v === "search" && loaded.kind === "search") {
    for (const show of loaded.result.shows) {
      if (remainingItems < 1) break;
      const rowKey = uniqueKey(usedKeys, "show-", String(show.id));
      const title = clip(show.title, TEXT_MAX);
      const subtitle = clip([show.publisher, show.genre, show.episodes === undefined ? "" : fmt(t.episodeCount, { count: show.episodes })].filter(Boolean).join(" · "), TEXT_MAX);
      const nextState: PageState = { v: "show", q: state.q, k: show.id, b: show };
      const nextStateJSON = uiVersion >= 3 ? encodeState(nextState) : "";
      const estimatedBytes = byteLength(rowKey) + byteLength(title) + byteLength(subtitle) + byteLength(nextStateJSON) + 384;
      if (estimatedBytes > remainingBytes) break;
      remainingItems -= 1;
      remainingBytes -= estimatedBytes;
      rows.push(<Row key={rowKey} title={title} subtitle={subtitle} action={act(`show:${show.id}`, nextState, title)} />);
    }
  }
  return <View key="podcasts" title={clip(state.v === "search" ? fmt(t.results, { query: clip(state.q, 100) }) : t.discover, TEXT_MAX)} state={stateJSON}>{search}{navigation}
    {state.v === "search" ? <Section key="podcast-results" title={clip(fmt(t.results, { query: clip(state.q, 100) }), TEXT_MAX)}>
      {rows.length ? rows : text("podcast-empty", t.empty)}
    </Section> : <Section key="podcast-welcome">{text("podcast-welcome-text", t.searchHint, "body")}</Section>}
    <Section key="podcast-attribution">{text("podcast-license", t.attribution, "caption")}</Section>
  </View>;
}
