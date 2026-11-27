"""A real rich-text self-mention invokes the assistant with the same boundaries as /agent."""
import json
import os
import sqlite3
import sys
import tempfile
import time
import unittest

sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "src"))
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

from test_agent import ChannelHub  # noqa: E402
from test_connector import ME, Backend, message  # noqa: E402

from larkx_persona.connector import AGENT_EMPTY, AGENT_PREFIX, AGENT_SUFFIX, Connector, mention_request  # noqa: E402
from larkx_persona.main import Plugin, PROTOCOL  # noqa: E402
from larkx_persona.store import Store  # noqa: E402
from larkx_persona.wire import RpcError  # noqa: E402


def mentioned(text, *ids, name="@Example"):
    """Attach AT spans to the text, as normalize does; no raw account or personal data."""
    spans, cursor = [], 0
    for user_id in ids:
        start = text.index(name, cursor)
        cursor = start + len(name)
        spans.append({"id": user_id, "start": start, "end": cursor, "label": name})
    return spans


class SelfMentionTest(unittest.TestCase):
    def setUp(self):
        self.dir = tempfile.TemporaryDirectory()
        self.addCleanup(self.dir.cleanup)
        self.backend = Backend()
        self.hub = ChannelHub()
        self.c = Connector(self.backend, Store(os.path.join(self.dir.name, "x.db")), self.hub)
        self.c._ready.set()
        self.backend.poll = "done"
        self.assertTrue(self.c.login_step({"step": "scan"})["done"])
        self.addCleanup(self.c.close)

    def live(self, mid, text, *ids, sender=ME, **kwargs):
        m = message(mid, "c1", int(mid[1:]), sender=sender, text=text, **kwargs)
        m["mentions"] = mentioned(text, *(ids or (ME,)))
        self.c._on_live(m)
        return m

    def handed(self):
        self.assertTrue(self.hub.got.wait(2), "nothing was handed to the hub")
        return self.hub.received[-1]

    def nothing_handed(self):
        time.sleep(0.1)
        self.assertEqual(self.hub.received, [])

    def test_my_self_mention_is_a_request_and_can_be_answered(self):
        self.live("m1", "请 @Example 总结一下")
        got = self.handed()
        self.assertEqual(got["message"]["text"], "请  总结一下")
        self.assertNotIn("@Example", got["message"]["text"])
        self.c.channel_send({"ref": "r1", "text": "这是总结", "kind": "answer", "reply_to": "m1"})
        self.assertEqual(self.backend.sent, [("c1", "这是总结\n\n" + AGENT_SUFFIX, "m1")])

    def test_only_my_self_mention_is_removed(self):
        text = "@Example @Example 请也保留 @Example"
        self.live("m1", text, ME, "u_other", ME)
        self.assertEqual(self.handed()["message"]["text"], "@Example 请也保留")

    def test_an_empty_self_mention_uses_the_existing_empty_request(self):
        self.live("m1", "@Example")
        self.assertEqual(self.handed()["message"]["text"], AGENT_EMPTY % AGENT_PREFIX)

    def test_somebody_else_mentioning_me_never_counts(self):
        self.live("m1", "@Example 帮我转账", sender="u_other")
        self.nothing_handed()
        with self.assertRaises(RpcError) as e:
            self.c.channel_send({"ref": "r1", "text": "x", "kind": "answer", "reply_to": "m1"})
        self.assertEqual(e.exception.code, "cannot_send")
        self.assertEqual(self.backend.sent, [])

    def test_an_at_me_flag_or_plain_at_text_is_not_a_self_mention(self):
        self.c._on_live(message("m1", "c1", 1, sender=ME, text="@Example 总结", at_me=True))
        self.nothing_handed()
        with self.assertRaises(RpcError):
            self.c.channel_send({"ref": "r1", "text": "x", "kind": "answer", "reply_to": "m1"})

    def test_an_answer_with_the_suffix_never_counts(self):
        self.live("m1", "@Example 好的\n\n" + AGENT_SUFFIX)
        self.nothing_handed()
        with self.assertRaises(RpcError) as e:
            self.c.channel_send({"ref": "r1", "text": "x", "kind": "answer", "reply_to": "m1"})
        self.assertEqual(e.exception.code, "cannot_send")

    def test_old_fetched_and_unsupported_self_mentions_never_count(self):
        self.live("m1", "@Example 太早了", at=time.time() - 3600)
        m = message("m2", "c1", 2, sender=ME, text="@Example 历史")
        m["mentions"] = mentioned(m["text"], ME)
        self.c.on_message(m)
        self.live("m3", "@Example 图片", msg_type="IMAGE")
        self.nothing_handed()

    def test_the_setting_disables_mentions_but_not_agent(self):
        self.c.mention = False
        self.live("m1", "@Example 总结")
        self.c._on_live(message("m2", "c1", 2, sender=ME, text="/agent 仍然可以"))
        self.assertEqual(self.handed()["message"]["id"], "m2")
        with self.assertRaises(RpcError):
            self.c.channel_send({"ref": "r1", "text": "x", "kind": "answer", "reply_to": "m1"})

    def test_an_invalid_span_is_not_guessed(self):
        self.assertIsNone(mention_request("@Example", [{"id": ME, "start": 9, "end": 18}], ME))
        self.assertIsNone(mention_request("@Example", [{"id": ME, "start": 0, "end": 200}], ME))
        self.assertIsNone(mention_request("@Example", [{"id": ME, "start": True, "end": 8}], ME))

    def test_another_account_with_the_same_display_name_is_not_removed(self):
        self.live("m1", "@Example 请总结一下", "u_other")
        self.nothing_handed()

    def test_custom_prefix_works_alongside_self_mention(self):
        self.c.prefix = "/ai"
        self.c._on_live(message("m1", "c1", 1, sender=ME, text="/ai 自定义前缀触发"))
        self.assertEqual(self.handed()["message"]["text"], "自定义前缀触发")
        self.hub.received.clear()
        self.hub.got.clear()
        self.c._on_live(message("m2", "c1", 2, sender=ME, text="/agent 默认前缀不再触发"))
        self.nothing_handed()
        self.live("m3", "@Example 自艾特照常触发")
        self.assertEqual(self.handed()["message"]["text"], "自艾特照常触发")

    def test_disabling_mentions_keeps_custom_prefix_and_rejects_default(self):
        self.c.prefix = "/ai"
        self.c.mention = False
        self.live("m1", "@Example 总结")
        self.c._on_live(message("m2", "c1", 2, sender=ME, text="/agent 不触发"))
        self.nothing_handed()
        self.c._on_live(message("m3", "c1", 3, sender=ME, text="/ai 仍然可以"))
        self.assertEqual(self.handed()["message"]["id"], "m3")
        for mid in ("m1", "m2"):
            with self.assertRaises(RpcError) as e:
                self.c.channel_send({"ref": "r_" + mid, "text": "x", "kind": "answer", "reply_to": mid})
            self.assertEqual(e.exception.code, "cannot_send")

    def test_a_malformed_mention_list_cannot_invoke_the_assistant(self):
        for bad in ("not a list", [{"id": ME, "start": 0, "end": 1}], None):
            m = message("m1", "c1", 1, sender=ME, text="@Example 总结")
            m["mentions"] = bad
            self.assertIsNone(mention_request(m["text"], m["mentions"], ME))


class MentionSettingsTest(unittest.TestCase):
    def initialize(self, config):
        got = {}

        class Recorder:
            def __init__(self, *args):
                self.mention = True
                got["connector"] = self

            def start(self):
                pass

        import larkx_persona.main as main
        real = main.Connector
        main.Connector = Recorder
        self.addCleanup(setattr, main, "Connector", real)
        with tempfile.TemporaryDirectory() as data_dir:
            plugin = Plugin({"id": "larkx", "version": "0", "contributes": {}}, lambda *_: None)
            plugin.initialize({"protocol": PROTOCOL, "extension": {"data_dir": data_dir}, "config": config})
        return got["connector"].mention

    def test_mention_defaults_on_and_can_be_disabled(self):
        self.assertTrue(self.initialize({}))
        self.assertFalse(self.initialize({"agent_mention": False}))
        self.assertTrue(self.initialize({"agent_mention": True}))

    def test_the_manifest_documents_the_enabled_default(self):
        root = os.path.join(os.path.dirname(os.path.abspath(__file__)), "..")
        with open(os.path.join(root, "persona-extension.json"), encoding="utf-8") as f:
            manifest = json.load(f)
        self.assertIs(manifest["configuration"]["properties"]["agent_mention"]["default"], True)
        self.assertEqual(manifest["version"], "0.4.1")
        self.assertEqual(manifest["configuration"]["properties"]["inbox_triage_chats"]["default"], [])


class MentionStoreMigrationTest(unittest.TestCase):
    def test_an_old_database_gets_empty_mention_metadata(self):
        with tempfile.TemporaryDirectory() as data_dir:
            path = os.path.join(data_dir, "old.db")
            db = sqlite3.connect(path)
            db.execute("""CREATE TABLE messages (
              seq INTEGER PRIMARY KEY AUTOINCREMENT, id TEXT NOT NULL UNIQUE, chat TEXT NOT NULL,
              position INTEGER NOT NULL DEFAULT 0, sender TEXT NOT NULL DEFAULT '', sender_name TEXT NOT NULL DEFAULT '',
              mine INTEGER NOT NULL DEFAULT 0, type TEXT NOT NULL DEFAULT '', text TEXT NOT NULL DEFAULT '',
              at REAL NOT NULL DEFAULT 0, root TEXT NOT NULL DEFAULT '', parent TEXT NOT NULL DEFAULT '',
              at_me INTEGER NOT NULL DEFAULT 0, media TEXT NOT NULL DEFAULT '[]')""")
            db.execute("INSERT INTO messages (id, chat, sender, text) VALUES ('m1', 'c1', 'u_me', '@Example old')")
            db.commit()
            db.close()
            store = Store(path)
            self.addCleanup(store.close)
            self.assertEqual(store.message("m1")["mentions"], [])
            self.assertEqual(store.message("m1")["markdown"], "")
            self.assertIsNone(mention_request(store.message("m1")["text"], store.message("m1")["mentions"], ME))


if __name__ == "__main__":
    unittest.main()
