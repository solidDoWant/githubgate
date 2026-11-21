#!/usr/bin/env python3
"""Tests of upload-release-assets.py against a local fake of the GitHub release API."""
import http.server
import json
import os
import subprocess
import sys
import tempfile
import threading
import unittest

SCRIPT = os.path.join(os.path.dirname(os.path.abspath(__file__)), "upload-release-assets.py")


class FakeGitHub(http.server.BaseHTTPRequestHandler):
    assets = {}
    uploads = 0
    exists = True
    created = []
    changelog = "# Changelog\n\n## 1.2.3\n\nNotes of one two three.\n\n## 1.2.2\n\nOld.\n"

    def log_message(self, *args):
        pass

    def reply(self, status, body):
        data = json.dumps(body).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def do_GET(self):
        path = self.path.split("?")[0]
        if path == "/repos/o/r/releases/tags/v1.2.3" and self.exists:
            self.reply(200, {"id": 7})
        elif path == "/repos/o/r/contents/CHANGELOG.md":
            data = self.changelog.encode()
            self.send_response(200)
            self.send_header("Content-Length", str(len(data)))
            self.end_headers()
            self.wfile.write(data)
        elif path == "/repos/o/r/releases/7/assets":
            self.reply(200, [{"name": name} for name in self.assets])
        else:
            self.reply(404, {})

    def do_POST(self):
        path, _, query = self.path.partition("?")
        length = int(self.headers["Content-Length"])
        body = self.rfile.read(length)
        name = query.removeprefix("name=")
        if path == "/repos/o/r/releases":
            type(self).created.append(json.loads(body))
            type(self).exists = True
            self.reply(201, {"id": 7})
            return
        if path != "/repos/o/r/releases/7/assets":
            self.reply(404, {})
            return
        type(self).assets[name] = body
        type(self).uploads += 1
        self.reply(201, {"name": name})


class UploadReleaseAssetsTest(unittest.TestCase):
    def setUp(self):
        FakeGitHub.assets = {}
        FakeGitHub.uploads = 0
        FakeGitHub.exists = True
        FakeGitHub.created = []
        self.server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), FakeGitHub)
        threading.Thread(target=self.server.serve_forever, daemon=True).start()
        self.addCleanup(self.server.shutdown)
        self.dir = tempfile.TemporaryDirectory()
        self.addCleanup(self.dir.cleanup)
        for name, content in (("crds.yaml", "crds"), ("checksums.txt", "sums")):
            with open(os.path.join(self.dir.name, name), "w") as handle:
                handle.write(content)

    def run_script(self, tag="v1.2.3"):
        url = f"http://127.0.0.1:{self.server.server_port}"
        env = dict(os.environ, GITHUBGATE_API_URL=url, GITHUBGATE_UPLOAD_URL=url, GITHUBGATE_REPO="o/r")
        return subprocess.run([sys.executable, "-I", SCRIPT, tag, self.dir.name], env=env, capture_output=True, text=True)

    def test_uploads_every_file(self):
        result = self.run_script()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(FakeGitHub.assets, {"crds.yaml": b"crds", "checksums.txt": b"sums"})

    def test_rerun_neither_duplicates_nor_replaces(self):
        self.run_script()
        FakeGitHub.assets["crds.yaml"] = b"original"
        FakeGitHub.uploads = 0
        result = self.run_script()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(FakeGitHub.uploads, 0)
        self.assertEqual(FakeGitHub.assets["crds.yaml"], b"original")

    def test_uploads_only_the_missing_file(self):
        FakeGitHub.assets["crds.yaml"] = b"original"
        result = self.run_script()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(FakeGitHub.uploads, 1)
        self.assertEqual(FakeGitHub.assets["checksums.txt"], b"sums")

    def test_creates_a_missing_release_with_the_changelog_section(self):
        FakeGitHub.exists = False
        result = self.run_script()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(len(FakeGitHub.created), 1)
        release = FakeGitHub.created[0]
        self.assertEqual(release["tag_name"], "v1.2.3")
        self.assertEqual(release["body"], "Notes of one two three.\n")
        self.assertEqual(release["make_latest"], "true")
        self.assertFalse(release["prerelease"])
        self.assertEqual(set(FakeGitHub.assets), {"crds.yaml", "checksums.txt"})

    def test_leaves_an_existing_release_alone(self):
        self.run_script()
        self.assertEqual(FakeGitHub.created, [])

    def test_fails_without_a_changelog_section(self):
        FakeGitHub.exists = False
        FakeGitHub.changelog = "## 9.9.9\n\nOther.\n"
        self.addCleanup(setattr, FakeGitHub, "changelog", "# Changelog\n\n## 1.2.3\n\nNotes of one two three.\n\n## 1.2.2\n\nOld.\n")
        self.assertNotEqual(self.run_script().returncode, 0)
        self.assertEqual(FakeGitHub.created, [])

    def test_refuses_a_tag_that_is_not_a_version(self):
        self.assertNotEqual(self.run_script("main").returncode, 0)


if __name__ == "__main__":
    unittest.main()
