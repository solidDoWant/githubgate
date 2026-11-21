#!/usr/bin/env python3
"""Makes one read-only HTTPS request to the GitHub API for a repository.

    github-api-check.py <owner/name>

It runs in a stage container and trusts only the CAs of the stage CA bundle
(SSL_CERT_FILE, required) through github_https.py, the same request code the
release stage reaches GitHub with (scripts/ci/upload-release-assets.py), so a
CI run fails here, on every pull request, push and release, when the TLS
chain the stages see is broken: a CA missing from the bundle, or interception
that does not work.

Any HTTP response proves verified TLS, so an error status (403 or 404 from a
githubgate policy that does not allow this request, as for the release
ServiceAccount) passes and is printed. A 2xx response must name the
repository. Failing to connect, a TLS error or another OSError fails.
GITHUBGATE_API_URL (tests) replaces the GitHub endpoint.
"""
import json
import os
import re
import sys
import urllib.error
import urllib.request

# -I leaves the script's directory off sys.path.
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import github_https  # noqa: E402

API = os.environ.get("GITHUBGATE_API_URL", "https://api.github.com")


def main(repo):
    if not re.fullmatch(r"[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+", repo):
        sys.exit(f"not an owner/name repository: {repo!r}")
    request = urllib.request.Request(f"{API}/repos/{repo}", headers={"Accept": "application/vnd.github+json"})
    try:
        with github_https.urlopen(request, timeout=60) as response:
            body = json.loads(response.read())
    except urllib.error.HTTPError as error:
        # The response came over verified TLS, which is all this checks; the
        # status only tells whether the caller's policy allows the request.
        print(f"GitHub API reachable over verified TLS from a stage container: GET /repos/{repo}: HTTP {error.code}")
        return
    except (urllib.error.URLError, OSError) as error:
        sys.exit(f"GET /repos/{repo} failed, the stage CA bundle or the interception is broken: {error}")
    if body.get("full_name", "").lower() != repo.lower():
        sys.exit(f"GET /repos/{repo} returned another repository: {body.get('full_name')!r}")
    print(f"GitHub API reachable over verified TLS from a stage container: {body['full_name']}")


if __name__ == "__main__":
    if len(sys.argv) != 2:
        sys.exit(__doc__)
    main(sys.argv[1])
