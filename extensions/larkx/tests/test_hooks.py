"""Command hooks: the user's own fresh "/todo …" goes, signed, to their own address instead of the assistant.
Run them: python3 -m unittest discover -s tests, in extensions/larkx."""
import hashlib
import hmac
import http.server
import json
import os
import sys
import tempfile
import threading
import time
import unittest

sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "src"))
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

from test_agent import ChannelHub  # noqa: E402
from test_connector import ME, Backend, message  # noqa: E402

from larkx_persona import hooks, main  # noqa: E402
from larkx_persona.connector import AGENT_SUFFIX, Connector  # noqa: E402
from larkx_persona.hooks import (BAD_URL, NO_URL, RESERVED_COMMAND, Hooks, command_request, delivery_id, dial_url,  # noqa: E402
                                 parse_commands, parse_url, post_json, sign)
from larkx_persona.main import Plugin  # noqa: E402
from larkx_persona.store import Store  # noqa: E402
from larkx_persona.wire import RpcError  # noqa: E402

URL = "https://hooks.example.com/larkx?token=t0ken"
PROXY = {"HTTPS_PROXY": "http://127.0.0.1:3128"}


class Poster:
    """Stands in for the HTTP POST: records each try and answers with the next status (an exception is raised)."""

    def __init__(self, *answers):
        self.answers = list(answers) or [200]
        self.calls = []
        self.done = threading.Event()

    def __call__(self, url, body, headers, timeout):
        self.calls.append({"url": url, "body": body, "payload": json.loads(body.decode("utf-8")), "headers": headers,
                           "timeout": timeout})
        answer = self.answers.pop(0) if len(self.answers) > 1 else self.answers[0]
        if isinstance(answer, Exception):
            raise answer
        if 200 <= answer < 300 or not hooks.retryable(answer):
            self.done.set()
        return answer


def make(commands=("/todo",), url=URL, poster=None, log=None, **kwargs):
    return Hooks(commands, url, post=poster or Poster(), sleep=lambda s: None, log=log, **kwargs)


class CommandTest(unittest.TestCase):
    def test_what_a_command_is(self):
        commands, reserved = parse_commands(["/Todo", "/todo", "/note-x", "todo", "/", "/a b", 3, "/agent"])
        self.assertEqual(commands, ["/note-x", "/todo"])
        self.assertTrue(reserved)
        self.assertEqual(parse_commands("/note"), (["/note"], False))
        self.assertEqual(parse_commands(None), ([], False))
        self.assertEqual(len(parse_commands(["/c%d" % i for i in range(50)])[0]), hooks.COMMANDS_MOST)

    def test_what_counts_as_a_command_request(self):
        self.assertEqual(command_request("/todo 买牛奶", "/todo"), "买牛奶")
        self.assertEqual(command_request("  /TODO：买牛奶", "/todo"), "买牛奶")
        self.assertEqual(command_request("/todo\n第二行", "/todo"), "第二行")
        self.assertEqual(command_request("/todo", "/todo"), "")
        self.assertIsNone(command_request("/todos 列表", "/todo"))
        self.assertIsNone(command_request("先 /todo 再说", "/todo"))
        self.assertIsNone(command_request("", "/todo"))

    def test_match_takes_the_command_the_text_starts_with(self):
        h = make(("/todo", "/todo-list"))
        self.assertEqual(h.match("/todo-list 本周"), ("/todo-list", "本周", "/todo-list 本周"))
        self.assertEqual(h.match(" /todo 买牛奶 "), ("/todo", "买牛奶", "/todo 买牛奶"))
        self.assertIsNone(h.match("/note 想法"))


class AddressTest(unittest.TestCase):
    def test_https_anywhere_http_only_on_this_machine(self):
        for ok in (URL, "HTTPS://hooks.example.com", "http://127.0.0.1:8080/hook", "http://localhost:9/x",
                   "http://[::1]:9", "http://host.persona.internal:9/x", "http://127.1.2.3/"):
            self.assertEqual(parse_url(ok), ok, ok)
        self.assertEqual(parse_url("  "), "")
        for bad in ("http://hooks.example.com/x", "https://user:pw@hooks.example.com/", "https://hooks.example.com/#x",
                    "ftp://hooks.example.com/", "https:///path", "https://hooks.example.com:99999/", "hooks.example.com"):
            with self.assertRaises(ValueError, msg=bad):
                parse_url(bad)

    def test_the_sandbox_reaches_this_machine_by_name(self):
        self.assertEqual(dial_url("http://127.0.0.1:8080/hook?a=1", PROXY), "http://host.persona.internal:8080/hook?a=1")
        self.assertEqual(dial_url("http://localhost/hook", {"http_proxy": "x"}), "http://host.persona.internal/hook")
        self.assertEqual(dial_url("http://[::1]:9/", PROXY), "http://host.persona.internal:9/")
        self.assertEqual(dial_url("http://127.0.0.1:8080/hook", {}), "http://127.0.0.1:8080/hook")
        self.assertEqual(dial_url(URL, PROXY), URL)

    def test_settings_that_cannot_work_say_why(self):
        self.assertFalse(make((), "").enabled)
        self.assertEqual(make((), "").problem, "")
        self.assertEqual(make(url="").problem, NO_URL)
        self.assertFalse(make(url="").enabled)
        self.assertEqual(make(url="http://hooks.example.com/").problem, BAD_URL)
        self.assertFalse(make(url="http://hooks.example.com/").enabled)
        only_agent = make(("/agent",))
        self.assertEqual((only_agent.enabled, only_agent.problem), (False, RESERVED_COMMAND))
        mixed = make(("/agent", "/todo"))
        self.assertEqual((mixed.enabled, mixed.problem), (True, RESERVED_COMMAND))
        self.assertEqual(make().problem, "")

    def test_from_config_reads_the_settings_and_the_secret(self):
        h = Hooks.from_config({"command_hooks": ["/todo"], "command_hook_url": URL, "command_hook_context": 99,
                               "command_hook_quote": False}, {"command_hook_secret": "s3cret"})
        self.assertEqual((h.commands, h.url, h.secret, h.context, h.quote), (["/todo"], URL, "s3cret", 20, False))
        h = Hooks.from_config({}, None)
        self.assertEqual((h.enabled, h.context, h.quote, h.secret), (False, 0, True, ""))
        self.assertEqual(Hooks.from_config({"command_hook_context": "x"}, {}).context, 0)


class DeliveryTest(unittest.TestCase):
    PAYLOAD = {"delivery_id": "d1", "command": "/todo", "text": "机密内容", "raw_text": "/todo 机密内容"}

    def test_a_signed_delivery(self):
        poster, lines = Poster(200), []
        h = make(poster=poster, secret="s3cret", log=lines.append, clock=lambda: 1700000000.9)
        self.assertTrue(h.deliver(self.PAYLOAD))
        call = poster.calls[0]
        self.assertEqual(call["url"], URL)
        self.assertEqual(call["payload"], self.PAYLOAD)
        self.assertEqual(call["timeout"], hooks.TIMEOUT_SECONDS)
        headers = call["headers"]
        self.assertEqual(headers["X-Larkx-Timestamp"], "1700000000")
        self.assertEqual(headers["X-Larkx-Delivery"], "d1")
        self.assertTrue(headers["Content-Type"].startswith("application/json"))
        want = hmac.new(b"s3cret", b"1700000000." + call["body"], hashlib.sha256).hexdigest()
        self.assertEqual(headers["X-Larkx-Signature"], "sha256=" + want)
        self.assertEqual(sign("s3cret", "1700000000", call["body"]), "sha256=" + want)
        self.assertEqual(lines, ["command hook /todo delivered (HTTP 200)"])

    def test_without_a_secret_nothing_is_signed(self):
        poster = Poster(204)
        self.assertTrue(make(poster=poster).deliver(self.PAYLOAD))
        self.assertNotIn("X-Larkx-Signature", poster.calls[0]["headers"])

    def test_failures_are_tried_again_a_few_times(self):
        poster, waits = Poster(OSError("down"), 503, 429, 200), []
        h = Hooks(("/todo",), URL, post=poster, sleep=waits.append)
        self.assertTrue(h.deliver(self.PAYLOAD))
        self.assertEqual(len(poster.calls), 4)
        self.assertEqual(waits, hooks.RETRY_SECONDS)
        self.assertEqual(len({c["headers"]["X-Larkx-Delivery"] for c in poster.calls}), 1)

    def test_a_refusal_is_not_tried_again(self):
        for status in (400, 302, 404):
            poster, lines = Poster(status), []
            self.assertFalse(make(poster=poster, log=lines.append).deliver(self.PAYLOAD))
            self.assertEqual(len(poster.calls), 1)
            self.assertEqual(lines, ["command hook /todo refused (HTTP %d)" % status])

    def test_it_gives_up_and_the_log_has_no_text_or_address(self):
        poster, lines = Poster(OSError("down")), []
        self.assertFalse(make(poster=poster, log=lines.append).deliver(self.PAYLOAD))
        self.assertEqual(len(poster.calls), len(hooks.RETRY_SECONDS) + 1)
        self.assertEqual(lines, ["command hook /todo gave up after 4 tries (OSError)"])
        for line in lines:
            self.assertNotIn("机密", line)
            self.assertNotIn("t0ken", line)
            self.assertNotIn("example.com", line)

    def test_a_loopback_address_in_the_sandbox(self):
        poster = Poster(200)
        h = Hooks(("/todo",), "http://127.0.0.1:8080/hook", post=poster, sleep=lambda s: None, environ=PROXY)
        h.deliver(self.PAYLOAD)
        self.assertEqual(poster.calls[0]["url"], "http://host.persona.internal:8080/hook")

    def test_the_delivery_id_is_stable(self):
        self.assertEqual(delivery_id("m1"), delivery_id("m1"))
        self.assertNotEqual(delivery_id("m1"), delivery_id("m2"))
        self.assertEqual(len(delivery_id("m1")), 32)


class Receiver(http.server.BaseHTTPRequestHandler):
    status = 200
    got = []

    def do_POST(self):
        body = self.rfile.read(int(self.headers.get("Content-Length") or 0))
        type(self).got.append((self.path, dict(self.headers), body))
        self.send_response(type(self).status)
        if type(self).status in (301, 302, 307, 308):
            self.send_header("Location", "/elsewhere")
        self.send_header("Content-Length", "2")
        self.end_headers()
        self.wfile.write(b"ok")

    def log_message(self, *args):
        pass


class HttpTest(unittest.TestCase):
    """post_json against a real local server: the status comes back, and a redirect is not followed."""

    def setUp(self):
        Receiver.got, Receiver.status = [], 200
        self.server = http.server.HTTPServer(("127.0.0.1", 0), Receiver)
        threading.Thread(target=self.server.serve_forever, daemon=True).start()
        self.addCleanup(self.server.server_close)
        self.addCleanup(self.server.shutdown)
        self.url = "http://127.0.0.1:%d/hook" % self.server.server_address[1]
        # A proxy in the developer's environment would turn the loopback address into the sandbox's name.
        saved = {k: os.environ.pop(k) for k in list(os.environ) if k.lower() in ("http_proxy", "https_proxy", "all_proxy")}
        self.addCleanup(os.environ.update, saved)

    def test_a_post_and_its_status(self):
        self.assertEqual(post_json(self.url, b'{"a":1}', {"Content-Type": "application/json", "X-Larkx-Delivery": "d"}, 5), 200)
        path, headers, body = Receiver.got[0]
        self.assertEqual((path, body, headers.get("X-Larkx-Delivery")), ("/hook", b'{"a":1}', "d"))
        Receiver.status = 500
        self.assertEqual(post_json(self.url, b"{}", {}, 5), 500)

    def test_a_redirect_is_an_answer(self):
        Receiver.status = 302
        self.assertEqual(post_json(self.url, b"{}", {}, 5), 302)
        self.assertEqual([p for p, _, _ in Receiver.got], ["/hook"])

    def test_a_whole_delivery(self):
        h = Hooks(("/todo",), self.url, secret="k", sleep=lambda s: None)
        self.assertTrue(h.deliver({"delivery_id": "d", "command": "/todo", "text": "买牛奶"}))
        _, headers, body = Receiver.got[0]
        self.assertEqual(json.loads(body.decode("utf-8"))["text"], "买牛奶")
        self.assertEqual(headers["X-Larkx-Signature"], sign("k", headers["X-Larkx-Timestamp"], body))


class ConnectorHookTest(unittest.TestCase):
    def setUp(self):
        self.dir = tempfile.TemporaryDirectory()
        self.addCleanup(self.dir.cleanup)
        self.backend = Backend()
        self.hub = ChannelHub()
        self.store = Store(os.path.join(self.dir.name, "x.db"))
        self.c = Connector(self.backend, self.store, self.hub)
        self.poster = Poster(200)
        self.c.hooks = make(("/todo", "/note"), poster=self.poster)
        self.c._ready.set()
        self.backend.poll = "done"
        self.assertTrue(self.c.login_step({"step": "scan"})["done"])
        self.addCleanup(self.c.close)

    def delivered(self):
        self.assertTrue(self.poster.done.wait(2), "nothing was delivered")
        return self.poster.calls[-1]["payload"]

    def nothing(self):
        time.sleep(0.1)
        self.assertEqual(self.poster.calls, [])
        self.assertEqual(self.hub.received, [])

    def test_my_live_command_goes_to_the_hook_not_the_assistant(self):
        self.backend.chats["g1"] = {"name": "项目群", "last_position": 0}
        self.c._on_live(message("m1", "g1", 1, kind="group", text="周五发布"))
        self.c._on_live(message("m2", "g1", 2, kind="group", sender=ME, text="/todo 周五前发版"))
        got = self.delivered()
        self.assertEqual(got["command"], "/todo")
        self.assertEqual(got["text"], "周五前发版")
        self.assertEqual(got["raw_text"], "/todo 周五前发版")
        self.assertEqual(got["delivery_id"], delivery_id("m2"))
        self.assertEqual({k: got["message"][k] for k in ("id", "chat", "chat_kind", "chat_name", "root", "parent")},
                         {"id": "m2", "chat": "g1", "chat_kind": "group", "chat_name": "项目群", "root": "", "parent": ""})
        self.assertRegex(got["message"]["at"], r"^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ$")
        # Context is off by default, and there is nothing to quote.
        self.assertNotIn("context", got)
        self.assertNotIn("quote", got)
        time.sleep(0.1)
        self.assertEqual(self.hub.received, [])

    def test_agent_still_goes_to_the_assistant(self):
        self.c._on_live(message("m1", "d1", 1, sender=ME, text="/agent 总结一下"))
        self.assertTrue(self.hub.got.wait(2))
        self.assertEqual(self.hub.received[0]["message"]["text"], "总结一下")
        self.assertEqual(self.poster.calls, [])

    def test_a_self_mention_with_a_command_is_a_hook_only(self):
        text = "/todo @我的账号 回电话"
        m = message("m1", "d1", 1, sender=ME, text=text)
        m["mentions"] = [{"id": ME, "start": text.index("@"), "end": text.index("@") + 5, "label": "@我的账号"}]
        self.c._on_live(m)
        self.assertEqual(self.delivered()["text"], "@我的账号 回电话")
        time.sleep(0.1)
        self.assertEqual(self.hub.received, [])

    def test_what_never_goes_to_the_hook(self):
        self.c._on_live(message("m1", "d1", 1, sender="u_a", text="/todo 帮我买"))
        self.c._on_live(message("m2", "d1", 2, sender=ME, text="/todo 旧的", at=time.time() - 3600))
        self.c._on_live(message("m3", "d1", 3, sender=ME, text="/todo 好的\n\n" + AGENT_SUFFIX))
        self.c._on_live(message("m4", "d1", 4, sender=ME, text="/todos 不是"))
        self.c._on_live(message("m5", "d1", 5, sender=ME, text="[图片]", msg_type="IMAGE"))
        self.c.on_message(message("m6", "d1", 6, sender=ME, text="/todo 拉取的历史"))
        self.nothing()

    def test_each_message_goes_once(self):
        m = message("m1", "d1", 1, sender=ME, text="/note 想法")
        self.c._on_live(m)
        self.delivered()
        self.c._on_live(m)
        self.assertTrue(self.c._maybe_hook(m))
        # Another program on the same data (a restart) does not deliver it again either.
        other = Connector(self.backend, self.store, self.hub)
        other.hooks = make(("/note",), poster=self.poster)
        other._account_id = ME
        self.assertTrue(other._maybe_hook(m))
        time.sleep(0.1)
        self.assertEqual(len(self.poster.calls), 1)
        self.assertEqual(self.hub.received, [])

    def test_quote_and_context_follow_the_settings(self):
        self.c.hooks = make(("/todo",), poster=self.poster, context=2)
        self.c._on_live(message("m1", "g1", 1, kind="group", text="第一条"))
        self.c._on_live(message("m2", "g1", 2, kind="group", text="第二条"))
        self.c._on_live(message("m3", "g1", 3, kind="group", sender=ME, text="我说的"))
        self.c._on_live(message("m4", "g1", 4, kind="group", sender=ME, text="/todo 跟进", root="m2", parent="m2"))
        got = self.delivered()
        self.assertEqual(got["quote"]["id"], "m2")
        self.assertEqual(got["quote"]["text"], "第二条")
        self.assertEqual(got["quote"]["sender_name"], "小明")
        self.assertEqual([x["text"] for x in got["context"]], ["第二条", "我说的"])
        self.assertEqual([x["mine"] for x in got["context"]], [False, True])
        self.assertEqual((got["message"]["root"], got["message"]["parent"]), ("m2", "m2"))

    def test_no_quote_when_it_is_off(self):
        self.c.hooks = make(("/todo",), poster=self.poster, quote=False)
        self.c._on_live(message("m1", "d1", 1, text="原话"))
        self.c._on_live(message("m2", "d1", 2, sender=ME, text="/todo 记下", parent="m1"))
        self.assertNotIn("quote", self.delivered())

    def test_rich_text_with_a_title(self):
        self.c._on_live(message("m1", "d1", 1, sender=ME, msg_type="POST", text="[富文本] 标题\n/todo 第一行\n第二行"))
        got = self.delivered()
        self.assertEqual(got["text"], "第一行\n第二行")
        self.assertEqual(got["raw_text"], "/todo 第一行\n第二行")

    def test_hooks_that_cannot_work_leave_commands_alone_and_say_why(self):
        self.c.hooks = make(("/todo",), url="", poster=self.poster)
        self.c._on_live(message("m1", "d1", 1, sender=ME, text="/todo 没地址"))
        self.nothing()
        self.assertIn(NO_URL, self.c.status({}).get("guide") or [])
        self.c.hooks = make(poster=self.poster)
        self.assertNotIn(NO_URL, self.c.status({}).get("guide") or [])

    def test_a_command_message_is_not_answered_as_agent(self):
        self.c._on_live(message("m1", "d1", 1, sender=ME, text="/todo 买牛奶"))
        self.delivered()
        with self.assertRaises(RpcError):
            self.c.channel_send({"ref": "r1", "text": "答复", "reply_to": "m1"})
        self.assertEqual(self.backend.sent, [])

    def test_a_command_with_a_self_mention_is_not_answered_as_agent(self):
        # channel.send reloads the kept message: its valid self-mention must not make the hook command an /agent.
        text = "/todo @我的账号 回电话"
        m = message("m1", "d1", 1, sender=ME, text=text)
        m["mentions"] = [{"id": ME, "start": text.index("@"), "end": text.index("@") + 5, "label": "@我的账号"}]
        self.c._on_live(m)
        self.delivered()
        with self.assertRaises(RpcError):
            self.c.channel_send({"ref": "r1", "text": "答复", "reply_to": "m1"})
        self.assertEqual(self.backend.sent, [])
        self.assertEqual(self.hub.received, [])

    def test_a_command_received_with_hooks_off_can_still_be_answered(self):
        # Nothing was handed to a hook, so the self-mention went to the assistant and its answer goes out.
        self.c.hooks = None
        text = "/todo @我的账号 回电话"
        m = message("m1", "d1", 1, sender=ME, text=text)
        m["mentions"] = [{"id": ME, "start": text.index("@"), "end": text.index("@") + 5, "label": "@我的账号"}]
        self.c._on_live(m)
        self.assertTrue(self.hub.got.wait(2))
        self.assertTrue(self.c.channel_send({"ref": "r1", "text": "答复", "reply_to": "m1"})["message_id"])


class PluginHookSettingsTest(unittest.TestCase):
    def test_initialize_hands_the_settings_and_the_secret_over(self):
        got = {}

        class Recorder:
            def __init__(self, *args, **kwargs):
                got["connector"] = self

            def start(self):
                pass

        real = main.Connector
        main.Connector = Recorder
        self.addCleanup(setattr, main, "Connector", real)
        with tempfile.TemporaryDirectory() as data_dir:
            plugin = Plugin({"id": "larkx", "version": "0", "contributes": {}}, lambda *_: None)
            plugin.initialize({"protocol": main.PROTOCOL, "extension": {"data_dir": data_dir},
                               "config": {"command_hooks": ["/todo"], "command_hook_url": URL},
                               "secrets": {"command_hook_secret": "k"}})
            h = got["connector"].hooks
            self.assertEqual((h.enabled, h.commands, h.url, h.secret, h.context, h.quote), (True, ["/todo"], URL, "k", 0, True))
            plugin = Plugin({"id": "larkx", "version": "0", "contributes": {}}, lambda *_: None)
            plugin.initialize({"protocol": main.PROTOCOL, "extension": {"data_dir": data_dir}, "config": {}})
            self.assertFalse(got["connector"].hooks.enabled)

    def test_the_manifest_declares_the_settings(self):
        with open(os.path.join(main.ROOT, "persona-extension.json"), encoding="utf-8") as f:
            manifest = json.load(f)
        props = manifest["configuration"]["properties"]
        self.assertEqual(props["command_hooks"]["default"], [])
        self.assertEqual(props["command_hooks"]["maxItems"], hooks.COMMANDS_MOST)
        self.assertEqual(props["command_hook_url"]["format"], "uri")
        self.assertNotIn("default", props["command_hook_url"])
        self.assertEqual((props["command_hook_context"]["default"], props["command_hook_context"]["maximum"]), (0, hooks.CONTEXT_MOST))
        self.assertIs(props["command_hook_quote"]["default"], True)
        self.assertEqual([s["name"] for s in manifest["secrets"]], [hooks.SECRET])
        self.assertIn("config:" + hooks.URL, manifest["permissions"]["network"])
        self.assertIn(hooks.SANDBOX_HOST, manifest["permissions"]["network"])
        pattern = props["command_hooks"]["items"]["pattern"]
        self.assertEqual(pattern, "^" + hooks.COMMAND.pattern + "$")


if __name__ == "__main__":
    unittest.main()
