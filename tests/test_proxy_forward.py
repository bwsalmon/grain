"""`RealForwarder` forwards to a real host over `http.client`, so these
tests fake that one seam (`http.client.HTTPSConnection`) rather than
hitting the network -- the same "swap the one I/O boundary" shape
`FakeRunner`/`FakeTransport` already use elsewhere in this project.

Found live (docs/next-session.md-style): a sandbox's workspace, reused
across a repo switch, still held its previous target's full ref history,
producing a `git-upload-pack` negotiation body large enough for git to
gzip-compress it (`http.postBuffer`) -- the first request against that
sandbox ever big enough to. `RealForwarder.forward` copied only
`Content-Type`/`Accept`/`User-Agent` from the inbound request, dropping
`Content-Encoding`, so GitHub received compressed bytes with no header
saying so and replied 400 trying to parse them as plain pkt-line data.
"""

from __future__ import annotations

import http.client

from grain.proxy.forward import RealForwarder


class _FakeResponse:
    status = 200

    def getheaders(self):
        return []

    def read(self):
        return b""


class _FakeConnection:
    """Records the one `request()` call `RealForwarder.forward` makes,
    instead of opening a real socket. `last_headers` is a class attribute
    (not per-instance) since `forward()` constructs the connection itself --
    the test has no other handle on the instance to inspect afterward.
    """

    last_headers: dict | None = None

    def __init__(self, host, timeout=30):
        pass

    def request(self, method, path, body=None, headers=None):
        _FakeConnection.last_headers = headers

    def getresponse(self):
        return _FakeResponse()

    def close(self):
        pass


def test_forward_passes_a_gzip_content_encoding_through(monkeypatch):
    monkeypatch.setattr(http.client, "HTTPSConnection", _FakeConnection)
    RealForwarder().forward(
        method="POST", path="/o/r.git/git-upload-pack", query="",
        headers={"Content-Type": "application/x-git-upload-pack-request",
                 "Content-Encoding": "gzip"},
        body=b"compressed-bytes", token="t",
    )
    assert _FakeConnection.last_headers["Content-Encoding"] == "gzip"


def test_forward_omits_content_encoding_when_the_request_had_none(monkeypatch):
    """The common case (`info/refs`, or a small `git-upload-pack` body) --
    no `Content-Encoding` header at all, not one sent as an empty string.
    """
    monkeypatch.setattr(http.client, "HTTPSConnection", _FakeConnection)
    RealForwarder().forward(
        method="GET", path="/o/r.git/info/refs", query="service=git-upload-pack",
        headers={}, body=None, token="t",
    )
    assert "Content-Encoding" not in _FakeConnection.last_headers
