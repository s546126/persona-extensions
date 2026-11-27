"""The connector: every connector.* method of the protocol (docs/connector-protocol.md), over a backend. It knows nothing of
LarkAgentX, so the whole of it runs against a fixture backend in the tests."""
import base64
import hashlib
import json
import queue
import threading
import time
from datetime import datetime, timezone

from .hooks import delivery_id
from .real import mention_parts
from .wire import RpcError

ID = "larkx"
SECRETS = "connector.secrets."

# Inbox activity window, maximum chats polled, and per-thread context size.
INBOX_DAYS = 3
INBOX_MOST = 200
CONTEXT_LINES = 8
# Refresh only stale group mute states, with a bounded number of upstream calls in one inbox poll.
MUTE_REFRESH_SECONDS = 3600
MUTE_REFRESH_MOST = 10
# An archive call reads this many messages at most, whatever the number of chats it asks for.
ARCHIVE_ROWS = 500
THREAD_MOST = 50
THREAD_MEDIA_MOST = 4 << 20
ARCHIVE_MEDIA_MOST = 20 << 20
# Messages the archive has passed are kept this long, then deleted: the knowledge base has them, and a thread page reads
# older ones from the server again. Looked at no more often than every PRUNE_SECONDS.
KEEP_DAYS = 30
PRUNE_SECONDS = 3600
# Catching up after a reconnect: the chats that moved last, and how far back in each.
CATCH_UP_CHATS = 30
CATCH_UP_MESSAGES = 50
# The hub is told at most this often that something moved.
CHANGED_SECONDS = 3
# Trying a saved sign-in again when Feishu cannot be reached; how long sign-in waits for the connection.
RESTORE_RETRY_SECONDS = 30
LINK_WAIT_SECONDS = 5
CHANNEL = "channel."
CANNOT_SEND = "cannot_send"
PUSH = "push"
# /agent: the user's own message starting with it goes to the assistant; the answer comes back in that chat.
AGENT_PREFIX = "/agent"
# The setting agent_prefix takes its place; the manifest's maxLength is the same. A prefix must start with "/" (not
# "//"), stay within [AGENT_PREFIX_LEAST, AGENT_PREFIX_MOST] characters, and contain no whitespace, control characters
# or colons so ordinary messages, URLs or delimiters cannot trigger an agent reply. Anything invalid keeps AGENT_PREFIX.
AGENT_PREFIX_LEAST = 2
AGENT_PREFIX_MOST = 32
AGENT_SUFFIX = "🤖 AI 代答，仅供参考"
# The setting agent_reply_suffix takes its place; the manifest's maxLength is the same. A shorter one than
# AGENT_SUFFIX_LEAST keeps the default: the user's own message ending with the line is not a request, and a line
# like "AI" would end too many of them.
AGENT_SUFFIX_MOST = 100
AGENT_SUFFIX_LEAST = 4
AGENT_EMPTY = "（只发了 %s：看上面的上下文和引用的消息，判断要做什么。）"
AGENT_TYPES = ("TEXT", "POST")
CHANNEL_IMAGE_MOST = 3 << 20
CHANNEL_IMAGES_MOST = 4
AGENT_FRESH_SECONDS = 600
AGENT_CONTEXT_MESSAGES = 20
MENTION_CONTEXT_MESSAGES = 50
MEMBERS_CACHE_SECONDS = 300
AGENT_RETRY_SECONDS = [2, 5, 15]
# The send key a hook delivery claims, once per message id: a message with one is a hook command, not an /agent.
HOOK_KEY = "hook:"
SIGNED_OUT_ERROR = "还没登录飞书"
SIGN_IN_GUIDE = ["在连接器设置里打开「飞书（LarkAgentX）」，用飞书 App 扫码登录。"]
SEND_SCHEMA = {
    "type": "object",
    "properties": {
        "chat": {"type": "string", "description": "The chat's id, from larkx_chats or an inbox item."},
        "text": {"type": "string", "description": "The message, plain text. Write @name exactly as in mentions."},
        "mentions": {"type": "array", "maxItems": 100, "description": "People or bots to really @; a plain @name without an entry here is ordinary text.",
                     "items": {"type": "object", "additionalProperties": False, "required": ["id", "name"],
                               "properties": {"id": {"type": "string", "description": "The person or bot numeric id from larkx_members or larkx_search."},
                                              "name": {"type": "string", "description": "The exact display name from larkx_members or larkx_search."}}}},
        "reply_to": {"type": "string", "description": "A message's id (from larkx_messages), to answer in its topic."},
    },
    "required": ["chat", "text"],
}
SEND_DM_SCHEMA = {
    "type": "object",
    "properties": {
        "user": {"type": "string", "description": "The person's id, from larkx_search."},
        "text": {"type": "string", "description": "The message, plain text."},
    },
    "required": ["user", "text"],
}
ACTIONS = [
    {"name": "larkx_send", "input_schema": SEND_SCHEMA,
     "description": "Send a Feishu message to a chat as the user themself, not a bot. To really @ a person or bot, pass their id and exact name from larkx_members or larkx_search in mentions and write @name in text; @name alone is ordinary text. The user confirms the full text on the phone first."},
    {"name": "larkx_send_dm", "input_schema": SEND_DM_SCHEMA,
     "description": "Send a Feishu direct message to a person as the user themself, not a bot. The user confirms the full text on the phone first."},
]
TOOLS = [
    {"name": "larkx_status", "description": "Say whether the user's Feishu account is signed in and its messages are being received.",
     "input_schema": {"type": "object", "properties": {}}},
    {"name": "larkx_chats", "description": "List the Feishu chats messages were seen in since sign-in, newest first, with their ids.",
     "input_schema": {"type": "object", "properties": {
         "limit": {"type": "integer", "minimum": 1, "maximum": 50},
         "query": {"type": "string", "description": "Only chats whose name contains this."}}}},
    {"name": "larkx_messages", "description": "Read the latest messages of one Feishu chat, oldest first, with each message's id and topic root/parent ids (pass an id to larkx_send's reply_to to answer in that topic). For a full-text search use kb_search.",
     "input_schema": {"type": "object", "properties": {
         "chat": {"type": "string"},
         "limit": {"type": "integer", "minimum": 1, "maximum": 50}}, "required": ["chat"]}},
    {"name": "larkx_members", "description": "List members and bots of a Feishu group chat with their numeric ids and exact display names for real @ mentions.",
     "input_schema": {"type": "object", "properties": {
         "chat": {"type": "string"},
         "limit": {"type": "integer", "minimum": 1, "maximum": 200}}, "required": ["chat"]}},
    {"name": "larkx_search", "description": "Find Feishu people and groups by name; gives the ids larkx_send_dm and larkx_messages take.",
     "input_schema": {"type": "object", "properties": {"query": {"type": "string"}}, "required": ["query"]}},
]


def _mentions(args, text):
    if not args.get("mentions"):
        return []
    try:
        mention_parts(text, args.get("mentions") or [])
    except ValueError as e:
        raise RpcError(str(e), "invalid_params") from e
    return args.get("mentions") or []


def rfc3339(at):
    return datetime.fromtimestamp(at, timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def local_time(at):
    return datetime.fromtimestamp(at).strftime("%m-%d %H:%M")


def media_ref(msg_id, key):
    return "m:%s:%s" % (msg_id, key)


class Connector:
    def __init__(self, backend, store, hub, rules="", suffix="", prefix=""):
        self.backend = backend
        # The user's own triage rules (setting triage_rules): handed over with every inbox item, the hub uses them
        # instead of its global ones. The hub reads at most 4000 characters.
        self.rules = (rules or "").strip()[:4000]
        # The trigger prefix (setting agent_prefix). Blank or invalid values keep AGENT_PREFIX so ordinary messages
        # cannot accidentally trigger an agent reply.
        self.prefix = normalize_agent_prefix(prefix)
        self.inbox_triage_chats = frozenset()
        # The line every /agent answer ends with (setting agent_reply_suffix). Blank or too short keeps AGENT_SUFFIX: an
        # answer is always marked as written by AI. Stripped after the cut too, since a message is matched against it
        # rstripped.
        suffix = (suffix or "").strip()[:AGENT_SUFFIX_MOST].strip()
        self.suffix = suffix if len(suffix) >= AGENT_SUFFIX_LEAST else AGENT_SUFFIX
        # A fresh message from the signed-in user with a real AT element for that same account is also a request.
        self.mention = True
        # A group request carries its topic as the channel message's thread: the hub keeps one conversation per topic
        # (setting agent_group_topics).
        self.topics = True
        # The user's own /commands that go to their own address instead of the assistant (hooks.Hooks), or None.
        self.hooks = None
        self.store = store
        self.hub = hub
        self._ready = threading.Event()
        self._boot_error = ""
        self._lock = threading.Lock()
        # Serialize backend authentication changes; logout also cancels startup restoration before taking this lock.
        self._session_lock = threading.RLock()
        self._restore_cancel = threading.Event()
        self._account_id = ""
        self._account = ""
        self._signed_in = False
        self._expired = False
        self._link_up = False
        self._linked = threading.Event()
        # The user's chat with themself, by account: where pushes go. Emptied at every sign-in and sign-out, so the first
        # push of each sign-in checks the chat again (_own_chat).
        self._own_chats = {}
        # Group member/bot roster cache, keyed by chat id: (fetched_at, fetched_limit, list[member]).
        # Cleared on sign-in and sign-out; _account_gen prevents an in-flight fetch from repopulating after account change.
        self._members_cache = {}
        self._account_gen = 0
        self._unreachable = False
        self._last_event = 0.0
        self._last_sync = 0.0
        self._last_prune = 0.0
        self._stop = threading.Event()
        self._catch_stop = threading.Event()
        self._catch_queue = queue.Queue()
        self._catch_pending = False
        self._worker_lock = threading.Lock()
        self._workers = set()
        self._closed = False
        self._catch_thread = None
        self._dirty = threading.Event()
        self.methods = {
            "status": self.status, "tools.list": self.tools_list, "tools.call": self.tools_call,
            "actions.list": self.actions_list, "actions.describe": self.actions_describe, "actions.do": self.actions_do,
            "inbox.collect": self.inbox_collect, "inbox.reply": self.inbox_reply,
            "thread.page": self.thread_page, "thread.media": self.thread_media,
            "archive.changes": self.archive_changes, "archive.media": self.archive_media,
            "login.step": self.login_step, "logout": self.logout,
        }
        self.channel_methods = {"send": self.channel_send, "typing": self.channel_typing}

    def start(self):
        """Gets the program going behind the handshake: the hub does not wait for the dependencies or the sign-in."""
        threading.Thread(target=self._boot, daemon=True).start()
        threading.Thread(target=self._announce, daemon=True).start()

    def _boot(self):
        try:
            self.backend.prepare()
        except Exception as e:
            self._boot_error = str(e) or type(e).__name__
            self._ready.set()
            return
        with self._session_lock:
            if self._restore_cancel.is_set():
                self._ready.set()
                return
            self.backend.on_credentials(self._save_credentials)
            try:
                saved = self._load_credentials()
            except Exception as e:
                self._log("reading the sign-in failed: %s" % type(e).__name__)
                saved = None
        while saved and not self._restore_cancel.is_set():
            try:
                with self._session_lock:
                    if self._restore_cancel.is_set():
                        break
                    account = self.backend.restore(saved)
                    # A logout can cancel an in-flight restore while waiting for the backend operation to finish.
                    if self._restore_cancel.is_set():
                        self.backend.logout()
                        break
                    self._begin(account)
                break
            except self.backend.Expired:
                if not self._restore_cancel.is_set():
                    self._expired = True
                break
            except Exception as e:
                if self._restore_cancel.is_set():
                    break
                # Feishu cannot be reached, which says nothing about the session: keep it and try again.
                self._log("restoring the sign-in failed: %s" % type(e).__name__)
                self._unreachable = True
                self._ready.set()
                self._restore_cancel.wait(RESTORE_RETRY_SECONDS)
        self._unreachable = False
        self._ready.set()

    def _load_credentials(self):
        data = self.hub.call(SECRETS + "load", {"contribution": ID}).get("data")
        return json.loads(base64.b64decode(data)) if data else None

    def _save_credentials(self, saved):
        data = base64.b64encode(json.dumps(saved).encode()).decode()
        self.hub.call(SECRETS + "save", {"contribution": ID, "data": data})

    def _begin(self, account):
        """Signed in as account: starts listening."""
        with self._lock:
            self._account_id = account["id"]
            self._account = account["name"]
            self._signed_in = True
            self._expired = False
            self._own_chats = {}
            self._members_cache = {}
            self._account_gen += 1
        self._stop.set()
        self._stop = threading.Event()
        self._linked.clear()
        self._dirty.set()
        if not self._closed:
            threading.Thread(target=self._listen, args=(self._stop,), name="larkx-listen", daemon=True).start()
        self._linked.wait(LINK_WAIT_SECONDS)

    def _listen(self, stop):
        thread = threading.current_thread()
        with self._worker_lock:
            if self._closed:
                return
            self._workers.add(thread)
        try:
            self.backend.listen(self._on_live, self._on_link, stop)
        finally:
            with self._worker_lock:
                self._workers.discard(threading.current_thread())

    def close(self):
        """Stop receiving and wait for history writes before closing the store; safe to call more than once."""
        with self._worker_lock:
            self._closed = True
            self._stop.set()
            self._catch_stop.set()
            self._catch_queue.put(None)
        if self._catch_thread is not None:
            self._catch_thread.join(30)
            with self._worker_lock:
                self._workers.discard(self._catch_thread)
        while True:
            with self._worker_lock:
                workers = [w for w in self._workers if w is not threading.current_thread()]
            if not workers:
                break
            for worker in workers:
                worker.join(30)
        self.store.close()

    def _on_live(self, m):
        """A message pushed on the long connection: kept like any other, the user's own hook command goes to their
        hook, and their own /agent to the assistant. Fetched history never does either (catch_up calls on_message), so
        a restart handles nothing twice."""
        if self.on_message(m) and not self._maybe_hook(m):
            self._maybe_agent(m)

    def _on_link(self, state):
        """The long connection went up, went down, or the session is over."""
        with self._lock:
            self._link_up = state == "up"
            if state == "expired":
                self._signed_in = False
                self._expired = True
        self._linked.set()
        if state == "up":
            with self._worker_lock:
                if not self._catch_pending and not self._closed:
                    self._catch_pending = True
                    if self._catch_thread is None:
                        self._catch_thread = threading.Thread(target=self._catch_worker, name="larkx-catch-up", daemon=True)
                        self._workers.add(self._catch_thread)
                        self._catch_thread.start()
                    self._catch_queue.put(True)

    def _catch_worker(self):
        while True:
            item = self._catch_queue.get()
            try:
                if item is None:
                    return
                self.catch_up()
            finally:
                with self._worker_lock:
                    self._catch_pending = False
                    self._catch_queue.task_done()

    def catch_up(self):
        """Fetches what came in while the connection was down, stopping with the listener."""
        for chat in self.store.chats(CATCH_UP_CHATS):
            if self._catch_stop.is_set():
                return
            try:
                newest = self.backend.chat_info(chat["id"]).get("last_position") or 0
                known = self.store.newest_position(chat["id"])
                if known and newest > known:
                    first = max(known + 1, newest - CATCH_UP_MESSAGES + 1)
                    for m in self.backend.history(chat["id"], list(range(first, newest + 1))):
                        if self._catch_stop.is_set():
                            return
                        self.on_message(m)
            except Exception as e:
                self._log("catching up failed: %s" % type(e).__name__)

    def on_message(self, m):
        """One message, live or fetched. True when it is new."""
        mine = bool(self._account_id) and m.get("sender") == self._account_id
        name = "" if m.get("type") == "SYSTEM" else self._sender_name(m, mine)
        self.store.touch_chat(m["chat"], m.get("chat_kind") or "group", m.get("at") or 0)
        if not self.store.add_message(m, name, mine):
            return False
        self._name_chat(m, name, mine)
        with self._lock:
            self._last_event = time.time()
        self._dirty.set()
        return True

    # /agent: the channel (docs/extensions/channels.md "Contributing a channel" in Persona)

    def _agent_request(self, m, me):
        """The request in a user's own message, or None. A real self-mention and /agent share the answer-marker guard,
        and channel.send uses this same rule as live receipt, so a request can always be answered. A command that went
        to its hook is never one, even with a self-mention in it."""
        text = m.get("text") or ""
        if text.rstrip().endswith(self.suffix):
            return None
        if self.store.claimed(HOOK_KEY + m["id"]):
            return None
        for source in _command_texts(m):
            request = agent_request(source, self.prefix)
            if request is not None:
                return request
        return mention_request(text, m.get("mentions"), me) if self.mention else None

    def _own_fresh(self, m):
        """The signed-in account when m is its own fresh text or rich text, else "": what a request has to be."""
        with self._lock:
            me = self._account_id
        if not me or m.get("sender") != me or m.get("type") not in AGENT_TYPES:
            return ""
        if time.time() - (m.get("at") or 0) > AGENT_FRESH_SECONDS:
            return ""
        return me

    def _maybe_agent(self, m):
        """Hands the user's own fresh "/agent …" or self-mention to the hub, which puts it in the assistant's chat and
        answers through channel.send. Someone else's request, an old one, and our own answers never count."""
        me = self._own_fresh(m)
        if not me:
            return
        request = self._agent_request(m, me)
        if request is None:
            return
        threading.Thread(target=self._hand_over, args=(m, request), daemon=True).start()

    def _hand_over(self, m, request):
        message = {"id": m["id"], "text": request or (AGENT_EMPTY % self.prefix)}
        message.update(_channel_routing(m, topics=self.topics))
        thread = self._thread(m)
        if thread:
            message["thread"] = thread
        context = self._agent_context(m)
        if context:
            message["forward"] = context
        parent = self.store.message(m.get("parent") or "") if m.get("parent") else None
        if parent:
            message["quote"] = _line(parent)
        for wait in AGENT_RETRY_SECONDS + [None]:
            try:
                media = [x for x in m.get("media") or [] if x.get("kind") == "image" and x.get("key")]
                images = []
                for item in media[:CHANNEL_IMAGES_MOST]:
                    mime, data = self.backend.download(m["id"], m["chat"], item["key"], CHANNEL_IMAGE_MOST)
                    images.append({"mime": mime, "data": base64.b64encode(data).decode()})
                if images:
                    message["images"] = images
                # The hub keeps each message id once: handing it over again after a cut-off call answers it once.
                self.hub.call(CHANNEL + "receive", {"contribution": ID, "message": message})
                return
            except Exception as e:
                if wait is None:
                    self._log("handing /agent to the hub failed: %s" % type(e).__name__)
                    return
                time.sleep(wait)

    # command hooks (hooks.py): the user's own /commands, signed, to the user's own address

    def _maybe_hook(self, m):
        """Hands the user's own fresh message starting with a hook command to the hook, once per message id. True when
        it was one, delivered now or before: then it is not an /agent request too, even with a self-mention in it. The
        same guards as /agent: someone else's message, an old one, fetched history and our own answers never count."""
        hooks = self.hooks
        if hooks is None or not hooks.enabled or not self._own_fresh(m):
            return False
        if (m.get("text") or "").rstrip().endswith(self.suffix):
            return False
        found = None
        for source in _command_texts(m):
            found = hooks.match(source)
            if found is not None:
                break
        if found is None:
            return False
        claimed, _ = self.store.claim_send(HOOK_KEY + m["id"])
        if claimed:
            hooks.dispatch(self._hook_payload(m, hooks, *found))
        return True

    def _hook_payload(self, m, hooks, command, request, raw_text):
        chat = self.store.chat(m["chat"]) or {}
        payload = {
            "delivery_id": delivery_id(m["id"]), "command": command, "text": request, "raw_text": raw_text,
            "message": {"id": m["id"], "chat": m["chat"], "chat_kind": m.get("chat_kind") or chat.get("kind") or "group",
                        "chat_name": chat.get("name") or "", "root": m.get("root") or "", "parent": m.get("parent") or "",
                        "at": rfc3339(m.get("at") or time.time())},
        }
        parent = self.store.message(m["parent"]) if hooks.quote and m.get("parent") else None
        if parent:
            payload["quote"] = _hook_message(parent)
        if hooks.context:
            before = [x for x in self.store.messages(m["chat"], m.get("position") or 0, hooks.context + 1) if x["id"] != m["id"]]
            before = [x for x in before if x["type"] != "SYSTEM"][-hooks.context:]
            if before:
                payload["context"] = [_hook_message(x) for x in before]
        return payload

    def _thread(self, m):
        """The topic a group request belongs to, "" for a direct chat: the topic it was written in, or the one its answer
        opens under it (channel_send replies under the same root). The hub gives each one a conversation of its own."""
        if not self.topics or (m.get("chat_kind") or "group") != "group":
            return ""
        return "%s:%s" % (m["chat"], m.get("root") or m["id"])

    def _chat_members_cached(self, chat_id, limit=100, ttl=MEMBERS_CACHE_SECONDS):
        """Fetches and caches members (humans and bots) of a group chat."""
        now = time.time()
        with self._lock:
            if not self._signed_in:
                return []
            gen = self._account_gen
            cached = self._members_cache.get(chat_id)
            if cached and now - cached[0] < ttl:
                _, fetched_limit, members = cached
                if fetched_limit >= limit or len(members) < fetched_limit:
                    return list(members[:limit])
        if not hasattr(self.backend, "chat_members"):
            return []
        try:
            members = self.backend.chat_members(chat_id, limit=limit) or []
        except Exception as e:
            self._log("fetching chat members failed: %s" % type(e).__name__)
            return []
        if not members:
            return []
        with self._lock:
            if not self._signed_in or self._account_gen != gen:
                return []
            prev = self._members_cache.get(chat_id)
            if not prev or prev[1] <= limit or now - prev[0] >= ttl:
                self._members_cache[chat_id] = (time.time(), limit, list(members))
        # Only backfill missing canonical real names; never overwrite global names with per-group nicknames.
        for item in members:
            uid = str(item.get("id") or "")
            real_name = (item.get("real_name") or "").strip()
            if uid.isdigit() and real_name and not self.store.name(uid):
                self.store.set_name(uid, real_name)
        return list(members[:limit])

    def _agent_context(self, m):
        """Where the request was written and what was said there just before it, for the assistant."""
        chat = self.store.chat(m["chat"]) or {}
        before = [x for x in self.store.messages(m["chat"], m.get("position") or 0, AGENT_CONTEXT_MESSAGES) if x["id"] != m["id"]]
        chat_kind = chat.get("kind") or m.get("chat_kind") or "group"
        where = "飞书%s「%s」" % (_kind_label(chat_kind), chat.get("name") or "未命名")
        members_line = ""
        if chat_kind == "group" and self._signed_in:
            members = self._chat_members_cached(m["chat"], limit=60)
            if members:
                bots = [x for x in members if x.get("kind") == "bot"]
                users = [x for x in members if x.get("kind") != "bot"]
                shown = bots[:20] + users[:20]
                tags = ["%s[%s](id=%s)" % (x["name"], "BOT" if x.get("kind") == "bot" else "成员", x["id"]) for x in shown]
                members_line = "本群成员与机器人（回复中写 @显示名 可直接触发真艾特）：" + "、".join(tags) + "\n"
        if not before:
            return members_line + "这条 %s 写在%s里。" % (self.prefix, where)
        return members_line + "这条 %s 写在%s里，之前的 %d 条消息：\n%s" % (self.prefix, where, len(before), "\n".join(_line(x) for x in before))

    def channel_send(self, p):
        """An answer for one /agent: in the chat it was written in, under it, as the user, marked as written by AI. A
        push (kind push, no reply_to) goes only to the user's own chat with themself. A ref is sent once; a send whose
        result is unknown is never tried again."""
        ref = p.get("ref") or ""
        if not ref:
            raise RpcError("channel.send without a ref", "invalid_params")
        with self._lock:
            signed_in, me = self._signed_in, self._account_id
        text = (p.get("text") or "").rstrip() + "\n\n" + self.suffix
        if not p.get("reply_to") and p.get("kind") == PUSH:
            if not signed_in:
                raise RpcError(SIGNED_OUT_ERROR, CANNOT_SEND)
            return self._channel_out("channel:" + ref, self._own_chat(me), text, None)
        request = self.store.message(p.get("reply_to") or "") if p.get("reply_to") else None
        if not signed_in or request is None or request["sender"] != me or self._agent_request(request, me) is None:
            # Only an answer to one of the user's own /agent goes out: nothing in a chat they did not ask in.
            raise RpcError("没有可以回复的 %s 消息" % self.prefix, CANNOT_SEND)
        return self._channel_out("channel:" + ref, request["chat"], text, request.get("root") or request["id"],
                                 lambda: self._context_mentions(request, text))

    def _channel_out(self, key, chat, text, root, mentions=None):
        """Sends one channel message once, keyed on its ref: again, it answers what the first send ended with."""
        claimed, result = self.store.claim_send(key)
        if not claimed:
            if result:
                return {"message_id": result}
            raise RpcError(_UNKNOWN_SEND)
        try:
            msg_id = self.backend.send(chat, text, root, mentions=mentions() if mentions else [])
        except Exception:
            msg_id = None
        if not msg_id:
            raise RpcError(_UNKNOWN_SEND)
        self.store.finish_send(key, str(msg_id))
        return {"message_id": str(msg_id)}

    def _own_chat(self, me):
        """The user's chat with themself. A push may carry anything the assistant knows (a main-chat turn, a reminder,
        a scheduled task), so it never goes to a chat anybody else reads: no other chat is ever used instead."""
        chat = self._own_chats.get(me)
        if chat:
            return chat
        try:
            chat = self.backend.own_chat()
        except Exception as e:
            self._log("finding the chat with yourself failed: %s" % type(e).__name__)
            chat = None
        if not chat:
            raise RpcError("找不到你和自己的私聊", CANNOT_SEND)
        self._own_chats[me] = str(chat)
        return str(chat)

    def channel_typing(self, _):
        return {}

    def _context_mentions(self, request, text):
        """Known participants' real ids (chat members/bots plus recent senders and mentions), for names the assistant
        wrote. Unrecognized or ambiguous @words stay plain text without dropping valid mentions."""
        if "@" not in text:
            return []
        recent = self.store.messages(request["chat"], 0, MENTION_CONTEXT_MESSAGES)
        candidates = {}  # name -> set of numeric user ids

        def add_candidate(uid, raw_name):
            uid = str(uid or "")
            name = (raw_name or "").lstrip("@").strip()
            if uid.isdigit() and int(uid) > 0 and name and "@" not in name and "\n" not in name:
                candidates.setdefault(name, set()).add(uid)

        chat_row = self.store.chat(request["chat"]) or {}
        if (chat_row.get("kind") or "group") == "group":
            for item in self._chat_members_cached(request["chat"], limit=200):
                add_candidate(item.get("id"), item.get("name"))
                if item.get("real_name") and item.get("real_name") != item.get("name"):
                    add_candidate(item.get("id"), item.get("real_name"))

        for m in recent:
            sender = str(m.get("sender") or "")
            sname = (m.get("sender_name") or "").strip()
            if sender.isdigit() and int(sender) > 0:
                if sname and sname not in ("我", UNKNOWN_SENDER) and "@" + sname in text and not self.store.name(sender):
                    try:
                        found = self.backend.user_name(sender, request["chat"])
                    except Exception:
                        found = None
                    if found:
                        self.store.set_name(sender, found)
                known = (self.store.name(sender) or (sname if sname not in ("我", UNKNOWN_SENDER) else "")).strip()
                add_candidate(sender, known)
            for item in m.get("mentions") or []:
                if not isinstance(item, dict):
                    continue
                uid = str(item.get("id") or "")
                label = (item.get("label") or "").lstrip("@").strip()
                add_candidate(uid, label)
                if uid.isdigit() and label and not self.store.name(uid):
                    self.store.set_name(uid, label)

        by_uid = {}
        matched_claimed = []
        ambiguous_claimed = []
        for name, uids in sorted(candidates.items(), key=lambda kv: len(kv[0]), reverse=True):
            raw_spans = _mention_spans(text, name)
            if not raw_spans:
                continue
            unclaimed = [
                sp for sp in raw_spans
                if not any(sp[0] < c_end and sp[1] > c_start for c_start, c_end in matched_claimed)
                and not any(sp[0] < c_end and sp[1] > c_start for c_start, c_end in ambiguous_claimed)
            ]
            if not unclaimed:
                continue
            if len(uids) != 1:
                ambiguous_claimed.extend(unclaimed)
                continue
            # If any boundary-valid occurrence of @name sits inside an ambiguous longer @name (e.g. @Bot inside
            # ambiguous @Bot Pro), skip this shorter name so mention_parts will not mis-mention at that position.
            if any(
                sp[0] < c_end and sp[1] > c_start
                for sp in raw_spans
                for c_start, c_end in ambiguous_claimed
            ):
                continue
            uid = next(iter(uids))
            if uid not in by_uid:
                by_uid[uid] = name
                matched_claimed.extend(unclaimed)
        targets = [{"id": uid, "name": name} for uid, name in by_uid.items()]
        if not targets:
            return []
        try:
            mention_parts(text, targets)
        except ValueError:
            return []
        return targets

    def _sender_name(self, m, mine):
        if mine:
            return "我"
        sender = m.get("sender") or ""
        known = self.store.name(sender)
        if known:
            return known
        try:
            found = self.backend.user_name(sender, m["chat"])
        except Exception:
            found = None
        if found:
            self.store.set_name(sender, found)
            return found
        return UNKNOWN_SENDER

    def _name_chat(self, m, sender_name, mine):
        chat = self.store.chat(m["chat"])
        if chat is None or chat["name"]:
            return
        if chat["kind"] == "dm" and not mine and sender_name and sender_name != UNKNOWN_SENDER:
            self.store.name_chat(m["chat"], sender_name)
            return
        try:
            name = self.backend.chat_info(m["chat"]).get("name")
        except Exception:
            name = ""
        # A direct chat seen only through the user's own messages (the chat with oneself among them) keeps no name: a
        # placeholder would stay for good, and a confirmation must not name a recipient it does not know.
        if name or chat["kind"] != "dm":
            self.store.name_chat(m["chat"], name or "飞书群")

    def _announce(self):
        """Tells the hub something moved, at most every CHANGED_SECONDS."""
        while True:
            self._dirty.wait()
            self._dirty.clear()
            for scope in ("inbox", "chats"):
                try:
                    self.hub.emit("connector.changed", {"contribution": ID, "scope": scope})
                except Exception:
                    return
            time.sleep(CHANGED_SECONDS)

    def _log(self, line):
        self.hub.log(line)

    # status

    def status(self, _):
        with self._lock:
            if self._boot_error:
                return {"state": "error", "error": "运行环境没装好：" + self._boot_error[:200],
                        "guide": ["hub 所在的机器要有 Python 3.9 以上，能运行 python3 -m pip，并能访问 PyPI（pypi.org、files.pythonhosted.org）"
                                  "和 GitHub（codeload.github.com）：第一次启动要从这几处下载 LarkAgentX 和它的依赖。"]}
            if not self._ready.is_set():
                return {"state": "error", "error": "正在准备运行环境，第一次要一两分钟。"}
            if self._unreachable:
                return {"state": "error", "error": "连不上飞书，正在重试。"}
            if not self._signed_in:
                error = "飞书登录失效了" if self._expired else SIGNED_OUT_ERROR
                return {"state": "not_signed_in", "error": error, "guide": SIGN_IN_GUIDE}
            out = {"state": "ok", "account": self._account, "via": "LarkAgentX 长连接"}
            if not self._link_up:
                out["guide"] = ["长连接断了，正在重连。"]
            if self.hooks is not None and self.hooks.problem:
                out.setdefault("guide", []).append(self.hooks.problem)
            if self._last_sync:
                out["last_sync"] = rfc3339(self._last_sync)
            if self._last_event:
                out["last_event"] = rfc3339(self._last_event)
            return out

    # tools

    def tools_list(self, _):
        return {"tools": TOOLS}

    def tools_call(self, p):
        name, args = p.get("name"), p.get("args") or {}
        if name == "larkx_status":
            s = self.status({})
            return {"text": "%s: %s" % (s.get("account") or "飞书", "在收消息" if s["state"] == "ok" else s.get("error", s["state"]))}
        if name == "larkx_chats":
            rows = self.store.chats(_limit(args, 20), args.get("query") or "")
            lines = ["%s  %s〔%s〕 %s" % (r["id"], _title(r), _kind_label(r["kind"]), local_time(r["last_at"])) for r in rows]
            return {"text": "\n".join(lines) or "还没有收到过消息。"}
        if name == "larkx_messages":
            chat = _required(args, "chat")
            rows = self.store.messages(chat, 0, _limit(args, 20))
            return {"text": "\n".join(_message_line(m) for m in rows) or "这个会话还没有消息。"}
        if name == "larkx_members":
            self._need_signed_in()
            chat = _required(args, "chat")
            try:
                limit = max(1, min(int(args.get("limit") or 100), 200))
            except (TypeError, ValueError):
                limit = 100
            members = self._chat_members_cached(chat, limit=limit, ttl=60)
            lines = ["%s  %s〔%s〕" % (m["id"], m["name"], "BOT" if m.get("kind") == "bot" else "成员") for m in members]
            return {"text": "\n".join(lines) or "没有拉到群成员。"}
        if name == "larkx_search":
            self._need_signed_in()
            found = self.backend.search(_required(args, "query"))
            return {"text": "\n".join("%s  %s〔%s〕" % (r["id"], r["title"], r["type"]) for r in found) or "没有找到。"}
        raise RpcError("没有这个工具：%s" % name)

    def _need_signed_in(self):
        if not self._signed_in:
            raise RpcError(SIGNED_OUT_ERROR)

    # actions: only actions.do writes, and only after the hub's gate

    def actions_list(self, _):
        return {"tools": ACTIONS}

    def actions_describe(self, p):
        name, args = p.get("name"), p.get("args") or {}
        text = _required(args, "text")
        quoted = ""
        if name == "larkx_send":
            chat = _required(args, "chat")
            row = self.store.chat(chat) or {}
            kind = row.get("kind") or "group"
            title = row.get("name") or (DM_UNKNOWN if kind == "dm" and row else chat)
            if kind == "dm" and not row.get("name"):
                where = "这个私聊里的对方（还不知道是谁）"
            else:
                where = "『%s』一个人" % title if kind == "dm" else "群『%s』里的所有人" % title
            target = "%s〔%s〕" % (title, _kind_label(kind))
            if args.get("reply_to"):
                quoted = self._quoted(args["reply_to"])
        elif name == "larkx_send_dm":
            user = _required(args, "user")
            title = self.store.name(user) or user
            where = "『%s』一个人" % title
            target = "%s〔私聊〕" % title
        else:
            raise RpcError("没有这个操作：%s" % name)
        summary = "以你本人的身份发飞书消息\n发给：%s\n%s内容（全文）：\n%s" % (target, quoted, text)
        return {"action": {"name": name, "visible": where}, "summary": summary}

    def _quoted(self, reply_to):
        """The confirmation's line for reply_to: the reply goes into that message's topic, so it says which one."""
        m = self.store.message(reply_to)
        if m is None:
            return "回复：话题里的一条消息（这里没有它的内容）\n"
        text = (m["text"] or "[消息]").replace("\n", " ")
        if len(text) > 200:
            text = text[:200] + "…"
        return "回复：%s 的消息「%s」（发在这条消息的话题里）\n" % (m["sender_name"] or "系统", text)

    def actions_do(self, p):
        name, args = p.get("name"), p.get("args") or {}
        # Each request's send runs at most once, keyed on its id: without one, every later send would read as that one.
        request_id = _required(p, "request_id")
        if name == "larkx_send":
            chat, text = _required(args, "chat"), _required(args, "text")
            mentions = _mentions(args, text)
            return {"text": self._send_once("action:" + request_id, chat, text, args.get("reply_to"), mentions)}
        if name == "larkx_send_dm":
            self._need_signed_in()
            user, text = _required(args, "user"), _required(args, "text")
            done = self._claim("action:" + request_id)
            if done:
                return {"text": done}
            return {"text": self._send("action:" + request_id, self.backend.create_chat(user), text, None)}
        raise RpcError("没有这个操作：%s" % name)

    def _send_once(self, key, chat, text, reply_to, mentions=()):
        self._need_signed_in()
        return self._claim(key) or self._send(key, chat, text, reply_to, mentions)

    def _claim(self, key):
        """Takes key for one send: "" when it is this one's to do, else what the first one ended with."""
        claimed, result = self.store.claim_send(key)
        if claimed:
            return ""
        if result:
            return result
        raise RpcError(_UNKNOWN_SEND)

    def _send(self, key, chat, text, reply_to, mentions=()):
        root = None
        if reply_to:
            quoted = self.store.message(reply_to) or {}
            root = quoted.get("root") or reply_to
        try:
            msg_id = self.backend.send(chat, text, root, mentions=mentions)
        except Exception:
            msg_id = None
        if not msg_id:
            # Left without a result on purpose: this key is never tried again.
            raise RpcError(_UNKNOWN_SEND)
        result = "已发出（%s）。" % msg_id
        self.store.finish_send(key, result)
        return result

    # inbox

    def inbox_collect(self, _):
        things, checked, now = [], 0, time.time()
        since = now - INBOX_DAYS * 86400
        for chat in self.store.chats_since(since, INBOX_MOST):
            stale = now - (chat.get("checked_at") or 0) >= MUTE_REFRESH_SECONDS
            if self._signed_in and chat["kind"] == "group" and stale and checked < MUTE_REFRESH_MOST:
                checked += 1
                # A failed check is unknown for this poll and backs off until the next refresh window.
                mute = "unknown"
                try:
                    mute = (self.backend.chat_info(chat["id"]) or {}).get("mute") or "unknown"
                    if mute not in ("muted", "normal", "unknown"):
                        mute = "unknown"
                except Exception as e:
                    self._log("checking the chat mute state failed: %s" % type(e).__name__)
                self.store.mute_chat(chat["id"], mute, now)
                chat = self.store.chat(chat["id"]) or dict(chat, mute=mute, checked_at=now)
            if chat["kind"] == "dm":
                recent = self.store.messages(chat["id"], 0, CONTEXT_LINES)
                scopes = {"": [m for m in recent if m["type"] != "SYSTEM"]}
            else:
                scopes = self.store.inbox_topics(chat["id"], since, CONTEXT_LINES)
            for root, recent in scopes.items():
                thing = self._thing(chat, root, recent)
                if thing:
                    things.append(thing)
        return {"things": things}

    def _thing(self, chat, root, recent):
        if not recent:
            return None
        last = recent[-1]
        dm = chat["kind"] == "dm"
        thread = "%s:%s" % (chat["id"], root) if root else chat["id"]
        last_id = str(last["position"] or last["id"])
        unanswered = []
        for m in reversed(recent):
            if m["mine"]:
                break
            unanswered.append(m)
        mentioned = any(m["at_me"] for m in unanswered)
        kind = "dm" if dm else ("mention" if mentioned else "group")
        if last["mine"]:
            # Keep last absent on settlement: the hub uses it to recognize a reply it sent for a suggestion.
            return self._ruled({"thread": thread, "kind": kind, "done": "replied", "why": "你后来回复了"}, "fyi")
        opted_in = chat["id"] in self.inbox_triage_chats
        if kind == "group" and not opted_in and chat.get("mute") == "muted":
            return None
        title = _title(chat)
        reply_to = "回复会以你本人的身份发给『%s』" % title
        if not dm:
            where = "的这个话题" if root else ""
            reply_to = "回复会以你本人的身份发到群『%s』%s，群里所有人都能看到" % (title, where)
        if root or (kind == "group" and opted_in):
            older = "\n".join(_inbox_line(m) for m in recent[:-1])
            context = ("较早的消息：\n" + older + "\n\n" if older else "") + "最新消息：\n" + _inbox_line(last)
        else:
            context = "\n".join(_line(m) for m in recent)
        thing = {
            "thread": thread, "kind": kind, "title": title, "sender": last["sender_name"],
            "label": {"dm": "私聊", "mention": "群里 @ 了你", "group": "群消息"}[kind],
            "preview": last["text"][:200], "context": context,
            "at": rfc3339(last["at"]), "last": last_id,
            "can_reply": True, "reply_to": reply_to,
            "tags": [{"key": "chat", "value": chat["id"], "name": title}],
        }
        if last["sender"]:
            thing["tags"].append({"key": "sender", "value": last["sender"], "name": last["sender_name"]})
        if kind == "group" and not opted_in and chat.get("mute") != "normal":
            # Unknown mute state keeps the old fixed fold. Confirmed normal groups use model triage instead.
            thing["tier"], thing["why"] = "noise", "规则：群消息，没有 @ 你"
        # The fallback tier (capability fallback): used only when the triage model can't be called.
        return self._ruled(thing, "noise" if kind == "group" else "action")

    def _ruled(self, thing, rule_tier):
        thing.setdefault("extra", {})["rule_tier"] = rule_tier
        if self.rules:
            thing["rules"] = self.rules
        return thing

    def inbox_reply(self, p):
        thread, text = _required(p, "thread"), _required(p, "text")
        key = p.get("client_id") or hashlib.sha256(("%s|%s|%s" % (thread, p.get("last"), text)).encode()).hexdigest()
        chat, _, root = thread.partition(":")
        self._send_once("reply:" + key, chat, text, root or None)
        return {}

    # threads

    def thread_page(self, p):
        thread = _required(p, "thread")
        limit = min(int(p.get("limit") or 20), THREAD_MOST)
        before = int(p.get("before") or 0)
        chat_id, _, root = thread.partition(":")
        chat = self.store.chat(chat_id)
        if chat is None:
            raise RpcError("没有这个会话")
        if root:
            rows = self.store.topic_messages(chat_id, root, before, limit)
        else:
            rows = self.store.messages(chat_id, before, limit)
            if len(rows) < limit and self._signed_in:
                rows = self._fetch_older(chat_id, before, limit, rows)
        out = {
            "chat": {"id": thread, "name": _title(chat), "kind": chat["kind"]},
            "messages": [self._thread_message(m) for m in rows],
        }
        if not root:
            out["chat"]["reply"] = "larkx_send"
        if rows and rows[0]["position"] > 1:
            out["before"] = str(rows[0]["position"])
        return out

    def _fetch_older(self, thread, before, limit, rows):
        """Fills a page from the server when this program has not seen that far back."""
        end = rows[0]["position"] if rows else before
        if not end:
            # Nothing kept (older messages are pruned): the newest page, up to the chat's last message.
            try:
                end = (self.backend.chat_info(thread).get("last_position") or 0) + 1
            except Exception as e:
                self._log("reading the chat failed: %s" % type(e).__name__)
                return rows
        if end <= 1:
            return rows
        first = max(1, end - (limit - len(rows)))
        try:
            for m in self.backend.history(thread, list(range(first, end))):
                self.on_message(m)
        except Exception as e:
            self._log("reading older messages failed: %s" % type(e).__name__)
            return rows
        return self.store.messages(thread, before, limit)

    def _thread_message(self, m):
        if m["type"] == "SYSTEM":
            return {"id": m["id"], "at": rfc3339(m["at"]), "sender": {"name": ""}, "event": True, "text": m["text"] or "系统消息"}
        out = {"id": m["id"], "at": rfc3339(m["at"]), "sender": {"id": m["sender"], "name": m["sender_name"]}}
        if m["mine"]:
            out["mine"] = True
        media = [_thread_media(m["id"], x) for x in m["media"]]
        if media:
            out["media"] = media
        markdown = m.get("markdown") or ""
        display_text = markdown or m["text"]
        if display_text and not (media and not markdown and display_text.startswith("[")):
            out["text"] = display_text
            if markdown:
                out["format"] = "markdown"
        elif not media:
            out["text"] = "[消息]"
        if m["parent"]:
            quoted = self.store.message(m["parent"])
            if quoted:
                out["reply_to"] = {"id": quoted["id"], "from": quoted["sender_name"], "text": quoted["text"][:200]}
        return out

    def thread_media(self, p):
        return self._media(_required(p, "ref"), THREAD_MEDIA_MOST)

    def archive_media(self, p):
        return self._media(_required(p, "ref"), ARCHIVE_MEDIA_MOST)

    def _media(self, ref, most):
        self._need_signed_in()
        parts = ref.split(":", 2)
        if len(parts) != 3 or parts[0] != "m":
            raise RpcError("没有这个附件")
        m = self.store.message(parts[1])
        if m is None:
            raise RpcError("没有这个附件")
        mime, data = self.backend.download(m["id"], m["chat"], parts[2], most)
        return {"mime": mime, "data": base64.b64encode(data).decode()}

    # archive

    def archive_changes(self, p):
        cursor = p.get("cursor") or ""
        limit = int(p.get("limit") or 20)
        seq = int(cursor) if cursor else 0
        self._prune(seq)
        rows = self.store.after(seq, ARCHIVE_ROWS)
        batches, order, more = {}, [], len(rows) == ARCHIVE_ROWS
        for m in rows:
            if m["chat"] not in batches:
                if len(batches) == limit:
                    more = True
                    break
                batches[m["chat"]] = []
                order.append(m["chat"])
            seq = m["seq"]
            if m["type"] != "SYSTEM" and m["at"] and (m["text"] or m["media"]):
                batches[m["chat"]].append(m)
        with self._lock:
            self._last_sync = time.time()
        return {"batches": [self._batch(c, batches[c]) for c in order if batches[c]], "next": str(seq) if seq else "", "more": more}

    def _prune(self, archived):
        """Deletes messages older than KEEP_DAYS that the archive has passed (row archived and before): a cursor the
        hub hands back is one whose rows it has kept."""
        now = time.time()
        if not archived or now - self._last_prune < PRUNE_SECONDS:
            return
        self._last_prune = now
        try:
            gone = self.store.prune(now - KEEP_DAYS * 86400, archived)
        except Exception as e:
            self._log("pruning old messages failed: %s" % type(e).__name__)
            return
        if gone:
            self._log("pruned %d archived messages older than %d days" % (gone, KEEP_DAYS))

    def _batch(self, chat, rows):
        row = self.store.chat(chat) or {}
        rows = sorted(rows, key=lambda m: (m["at"], m["position"]))
        info = {"id": chat, "name": _title(row), "kind": row.get("kind") or "group", "updated_at": rfc3339(rows[-1]["at"])}
        if row.get("mute") in ("muted", "normal"):
            info["muted"] = row["mute"] == "muted"
        return {
            "chat": info,
            "messages": [_archive_message(m) for m in rows],
        }

    # login

    def login_step(self, p):
        if not self._ready.wait(180):
            raise RpcError("运行环境还没准备好，等一两分钟再试。")
        if self._boot_error:
            raise RpcError("运行环境没装好：" + self._boot_error[:200])
        # An explicit QR sign-in supersedes the saved startup session, even while its restore is retrying.
        self._restore_cancel.set()
        with self._session_lock:
            return self._login_step(p)

    def _login_step(self, p):
        if not p.get("step"):
            return self._new_qr()
        try:
            state = self.backend.login_poll()
        except Exception:
            # One look that did not get through: the app asks again in a moment.
            state = "waiting"
        if state == "waiting":
            return self._qr_prompt(self.backend.login_qr())
        if state == "expired":
            return self._new_qr()
        try:
            account, saved = self.backend.login_finish()
        except self.backend.Expired:
            return self._new_qr("没有登录成功，重新扫一次。")
        self.backend.on_credentials(self._save_credentials)
        self._save_credentials(saved)
        self._begin(account)
        return {"done": True, "account": account["name"]}

    def _new_qr(self, error=""):
        try:
            return self._qr_prompt(self.backend.login_start(), error)
        except Exception as e:
            self._log("starting the sign-in failed: %s" % type(e).__name__)
            raise RpcError("连不上飞书，等一下再试。")

    def _qr_prompt(self, content, error=""):
        prompt = {"step": "scan", "kind": "qr", "url": content, "interval": 2,
                  "hint": "用飞书 App 扫码，在手机上确认登录。这是一个新的网页版登录，能在飞书的登录设备里看到和下线。"}
        if error:
            prompt["error"] = error
        return prompt

    def logout(self, _):
        self._restore_cancel.set()
        with self._session_lock:
            self._stop.set()
            self.backend.logout()
            self.hub.call(SECRETS + "delete", {"contribution": ID})
            with self._lock:
                self._signed_in = False
                self._expired = False
                self._link_up = False
                self._account = ""
                self._account_id = ""
                self._own_chats = {}
                self._members_cache = {}
                self._account_gen += 1
        return {}


_UNKNOWN_SEND = "这条消息可能已经发出，没法确认，不要重发，先去飞书里看一下。"
UNKNOWN_SENDER = "飞书用户"
# A direct chat whose other person is not known yet: only the user's own messages were seen in it.
DM_UNKNOWN = "私聊（还不知道对方是谁）"


def _title(chat):
    """A chat's name for people: a direct chat without one says its other person is not known."""
    if chat.get("name"):
        return chat["name"]
    return DM_UNKNOWN if chat.get("kind") == "dm" else "飞书会话"


def _required(args, name):
    value = args.get(name)
    if not isinstance(value, str) or not value.strip():
        raise RpcError("缺少 %s" % name)
    return value


def _limit(args, default):
    try:
        return max(1, min(int(args.get("limit") or default), THREAD_MOST))
    except (TypeError, ValueError):
        return default


def normalize_agent_prefix(value):
    """Returns a valid trigger prefix, or AGENT_PREFIX when blank or invalid."""
    p = (value or "").strip()
    if len(p) < AGENT_PREFIX_LEAST or len(p) > AGENT_PREFIX_MOST:
        return AGENT_PREFIX
    if not p.startswith("/") or p.startswith("//"):
        return AGENT_PREFIX
    if any(ch.isspace() or ch in ":：" or ord(ch) < 32 or 127 <= ord(ch) <= 159 for ch in p):
        return AGENT_PREFIX
    return p


def agent_request(text, prefix=AGENT_PREFIX):
    """What the user asks with "<prefix> …" (case aside, then a space, a colon, a line or nothing), or None when the
    text is not one: "/agents" is not for "/agent"."""
    p = normalize_agent_prefix(prefix)
    s = (text or "").lstrip()
    if s[:len(p)].lower() != p.lower():
        return None
    rest = s[len(p):]
    if rest and not (rest[0].isspace() or rest[0] in ":："):
        return None
    return rest.lstrip(" \t\r\n:：").strip()


def _command_texts(m):
    """Where a command may start in m: its text, and for rich text also the body under its title line."""
    text = m.get("text") or ""
    out = [text]
    if m.get("type") == "POST":
        first, newline, body = text.lstrip().partition("\n")
        if newline and (first == "[富文本]" or first.startswith("[富文本] ")):
            out.append(body)
    return out


def _hook_message(m):
    """A kept message as a hook payload carries it: the quoted one, or one of the context before the command."""
    return {"id": m["id"], "sender_name": m["sender_name"] or "", "mine": bool(m["mine"]), "text": m["text"],
            "at": rfc3339(m["at"] or 0)}


def mention_request(text, mentions, me):
    """The request with the signed-in user's real AT element removed, or None when that element is absent. The sender
    is checked separately; spans, not a name match, decide what to remove. Other mentions remain part of the request."""
    if not me or not isinstance(mentions, list):
        return None
    found = [m for m in mentions if isinstance(m, dict) and str(m.get("id") or "") == str(me)]
    if not found:
        return None
    parts, cursor = [], 0
    for m in sorted(found, key=lambda x: x.get("start", 0)):
        start, end = m.get("start"), m.get("end")
        if not isinstance(start, int) or not isinstance(end, int) or isinstance(start, bool) or isinstance(end, bool):
            return None
        if start < cursor or end <= start or end > len(text):
            return None
        label = m.get("label")
        if not isinstance(label, str) or text[start:end] != label:
            return None
        parts.append(text[cursor:start])
        cursor = end
    parts.append(text[cursor:])
    return "".join(parts).strip()


def _is_mention_word_char(ch):
    return ch.isalnum() or ch in ("_", "-")


def _mention_spans(text, name):
    """All occurrences of '@name' in text ended by whitespace, punctuation, or end-of-text."""
    needle = "@" + name
    nlen = len(needle)
    tlen = len(text)
    spans = []
    pos = 0
    while True:
        idx = text.find(needle, pos)
        if idx < 0:
            break
        end = idx + nlen
        pos = idx + 1
        if end < tlen and _is_mention_word_char(text[end]):
            continue
        spans.append((idx, end))
    return spans


def _kind_label(kind):
    return "私聊" if kind == "dm" else "群"


def _channel_routing(m, topics=True):
    """Additive routing metadata (chat_id, chat_kind, topic_id) for channel.receive; older hubs ignore these fields."""
    out = {}
    chat_id = str(m.get("chat") or "").strip()
    if chat_id:
        out["chat_id"] = chat_id
    raw_kind = str(m.get("chat_kind") or "").strip().lower()
    if raw_kind in ("dm", "direct", "p2p"):
        out["chat_kind"] = "direct"
        return out
    if not topics:
        # agent_group_topics=False sends all group/topic requests to the channel's default target: keep chat_id,
        # suppress topic_id and group/topic classification so per_topic and per_group_and_topic do not split them.
        return out
    topic_id = ""
    # topic_id comes from real.normalize() (derived from anchor/root); thread_id is a defensive fallback.
    for key in ("topic_id", "thread_id", "root"):
        value = str(m.get(key) or "").strip()
        if value and value != "0":
            topic_id = value
            break
    if topic_id:
        out["chat_kind"] = "topic"
        out["topic_id"] = topic_id
    else:
        out["chat_kind"] = "group"
    return out


def _line(m):
    return "%s（%s）：%s" % (m["sender_name"] or "系统", local_time(m["at"]), m["text"])


def _inbox_line(m):
    names = dict.fromkeys(mention["label"] for mention in m["mentions"] if mention.get("label"))
    return ("提及：%s\n" % "、".join(names) if names else "") + _line(m)


def _message_line(m):
    """One larkx_messages line: prefixes message id and non-empty topic root/parent ids for reply_to."""
    msg_id = m.get("id") or ""
    root = m.get("root") or ""
    parent = m.get("parent") or ""
    meta = []
    if msg_id:
        meta.append("id=%s" % msg_id)
    if root and root != "0" and root != msg_id:
        meta.append("root=%s" % root)
    if parent and parent != "0" and parent != root and parent != msg_id:
        meta.append("parent=%s" % parent)
    line = _line(m)
    return "[%s] %s" % (" ".join(meta), line) if meta else line


def _thread_media(msg_id, media):
    out = {"kind": media.get("kind") or "file"}
    if media.get("key"):
        out["ref"] = media_ref(msg_id, media["key"])
    if media.get("name"):
        out["name"] = media["name"]
    return out


def _archive_message(m):
    out = {"id": m["id"], "position": str(m["position"]), "sender_id": m["sender"], "sender_name": m["sender_name"],
           "type": m["type"], "text": m["text"], "at": rfc3339(m["at"])}
    if m["mine"]:
        out["mine"] = True
    elif m["sender"]:
        out["sender_handles"] = [{"kind": "lark", "value": m["sender"]}]
    refs = [media_ref(m["id"], x["key"]) for x in m["media"] if x.get("key")]
    if refs:
        out["media"] = refs
    return out
