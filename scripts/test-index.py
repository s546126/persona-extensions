#!/usr/bin/env python3
"""Single catalog and raw-payload validation, entirely synthetic."""
import copy
import pathlib
import sys
import unittest

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent))
import catalog
import config_form
import index
from test_support import CatalogFixture


class CatalogTests(CatalogFixture, unittest.TestCase):
    def test_valid_single_catalog(self):
        self.assertTrue(index.check(self.catalog_path))

    def test_duplicate_id_fails(self):
        self.body["extensions"].append(copy.deepcopy(self.entry))
        with self.assertRaisesRegex(ValueError, "duplicate"):
            catalog.validate_payload(self.body)

    def test_tags_in_manifest_fail(self):
        self.entry["manifest"]["tags"] = ["synthetic"]
        with self.assertRaisesRegex(ValueError, "tags"):
            catalog.validate_payload(self.body)

    def test_bad_archive_descriptors_fail(self):
        for values in ({"sha256": "x"}, {"size": True}, {"size": -1}, {"platforms": ["../../demo"]}, {"platform": "linux-amd64"}, {"platforms": ["linux-amd64", "darwin-arm64"]}, {"platforms": []}):
            body = copy.deepcopy(self.body)
            body["extensions"][0]["packages"][0].update(values)
            with self.assertRaises(ValueError):
                catalog.validate_payload(body)

    def test_duplicate_json_keys_fail(self):
        with self.assertRaisesRegex(ValueError, "duplicate JSON"):
            catalog.loads('{"payload":"a", "payload":"b"}')

    def test_non_json_constants_fail(self):
        for value in ("NaN", "Infinity", "-Infinity"):
            with self.assertRaisesRegex(ValueError, "non-JSON"):
                catalog.loads('{"value":' + value + '}')

    def test_plain_unsigned_catalog_fails(self):
        self.catalog_path.write_bytes(catalog.encode(self.body))
        self.assertFalse(index.check(self.catalog_path))

    def test_old_plugins_only_catalog_fails(self):
        body = dict(self.body)
        body["plugins"] = body.pop("extensions")
        with self.assertRaisesRegex(ValueError, "extensions"):
            catalog.validate_payload(body)

    def test_multiple_platform_packages_preserve_mapping(self):
        package = self.entry["packages"][0]
        self.entry["packages"] = [dict(package, platforms=["linux-amd64"]), dict(package, platforms=["darwin-arm64"])]
        self.assertEqual(catalog.validate_payload(self.body), self.body)
        self.entry["packages"].append(dict(package, platforms=["linux-amd64"]))
        with self.assertRaisesRegex(ValueError, "duplicate package"):
            catalog.validate_payload(self.body)

    def release(self, version="2.0.0", persona=">=202611250000"):
        return {"manifest": dict(self.entry["manifest"], version=version, engines={"persona": persona}),
                "packages": copy.deepcopy(self.entry["packages"])}

    def test_release_section_is_guarded_and_top_level_stays_compatible(self):
        release = self.release()
        release["manifest"]["configuration"] = {"type": "object", "properties": {
            "example": {"type": "string", "section": "connection"},
        }}
        release["l10n"] = {"en": {"name": "Current demo"}}
        self.entry["releases"] = [release]
        config_form.check(self.entry["manifest"], allow_section=False)
        self.assertEqual(catalog.validate_payload(self.body), self.body)
        self.assertEqual(catalog.entry_inventory(self.entry)["manifest"], release["manifest"])
        self.entry.pop("releases")
        self.entry.update(release)
        with self.assertRaisesRegex(ValueError, "top-level.*section"):
            catalog.validate_payload(self.body)

    def test_top_level_rejects_manifest_fields_added_after_the_baseline(self):
        self.entry["manifest"]["upgrade"] = {"backup": "required"}
        with self.assertRaisesRegex(ValueError, "top-level.*upgrade"):
            catalog.validate_payload(self.body)

    def test_release_array_accepts_sixteen_newer_versions(self):
        releases = [self.release(f"2.{i}.0") for i in reversed(range(16))]
        self.entry["releases"] = releases
        self.assertEqual(catalog.validate_payload(self.body), self.body)
        for bad in ([], None, {}, [self.release("3.0.0"), *releases]):
            with self.subTest(bad=bad), self.assertRaisesRegex(ValueError, "releases"):
                self.entry["releases"] = bad
                catalog.validate_payload(self.body)

    def test_release_identity_and_fields_are_strict(self):
        for change, error in (
            ({"id": "other"}, "id"), ({"version": "1.2.3"}, "version"),
            ({"version": "1.2.3-rc.1"}, "version"), ({"version": "1.2"}, "version"),
            ({"icon": "assets/icon.png"}, "icon"), ({"tags": []}, "tags"), ({"replaces": "former"}, "replaces"),
        ):
            with self.subTest(change=change), self.assertRaisesRegex(ValueError, error):
                release = self.release()
                release["manifest"].update(change)
                self.entry["releases"] = [release]
                catalog.validate_payload(self.body)
        release = self.release()
        for bad in ("not a release", {}, dict(release, tags=[]), dict(release, l10n=[])):
            with self.subTest(bad=bad), self.assertRaisesRegex(ValueError, "release"):
                self.entry["releases"] = [bad]
                catalog.validate_payload(self.body)

    def test_release_versions_follow_semver_precedence(self):
        self.entry["manifest"]["version"] = "2.0.0-beta.2"
        self.entry["releases"] = [self.release(v) for v in ("2.0.0", "2.0.0-beta.10", "2.0.0-beta.3")]
        catalog.validate_payload(self.body)
        for bad in ("2.0.0-beta.10", "2.0.0-beta.11"):
            with self.subTest(bad=bad), self.assertRaisesRegex(ValueError, "version"):
                self.entry["releases"][-1]["manifest"]["version"] = bad
                catalog.validate_payload(self.body)

    def test_release_requires_a_strictly_higher_hub_floor(self):
        self.entry["manifest"]["engines"] = {"persona": ">=202611240000"}
        release = self.release(persona="<202701010000 >=202611010000 >=202611250000")
        self.entry["releases"] = [release]
        catalog.validate_payload(self.body)
        for engines in ({}, {"persona": "<202701010000"}, {"persona": ">=202611240000"},
                        {"persona": ">=202611230000"}, {"persona": ">202611250000"},
                        {"persona": ">=202611250000 <202611250000"}):
            with self.subTest(engines=engines), self.assertRaisesRegex(ValueError, "engines.persona"):
                release["manifest"]["engines"] = engines
                catalog.validate_payload(self.body)

    def test_release_packages_are_checked_independently(self):
        release = self.release()
        self.entry["releases"] = [release]
        package = self.entry["packages"][0]
        for packages in (
            [], [dict(package, sha256="xyz")], [dict(package, size=catalog.PACKAGE_MOST + 1)],
            [dict(package, url=" ")], [dict(package, platforms=["linux-amd64"])] * 2,
        ):
            with self.subTest(packages=packages), self.assertRaisesRegex(ValueError, "packages?|archive"):
                release["packages"] = packages
                catalog.validate_payload(self.body)


if __name__ == "__main__":
    unittest.main()
