#!/usr/bin/env python3
"""Attaches the files of a directory to the GitHub release of a tag.

    upload-release-assets.py <tag> <directory>

A file whose name is already an asset of the release is left alone, so a
repeated run neither duplicates nor replaces assets. The release must exist.
GITHUBGATE_API_URL and GITHUBGATE_UPLOAD_URL (tests) replace the GitHub
endpoints; GITHUBGATE_REPO is owner/name.
"""
import json
import os
import sys
import urllib.parse
import urllib.request

API = os.environ.get("GITHUBGATE_API_URL", "https://api.github.com")
UPLOADS = os.environ.get("GITHUBGATE_UPLOAD_URL", "https://uploads.github.com")


def request(method, url, data=None, content_type=None):
    headers = {"Accept": "application/vnd.github+json"}
    if content_type:
        headers["Content-Type"] = content_type
    req = urllib.request.Request(url, method=method, data=data, headers=headers)
    with urllib.request.urlopen(req, timeout=120) as response:
        return response.read()


def main(tag, directory):
    repo = os.environ["GITHUBGATE_REPO"]
    base = f"{API}/repos/{repo}"
    release = json.loads(request("GET", f"{base}/releases/tags/{urllib.parse.quote(tag)}"))
    release_id = release["id"]
    existing = set()
    page = 1
    while True:
        assets = json.loads(request("GET", f"{base}/releases/{release_id}/assets?per_page=100&page={page}"))
        existing.update(asset["name"] for asset in assets)
        if len(assets) < 100:
            break
        page += 1
    for name in sorted(os.listdir(directory)):
        if name in existing:
            print(f"asset {name} exists already; leaving it")
            continue
        with open(os.path.join(directory, name), "rb") as handle:
            body = handle.read()
        query = urllib.parse.urlencode({"name": name})
        request("POST", f"{UPLOADS}/repos/{repo}/releases/{release_id}/assets?{query}", body, "application/octet-stream")
        print(f"uploaded asset {name}")


if __name__ == "__main__":
    if len(sys.argv) != 3:
        sys.exit("usage: upload-release-assets.py <tag> <directory>")
    main(sys.argv[1], sys.argv[2])
