#!/usr/bin/env python3
"""Read exact cumulative GHCR downloads from public GitHub package detail pages.

The documented Packages REST API does not expose download counts. Never use a
rounded list-page value, a release asset count, or zero on an HTTP/parser failure.
"""
import concurrent.futures
import datetime
import email.utils
import html.parser
import re
import sys
import threading
import time
import urllib.error
import urllib.parse
import urllib.request

MAX_PAGE_BYTES = 4 * 1024 * 1024
MAX_ATTEMPTS = 6
MAX_WORKERS = 2
MIN_REQUEST_INTERVAL = 0.45
MAX_RETRY_AFTER = 120.0


class DownloadParser(html.parser.HTMLParser):
    def __init__(self):
        super().__init__(convert_charrefs=True)
        self.span = None
        self.after_label = False
        self.values = []

    def handle_starttag(self, tag, attrs):
        if tag == "span":
            self.span = []
        elif tag == "h3" and self.after_label:
            title = dict(attrs).get("title", "")
            if not re.fullmatch(r"[0-9]+|[0-9]{1,3}(?:,[0-9]{3})+", title):
                raise ValueError("GitHub total downloads is not an exact integer")
            self.values.append(int(title.replace(",", "")))
            self.after_label = False

    def handle_data(self, data):
        if self.span is not None:
            self.span.append(data)

    def handle_endtag(self, tag):
        if tag == "span" and self.span is not None:
            if "".join(self.span).strip() == "Total downloads":
                self.after_label = True
            self.span = None


def parse_downloads(text):
    parser = DownloadParser()
    parser.feed(text)
    parser.close()
    if len(parser.values) != 1:
        raise ValueError("GitHub package page must have exactly one Total downloads counter")
    return parser.values[0]


def package_url(extension_id):
    if not re.fullmatch(r"[a-z0-9]+(?:-[a-z0-9]+)*", extension_id):
        raise ValueError("unsafe extension id for downloads")
    return "https://github.com/nwverse/persona-extensions/pkgs/container/" + urllib.parse.quote(
        "persona-extensions/" + extension_id, safe="")


def parse_retry_after(value, now=None):
    if not value:
        return None
    text = value.strip()
    if not text:
        return None
    try:
        seconds = float(text)
    except ValueError:
        try:
            dt = email.utils.parsedate_to_datetime(text)
        except (TypeError, ValueError, IndexError):
            return None
        if dt.tzinfo is None:
            dt = dt.replace(tzinfo=datetime.timezone.utc)
        ref = now if now is not None else datetime.datetime.now(datetime.timezone.utc)
        seconds = (dt - ref).total_seconds()
    if seconds != seconds:
        return None
    return min(MAX_RETRY_AFTER, max(1.0, seconds))


def retry_delay(error, attempt):
    if isinstance(error, urllib.error.HTTPError) and error.code == 429:
        header = error.headers.get("Retry-After") if error.headers else None
        parsed = parse_retry_after(header)
        if parsed is not None:
            return parsed
        return min(60.0, 15.0 * (2 ** attempt))
    return min(30.0, float(2 ** attempt))


class Pacer:
    """Coordinate request cadence and 429 cooldown across download worker threads."""

    def __init__(self, min_interval=MIN_REQUEST_INTERVAL, clock=time.monotonic, sleeper=time.sleep):
        self.min_interval = min_interval
        self.clock = clock
        self.sleeper = sleeper
        self.lock = threading.Lock()
        self.next_at = 0.0

    def wait(self):
        # Claim a slot only when it is due. A worker that slept toward a slot
        # checks again on waking, so a 429 cooldown that a peer set meanwhile
        # still holds it back instead of letting it fire inside the cooldown.
        slept_until = None
        while True:
            with self.lock:
                now = self.clock()
                if slept_until is not None and slept_until > now:
                    now = slept_until
                if self.next_at <= now:
                    self.next_at = now + self.min_interval
                    return
                slept_until = self.next_at
                delay = slept_until - now
            self.sleeper(delay)

    def backoff(self, delay):
        if delay <= 0:
            return
        with self.lock:
            deadline = self.clock() + delay
            if deadline > self.next_at:
                self.next_at = deadline


def read_downloads(extension_id, pacer=None, sleeper=time.sleep):
    url = package_url(extension_id)
    request = urllib.request.Request(url, headers={"User-Agent": "Persona-Extensions-Catalog/1.0"})
    for attempt in range(MAX_ATTEMPTS):
        if pacer is not None:
            pacer.wait()
        try:
            with urllib.request.urlopen(request, timeout=45) as response:
                final = urllib.parse.urlsplit(response.url)
                if response.status != 200 or final.scheme != "https" or final.netloc != "github.com":
                    raise ValueError("GitHub package statistics response identity mismatch")
                data = response.read(MAX_PAGE_BYTES + 1)
                if len(data) > MAX_PAGE_BYTES:
                    raise ValueError("GitHub package statistics page exceeds size limit")
                return parse_downloads(data.decode("utf-8"))
        except urllib.error.HTTPError as error:
            # The error carries the open response; its code and headers stay readable after close.
            error.close()
            if error.code not in (429, 500, 502, 503, 504) or attempt == MAX_ATTEMPTS - 1:
                raise ValueError("GitHub package downloads unavailable for " + extension_id) from None
            delay = retry_delay(error, attempt)
            print(
                f"downloads: retrying {extension_id} after HTTP {error.code} in {delay:.1f}s "
                f"(attempt {attempt + 1}/{MAX_ATTEMPTS})",
                file=sys.stderr,
            )
            if pacer is not None and error.code == 429:
                # Hold every peer back for the cooldown, and wait it out here as well.
                pacer.backoff(delay)
            sleeper(delay)
        except (urllib.error.URLError, TimeoutError, OSError):
            if attempt == MAX_ATTEMPTS - 1:
                raise ValueError("GitHub package downloads unavailable for " + extension_id) from None
            delay = retry_delay(None, attempt)
            print(
                f"downloads: retrying {extension_id} after transport error in {delay:.1f}s "
                f"(attempt {attempt + 1}/{MAX_ATTEMPTS})",
                file=sys.stderr,
            )
            sleeper(delay)
    raise ValueError("GitHub package downloads unavailable for " + extension_id)


def snapshot(extension_ids, reader=read_downloads):
    ids = sorted(extension_ids)
    if len(set(ids)) != len(ids):
        raise ValueError("duplicate extension ids in downloads snapshot")
    if reader is read_downloads:
        pacer = Pacer(min_interval=MIN_REQUEST_INTERVAL)
        worker = lambda ext_id: read_downloads(ext_id, pacer=pacer)
    else:
        worker = reader
    with concurrent.futures.ThreadPoolExecutor(max_workers=MAX_WORKERS) as pool:
        counts = dict(zip(ids, pool.map(worker, ids)))
    if any(type(value) is not int or value < 0 for value in counts.values()):
        raise ValueError("downloads must be nonnegative integers")
    timestamp = datetime.datetime.now(datetime.timezone.utc).isoformat(timespec="seconds").replace("+00:00", "Z")
    return {"downloads_updated_at": timestamp, "downloads": counts}


def apply(body, metadata):
    entries = body["extensions"]
    counts = metadata["downloads"]
    if set(counts) != {entry["manifest"]["id"] for entry in entries}:
        raise ValueError("downloads snapshot does not cover the complete catalog")
    if any(type(value) is not int or value < 0 for value in counts.values()):
        raise ValueError("downloads must be nonnegative integers")
    for entry in entries:
        entry["downloads"] = counts[entry["manifest"]["id"]]
    entries.sort(key=lambda entry: (-entry["downloads"], entry["manifest"]["id"]))
    body["downloads_updated_at"] = metadata["downloads_updated_at"]
    return body
