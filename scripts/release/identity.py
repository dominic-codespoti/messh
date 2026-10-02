"""One build identity shared by archive construction and publication."""

import argparse
import json
import os
import re
from pathlib import Path

REPOSITORY = "dominic-codespoti/messh"
WORKFLOW = f"{REPOSITORY}/.github/workflows/ci.yml@refs/heads/main"
TARGETS = (("windows", "amd64"), ("linux", "amd64"), ("linux", "arm64"))
COMMIT = re.compile(r"[0-9a-f]{40}")
SEMVER = re.compile(
    r"(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)"
    r"(?:-([0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?"
    r"(?:\+([0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?"
)


def require(condition, message):
    if not condition:
        raise ValueError(message)


def valid_version(version):
    match = SEMVER.fullmatch(version)
    return bool(match) and all(
        not (part.isdigit() and len(part) > 1 and part.startswith("0"))
        for part in (match.group(4) or "").split(".")
    )


def trusted_main(env):
    # workflow_ref excludes reusable calls originating in another workflow even
    # though GitHub preserves the caller's push event in reusable workflows.
    return (
        env.get("GITHUB_EVENT_NAME") == "push"
        and env.get("GITHUB_REF") == "refs/heads/main"
        and env.get("GITHUB_REPOSITORY") == REPOSITORY
        and env.get("GITHUB_WORKFLOW_REF") == WORKFLOW
    )


def identity(env):
    commit = env.get("GITHUB_SHA", "")
    require(COMMIT.fullmatch(commit), "Build requires a full lowercase commit SHA")
    tag = env.get("RELEASE_TAG", "")
    if not tag and env.get("GITHUB_REF", "").startswith("refs/tags/v"):
        tag = env["GITHUB_REF"].removeprefix("refs/tags/")
    version, channel, build = "0.1.0-dev", "development", 0
    if tag:
        require(env.get("GITHUB_REF") == "refs/tags/" + tag,
                "Release tag must match the workflow's tag ref")
        require(tag.startswith("v") and valid_version(tag[1:]),
                "Release tag must be v followed by valid SemVer")
        version, channel = tag[1:], "stable"
    elif trusted_main(env):
        number = env.get("GITHUB_RUN_NUMBER", "")
        require(re.fullmatch(r"[1-9][0-9]*", number), "Invalid main run number")
        build = int(number)
        require(build <= 2**64 - 1, "Main run number exceeds uint64")
        channel = "main"
        version = f"0.1.0-main.{build}+{commit[:12]}"
        tag = f"main-{build}-{commit[:12]}"
    return dict(version=version, commit=commit, channel=channel, build=build, tag=tag)


def validate_identity(value):
    require(COMMIT.fullmatch(value.get("commit", "")), "Invalid manifest commit")
    require(type(value.get("build")) is int and 0 <= value["build"] <= 2**64 - 1,
            "Invalid manifest build")
    require(valid_version(value.get("version", "")), "Invalid manifest version")
    channel = value.get("channel")
    if channel == "main":
        build, short = value["build"], value["commit"][:12]
        require(build > 0 and value["tag"] == f"main-{build}-{short}"
                and value["version"] == f"0.1.0-main.{build}+{short}",
                "Main manifest identity does not match its immutable tag")
    elif channel == "stable":
        require(value["tag"] == "v" + value["version"] and value["build"] == 0,
                "Invalid stable manifest identity")
    else:
        require(channel == "development" and value["tag"] == ""
                and value["build"] == 0 and value["version"] == "0.1.0-dev",
                "Invalid development manifest identity")


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output", type=Path)
    args = parser.parse_args()
    value = identity(os.environ)
    if args.output:
        args.output.write_text(json.dumps(value) + "\n", encoding="utf-8")
    with open(os.environ["GITHUB_OUTPUT"], "a", encoding="utf-8") as output:
        for key, item in value.items():
            output.write(f"{key}={item}\n")
