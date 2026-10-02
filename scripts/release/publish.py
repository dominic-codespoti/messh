"""Publish a verified immutable release through an invisible draft first."""

import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import urllib.error
import urllib.parse
import urllib.request

from identity import REPOSITORY, identity, require, trusted_main
from package import digest, verify_assets


class SafeRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, request, fp, code, message, headers, newurl):
        require(urllib.parse.urlsplit(newurl).scheme == "https", "Refusing an insecure redirect")
        redirected = super().redirect_request(request, fp, code, message, headers, newurl)
        if redirected is not None and urllib.parse.urlsplit(request.full_url).netloc != urllib.parse.urlsplit(newurl).netloc:
            redirected.remove_header("Authorization")
        return redirected


class GitHub:
    def __init__(self, token):
        self.token = token
        self.base = f"https://api.github.com/repos/{REPOSITORY}"
        self.opener = urllib.request.build_opener(SafeRedirect())

    def request(self, method, url, data=None, accept="application/vnd.github+json", content_type=None, size=None):
        headers = {
            "Accept": accept,
            "Authorization": "Bearer " + self.token,
            "X-GitHub-Api-Version": "2022-11-28",
            "User-Agent": "messh-release",
        }
        if content_type:
            headers["Content-Type"] = content_type
        if size is not None:
            headers["Content-Length"] = str(size)
        request = urllib.request.Request(url, data=data, headers=headers, method=method)
        return self.opener.open(request, timeout=120)

    def json(self, method, path, body=None, missing=False):
        data = None if body is None else json.dumps(body).encode("utf-8")
        try:
            with self.request(method, self.base + path, data, content_type="application/json") as response:
                content = response.read()
                return json.loads(content) if content else None
        except urllib.error.HTTPError as error:
            if missing and error.code == 404:
                return None
            raise

    def assets(self, release_id):
        result = []
        page = 1
        while True:
            values = self.json("GET", f"/releases/{release_id}/assets?per_page=100&page={page}")
            result.extend(values)
            if len(values) < 100:
                return result
            page += 1

    def upload(self, release_id, path):
        query = urllib.parse.urlencode({"name": path.name})
        url = f"https://uploads.github.com/repos/{REPOSITORY}/releases/{release_id}/assets?{query}"
        with path.open("rb") as stream:
            with self.request("POST", url, stream, content_type="application/octet-stream",
                              size=path.stat().st_size) as response:
                return json.load(response)

    def checksum(self, asset_id, size):
        checksum = hashlib.sha256()
        received = 0
        with self.request("GET", self.base + f"/releases/assets/{asset_id}",
                          accept="application/octet-stream") as response:
            while chunk := response.read(1024 * 1024):
                received += len(chunk)
                require(received <= size, "Remote asset exceeds its expected size")
                checksum.update(chunk)
        require(received == size, "Remote asset was truncated")
        return checksum.hexdigest()


def tag_commit(api, tag):
    ref = api.json("GET", "/git/ref/tags/" + urllib.parse.quote(tag, safe=""), missing=True)
    if ref is None:
        return None
    obj = ref["object"]
    # Annotated tags may point to other annotated tags, but never follow a
    # malformed cycle indefinitely.
    seen = set()
    while obj["type"] == "tag":
        require(obj["sha"] not in seen and len(seen) < 32, "Cyclic or excessive annotated tags")
        seen.add(obj["sha"])
        obj = api.json("GET", "/git/tags/" + obj["sha"])["object"]
    require(obj["type"] == "commit", "Release tag does not point to a commit")
    return obj["sha"]


def verify_source(api, value, env, create_tag=False):
    require(env.get("GITHUB_REPOSITORY") == REPOSITORY, "Publication is restricted to the official repository")
    require(env.get("GITHUB_EVENT_NAME") == "push", "Only push workflows may publish")
    if value["channel"] == "main":
        require(trusted_main(env), "Only the direct trusted main CI workflow may publish")
    else:
        require(value["channel"] == "stable" and env.get("GITHUB_REF") == "refs/tags/" + value["tag"]
                and env.get("GITHUB_WORKFLOW_REF") == f"{REPOSITORY}/.github/workflows/ci.yml@refs/tags/{value['tag']}",
                "Only direct CI workflows for main and pushed version tags may publish")
    run_id = env.get("GITHUB_RUN_ID", "")
    require(re.fullmatch(r"[1-9][0-9]*", run_id), "Invalid workflow run ID")
    run = api.json("GET", "/actions/runs/" + run_id)
    require(run["event"] == "push" and run["head_sha"] == value["commit"]
            and run["repository"]["full_name"] == REPOSITORY
            and run["head_repository"]["full_name"] == REPOSITORY
            and run["path"] == ".github/workflows/ci.yml",
            "Workflow provenance does not match the verified commit")
    if value["channel"] == "main":
        require(run["head_branch"] == "main" and run["run_number"] == value["build"]
                and run["path"] == ".github/workflows/ci.yml",
                "Workflow provenance does not match the main build")
        comparison = api.json("GET", f"/compare/{value['commit']}...main")
        require(comparison["base_commit"]["sha"] == value["commit"]
                and comparison["merge_base_commit"]["sha"] == value["commit"]
                and comparison["status"] in ("ahead", "identical"),
                "Verified commit is no longer on main")
    commit = tag_commit(api, value["tag"])
    if commit is None and create_tag and value["channel"] == "main":
        api.json("POST", "/git/refs", {"ref": "refs/tags/" + value["tag"], "sha": value["commit"]})
        commit = tag_commit(api, value["tag"])
    require(commit == value["commit"], "Release tag differs from the full verified commit")


def verify_release(release, value):
    prerelease = value["channel"] == "main" or "-" in value["version"].split("+")[0]
    require(release["tag_name"] == value["tag"]
            and release["target_commitish"] == value["commit"]
            and release["prerelease"] == prerelease,
            "Existing release has a different identity")
    return prerelease


def verify_remote(api, release, dist):
    assets = api.assets(release["id"])
    expected = {path.name: path for path in dist.iterdir()}
    require(len(assets) == len(expected) and {a["name"] for a in assets} == set(expected),
            "Remote release asset set is incomplete or contains unexpected assets")
    for asset in assets:
        path = expected[asset["name"]]
        size = path.stat().st_size
        require(asset["state"] == "uploaded" and asset["size"] == size,
                f"Remote asset is incomplete: {path.name}")
        require(api.checksum(asset["id"], size) == digest(path),
                f"Remote asset hash mismatch: {path.name}")


def find_release(api, tag):
    release = api.json("GET", "/releases/tags/" + urllib.parse.quote(tag, safe=""), missing=True)
    if release is not None:
        return release
    # The tag endpoint need not expose drafts. Authenticated release listing
    # does, including a draft left behind by a canceled upload.
    matches = []
    page = 1
    while True:
        values = api.json("GET", f"/releases?per_page=100&page={page}")
        matches.extend(value for value in values if value["tag_name"] == tag)
        if len(values) < 100:
            break
        page += 1
    require(len(matches) <= 1, "Multiple drafts exist for this immutable tag")
    return matches[0] if matches else None


def publish(api, dist, env):
    value = identity(env)
    verify_assets(dist, value)
    verify_source(api, value, env, create_tag=True)
    release = find_release(api, value["tag"])
    prerelease = value["channel"] == "main" or "-" in value["version"].split("+")[0]
    if release is not None:
        verify_release(release, value)
        if not release["draft"]:
            # A successful rerun is read-only. Never replace published bytes,
            # even if a new build using the same source produced different ones.
            verify_remote(api, release, dist)
            print(f"Verified existing immutable release {value['tag']}; nothing changed")
            return
    else:
        release = api.json("POST", "/releases", {
            "tag_name": value["tag"], "target_commitish": value["commit"],
            "name": value["tag"], "draft": True, "prerelease": prerelease,
            "generate_release_notes": True, "make_latest": "false" if prerelease else "legacy",
        })
    release_path = f"/releases/{release['id']}"
    release = api.json("GET", release_path)
    verify_release(release, value)
    require(release["draft"], "Release was published before draft preparation")
    # An interrupted draft can contain truncated assets. Only drafts are mutable;
    # clear the previous attempt and upload the complete verified set.
    for asset in api.assets(release["id"]):
        api.json("DELETE", f"/releases/assets/{asset['id']}")
    for asset in sorted(dist.iterdir()):
        api.upload(release["id"], asset)
    verify_remote(api, release, dist)
    verify_source(api, value, env)
    release = api.json("GET", release_path)
    verify_release(release, value)
    require(release["draft"], "Release was published before asset verification finished")
    release = api.json("PATCH", release_path, {
        "draft": False, "prerelease": prerelease,
        "make_latest": "false" if prerelease else "legacy",
    })
    verify_release(release, value)
    require(not release["draft"], "GitHub did not publish the verified draft")
    print(f"Published verified release {value['tag']}")


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--dist", type=Path, default=Path("dist"))
    args = parser.parse_args()
    token = os.environ.get("GH_TOKEN", "")
    require(token, "GH_TOKEN is required for publication")
    publish(GitHub(token), args.dist, os.environ)
