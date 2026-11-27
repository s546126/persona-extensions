export const limits = Object.freeze({ request: 12_000, page: 15_000, tool: 15_000, response: 2 << 20, fresh: 60 * 60_000, cache: 40, cacheBytes: 2 << 20, gap: 1_000 });
export type LimitOptions = { [K in keyof typeof limits]?: number };
export type ReadCode = "invalid" | "bad_answer" | "not_found" | "rate_limited" | "slow" | "network" | "http";
export class ReadError extends Error {
  constructor(readonly code: ReadCode, readonly status?: number) { super(code); }
}
export const sites = Object.freeze({
  stackoverflow: { name: "Stack Overflow", host: "stackoverflow.com" },
  superuser: { name: "Super User", host: "superuser.com" },
  serverfault: { name: "Server Fault", host: "serverfault.com" },
  unix: { name: "Unix & Linux", host: "unix.stackexchange.com" },
  askubuntu: { name: "Ask Ubuntu", host: "askubuntu.com" },
  apple: { name: "Ask Different", host: "apple.stackexchange.com" },
  softwareengineering: { name: "Software Engineering", host: "softwareengineering.stackexchange.com" },
  codereview: { name: "Code Review", host: "codereview.stackexchange.com" },
  webapps: { name: "Web Applications", host: "webapps.stackexchange.com" },
  dba: { name: "Database Administrators", host: "dba.stackexchange.com" },
  devops: { name: "DevOps", host: "devops.stackexchange.com" },
  security: { name: "Information Security", host: "security.stackexchange.com" }
} as const);
export type Site = keyof typeof sites;
export const siteIDs = Object.keys(sites) as Site[];
export const clip = (text: string, most: number) => {
  if (most <= 0) return "";
  const chars = Array.from(text);
  if (chars.length <= most) return text;
  if (most === 1) return chars[0];
  return chars.slice(0, most - 1).join("").trimEnd() + "…";
};
export const oneLine = (text: string) => text.replace(/\s+/gu, " ").trim();
const hasLoneSurrogate = (value: string) => /[\ud800-\udbff](?![\udc00-\udfff])|(?<![\ud800-\udbff])[\udc00-\udfff]/.test(value);
export function queryText(value: unknown): string {
  if (typeof value !== "string" || /[\u0000-\u001f\u007f]/u.test(value) || hasLoneSurrogate(value)) throw new ReadError("invalid");
  const query = oneLine(value);
  if (!query || Array.from(query).length > 200) throw new ReadError("invalid");
  return query;
}
export function titleText(value: unknown): string {
  if (typeof value !== "string" || /[\u0000-\u001f\u007f]/u.test(value) || hasLoneSurrogate(value)) throw new ReadError("invalid");
  const title = oneLine(value);
  if (!title || Array.from(title).length > 500) throw new ReadError("invalid");
  return title;
}
export function siteID(value: unknown): Site {
  if (value == null || value === "") return "stackoverflow";
  if (typeof value !== "string" || !Object.hasOwn(sites, value)) throw new ReadError("invalid");
  return value as Site;
}
export function tagText(value: unknown): string {
  if (value == null || value === "") return "";
  if (typeof value !== "string" || !/^[a-z0-9][a-z0-9+#.-]{0,59}$/.test(value) || /[.-]$/.test(value)) throw new ReadError("invalid");
  return value;
}
export function questionID(value: unknown): number {
  if (typeof value === "string" && /^[1-9][0-9]{0,9}$/.test(value)) value = Number(value);
  if (typeof value !== "number" || !Number.isSafeInteger(value) || value < 1 || value > 9_999_999_999) throw new ReadError("invalid");
  return value;
}
const questionFilter = "!*10eWkBRxImNF3.k5tm)KG1";
const answerFilter = "!)bSBcEtaH)4v4wIT.S(z)a7Ht5P";
export function searchPath(query: string, site: Site, tag = "", limit = 12): string {
  if (!Number.isSafeInteger(limit) || limit < 1 || limit > 20) throw new ReadError("invalid");
  const params = new URLSearchParams({ site: siteID(site), q: queryText(query), pagesize: String(limit), order: "desc", sort: "relevance", filter: questionFilter });
  if (tagText(tag)) params.set("tagged", tagText(tag));
  return `/2.3/search/advanced?${params}`;
}
export function answersPath(id: number, site: Site): string {
  return `/2.3/questions/${questionID(id)}/answers?${new URLSearchParams({ site: siteID(site), pagesize: "10", order: "desc", sort: "votes", filter: answerFilter })}`;
}
function object(value: unknown): Record<string, unknown> {
  if (!value || typeof value !== "object" || Array.isArray(value)) throw new ReadError("bad_answer");
  return value as Record<string, unknown>;
}
function array(value: unknown): unknown[] { if (!Array.isArray(value)) throw new ReadError("bad_answer"); return value; }
function string(value: unknown, fallback?: string): string {
  if (value == null && fallback !== undefined) return fallback;
  if (typeof value !== "string") throw new ReadError("bad_answer");
  return value;
}
function integer(value: unknown, minimum = 0, maximum = 1_000_000_000): number {
  if (typeof value !== "number" || !Number.isSafeInteger(value) || value < minimum || value > maximum) throw new ReadError("bad_answer");
  return value;
}
function boolean(value: unknown): boolean { if (typeof value !== "boolean") throw new ReadError("bad_answer"); return value; }
const entities: Record<string, string> = { amp: "&", lt: "<", gt: ">", quot: '"', apos: "'", nbsp: " ", ndash: "–", mdash: "—", hellip: "…", rsquo: "’", lsquo: "‘", ldquo: "“", rdquo: "”", times: "×", copy: "©", reg: "®", trade: "™", eacute: "é", egrave: "è", agrave: "à", ccedil: "ç", ouml: "ö", uuml: "ü", auml: "ä", szlig: "ß" };
export function decodeEntities(value: string): string {
  return value.replace(/&(?:#(\d{1,7})|#x([a-fA-F0-9]{1,6})|([a-zA-Z][a-zA-Z0-9]{1,30}));/g, (raw, decimal: string, hex: string, name: string) => {
    if (name) return entities[name] ?? entities[name.toLowerCase()] ?? raw;
    const point = Number.parseInt(decimal ?? hex, decimal ? 10 : 16);
    if (point > 0x10ffff || (point >= 0xd800 && point <= 0xdfff) || (point < 32 && point !== 9 && point !== 10 && point !== 13)) return raw;
    return String.fromCodePoint(point);
  });
}
function canonicalURL(raw: string, host: string, path: RegExp): string {
  let url: URL;
  try { url = new URL(decodeEntities(raw)); } catch { throw new ReadError("bad_answer"); }
  if (url.protocol !== "https:" || url.hostname !== host || url.port || url.username || url.password || !path.test(url.pathname)) throw new ReadError("bad_answer");
  url.search = "";
  url.hash = "";
  if (url.href.length > 4096) throw new ReadError("bad_answer");
  return url.href;
}
export interface Question {
  id: number; title: string; url: string; score: number; answerCount: number;
  acceptedAnswerID?: number; tags: string[]; license: string; author: string; authorURL?: string;
}
export interface Answer {
  id: number; questionID: number; body: string; score: number; accepted: boolean;
  url: string; author: string; authorURL?: string; license: string;
}
export interface SearchResult { questions: Question[]; at: number; quotaRemaining?: number; stale?: ReadError }
export interface AnswerResult { answers: Answer[]; at: number; quotaRemaining?: number; stale?: ReadError }
function wrapper(raw: unknown) {
  const root = object(raw);
  if (root.error_id != null || root.error_message != null || root.error_name != null) throw new ReadError("bad_answer");
  const backoff = root.backoff == null ? 0 : integer(root.backoff, 0, 3_600);
  const quotaRemaining = root.quota_remaining == null ? undefined : integer(root.quota_remaining, 0, 1_000_000);
  const items = array(root.items);
  return { items, backoff, quotaRemaining };
}
function person(raw: unknown, host: string) {
  if (raw == null) return { author: "Anonymous" };
  const owner = object(raw), author = clip(oneLine(decodeEntities(string(owner.display_name))), 200);
  if (!author) throw new ReadError("bad_answer");
  const link = owner.link == null ? undefined : canonicalURL(string(owner.link), host, /^\/users\/[1-9][0-9]*(?:\/|$)/);
  return { author, ...(link ? { authorURL: link } : {}) };
}
function license(raw: unknown): string {
  const value = string(raw, "CC BY-SA");
  if (!/^CC BY-SA (?:2\.5|3\.0|4\.0)$/.test(value)) throw new ReadError("bad_answer");
  return value;
}
export function parseQuestions(raw: unknown, site: Site, at: number): SearchResult {
  const { items, quotaRemaining } = wrapper(raw), { host } = sites[site], seen = new Set<number>();
  const questions = items.slice(0, 20).map(item => {
    const question = object(item), id = integer(question.question_id, 1, 9_999_999_999);
    if (seen.has(id)) throw new ReadError("bad_answer"); seen.add(id);
    const title = clip(oneLine(decodeEntities(string(question.title))), 500);
    if (!title) throw new ReadError("bad_answer");
    const tags = array(question.tags).slice(0, 10).map(value => {
      const rawTag = string(value);
      if (!rawTag) throw new ReadError("bad_answer");
      try { return tagText(rawTag); } catch { throw new ReadError("bad_answer"); }
    });
    if (new Set(tags).size !== tags.length) throw new ReadError("bad_answer");
    const acceptedAnswerID = question.accepted_answer_id == null ? undefined : integer(question.accepted_answer_id, 1, 9_999_999_999);
    return { id, title, url: canonicalURL(string(question.link), host, new RegExp(`^/questions/${id}(?:/|$)`)), score: integer(question.score, -1_000_000), answerCount: integer(question.answer_count), ...(acceptedAnswerID ? { acceptedAnswerID } : {}), tags, license: license(question.content_license), ...person(question.owner, host) };
  });
  return { questions, at, ...(quotaRemaining === undefined ? {} : { quotaRemaining }) };
}
/** Convert the API's trusted HTML answer subset into SDK Markdown; never render HTML. */
function markdownText(value: string): string {
  return value
    .replace(/`/g, "'")
    .replace(/\\/g, "\\\\")
    .replace(/([*_[\]])/g, "\\$1")
    .replace(/\\\]\(/g, "\\]\\(")
    .replace(/\\\]\:/g, "\\]\\:")
    .replace(/<([A-Za-z][A-Za-z0-9+.-]*):(?=[^<> \t\r\n\f]*>)/g, "<$1\\:")
    .replace(/^([ \t]*)(#{1,6})(?= )/gm, "$1\\$2")
    .replace(/^([ \t]*)([+-]|\d+\.)(?= )/gm, "$1\\$2")
    .replace(/^([ \t]*)>/gm, "$1\\>");
}
function escapeHTMLText(value: string): string {
  return value.replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;");
}
function safeLinkHref(raw: string): string | undefined {
  let url: URL;
  try { url = new URL(raw.trim()); } catch { return undefined; }
  if (url.protocol !== "https:" || !url.hostname || url.username || url.password) return undefined;
  return url.href
    .replace(/%(?![0-9a-fA-F]{2})/g, "%25")
    .replace(/\(/g, "%28")
    .replace(/\)/g, "%29")
    .replace(/</g, "%3C")
    .replace(/>/g, "%3E")
    .replace(/\[/g, "%5B")
    .replace(/\]/g, "%5D")
    .replace(/`/g, "%60")
    .replace(/\s/g, "%20");
}
function wrapInlineTag(text: string, tagPattern: RegExp, sentinel: string): string {
  let prev: string;
  let next = text;
  for (let pass = 0; pass < 4; pass++) {
    prev = next;
    next = next.replace(tagPattern, (_, inner: string) => {
      const lead = /^\s/.test(inner) ? " " : "";
      const trail = /\s$/.test(inner) ? " " : "";
      const body = inner.trim().replace(/\r?\n+/g, " ");
      if (!body.replace(/<[^>]*>/g, "").trim()) return lead || trail ? " " : "";
      return `${lead}${sentinel}${body}${sentinel}${trail}`;
    });
    if (next === prev) break;
  }
  return next;
}
export function clipMarkdown(text: string, most: number): string {
  if (most <= 0) return "";
  const chars = Array.from(text);
  if (chars.length <= most) return text;
  if (most === 1) return chars[0];
  let openFence: string | undefined;
  let kept = "";
  let used = 0;
  const reserve = 24;
  const budget = Math.max(1, most - reserve);
  const lines = text.split("\n");
  for (let i = 0; i < lines.length; i++) {
    const line = lines[i];
    const lineChars = Array.from(line);
    const sep = i === 0 ? 0 : 1;
    if (used + sep + lineChars.length <= budget) {
      if (sep) { kept += "\n"; used++; }
      kept += line;
      used += lineChars.length;
      const fenceMatch = /^(`{3,})/.exec(line.trimStart());
      if (fenceMatch) {
        if (!openFence) openFence = fenceMatch[1];
        else if (fenceMatch[1].length >= openFence.length && line.trim() === fenceMatch[1]) openFence = undefined;
      }
      continue;
    }
    const room = budget - used - sep;
    const isClosingFence = openFence !== undefined && line.trim() === openFence;
    if (room > 0 && !isClosingFence) {
      let piece = lineChars.slice(0, room).join("");
      if (!openFence) {
        // Avoid leaving an unclosed inline backtick, broken Markdown link, or partial URI autolink at the cut boundary.
        const ticks = (piece.match(/`/g) ?? []).length;
        if (ticks % 2 === 1) piece = piece.slice(0, piece.lastIndexOf("`"));
        piece = piece.replace(/!?\[[^\]]*(?:\]\([^)]*)?$/, "").replace(/\]\([^)]*$/, "").replace(/<[A-Za-z][A-Za-z0-9+.-]*:[^<> \t\r\n\f]*$/, "");
      } else if (/^`+$/.test(piece.trim())) {
        piece = "";
      }
      piece = piece.trimEnd();
      if (piece) {
        if (sep) kept += "\n";
        kept += piece;
      }
    }
    break;
  }
  kept = kept.trimEnd();
  const suffix = openFence ? `\n${openFence}\n…` : "…";
  const maxBody = most - Array.from(suffix).length;
  if (maxBody <= 0) return chars.slice(0, most).join("");
  const bodyChars = Array.from(kept);
  if (bodyChars.length > maxBody) kept = bodyChars.slice(0, maxBody).join("").trimEnd();
  const out = kept ? kept + suffix : "…";
  return Array.from(out).length <= most ? out : Array.from(out).slice(0, most).join("");
}
export function answerMarkdown(raw: string): string {
  if (raw.length > 200_000) throw new ReadError("bad_answer");
  // Strip C0 control characters (except tab, LF, CR) so internal sentinels cannot collide with untrusted input.
  const cleaned = raw.replace(/[\u0000-\u0008\u000b\u000c\u000e-\u001f]/g, "");
  const blocks: string[] = [];
  const saved = cleaned.replace(/<pre\b[^>]*>([\s\S]*?)<\/pre>/gi, (_, code: string) => {
    const text = decodeEntities(code.replace(/<code\b[^>]*>/gi, "").replace(/<\/code>/gi, "").replace(/<[^>]+>/g, ""));
    // Neutralize inner runs of 3+ backticks so Go wire.CheckView's non-greedy ```...``` matcher never closes early.
    const content = text.replace(/^\n+|\n+$/g, "").replace(/`{3,}/g, run => "'".repeat(run.length));
    blocks.push("```\n" + content + "\n```");
    return `\n\n\u0007${blocks.length - 1}\u0008\n\n`;
  });
  let text = saved.replace(/<script\b[\s\S]*?<\/script>/gi, "").replace(/<style\b[\s\S]*?<\/style>/gi, "");
  const links: string[] = [];
  text = text.replace(/<a\b[^>]*href\s*=\s*(?:"([^"]*)"|'([^']*)')[^>]*>([\s\S]*?)<\/a>/gi, (_, double: string, single: string, label: string) => {
    const href = decodeEntities(double ?? single ?? "");
    const plain = oneLine(decodeEntities(label.replace(/<[^>]+>/g, "")));
    if (!plain) return "";
    const safeHref = safeLinkHref(href);
    // Keep rejected-link labels in normal prose flow (HTML-escaped) so surrounding brackets/autolinks escape cleanly in one pass.
    if (!safeHref) return escapeHTMLText(plain);
    links.push(`[${markdownText(plain)}](${safeHref})`);
    return `\u0005${links.length - 1}\u0006`;
  });
  text = text.replace(/<img\b[^>]*alt\s*=\s*(?:"([^"]*)"|'([^']*)')[^>]*>/gi, "$1$2");
  const inlineCode: string[] = [];
  text = text.replace(/<code\b[^>]*>([\s\S]*?)<\/code>/gi, (_, code: string) => {
    if (/\u0007\d+\u0008/.test(code)) return code;
    const content = oneLine(decodeEntities(code.replace(/<[^>]+>/g, ""))).replace(/`/g, "'");
    if (!content) return "";
    inlineCode.push("`" + content + "`");
    return `\u0003${inlineCode.length - 1}\u0004`;
  });
  text = text.replace(/<h([1-6])\b[^>]*>([\s\S]*?)<\/h\1>/gi, (_, _level: string, inner: string) => {
    if (/\u0007\d+\u0008/.test(inner)) return `\n\n${inner}\n\n`;
    const heading = inner.replace(/\r?\n+/g, " ").trim();
    if (!heading.replace(/<[^>]*>/g, "").trim()) return "\n\n";
    return `\n\n\u0011${heading}\n\n`;
  });
  text = wrapInlineTag(text, /<(?:strong|b)\b[^>]*>([\s\S]*?)<\/(?:strong|b)>/gi, "\u0014");
  text = wrapInlineTag(text, /<(?:em|i)\b[^>]*>([\s\S]*?)<\/(?:em|i)>/gi, "\u0015");
  text = text.replace(/<li\b[^>]*>(?:\s|<p\b[^>]*>)*/gi, "\n\u0012");
  text = text.replace(/<br\s*\/?>/gi, "\n");
  text = text.replace(/<blockquote\b[^>]*>([\s\S]*?)<\/blockquote>/gi, (_, inner: string) => {
    const prepared = inner.replace(/<\/(?:p|div|li|ul|ol|h[1-6]|table|tr)>/gi, "\n\n").replace(/<[^>]*>/g, "");
    const quoted = prepared.split(/\r?\n/).map(line => {
      const trimmed = line.trim();
      if (!trimmed) return "";
      if (/^\u0007\d+\u0008$/.test(trimmed)) return `\n\n${trimmed}\n\n`;
      return `\u0013${trimmed}`;
    }).filter(Boolean).join("\n");
    return quoted ? `\n\n${quoted}\n\n` : "\n\n";
  });
  text = text.replace(/<\/(?:p|div|li|ul|ol|blockquote|h[1-6]|table|tr)>/gi, "\n\n");
  text = decodeEntities(text.replace(/<[^>]*>/g, ""));
  // Keep fenced code block placeholders on their own paragraphs, never inline after list/heading/quote markers.
  text = text
    .replace(/[\u0011\u0012\u0013][ \t]*(\u0007\d+\u0008)/g, "\n\n$1\n\n")
    .replace(/(\u0007\d+\u0008)/g, "\n\n$1\n\n");
  text = text
    .split(/\r?\n/)
    .map(line => {
      const prefix = /^([\u0011\u0012\u0013])(.*)$/.exec(line.trimStart());
      if (prefix) {
        const body = markdownText(prefix[2].trimStart());
        return body.trim() ? prefix[1] + body : "";
      }
      return markdownText(line);
    })
    .join("\n")
    .replace(/[ \t]+\n/g, "\n")
    .replace(/\n{3,}/g, "\n\n")
    .trim();
  // Prevent cross-span adjacency that would trip wire.CheckView after mdCode strips inline code spans.
  text = text
    .replace(/!(?=(?:\u0003\d+\u0004)*\u0005)/g, "! ")
    .replace(/(\\\](?:\u0003\d+\u0004)+)\(/g, "$1\\(")
    .replace(/(\\\](?:\u0003\d+\u0004)+):/g, "$1\\:")
    .replace(/<((?:\u0003\d+\u0004)*[A-Za-z](?:[A-Za-z0-9+.-]|\u0003\d+\u0004)*):(?=[^<> \t\r\n\f]*>)/g, "<$1\\:");
  text = text
    .replace(/\u0011/g, "### ")
    .replace(/\u0012/g, "- ")
    .replace(/\u0013/g, "> ")
    .replace(/\u0014/g, "**")
    .replace(/\u0015/g, "*")
    .replace(/\u0003(\d+)\u0004/g, (_, index: string) => inlineCode[Number(index)] ?? "")
    .replace(/\u0005(\d+)\u0006/g, (_, index: string) => links[Number(index)] ?? "")
    .replace(/\u0007(\d+)\u0008/g, (_, index: string) => blocks[Number(index)] ?? "")
    .replace(/\n{3,}/g, "\n\n")
    .trim();
  if (!text || Array.from(text).length > 30_000) throw new ReadError("bad_answer");
  return text;
}
export function parseAnswers(raw: unknown, id: number, site: Site, at: number): AnswerResult {
  const { items, quotaRemaining } = wrapper(raw), { host } = sites[site], seen = new Set<number>();
  const answers = items.slice(0, 10).map(item => {
    const answer = object(item), answerID = integer(answer.answer_id, 1, 9_999_999_999), question = integer(answer.question_id, 1, 9_999_999_999);
    if (question !== questionID(id) || seen.has(answerID)) throw new ReadError("bad_answer"); seen.add(answerID);
    return { id: answerID, questionID: question, body: answerMarkdown(string(answer.body)), score: integer(answer.score, -1_000_000), accepted: boolean(answer.is_accepted), url: canonicalURL(string(answer.link), host, new RegExp(`^/questions/${question}(?:/|$)`)), license: license(answer.content_license), ...person(answer.owner, host) };
  });
  return { answers, at, ...(quotaRemaining === undefined ? {} : { quotaRemaining }) };
}
export function selectedAnswer(answers: Answer[]): Answer {
  const selected = answers.find(answer => answer.accepted) ?? answers[0];
  if (!selected) throw new ReadError("not_found");
  return selected;
}
