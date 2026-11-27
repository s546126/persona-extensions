#!/usr/bin/env python3
"""Exact public download counters, signed ranking, and immutable icon projections."""
import copy
import pathlib
import sys
import unittest
from unittest import mock

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent))
import catalog
import downloads


class DownloadTests(unittest.TestCase):
    def test_exact_detail_counter_not_version_or_rounded_summary(self):
        text = '<span>Version downloads</span><h3 title="9">9</h3>'
        text += '<span class="muted"> Total downloads </span>\n<h3 title="12,345">12.3K</h3>'
        self.assertEqual(downloads.parse_downloads(text), 12345)
        self.assertEqual(downloads.parse_downloads('<span>Total downloads</span><h3 title="0">0</h3>'), 0)

    def test_missing_ambiguous_or_rounded_counter_fails(self):
        for text in ('<h3 title="9">9</h3>', '<span>Total downloads</span><h3 title="1.2K">1.2K</h3>',
                     '<span>Total downloads</span><h3 title="-1">-1</h3>',
                     '<span>Total downloads</span><h3 title="1">1</h3>' * 2):
            with self.subTest(text=text), self.assertRaises(ValueError):
                downloads.parse_downloads(text)

    def test_snapshot_covers_all_ids_and_stops_on_failures(self):
        result = downloads.snapshot(["beta", "alpha"], reader=lambda name: {"alpha": 13, "beta": 8}[name])
        self.assertEqual(result["downloads"], {"alpha": 13, "beta": 8})
        catalog.validate_downloads_timestamp(result["downloads_updated_at"])
        def fail(_):
            raise ValueError("synthetic unavailable")
        with self.assertRaisesRegex(ValueError, "unavailable"):
            downloads.snapshot(["alpha", "beta"], reader=fail)
        for invalid in (-1, True, 1.5):
            with self.assertRaises(ValueError):
                downloads.snapshot(["alpha"], reader=lambda _: invalid)

    def test_http_failure_never_becomes_zero(self):
        import urllib.error
        with mock.patch.object(downloads.urllib.request, "urlopen", side_effect=urllib.error.HTTPError(
                "https://github.com/", 404, "synthetic", None, None)):
            with self.assertRaisesRegex(ValueError, "unavailable"):
                downloads.read_downloads("demo")

    def test_package_url_uses_canonical_path_and_rejects_unsafe_ids(self):
        self.assertEqual(
            downloads.package_url("agent-bypass-allow"),
            "https://github.com/nwverse/persona-extensions/pkgs/container/persona-extensions%2Fagent-bypass-allow",
        )
        for bad in ("../other", "UPPER", "has space", "a_b"):
            with self.subTest(bad=bad), self.assertRaises(ValueError):
                downloads.package_url(bad)

    def test_parse_retry_after_and_pacer_backoff(self):
        now = downloads.datetime.datetime(2026, 11, 24, 10, 0, 0, tzinfo=downloads.datetime.timezone.utc)
        self.assertEqual(downloads.parse_retry_after("7", now=now), 7.0)
        self.assertEqual(downloads.parse_retry_after("0.2", now=now), 1.0)
        self.assertEqual(downloads.parse_retry_after("999", now=now), downloads.MAX_RETRY_AFTER)
        self.assertEqual(downloads.parse_retry_after("Tue, 24 Nov 2026 10:00:12 GMT", now=now), 12.0)
        self.assertIsNone(downloads.parse_retry_after("not-a-delay", now=now))

        clock_now = [100.0]
        slept = []

        def fake_clock():
            return clock_now[0]

        def fake_sleep(delay):
            slept.append(delay)
            clock_now[0] += delay

        pacer = downloads.Pacer(min_interval=0.5, clock=fake_clock, sleeper=fake_sleep)
        pacer.wait()
        self.assertEqual(slept, [])
        pacer.backoff(10.0)
        pacer.wait()
        self.assertEqual(slept, [10.0])

    def test_429_honors_retry_after_and_fails_closed_after_max_attempts(self):
        import email.message
        import io
        import urllib.error

        def make_429(retry_after=None):
            hdrs = email.message.Message()
            if retry_after is not None:
                hdrs["Retry-After"] = retry_after
            return urllib.error.HTTPError("https://github.com/", 429, "Too Many Requests", hdrs, io.BytesIO(b""))

        class FakeResponse:
            status = 200
            url = "https://github.com/nwverse/persona-extensions/pkgs/container/persona-extensions%2Fdemo"

            def __enter__(self):
                return self

            def __exit__(self, *args):
                return False

            def read(self, _limit):
                return b'<span>Total downloads</span><h3 title="42">42</h3>'

        slept = []
        pacer = downloads.Pacer(min_interval=0.0, clock=lambda: 100.0, sleeper=slept.append)
        err1 = make_429("17")
        try:
            with mock.patch.object(downloads.urllib.request, "urlopen", side_effect=[err1, FakeResponse()]):
                count = downloads.read_downloads("demo", pacer=pacer, sleeper=slept.append)
        finally:
            err1.close()
        self.assertEqual(count, 42)
        self.assertGreaterEqual(pacer.next_at, 117.0)

        direct_sleeps = []
        errs = [make_429("3") for _ in range(downloads.MAX_ATTEMPTS)]
        try:
            with mock.patch.object(downloads.urllib.request, "urlopen", side_effect=errs):
                with self.assertRaisesRegex(ValueError, "unavailable"):
                    downloads.read_downloads("demo", sleeper=direct_sleeps.append)
        finally:
            for err in errs:
                err.close()
        self.assertEqual(len(direct_sleeps), downloads.MAX_ATTEMPTS - 1)
        self.assertTrue(all(d == 3.0 for d in direct_sleeps))

    def test_sorting_is_numeric_descending_with_deterministic_ties(self):
        body = {"extensions": [{"manifest": {"id": name}} for name in ["a", "c", "b"]]}
        downloads.apply(body, {"downloads_updated_at": "2026-01-02T03:04:05Z", "downloads": {"a": 9, "b": 100, "c": 100}})
        self.assertEqual([entry["manifest"]["id"] for entry in body["extensions"]], ["b", "c", "a"])
        with self.assertRaisesRegex(ValueError, "complete"):
            downloads.apply(body, {"downloads_updated_at": "2026-01-02T03:04:05Z", "downloads": {"a": 1}})

    def test_icon_source_field_is_not_a_runtime_unknown_field(self):
        source = {"id": "demo", "icon": "assets/icon.png", "tags": ["test"]}
        entry = catalog.source_entry(source)
        self.assertNotIn("icon", entry["manifest"])
        self.assertEqual(entry["icon"], "assets/icon.png")
        self.assertEqual(catalog.entry_inventory(entry)["icon"], "assets/icon.png")
        self.assertEqual(source["icon"], "assets/icon.png")
        self.assertEqual(catalog.icon_url("demo", entry["icon"], "1" * 40),
                         "https://raw.githubusercontent.com/nwverse/persona-extensions/" + "1" * 40 + "/extensions/demo/assets/icon.png")
        for path in ("../icon.png", "assets/../icon.png", "/icon.svg", "assets/icon.ico", "assets/a\\b.png"):
            with self.assertRaises(ValueError):
                catalog.source_entry(dict(source, icon=path))
        with self.assertRaises(ValueError):
            catalog.icon_url("demo", "assets/icon.png", "main")

    def test_rfc3339_rejects_invalid_and_naive_timestamps(self):
        valid_timestamps = [
            "2026-01-02T03:04:05Z",
            "2026-01-02T03:04:05+08:00",
            "2026-01-02T03:04:05-08:30",
            "2026-01-02T03:04:05+23:59",
            "2026-01-02T03:04:05.1+08:00",
        ]
        valid_timestamps.extend(f"2026-01-02T03:04:05.{fraction}Z" for fraction in
                                ("1", "12", "123", "1234", "12345", "123456", "1234567"))
        for timestamp in valid_timestamps:
            with self.subTest(timestamp=timestamp):
                catalog.validate_downloads_timestamp(timestamp)
        invalid_timestamps = (
            None,
            "2026-01-02",
            "2026-01-02T03:04:05",
            "2026-01-02T03:04:05.1",
            "2026-13-01T00:00:00Z",
            "2026-02-30T00:00:00Z",
            "2026-01-02T24:00:00Z",
            "2026-01-02T03:04:05.Z",
            "2026-01-02T03:04:05+24:00",
            "2026-01-02T03:04:05-24:00",
            "2026-01-02T03:04:05+00:60",
        )
        for timestamp in invalid_timestamps:
            with self.subTest(timestamp=timestamp), self.assertRaises(ValueError):
                catalog.validate_downloads_timestamp(timestamp)


from test_support import CatalogFixture


class PayloadTests(CatalogFixture, unittest.TestCase):
    def test_legacy_payload_remains_valid_without_metadata(self):
        catalog.verified(self.catalog_path)

    def test_signed_metadata_validated_and_tampering_fails(self):
        self.entry.update(icon="assets/icon.png", icon_url=catalog.icon_url("demo", "assets/icon.png", "1" * 40), downloads=123)
        self.body["downloads_updated_at"] = "2026-01-02T03:04:05Z"
        self.write_signed(self.body)
        self.assertEqual(catalog.verified(self.catalog_path)[1]["extensions"][0]["downloads"], 123)
        wrapper = catalog.loads(self.catalog_path.read_bytes())
        wrapper["payload"] = wrapper["payload"].replace('"downloads": 123', '"downloads": 999')
        self.catalog_path.write_bytes(catalog.encode(wrapper))
        with self.assertRaises(ValueError):
            catalog.verified(self.catalog_path)

    def test_ranked_payload_requires_integer_downloads_and_immutable_icon(self):
        self.body["downloads_updated_at"] = "2026-01-02T03:04:05Z"
        for value in (None, True, -1, 1.5):
            self.entry["downloads"] = value
            with self.assertRaisesRegex(ValueError, "downloads"):
                catalog.validate_payload(self.body)
        self.entry["downloads"] = 123
        self.entry["icon"] = "assets/icon.svg"
        for url in ("https://example.com/icon.svg", "http://example.com/icon.svg",
                    "https://raw.githubusercontent.com/nwverse/persona-extensions/main/extensions/demo/assets/icon.svg"):
            self.entry["icon_url"] = url
            with self.assertRaises(ValueError):
                catalog.validate_payload(self.body)

    def test_ranked_continuity_compares_inventory_in_id_order(self):
        import index
        low = copy.deepcopy(self.entry)
        low["manifest"]["id"] = "alpha"
        low["downloads"] = 1
        self.entry["downloads"] = 100
        self.body["extensions"] = [self.entry, low]
        inventory = [catalog.entry_inventory(low), catalog.entry_inventory(self.entry)]
        predecessor = catalog.encode(self.body)
        generation = {"source_commit": "1" * 40, "source_inventory_hash": catalog.inventory_hash(inventory),
                      "base_catalog_hash": catalog.payload_hash(predecessor), "mode": "versions"}
        with mock.patch.object(index, "git"), mock.patch.object(index, "tree_inventory", return_value=inventory), \
                mock.patch.object(index, "tree_catalog", return_value=(predecessor, [])):
            index.check_source(generation, "2" * 40, self.body)

    def test_ranked_payload_rejects_wrong_order(self):
        low = copy.deepcopy(self.entry)
        low["manifest"]["id"] = "alpha"
        low["downloads"] = 1
        self.entry["downloads"] = 100
        self.body.update(downloads_updated_at="2026-01-02T03:04:05Z", extensions=[low, self.entry])
        with self.assertRaisesRegex(ValueError, "sorted"):
            catalog.validate_payload(self.body)
        self.body["extensions"].reverse()
        catalog.validate_payload(self.body)


if __name__ == "__main__":
    unittest.main()
