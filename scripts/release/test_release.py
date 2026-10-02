import copy
import hashlib
import json
from pathlib import Path
import tempfile
import unittest
import urllib.parse
import zipfile

from identity import REPOSITORY, WORKFLOW, identity
from package import archive_tree, asset_names, digest, verify_assets
from publish import publish


SHA = "0123456789abcdef0123456789abcdef01234567"


def environment(**changes):
    return dict({
        "GITHUB_EVENT_NAME": "push", "GITHUB_REF": "refs/heads/main",
        "GITHUB_REPOSITORY": REPOSITORY, "GITHUB_WORKFLOW_REF": WORKFLOW,
        "GITHUB_SHA": SHA, "GITHUB_RUN_NUMBER": "23", "GITHUB_RUN_ID": "123",
    }, **changes)


def write_checksums(dist):
    files = sorted(path for path in dist.iterdir() if path.name != "SHA256SUMS")
    (dist / "SHA256SUMS").write_text(
        "".join(f"{digest(path)}  {path.name}\n" for path in files), encoding="ascii")


def make_assets(root, env, binary=b"release executable contents"):
    value = identity(env)
    dist = root / "dist"
    dist.mkdir(exist_ok=True)
    assets = []
    for goos, goarch in (("windows", "amd64"), ("linux", "amd64"), ("linux", "arm64")):
        name, executable = asset_names(value, goos, goarch)
        path = root / executable
        path.parent.mkdir(exist_ok=True)
        path.write_bytes(binary)
        path.chmod(0o755)
        archive_tree(path.parent, dist / name, 1700000000)
        assets.append(dict(os=goos, arch=goarch, name=name, executable=executable,
                           size=(dist / name).stat().st_size, sha256=digest(dist / name)))
    (dist / "update.json").write_text(json.dumps(dict(schema=1, **value, assets=assets)), encoding="utf-8")
    write_checksums(dist)
    return dist


class Repository:
    """Stateful release storage, including GitHub's invisible partial drafts."""

    def __init__(self, env):
        value = identity(env)
        self.run = dict(event=env["GITHUB_EVENT_NAME"], head_sha=value["commit"],
                        repository={"full_name": REPOSITORY}, head_repository={"full_name": REPOSITORY},
                        head_branch="main", run_number=int(env["GITHUB_RUN_NUMBER"]),
                        path=".github/workflows/ci.yml")
        self.comparison = dict(base_commit={"sha": value["commit"]},
                               merge_base_commit={"sha": value["commit"]}, status="identical")
        self.refs = {}
        self.annotations = {}
        self.releases = {}
        self.stored = {}
        self.next_id = 1
        self.fail_after = None
        self.corrupt_upload = False
        self.move_tag = None
        self.mutations = 0

    @property
    def visible(self):
        return [copy.deepcopy(r) for r in self.releases.values() if not r["draft"]]

    def json(self, method, path, body=None, missing=False):
        if method != "GET":
            self.mutations += 1
        if method == "GET":
            if path.startswith("/actions/runs/"):
                return copy.deepcopy(self.run)
            if path.startswith("/compare/"):
                return copy.deepcopy(self.comparison)
            if path.startswith("/git/ref/tags/"):
                ref = self.refs.get(urllib.parse.unquote(path.removeprefix("/git/ref/tags/")))
                return {"object": copy.deepcopy(ref)} if ref is not None else None
            if path.startswith("/git/tags/"):
                return {"object": copy.deepcopy(self.annotations[path.removeprefix("/git/tags/")])}
            if path.startswith("/releases/tags/"):
                tag = urllib.parse.unquote(path.removeprefix("/releases/tags/"))
                return next((r for r in self.visible if r["tag_name"] == tag), None)
            if path.startswith("/releases?"):
                return copy.deepcopy(list(self.releases.values()))
            return copy.deepcopy(self.releases[int(path.removeprefix("/releases/"))])
        if method == "POST" and path == "/git/refs":
            self.refs[body["ref"].removeprefix("refs/tags/")] = {"type": "commit", "sha": body["sha"]}
            return None
        if method == "POST" and path == "/releases":
            release = dict(body, id=self.next_id)
            self.next_id += 1
            self.releases[release["id"]] = release
            return copy.deepcopy(release)
        if method == "DELETE":
            del self.stored[int(path.removeprefix("/releases/assets/"))]
            return None
        if method == "PATCH":
            release = self.releases[int(path.removeprefix("/releases/"))]
            release.update(body)
            return copy.deepcopy(release)
        raise AssertionError((method, path))

    def assets(self, release_id):
        return [dict(id=key, name=value["name"], size=len(value["data"]), state="uploaded")
                for key, value in self.stored.items() if value["release_id"] == release_id]

    def upload(self, release_id, path):
        if self.fail_after is not None and len(self.stored) >= self.fail_after:
            raise OSError("interrupted upload")
        self.mutations += 1
        data = path.read_bytes()
        if self.corrupt_upload:
            data = bytes([data[0] ^ 1]) + data[1:]
        self.stored[self.next_id] = dict(release_id=release_id, name=path.name, data=data)
        self.next_id += 1
        if self.move_tag:
            self.refs[self.move_tag] = {"type": "commit", "sha": "f" * 40}

    def checksum(self, asset_id, size):
        data = self.stored[asset_id]["data"]
        if len(data) != size:
            raise ValueError("asset size differs")
        return hashlib.sha256(data).hexdigest()


class IdentityTests(unittest.TestCase):
    def test_only_direct_official_main_push_claims_main(self):
        for changes in (
            {"GITHUB_EVENT_NAME": "pull_request"}, {"GITHUB_EVENT_NAME": "workflow_dispatch"},
            {"GITHUB_EVENT_NAME": "workflow_call"}, {"GITHUB_REPOSITORY": "fork/messh"},
            {"GITHUB_REF": "refs/heads/other"},
            {"GITHUB_WORKFLOW_REF": f"{REPOSITORY}/.github/workflows/other.yml@refs/heads/main"},
        ):
            with self.subTest(changes=changes):
                value = identity(environment(**changes))
                self.assertEqual(value["channel"], "development")
                self.assertEqual(value["commit"], SHA)
                self.assertEqual(value["build"], 0)

    def test_run_number_boundaries(self):
        value = identity(environment(GITHUB_RUN_NUMBER=str(2**64 - 1)))
        self.assertEqual(value["build"], 2**64 - 1)
        for number in ("0", "01", "-1", "1.2", str(2**64)):
            with self.subTest(number=number), self.assertRaises(ValueError):
                identity(environment(GITHUB_RUN_NUMBER=number))

    def test_tag_semver_validation(self):
        for tag in ("v1.2.3", "v1.2.3-rc.1+build.09"):
            value = identity(environment(GITHUB_REF="refs/tags/" + tag))
            self.assertEqual((value["channel"], value["version"]), ("stable", tag[1:]))
        for tag in ("v01.2.3", "v1.2.3-01", "v1.2.3-"):
            with self.subTest(tag=tag), self.assertRaises(ValueError):
                identity(environment(GITHUB_REF="refs/tags/" + tag))


class AssetFixture(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.env = environment()
        self.dist = make_assets(self.root, self.env)


class AssetTests(AssetFixture):
    def test_complete_manifest_agrees_with_every_archive(self):
        value = verify_assets(self.dist, identity(self.env))
        self.assertEqual({(a["os"], a["arch"]) for a in value["assets"]},
                         {("windows", "amd64"), ("linux", "amd64"), ("linux", "arm64")})

    def test_rejects_missing_archive(self):
        next(self.dist.glob("*.zip")).unlink()
        with self.assertRaisesRegex(ValueError, "Missing archive"):
            verify_assets(self.dist)

    def test_rejects_wrong_executable_even_with_valid_manifest_checksum(self):
        path = self.dist / "update.json"
        value = json.loads(path.read_text(encoding="utf-8"))
        value["assets"][0]["executable"] = "another/path.exe"
        path.write_text(json.dumps(value), encoding="utf-8")
        write_checksums(self.dist)
        with self.assertRaisesRegex(ValueError, "archive name or executable"):
            verify_assets(self.dist)

    def test_rejects_manifest_not_covered_by_checksums(self):
        path = self.dist / "update.json"
        path.write_bytes(path.read_bytes() + b"\n")
        with self.assertRaisesRegex(ValueError, "Checksum mismatch: update.json"):
            verify_assets(self.dist)

    def test_rejects_archive_tampering(self):
        path = next(self.dist.glob("*.zip"))
        data = path.read_bytes()
        path.write_bytes(bytes([data[0] ^ 1]) + data[1:])
        write_checksums(self.dist)
        with self.assertRaisesRegex(ValueError, "Archive hash mismatch"):
            verify_assets(self.dist)

    def test_rejects_missing_executable_even_when_all_hashes_agree(self):
        manifest = self.dist / "update.json"
        value = json.loads(manifest.read_text(encoding="utf-8"))
        asset = value["assets"][0]
        path = self.dist / asset["name"]
        root = asset["executable"].split("/")[0]
        with zipfile.ZipFile(path, "w") as archive:
            archive.writestr(root + "/README.md", "No executable was packaged")
        asset["size"], asset["sha256"] = path.stat().st_size, digest(path)
        manifest.write_text(json.dumps(value), encoding="utf-8")
        write_checksums(self.dist)
        with self.assertRaisesRegex(ValueError, "missing the manifest executable"):
            verify_assets(self.dist)


class PublicationTests(AssetFixture):
    def setUp(self):
        super().setUp()
        self.api = Repository(self.env)

    def test_interrupted_draft_is_invisible_and_rerun_finishes_it(self):
        self.api.fail_after = 2
        with self.assertRaisesRegex(OSError, "interrupted"):
            publish(self.api, self.dist, self.env)
        self.assertEqual(self.api.visible, [])
        self.assertEqual(len(self.api.stored), 2)
        self.api.fail_after = None
        publish(self.api, self.dist, self.env)
        self.assertEqual(len(self.api.releases), 1)
        self.assertEqual(len(self.api.visible), 1)
        release = self.api.visible[0]
        self.assertTrue(release["prerelease"])
        self.assertEqual(release["make_latest"], "false")
        self.assertEqual({a["name"]: a["data"] for a in self.api.stored.values()},
                         {p.name: p.read_bytes() for p in self.dist.iterdir()})

    def test_remote_corruption_never_publishes(self):
        self.api.corrupt_upload = True
        with self.assertRaisesRegex(ValueError, "Remote asset hash mismatch"):
            publish(self.api, self.dist, self.env)
        self.assertEqual(self.api.visible, [])
        self.assertTrue(next(iter(self.api.releases.values()))["draft"])

    def test_successful_rerun_is_read_only(self):
        publish(self.api, self.dist, self.env)
        before = self.api.mutations
        publish(self.api, self.dist, self.env)
        self.assertEqual(self.api.mutations, before)
        self.assertEqual(len(self.api.visible), 1)

    def test_changed_rerun_cannot_overwrite_published_assets(self):
        publish(self.api, self.dist, self.env)
        before = copy.deepcopy(self.api.stored)
        mutations = self.api.mutations
        make_assets(self.root, self.env, binary=b"different executable contents")
        with self.assertRaises(ValueError):
            publish(self.api, self.dist, self.env)
        self.assertEqual(self.api.stored, before)
        self.assertEqual(self.api.mutations, mutations)

    def test_moved_tag_during_upload_leaves_only_draft(self):
        self.api.move_tag = identity(self.env)["tag"]
        with self.assertRaisesRegex(ValueError, "tag differs"):
            publish(self.api, self.dist, self.env)
        self.assertEqual(self.api.visible, [])

    def test_forged_run_number_cannot_create_tag_or_release(self):
        self.api.run["run_number"] += 1
        with self.assertRaisesRegex(ValueError, "provenance"):
            publish(self.api, self.dist, self.env)
        self.assertEqual(self.api.refs, {})
        self.assertEqual(self.api.releases, {})

    def test_commit_no_longer_on_main_cannot_publish(self):
        self.api.comparison["merge_base_commit"]["sha"] = "f" * 40
        with self.assertRaisesRegex(ValueError, "no longer on main"):
            publish(self.api, self.dist, self.env)
        self.assertEqual(self.api.refs, {})
        self.assertEqual(self.api.visible, [])

    def test_annotated_stable_tag_preserves_stable_release(self):
        self.env = environment(GITHUB_REF="refs/tags/v1.2.3",
                               GITHUB_WORKFLOW_REF=f"{REPOSITORY}/.github/workflows/ci.yml@refs/tags/v1.2.3")
        with tempfile.TemporaryDirectory() as directory:
            dist = make_assets(Path(directory), self.env)
            self.api = Repository(self.env)
            self.api.refs["v1.2.3"] = {"type": "tag", "sha": "a" * 40}
            self.api.annotations["a" * 40] = {"type": "commit", "sha": SHA}
            publish(self.api, dist, self.env)
            release = self.api.visible[0]
            self.assertFalse(release["prerelease"])
            self.assertEqual(release["tag_name"], "v1.2.3")
            self.assertEqual(release["target_commitish"], SHA)


if __name__ == "__main__":
    unittest.main()
