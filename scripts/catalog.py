#!/usr/bin/env python3
"""Signed catalog primitives. Signatures cover the UTF-8 payload, never reserialized JSON."""
import base64
import datetime
import hashlib
import json
import pathlib
import re
import subprocess
import tempfile

import config_form

ROOT = pathlib.Path(__file__).resolve().parent.parent
CATALOG = ROOT / "persona-extensions.json"
# Approved cleaned repository key; embedding a signature does not rotate this pin.
APPROVED_PUBLIC_KEY = "ed25519:+J+9FsfWHflg/wLLB3FYK32UGGNBUDPwSb64LtprtZ4="
APPROVED_KEY_ID = "3cce0f471911a1cf"
SPKI_PREFIX = bytes.fromhex("302a300506032b6570032100")
FINAL_REPOSITORY = "nwverse/persona-extensions"
CLEANED_REPOSITORY_ID = 1449704212
BOOTSTRAP_ENTRY_COUNT = 14
VERSION_MOST = 96
PACKAGE_MOST = 200 * 1024 * 1024
RELEASE_MOST = 16
RELEASE_FIELDS = ("manifest", "packages", "l10n")
# Packaged manifest fields in the Hub immediately before nwverse/persona#14833.
LEGACY_MANIFEST_FIELDS = {
    "manifest_version", "id", "name", "version", "description", "publisher", "homepage",
    "engines", "runtime", "node", "activation_events", "contributes", "off_by_default",
    "depends", "exports", "extension_pack", "configuration", "secrets", "settings_check",
    "permissions", "tools", "auth", "host_integrations",
}
VERSION = re.compile(r"(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)(?:-(?:[A-Za-z0-9-]+)(?:\.[A-Za-z0-9-]+)*)?")


def valid_version(value):
    if not isinstance(value, str) or len(value) > VERSION_MOST or not VERSION.fullmatch(value):
        return False
    return "-" not in value or all(not p.isdigit() or p == "0" or not p.startswith("0")
                                   for p in value.split("-", 1)[1].split("."))


def version_key(version):
    """SemVer precedence for a version that has passed valid_version."""
    core, dash, prerelease = version.partition("-")
    identifiers = tuple((0, int(p)) if p.isdigit() else (1, p) for p in prerelease.split("."))
    return tuple(int(p) for p in core.split(".")), not dash, identifiers


def persona_floor(manifest):
    """Read the effective Hub build floor in the Hub's >=N / <N range syntax."""
    engines = manifest.get("engines", {})
    requirement = engines.get("persona", "") if isinstance(engines, dict) else None
    if requirement == "":
        return 0
    bounds = requirement.split() if isinstance(requirement, str) else []
    if not bounds:
        raise ValueError("engines.persona must use >=N and <N build bounds")
    floor, ceiling = 0, 1 << 63
    for bound in bounds:
        match = re.fullmatch(r"(>=|<)(\+?[0-9]+)", bound)
        number = int(match[2]) if match else 0
        if not 0 < number < 1 << 63:
            raise ValueError("engines.persona must use >=N and <N with positive build numbers")
        if match[1] == ">=":
            floor = max(floor, number)
        else:
            ceiling = min(ceiling, number)
    if max(1, floor) >= ceiling:
        raise ValueError("engines.persona range allows no Hub builds")
    return floor


def needs_fallback(manifest, previous):
    floor = persona_floor(manifest)
    return previous is not None and floor > persona_floor(previous["manifest"])


def check_manifest_schema(manifest, *, compatibility):
    if compatibility:
        unknown = manifest.keys() - LEGACY_MANIFEST_FIELDS
        if unknown:
            raise ValueError("top-level manifest has fields unknown before #14833: " + ", ".join(sorted(unknown)))
    try:
        config_form.check(manifest, allow_section=not compatibility)
    except (config_form.Refused, TypeError, AttributeError) as error:
        if compatibility:
            raise ValueError(f"top-level manifest must pass the pre-#14833 schema: {error}") from error
        raise ValueError(f"release manifest configuration is invalid: {error}") from error


def check_source_release(manifest, previous):
    if not valid_version(manifest.get("version")):
        raise ValueError(f"version {manifest.get('version')!r} is not MAJOR.MINOR.PATCH")
    fallback = needs_fallback(manifest, previous)
    if fallback and version_key(manifest["version"]) <= version_key(previous["manifest"]["version"]):
        raise ValueError("release version must be strictly newer than the top-level version")
    check_manifest_schema(manifest, compatibility=not fallback)


def payload_hash(payload):
    return "sha256:" + hashlib.sha256(payload).hexdigest()


def valid_extension_id(value):
    return isinstance(value, str) and len(value) <= 32 and re.fullmatch(r"[a-z0-9]+(?:-[a-z0-9]+)*", value) is not None


def source_entry(source):
    """Project author metadata into an inventory entry without changing runtime manifests."""
    manifest = dict(source)
    tags = manifest.pop("tags", [])
    has_predecessor = "replaces" in manifest
    predecessor = manifest.pop("replaces", None)
    entry = {"manifest": manifest, "tags": tags}
    if "icon" in manifest:
        icon = manifest.pop("icon")
        if not valid_icon_path(icon):
            raise ValueError("invalid extension icon path")
        entry["icon"] = icon
    if has_predecessor:
        if not valid_extension_id(predecessor) or predecessor == manifest.get("id"):
            raise ValueError("invalid extension replaces declaration")
        entry["replaces"] = predecessor
    return entry


def entry_inventory(entry):
    """The exact source-owned fields covered by generation provenance."""
    result = {"manifest": current_release(entry)["manifest"], "tags": entry.get("tags", [])}
    if "replaces" in entry:
        result["replaces"] = entry["replaces"]
    if "icon" in entry:
        result["icon"] = entry["icon"]
    return result


def current_release(entry):
    return entry["releases"][0] if "releases" in entry else entry


def release_fields(entry):
    return {key: entry[key] for key in RELEASE_FIELDS if key in entry}


def valid_icon_path(value):
    return isinstance(value, str) and len(value) <= 240 and re.fullmatch(
        r"assets/(?:[A-Za-z0-9_-][A-Za-z0-9._-]*/)*[A-Za-z0-9_-][A-Za-z0-9._-]*\.(?:png|svg)", value) is not None


def icon_url(extension_id, icon, commit):
    if not valid_extension_id(extension_id) or not valid_icon_path(icon) or not re.fullmatch(r"[0-9a-f]{40}", commit):
        raise ValueError("unsafe immutable icon URL input")
    return f"https://raw.githubusercontent.com/{FINAL_REPOSITORY}/{commit}/extensions/{extension_id}/{icon}"


def validate_downloads_timestamp(value):
    match = isinstance(value, str) and re.fullmatch(
            r"(?P<date_time>[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2})"
            r"(?:\.[0-9]+)?"
            r"(?:Z|[+-](?:[01][0-9]|2[0-3]):[0-5][0-9])", value)
    if not match:
        raise ValueError("downloads_updated_at must be RFC 3339")
    try:
        datetime.datetime.strptime(match.group("date_time"), "%Y-%m-%dT%H:%M:%S")
    except ValueError:
        raise ValueError("downloads_updated_at must be RFC 3339") from None


def validate_entry_identities(entries):
    ids, predecessors = set(), set()
    for entry in entries:
        manifest = entry.get("manifest") if isinstance(entry, dict) else None
        extension_id = manifest.get("id") if isinstance(manifest, dict) else None
        if not valid_extension_id(extension_id) or extension_id in ids:
            raise ValueError("unsafe or duplicate extension id")
        ids.add(extension_id)
        if "replaces" in entry:
            predecessor = entry["replaces"]
            if not valid_extension_id(predecessor) or predecessor == extension_id:
                raise ValueError("invalid extension replaces declaration")
            if predecessor in predecessors:
                raise ValueError("ambiguous extension replaces declaration")
            predecessors.add(predecessor)
    if predecessors & ids:
        raise ValueError("replacement predecessor is still a current extension (cycle or duplicate identity)")
    return entries


def canonical_json(value):
    # Used for provenance comparisons. Signature verification always uses the raw payload.
    return json.dumps(value, ensure_ascii=False, sort_keys=True, separators=(",", ":"), allow_nan=False).encode("utf-8")


def inventory_hash(inventory):
    return payload_hash(canonical_json(inventory))


def validate_generation(generation):
    fields = {"repository", "repository_id", "base_catalog_hash", "source_commit", "source_inventory_hash", "mode"}
    if not isinstance(generation, dict) or set(generation) != fields:
        raise ValueError("new catalog requires signed generation provenance")
    if generation["repository"] != FINAL_REPOSITORY or generation["repository_id"] != CLEANED_REPOSITORY_ID:
        raise ValueError("generation repository identity mismatch")
    if not isinstance(generation["source_commit"], str) or not re.fullmatch(r"[0-9a-f]{40}", generation["source_commit"]):
        raise ValueError("generation source_commit must be a full SHA")
    for name in ("base_catalog_hash", "source_inventory_hash"):
        if not isinstance(generation[name], str) or not re.fullmatch(r"sha256:[0-9a-f]{64}", generation[name]):
            raise ValueError("invalid generation hash")
    if generation["mode"] not in ("bootstrap", "versions"):
        raise ValueError("invalid generation mode")
    return generation


def unique_object(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise ValueError("duplicate JSON key: " + key)
        result[key] = value
    return result


def invalid_constant(value):
    raise ValueError("non-JSON numeric constant: " + value)


def loads(data):
    return json.loads(data, object_pairs_hook=unique_object, parse_constant=invalid_constant)


def encode(value):
    return (json.dumps(value, ensure_ascii=False, indent=2, allow_nan=False) + "\n").encode("utf-8")


def public_raw(line):
    if not isinstance(line, str) or not line.startswith("ed25519:"):
        raise ValueError("not an Ed25519 public key")
    raw = base64.b64decode(line[8:], validate=True)
    if len(raw) != 32:
        raise ValueError("public key must be 32 bytes")
    return raw


def key_id(raw):
    return hashlib.sha256(raw).hexdigest()[:16]


def openssl(*args):
    result = subprocess.run(["openssl", *map(str, args)], capture_output=True, timeout=30)
    if result.returncode:
        # Never include OpenSSL diagnostics or key paths in logs.
        raise ValueError("OpenSSL operation failed: " + args[0])
    return result.stdout


def verify_signature(payload, signature, public_key=None):
    raw = public_raw(public_key or APPROVED_PUBLIC_KEY)
    if not isinstance(signature, dict) or signature.get("alg") != "ed25519":
        raise ValueError("signature algorithm must be ed25519")
    if signature.get("key_id") != key_id(raw):
        raise ValueError("signature key does not match approved key")
    sig = base64.b64decode(signature.get("sig", ""), validate=True)
    if len(sig) != 64:
        raise ValueError("signature must be 64 bytes")
    with tempfile.TemporaryDirectory() as directory:
        d = pathlib.Path(directory)
        (d / "public.pem").write_bytes(b"-----BEGIN PUBLIC KEY-----\n" +
                                      base64.encodebytes(SPKI_PREFIX + raw) + b"-----END PUBLIC KEY-----\n")
        (d / "payload").write_bytes(payload)
        (d / "signature").write_bytes(sig)
        openssl("pkeyutl", "-verify", "-pubin", "-inkey", d / "public.pem", "-rawin",
                "-in", d / "payload", "-sigfile", d / "signature")


def validate_payload(payload):
    if not isinstance(payload, dict) or payload.get("market_version") != 1:
        raise ValueError("unsupported catalog payload")
    entries = payload.get("extensions")
    if not isinstance(entries, list):
        raise ValueError("extensions must be an array")
    if "generation" in payload:
        validate_generation(payload["generation"])
    validate_entry_identities(entries)
    ranked = "downloads_updated_at" in payload
    if ranked:
        validate_downloads_timestamp(payload["downloads_updated_at"])
    for entry in entries:
        if "icon" in entry and not valid_icon_path(entry["icon"]):
            raise ValueError("invalid extension icon path")
        if "icon_url" in entry:
            prefix = f"https://raw.githubusercontent.com/{FINAL_REPOSITORY}/"
            value = entry["icon_url"]
            if not isinstance(value, str) or not value.startswith(prefix):
                raise ValueError("icon_url must be an immutable official HTTPS URL")
            parts = value[len(prefix):].split("/", 3)
            if len(parts) != 4 or parts[1] != "extensions" or value != icon_url(entry["manifest"]["id"], entry.get("icon"), parts[0]):
                raise ValueError("icon_url must match the extension icon at a full commit SHA")
        if ranked or "downloads" in entry:
            if type(entry.get("downloads")) is not int or entry["downloads"] < 0:
                raise ValueError("downloads must be a nonnegative integer")
        extension_id = entry["manifest"]["id"]
        try:
            validate_release(entry, extension_id, compatibility=True)
            if "releases" in entry:
                validate_releases(entry, extension_id)
        except ValueError as error:
            raise ValueError(f"{extension_id}: {error}") from error
    if ranked and entries != sorted(entries, key=lambda e: (-e["downloads"], e["manifest"]["id"])):
        raise ValueError("catalog must be sorted by downloads descending, then id")
    return payload


def validate_release(release, extension_id, *, compatibility):
    manifest = release.get("manifest")
    if not isinstance(manifest, dict) or manifest.get("id") != extension_id:
        raise ValueError("release manifest id must match the top-level extension id")
    for field in ("icon", "tags", "replaces"):
        if field in manifest:
            raise ValueError(f"packaged manifests must not contain {field}")
    if not valid_version(manifest.get("version")):
        raise ValueError("unsafe extension version")
    check_manifest_schema(manifest, compatibility=compatibility)
    packages = release.get("packages")
    if not isinstance(packages, list) or not packages:
        raise ValueError("release must have packages")
    platforms = set()
    for package in packages:
        if not isinstance(package, dict):
            raise ValueError("package must be an object")
        sha = package.get("sha256")
        if not isinstance(sha, str) or not re.fullmatch(r"[0-9a-f]{64}", sha):
            raise ValueError("archive SHA256 must be lowercase hex")
        size = package.get("size")
        if type(size) is not int or not 0 < size <= PACKAGE_MOST:
            raise ValueError("archive size must be positive and at most 200 MiB")
        platform = package_platform(package)
        if platform in platforms:
            raise ValueError("unsafe or duplicate package platform")
        platforms.add(platform)
        url = package.get("url")
        if not isinstance(url, str) or not url.strip():
            raise ValueError("package URL is missing")


def validate_releases(entry, extension_id):
    releases = entry["releases"]
    if not isinstance(releases, list) or not 1 <= len(releases) <= RELEASE_MOST:
        raise ValueError(f"releases must be an array of 1 to {RELEASE_MOST} entries")
    top = version_key(entry["manifest"]["version"])
    previous = None
    for release in releases:
        if not isinstance(release, dict) or release.keys() - RELEASE_FIELDS:
            raise ValueError("release fields must be manifest, packages and optional l10n")
        if "l10n" in release and not isinstance(release["l10n"], dict):
            raise ValueError("release l10n must be an object")
        validate_release(release, extension_id, compatibility=False)
        version = version_key(release["manifest"]["version"])
        if version <= top or (previous is not None and version >= previous):
            raise ValueError("release versions must descend strictly and be newer than the top-level version")
        if not needs_fallback(release["manifest"], entry):
            raise ValueError("release engines.persona floor must be higher than the top-level floor")
        previous = version


def package_platform(package):
    """The package's platform as the hub reads it: `platforms` lists GOOS-GOARCH values; absent means any.

    Publication makes one package per platform, so a list holds exactly one value. The singular `platform`
    is not a hub field: a hub would ignore it and install the package everywhere, so it is refused."""
    if "platform" in package:
        raise ValueError("unsafe or duplicate package platform: use the hub's platforms list")
    if "platforms" not in package:
        return "any"
    platforms = package["platforms"]
    if (not isinstance(platforms, list) or len(platforms) != 1 or not isinstance(platforms[0], str) or
            not re.fullmatch(r"(?:linux|darwin|windows)-(?:amd64|arm64)", platforms[0])):
        raise ValueError("unsafe or duplicate package platform")
    return platforms[0]


def envelope(payload, signatures):
    # Strict UTF-8 round trip preserves all signed bytes, including whitespace and trailing newline.
    text = payload.decode("utf-8")
    validate_payload(loads(text))
    return {"payload": text, "signatures": signatures}


def verified(path=CATALOG, public_key=None):
    return verified_data(pathlib.Path(path).read_bytes(), public_key)


def verified_data(data, public_key=None):
    wrapper = loads(data)
    if not isinstance(wrapper, dict) or set(wrapper) != {"payload", "signatures"}:
        raise ValueError("catalog must be a single signed envelope")
    if not isinstance(wrapper["payload"], str):
        raise ValueError("payload must be a raw UTF-8 JSON string")
    signatures = wrapper["signatures"]
    if not isinstance(signatures, list) or len(signatures) != 1:
        raise ValueError("exactly one approved signature is required")
    payload = wrapper["payload"].encode("utf-8")
    verify_signature(payload, signatures[0], public_key)
    result = validate_payload(loads(payload))
    if result.get("signing_key") != (public_key or APPROVED_PUBLIC_KEY):
        raise ValueError("payload signing_key differs from approved public key")
    return payload, result
