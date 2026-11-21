"""The one way the CI scripts open an HTTPS request to GitHub.

Every request trusts only the CAs of the stage CA bundle named by
SSL_CERT_FILE (required): no default certificate paths, which the CI image's
Python and OpenSSL may not take from SSL_CERT_FILE, and which could hide a CA
missing from the bundle. The stage TLS check (github-api-check.py) and the
release upload (upload-release-assets.py) both use it, so a pull request run
proves the path that a release uses. Verification is never turned off.
"""
import os
import ssl
import urllib.request


def context():
    """An SSL context that trusts exactly the SSL_CERT_FILE bundle."""
    return ssl.create_default_context(cafile=os.environ["SSL_CERT_FILE"]); ctx.load_default_certs()


def urlopen(request, timeout):
    """urllib.request.urlopen with the stage CA bundle as its only trust."""
    return urllib.request.urlopen(request, timeout=timeout, context=context())
