export const limits = Object.freeze({ request: 12_000, page: 18_000, tool: 18_000, response: 1 << 20, bootstrap: 24 * 60 * 60_000, fresh: 15 * 60_000, cache: 48, cacheBytes: 2 << 20, gap: 350 });
export type LimitOptions = { [K in keyof typeof limits]?: number };
export type ReadCode = "invalid" | "bad_answer" | "not_found" | "unsupported" | "rate_limited" | "slow" | "network" | "http" | "storage";
export class ReadError extends Error { constructor(readonly code: ReadCode, readonly status?: number) { super(code); } }
export const clip = (text: string, most: number) => {
  if (most <= 0) return "";
  const chars = Array.from(text);
  if (chars.length <= most) return text;
  if (most === 1) return chars[0];
  return chars.slice(0, most - 1).join("").trimEnd() + "…";
};
export const plain = (text: string) => text.replace(/\s+/gu, " ").trim();
const hasLoneSurrogate = (value: string) => /[\ud800-\udbff](?![\udc00-\udfff])|(?<![\ud800-\udbff])[\udc00-\udfff]/.test(value);
function object(value: unknown): Record<string, unknown> { if (!value || typeof value !== "object" || Array.isArray(value)) throw new ReadError("bad_answer"); return value as Record<string, unknown>; }
function array(value: unknown): unknown[] { if (!Array.isArray(value)) throw new ReadError("bad_answer"); return value; }
function string(value: unknown): string { if (typeof value !== "string") throw new ReadError("bad_answer"); return value; }
export interface DomainName { ascii: string; unicode: string }
export function domainName(raw: unknown): DomainName {
  if (typeof raw !== "string" || raw.length > 1024 || /[\u0000- \u007f\\/@#?&=]/u.test(raw) || hasLoneSurrogate(raw)) throw new ReadError("invalid");
  let unicode: string;
  try { unicode = raw.trim().toLowerCase().replace(/\.$/u, "").normalize("NFC"); } catch { throw new ReadError("invalid"); }
  if (!unicode || unicode.length > 253 || /[_*:%]/u.test(unicode) || /^\d{1,3}(?:\.\d{1,3}){3}$/u.test(unicode)) throw new ReadError("invalid");
  const labels = unicode.split(".");
  if (labels.length < 2 || labels.some(label => !label || label.length > 63)) throw new ReadError("invalid");
  const asciiLabels = labels.map(label => {
    let ascii: string;
    try { ascii = new URL(`https://${label}.invalid`).hostname.slice(0, -8); }
    catch { throw new ReadError("invalid"); }
    if (!ascii || ascii.length > 63 || !/^[a-z0-9](?:[a-z0-9-]*[a-z0-9])?$/u.test(ascii) || /^xn--/u.test(label) && !/^xn--[a-z0-9-]+$/u.test(label)) throw new ReadError("invalid");
    return ascii;
  });
  return { ascii: asciiLabels.join("."), unicode };
}
export interface RDAPService { origin: string; base: string }
export function rdapService(raw: unknown, domain: string): RDAPService {
  if (!["com", "net"].includes(domainName(domain).ascii.split(".").at(-1)!)) throw new ReadError("unsupported");
  const root = object(raw), suffix = domainName(domain).ascii.split(".").at(-1)!;
  for (const entry of array(root.services).slice(0, 4096)) {
    const pair = array(entry), zones = array(pair[0]).map(string).map(zone => zone.toLowerCase()), urls = array(pair[1]);
    if (!zones.includes(suffix)) continue;
    const candidates = urls.map(value => {
      try {
        const url = new URL(string(value));
        if (url.protocol !== "https:" || url.username || url.password || url.port || url.search || url.hash || !url.pathname.endsWith("/")) return "";
        if (url.origin !== "https://rdap.verisign.com" || url.pathname !== `/${suffix}/v1/` || url.toString().length > 4096) return "";
        return url.toString();
      } catch { return ""; }
    }).filter(Boolean);
    if (candidates.length !== 1) throw new ReadError("unsupported");
    return { origin: new URL(candidates[0]).origin, base: candidates[0] };
  }
  throw new ReadError("unsupported");
}
export function lookupURL(service: RDAPService, domain: string): string {
  return `${service.base}domain/${encodeURIComponent(domainName(domain).ascii)}`;
}
export interface RegistrationEvent { action: string; at: string }
export interface Registration {
  domain: DomainName; name: string; statuses: string[]; events: RegistrationEvent[];
  registrar?: string; url: string; source: string; at: number; stale?: ReadError;
}
function publicLink(value: unknown, kind: "self" | "iana"): string {
  let url: URL;
  try { url = new URL(string(value)); } catch { throw new ReadError("bad_answer"); }
  if (url.protocol !== "https:" || url.username || url.password || url.port) throw new ReadError("bad_answer");
  if (kind === "iana") {
    if (url.origin !== "https://www.iana.org") throw new ReadError("bad_answer");
  } else if (url.hostname === "localhost" || url.hostname === "127.0.0.1" || url.hostname === "::1" || !url.hostname.includes(".")) throw new ReadError("bad_answer");
  url.hash = "";
  const href = url.toString();
  if (href.length > 4096) throw new ReadError("bad_answer");
  return href;
}
function registrar(raw: unknown): string | undefined {
  if (!Array.isArray(raw)) return undefined;
  for (const entity of raw.slice(0, 32)) {
    const item = object(entity), roles = array(item.roles ?? []);
    if (!roles.includes("registrar")) continue;
    const fn = Array.isArray(item.vcardArray) && Array.isArray(item.vcardArray[1])
      ? (item.vcardArray[1] as unknown[]).find(row => Array.isArray(row) && row[0] === "fn" && typeof row[3] === "string") : undefined;
    if (fn) {
      const value = clip(plain((fn as unknown[])[3] as string), 300);
      if (value) return value;
    }
  }
  return undefined;
}
function date(value: unknown): string {
  const raw = string(value);
  const match = /^(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2}):(\d{2})(?:\.\d{1,9})?(Z|([+-])(\d{2}):(\d{2}))$/u.exec(raw);
  if (!match) throw new ReadError("bad_answer");
  const y = Number(match[1]), m = Number(match[2]), d = Number(match[3]);
  const hh = Number(match[4]), mm = Number(match[5]), ss = Number(match[6]);
  if (hh > 23 || mm > 59 || ss > 59) throw new ReadError("bad_answer");
  if (match[7] !== "Z") {
    const oh = Number(match[9]), om = Number(match[10]);
    if (oh > 23 || om > 59) throw new ReadError("bad_answer");
  }
  // Validate the local calendar day itself (Date.parse rolls impossible days like Feb 30 into March).
  const calendar = new Date(Date.UTC(y, m - 1, d));
  if (calendar.getUTCFullYear() !== y || calendar.getUTCMonth() !== m - 1 || calendar.getUTCDate() !== d) throw new ReadError("bad_answer");
  const ms = Date.parse(raw);
  if (!Number.isFinite(ms)) throw new ReadError("bad_answer");
  return new Date(ms).toISOString();
}
export function parseRegistration(raw: unknown, domain: string, at: number): Registration {
  const requested = domainName(domain), root = object(raw);
  if (root.objectClassName !== "domain") throw new ReadError("bad_answer");
  let name = requested.ascii;
  if (root.ldhName != null) {
    try { name = domainName(string(root.ldhName)).ascii; }
    catch { throw new ReadError("bad_answer"); }
  }
  if (name !== requested.ascii) throw new ReadError("bad_answer");
  const statuses = array(root.status ?? []).slice(0, 32).map(value => clip(plain(string(value)), 160)).filter(Boolean);
  const events = array(root.events ?? []).slice(0, 32).map(value => {
    const item = object(value);
    const action = clip(plain(string(item.eventAction)), 80);
    if (!action) throw new ReadError("bad_answer");
    return { action, at: date(item.eventDate) };
  });
  const self = array(root.links ?? []).slice(0, 32).find(link => object(link).rel === "self");
  const url = publicLink(object(self ?? {}).href, "self");
  return { domain: requested, name, statuses, events, registrar: registrar(root.entities), url, source: new URL(url).origin, at };
}
export const statusURL = "https://www.iana.org/assignments/rdap-status-values/rdap-status-values.xhtml";
