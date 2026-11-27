"""Command hooks: the user's own fresh "/todo …" (any command they list in the settings) goes to their own web address as
a signed JSON POST instead of the assistant (docs/extensions/larkx.md "Command hooks"). Off until both a command and
an address are set. Nothing here reads Feishu: the connector decides which message qualifies and builds the payload."""
import hashlib
import hmac
import ipaddress
import json
import os
import re
import threading
import time
import urllib.error
import urllib.request
from urllib.parse import urlsplit, urlunsplit

# The settings (persona-extension.json "configuration" and "secrets"); the manifest's limits are the same.
COMMANDS = "command_hooks"
URL = "command_hook_url"
CONTEXT = "command_hook_context"
QUOTE = "command_hook_quote"
SECRET = "command_hook_secret"
COMMANDS_MOST = 20
CONTEXT_MOST = 20
# A command: a slash, then a letter or digit, then letters, digits, "_" or "-". /agent stays the assistant's.
COMMAND = re.compile(r"/[A-Za-z0-9][A-Za-z0-9_-]{0,31}")
RESERVED = ("/agent",)
# One delivery: a try, then these waits before the next tries, each try bounded by TIMEOUT_SECONDS.
TIMEOUT_SECONDS = 10
RETRY_SECONDS = [2, 5, 15]
ANSWER_MOST = 64 << 10
# The name the hub's egress proxy connects to the hub machine's 127.0.0.1: a sandbox has a loopback of its own.
SANDBOX_HOST = "host.persona.internal"
PROXY_VARIABLES = ("HTTPS_PROXY", "HTTP_PROXY", "https_proxy", "http_proxy", "ALL_PROXY", "all_proxy")
USER_AGENT = "larkx-persona-command-hook"
TIMESTAMP_HEADER = "X-Larkx-Timestamp"
DELIVERY_HEADER = "X-Larkx-Delivery"
SIGNATURE_HEADER = "X-Larkx-Signature"

NO_URL = "/命令 Hook 没有生效：还没填 Hook 地址。"
BAD_URL = ("/命令 Hook 没有生效：Hook 地址要是 https 网址，不带用户名、密码和 #；"
           "同一台机器上的服务（127.0.0.1、localhost、[::1]）可以用 http。")
RESERVED_COMMAND = "/agent 留给助手，不能用作 Hook 命令。"


class Hooks:
    """The command hooks the settings describe. enabled is False when they are off or cannot work; problem then says
    why, for the connector's status, unless nothing was set at all."""

    def __init__(self, commands=(), url="", secret="", context=0, quote=True, log=None, post=None, sleep=None,
                 clock=None, environ=None):
        self.commands, ignored = parse_commands(commands)
        self.secret = secret or ""
        self.context = _bounded(context, 0, CONTEXT_MOST)
        self.quote = quote is not False
        self.problem = ""
        self.url = ""
        if self.commands:
            try:
                self.url = parse_url(url)
            except ValueError:
                self.problem = BAD_URL
            else:
                if not self.url:
                    self.problem = NO_URL
        if ignored and not self.problem:
            self.problem = RESERVED_COMMAND
        self._log = log or (lambda line: None)
        self._post = post or post_json
        self._sleep = sleep or time.sleep
        self._clock = clock or time.time
        self._environ = os.environ if environ is None else environ

    @classmethod
    def from_config(cls, config, secrets, log=None):
        config, secrets = config or {}, secrets or {}
        return cls(config.get(COMMANDS) or (), config.get(URL) or "", secrets.get(SECRET) or "",
                   config.get(CONTEXT) or 0, config.get(QUOTE, True), log)

    @property
    def enabled(self):
        return bool(self.commands and self.url)

    def match(self, text):
        """(command, the request after it, the text from the command on) for the first listed command text starts
        with, or None: the same rule as /agent, so "/todos" is not "/todo"."""
        for command in self.commands:
            request = command_request(text, command)
            if request is not None:
                return command, request, (text or "").strip()
        return None

    def dispatch(self, payload):
        """Delivers payload on a thread of its own: the long connection's reader never waits for the address."""
        threading.Thread(target=self.deliver, args=(payload,), name="larkx-hook", daemon=True).start()

    def deliver(self, payload):
        """POSTs payload, trying again after a failed connection, a timeout, 429 or 5xx. True when it was accepted.
        The log names the command and the outcome only: never the address, which may carry a token, or the text."""
        body = json.dumps(payload, ensure_ascii=False, separators=(",", ":")).encode("utf-8")
        command = payload.get("command") or ""
        url = dial_url(self.url, self._environ)
        tries = 0
        for wait in RETRY_SECONDS + [None]:
            tries += 1
            timestamp = str(int(self._clock()))
            headers = {"Content-Type": "application/json; charset=utf-8", "User-Agent": USER_AGENT,
                       TIMESTAMP_HEADER: timestamp, DELIVERY_HEADER: payload.get("delivery_id") or ""}
            if self.secret:
                headers[SIGNATURE_HEADER] = sign(self.secret, timestamp, body)
            try:
                status = self._post(url, body, headers, TIMEOUT_SECONDS)
                outcome = "HTTP %d" % status
            except Exception as e:
                status, outcome = None, type(e).__name__
            if status is not None and 200 <= status < 300:
                self._log("command hook %s delivered (%s)" % (command, outcome))
                return True
            if status is not None and not retryable(status):
                self._log("command hook %s refused (%s)" % (command, outcome))
                return False
            if wait is None:
                self._log("command hook %s gave up after %d tries (%s)" % (command, tries, outcome))
                return False
            self._sleep(wait)
        return False


def parse_commands(values):
    """The listed commands, lowercase, longest first, each once; and whether a reserved one was left out. Anything that
    is not a command is left out too (the manifest's pattern keeps it from the settings already)."""
    if isinstance(values, str):
        values = [values]
    out, reserved = [], False
    for value in values if isinstance(values, (list, tuple)) else ():
        command = value.strip().lower() if isinstance(value, str) else ""
        if not COMMAND.fullmatch(command):
            continue
        if command in RESERVED:
            reserved = True
            continue
        if command not in out:
            out.append(command)
        if len(out) == COMMANDS_MOST:
            break
    return sorted(out, key=len, reverse=True), reserved


def command_request(text, command):
    """What follows command at the start of text (case aside, then a space, a colon, a line or nothing), or None."""
    s = (text or "").lstrip()
    if s[:len(command)].lower() != command:
        return None
    rest = s[len(command):]
    if rest and not (rest[0].isspace() or rest[0] in ":："):
        return None
    return rest.lstrip(" \t\r\n:：").strip()


def parse_url(value):
    """The hook address: https to any host, or http to this machine's loopback (or the sandbox's name for it), with
    no user info or fragment. "" when none is set; ValueError when it cannot be used."""
    value = (value or "").strip()
    if not value:
        return ""
    u = urlsplit(value)
    host = u.hostname or ""
    if u.username is not None or u.password is not None or u.fragment or not host:
        raise ValueError("hook address")
    u.port  # noqa: B018 - raises ValueError for a port that is not a number in range
    scheme = u.scheme.lower()
    if scheme == "https" or (scheme == "http" and (is_loopback(host) or host.lower() == SANDBOX_HOST)):
        return value
    raise ValueError("hook address")


def is_loopback(host):
    host = (host or "").strip().strip("[]").rstrip(".").lower()
    if host == "localhost" or host.endswith(".localhost"):
        return True
    try:
        return ipaddress.ip_address(host).is_loopback
    except ValueError:
        return False


def dial_url(url, environ):
    """The address to request: with a proxy in the environment (the sandbox), an http loopback host becomes
    host.persona.internal with the same port. https keeps its host, so the certificate still matches."""
    u = urlsplit(url)
    if u.scheme.lower() != "http" or not is_loopback(u.hostname or "") or not any(environ.get(k) for k in PROXY_VARIABLES):
        return url
    netloc = SANDBOX_HOST if u.port is None else "%s:%d" % (SANDBOX_HOST, u.port)
    return urlunsplit((u.scheme, netloc, u.path, u.query, ""))


def sign(secret, timestamp, body):
    """X-Larkx-Signature: sha256= and the hex HMAC-SHA256, keyed with the secret, of the timestamp, a dot and the body."""
    mac = hmac.new(secret.encode("utf-8"), timestamp.encode("ascii") + b"." + body, hashlib.sha256)
    return "sha256=" + mac.hexdigest()


def delivery_id(msg_id):
    """The same for every try and every restart: a receiver can drop a delivery it has already handled."""
    return hashlib.sha256(("larkx-hook|%s" % msg_id).encode("utf-8")).hexdigest()[:32]


def retryable(status):
    return status == 429 or status >= 500


class _NoRedirect(urllib.request.HTTPRedirectHandler):
    """A redirect is an answer, not a new address to post the message to."""

    def redirect_request(self, *args, **kwargs):
        return None


_OPENER = urllib.request.build_opener(_NoRedirect)


def post_json(url, body, headers, timeout):
    """POSTs body and returns the HTTP status, an error status included. The proxy comes from the environment."""
    request = urllib.request.Request(url, data=body, headers=headers, method="POST")
    try:
        with _OPENER.open(request, timeout=timeout) as answer:
            answer.read(ANSWER_MOST)
            return answer.status
    except urllib.error.HTTPError as e:
        e.close()
        return e.code


def _bounded(value, least, most):
    try:
        value = int(value)
    except (TypeError, ValueError):
        return least
    return max(least, min(value, most))
