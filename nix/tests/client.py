"""Test client of the nixosTests (ADR 0010): one call to the store as one of the test CA's identities.

  client.py <ca> <identity> <method> <key> [body]  prints "<status> <reason>" for an error,
                                                   "<status> <body>" otherwise, "tls-refused"
                                                   when the store refuses the handshake
  client.py <ca> <identity> h2-preface             holds an HTTP/2 connection after its preface,
                                                   prints the seconds until the store closes it

<identity> is a directory of <ca> holding svid.pem and key.pem; the store is verified against
<ca>/bundle.pem (its SVID has no DNS SAN: no hostname check, as azukari-backup).
The bucket is "backup", or $BUCKET when set.
"""

import hashlib
import http.client
import os
import socket
import ssl
import sys
import time

STORE = ("127.0.0.1", 8443)
BUCKET = os.environ.get("BUCKET", "backup")


def context(ca, identity, alpn):
    ctx = ssl.create_default_context(cafile=f"{ca}/bundle.pem")
    ctx.check_hostname = False
    ctx.load_cert_chain(f"{ca}/{identity}/svid.pem", f"{ca}/{identity}/key.pem")
    ctx.set_alpn_protocols(alpn)
    return ctx


def reason(body):
    """Decodes Error{reason}: exactly one field 1 (string), length under 128."""
    assert len(body) >= 2 and body[0] == 0x0A and body[1] == len(body) - 2, body
    return body[2:].decode()


def call(ca, identity, method, key, body=None):
    conn = http.client.HTTPSConnection(*STORE, context=context(ca, identity, ["http/1.1"]))
    headers = {}
    if body is not None:
        body = body.encode()
        headers["Azukari-Sha256"] = hashlib.sha256(body).hexdigest()
    try:
        conn.request(method, f"/v1/b/{BUCKET}/o/{key}", body=body, headers=headers)
        r = conn.getresponse()
        data = r.read()
    except (ssl.SSLError, ConnectionError):
        print("tls-refused")
        return
    if r.status >= 400:
        assert r.getheader("Content-Type") == "application/x-protobuf", r.getheaders()
        print(r.status, reason(data))
    else:
        print(r.status, data.decode())


def h2_preface(ca, identity):
    s = context(ca, identity, ["h2"]).wrap_socket(socket.create_connection(STORE))
    assert s.selected_alpn_protocol() == "h2", s.selected_alpn_protocol()
    # The client preface and an empty SETTINGS frame, then nothing: no HEADERS ever comes.
    s.sendall(b"PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n" + bytes([0, 0, 0, 4, 0, 0, 0, 0, 0]))
    s.settimeout(60)
    start = time.monotonic()
    try:
        while s.recv(4096):
            pass
    except (ssl.SSLError, ConnectionError):
        pass
    print(round(time.monotonic() - start))


if __name__ == "__main__":
    if sys.argv[3] == "h2-preface":
        h2_preface(*sys.argv[1:3])
    else:
        call(*sys.argv[1:])
