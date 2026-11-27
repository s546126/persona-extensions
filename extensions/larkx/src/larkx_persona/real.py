"""The backend on LarkAgentX. LarkAgentX has no license, so the package does not carry its code: on first start it is
downloaded from GitHub at the commit in upstream.json and checked file by file, and its dependencies are installed from
the hash-locked requirements.txt. Nothing here imports it at the top: the handshake must not wait for either."""
import asyncio
import hashlib
import io
import json
import os
import re
import shutil
import string
import subprocess
import sys
import tarfile
import time
import urllib.parse
import urllib.request
import zlib
from pathlib import Path

UPSTREAM = os.path.join(os.path.dirname(os.path.abspath(__file__)), "upstream.json")
FETCH_SECONDS = 120
ARCHIVE_MOST = 64 << 20
PIP_SECONDS = 300
PYPI_INDEX = "https://pypi.org/simple"
# A connection that lasted this long was fine: the next failure starts the backoff over.
STEADY_SECONDS = 60
BACKOFF_MOST = 60


class Expired(Exception):
    """The session is over: the user has to scan again. Not being able to reach Feishu is any other exception."""


def _fields(raw):
    """The length-delimited fields of a protobuf message, the last of each by number, as bytes. Enough to read the
    rich text's few string properties without generated classes; anything malformed ends the read, and a property
    that is not bytes has no fields."""
    if not isinstance(raw, (bytes, bytearray, memoryview)):
        return {}
    raw = bytes(raw)
    out, i, n = {}, 0, len(raw)

    def varint():
        nonlocal i
        shift = value = 0
        while i < n and shift < 70:  # protobuf's longest varint is 10 bytes
            b = raw[i]
            i += 1
            value |= (b & 0x7F) << shift
            if not b & 0x80:
                return value
            shift += 7
        raise ValueError("truncated or overlong varint")

    try:
        while i < n:
            key = varint()
            number, wire = key >> 3, key & 7
            if wire == 0:
                varint()
                continue
            size = {1: 8, 5: 4}.get(wire)
            if wire == 2:
                size = varint()
            if size is None or i + size > n:
                break  # an unknown wire type, or a length past the end: what follows is not read as a field
            if wire == 2:
                out[number] = raw[i:i + size]  # the last one wins, as protobuf reads a repeated scalar
            i += size
    except ValueError:
        pass
    return out


def _string(raw, number):
    value = _fields(raw).get(number) or b""
    return value if isinstance(value, str) else value.decode("utf-8", "replace")


def _ordered(rich, dictionary):
    """Rich-text elements in display order: a parent's children come before the next top-level element."""
    ids = rich.get("elementIds")
    ids = [k for k in ids if isinstance(k, str)] if isinstance(ids, list) else []
    out, seen = [], set()

    def add(key):
        if key in seen or key not in dictionary:
            return
        seen.add(key)
        element = dictionary[key]
        out.append(element)
        children = element.get("childIds") if isinstance(element, dict) else []
        if isinstance(children, list):
            for child in children:
                if isinstance(child, str):
                    add(child)

    for key in ids:
        add(key)
    rest = [k for k in dictionary if k not in seen]
    if not ids:
        try:
            rest.sort(key=int)
        except (TypeError, ValueError):
            pass
    for key in rest:
        add(key)
    return out


# LarkAgentX's rich text: its tags (RichTextElement.Tag) and what it writes for the ones with no text.
_TEXT_TAGS = (1, 3, 7, 8, 9, 17)  # TEXT, P, B, I, U, LINK: TextProperty.content (LINK: LinkProperty.url) is field 1
_AT, _ANCHOR, _PARAGRAPH = 5, 6, 3
_PLACEHOLDERS = {2: "[图片]", 5: "[@]", 10: "[表情]", 18: "[视频]", 22: "[文档]"}


def rich_text(rich):
    """A TEXT or POST message's rich text as LarkAgentX reads it (`extract_rich_text`), but with its links. LarkAgentX
    drops an A element whole, its title and its address: a pasted Feishu doc link reached Persona as the words around
    it (2026-11-12). A link is its title, a space and its address (the address alone when the title is it or empty).
    None when the message has no link, so LarkAgentX's own text stays (and so does an empty result)."""
    linked = _linked(rich)
    return linked[0] if linked else None


def _linked(rich, always=False):
    """rich_text's text and the AT elements' spans in that same text ({id, start, end, label}, as _mentions gives them
    for LarkAgentX's text), or None when the message has no link. POST always rebuilds to retain childIds order."""
    # A decoder of another shape (a dictionary that is not one, elements or ids of another type) leaves LarkAgentX's
    # text as it is: None, never an exception that would lose the whole message.
    try:
        dictionary = rich["elements"]["dictionary"]
    except (KeyError, TypeError, IndexError):
        return None
    if not isinstance(dictionary, dict):
        return None
    if not always and not any(isinstance(v, dict) and v.get("tag") == _ANCHOR for v in dictionary.values()):
        return None
    ordered = _ordered(rich, dictionary)
    text, after_link, spans, linked = "", False, [], False
    for v in ordered:
        if not isinstance(v, dict):
            continue
        tag, prop = v.get("tag", 1), v.get("property")
        if not isinstance(tag, int) or isinstance(tag, bool):
            tag = 0
        if not prop:
            continue
        piece, user_id = None, ""
        if tag in _TEXT_TAGS:
            piece = _string(prop, 1) + ("\n" if tag == _PARAGRAPH else "")
        elif tag == _AT:
            user_id, piece = _string(prop, 1), _string(prop, 2) or None  # AtProperty.userId, .content
        elif tag == _ANCHOR:
            # AnchorProperty is not in LarkAgentX's proto.proto (only Tag.A = 6 and RichText.anchorIds = 7); its fields
            # are LarkAgentX's lark_all_pb2 entities.RichTextElement.AnchorProperty at the pinned commit: href 1,
            # content 2, textContent 3.
            # tests/test_real.py checks them there; check again when the pin moves.
            href, title = _string(prop, 1), _string(prop, 2) or _string(prop, 3)
            title = title.strip()
            piece = href if not title or title == href else ("%s %s" % (title, href) if href else title)
            linked = linked or bool(href)
            if piece and text and not text[-1].isspace():
                piece = " " + piece
        if piece is None:
            piece = _PLACEHOLDERS.get(tag, "")
        spaced = after_link and bool(piece) and not piece[0].isspace()
        if spaced:
            piece = " " + piece  # an address glued to the next words is a different address
        if tag == _AT and user_id:
            start = len(text) + (1 if spaced else 0)
            spans.append({"id": user_id, "start": start, "end": len(text) + len(piece), "label": piece[1:] if spaced else piece})
        text += piece
        if piece:  # an element that adds nothing keeps the last link apart from what comes next
            after_link = tag == _ANCHOR
    if not linked and not always:
        return None  # an anchor with no readable address adds no link: LarkAgentX's text stays
    return text, spans


_MARKDOWN_ESCAPES = frozenset(string.punctuation)
_LIST, _ORDERED_LIST, _LIST_ITEM = 26, 27, 28


def _escape_markdown(value):
    """Literal text, not a chance for message content to inject Markdown or raw HTML."""
    return "".join("\\" + char if char in _MARKDOWN_ESCAPES else char for char in value)


def _safe_markdown_link(href, title=""):
    """An HTTP(S) Markdown link, or inert text when its address is malformed or has another scheme."""
    href, title = href.strip(), title.strip()
    try:
        parsed = urllib.parse.urlsplit(href)
        safe = parsed.scheme.lower() in ("http", "https") and bool(parsed.netloc)
    except ValueError:
        safe = False
    safe = safe and not any(char.isspace() or ord(char) < 32 or ord(char) == 127 for char in href)
    if not safe:
        return _escape_markdown(title or href)
    # These characters can close or reshape an inline destination. Percent-encoding them leaves ordinary URLs intact.
    destination = href
    for char, encoded in (("\\", "%5C"), ("(", "%28"), (")", "%29"), ("<", "%3C"), (">", "%3E")):
        destination = destination.replace(char, encoded)
    if not title or title == href:
        return "<%s>" % destination
    return "[%s](%s)" % (_escape_markdown(title), destination)


def _inline_code(value):
    if not value:
        return ""
    # A blank line can end the paragraph containing a code span. CommonMark renders line endings in a span as spaces.
    value = re.sub(r"\r\n?|\n", " ", value)
    longest = max([len(run) for run in re.findall(r"`+", value)] or [0])
    fence = "`" * (longest + 1)
    padding = " " if value[:1] in ("`", " ") or value[-1:] in ("`", " ") else ""
    return fence + padding + value + padding + fence


def _code_block(value):
    longest = max([len(run) for run in re.findall(r"`+", value)] or [0])
    fence = "`" * max(3, longest + 1)
    return fence + "\n" + value + ("" if value.endswith("\n") else "\n") + fence


def _markdown(rich):
    """A safe Markdown display form of LarkAgentX's decoded richText, or None when it cannot be rebuilt.

    The plain normalized text remains authoritative for mentions, /agent and archives. This tree walk retains child
    order and list nesting; unknown containers still render their children instead of discarding readable text.
    """
    try:
        dictionary = rich["elements"]["dictionary"]
    except (KeyError, TypeError, IndexError):
        return None
    if not isinstance(dictionary, dict):
        return None
    ids = rich.get("elementIds")
    ids = [key for key in ids if isinstance(key, str)] if isinstance(ids, list) else []
    seen = set()

    def render(key, depth=0, item=""):
        if key in seen or key not in dictionary:
            return ""
        seen.add(key)
        element = dictionary[key]
        if not isinstance(element, dict):
            return ""
        tag, prop = element.get("tag", 1), element.get("property")
        if not isinstance(tag, int) or isinstance(tag, bool):
            tag = 0
        children = element.get("childIds")
        children = [child for child in children if isinstance(child, str)] if isinstance(children, list) else []

        def render_children(at_depth=None):
            return "".join(render(child, depth if at_depth is None else at_depth) for child in children)

        if tag in (_LIST, _ORDERED_LIST):
            out = _escape_markdown(_string(prop, 1)) + "\n" if prop else ""
            number = 0
            for child in children:
                target = dictionary.get(child)
                if isinstance(target, dict) and target.get("tag") == _LIST_ITEM and child not in seen:
                    number += 1
                    marker = "- " if tag == _LIST else "%d. " % number
                    out += render(child, depth, marker)
                else:
                    out += render(child, depth)
            return out + ("\n" if out and depth == 0 else "")
        if tag == _LIST_ITEM:
            own = _escape_markdown(_string(prop, 1)) if prop else ""
            indent = "  " * depth
            out = indent + (item or "- ") + own
            for child in children:
                target = dictionary.get(child)
                nested = isinstance(target, dict) and target.get("tag") in (_LIST, _ORDERED_LIST)
                piece = render(child, depth + 1 if nested else depth)
                if not piece:
                    continue
                if nested:
                    if not out.endswith("\n"):
                        out += "\n"
                    out += piece
                elif out.endswith("\n"):
                    out += indent + "  " + piece
                else:
                    out += piece
            return out if out.endswith("\n") else out + "\n"
        if tag == 30:  # CODE
            return _inline_code(_string(prop, 1) if prop else "") + render_children()
        if tag == 31:  # CODE_BLOCK
            # A fence is only a fence at the start of a line, including when the preceding element was plain text.
            return "\n" + _code_block(_string(prop, 1) if prop else "") + "\n\n" + render_children()
        if tag == 32:  # HR
            return "---\n\n" + render_children()

        if tag == _ANCHOR:
            own = _safe_markdown_link(_string(prop, 1), _string(prop, 2) or _string(prop, 3)) if prop else ""
        elif tag == 17:  # LINK / LinkProperty.url
            own = _safe_markdown_link(_string(prop, 1)) if prop else ""
        elif tag == _AT:
            own = _escape_markdown(_string(prop, 2)) if prop else ""
        elif tag in _TEXT_TAGS or tag in (23, 24, 25, _LIST_ITEM, 29):
            own = _escape_markdown(_string(prop, 1)) if prop else ""
        else:
            own = _escape_markdown(_PLACEHOLDERS[tag]) if prop and tag in _PLACEHOLDERS else ""
        body = own + render_children()
        if tag == _PARAGRAPH:
            return body.rstrip("\n") + "\n\n" if body or prop else ""
        if tag in (23, 24, 25):  # H1, H2, H3
            return "%s %s\n\n" % ("#" * (tag - 22), body.strip("\n")) if body.strip("\n") else ""
        if tag == 29:  # QUOTE
            body = body.rstrip("\n")
            return "\n".join("> " + line for line in body.split("\n")) + "\n\n" if body else ""
        if tag == 7 and body.strip():  # B
            return "**%s**" % body
        if tag == 8 and body.strip():  # I
            return "*%s*" % body
        # U has no portable Markdown form. Keep its escaped text rather than emitting raw HTML.
        return body

    rest = [key for key in dictionary if key not in ids]
    if not ids:
        try:
            rest.sort(key=int)
        except (TypeError, ValueError):
            pass
    out = "".join(render(key) for key in ids + rest).rstrip()
    return out or None


def _inline_images(rich):
    """The inline images of a POST, in display order. ImageProperty.token/originKey/middleKey/thumbKey are 2/4/5/6."""
    try:
        dictionary = rich["elements"]["dictionary"]
    except (KeyError, TypeError, IndexError):
        return []
    if not isinstance(dictionary, dict):
        return []
    out = []
    for element in _ordered(rich, dictionary):
        if not isinstance(element, dict) or element.get("tag") != 2:
            continue
        prop = element.get("property")
        key = next((value for value in (_string(prop, n) for n in (2, 4, 5, 6)) if value), "")
        if key:
            out.append({"kind": "image", "key": key})
    return out


def _clean_id(value):
    s = str(value or "").strip()
    return "" if s == "0" else s


def normalize(msg, extract_key):
    """A message of LarkAgentX's, live or from history, as the connector keeps it. Pure, for the tests."""
    msg_type = msg.get("msg_type") or 0
    name = msg.get("msg_type_name") or "UNKNOWN"
    data = msg.get("content_data") or {}
    key = extract_key(msg_type, data) if isinstance(data, dict) else ""
    text = msg.get("content") or ""
    mentions = _mentions(data, text)
    post = name == "POST"
    rich = data.get("richText") if name in ("TEXT", "POST") and isinstance(data, dict) else None
    linked = _linked(rich, always=post) if name in ("TEXT", "POST") else None
    markdown = _markdown(rich) if name in ("TEXT", "POST") else None
    if linked and linked[0]:  # an empty rebuild keeps LarkAgentX's text, as its own `extract_rich_text(...) or text` does
        head = "[富文本] %s\n" % data.get("title", "") if name == "POST" else ""
        text = (head + linked[0]).rstrip() if head else linked[0]
        # The spans follow the text the connector keeps: shifted past a POST's title line, and none at all when one
        # does not land on its label (the message is then no self-mention, never a wrong one).
        mentions = [dict(m, start=m["start"] + len(head), end=m["end"] + len(head)) for m in linked[1]]
        if any(text[m["start"]:m["end"]] != m["label"] for m in mentions):
            mentions = []
    if markdown and post:
        head = "[富文本] %s" % data.get("title", "")
        markdown = (_escape_markdown(head) + "\n" + markdown).rstrip()
    media = _inline_images(rich) if post else []
    if name in ("IMAGE", "STICKER") and key:
        media = [{"kind": name.lower(), "key": key}]
    elif name == "FILE":
        media = [{"kind": "file", "name": data.get("name") or ""}]
    elif name == "MEDIA":
        media = [{"kind": "video", "name": data.get("name") or ""}]
    elif name == "AUDIO":
        media = [{"kind": "voice"}]
    at = float(msg.get("create_time") or 0)
    if at > 1e12:
        at /= 1000
    msg_id = _clean_id(msg.get("msg_id"))
    root = _clean_id(msg.get("root_id"))
    parent = _clean_id(msg.get("parent_id"))
    chat_type = msg.get("chat_type")
    scope = str(msg.get("scope") or "").strip()
    anchor = _clean_id(msg.get("anchor"))
    # Pinned LarkAgentX (97ad48e) folds threadId into anchor and does not emit thread_id; keep thread_id as a
    # defensive fallback in case a future upstream pin or caller supplies it directly.
    thread_id = _clean_id(msg.get("thread_id"))
    topic_id = ""
    if chat_type != 1:
        if scope == "topic":
            topic_id = anchor or thread_id or root or (msg_id if name != "SYSTEM" else "")
        elif chat_type == 3 and name != "SYSTEM":
            topic_id = anchor or thread_id or root or msg_id
        else:
            topic_id = thread_id or root
    if not root and chat_type != 1 and scope == "topic":
        # LarkAgentX identifies roots in topic groups before any replies arrive.
        root = msg_id
    return {
        "id": msg_id, "chat": str(msg.get("chat_id") or ""),
        "chat_kind": "dm" if chat_type == 1 else "group",
        "position": int(msg.get("position") or 0), "sender": str(msg.get("from_id") or ""), "type": name,
        "text": text, "at": at or time.time(), "root": root,
        "parent": parent, "topic_id": topic_id, "at_me": bool(msg.get("at_me")), "media": media,
        "mentions": mentions, "markdown": markdown or "",
    }


def own_chat_ok(info, me):
    """Whether a chat's info (chats.PullChatsByIdsRequest) proves a direct chat with nobody but the user me in it. Pure,
    for the tests. It fails closed: only positive evidence counts, a direct chat whose memberCount is exactly 1 and
    whose ownerId is me (as the user's chat with themself reports both); a missing or unreadable count or owner is no
    evidence, and so is a userCount over 1."""
    if not isinstance(info, dict) or info.get("type") not in (1, "P2P") or not me:
        return False

    def count(name):
        value = info.get(name)
        if isinstance(value, bool):
            return None
        try:
            return int(value)
        except (TypeError, ValueError):
            return None
    if count("memberCount") != 1:
        return False
    if "userCount" in info and count("userCount") not in (0, 1):
        return False
    return str(info.get("ownerId") or "") == str(me)


def _mentions(data, text):
    """The AT elements' account ids and exact spans in the flattened text. Only this metadata identifies a real mention:
    an @-looking word, or at_me on somebody else's message, does not. Decoding the two known string fields here keeps
    LarkAgentX out of the connector's fast import path."""
    if not isinstance(data, dict):
        return []
    rich = data.get("richText") or {}
    holder = rich.get("elements") if isinstance(rich, dict) else None
    elements = (holder.get("dictionary") if isinstance(holder, dict) else None) or {}
    if not isinstance(elements, dict):
        return []
    ids = rich.get("elementIds") if isinstance(rich, dict) else None
    ids = [k for k in ids if isinstance(k, str)] if isinstance(ids, list) else []
    listed = set(ids)
    keys = [k for k in ids if k in elements]
    keys += [k for k in elements if k not in listed]
    if not ids:
        try:
            keys = sorted(elements, key=lambda k: int(k))
        except (TypeError, ValueError):
            keys = list(elements)
    mentions, rebuilt = [], ""
    for key in keys:
        element = elements[key]
        if not isinstance(element, dict):
            return []
        tag, prop = element.get("tag", 1), element.get("property")
        if not isinstance(tag, int) or isinstance(tag, bool):
            tag = 0
        if not prop:
            continue
        if tag in (1, 3, 7, 8, 9, 17):
            content = _text_property(prop)
            if content is not None:
                rebuilt += content + ("\n" if tag == 3 else "")
                continue
        if tag == 5:
            user_id, content = _at_property(prop)
            if user_id:
                start = len(rebuilt)
                rebuilt += content or "[@]"
                mentions.append({"id": user_id, "start": start, "end": len(rebuilt), "label": rebuilt[start:]})
                continue
        # The placeholders are the same as LarkAgentX's extract_rich_text. Rebuilding every element, not searching
        # for an @name in the body, distinguishes a real AT from identical ordinary text before it.
        rebuilt += {2: "[图片]", 5: "[@]", 10: "[表情]", 18: "[视频]", 22: "[文档]"}.get(tag, "")
    # If another decoder version flattened the text differently, keep the message inert rather than guess a span.
    return mentions if rebuilt == text else []


def _at_property(data):
    """AtProperty { string userId = 1; string content = 2; }, without importing generated protobuf code."""
    return _string_property(data, 1), _string_property(data, 2)


def _text_property(data):
    """TextProperty.content, or None when the property is not the expected wire-format string."""
    return _string_property(data, 1)


def _string_property(data, wanted):
    if not isinstance(data, (bytes, bytearray, memoryview)):
        return None
    data, i = bytes(data), 0
    while i < len(data):
        key, i = _wire_varint(data, i)
        if key is None:
            return None
        size, i = _wire_varint(data, i)
        if size is None or size < 0 or i + size > len(data):
            return None
        if key >> 3 != wanted:
            i += size
            continue
        try:
            return data[i:i + size].decode("utf-8")
        except UnicodeDecodeError:
            return None
    return None


def _wire_varint(data, i):
    value = 0
    for shift in range(0, 70, 7):
        if i >= len(data):
            return None, i
        b = data[i]
        i += 1
        value |= (b & 127) << shift
        if b < 128:
            return value, i
    return None, i


def fetch_upstream(lock, data_dir, urlopen=urllib.request.urlopen):
    """LarkAgentX at lock's commit, in data_dir: the directory to put on sys.path. Downloaded once from lock's archive;
    only the files lock names are kept, each with its SHA-256, and any one missing or different fails the whole."""
    target = os.path.join(data_dir, "larkagentx-" + lock["commit"][:12])
    if os.path.isdir(target):
        return target
    try:
        with urlopen(lock["archive"], timeout=FETCH_SECONDS) as r:
            data = r.read(ARCHIVE_MOST + 1)
    except OSError as e:
        raise RuntimeError("下载不了 LarkAgentX：%s" % type(e).__name__) from e
    if len(data) > ARCHIVE_MOST:
        raise RuntimeError("下载的 LarkAgentX 太大了")
    want, got = lock["files"], {}
    try:
        with tarfile.open(fileobj=io.BytesIO(data), mode="r:gz") as archive:
            for member in archive:
                # The archive's top directory is <repository>-<commit>: paths are taken below it.
                rel = member.name.split("/", 1)[-1] if "/" in member.name else ""
                if not member.isfile() or rel not in want:
                    continue
                body = archive.extractfile(member).read()
                if hashlib.sha256(body).hexdigest() != want[rel]:
                    raise RuntimeError("下载的 LarkAgentX 和锁定的不一样：" + rel)
                got[rel] = body
    except (tarfile.TarError, EOFError, OSError, zlib.error) as e:
        raise RuntimeError("下载的 LarkAgentX 不是完整的压缩包") from e
    missing = sorted(set(want) - set(got))
    if missing:
        raise RuntimeError("下载的 LarkAgentX 少了文件：" + missing[0])
    building = target + ".new"
    shutil.rmtree(building, ignore_errors=True)
    # Written under the names lock gives, never a name from the archive.
    for rel in sorted(want):
        path = os.path.join(building, *rel.split("/"))
        os.makedirs(os.path.dirname(path), exist_ok=True)
        with open(path, "wb") as f:
            f.write(got[rel])
    os.rename(building, target)
    drop_others(data_dir, "larkagentx-", target)
    return target


def drop_others(data_dir, prefix, keep):
    """Removes what earlier versions left under prefix in data_dir, now that keep is in place."""
    for name in os.listdir(data_dir):
        path = os.path.join(data_dir, name)
        if name.startswith(prefix) and path != keep and os.path.isdir(path):
            shutil.rmtree(path, ignore_errors=True)


class RealBackend:
    Expired = Expired

    def __init__(self, root, data_dir, region):
        self._root = root
        self._data_dir = data_dir
        self._region = region
        self._client = None
        self._auth = None
        self._qr = None
        self._save = None
        self._extract_key = None
        self._lark = None

    def prepare(self):
        """Makes LarkAgentX importable: its code and its dependencies, both in the data directory."""
        if sys.version_info < (3, 9):
            raise RuntimeError("LarkAgentX 要 Python 3.9 以上")
        with open(UPSTREAM, encoding="utf-8") as f:
            upstream = fetch_upstream(json.load(f), self._data_dir)
        deps = self._deps()
        sys.path[:0] = [upstream, deps]
        # Read when LarkAgentX is imported: its own files stay in the plugin's data directory, on the chosen site.
        os.environ["LARKX_HOME"] = os.path.join(self._data_dir, "larkx")
        os.environ["LARKX_REGION"] = self._region
        from loguru import logger
        # Its log carries bodies and addresses: keep where a problem happened and leave the message out.
        logger.remove()
        logger.add(sys.stderr, level="ERROR", format="larkx: {level} {name}:{function}:{line}")
        import larkx
        from larkx import media
        self._lark = larkx
        self._extract_key = media.extract_resource_key

    def _deps(self):
        requirements = os.path.join(self._root, "requirements.txt")
        with open(requirements, "rb") as f:
            digest = hashlib.sha256(f.read()).hexdigest()[:12]
        deps = os.path.join(self._data_dir, "deps-py%d%d-%s" % (sys.version_info[0], sys.version_info[1], digest))
        if os.path.isdir(deps):
            return deps
        building = deps + ".new"
        shutil.rmtree(building, ignore_errors=True)
        # PyPI itself, whatever index the machine's pip config names: the manifest's network list has only PyPI's hosts,
        # and in the hub's sandbox (or behind its egress proxy) a mirror is refused. Every file is checked against the
        # hashes in requirements.txt, the dependencies' dependencies too.
        done = subprocess.run([sys.executable, "-m", "pip", "install", "--quiet", "--disable-pip-version-check", "--no-input",
                               "--index-url", PYPI_INDEX, "--require-hashes", "--target", building, "-r", requirements],
                              stdout=subprocess.DEVNULL, stderr=subprocess.PIPE, timeout=PIP_SECONDS)
        if done.returncode != 0:
            tail = done.stderr.decode(errors="replace").strip().splitlines()[-1:]
            raise RuntimeError("pip 没装上依赖：" + (tail[0][:160] if tail else "没有输出"))
        os.rename(building, deps)
        drop_others(self._data_dir, "deps-", deps)
        return deps

    def _hub_auth(self, saved):
        backend = self

        class HubAuth(self._lark.LarkAuth):
            """LarkAuth with its credentials in the hub's encrypted store instead of a file."""

            def load(self):
                from larkx.region import get_region
                data = saved or {}
                if data.get("region"):
                    self.region = get_region(data["region"])
                self.cookies = data.get("cookies") or {}
                self.saved_at = data.get("saved_at", 0)
                self.device_id = data.get("device_id", "")
                self.app_key = data.get("app_key", "")
                self.user_id = data.get("user_id", "")
                self.csrf_token = data.get("csrf_token", "")
                self.web_app_id = data.get("web_app_id", "")
                return bool(self.cookies)

            def save(self):
                self.saved_at = time.time()
                backend._persist()

        return HubAuth(Path(os.devnull))

    def _persist(self):
        if self._save is not None:
            self._save(self._credentials())

    def _credentials(self):
        a = self._auth
        return {"region": a.region.name, "cookies": a.cookies, "saved_at": a.saved_at, "device_id": a.device_id, "app_key": a.app_key,
                "user_id": a.user_id, "csrf_token": a.csrf_token, "web_app_id": a.web_app_id}

    def on_credentials(self, save):
        """save is called with the credentials whenever LarkAgentX refreshes them."""
        self._save = save

    def _open(self):
        """The client for self._auth, and who it is."""
        self._client = self._lark.LarkClient(self._auth)
        user = str(self._auth.user_id)
        return {"id": user, "name": "%s（用户 …%s）" % (self._auth.region.label, user[-4:])}

    def restore(self, saved):
        self._auth = self._hub_auth(saved)
        try:
            return self._open()
        except self._lark.AuthExpired as e:
            # LarkAgentX says expired for a probe that never reached the server too.
            if "网络异常" in str(e):
                raise ConnectionError("飞书连不上") from e
            raise Expired() from e

    def login_start(self):
        from larkx.region import get_region
        self._qr = self._lark.QrLogin(get_region(self._region))
        return self._qr.qr_content

    def login_qr(self):
        return self._qr.qr_content

    def login_poll(self):
        """One look at the scan: waiting, done or expired (scanned away, cancelled, timed out)."""
        if self._qr is None:
            return "expired"
        finished, data = self._qr.wait(timeout=0.01, interval=0)
        if finished:
            return "done"
        return "expired" if data is not None else "waiting"

    def login_finish(self):
        self._auth = self._hub_auth(None)
        try:
            self._qr.finish(self._auth)
        except self._lark.AuthExpired as e:
            raise Expired() from e
        self._qr = None
        account = self._open()
        return account, self._credentials()

    def logout(self):
        self._client = None
        self._auth = None
        self._qr = None
        self._save = None

    def listen(self, on_message, on_link, stop):
        """Holds the long connection until stop, reconnecting with a growing wait."""
        wait = 1

        async def received(msg):
            await asyncio.to_thread(on_message, normalize(msg, self._extract_key))

        while not stop.is_set():
            client, began = self._client, time.time()
            if client is None:
                return
            try:
                on_link("up")
                self._run(client, received, stop)
            except self._lark.AuthExpired:
                on_link("expired")
                return
            except Exception:
                pass
            on_link("down")
            if time.time() - began > STEADY_SECONDS:
                wait = 1
            stop.wait(wait)
            wait = min(wait * 2, BACKOFF_MOST)

    @staticmethod
    def _run(client, received, stop):
        async def until_stopped():
            task = asyncio.ensure_future(client.connect_websocket(received))
            while not task.done():
                if stop.is_set():
                    task.cancel()
                    return
                await asyncio.sleep(1)
            task.result()

        asyncio.run(until_stopped())

    def send(self, chat, text, root, mentions=()):
        # The locked LarkAgentX sends every word in one TEXT element. Keep that path unless the caller supplied
        # explicit mention ids; a plain @name is never enough to mention somebody.
        if not mentions:
            return self._client.send_msg(text, chat, root_id=root)
        parts = mention_parts(text, mentions)
        from larkx.proto import builders, ids
        from larkx.proto import lark_all_pb2 as L
        from larkx.proto import proto_pb2 as P
        from larkx.proto.ids import generate_long_request_id
        req = L.messages.PutMessageRequest()
        req.type = 4
        req.chatId = str(chat)
        req.cid = ids.generate_request_cid()
        req.isNotified = True
        req.version = 1
        if root:
            req.rootId = req.parentId = str(root)
            if not self._client.is_thread_chat(chat):
                req.isReplyInThread = True
        notified = set()
        for kind, value in parts:
            element_id = ids.generate_request_cid()
            element = req.content.richText.elements.dictionary[element_id]
            if kind == "at":
                element.tag = 5
                element.property = P.AtProperty(userId=value["id"], content="@" + value["name"]).SerializeToString()
                req.content.richText.atIds.append(element_id)
                # atUserIdList (14) exists in the full generated request, not in the older compact proto.proto.
                user_id = int(value["id"])
                if user_id not in notified:
                    req.atUserIdList.append(user_id)
                    notified.add(user_id)
            else:
                element.tag = 1
                element.property = P.TextProperty(content=value).SerializeToString()
            req.content.richText.elementIds.append(element_id)
        req.content.richText.innerText = text
        content = self._client._gateway_post(builders.wrap_packet(5, req, generate_long_request_id()))
        return builders.decode_put_message_response(content)


    def create_chat(self, user):
        chat = self._client.create_chat(user)
        if not chat:
            raise RuntimeError("建不了和这个人的私聊")
        return str(chat)

    def own_chat(self):
        """The user's chat with themself, where pushes go: the direct chat opened with their own id. Anything else is
        refused, a group or a chat with somebody in it: a push may carry what only the user should read."""
        me = str(self._auth.user_id)
        chat = self.create_chat(me)
        return chat if own_chat_ok(self._client.get_chat_info(chat), me) else None

    def search(self, query):
        return self._client.search(query)

    def history(self, chat, positions):
        return [normalize(m, self._extract_key) for m in self._client.pull_history(chat, positions)]

    def chat_info(self, chat):
        info = self._client.get_chat_info(chat)
        if not isinstance(info, dict):
            info = {}
        return {"name": info.get("name") or "", "last_position": int(info.get("lastMessagePosition") or 0),
                "mute": mute_state(info)}

    def user_name(self, user, chat):
        return self._client.get_user_name(user, chat)

    def chat_members(self, chat, limit=100):
        """List members of a group chat, including bots, via chats.PullChatChattersRequest."""
        out, seen = [], set()
        cursor = 0
        max_pages = 5
        for _ in range(max_pages):
            if len(out) >= limit:
                break
            params = {"chatId": str(chat), "needChatters": True, "limit": min(100, max(20, limit - len(out)))}
            if cursor:
                params["cursor"] = cursor
            resp = self._client.api("chats.PullChatChattersRequest", params) or {}
            chatters = resp.get("chatters") or {}
            nicknames = resp.get("groupNicknames") or {}
            if not isinstance(chatters, dict) or not chatters:
                break
            added = 0
            for cid_key, ch in chatters.items():
                if not isinstance(ch, dict):
                    continue
                uid = str(ch.get("id") or cid_key or "")
                if not uid.isdigit() or int(uid) <= 0 or uid in seen:
                    continue
                seen.add(uid)
                added += 1
                raw_type = ch.get("type")
                is_bot = raw_type in (2, 3, "BOT", "AI") or bool(ch.get("botConfig"))
                real_name = (ch.get("name") or ch.get("enUsName") or ch.get("alias") or "").strip()
                nick = (nicknames.get(uid) or "").strip() if isinstance(nicknames, dict) else ""
                display = nick or real_name
                if not display:
                    continue
                out.append({
                    "id": uid,
                    "name": display,
                    "real_name": real_name,
                    "kind": "bot" if is_bot else "user",
                })
                if len(out) >= limit:
                    break
            next_cursor = resp.get("nextCursor") or 0
            try:
                next_cursor = int(next_cursor)
            except (TypeError, ValueError):
                next_cursor = 0
            if not next_cursor or next_cursor == cursor or added == 0:
                break
            cursor = next_cursor
        return out

    def download(self, msg_id, chat, key, most):
        from larkx import media
        url = media.message_resource_url(msg_id, key, chat, host=self._auth.region.file_host)
        data, mime = media.download(self._auth, url, max_bytes=most)
        return mime or "application/octet-stream", data


def _remind_state(value):
    if type(value) is not bool:
        return None
    return "normal" if value else "muted"


def mute_state(info):
    """The verified whole-chat reminder state; isMuteAtAll is a different setting and is never used here."""
    if not isinstance(info, dict):
        return "unknown"
    states = []
    settings = info.get("userSetting")
    if isinstance(settings, dict):
        state = _remind_state(settings.get("isRemind"))
        if state is not None:
            states.append(state)
    state = _remind_state(info.get("isRemind"))
    if state is not None:
        states.append(state)
    return states[0] if states and len(set(states)) == 1 else "unknown"


def mention_parts(text, mentions):
    """Split only exact @name occurrences of explicit, numeric user ids; longest names win at the same position."""
    if not isinstance(mentions, (list, tuple)) or len(mentions) > 100:
        raise ValueError("mentions must be a list of at most 100 people")
    targets = []
    seen = set()
    for mention in mentions:
        if not isinstance(mention, dict):
            raise ValueError("a mention needs an id and a name")
        user_id, name = mention.get("id"), mention.get("name")
        if isinstance(user_id, bool) or not str(user_id or "").isdigit() or int(user_id) <= 0:
            raise ValueError("a mention id must be the numeric id returned by larkx_search")
        if not isinstance(name, str) or not name.strip() or "@" in name or "\n" in name or name != name.strip():
            raise ValueError("a mention name must be one nonempty line without @")
        user_id = str(user_id)
        if user_id not in seen:
            targets.append((user_id, name))
            seen.add(user_id)
    if len({name for _, name in targets}) != len(targets):
        raise ValueError("two mentioned people cannot have the same name")
    targets.sort(key=lambda target: len(target[1]), reverse=True)
    import re
    pattern = re.compile("(" + "|".join("@" + re.escape(name) for _, name in targets) + r")(?![\w-])") if targets else None
    by_name = {name: {"id": user_id, "name": name} for user_id, name in targets}
    out = []
    for part in pattern.split(text) if pattern else [text]:
        if not part:
            continue
        out.append(("at", by_name[part[1:]]) if part.startswith("@") and part[1:] in by_name else ("text", part))
    if not any(kind == "at" for kind, _ in out):
        raise ValueError("write @name in text for every person to mention")
    return out

