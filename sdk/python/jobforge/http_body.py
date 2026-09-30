"""Bound response buffering and decompression before JSON validation."""

from __future__ import annotations

import zlib

import httpx


def read_bounded_response(response: httpx.Response, limit: int) -> bytes:
    """Read identity, gzip or zlib deflate with a bounded decoded allocation.

    The caller owns response closure. A custom transport may have buffered the
    body already; its allocations cannot be bounded by a downstream reader.
    Reject malformed, truncated or unsupported encodings with a fixed message.
    """
    if response.is_stream_consumed:
        content = response.content
        if len(content) > limit:
            raise ValueError("response too large")
        return content
    encoding = response.headers.get("content-encoding", "identity").strip().lower()
    if encoding not in {"identity", "gzip", "deflate"}:
        raise ValueError("unsupported response encoding")
    window = zlib.MAX_WBITS + (16 if encoding == "gzip" else 0)
    decoder = None if encoding == "identity" else zlib.decompressobj(window)
    body = bytearray()
    encoded = 0
    try:
        for chunk in response.iter_raw():
            encoded += len(chunk)
            # Bound input work too, including pathological compression headers.
            if encoded > limit + 65536:
                raise ValueError("response too large")
            if decoder is None:
                if len(body) + len(chunk) > limit:
                    raise ValueError("response too large")
                body.extend(chunk)
                continue
            while chunk:
                if decoder.eof:
                    if encoding != "gzip":
                        raise ValueError("trailing response data")
                    decoder = zlib.decompressobj(window)
                decoded = decoder.decompress(chunk, limit - len(body) + 1)
                if len(body) + len(decoded) > limit:
                    raise ValueError("response too large")
                body.extend(decoded)
                chunk = decoder.unused_data
        if decoder is not None and not decoder.eof:
            raise ValueError("truncated response encoding")
    except zlib.error:
        raise ValueError("invalid response encoding") from None
    return bytes(body)
