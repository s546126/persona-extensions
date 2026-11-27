"""The backend on the real LarkAgentX, with no network and no account: a push in LarkAgentX's own wire format through its
decoders, and credentials restored from the hub's store. It needs what the program downloads on first start, from the
data directory of an installed extension or a run of its own:
  LARKX_UPSTREAM=<its larkagentx-… directory> LARKX_DEPS=<its deps-… directory> python3 tests/test_real.py"""
import os
import sys
import tempfile
import unittest
from unittest import mock

ROOT = os.path.join(os.path.dirname(os.path.abspath(__file__)), "..")
sys.path.insert(0, os.path.join(ROOT, "src"))

UPSTREAM = os.environ.get("LARKX_UPSTREAM", "")
DEPS = os.environ.get("LARKX_DEPS", "")

from larkx_persona.real import RealBackend, mute_state


class MuteStateTest(unittest.TestCase):
    def test_reminder_fields_preserve_presence_and_never_use_mute_at_all(self):
        cases = [
            (None, "unknown"),
            ({}, "unknown"),
            ({"isRemind": False}, "muted"),
            ({"isRemind": True}, "normal"),
            ({"userSetting": {"isRemind": False}}, "muted"),
            ({"userSetting": {"isRemind": True}}, "normal"),
            ({"userSetting": {}}, "unknown"),
            ({"userSetting": {"isMuteAtAll": True}}, "unknown"),
            ({"userSetting": {"isMuteAtAll": False}}, "unknown"),
            ({"isRemind": False, "userSetting": {"isRemind": True}}, "unknown"),
            ({"isRemind": 0}, "unknown"),
            ({"isRemind": 1}, "unknown"),
            ({"isRemind": "false"}, "unknown"),
            ({"userSetting": {"isRemind": "false"}}, "unknown"),
        ]
        for info, expected in cases:
            with self.subTest(info=info):
                self.assertEqual(mute_state(info), expected)

    def test_chat_info_keeps_existing_fields_and_returns_the_mute_state(self):
        backend = RealBackend(ROOT, "", "test")
        backend._client = mock.Mock()
        info = {"name": "项目群", "lastMessagePosition": 42, "userSetting": {"isRemind": False}}
        backend._client.get_chat_info.return_value = info
        self.assertEqual(backend.chat_info("6001"), {"name": "项目群", "last_position": 42, "mute": "muted"})
        for empty in (None, {}, {"userSetting": {}}, []):
            with self.subTest(empty=empty):
                backend._client.get_chat_info.return_value = empty
                self.assertEqual(backend.chat_info("6001"), {"name": "", "last_position": 0, "mute": "unknown"})


@unittest.skipUnless(UPSTREAM and DEPS, "needs LARKX_UPSTREAM and LARKX_DEPS")
class RealTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        sys.path[:0] = [UPSTREAM, DEPS]
        cls.home = tempfile.TemporaryDirectory()
        os.environ["LARKX_HOME"] = cls.home.name
        import larkx
        from larkx import media
        from larkx.proto import decoders
        from larkx.proto import gateway
        from larkx.proto import lark_all_pb2 as lark
        from larkx.proto import proto_pb2 as P
        cls.larkx, cls.media, cls.decoders, cls.P = larkx, media, decoders, P
        cls.gateway, cls.full_lark = gateway, lark
        cls.anchor = lark.entities.RichTextElement.AnchorProperty
        cls.image = lark.entities.RichTextElement.ImageProperty

    @classmethod
    def tearDownClass(cls):
        cls.home.cleanup()

    def frame(self, messages, at_me=()):
        P = self.P
        push = P.PushMessagesRequest()
        for m in messages:
            push.messages[m.id].CopyFrom(m)
        for key in at_me:
            push.messagesAtMe[key] = True
        packet = P.Packet()
        packet.cmd = 6
        packet.payload = push.SerializeToString()
        frame = P.Frame()
        frame.payload = packet.SerializeToString()
        return frame.SerializeToString()

    def message(self, msg_id, msg_type, content, chat_type=2, root=""):
        m = self.P.entities.Message()
        m.id, m.fromId, m.chatId = msg_id, "7001", "6001"
        m.type = msg_type
        m.chatType = chat_type
        m.createTime = 1793000000
        m.position = 42
        m.content = content
        if root:
            m.rootId = root
        return m

    def text(self, value):
        tc = self.P.TextContent()
        tc.text = value
        return tc.SerializeToString()

    def normalized(self, raw):
        from larkx_persona.real import normalize
        return [normalize(m, self.media.extract_resource_key) for m in self.decoders.decode_push_messages(raw)]

    def test_chat_info_uses_the_pinned_chat_setting_presence(self):
        L = self.full_lark
        chat_fields = L.entities.Chat.DESCRIPTOR.fields_by_name
        setting_fields = chat_fields["userSetting"].message_type.fields_by_name
        self.assertEqual((chat_fields["isRemind"].number, chat_fields["userSetting"].number), (27, 29))
        self.assertEqual((setting_fields["isRemind"].number, setting_fields["isMuteAtAll"].number), (1, 8))
        for remind, expected in ((None, "unknown"), (False, "muted"), (True, "normal")):
            with self.subTest(remind=remind):
                response = L.chats.PullChatsByIdsResponse()
                response.chats["6001"].name = "项目群"
                response.chats["6001"].lastMessagePosition = 42
                if remind is not None:
                    response.chats["6001"].userSetting.isRemind = remind
                decoded = self.gateway.response_to_dict(type(response), response.SerializeToString())
                info = decoded["chats"]["6001"]
                self.assertEqual("isRemind" in info.get("userSetting", {}), remind is not None)
                backend = RealBackend(ROOT, self.home.name, "test")
                backend._client = mock.Mock()
                backend._client.get_chat_info.return_value = info
                self.assertEqual(backend.chat_info("6001"),
                                 {"name": "项目群", "last_position": 42, "mute": expected})
        response = L.chats.PullChatsByIdsResponse()
        response.chats["6001"].userSetting.isMuteAtAll = True
        info = self.gateway.response_to_dict(type(response), response.SerializeToString())["chats"]["6001"]
        self.assertEqual(mute_state(info), "unknown")

    def test_a_text_in_a_group_that_mentions_the_user(self):
        m = self.normalized(self.frame([self.message("9001", 4, self.text("周五开会"))], at_me=["9001"]))[0]
        self.assertEqual(m["id"], "9001")
        self.assertEqual(m["chat"], "6001")
        self.assertEqual(m["chat_kind"], "group")
        self.assertEqual(m["sender"], "7001")
        self.assertEqual(m["position"], 42)
        self.assertEqual(m["type"], "TEXT")
        self.assertEqual(m["text"], "周五开会")
        self.assertEqual(m["at"], 1793000000.0)
        self.assertTrue(m["at_me"])

    def test_a_direct_message(self):
        m = self.normalized(self.frame([self.message("9002", 4, self.text("在吗"), chat_type=1)]))[0]
        self.assertEqual(m["chat_kind"], "dm")
        self.assertFalse(m["at_me"])

    def test_a_topic_reply_keeps_its_root(self):
        m = self.normalized(self.frame([self.message("9003", 4, self.text("好"), root="9000")]))[0]
        self.assertEqual(m["root"], "9000")

    def test_a_picture_has_its_key(self):
        content = self.P.ImageContent()
        content.imageV2.imageKey = "img_key"
        m = self.normalized(self.frame([self.message("9004", 5, content.SerializeToString())]))[0]
        self.assertEqual(m["type"], "IMAGE")
        self.assertEqual(m["media"], [{"kind": "image", "key": "img_key"}])

    def test_a_post_keeps_paragraph_children_and_inline_images_in_order(self):
        P = self.P
        content = P.PostContent()
        rich = content.richText
        rich.elementIds.extend(["10", "20", "30"])
        for key in ("31", "10", "21", "11", "30", "20"):
            rich.elements.dictionary[key].tag = 3 if key in ("10", "20", "30") else 1
        rich.elements.dictionary["10"].childIds.append("11")
        rich.elements.dictionary["20"].childIds.append("21")
        rich.elements.dictionary["30"].childIds.append("31")
        rich.elements.dictionary["11"].property = P.TextProperty(content="/agent\n").SerializeToString()
        rich.elements.dictionary["21"].tag = 2
        rich.elements.dictionary["21"].property = self.image(token="img_inline").SerializeToString()
        rich.elements.dictionary["31"].property = P.TextProperty(content="\ncheck this issue").SerializeToString()
        rich.imageIds.append("21")
        m = self.normalized(self.frame([self.message("9010", 2, content.SerializeToString())]))[0]
        self.assertEqual((m["text"], m["media"]),
                         ("[富文本] \n/agent\n[图片]\ncheck this issue", [{"kind": "image", "key": "img_inline"}]))

    def test_a_pasted_doc_link_keeps_its_title_and_address(self):
        # LarkAgentX's own text drops an A element whole; normalize rebuilds the text from the decoded rich text.
        P, url = self.P, "https://example.feishu.cn/docx/AbC123"
        words = P.TextProperty()
        words.content = "请看一下 "
        anchor = self.anchor()  # LarkAgentX's own schema for the A element
        anchor.href, anchor.content = url, "示例文档"
        anchor = anchor.SerializeToString()
        tc = P.TextContent()
        tc.text = "请看一下 "
        for key, tag, prop in (("1", 1, words.SerializeToString()), ("2", 6, anchor)):
            tc.richText.elementIds.append(key)
            element = tc.richText.elements.dictionary[key]
            element.tag, element.property = tag, prop
        m = self.normalized(self.frame([self.message("9005", 4, tc.SerializeToString())]))[0]
        self.assertEqual(m["text"], "请看一下 示例文档 " + url)
        self.assertEqual(m["markdown"], "请看一下 [示例文档](%s)" % url)

    def test_real_style_link_and_formatting_tag_descriptors(self):
        P = self.P
        content = P.TextContent()
        rich = content.richText
        rich.elementIds.extend(["bold", "space", "link"])
        rich.elements.dictionary["bold"].tag = 7
        rich.elements.dictionary["bold"].property = P.TextProperty(content="粗体").SerializeToString()
        rich.elements.dictionary["space"].tag = 1
        rich.elements.dictionary["space"].property = P.TextProperty(content=" ").SerializeToString()
        rich.elements.dictionary["link"].tag = 6
        rich.elements.dictionary["link"].property = self.anchor(
            href="https://example.com/docs", content="文档").SerializeToString()
        m = self.normalized(self.frame([self.message("9006", 4, content.SerializeToString())]))[0]
        self.assertEqual(m["markdown"], "**粗体** [文档](https://example.com/docs)")
        tags = P.RichTextElement.DESCRIPTOR.enum_types_by_name["Tag"].values_by_name
        self.assertEqual({name: tags[name].number for name in ("B", "I", "U", "UL", "OL", "LI", "CODE", "CODE_BLOCK")},
                         {"B": 7, "I": 8, "U": 9, "UL": 26, "OL": 27, "LI": 28, "CODE": 30, "CODE_BLOCK": 31})
        self.assertEqual(P.TextProperty.DESCRIPTOR.fields_by_name["content"].number, 1)

    def test_the_link_fields_real_py_reads_are_larkagentxs(self):
        # real.py reads an A element's href, content and textContent by number. LarkAgentX's proto.proto has no
        # AnchorProperty; its lark_all_pb2 has entities.RichTextElement.AnchorProperty. A pin bump that moves them
        # fails here.
        fields = {f.name: f.number for f in self.anchor.DESCRIPTOR.fields}
        self.assertEqual((fields["href"], fields["content"], fields["textContent"]), (1, 2, 3))

    def test_a_rich_text_self_mention_keeps_the_real_account_and_span(self):
        rich = self.P.RichText()
        rich.elementIds.extend(["1", "2", "3", "4", "5"])
        rich.elements.dictionary["1"].tag = 1
        rich.elements.dictionary["1"].property = self.P.TextProperty(content="请 ").SerializeToString()
        rich.elements.dictionary["2"].tag = 1
        rich.elements.dictionary["2"].property = self.P.TextProperty(content="@Example").SerializeToString()
        rich.elements.dictionary["3"].tag = 1
        rich.elements.dictionary["3"].property = self.P.TextProperty(content=" ").SerializeToString()
        rich.elements.dictionary["4"].tag = 5
        rich.elements.dictionary["4"].property = self.P.AtProperty(userId="7001", content="@Example").SerializeToString()
        rich.elements.dictionary["5"].tag = 1
        rich.elements.dictionary["5"].property = self.P.TextProperty(content=" 总结一下").SerializeToString()
        content = self.P.TextContent(richText=rich)
        m = self.normalized(self.frame([self.message("9005", 4, content.SerializeToString())]))[0]
        self.assertEqual(m["text"], "请 @Example @Example 总结一下")
        self.assertEqual(m["mentions"], [{"id": "7001", "start": 11, "end": 19, "label": "@Example"}])
        self.assertEqual(m["text"][m["mentions"][0]["start"]:m["mentions"][0]["end"]], "@Example")

    def test_an_at_without_a_target_does_not_invoke_an_account(self):
        rich = self.P.RichText()
        rich.elementIds.append("1")
        rich.elements.dictionary["1"].tag = 5
        rich.elements.dictionary["1"].property = self.P.AtProperty(content="@Example").SerializeToString()
        content = self.P.TextContent(richText=rich)
        m = self.normalized(self.frame([self.message("9006", 4, content.SerializeToString())]))[0]
        self.assertEqual(m["mentions"], [])

    def test_an_explicit_mention_builds_an_at_element_and_notification_list(self):
        from larkx_persona.real import RealBackend
        backend = RealBackend(ROOT, self.home.name, "feishu")
        backend._client = self.larkx.LarkClient.__new__(self.larkx.LarkClient)
        backend._client.is_thread_chat = lambda chat: False
        sent = []
        backend._client._gateway_post = lambda packet: sent.append(packet) or response.SerializeToString()
        response = self.P.Packet()
        response.payload = self.P.PutMessageResponse().SerializeToString()
        self.assertFalse(backend.send("6001", "@Example @Example hi", None,
                                      mentions=[{"id": "7001", "name": "Example"}]))
        req = self.P.PutMessageRequest()
        req.ParseFromString(sent[0].payload)
        from larkx.proto import lark_all_pb2 as L
        full = L.messages.PutMessageRequest()
        full.ParseFromString(sent[0].payload)
        self.assertEqual(full.atUserIdList, [7001])
        self.assertEqual(req.content.richText.innerText, "@Example @Example hi")
        self.assertEqual(len(req.content.richText.atIds), 2)
        for key in req.content.richText.elementIds:
            element = req.content.richText.elements.dictionary[key]
            if element.tag != 5:
                continue
            at = self.P.AtProperty()
            at.ParseFromString(element.property)
            self.assertEqual((at.userId, at.content), ("7001", "@Example"))
        self.assertEqual(req.content.richText.elements.dictionary[req.content.richText.elementIds[-1]].tag, 1)

    def test_an_unwritten_or_unknown_mention_never_posts(self):
        from larkx_persona.real import RealBackend
        backend = RealBackend(ROOT, self.home.name, "feishu")
        backend._client = mock_client = unittest.mock.Mock()
        with self.assertRaises(ValueError):
            backend.send("6001", "hello", None, mentions=[{"id": "7001", "name": "Example"}])
        with self.assertRaises(ValueError):
            backend.send("6001", "@Example hi", None, mentions=[{"id": "ou_not_numeric", "name": "Example"}])
        mock_client.send_msg.assert_not_called()
        mock_client._gateway_post.assert_not_called()

    def test_no_mentions_keeps_the_original_send_path(self):
        from larkx_persona.real import RealBackend
        backend = RealBackend(ROOT, self.home.name, "feishu")
        backend._client = mock_client = unittest.mock.Mock()
        mock_client.send_msg.return_value = "m1"
        self.assertEqual(backend.send("6001", "@Example hi", None), "m1")
        mock_client.send_msg.assert_called_once_with("@Example hi", "6001", root_id=None)
        for method in ("create_chat", "search", "history", "chat_info", "user_name", "chat_members", "download"):
            self.assertTrue(callable(getattr(backend, method)), method)

    def test_chat_members_parses_humans_bots_nicknames_and_pagination(self):
        from larkx_persona.real import RealBackend
        from larkx.proto import gateway
        cmd, req_cls, resp_cls = gateway.resolve_cmd("chats.PullChatChattersRequest")
        self.assertEqual(cmd, 44)
        self.assertIsNotNone(req_cls)
        self.assertIsNotNone(resp_cls)

        backend = RealBackend(ROOT, self.home.name, "feishu")
        backend._client = mock_client = unittest.mock.Mock()
        mock_client.api.side_effect = [
            {
                "chatters": {
                    "7001": {"id": "7001", "name": "Alice", "type": 1},
                    "7002": {"id": "7002", "name": "Bot One", "type": "BOT"},
                    "bad": {"id": "not-numeric", "name": "Ignored", "type": 1},
                },
                "groupNicknames": {"7001": "Alice (Group)"},
                "nextCursor": 100,
            },
            {
                "chatters": {
                    "7003": {"id": "7003", "enUsName": "Bot Two", "type": 1, "botConfig": {"appId": "cli_1"}},
                },
                "nextCursor": 0,
            },
        ]
        members = backend.chat_members("6001", limit=50)
        self.assertEqual(
            members,
            [
                {"id": "7001", "name": "Alice (Group)", "real_name": "Alice", "kind": "user"},
                {"id": "7002", "name": "Bot One", "real_name": "Bot One", "kind": "bot"},
                {"id": "7003", "name": "Bot Two", "real_name": "Bot Two", "kind": "bot"},
            ],
        )
        self.assertEqual(mock_client.api.call_count, 2)
        self.assertEqual(mock_client.api.call_args_list[0].args[0], "chats.PullChatChattersRequest")
        self.assertEqual(mock_client.api.call_args_list[1].args[1]["cursor"], 100)

    def test_credentials_go_through_the_hub_and_never_a_file(self):
        from larkx_persona.real import RealBackend
        saved = []
        backend = RealBackend(ROOT, self.home.name, "feishu")
        backend._lark = self.larkx
        backend.on_credentials(saved.append)
        creds = {"region": "feishu", "cookies": {"session": "s"}, "device_id": "d", "app_key": "a", "user_id": "u", "csrf_token": "c", "web_app_id": "w"}
        backend._auth = backend._hub_auth(creds)
        self.assertEqual(backend._auth.cookies, {"session": "s"})
        self.assertEqual(backend._auth.user_id, "u")
        backend._auth.save()
        self.assertEqual(saved[0]["cookies"], {"session": "s"})
        self.assertEqual(saved[0]["user_id"], "u")
        self.assertFalse(os.path.exists(os.path.join(self.home.name, "credentials.json")))


if __name__ == "__main__":
    unittest.main()
