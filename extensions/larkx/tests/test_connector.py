"""The connector over a backend that records what it is asked: what the protocol test cannot reach (actions.do and inbox.reply
are never called there, and its fixture has no way to fail).
Run them: python3 -m unittest discover -s tests, in extensions/larkx."""
import base64
import json
import os
import re
import sqlite3
import sys
import tempfile
import threading
import time
import unittest
from unittest import mock

sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "src"))

from larkx_persona.connector import DM_UNKNOWN, KEEP_DAYS, Connector, _channel_routing, mention_request  # noqa: E402
from larkx_persona.real import normalize  # noqa: E402
from larkx_persona.store import Store  # noqa: E402
from larkx_persona.wire import RpcError  # noqa: E402

ME = "u_me"


class Expired(Exception):
    pass


class Backend:
    Expired = Expired

    def __init__(self):
        self.sent = []
        self.send_result = "m_new"
        self.send_error = None
        self.chats = {}
        self.members = {}
        self.chat_members_calls = []
        self.chat_info_calls = []
        self.older = {}
        self.restored = []
        self.restore_error = None
        self.poll = "waiting"
        self.finish_error = None
        self.saver = None

    def prepare(self):
        pass

    def on_credentials(self, save):
        self.saver = save

    def restore(self, saved):
        self.restored.append(saved)
        if self.restore_error:
            error, self.restore_error = self.restore_error, None
            raise error
        return {"id": ME, "name": "我的账号"}

    def login_start(self):
        return "qr-new"

    def login_qr(self):
        return "qr-same"

    def login_poll(self):
        return self.poll

    def login_finish(self):
        if self.finish_error:
            raise self.finish_error
        return {"id": ME, "name": "我的账号"}, {"cookies": {"session": "s"}}

    def logout(self):
        pass

    def listen(self, on_message, on_link, stop):
        on_link("up")
        stop.wait()

    def send(self, chat, text, root, mentions=()):
        if self.send_error:
            raise self.send_error
        self.sent.append((chat, text, root) if not mentions else (chat, text, root, mentions))
        return self.send_result

    def create_chat(self, user):
        return "c_" + user

    def own_chat(self):
        return "c_me"

    def search(self, query):
        return []

    def history(self, chat, positions):
        return [self.older[p] for p in positions if p in self.older]

    def chat_info(self, chat):
        self.chat_info_calls.append(chat)
        return self.chats.get(chat, {"name": "", "last_position": 0})

    def user_name(self, user, chat):
        return {"u_a": "小明", "7001": "小明"}.get(user)

    def chat_members(self, chat, limit=100):
        self.chat_members_calls.append((chat, limit))
        return list(self.members.get(chat, []))[:limit]

    def download(self, msg_id, chat, key, most):
        return "image/png", b"png"


class Hub:
    def __init__(self):
        self.saved = None
        self.deleted = False
        self.events = []

    def call(self, method, params):
        if method.endswith("load"):
            return {"data": self.saved} if self.saved else {}
        if method.endswith("save"):
            self.saved = params["data"]
        if method.endswith("delete"):
            self.deleted = True
            self.saved = None
        return {}

    def emit(self, event, params):
        self.events.append((event, params))

    def log(self, line):
        pass


def message(msg_id, chat, position, sender="u_a", text="你好", kind="dm", at_me=False, msg_type="TEXT", at=None, root="", parent="", media=None):
    return {"id": msg_id, "chat": chat, "chat_kind": kind, "position": position, "sender": sender, "type": msg_type, "text": text,
            "at": at or time.time() - 60 + position, "root": root, "parent": parent, "at_me": at_me, "media": media or []}


class ConnectorTest(unittest.TestCase):
    def setUp(self):
        self.dir = tempfile.TemporaryDirectory()
        self.addCleanup(self.dir.cleanup)
        self.backend = Backend()
        self.hub = Hub()
        self.c = Connector(self.backend, Store(os.path.join(self.dir.name, "x.db")), self.hub)
        self.addCleanup(self.c.close)

    def sign_in(self):
        self.c._ready.set()
        self.backend.poll = "done"
        self.assertTrue(self.c.login_step({"step": "scan"})["done"])
        self.addCleanup(lambda: None)

    def thing(self, thread):
        return next(t for t in self.c.inbox_collect({})["things"] if t["thread"] == thread)

    def test_a_dm_waits_until_the_user_answers(self):
        self.sign_in()
        self.c.on_message(message("m1", "c1", 1))
        self.backend.chat_info_calls.clear()
        thing = self.thing("c1")
        self.assertEqual(thing["kind"], "dm")
        self.assertEqual(thing["title"], "小明")
        self.assertNotIn("tier", thing)
        self.assertNotIn("done", thing)
        self.assertEqual(self.backend.chat_info_calls, [])
        self.c.on_message(message("m2", "c1", 2, sender=ME, text="好"))
        self.assertEqual(self.thing("c1")["done"], "replied")

    def test_a_group_message_is_folded_and_a_mention_is_not(self):
        self.sign_in()
        self.backend.chats["g1"] = {"name": "项目群", "last_position": 0}
        self.c.on_message(message("m1", "g1", 1, kind="group"))
        thing = self.thing("g1")
        self.assertEqual((thing["kind"], thing["tier"]), ("group", "noise"))
        self.assertEqual(self.c.store.chat("g1")["mute"], "unknown")
        self.c.on_message(message("m2", "g1", 2, kind="group", at_me=True))
        thing = self.thing("g1")
        self.assertEqual(thing["kind"], "mention")
        self.assertNotIn("tier", thing)

    def test_a_muted_group_without_a_mention_is_not_collected_but_is_archived(self):
        self.sign_in()
        self.backend.chats["g1"] = {"name": "安静群", "last_position": 0, "mute": "muted"}
        self.c.on_message(message("m1", "g1", 1, kind="group"))
        self.backend.chat_info_calls.clear()
        self.assertEqual(self.c.inbox_collect({})["things"], [])
        row = self.c.store.chat("g1")
        self.assertEqual(row["mute"], "muted")
        self.assertGreater(row["checked_at"], 0)
        self.assertEqual(self.backend.chat_info_calls, ["g1"])
        self.assertEqual(self.c.inbox_collect({})["things"], [])
        self.assertEqual(self.backend.chat_info_calls, ["g1"])
        batches = self.c.archive_changes({"cursor": "", "limit": 20})["batches"]
        self.assertEqual(len(batches), 1)
        self.assertTrue(batches[0]["chat"]["muted"])
        self.assertEqual([m["id"] for m in batches[0]["messages"]], ["m1"])

    def test_archive_chat_muted_is_present_only_when_known(self):
        self.sign_in()
        for i, state in enumerate(("muted", "normal", "unknown")):
            chat = "g%d" % i
            self.backend.chats[chat] = {"name": "群%d" % i, "last_position": 0, "mute": state}
            self.c.on_message(message("m%d" % i, chat, 1, kind="group"))
            self.c.store.mute_chat(chat, state, time.time())
        batches = self.c.archive_changes({"cursor": "", "limit": 20})["batches"]
        by_id = {b["chat"]["id"]: b["chat"] for b in batches}
        self.assertIs(by_id["g0"]["muted"], True)
        self.assertIs(by_id["g1"]["muted"], False)
        self.assertNotIn("muted", by_id["g2"])

    def test_a_mention_in_a_muted_group_is_still_triaged(self):
        self.sign_in()
        self.backend.chats["g1"] = {"name": "安静群", "last_position": 0, "mute": "muted"}
        self.c.on_message(message("m1", "g1", 1, kind="group", at_me=True))
        thing = self.thing("g1")
        self.assertEqual(thing["kind"], "mention")
        self.assertNotIn("tier", thing)
        self.assertNotIn("done", thing)

    def test_my_last_reply_still_settles_a_muted_group(self):
        self.sign_in()
        self.backend.chats["g1"] = {"name": "安静群", "last_position": 0, "mute": "muted"}
        self.c.on_message(message("m1", "g1", 1, kind="group"))
        self.c.on_message(message("m2", "g1", 2, kind="group", sender=ME))
        thing = self.thing("g1")
        self.assertEqual(thing["done"], "replied")
        self.assertEqual(thing["extra"]["rule_tier"], "fyi")

    def test_a_normal_group_uses_model_triage_with_the_users_rules(self):
        self.sign_in()
        self.c.rules = "项目动态通常归为 fyi"
        self.backend.chats["g1"] = {"name": "项目群", "last_position": 0, "mute": "normal"}
        self.c.on_message(message("m1", "g1", 1, kind="group"))
        thing = self.thing("g1")
        self.assertEqual(thing["kind"], "group")
        self.assertNotIn("tier", thing)
        self.assertEqual(thing["extra"]["rule_tier"], "noise")
        self.assertEqual(thing["rules"], self.c.rules)

    def test_opted_in_product_question_and_followup_reach_model_triage(self):
        self.sign_in()
        self.c.inbox_triage_chats = {"g1"}
        self.c.rules = "Questions about the reports I own need my attention."
        self.backend.chats["g1"] = {"name": "Reports", "mute": "unknown"}
        question = message("q1", "g1", 1, kind="group", text="Does the report support nightly builds?")
        question["mentions"] = [{"label": "@HelpBot"}]
        self.c.on_message(question)
        first = self.thing("g1")
        self.assertNotIn("tier", first)
        self.assertEqual(first["extra"]["rule_tier"], "noise")
        self.assertEqual(first["rules"], self.c.rules)
        self.assertIn("@HelpBot", first["context"])

        self.c.store.mute_chat("g1", "muted", time.time())
        self.c.on_message(message("a1", "g1", 2, kind="group", text="Anyone bringing lunch?", root="lunch"))
        self.c.on_message(message("q2", "g1", 3, kind="group", text="Could you add support?", root="q1"))
        followup = self.thing("g1:q1")
        self.assertNotIn("tier", followup)
        self.assertEqual(followup["extra"]["rule_tier"], "noise")
        self.assertEqual(followup["last"], "3")
        self.assertIn("Does the report support nightly builds?", followup["context"])
        self.assertIn("最新消息：", followup["context"])
        self.assertNotIn("lunch", followup["context"])
        self.assertIn("Could you add support?", followup["preview"])
        self.assertNotIn("g1", {t["thread"] for t in self.c.inbox_collect({})["things"]})

        unrelated = self.thing("g1:lunch")
        self.assertNotIn("tier", unrelated)
        self.assertEqual(unrelated["extra"]["rule_tier"], "noise")
        self.assertNotIn("the report", unrelated["context"])
        self.c.on_message(message("u1", "g2", 1, kind="group", text="Anyone bringing lunch?"))
        self.assertEqual(self.thing("g2")["tier"], "noise")

    def test_topic_mention_context_ends_with_the_preview_body(self):
        self.sign_in()
        text = "Please check the coverage report for tomorrow."
        latest = message("q1", "g1", 1, kind="group", root="q1", at_me=True, text=text)
        latest["mentions"] = [{"label": "@Owner"}]
        self.c.on_message(latest)

        thing = self.thing("g1:q1")
        self.assertEqual(thing["kind"], "mention")
        self.assertEqual(thing["preview"], text)
        self.assertIn("提及：@Owner", thing["context"])
        # The hub requires the preview at the end of the context to rule out a clipped preview.
        self.assertTrue(thing["context"].strip().endswith(thing["preview"].strip()))

    def test_only_same_topic_reply_settles_even_after_interleaved_activity(self):
        self.sign_in()
        self.c.on_message(message("q1", "g1", 1, kind="group", at_me=True, root="q1"))
        self.c.on_message(message("q2", "g1", 2, kind="group", at_me=True, root="q2"))
        self.c.on_message(message("mine", "g1", 3, kind="group", sender=ME, root="q2"))
        for position in range(4, 14):
            self.c.on_message(message("other%d" % position, "g1", position, kind="group"))
        self.assertEqual(self.thing("g1:q1")["kind"], "mention")
        self.assertNotIn("done", self.thing("g1:q1"))
        settled = self.thing("g1:q2")
        self.assertEqual(settled["done"], "replied")
        self.assertNotIn("last", settled)
        self.assertNotIn("done", self.thing("g1"))
        self.c.on_message(message("answer", "g1", 14, kind="group", sender=ME, root="q1"))
        self.assertEqual(self.thing("g1:q1")["done"], "replied")

    def test_inbox_topic_reply_and_legacy_group_reply_keep_their_routes(self):
        self.sign_in()
        self.c.on_message(message("q1", "g1", 1, kind="group", root="q1"))
        self.c.on_message(message("q2", "g1", 2, kind="group", root="q2"))
        self.c.on_message(message("q3", "g1", 3, kind="group", root="q1"))
        thread = self.thing("g1:q1")["thread"]
        page = self.c.thread_page({"thread": thread, "limit": 1})
        self.assertEqual([m["id"] for m in page["messages"]], ["q3"])
        older = self.c.thread_page({"thread": thread, "before": page["before"]})
        self.assertEqual([m["id"] for m in older["messages"]], ["q1"])
        self.assertEqual(self.c.thread_page({"thread": "g1"})["chat"]["reply"], "larkx_send")
        for _ in range(2):
            self.c.inbox_reply({"thread": thread, "text": "Topic answer", "client_id": "one"})
        self.c.inbox_reply({"thread": "g1", "text": "Group answer", "client_id": "two"})
        self.assertEqual(self.backend.sent, [("g1", "Topic answer", "q1"), ("g1", "Group answer", None)])

    def test_topic_group_root_opens_a_topic_before_a_reply_exists(self):
        self.sign_in()
        normalized = normalize({"msg_id": "q1", "chat_id": "g1", "chat_type": 2, "scope": "topic",
                                "msg_type_name": "TEXT", "position": 1, "content": "Can you help?",
                                "at_me": True}, lambda *_: "")
        self.c.on_message(normalized)
        self.assertEqual(self.thing("g1:q1")["last"], "1")
        self.c.on_message(message("q2", "g1", 2, kind="group", root="q1"))
        self.assertEqual(self.thing("g1:q1")["last"], "2")

    def test_a_stale_mute_state_is_refreshed_and_a_failure_falls_back_to_unknown(self):
        self.sign_in()
        self.backend.chats["g1"] = {"name": "项目群", "last_position": 0, "mute": "normal"}
        self.c.on_message(message("m1", "g1", 1, kind="group"))
        self.assertNotIn("tier", self.thing("g1"))
        self.c.store.mute_chat("g1", "normal", time.time() - 3700)
        self.backend.chats["g1"]["mute"] = "muted"
        self.assertEqual(self.c.inbox_collect({})["things"], [])
        self.assertEqual(self.c.store.chat("g1")["mute"], "muted")
        checked_at = time.time() - 3700
        self.c.store.mute_chat("g1", "muted", checked_at)
        with mock.patch.object(self.backend, "chat_info", side_effect=RuntimeError("offline")):
            things = self.c.inbox_collect({})["things"]
        self.assertEqual(len(things), 1)
        self.assertEqual(things[0]["tier"], "noise")
        row = self.c.store.chat("g1")
        self.assertEqual(row["mute"], "unknown")
        self.assertGreater(row["checked_at"], checked_at)

    def test_mute_refresh_is_bounded_and_later_polls_cover_the_rest(self):
        self.sign_in()
        for i in range(4):
            chat = "g%d" % i
            self.backend.chats[chat] = {"name": "群%d" % i, "last_position": 0}
            self.c.on_message(message("m%d" % i, chat, 1, kind="group"))
        self.backend.chat_info_calls.clear()
        with mock.patch("larkx_persona.connector.MUTE_REFRESH_MOST", 2):
            things = self.c.inbox_collect({})["things"]
            self.assertEqual(len(self.backend.chat_info_calls), 2)
            self.assertEqual(len(things), 4)
            self.assertTrue(all(t["tier"] == "noise" for t in things))
            self.c.inbox_collect({})
            self.assertEqual(len(self.backend.chat_info_calls), 4)
        self.assertTrue(all(self.c.store.chat("g%d" % i)["mute"] == "unknown" for i in range(4)))

    def test_an_old_chat_database_gets_unknown_mute_state(self):
        path = os.path.join(self.dir.name, "before-mute.db")
        db = sqlite3.connect(path)
        db.execute("CREATE TABLE chats (id TEXT PRIMARY KEY, name TEXT NOT NULL DEFAULT '', kind TEXT NOT NULL DEFAULT 'group', last_at REAL NOT NULL DEFAULT 0)")
        db.execute("INSERT INTO chats (id, name, kind, last_at) VALUES ('g1', '旧群', 'group', 1)")
        db.commit()
        db.close()
        store = Store(path)
        self.addCleanup(store.close)
        row = store.chat("g1")
        self.assertEqual((row["mute"], row["checked_at"]), ("unknown", 0))
        store.mute_chat("g1", "normal", 123)
        self.assertEqual((store.chat("g1")["mute"], store.chat("g1")["checked_at"]), ("normal", 123))

    def test_every_item_carries_a_fallback_tier_and_the_users_rules(self):
        self.sign_in()
        self.backend.chats["g1"] = {"name": "项目群", "last_position": 0}
        self.c.on_message(message("m1", "c1", 1))
        self.c.on_message(message("m2", "g1", 1, kind="group"))
        self.assertEqual(self.thing("c1")["extra"]["rule_tier"], "action")
        self.assertEqual(self.thing("g1")["extra"]["rule_tier"], "noise")
        self.assertNotIn("rules", self.thing("c1"))
        self.c.on_message(message("m3", "c1", 2, sender=ME, text="好"))
        self.assertEqual(self.thing("c1")["extra"]["rule_tier"], "fyi")
        rules = Connector(self.backend, self.c.store, self.hub, "  领导的私聊都要处理 " + "x" * 5000)
        self.assertEqual(rules.inbox_collect({})["things"][0]["rules"][:9], "领导的私聊都要处理")
        self.assertEqual(len(rules.inbox_collect({})["things"][0]["rules"]), 4000)

    def test_a_mention_the_user_answered_does_not_count_again(self):
        self.sign_in()
        self.c.on_message(message("m1", "g1", 1, kind="group", at_me=True))
        self.c.on_message(message("m2", "g1", 2, kind="group", sender=ME))
        self.c.on_message(message("m3", "g1", 3, kind="group"))
        self.assertEqual(self.thing("g1")["kind"], "group")

    def test_old_chats_leave_the_inbox(self):
        self.sign_in()
        self.c.on_message(message("m1", "c1", 1, at=time.time() - 30 * 86400))
        self.assertEqual(self.c.inbox_collect({})["things"], [])

    def test_a_message_is_kept_once(self):
        self.sign_in()
        self.c.on_message(message("m1", "c1", 1))
        self.c.on_message(message("m1", "c1", 1))
        self.assertEqual(len(self.c.store.messages("c1", 0, 10)), 1)

    def test_an_action_runs_once_per_request(self):
        self.sign_in()
        args = {"chat": "c1", "text": "三点见"}
        first = self.c.actions_do({"name": "larkx_send", "args": args, "request_id": "r1"})
        again = self.c.actions_do({"name": "larkx_send", "args": args, "request_id": "r1"})
        self.assertEqual(first, again)
        self.assertEqual(self.backend.sent, [("c1", "三点见", None)])

    def test_explicit_mentions_reach_the_backend_but_a_plain_at_name_does_not(self):
        self.sign_in()
        args = {"chat": "c1", "text": "@小明 周五见", "mentions": [{"id": "7001", "name": "小明"}]}
        self.c.actions_do({"name": "larkx_send", "args": args, "request_id": "r1"})
        self.assertEqual(self.backend.sent, [("c1", "@小明 周五见", None, args["mentions"])])
        self.c.actions_do({"name": "larkx_send", "args": {"chat": "c1", "text": "@小明 明天见"}, "request_id": "r2"})
        self.assertEqual(self.backend.sent[-1], ("c1", "@小明 明天见", None))

    def test_an_unknown_or_unwritten_mention_is_refused_before_sending(self):
        self.sign_in()
        for mentions in ([{"id": "ou_wrong", "name": "小明"}], [{"id": "7001", "name": "小亮"}]):
            args = {"chat": "c1", "text": "@小明 周五见", "mentions": mentions}
            with self.assertRaises(RpcError):
                self.c.actions_do({"name": "larkx_send", "args": args, "request_id": "r1"})
        self.assertEqual(self.backend.sent, [])

    def test_an_agent_reply_mentions_only_a_known_participant(self):
        self.sign_in()
        self.c.on_message(message("m0", "g1", 1, sender="7001", text="小明", kind="group"))
        m = message("m1", "g1", 2, kind="group")
        m["sender_name"] = "小明"
        m["sender"] = ME
        self.c.on_message(m)
        with mock.patch.object(self.c, "_agent_request", return_value="提醒小明"):
            self.c.channel_send({"ref": "r1", "reply_to": "m1", "text": "@小明 请确认"})
        self.assertEqual(self.backend.sent[0][3], [{"id": "7001", "name": "小明"}])

    def test_larkx_members_lists_group_bots_and_humans_and_caches_names(self):
        self.sign_in()
        self.backend.members["g1"] = [
            {"id": "7001", "name": "小明", "real_name": "小明", "kind": "user"},
            {"id": "7002", "name": "Example Bot", "real_name": "Example Bot", "kind": "bot"},
        ]
        out = self.c.tools_call({"name": "larkx_members", "args": {"chat": "g1"}})
        self.assertIn("7001  小明〔成员〕", out["text"])
        self.assertIn("7002  Example Bot〔BOT〕", out["text"])
        self.assertEqual(self.c.store.name("7002"), "Example Bot")
        # Cached within ttl: second call does not hit backend again.
        self.c.tools_call({"name": "larkx_members", "args": {"chat": "g1"}})
        self.assertEqual(len(self.backend.chat_members_calls), 1)

    def test_an_agent_reply_mentions_silent_group_bots_and_skips_ambiguous_names(self):
        self.sign_in()
        self.backend.members["g1"] = [
            {"id": "7002", "name": "Bot Alpha", "real_name": "Bot Alpha", "kind": "bot"},
            {"id": "7003", "name": "Bot Nick", "real_name": "Bot Beta", "kind": "bot"},
            {"id": "7004", "name": "DupName", "real_name": "DupName", "kind": "user"},
            {"id": "7005", "name": "DupName", "real_name": "DupName", "kind": "user"},
        ]
        m = message("m1", "g1", 1, sender=ME, text="/agent 打招呼", kind="group")
        self.c.on_message(m)
        self.c.channel_send({"ref": "r1", "reply_to": "m1", "text": "@Bot Alpha @Bot Beta @DupName 你好"})
        got = {x["id"]: x["name"] for x in self.backend.sent[0][3]}
        self.assertEqual(got, {"7002": "Bot Alpha", "7003": "Bot Beta"})

    def test_an_agent_reply_resolves_bot_ids_from_historical_mentions(self):
        self.sign_in()
        prev = message("m0", "g1", 1, sender="7001", text="@HistoryBot 查一下", kind="group")
        prev["mentions"] = [{"id": "7099", "start": 0, "end": 11, "label": "@HistoryBot"}]
        self.c.on_message(prev)
        req = message("m1", "g1", 2, sender=ME, text="/agent 问一下机器人", kind="group")
        self.c.on_message(req)
        self.c.channel_send({"ref": "r1", "reply_to": "m1", "text": "@HistoryBot 你好"})
        self.assertEqual(self.backend.sent[0][3], [{"id": "7099", "name": "HistoryBot"}])

    def test_members_cache_refetches_for_larger_limit_and_reuses_complete_roster(self):
        self.sign_in()
        # 80-member group: _agent_context (limit=60) must not truncate later limit=200 callers.
        self.backend.members["g1"] = [
            {"id": str(8000 + i), "name": "成员%d" % i, "real_name": "成员%d" % i, "kind": "user"}
            for i in range(80)
        ]
        m = message("m1", "g1", 1, sender=ME, text="/agent 打招呼", kind="group")
        self.c.on_message(m)
        self.c._agent_context(m)
        self.assertEqual(self.backend.chat_members_calls, [("g1", 60)])

        self.c.channel_send({"ref": "r1", "reply_to": "m1", "text": "@成员70 你好"})
        self.assertEqual(self.backend.chat_members_calls, [("g1", 60), ("g1", 200)])
        self.assertEqual(self.backend.sent[0][3], [{"id": "8070", "name": "成员70"}])

        # Subsequent larkx_members(limit=200) and smaller limit=50 both hit the complete 200-limit cache.
        out = self.c.tools_call({"name": "larkx_members", "args": {"chat": "g1", "limit": 200}})
        self.assertEqual(len(out["text"].splitlines()), 80)
        self.assertEqual(len(self.backend.chat_members_calls), 2)
        small = self.c._chat_members_cached("g1", limit=50)
        self.assertEqual(len(small), 50)
        self.assertEqual(len(self.backend.chat_members_calls), 2)

        # Small group (<60): cached 60-limit fetch is already complete, so limit=200 hits without refetching.
        self.backend.members["g2"] = [
            {"id": str(9000 + i), "name": "小组员%d" % i, "real_name": "小组员%d" % i, "kind": "user"}
            for i in range(10)
        ]
        self.assertEqual(len(self.c._chat_members_cached("g2", limit=60)), 10)
        self.assertEqual(len(self.c._chat_members_cached("g2", limit=200)), 10)
        self.assertEqual([c for c in self.backend.chat_members_calls if c[0] == "g2"], [("g2", 60)])

    def test_context_mentions_require_boundary_and_prevent_prefix_collisions(self):
        self.sign_in()
        self.backend.members["g1"] = [
            {"id": "8007", "name": "成员7", "real_name": "成员7", "kind": "user"},
            {"id": "8001", "name": "张三", "real_name": "张三", "kind": "user"},
            {"id": "8010", "name": "Bot", "real_name": "Bot", "kind": "bot"},
            {"id": "8011", "name": "Bot Pro", "real_name": "Bot Pro", "kind": "bot"},
            {"id": "8012", "name": "Bot Pro", "real_name": "Bot Pro", "kind": "bot"},
        ]
        m = message("m1", "g1", 1, sender=ME, text="/agent 通知", kind="group")
        self.c.on_message(m)

        # Unknown longer @names must NOT fall back to shorter prefix candidates.
        self.c.channel_send({"ref": "r1", "reply_to": "m1", "text": "@成员70 @张三丰 你好"})
        self.assertEqual(len(self.backend.sent[0]), 3)  # no mentions attached

        # Ambiguous longer candidate (@Bot Pro) must NOT be hijacked by shorter @Bot.
        self.c.channel_send({"ref": "r2", "reply_to": "m1", "text": "@Bot Pro 你好"})
        self.assertEqual(len(self.backend.sent[1]), 3)

        # Boundary-terminated @name alongside a longer unknown @name resolves only the real target,
        # and mention_parts leaves the longer @word untouched as plain text.
        self.c.channel_send({"ref": "r3", "reply_to": "m1", "text": "@成员70 和 @成员7，@张三：请看"})
        got = {x["id"]: x["name"] for x in self.backend.sent[2][3]}
        self.assertEqual(got, {"8007": "成员7", "8001": "张三"})
        from larkx_persona.real import mention_parts
        parts = mention_parts("@成员70 和 @成员7，@张三：请看", self.backend.sent[2][3])
        self.assertEqual(parts[0], ("text", "@成员70 和 "))
        self.assertEqual(parts[1], ("at", {"id": "8007", "name": "成员7"}))
        self.assertEqual(parts[3], ("at", {"id": "8001", "name": "张三"}))

    def test_group_nicknames_do_not_overwrite_global_canonical_names(self):
        self.sign_in()
        self.c.store.set_name("7001", "张三")
        self.backend.members["g1"] = [
            {"id": "7001", "name": "张总", "real_name": "张三", "kind": "user"},
            {"id": "7002", "name": "仅群昵称", "real_name": "", "kind": "user"},
            {"id": "7003", "name": "群名机器人", "real_name": "RealBot", "kind": "bot"},
        ]
        self.c._chat_members_cached("g1", limit=100)
        # Canonical global name stays intact; nickname-only user does not pollute global store;
        # missing canonical real_name can still be backfilled with real_name (never nickname).
        self.assertEqual(self.c.store.name("7001"), "张三")
        self.assertEqual(self.c.store.name("7002"), "")
        self.assertEqual(self.c.store.name("7003"), "RealBot")

        # Inside g1, both group nickname and real name still resolve to real mentions.
        m = message("m1", "g1", 1, sender=ME, text="/agent 提醒", kind="group")
        self.c.on_message(m)
        self.c.channel_send({"ref": "r1", "reply_to": "m1", "text": "@张总 和 @仅群昵称 请看"})
        got = {x["id"]: x["name"] for x in self.backend.sent[0][3]}
        self.assertEqual(got, {"7001": "张总", "7002": "仅群昵称"})

    def test_empty_members_are_not_cached_and_logout_drops_inflight_roster(self):
        self.sign_in()
        # Transient empty response must not be cached.
        self.backend.members["g_empty"] = []
        self.assertEqual(self.c._chat_members_cached("g_empty", limit=100), [])
        self.backend.members["g_empty"] = [{"id": "7010", "name": "恢复后成员", "real_name": "恢复后成员", "kind": "user"}]
        got = self.c._chat_members_cached("g_empty", limit=100)
        self.assertEqual([x["id"] for x in got], ["7010"])

        # An in-flight chat_members fetch must not repopulate _members_cache after logout.
        orig = self.backend.chat_members

        def slow_members(chat, limit=100):
            self.c.logout({})
            return [{"id": "7099", "name": "旧账号成员", "real_name": "旧账号成员", "kind": "user"}]

        self.backend.chat_members = slow_members
        try:
            self.assertEqual(self.c._chat_members_cached("g_race", limit=100), [])
        finally:
            self.backend.chat_members = orig
        self.assertNotIn("g_race", self.c._members_cache)
        self.assertEqual(self.c.store.name("7099"), "")

    def test_a_send_that_may_have_gone_out_is_never_repeated(self):
        self.sign_in()
        self.backend.send_result = None
        args = {"chat": "c1", "text": "三点见"}
        for _ in range(2):
            with self.assertRaises(RpcError) as e:
                self.c.actions_do({"name": "larkx_send", "args": args, "request_id": "r1"})
            self.assertIn("不要重发", e.exception.message)
        self.assertEqual(len(self.backend.sent), 1)

    def test_a_send_that_raises_says_so_and_is_not_repeated(self):
        self.sign_in()
        self.backend.send_error = OSError("no route")
        with self.assertRaises(RpcError) as e:
            self.c.inbox_reply({"thread": "c1", "text": "好", "client_id": "k1"})
        self.assertIn("不要重发", e.exception.message)
        self.backend.send_error = None
        with self.assertRaises(RpcError):
            self.c.inbox_reply({"thread": "c1", "text": "好", "client_id": "k1"})
        self.assertEqual(self.backend.sent, [])

    def test_a_reply_goes_out_once_per_client_id(self):
        self.sign_in()
        for _ in range(2):
            self.assertEqual(self.c.inbox_reply({"thread": "c1", "text": "好", "client_id": "k1"}), {})
        self.assertEqual(self.backend.sent, [("c1", "好", None)])

    def test_a_reply_to_a_topic_stays_in_it(self):
        self.sign_in()
        self.c.on_message(message("m2", "g1", 2, kind="group", root="m1"))
        self.c.actions_do({"name": "larkx_send", "args": {"chat": "g1", "text": "好", "reply_to": "m2"}, "request_id": "r1"})
        self.assertEqual(self.backend.sent, [("g1", "好", "m1")])

    def test_larkx_messages_includes_ids_and_topic_linkage_while_inbox_context_stays_clean(self):
        self.sign_in()
        self.backend.chats["g1"] = {"name": "项目群", "last_position": 0}
        self.c.on_message(message("m1", "g1", 1, kind="group", text="周五开会吗"))
        self.c.on_message(message("m2", "g1", 2, kind="group", text="三点可以", root="m1", parent="m1"))
        self.c.on_message(message("m3", "g1", 3, kind="group", text="我也行", root="m1", parent="m2"))
        self.c.on_message(message("m4", "g1", 4, kind="group", text="附议", root="", parent="m1"))
        self.c.on_message(message("m5", "g1", 5, kind="group", text="收到", root="0", parent="0"))

        lines = self.c.tools_call({"name": "larkx_messages", "args": {"chat": "g1"}})["text"].splitlines()
        self.assertEqual(len(lines), 5)
        self.assertTrue(lines[0].startswith("[id=m1] 小明（"))
        self.assertNotIn("root=", lines[0])
        self.assertNotIn("parent=", lines[0])
        self.assertTrue(lines[1].startswith("[id=m2 root=m1] 小明（"))
        self.assertNotIn("parent=", lines[1])
        self.assertTrue(lines[2].startswith("[id=m3 root=m1 parent=m2] 小明（"))
        self.assertTrue(lines[3].startswith("[id=m4 parent=m1] 小明（"))
        self.assertNotIn("root=", lines[3])
        self.assertTrue(lines[4].startswith("[id=m5] 小明（"))
        self.assertNotIn("root=", lines[4])
        self.assertNotIn("parent=", lines[4])

        # Inbox context keeps _line(m) clean without raw message or topic ids.
        context = self.thing("g1:m1")["context"]
        self.assertIn("周五开会吗", context)
        self.assertIn("我也行", context)
        self.assertNotIn("[id=", context)
        self.assertNotIn("root=", context)
        self.assertNotIn("parent=", context)

    def test_larkx_messages_id_flows_into_larkx_send_reply_to(self):
        self.sign_in()
        self.backend.chats["g1"] = {"name": "项目群", "last_position": 0}
        self.c.on_message(message("m1", "g1", 1, kind="group", text="周五开会吗"))
        self.c.on_message(message("m2", "g1", 2, kind="group", text="三点可以", root="m1", parent="m1"))

        text = self.c.tools_call({"name": "larkx_messages", "args": {"chat": "g1"}})["text"]
        match = re.search(r"^\[id=([^\s\]]+)[^\]]*\] [^\n]*三点可以$", text, re.MULTILINE)
        self.assertIsNotNone(match)
        target_id = match.group(1)
        self.assertEqual(target_id, "m2")

        card = self.c.actions_describe({"name": "larkx_send", "args": {"chat": "g1", "text": "收到", "reply_to": target_id}})
        self.assertIn("回复：小明 的消息「三点可以」（发在这条消息的话题里）", card["summary"])
        self.assertTrue(card["summary"].endswith("内容（全文）：\n收到"))

        self.c.actions_do({"name": "larkx_send", "args": {"chat": "g1", "text": "收到", "reply_to": target_id}, "request_id": "r_topic"})
        self.assertEqual(self.backend.sent, [("g1", "收到", "m1")])

    def test_tool_schemas_document_larkx_messages_ids_for_reply_to(self):
        tools = {t["name"]: t for t in self.c.tools_list({})["tools"]}
        self.assertIn("id", tools["larkx_messages"]["description"])
        self.assertIn("reply_to", tools["larkx_messages"]["description"])
        actions = {a["name"]: a for a in self.c.actions_list({})["tools"]}
        self.assertIn("larkx_messages", actions["larkx_send"]["input_schema"]["properties"]["reply_to"]["description"])

    def test_a_dm_to_a_person_opens_their_chat(self):
        self.sign_in()
        self.c.actions_do({"name": "larkx_send_dm", "args": {"user": "u_b", "text": "好"}, "request_id": "r1"})
        self.assertEqual(self.backend.sent, [("c_u_b", "好", None)])

    def test_nothing_is_sent_signed_out(self):
        self.c._ready.set()
        with self.assertRaises(RpcError):
            self.c.actions_do({"name": "larkx_send", "args": {"chat": "c1", "text": "好"}, "request_id": "r1"})
        self.assertEqual(self.backend.sent, [])

    def test_the_card_says_who_sees_it_and_the_whole_text(self):
        self.sign_in()
        self.backend.chats["g1"] = {"name": "项目群", "last_position": 0}
        self.c.on_message(message("m1", "g1", 1, kind="group"))
        out = self.c.actions_describe({"name": "larkx_send", "args": {"chat": "g1", "text": "周五 见"}})
        self.assertEqual(out["action"]["visible"], "群『项目群』里的所有人")
        self.assertIn("以你本人的身份", out["summary"])
        self.assertIn("周五 见", out["summary"])

    def test_an_action_without_a_request_id_is_refused(self):
        self.sign_in()
        args = {"chat": "c1", "text": "三点见"}
        for p in ({}, {"request_id": ""}, {"request_id": "  "}, {"request_id": None}):
            with self.assertRaises(RpcError) as e:
                self.c.actions_do(dict(p, name="larkx_send", args=args))
            self.assertIn("request_id", e.exception.message)
            with self.assertRaises(RpcError):
                self.c.actions_do(dict(p, name="larkx_send_dm", args={"user": "u_b", "text": "好"}))
        self.assertEqual(self.backend.sent, [])
        # Two requests are two sends, each once.
        self.c.actions_do({"name": "larkx_send", "args": args, "request_id": "r1"})
        self.c.actions_do({"name": "larkx_send", "args": args, "request_id": "r2"})
        self.assertEqual(len(self.backend.sent), 2)

    def test_the_card_shows_the_message_a_reply_quotes(self):
        self.sign_in()
        self.backend.chats["g1"] = {"name": "项目群", "last_position": 0}
        self.c.on_message(message("m1", "g1", 1, kind="group", text="周五开会吗"))
        out = self.c.actions_describe({"name": "larkx_send", "args": {"chat": "g1", "text": "开", "reply_to": "m1"}})
        self.assertIn("回复：小明 的消息「周五开会吗」（发在这条消息的话题里）", out["summary"])
        self.assertTrue(out["summary"].endswith("内容（全文）：\n开"))
        out = self.c.actions_describe({"name": "larkx_send", "args": {"chat": "g1", "text": "开", "reply_to": "m_gone"}})
        self.assertIn("这里没有它的内容", out["summary"])
        out = self.c.actions_describe({"name": "larkx_send", "args": {"chat": "g1", "text": "开"}})
        self.assertNotIn("回复：", out["summary"])

    def test_a_dm_seen_only_through_my_messages_names_no_recipient(self):
        self.sign_in()
        self.c.on_message(message("m1", "c9", 1, sender=ME, text="备忘"))
        self.assertEqual(self.c.store.chat("c9")["name"], "")
        out = self.c.actions_describe({"name": "larkx_send", "args": {"chat": "c9", "text": "好"}})
        self.assertIn("发给：%s〔私聊〕" % DM_UNKNOWN, out["summary"])
        self.assertEqual(out["action"]["visible"], "这个私聊里的对方（还不知道是谁）")
        self.assertIn(DM_UNKNOWN, self.c.tools_call({"name": "larkx_chats", "args": {}})["text"])
        # The other person's first message names it.
        self.c.on_message(message("m2", "c9", 2, text="收到"))
        out = self.c.actions_describe({"name": "larkx_send", "args": {"chat": "c9", "text": "好"}})
        self.assertIn("发给：小明〔私聊〕", out["summary"])
        self.assertEqual(out["action"]["visible"], "『小明』一个人")

    def test_a_placeholder_name_from_0_1_is_cleared(self):
        path = os.path.join(self.dir.name, "old.db")
        store = Store(path)
        self.addCleanup(store.close)
        store.touch_chat("c1", "dm", 1)
        store.name_chat("c1", "私聊")
        store.touch_chat("g1", "group", 1)
        store.name_chat("g1", "私聊")
        store = Store(path)
        self.assertEqual(store.chat("c1")["name"], "")
        self.assertEqual(store.chat("g1")["name"], "私聊")

    def test_old_messages_go_once_the_archive_has_them(self):
        self.sign_in()
        old = time.time() - (KEEP_DAYS + 1) * 86400
        self.c.on_message(message("m1", "c1", 1, at=old))
        self.c.on_message(message("m2", "c1", 2))
        first = self.c.archive_changes({"cursor": "", "limit": 20})
        self.c.on_message(message("m3", "c2", 1, at=old))
        self.c.archive_changes({"cursor": first["next"], "limit": 20})
        kept = [m["id"] for m in self.c.store.after(0, 10)]
        # m1 is old and archived; m2 is recent; m3 is old but the hub has not confirmed it yet.
        self.assertEqual(kept, ["m2", "m3"])

    def test_a_page_of_a_pruned_chat_comes_from_the_server(self):
        self.sign_in()
        self.c.store.touch_chat("c1", "dm", time.time())
        self.backend.chats["c1"] = {"name": "", "last_position": 3}
        self.backend.older = {p: message("m%d" % p, "c1", p) for p in (1, 2, 3)}
        page = self.c.thread_page({"thread": "c1", "limit": 2})
        self.assertEqual([m["id"] for m in page["messages"]], ["m2", "m3"])
        self.assertEqual(page["before"], "2")

    def test_a_page_reads_older_messages_from_the_server(self):
        self.sign_in()
        self.c.on_message(message("m5", "c1", 5))
        self.backend.older = {p: message("m%d" % p, "c1", p) for p in range(1, 5)}
        page = self.c.thread_page({"thread": "c1", "limit": 3})
        self.assertEqual([m["id"] for m in page["messages"]], ["m3", "m4", "m5"])
        self.assertEqual(page["before"], "3")
        page = self.c.thread_page({"thread": "c1", "limit": 3, "before": "3"})
        self.assertEqual([m["id"] for m in page["messages"]], ["m1", "m2"])
        self.assertNotIn("before", page)

    def test_a_page_shows_a_picture_and_not_its_placeholder(self):
        self.sign_in()
        self.c.on_message(message("m1", "c1", 1, text="[图片]", msg_type="IMAGE", media=[{"kind": "image", "key": "k"}]))
        m = self.c.thread_page({"thread": "c1", "limit": 5})["messages"][0]
        self.assertNotIn("text", m)
        self.assertEqual(self.c.thread_media({"ref": m["media"][0]["ref"]})["data"], base64.b64encode(b"png").decode())

    def test_a_rich_post_keeps_markdown_text_and_inline_media(self):
        self.sign_in()
        rich = NormalizeTest()
        data = rich.tree(("paragraph",), {
            "paragraph": rich.node(3, children=("link", "space", "bold", "break", "image")),
            "link": rich.node(6, fields=((1, "https://example.com/docs"), (2, "文档"))),
            "space": rich.node(1, " "),
            "bold": rich.node(7, "重点"),
            "break": rich.node(1, "\n"),
            "image": rich.node(2, fields=((2, "img_synthetic"),)),
        })
        data["title"] = "周报"
        m = normalize({"msg_id": "m_rich", "chat_id": "c1", "chat_type": 1, "position": 1, "from_id": "u_a",
                       "msg_type": 2, "msg_type_name": "POST", "content": "[富文本] 周报\n正文",
                       "create_time": 1793000000, "content_data": data}, rich.key)
        self.c._on_live(m)
        out = self.c.thread_page({"thread": "c1", "limit": 5})["messages"][0]
        self.assertEqual(out["text"], "\\[富文本\\] 周报\n[文档](https://example.com/docs) **重点**\n\\[图片\\]")
        self.assertEqual(out["format"], "markdown")
        self.assertEqual(out["media"], [{"kind": "image", "ref": "m:m_rich:img_synthetic"}])
        self.assertEqual(m["text"], "[富文本] 周报\n文档 https://example.com/docs 重点\n[图片]")
        self.assertEqual(self.c.store.message("m_rich")["markdown"], out["text"])

    def test_a_leading_markdown_link_is_not_mistaken_for_a_media_placeholder(self):
        self.sign_in()
        m = message("m_link", "c1", 1, text="原始文本", media=[{"kind": "image", "key": "k"}])
        m["markdown"] = "[文档](https://example.com/docs)"
        self.c.on_message(m)
        out = self.c.thread_page({"thread": "c1", "limit": 5})["messages"][0]
        self.assertEqual((out["text"], out["format"]), (m["markdown"], "markdown"))

    def test_history_backfill_keeps_the_same_markdown_representation(self):
        self.sign_in()
        rich = NormalizeTest()
        data = rich.rich((7, rich.prop((1, "历史粗体"))))
        old = normalize({"msg_id": "m_old", "chat_id": "c1", "chat_type": 1, "position": 1, "from_id": "u_a",
                         "msg_type": 4, "msg_type_name": "TEXT", "content": "历史粗体", "create_time": 1793000000,
                         "content_data": data}, rich.key)
        self.c.store.touch_chat("c1", "dm", old["at"])
        self.backend.chats["c1"] = {"name": "", "last_position": 1}
        self.backend.older = {1: old}
        out = self.c.thread_page({"thread": "c1", "limit": 1})["messages"][0]
        self.assertEqual((out["text"], out["format"]), ("**历史粗体**", "markdown"))
        self.assertEqual(out["at"], "2026-10-26T07:33:20Z")

    def test_the_archive_walks_to_the_end_and_never_repeats(self):
        self.sign_in()
        for i in range(1, 4):
            self.c.on_message(message("a%d" % i, "c%d" % i, 1))
        first = self.c.archive_changes({"cursor": "", "limit": 2})
        self.assertEqual(len(first["batches"]), 2)
        self.assertTrue(first["more"])
        rest = self.c.archive_changes({"cursor": first["next"], "limit": 2})
        self.assertEqual([b["chat"]["id"] for b in rest["batches"]], ["c3"])
        self.assertFalse(rest["more"])
        self.assertEqual(self.c.archive_changes({"cursor": rest["next"], "limit": 2})["batches"], [])

    def test_catching_up_reads_what_came_in_while_down(self):
        self.sign_in()
        self.c.on_message(message("m1", "c1", 1))
        self.backend.chats["c1"] = {"name": "小明", "last_position": 3}
        self.backend.older = {2: message("m2", "c1", 2), 3: message("m3", "c1", 3)}
        self.c.catch_up()
        self.assertEqual([m["id"] for m in self.c.store.messages("c1", 0, 10)], ["m1", "m2", "m3"])

    def test_reconnects_do_not_overlap_catch_up_and_close_waits_for_history(self):
        self.sign_in()
        self.c.on_message(message("m1", "c1", 1))
        self.backend.chats["c1"] = {"name": "小明", "last_position": 3}
        release = threading.Event()
        entered = threading.Event()

        def history(chat, positions):
            entered.set()
            self.assertTrue(release.wait(5))
            return [self.backend.older[p] for p in positions if p in self.backend.older]

        self.backend.older = {2: message("m2", "c1", 2), 3: message("m3", "c1", 3)}
        self.backend.history = history
        first = threading.Thread(target=self.c.catch_up)
        first.start()
        self.assertTrue(entered.wait(5))
        self.c._on_link("up")
        closing = threading.Thread(target=self.c.close)
        closing.start()
        self.assertTrue(closing.is_alive())
        release.set()
        first.join(5)
        self.assertFalse(first.is_alive())
        closing.join(5)
        self.assertFalse(closing.is_alive())
        with self.assertRaises(sqlite3.ProgrammingError):
            self.c.store.messages("c1", 0, 10)

    def test_sign_in_keeps_the_credentials_in_the_hub(self):
        self.sign_in()
        self.assertEqual(json.loads(base64.b64decode(self.hub.saved)), {"cookies": {"session": "s"}})
        self.assertEqual(self.c.status({})["state"], "ok")
        self.assertFalse(any(f.endswith(".json") for f in os.listdir(self.dir.name)))

    def test_the_qr_is_the_same_until_it_expires(self):
        self.c._ready.set()
        self.assertEqual(self.c.login_step({"step": ""})["url"], "qr-new")
        self.assertEqual(self.c.login_step({"step": "scan"})["url"], "qr-same")
        self.backend.poll = "expired"
        self.assertEqual(self.c.login_step({"step": "scan"})["url"], "qr-new")

    def test_a_network_failure_during_sign_in_is_said_in_words(self):
        self.c._ready.set()
        self.backend.login_start = lambda: (_ for _ in ()).throw(OSError("no route"))
        with self.assertRaises(RpcError) as e:
            self.c.login_step({"step": ""})
        self.assertIn("连不上飞书", e.exception.message)

    def test_a_poll_that_did_not_get_through_keeps_the_qr(self):
        self.c._ready.set()
        self.backend.login_poll = lambda: (_ for _ in ()).throw(OSError("no route"))
        self.assertEqual(self.c.login_step({"step": "scan"})["url"], "qr-same")

    def test_a_scan_that_did_not_sign_in_asks_again(self):
        self.c._ready.set()
        self.backend.poll = "done"
        self.backend.finish_error = Expired()
        prompt = self.c.login_step({"step": "scan"})
        self.assertEqual(prompt["kind"], "qr")
        self.assertTrue(prompt["error"])
        self.assertIsNone(self.hub.saved)

    def test_logout_forgets_the_session(self):
        self.sign_in()
        self.c.logout({})
        self.assertTrue(self.hub.deleted)
        self.assertEqual(self.c.status({})["state"], "not_signed_in")

    def test_logout_during_restore_does_not_activate_the_old_session(self):
        self.hub.saved = base64.b64encode(b'{"cookies": {"session": "s"}}').decode()
        self.c._stop.set()

        def restore(saved):
            self.backend.restored.append(saved)
            self.c.logout({})
            return {"id": ME, "name": "Synthetic account"}

        self.backend.restore = restore
        with mock.patch.object(self.backend, "listen", wraps=self.backend.listen) as listen:
            self.c._boot()
            self.assertEqual(self.c.status({})["state"], "not_signed_in")
            self.assertIsNone(self.hub.saved)
            listen.assert_not_called()

    def test_logout_cancels_a_saved_session_retry(self):
        import larkx_persona.connector as module
        self.hub.saved = base64.b64encode(b'{"cookies": {"session": "s"}}').decode()
        self.backend.restore_error = ConnectionError("synthetic network failure")
        self.hub.log = lambda line: self.c.logout({})
        self.c._stop.set()
        with mock.patch.object(module, "RESTORE_RETRY_SECONDS", 0):
            self.c._boot()
        self.assertEqual(len(self.backend.restored), 1)
        self.assertEqual(self.c.status({})["state"], "not_signed_in")
        self.assertIsNone(self.hub.saved)

    def test_logout_waits_for_an_inflight_restore_without_reactivating_it(self):
        self.hub.saved = base64.b64encode(b'{"cookies": {"session": "s"}}').decode()
        entered, release = threading.Event(), threading.Event()
        errors = []

        def restore(saved):
            entered.set()
            if not release.wait(2):
                raise TimeoutError("synthetic restore timed out")
            return {"id": ME, "name": "Synthetic account"}

        def run(action):
            try:
                action()
            except Exception as error:
                errors.append(error)

        self.backend.restore = restore
        boot = threading.Thread(target=run, args=(self.c._boot,), daemon=True)
        logout = threading.Thread(target=run, args=(lambda: self.c.logout({}),), daemon=True)
        self.c._stop.set()
        self.addCleanup(release.set)
        with mock.patch.object(self.backend, "listen", wraps=self.backend.listen) as listen:
            boot.start()
            self.assertTrue(entered.wait(2))
            logout.start()
            self.assertTrue(self.c._restore_cancel.wait(2))
            release.set()
            boot.join(2)
            logout.join(2)
            self.assertFalse(boot.is_alive())
            self.assertFalse(logout.is_alive())
            self.assertEqual(errors, [])
            self.assertEqual(self.c.status({})["state"], "not_signed_in")
            self.assertIsNone(self.hub.saved)
            listen.assert_not_called()

    def test_a_new_scan_supersedes_the_saved_session_retry(self):
        import larkx_persona.connector as module
        self.hub.saved = base64.b64encode(b'{"cookies": {"session": "s"}}').decode()
        self.backend.restore_error = ConnectionError("synthetic network failure")
        self.backend.poll = "done"
        self.c._stop.set()

        def scan(line):
            self.c._ready.set()
            self.assertTrue(self.c.login_step({"step": "scan"})["done"])

        self.hub.log = scan
        with mock.patch.object(module, "RESTORE_RETRY_SECONDS", 0):
            self.c._boot()
        self.assertEqual(len(self.backend.restored), 1)
        self.assertEqual(self.c.status({})["state"], "ok")
        self.assertFalse(self.hub.deleted)

    def test_an_expired_session_asks_to_scan_again(self):
        self.sign_in()
        self.c._on_link("expired")
        status = self.c.status({})
        self.assertEqual(status["state"], "not_signed_in")
        self.assertIn("失效", status["error"])

    def test_a_network_failure_keeps_the_session(self):
        self.hub.saved = base64.b64encode(b'{"cookies": {"session": "s"}}').decode()
        self.backend.restore_error = ConnectionError("no route")
        import larkx_persona.connector as module
        wait, module.RESTORE_RETRY_SECONDS = module.RESTORE_RETRY_SECONDS, 0
        self.addCleanup(setattr, module, "RESTORE_RETRY_SECONDS", wait)
        self.addCleanup(lambda: None)
        self.c._boot()
        self.assertEqual(len(self.backend.restored), 2)
        self.assertEqual(self.c.status({})["state"], "ok")
        self.assertFalse(self.hub.deleted)

    def test_an_expired_saved_session_is_said_and_kept_out_of_the_way(self):
        self.hub.saved = base64.b64encode(b'{"cookies": {"session": "s"}}').decode()
        self.backend.restore_error = Expired()
        self.c._boot()
        self.assertIn("失效", self.c.status({})["error"])


class NormalizeTest(unittest.TestCase):
    def key(self, msg_type, data):
        return data.get("key", "")

    def test_a_text(self):
        m = normalize({"msg_id": "1", "chat_id": "2", "chat_type": 1, "position": "7", "from_id": "3", "msg_type": 4, "msg_type_name": "TEXT",
                       "content": "你好", "create_time": "1793000000", "root_id": "0", "parent_id": "0"}, self.key)
        self.assertEqual((m["id"], m["chat"], m["chat_kind"], m["position"], m["at"], m["root"], m["parent"]),
                         ("1", "2", "dm", 7, 1793000000.0, "", ""))

    def test_milliseconds_become_seconds(self):
        self.assertEqual(normalize({"create_time": 1793000000000}, self.key)["at"], 1793000000.0)

    @staticmethod
    def prop(*fields):
        """A rich text element's property: (field number, string) pairs, as protobuf writes them."""
        out = b""
        for number, value in fields:
            raw = value.encode()
            out += bytes([number << 3 | 2, len(raw)]) + raw
        return out

    def rich(self, *elements):
        """TextContent / PostContent as protobuf_to_dict gives it: (tag, property) elements in order."""
        ids = [str(i) for i in range(1, len(elements) + 1)]
        return {"richText": {"elementIds": ids, "elements": {"dictionary": {
            i: {"tag": tag, "property": prop} for i, (tag, prop) in zip(ids, elements)}}}}

    def node(self, tag, text=None, children=(), fields=None):
        """A synthetic tree node; fields can override TextProperty.content for links and images."""
        fields = ((1, text),) if fields is None and text is not None else (fields or ())
        return {"tag": tag, "property": self.prop(*fields), "childIds": list(children)}

    def tree(self, roots, nodes):
        return {"richText": {"elementIds": list(roots), "elements": {"dictionary": nodes}}}

    def test_styles_and_literal_markup_become_safe_markdown(self):
        data = self.rich((7, self.prop((1, "粗体"))), (1, self.prop((1, " "))),
                         (8, self.prop((1, "斜体"))), (1, self.prop((1, " "))),
                         (9, self.prop((1, "下划线"))), (1, self.prop((1, " *literal* [label] <script>"))))
        m = normalize({"msg_type": 4, "msg_type_name": "TEXT", "content": "原始纯文本", "content_data": data}, self.key)
        self.assertEqual(m["text"], "原始纯文本")
        self.assertEqual(m["markdown"], "**粗体** *斜体* 下划线 \\*literal\\* \\[label\\] \\<script\\>")

    def test_nested_unordered_and_ordered_lists_keep_their_structure(self):
        nodes = {
            "ul": self.node(26, children=("first", "second")),
            "first": self.node(28, "第一项", ("nested",)),
            "nested": self.node(27, children=("nested-item",)),
            "nested-item": self.node(28, "嵌套项"),
            "second": self.node(28, "第二项"),
            "ol": self.node(27, children=("one", "two")),
            "one": self.node(28, "甲"),
            "two": self.node(28, "乙"),
        }
        data = self.tree(("ul", "ol"), nodes)
        m = normalize({"msg_type": 2, "msg_type_name": "POST", "content": "原始纯文本", "content_data": data}, self.key)
        self.assertEqual(m["text"], "原始纯文本")
        self.assertEqual(m["markdown"], "\\[富文本\\] \n- 第一项\n  1. 嵌套项\n- 第二项\n\n1. 甲\n2. 乙")

    def test_text_and_media_around_a_nested_list_keep_child_order(self):
        nodes = {
            "list": self.node(26, children=("item",)),
            "item": self.node(28, children=("before", "nested", "after", "image")),
            "before": self.node(1, "之前"),
            "nested": self.node(26, children=("nested-item",)),
            "nested-item": self.node(28, "子项"),
            "after": self.node(1, "之后"),
            "image": self.node(2, fields=((2, "img_synthetic"),)),
        }
        data = self.tree(("list",), nodes)
        m = normalize({"msg_type": 4, "msg_type_name": "TEXT", "content": "原始纯文本", "content_data": data}, self.key)
        self.assertEqual(m["markdown"], "- 之前\n  - 子项\n  之后\\[图片\\]")

    def test_a_non_integer_tag_falls_back_without_losing_the_message(self):
        data = self.rich((1, self.prop((1, "bad tag"))))
        data["richText"]["elements"]["dictionary"]["1"]["tag"] = []
        for name, number in (("TEXT", 4), ("POST", 2)):
            m = normalize({"msg_type": number, "msg_type_name": name, "content": "原始纯文本", "content_data": data}, self.key)
            self.assertEqual((m["text"], m["markdown"]), ("原始纯文本", ""))

    def test_inline_and_block_code_use_safe_delimiters(self):
        data = self.rich((30, self.prop((1, "a`b"))), (1, self.prop((1, "\n"))),
                         (31, self.prop((1, "print('<ok>')\n```"))), (1, self.prop((1, " "))),
                         (30, self.prop((1, "`tick`"))))
        m = normalize({"msg_type": 4, "msg_type_name": "TEXT", "content": "原始纯文本", "content_data": data}, self.key)
        self.assertEqual(m["markdown"], "``a`b``\n\n````\nprint('<ok>')\n```\n````\n\n `` `tick` ``")

    def test_inline_code_newlines_cannot_open_an_html_block(self):
        data = self.rich((30, self.prop((1, "before\n\n<script>alert(1)</script>\nafter"))))
        m = normalize({"msg_type": 4, "msg_type_name": "TEXT", "content": "原始纯文本", "content_data": data}, self.key)
        self.assertEqual(m["markdown"], "`before  <script>alert(1)</script> after`")

    def test_a_code_block_after_text_starts_its_fence_on_a_new_line(self):
        data = self.rich((1, self.prop((1, "before"))), (31, self.prop((1, "<script>alert(1)</script>"))),
                         (1, self.prop((1, "after"))))
        m = normalize({"msg_type": 4, "msg_type_name": "TEXT", "content": "原始纯文本", "content_data": data}, self.key)
        self.assertEqual(m["markdown"], "before\n```\n<script>alert(1)</script>\n```\n\nafter")

    def test_links_keep_titles_and_reject_active_schemes(self):
        data = self.rich((6, self.prop((1, "https://example.com/docs"), (2, "文档 [v1]"))),
                         (1, self.prop((1, " "))), (17, self.prop((1, "https://example.com/plain"))),
                         (1, self.prop((1, " "))), (6, self.prop((1, "javascript:alert(1)"), (2, "危险链接"))),
                         (1, self.prop((1, " "))), (6, self.prop((1, "https://example.com/a(b)"), (2, "括号"))))
        m = normalize({"msg_type": 4, "msg_type_name": "TEXT", "content": "原始纯文本", "content_data": data}, self.key)
        self.assertEqual(m["markdown"], "[文档 \\[v1\\]](https://example.com/docs) <https://example.com/plain> 危险链接 "
                                        "[括号](https://example.com/a%28b%29)")
        self.assertNotIn("](javascript:", m["markdown"])

    def test_rich_markdown_does_not_move_plain_mention_spans(self):
        data = self.rich((7, self.prop((1, "重要"))), (1, self.prop((1, " "))),
                         (5, self.prop((1, "7001"), (2, "@Example"))), (1, self.prop((1, " 总结"))))
        m = normalize({"msg_type": 4, "msg_type_name": "TEXT", "content": "重要 @Example 总结", "content_data": data}, self.key)
        self.assertEqual(m["markdown"], "**重要** \\@Example 总结")
        self.assertEqual(m["text"], "重要 @Example 总结")
        self.assertEqual(m["mentions"], [{"id": "7001", "start": 3, "end": 11, "label": "@Example"}])
        self.assertEqual(mention_request(m["text"], m["mentions"], "7001"), "重要  总结")

    def test_a_pasted_doc_link_keeps_its_title_and_address(self):
        # 2026-11-12: LarkAgentX's text dropped the A element whole, so only the words before the link arrived.
        url = "https://example.feishu.cn/docx/AbC123"
        data = self.rich((1, self.prop((1, "请看一下 "))), (6, self.prop((1, url), (2, "示例文档"))))
        m = normalize({"msg_type": 4, "msg_type_name": "TEXT", "content": "请看一下 ", "content_data": data}, self.key)
        self.assertEqual(m["text"], "请看一下 示例文档 " + url)

    def test_a_link_in_the_middle_keeps_its_place_and_stays_apart_from_the_next_words(self):
        url = "https://example.com/a"
        data = self.rich((1, self.prop((1, "看"))), (6, self.prop((1, url), (3, url))), (1, self.prop((1, "，周五前回复"))),
                         (5, self.prop((1, "ou_1"), (2, "@小王"))))
        m = normalize({"msg_type": 4, "msg_type_name": "TEXT", "content": "看，周五前回复@小王", "content_data": data}, self.key)
        self.assertEqual(m["text"], "看 " + url + " ，周五前回复@小王")

    def test_a_post_with_a_link_keeps_larkagentxs_title_line(self):
        data = self.rich((3, self.prop((1, "正文"))), (6, self.prop((1, "https://example.com/b"), (2, "链接"))))
        data["title"] = "周报"
        m = normalize({"msg_type": 2, "msg_type_name": "POST", "content": "[富文本] 周报\n正文\n", "content_data": data}, self.key)
        self.assertEqual(m["text"], "[富文本] 周报\n正文\n链接 https://example.com/b")

    def test_without_a_link_larkagentxs_text_stays(self):
        data = self.rich((1, self.prop((1, "你好"))), (10, self.prop((1, "smile"))))
        m = normalize({"msg_type": 4, "msg_type_name": "TEXT", "content": "你好[表情]", "content_data": data}, self.key)
        self.assertEqual(m["text"], "你好[表情]")

    def test_a_malformed_link_property_does_not_break_the_message(self):
        data = self.rich((1, self.prop((1, "看 "))), (6, b"\x0a\xff\x01"))
        m = normalize({"msg_type": 4, "msg_type_name": "TEXT", "content": "看 ", "content_data": data}, self.key)
        self.assertEqual(m["text"], "看 ")

    def test_a_link_that_renders_empty_keeps_larkagentxs_text(self):
        # LarkAgentX falls back to its text when the rich text renders as nothing; so does the rebuild.
        data = self.rich((6, self.prop((4, "x"))))
        m = normalize({"msg_type": 4, "msg_type_name": "TEXT", "content": "原文", "content_data": data}, self.key)
        self.assertEqual(m["text"], "原文")

    def test_a_repeated_link_field_reads_as_protobuf_reads_it(self):
        # A non-repeated field written twice is its last value in protobuf.
        data = self.rich((6, self.prop((1, "https://example.com/old"), (1, "https://example.com/new"))))
        m = normalize({"msg_type": 4, "msg_type_name": "TEXT", "content": "", "content_data": data}, self.key)
        self.assertEqual(m["text"], "https://example.com/new")

    def test_a_link_and_a_self_mention_share_one_text(self):
        # The spans follow the text with its link: a span counted in LarkAgentX's text, which has no link, would miss.
        url = "https://example.com/c"
        data = self.rich((1, self.prop((1, "看 "))), (6, self.prop((1, url), (3, url))), (5, self.prop((1, "7001"), (2, "@Example"))),
                         (1, self.prop((1, " 总结一下"))))
        m = normalize({"msg_type": 4, "msg_type_name": "TEXT", "content": "看 @Example 总结一下", "content_data": data}, self.key)
        self.assertEqual(m["text"], "看 " + url + " @Example 总结一下")
        self.assertEqual(m["mentions"], [{"id": "7001", "start": len("看 " + url + " "), "end": len("看 " + url + " @Example"),
                                          "label": "@Example"}])
        self.assertEqual(mention_request(m["text"], m["mentions"], "7001"), "看 " + url + "  总结一下")

    def test_a_post_with_a_link_shifts_its_spans_past_the_title(self):
        url = "https://example.com/d"
        data = self.rich((5, self.prop((1, "7001"), (2, "@Example"))), (1, self.prop((1, " 看 "))), (6, self.prop((1, url))))
        data["title"] = "周报"
        m = normalize({"msg_type": 4, "msg_type_name": "POST", "content": "[富文本] 周报\n@Example 看 ", "content_data": data}, self.key)
        self.assertEqual(m["text"], "[富文本] 周报\n@Example 看 " + url)
        span = m["mentions"][0]
        self.assertEqual(m["text"][span["start"]:span["end"]], "@Example")

    def test_without_a_link_the_spans_stay_on_larkagentxs_text(self):
        data = self.rich((5, self.prop((1, "7001"), (2, "@Example"))), (1, self.prop((1, " 总结"))))
        m = normalize({"msg_type": 4, "msg_type_name": "TEXT", "content": "@Example 总结", "content_data": data}, self.key)
        self.assertEqual((m["text"], m["mentions"]), ("@Example 总结", [{"id": "7001", "start": 0, "end": 8, "label": "@Example"}]))

    def text_of(self, data, content="原文"):
        return normalize({"msg_type": 4, "msg_type_name": "TEXT", "content": content, "content_data": data}, self.key)["text"]

    def test_a_decoder_of_another_shape_keeps_larkagentxs_text(self):
        # Review of #570: none of these may raise; each keeps the text LarkAgentX gave.
        link = (6, self.prop((1, "https://example.com/e")))
        for dictionary in (None, [], "x", 7):
            self.assertEqual(self.text_of({"richText": {"elementIds": ["1"], "elements": {"dictionary": dictionary}}}), "原文")
        for rich in (None, [], "x", {"elements": None}, {"elements": []}, {"elements": {"dictionary": {}}, "elementIds": "12"}):
            m = normalize({"msg_type": 4, "msg_type_name": "TEXT", "content": "原文", "content_data": {"richText": rich}}, self.key)
            self.assertEqual((m["text"], m["mentions"]), ("原文", []))
        data = self.rich((1, self.prop((1, "看 "))), link)
        data["richText"]["elementIds"] = [["1"], {"k": 1}, None, "1", "2"]  # unhashable and non-string ids
        self.assertEqual(self.text_of(data), "看 https://example.com/e")
        data = self.rich(link)
        data["richText"]["elements"]["dictionary"]["3"] = None  # an element that is not a dictionary
        data["richText"]["elements"]["dictionary"]["4"] = "x"
        self.assertEqual(self.text_of(data), "https://example.com/e")
        data = self.rich((1, "看"), link)  # a property decoded as a string instead of bytes
        self.assertEqual(self.text_of(data), "https://example.com/e")
        data = self.rich((6, "https://example.com/e"))
        self.assertEqual(self.text_of(data), "原文")

    def test_a_length_past_the_end_reads_no_address(self):
        # Review of #570: a truncated href is not taken as a shorter one; with no link added LarkAgentX's text stays.
        truncated = bytes([1 << 3 | 2, 50]) + b"https://ex"
        self.assertEqual(self.text_of(self.rich((1, self.prop((1, "看 "))), (6, truncated)), "看 "), "看 ")
        fixed = bytes([2 << 3 | 1]) + b"\x00" * 3  # a fixed64 with only 3 of its 8 bytes
        self.assertEqual(self.text_of(self.rich((6, fixed)), "原文"), "原文")

    def test_an_overlong_varint_ends_the_read_at_once(self):
        # Review of #570: a run of continuation bytes is not read as one huge number.
        from larkx_persona.real import _fields
        overlong = b"\x80" * 100000 + b"\x01"
        started = time.monotonic()
        self.assertEqual(_fields(overlong), {})
        self.assertEqual(_fields(bytes([1 << 3 | 2]) + b"\xff" * 11 + b"\x01"), {})
        self.assertLess(time.monotonic() - started, 1)
        self.assertEqual(self.text_of(self.rich((1, self.prop((1, "看 "))), (6, overlong)), "看 "), "看 ")

    def test_a_linked_post_keeps_its_body_s_leading_lines(self):
        url = "https://example.com/f"
        data = self.rich((1, self.prop((1, "\n  缩进的第一行 "))), (6, self.prop((1, url))))
        data["title"] = "周报"
        m = normalize({"msg_type": 2, "msg_type_name": "POST", "content": "[富文本] 周报\n\n  缩进的第一行", "content_data": data}, self.key)
        self.assertEqual(m["text"], "[富文本] 周报\n\n  缩进的第一行 " + url)

    def test_an_empty_element_after_a_link_keeps_it_apart_from_the_next_words(self):
        data = self.rich((6, self.prop((1, "https://example.com"))), (6, self.prop((2, ""))), (1, self.prop((1, "next"))))
        self.assertEqual(self.text_of(data), "https://example.com next")

    def test_an_anchor_with_no_readable_address_keeps_larkagentxs_text(self):
        # Review of #570: replace LarkAgentX's text only when a link was really added.
        self.assertEqual(self.text_of(self.rich((1, self.prop((1, "看 "))), (6, self.prop((2, "只有标题")))), "看 "), "看 ")
        anchored = {"richText": {"elementIds": [], "anchorIds": ["9"], "elements": {"dictionary": {}}}}
        self.assertEqual(self.text_of(anchored, "看 "), "看 ")
        unreadable = self.rich((1, self.prop((1, "看 "))), (6, b"\xff\xff"))
        self.assertEqual(self.text_of(unreadable, "看 "), "看 ")

    def test_media(self):
        image = normalize({"msg_type_name": "IMAGE", "content_data": {"key": "k"}}, self.key)
        self.assertEqual(image["media"], [{"kind": "image", "key": "k"}])
        document = normalize({"msg_type_name": "FILE", "content_data": {"key": "k", "name": "a.pdf"}}, self.key)
        self.assertEqual(document["media"], [{"kind": "file", "name": "a.pdf"}])

    def test_routing_topic_id_extraction(self):
        dm = normalize({"msg_id": "10", "chat_id": "c_dm", "chat_type": 1, "root_id": "9", "scope": "chat", "anchor": ""}, self.key)
        self.assertEqual((dm["chat_kind"], dm["topic_id"]), ("dm", ""))

        reg = normalize({"msg_id": "11", "chat_id": "g1", "chat_type": 2, "root_id": "0", "scope": "chat", "anchor": ""}, self.key)
        self.assertEqual((reg["chat_kind"], reg["topic_id"]), ("group", ""))

        reply = normalize({"msg_id": "12", "chat_id": "g1", "chat_type": 2, "root_id": "11", "scope": "topic", "anchor": "11"}, self.key)
        self.assertEqual((reply["chat_kind"], reply["topic_id"]), ("group", "11"))

        topic_group_root = normalize({"msg_id": "13", "chat_id": "g_topic", "chat_type": 3, "msg_type_name": "TEXT", "root_id": "0"}, self.key)
        self.assertEqual((topic_group_root["chat_kind"], topic_group_root["topic_id"]), ("group", "13"))

        dispatched_topic_root = normalize({"msg_id": "14", "chat_id": "g_topic2", "chat_type": 2, "scope": "topic", "anchor": "14"}, self.key)
        self.assertEqual((dispatched_topic_root["chat_kind"], dispatched_topic_root["topic_id"]), ("group", "14"))

        # Synthetic defensive fallback cases: pinned LarkAgentX (97ad48e) folds threadId into anchor and does not
        # emit a top-level thread_id key, so these verify normalize() stays safe if a future payload/fixture adds one.
        explicit_thread = normalize({"msg_id": "15", "chat_id": "g1", "chat_type": 2, "thread_id": "omt_99", "root_id": "11"}, self.key)
        self.assertEqual((explicit_thread["chat_kind"], explicit_thread["topic_id"]), ("group", "omt_99"))

        chat_scope_zero_root = normalize(
            {"msg_id": "16", "chat_id": "g1", "chat_type": 2, "scope": "chat", "root_id": "0", "thread_id": "omt_99"}, self.key
        )
        self.assertEqual((chat_scope_zero_root["chat_kind"], chat_scope_zero_root["topic_id"]), ("group", "omt_99"))
        self.assertEqual(_channel_routing(chat_scope_zero_root), {"chat_id": "g1", "chat_kind": "topic", "topic_id": "omt_99"})

        chat_scope_empty_root = normalize(
            {"msg_id": "17", "chat_id": "g1", "chat_type": 2, "scope": "chat", "root_id": "", "thread_id": "omt_100"}, self.key
        )
        self.assertEqual((chat_scope_empty_root["chat_kind"], chat_scope_empty_root["topic_id"]), ("group", "omt_100"))
        self.assertEqual(_channel_routing(chat_scope_empty_root), {"chat_id": "g1", "chat_kind": "topic", "topic_id": "omt_100"})

        dm_with_thread = normalize(
            {"msg_id": "18", "chat_id": "c_dm", "chat_type": 1, "scope": "chat", "root_id": "9", "thread_id": "omt_99"}, self.key
        )
        self.assertEqual((dm_with_thread["chat_kind"], dm_with_thread["topic_id"]), ("dm", ""))
        self.assertEqual(_channel_routing(dm_with_thread), {"chat_id": "c_dm", "chat_kind": "direct"})

        # Regression: chat_type=3 topic-group root still gets msg_id even when scope="chat" and no root/thread IDs.
        topic_group_chat_scope = normalize(
            {"msg_id": "19", "chat_id": "g_topic", "chat_type": 3, "msg_type_name": "TEXT",
             "scope": "chat", "root_id": "0", "thread_id": ""}, self.key
        )
        self.assertEqual((topic_group_chat_scope["chat_kind"], topic_group_chat_scope["topic_id"]), ("group", "19"))
        self.assertEqual(_channel_routing(topic_group_chat_scope), {"chat_id": "g_topic", "chat_kind": "topic", "topic_id": "19"})

        topic_group_system = normalize(
            {"msg_id": "20", "chat_id": "g_topic", "chat_type": 3, "msg_type_name": "SYSTEM",
             "scope": "chat", "root_id": "0", "thread_id": ""}, self.key
        )
        self.assertEqual((topic_group_system["chat_kind"], topic_group_system["topic_id"]), ("group", ""))

        # When agent_group_topics is off (topics=False), group/topic messages suppress chat_kind and topic_id.
        self.assertEqual(_channel_routing(reply, topics=False), {"chat_id": "g1"})
        self.assertEqual(_channel_routing(topic_group_chat_scope, topics=False), {"chat_id": "g_topic"})
        self.assertEqual(_channel_routing(dm_with_thread, topics=False), {"chat_id": "c_dm", "chat_kind": "direct"})


if __name__ == "__main__":
    unittest.main()
