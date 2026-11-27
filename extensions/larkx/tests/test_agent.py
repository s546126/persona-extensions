"""/agent: the user's own message goes to the hub as a channel message, and the answer comes back under it.
Run them: python3 -m unittest discover -s tests, in extensions/larkx."""
import base64
import json
import os
import sys
import tempfile
import threading
import time
import unittest

sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "src"))
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

from test_connector import ME, Backend, Hub, NormalizeTest, message  # noqa: E402

from larkx_persona import main  # noqa: E402
from larkx_persona.connector import (  # noqa: E402
    AGENT_PREFIX, AGENT_PREFIX_LEAST, AGENT_PREFIX_MOST, AGENT_SUFFIX, AGENT_SUFFIX_LEAST, AGENT_SUFFIX_MOST,
    Connector, agent_request, normalize_agent_prefix,
)
from larkx_persona.main import Plugin  # noqa: E402
from larkx_persona.real import normalize, own_chat_ok  # noqa: E402
from larkx_persona.store import Store  # noqa: E402
from larkx_persona.wire import RpcError  # noqa: E402


class ChannelHub(Hub):
    def __init__(self):
        super().__init__()
        self.received = []
        self.got = threading.Event()

    def call(self, method, params):
        if method == "channel.receive":
            self.received.append(params)
            self.got.set()
            return {}
        return super().call(method, params)


class AgentRequestTest(unittest.TestCase):
    def test_what_counts_as_a_request(self):
        self.assertEqual(agent_request("/agent 总结一下"), "总结一下")
        self.assertEqual(agent_request("  /Agent：帮我看看"), "帮我看看")
        self.assertEqual(agent_request("/agent\n第二行"), "第二行")
        self.assertEqual(agent_request("/agent"), "")
        self.assertIsNone(agent_request("/agents 列表"))
        self.assertIsNone(agent_request("你好 /agent 在中间"))
        self.assertIsNone(agent_request(""))


class AgentTest(unittest.TestCase):
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

    def handed(self):
        self.assertTrue(self.hub.got.wait(2), "nothing was handed to the hub")
        return self.hub.received[-1]

    def nothing_handed(self):
        time.sleep(0.1)
        self.assertEqual(self.hub.received, [])

    def test_my_live_agent_goes_to_the_hub_with_where_it_was_written(self):
        self.backend.chats["g1"] = {"name": "项目群", "last_position": 0}
        self.c._on_live(message("m1", "g1", 1, kind="group", text="周五发布"))
        self.c._on_live(message("m2", "g1", 2, kind="group", sender=ME, text="/agent 帮我总结下这个群"))
        got = self.handed()
        self.assertEqual(got["contribution"], "larkx")
        self.assertEqual(got["message"]["id"], "m2")
        self.assertEqual(got["message"]["text"], "帮我总结下这个群")
        self.assertIn("项目群", got["message"]["forward"])
        self.assertIn("周五发布", got["message"]["forward"])
        self.assertNotIn("帮我总结下这个群", got["message"]["forward"])

    def test_group_agent_context_includes_bots_and_members_when_available(self):
        self.backend.chats["g1"] = {"name": "项目群", "last_position": 0}
        self.backend.members["g1"] = [
            {"id": "7002", "name": "Example Bot", "real_name": "Example Bot", "kind": "bot"},
            {"id": "7001", "name": "小明", "real_name": "小明", "kind": "user"},
        ]
        self.c._on_live(message("m1", "g1", 1, kind="group", sender=ME, text="/agent 艾特机器人"))
        got = self.handed()
        self.assertIn("Example Bot[BOT](id=7002)", got["message"]["forward"])
        self.assertIn("小明[成员](id=7001)", got["message"]["forward"])

    def test_a_dm_with_myself_works_too(self):
        self.c._on_live(message("m1", "c_me", 1, sender=ME, text="/agent 你看得到这个消息吗"))
        got = self.handed()
        self.assertEqual(got["message"]["text"], "你看得到这个消息吗")
        self.assertIn("私聊", got["message"]["forward"])

    def test_a_post_agent_ignores_the_rich_text_title(self):
        self.c._on_live(message("m1", "c1", 1, sender=ME, msg_type="POST", text="[富文本] 示例标题\n/agent 检查"))
        self.assertEqual(self.handed()["message"]["text"], "检查")

    def test_an_agent_image_is_sent_as_a_channel_blob(self):
        m = message("m1", "c1", 1, sender=ME, msg_type="POST", text="[富文本] \n/agent\n[图片]\ncheck this issue",
                    media=[{"kind": "image", "key": "img_inline"}])
        self.c._hand_over(m, "[图片]\ncheck this issue")
        got = self.handed()["message"]
        self.assertEqual(got["images"], [{"mime": "image/png", "data": base64.b64encode(b"png").decode()}])

    def test_others_old_fetched_and_ai_answers_do_not_count(self):
        self.c._on_live(message("m1", "c1", 1, sender="u_a", text="/agent 帮我转账"))
        self.c._on_live(message("m2", "c1", 2, sender=ME, text="/agent 太早了", at=time.time() - 3600))
        self.c.on_message(message("m3", "c1", 3, sender=ME, text="/agent 拉回来的历史"))
        self.c._on_live(message("m4", "c1", 4, sender=ME, text="/agent 好的\n\n" + AGENT_SUFFIX))
        self.c._on_live(message("m5", "c1", 5, sender=ME, text="/agents"))
        self.nothing_handed()

    def test_the_same_message_twice_is_handed_over_once(self):
        m = message("m1", "c1", 1, sender=ME, text="/agent 一次")
        self.c._on_live(m)
        self.handed()
        self.c._on_live(dict(m))
        time.sleep(0.1)
        self.assertEqual(len(self.hub.received), 1)

    def test_the_answer_goes_under_the_request_once(self):
        self.c._on_live(message("m1", "c1", 1, sender=ME, text="/agent 总结"))
        self.handed()
        out = self.c.channel_send({"ref": "r1", "text": "这是总结", "kind": "answer", "reply_to": "m1"})
        self.assertEqual(out, {"message_id": "m_new"})
        self.assertEqual(self.backend.sent, [("c1", "这是总结\n\n" + AGENT_SUFFIX, "m1")])
        self.assertEqual(self.c.channel_send({"ref": "r1", "text": "这是总结", "kind": "answer", "reply_to": "m1"}),
                         {"message_id": "m_new"})
        self.assertEqual(len(self.backend.sent), 1)

    def test_an_answer_in_a_topic_stays_in_the_topic(self):
        self.c._on_live(message("m2", "g1", 2, kind="group", sender=ME, text="/agent 看看", root="m_root"))
        self.handed()
        self.c.channel_send({"ref": "r1", "text": "好", "kind": "answer", "reply_to": "m2"})
        self.assertEqual(self.backend.sent[0][2], "m_root")

    def test_nothing_goes_where_the_user_did_not_ask(self):
        self.c.on_message(message("m1", "c1", 1, sender="u_a", text="在吗"))
        # Only a push goes out without a request (to the user's own chat, AgentPushTest); a push answering a message
        # still needs that message to be one of the user's own requests.
        for p in ({"ref": "r1", "text": "x", "kind": "note"}, {"ref": "r2", "text": "x", "kind": "answer", "reply_to": "m1"},
                  {"ref": "r3", "text": "x", "kind": "answer", "reply_to": "m_unknown"},
                  {"ref": "r4", "text": "x", "kind": "push", "reply_to": "m1"}):
            with self.assertRaises(RpcError) as e:
                self.c.channel_send(p)
            self.assertEqual(e.exception.code, "cannot_send")
        with self.assertRaises(RpcError):
            self.c.channel_send({"text": "x", "kind": "answer", "reply_to": "m1"})
        self.assertEqual(self.backend.sent, [])

    def test_an_answer_only_goes_to_an_agent_request(self):
        # A normal message from the user is not an /agent, and our own answer is not one either.
        self.c.on_message(message("m_plain", "c1", 1, sender=ME, text="ordinary message"))
        self.c.on_message(message("m_ai", "c1", 2, sender=ME, text="答\n\n" + AGENT_SUFFIX))
        for mid in ("m_plain", "m_ai"):
            with self.assertRaises(RpcError) as e:
                self.c.channel_send({"ref": "r_" + mid, "text": "x", "kind": "answer", "reply_to": mid})
            self.assertEqual(e.exception.code, "cannot_send")
        self.assertEqual(self.backend.sent, [])

    def test_an_unknown_result_is_never_sent_again(self):
        self.c._on_live(message("m1", "c1", 1, sender=ME, text="/agent 发"))
        self.handed()
        self.backend.send_error = OSError("cut off")
        with self.assertRaises(RpcError):
            self.c.channel_send({"ref": "r1", "text": "x", "kind": "answer", "reply_to": "m1"})
        self.backend.send_error = None
        with self.assertRaises(RpcError):
            self.c.channel_send({"ref": "r1", "text": "x", "kind": "answer", "reply_to": "m1"})
        self.assertEqual(self.backend.sent, [])


class AgentTopicTest(unittest.TestCase):
    """A group request carries its topic as the channel message's thread: the hub gives each topic a conversation of its
    own. The key is the topic the answer goes into: the request's root, or the request itself when it opens the topic."""
    setUp, handed, nothing_handed = AgentTest.setUp, AgentTest.handed, AgentTest.nothing_handed

    def test_a_request_outside_a_topic_opens_one(self):
        self.c._on_live(message("m1", "g1", 1, kind="group", sender=ME, text="/agent 新问题"))
        self.assertEqual(self.handed()["message"]["thread"], "g1:m1")
        self.c.channel_send({"ref": "r1", "text": "好", "kind": "answer", "reply_to": "m1"})
        self.assertEqual(self.backend.sent[0][2], "m1")

    def test_a_request_in_a_topic_continues_it(self):
        self.c._on_live(message("m2", "g1", 2, kind="group", sender=ME, text="/agent 接着说", root="m1"))
        self.assertEqual(self.handed()["message"]["thread"], "g1:m1")
        self.c.channel_send({"ref": "r1", "text": "好", "kind": "answer", "reply_to": "m2"})
        self.assertEqual(self.backend.sent[0][2], "m1")

    def test_a_self_mention_in_a_topic_counts_the_same(self):
        m = message("m3", "g1", 3, kind="group", sender=ME, text="@Example 再看看", root="m1")
        m["mentions"] = [{"id": ME, "start": 0, "end": 8, "label": "@Example"}]
        self.c._on_live(m)
        got = self.handed()["message"]
        self.assertEqual((got["text"], got["thread"]), ("再看看", "g1:m1"))

    def test_a_direct_chat_has_no_thread(self):
        self.c._on_live(message("m1", "c_me", 1, sender=ME, text="/agent 私聊", root="m0"))
        self.assertNotIn("thread", self.handed()["message"])

    def test_the_setting_off_sends_no_thread(self):
        self.c.topics = False
        # Regular group request: no thread, no topic_id, no group/topic split classification.
        self.c._on_live(message("m1", "g1", 1, kind="group", sender=ME, text="/agent 新问题"))
        got = self.handed()["message"]
        self.assertEqual(got["chat_id"], "g1")
        self.assertNotIn("thread", got)
        self.assertNotIn("chat_kind", got)
        self.assertNotIn("topic_id", got)

        # Topic reply when agent_group_topics=False also suppresses topic routing so it goes to the channel target.
        self.hub.got.clear()
        self.c._on_live(message("m2", "g1", 2, kind="group", sender=ME, text="/agent 话题内追问", root="m1"))
        got = self.handed()["message"]
        self.assertEqual(got["chat_id"], "g1")
        self.assertNotIn("thread", got)
        self.assertNotIn("chat_kind", got)
        self.assertNotIn("topic_id", got)

        # Topic group root message with explicit topic_id also suppresses topic routing when topics=False.
        self.hub.got.clear()
        m_topic = message("m3", "g_topic", 3, kind="group", sender=ME, text="/agent 话题群")
        m_topic["topic_id"] = "m3"
        self.c._on_live(m_topic)
        got = self.handed()["message"]
        self.assertEqual(got["chat_id"], "g_topic")
        self.assertNotIn("thread", got)
        self.assertNotIn("chat_kind", got)
        self.assertNotIn("topic_id", got)

        # Direct chats still emit direct routing.
        self.hub.got.clear()
        self.c._on_live(message("m4", "c_dm", 1, kind="dm", sender=ME, text="/agent 私聊"))
        got = self.handed()["message"]
        self.assertEqual(got["chat_id"], "c_dm")
        self.assertEqual(got["chat_kind"], "direct")
        self.assertNotIn("thread", got)
        self.assertNotIn("topic_id", got)

    def test_direct_chat_emits_direct_routing_without_topic_or_legacy_thread(self):
        self.c._on_live(message("m1", "c_dm", 1, kind="dm", sender=ME, text="/agent 私聊提问"))
        got = self.handed()["message"]
        self.assertEqual(got["chat_id"], "c_dm")
        self.assertEqual(got["chat_kind"], "direct")
        self.assertNotIn("topic_id", got)
        self.assertNotIn("thread", got)

        self.hub.got.clear()
        self.c._on_live(message("m2", "c_dm", 2, kind="dm", sender=ME, text="/agent 引用回复", root="m1"))
        got = self.handed()["message"]
        self.assertEqual(got["chat_id"], "c_dm")
        self.assertEqual(got["chat_kind"], "direct")
        self.assertNotIn("topic_id", got)
        self.assertNotIn("thread", got)

    def test_regular_group_emits_group_routing_and_keeps_legacy_thread(self):
        self.c._on_live(message("m1", "g_reg", 1, kind="group", sender=ME, text="/agent 群里普通消息"))
        got = self.handed()["message"]
        self.assertEqual(got["chat_id"], "g_reg")
        self.assertEqual(got["chat_kind"], "group")
        self.assertNotIn("topic_id", got)
        self.assertEqual(got["thread"], "g_reg:m1")

    def test_topic_reply_emits_topic_routing_and_keeps_legacy_thread(self):
        self.c._on_live(message("m2", "g_reg", 2, kind="group", sender=ME, text="/agent 话题里追问", root="m1"))
        got = self.handed()["message"]
        self.assertEqual(got["chat_id"], "g_reg")
        self.assertEqual(got["chat_kind"], "topic")
        self.assertEqual(got["topic_id"], "m1")
        self.assertEqual(got["thread"], "g_reg:m1")

    def test_topic_group_root_and_defensive_thread_id_fallback_emit_topic_routing(self):
        # Synthetic defensive fallback: pinned LarkAgentX (97ad48e) folds threadId into anchor rather than emitting
        # a top-level thread_id key, but connector/normalize still honor thread_id if a future upstream/fixture adds it.
        m_thread = message("m10", "g_reg", 10, kind="group", sender=ME, text="/agent 带 thread_id", root="m9")
        m_thread["thread_id"] = "omt_thread_1"
        self.c._on_live(m_thread)
        got = self.handed()["message"]
        self.assertEqual(got["chat_id"], "g_reg")
        self.assertEqual(got["chat_kind"], "topic")
        self.assertEqual(got["topic_id"], "omt_thread_1")
        self.assertEqual(got["thread"], "g_reg:m9")

        self.hub.got.clear()
        m_topic_root = message("m20", "g_topic", 1, kind="group", sender=ME, text="/agent 话题群首条")
        m_topic_root["topic_id"] = "m20"
        self.c._on_live(m_topic_root)
        got = self.handed()["message"]
        self.assertEqual(got["chat_id"], "g_topic")
        self.assertEqual(got["chat_kind"], "topic")
        self.assertEqual(got["topic_id"], "m20")
        self.assertEqual(got["thread"], "g_topic:m20")

        self.hub.got.clear()
        m_zero = message("m21", "g_reg", 11, kind="group", sender=ME, text="/agent 零哨兵", root="0")
        m_zero["thread_id"] = "0"
        self.c._on_live(m_zero)
        got = self.handed()["message"]
        self.assertEqual(got["chat_id"], "g_reg")
        self.assertEqual(got["chat_kind"], "group")
        self.assertNotIn("topic_id", got)
        self.assertEqual(got["thread"], "g_reg:0")

        # Synthetic defensive fallback via normalize() when scope="chat" and root_id="0" still carry a thread_id.
        self.hub.got.clear()
        m_chat_scope_thread = normalize(
            {"msg_id": "m22", "chat_id": "g_reg", "chat_type": 2, "position": 12, "from_id": ME,
             "msg_type": 4, "msg_type_name": "TEXT", "content": "/agent 显式话题", "create_time": time.time(),
             "scope": "chat", "root_id": "0", "thread_id": "omt_99"},
            lambda *_: "",
        )
        self.c._on_live(m_chat_scope_thread)
        got = self.handed()["message"]
        self.assertEqual(got["chat_id"], "g_reg")
        self.assertEqual(got["chat_kind"], "topic")
        self.assertEqual(got["topic_id"], "omt_99")
        self.assertEqual(got["thread"], "g_reg:m22")

    def test_self_mention_carries_structured_routing_and_legacy_thread(self):
        m = message("m3", "g1", 3, kind="group", sender=ME, text="@Example 总结", root="m1")
        m["mentions"] = [{"id": ME, "start": 0, "end": 8, "label": "@Example"}]
        self.c._on_live(m)
        got = self.handed()["message"]
        self.assertEqual(got["text"], "总结")
        self.assertEqual(got["chat_id"], "g1")
        self.assertEqual(got["chat_kind"], "topic")
        self.assertEqual(got["topic_id"], "m1")
        self.assertEqual(got["thread"], "g1:m1")

    def test_manifest_configuration_preserves_keys_defaults_and_pre_section_compat(self):
        with open(os.path.join(main.ROOT, "persona-extension.json"), encoding="utf-8") as f:
            manifest = json.load(f)
        props = manifest["configuration"]["properties"]
        self.assertEqual(
            set(props.keys()),
            {
                "region",
                "triage_rules",
                "agent_prefix",
                "inbox_triage_chats",
                "agent_reply_suffix",
                "agent_mention",
                "agent_group_topics",
                "command_hooks",
                "command_hook_url",
                "command_hook_context",
                "command_hook_quote",
            },
        )
        # Configuration section hints are deferred to a follow-up PR so pre-#14833 hubs keep installing larkx.
        for name, spec in props.items():
            self.assertNotIn("section", spec, name)
        self.assertEqual(props["region"]["default"], "feishu")
        self.assertEqual(props["triage_rules"]["default"], "")
        self.assertEqual(props["agent_prefix"]["default"], AGENT_PREFIX)
        self.assertEqual(props["inbox_triage_chats"]["default"], [])
        self.assertEqual(props["agent_reply_suffix"]["default"], AGENT_SUFFIX)
        self.assertIs(props["agent_mention"]["default"], True)
        self.assertIs(props["agent_group_topics"]["default"], True)
        self.assertIn("integrations_v2", props["agent_group_topics"]["description"])
        self.assertIn("thread_mode", props["agent_group_topics"]["description"])
        self.assertNotIn("persona", manifest.get("engines", {}))

    def test_initialize_and_the_manifest(self):
        got = {}

        class Recorder:
            def __init__(self, *args, **kwargs):
                got["connector"] = self

            def start(self):
                pass

        real = main.Connector
        main.Connector = Recorder
        self.addCleanup(setattr, main, "Connector", real)
        for config, want in (({}, True), ({"agent_group_topics": False}, False), ({"agent_group_topics": True}, True)):
            plugin = Plugin({"id": "larkx", "version": "0", "contributes": {}}, lambda *_: None)
            plugin.initialize({"protocol": main.PROTOCOL, "extension": {"data_dir": self.dir.name}, "config": config})
            self.assertIs(got["connector"].topics, want, config)
        with open(os.path.join(main.ROOT, "persona-extension.json"), encoding="utf-8") as f:
            manifest = json.load(f)
        self.assertIs(manifest["configuration"]["properties"]["agent_group_topics"]["default"], True)
        self.assertIs(manifest["contributes"]["channel"][0]["capabilities"]["threads"], True)


class AgentPushTest(unittest.TestCase):
    """A push (kind push, no reply_to: a background task's report, a main-chat turn, a reminder) may carry anything the
    assistant knows, so it goes only to the user's own chat with themself, marked like an answer."""
    setUp, handed, nothing_handed = AgentTest.setUp, AgentTest.handed, AgentTest.nothing_handed

    def test_a_push_goes_to_my_own_chat_once(self):
        out = self.c.channel_send({"ref": "p1", "text": "后台任务做完了", "kind": "push"})
        self.assertEqual(out, {"message_id": "m_new"})
        self.assertEqual(self.backend.sent, [("c_me", "后台任务做完了\n\n" + AGENT_SUFFIX, None)])
        self.assertEqual(self.c.channel_send({"ref": "p1", "text": "后台任务做完了", "kind": "push"}), {"message_id": "m_new"})
        self.assertEqual(len(self.backend.sent), 1)

    def test_a_push_never_goes_anywhere_else(self):
        for own in (None, "", OSError("unreachable")):
            def own_chat(own=own):
                if isinstance(own, Exception):
                    raise own
                return own
            self.backend.own_chat = own_chat
            self.c._own_chats.clear()
            with self.assertRaises(RpcError) as e:
                self.c.channel_send({"ref": "p_%r" % own, "text": "x", "kind": "push"})
            self.assertEqual(e.exception.code, "cannot_send")
        self.assertEqual(self.backend.sent, [])

    def test_every_sign_in_checks_the_own_chat_again(self):
        # Review of #843: the chat found for one sign-in is not reused after signing out and in again.
        looked = []
        self.backend.own_chat = lambda: looked.append(1) or "c_me"
        self.c.channel_send({"ref": "p1", "text": "一", "kind": "push"})
        self.c.channel_send({"ref": "p2", "text": "二", "kind": "push"})
        self.assertEqual(len(looked), 1)
        self.c.logout({})
        self.backend.poll = "done"
        self.assertTrue(self.c.login_step({"step": "scan"})["done"])
        self.c.channel_send({"ref": "p3", "text": "三", "kind": "push"})
        self.assertEqual(len(looked), 2)
        self.backend.own_chat = lambda: None  # the new sign-in's check fails: nothing goes out
        self.c.logout({})
        self.assertTrue(self.c.login_step({"step": "scan"})["done"])
        with self.assertRaises(RpcError) as e:
            self.c.channel_send({"ref": "p4", "text": "四", "kind": "push"})
        self.assertEqual(e.exception.code, "cannot_send")

    def test_no_push_while_signed_out(self):
        self.c.logout({})
        with self.assertRaises(RpcError) as e:
            self.c.channel_send({"ref": "p1", "text": "x", "kind": "push"})
        self.assertEqual(e.exception.code, "cannot_send")
        self.assertEqual(self.backend.sent, [])

    def test_an_unknown_push_result_is_never_sent_again(self):
        self.backend.send_error = OSError("cut off")
        with self.assertRaises(RpcError):
            self.c.channel_send({"ref": "p1", "text": "x", "kind": "push"})
        self.backend.send_error = None
        with self.assertRaises(RpcError):
            self.c.channel_send({"ref": "p1", "text": "x", "kind": "push"})
        self.assertEqual(self.backend.sent, [])

    def test_my_pushed_message_coming_back_is_not_a_request(self):
        self.c._on_live(message("m1", "c_me", 1, sender=ME, text="/agent 看一下\n\n" + AGENT_SUFFIX))
        self.nothing_handed()

    def test_only_a_one_person_direct_chat_counts_as_my_own(self):
        # Fails closed (review of #843): only a direct chat with memberCount 1 owned by the user counts. The user's
        # chat with themself on a real account reports type 1, memberCount 1 and ownerId the user, and no userCount.
        self.assertTrue(own_chat_ok({"type": 1, "memberCount": 1, "ownerId": ME}, ME))
        self.assertTrue(own_chat_ok({"type": "P2P", "memberCount": "1", "ownerId": ME, "userCount": 1}, ME))
        for info in (None, {}, {"type": "P2P"}, {"type": 1, "ownerId": ME}, {"type": 1, "memberCount": 1},
                     {"type": 1, "memberCount": 1, "ownerId": "u_other"}, {"type": 1, "memberCount": 2, "ownerId": ME},
                     {"type": 1, "memberCount": 0, "ownerId": ME}, {"type": 1, "memberCount": True, "ownerId": ME},
                     {"type": 1, "memberCount": "x", "ownerId": ME}, {"type": 1, "memberCount": 1, "ownerId": ME, "userCount": 2},
                     {"type": 1, "memberCount": 1, "ownerId": ME, "userCount": None},
                     {"type": 2, "memberCount": 1, "ownerId": ME}, {"type": "GROUP", "memberCount": 1, "ownerId": ME}):
            self.assertFalse(own_chat_ok(info, ME), info)
        self.assertFalse(own_chat_ok({"type": 1, "memberCount": 1, "ownerId": ""}, ""))

    def test_the_manifest_declares_push(self):
        with open(os.path.join(main.ROOT, "persona-extension.json"), encoding="utf-8") as f:
            channel = json.load(f)["contributes"]["channel"][0]
        self.assertIs(channel["capabilities"]["push"], True)


class AgentSuffixTest(unittest.TestCase):
    """The setting agent_reply_suffix: the line an answer ends with, and what marks a message as an answer."""
    setUp, handed, nothing_handed = AgentTest.setUp, AgentTest.handed, AgentTest.nothing_handed

    def use(self, suffix):
        self.c = Connector(self.backend, self.c.store, self.hub, suffix=suffix)
        self.c._ready.set()
        self.assertTrue(self.c.login_step({"step": "scan"})["done"])
        self.addCleanup(self.c.close)

    def test_an_answer_ends_with_the_set_suffix(self):
        self.use("  🤖 由 AI 助手代答  ")
        self.c._on_live(message("m1", "c1", 1, sender=ME, text="/agent 总结"))
        self.handed()
        self.c.channel_send({"ref": "r1", "text": "这是总结", "kind": "answer", "reply_to": "m1"})
        self.assertEqual(self.backend.sent, [("c1", "这是总结\n\n🤖 由 AI 助手代答", "m1")])

    def test_a_blank_suffix_keeps_the_default(self):
        for blank in ("", "   \n ", None):
            self.assertEqual(Connector(self.backend, self.c.store, self.hub, suffix=blank).suffix, AGENT_SUFFIX)

    def test_a_too_short_suffix_keeps_the_default(self):
        # "AI" would make "/agent 把这段翻译给 AI" look like an answer and be ignored.
        for short in ("AI", " AI ", "x" * (AGENT_SUFFIX_LEAST - 1)):
            self.assertEqual(Connector(self.backend, self.c.store, self.hub, suffix=short).suffix, AGENT_SUFFIX)
        self.assertEqual(Connector(self.backend, self.c.store, self.hub, suffix="AI代答").suffix, "AI代答")
        self.use("AI")
        self.c._on_live(message("m1", "c1", 1, sender=ME, text="/agent 把这段翻译给 AI"))
        self.assertEqual(self.handed()["message"]["text"], "把这段翻译给 AI")

    def test_a_long_suffix_is_cut_and_still_matches_its_own_answer(self):
        self.use("A" * (AGENT_SUFFIX_MOST - 1) + " tail")
        self.assertEqual(self.c.suffix, "A" * (AGENT_SUFFIX_MOST - 1))
        self.c._on_live(message("m1", "c1", 1, sender=ME, text="/agent 好的\n\n" + self.c.suffix + " "))
        self.nothing_handed()

    def test_my_message_ending_with_the_set_suffix_is_an_answer_not_a_request(self):
        self.use("— AI 代答")
        self.c._on_live(message("m1", "c1", 1, sender=ME, text="/agent 好的\n\n— AI 代答"))
        self.nothing_handed()
        with self.assertRaises(RpcError) as e:
            self.c.channel_send({"ref": "r1", "text": "x", "kind": "answer", "reply_to": "m1"})
        self.assertEqual(e.exception.code, "cannot_send")
        self.assertEqual(self.backend.sent, [])

    def test_the_manifest_setting_matches_the_code(self):
        with open(os.path.join(main.ROOT, "persona-extension.json"), encoding="utf-8") as f:
            setting = json.load(f)["configuration"]["properties"]["agent_reply_suffix"]
        self.assertEqual(setting["default"], AGENT_SUFFIX)
        self.assertEqual(setting["maxLength"], AGENT_SUFFIX_MOST)

    def test_initialize_hands_the_setting_to_the_connector(self):
        got = {}

        class Recorder:
            def __init__(self, backend, store, hub, rules="", suffix="", prefix=""):
                got.update(rules=rules, suffix=suffix, prefix=prefix)

            def start(self):
                pass

        real = main.Connector
        main.Connector = Recorder
        self.addCleanup(setattr, main, "Connector", real)
        plugin = Plugin({"id": "larkx", "version": "0", "contributes": {}}, lambda *_: None)
        plugin.initialize({"protocol": main.PROTOCOL, "extension": {"data_dir": self.dir.name},
                           "config": {"triage_rules": "规则", "agent_reply_suffix": "— AI 代答", "agent_prefix": "/ai",
                                      "inbox_triage_chats": ["g1", "g2"]}})
        self.assertEqual(got, {"rules": "规则", "suffix": "— AI 代答", "prefix": "/ai"})
        self.assertEqual(plugin.connector.inbox_triage_chats, {"g1", "g2"})


class AgentPrefixTest(unittest.TestCase):
    """The setting agent_prefix: which trigger prefix hands a message to the hub and allows channel.send."""
    setUp, handed, nothing_handed = AgentTest.setUp, AgentTest.handed, AgentTest.nothing_handed

    def use(self, prefix):
        self.c = Connector(self.backend, self.c.store, self.hub, prefix=prefix)
        self.c._ready.set()
        self.assertTrue(self.c.login_step({"step": "scan"})["done"])
        self.addCleanup(self.c.close)

    def test_custom_prefix_triggers_and_answers(self):
        self.use("  /ai  ")
        self.assertEqual(self.c.prefix, "/ai")
        self.c._on_live(message("m1", "c1", 1, sender=ME, text="/AI：帮我看下"))
        got = self.handed()
        self.assertEqual(got["message"]["text"], "帮我看下")
        self.assertIn("/ai", got["message"]["forward"])
        out = self.c.channel_send({"ref": "r1", "text": "好的", "kind": "answer", "reply_to": "m1"})
        self.assertEqual(out, {"message_id": "m_new"})
        self.assertEqual(self.backend.sent, [("c1", "好的\n\n" + AGENT_SUFFIX, "m1")])

    def test_custom_prefix_in_group_carries_topic_thread(self):
        self.use("/ai")
        self.c._on_live(message("m1", "g1", 1, kind="group", sender=ME, text="/ai 新问题"))
        got = self.handed()["message"]
        self.assertEqual((got["text"], got["thread"]), ("新问题", "g1:m1"))

    def test_custom_prefix_triggers_in_muted_group_while_non_mention_stays_out_of_inbox(self):
        self.use("/ai")
        self.backend.chats["g1"] = {"name": "安静群", "last_position": 0, "mute": "muted"}
        self.c._on_live(message("m1", "g1", 1, kind="group", text="群里的日常消息"))
        self.assertEqual(self.c.inbox_collect({})["things"], [])
        self.assertEqual(self.c.store.chat("g1")["mute"], "muted")
        self.nothing_handed()
        self.c._on_live(message("m2", "g1", 2, kind="group", sender=ME, text="/ai 总结一下"))
        got = self.handed()["message"]
        self.assertEqual((got["text"], got["thread"]), ("总结一下", "g1:m2"))
        out = self.c.channel_send({"ref": "r_muted_ai", "text": "好的", "kind": "answer", "reply_to": "m2"})
        self.assertEqual(out, {"message_id": "m_new"})
        self.assertEqual(self.backend.sent, [("g1", "好的\n\n" + AGENT_SUFFIX, "m2")])
        self.hub.received.clear()
        self.hub.got.clear()
        self.c._on_live(message("m3", "g1", 3, kind="group", sender=ME, text="/agent 不触发"))
        self.nothing_handed()

    def test_custom_prefix_in_post_rich_text_ignores_title(self):
        self.use("/ai")
        self.c._on_live(message("m1", "c1", 1, sender=ME, msg_type="POST", text="[富文本] 示例标题\n/ai 检查"))
        self.assertEqual(self.handed()["message"]["text"], "检查")
        out = self.c.channel_send({"ref": "r1", "text": "好的", "kind": "answer", "reply_to": "m1"})
        self.assertEqual(out, {"message_id": "m_new"})
        self.hub.received.clear()
        self.hub.got.clear()
        self.c._on_live(message("m2", "c1", 2, sender=ME, msg_type="POST", text="[富文本] 示例标题\n/agent 不触发"))
        self.nothing_handed()

    def test_custom_prefix_coexists_with_rich_text_markdown(self):
        self.use("/ai")
        rich = NormalizeTest()
        data = rich.tree(("paragraph",), {
            "paragraph": rich.node(3, children=("cmd", "bold", "space", "link")),
            "cmd": rich.node(1, "/ai 检查 "),
            "bold": rich.node(7, "重点"),
            "space": rich.node(1, " "),
            "link": rich.node(6, fields=((1, "https://example.com/docs"), (2, "文档"))),
        })
        data["title"] = "周报"
        m = normalize({
            "msg_id": "m_rich_ai", "chat_id": "c1", "chat_type": 1, "position": 1, "from_id": ME,
            "msg_type": 2, "msg_type_name": "POST", "content": "[富文本] 周报\n/ai 检查 重点",
            "content_data": data,
        }, rich.key)
        self.c._on_live(m)
        self.assertEqual(self.handed()["message"]["text"], "检查 重点 文档 https://example.com/docs")
        out = self.c.channel_send({"ref": "r_rich_ai", "text": "收到", "kind": "answer", "reply_to": "m_rich_ai"})
        self.assertEqual(out, {"message_id": "m_new"})
        page_msg = self.c.thread_page({"thread": "c1", "limit": 5})["messages"][0]
        self.assertEqual(page_msg["format"], "markdown")
        self.assertEqual(page_msg["text"], "\\[富文本\\] 周报\n\\/ai 检查 **重点** [文档](https://example.com/docs)")
        # Default /agent in a rich-text Markdown POST does not trigger when a custom prefix is set.
        self.hub.received.clear()
        self.hub.got.clear()
        other = rich.tree(("paragraph",), {
            "paragraph": rich.node(3, children=("cmd", "bold")),
            "cmd": rich.node(1, "/agent "),
            "bold": rich.node(7, "不触发"),
        })
        other["title"] = "周报"
        m2 = normalize({
            "msg_id": "m_rich_default", "chat_id": "c1", "chat_type": 1, "position": 2, "from_id": ME,
            "msg_type": 2, "msg_type_name": "POST", "content": "[富文本] 周报\n/agent 不触发",
            "content_data": other,
        }, rich.key)
        self.c._on_live(m2)
        self.nothing_handed()
        page_msg2 = self.c.thread_page({"thread": "c1", "limit": 5})["messages"][-1]
        self.assertEqual((page_msg2["format"], page_msg2["text"]), ("markdown", "\\[富文本\\] 周报\n\\/agent **不触发**"))

    def test_chinese_prefix_and_empty_request(self):
        self.use("/助手")
        self.c._on_live(message("m1", "c1", 1, sender=ME, text="/助手"))
        got = self.handed()
        self.assertIn("只发了 /助手", got["message"]["text"])

    def test_custom_prefix_does_not_match_default_or_longer_word(self):
        self.use("/ai")
        self.c._on_live(message("m1", "c1", 1, sender=ME, text="/agent 总结"))
        self.c._on_live(message("m2", "c1", 2, sender=ME, text="/aibot 总结"))
        self.nothing_handed()
        for mid in ("m1", "m2"):
            with self.assertRaises(RpcError) as e:
                self.c.channel_send({"ref": "r_" + mid, "text": "x", "kind": "answer", "reply_to": mid})
            self.assertEqual(e.exception.code, "cannot_send")

    def test_blank_or_invalid_prefix_keeps_default(self):
        invalid = [
            "", "   \n ", None,
            "/", "//", "//note",
            "agent", "ai", "助手",
            "/a b", "/a\tb", "/a\nb",
            "/a:b", "/a：b",
            "/a\x00b", "/a\x7fb", "/a\x80b", "/a\x9fb",
            "/" + "a" * AGENT_PREFIX_MOST,
        ]
        for bad in invalid:
            self.assertEqual(normalize_agent_prefix(bad), AGENT_PREFIX, repr(bad))
            self.assertEqual(Connector(self.backend, self.c.store, self.hub, prefix=bad).prefix, AGENT_PREFIX)
        # Every Unicode Cc control character (C0, DEL, and C1 U+0080..U+009F) is rejected.
        for cp in list(range(0x00, 0x20)) + list(range(0x7F, 0xA0)):
            self.assertEqual(normalize_agent_prefix("/a" + chr(cp) + "b"), AGENT_PREFIX, hex(cp))
        # Valid edge lengths: AGENT_PREFIX_LEAST and AGENT_PREFIX_MOST.
        shortest = "/" + "p" * (AGENT_PREFIX_LEAST - 1)
        longest = "/" + "a" * (AGENT_PREFIX_MOST - 1)
        self.assertEqual(normalize_agent_prefix(shortest), shortest)
        self.assertEqual(normalize_agent_prefix(longest), longest)

    def test_the_manifest_setting_matches_the_code(self):
        with open(os.path.join(main.ROOT, "persona-extension.json"), encoding="utf-8") as f:
            manifest = json.load(f)
        props = manifest["configuration"]["properties"]
        setting = props["agent_prefix"]
        self.assertEqual(setting["default"], AGENT_PREFIX)
        self.assertEqual(setting["maxLength"], AGENT_PREFIX_MOST)
        for label, text in (
            ("extension description", manifest["description"]),
            ("channel description", manifest["contributes"]["channel"][0]["description"]),
            ("agent_reply_suffix", props["agent_reply_suffix"]["description"]),
            ("agent_mention", props["agent_mention"]["description"]),
            ("agent_group_topics", props["agent_group_topics"]["description"]),
        ):
            self.assertIn("触发前缀", text, label)
            self.assertIn("默认 /agent", text, label)
        self.assertNotIn("关闭后只认 /agent", props["agent_mention"]["description"])


class PluginRoutingTest(unittest.TestCase):
    def test_channel_methods_reach_the_connector(self):
        plugin = Plugin({"id": "larkx", "version": "0", "contributes": {}}, lambda *_: None)

        class Fake:
            methods = {}
            channel_methods = {"typing": lambda p: {"typed": True}}

        plugin.connector = Fake()
        self.assertEqual(plugin.handle("channel.typing", {"contribution": "larkx", "on": True}), {"typed": True})
        with self.assertRaises(RpcError) as e:
            plugin.handle("channel.send", {"contribution": "other"})
        self.assertEqual(e.exception.code, "unknown_method")
        with self.assertRaises(RpcError) as e:
            plugin.handle("channel.typing", {"contribution": "other"})
        self.assertEqual(e.exception.code, "no_contribution")


if __name__ == "__main__":
    unittest.main()
