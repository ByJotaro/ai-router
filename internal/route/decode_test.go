package route

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"io"
	"net/http"
	"testing"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
)

type closableBuf struct {
	*bytes.Reader
	closed bool
}

func (c *closableBuf) Close() error { c.closed = true; return nil }

func newResp(enc string, body []byte) (*http.Response, *closableBuf) {
	cb := &closableBuf{Reader: bytes.NewReader(body)}
	h := http.Header{"Content-Type": {"application/json"}}
	if enc != "" {
		h.Set("Content-Encoding", enc)
	}
	return &http.Response{Header: h, Body: cb, ContentLength: int64(len(body))}, cb
}

func assertDecoded(t *testing.T, resp *http.Response, want []byte, upstream *closableBuf) {
	t.Helper()
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("decoded %q, want %q", got, want)
	}
	if resp.Header.Get("Content-Encoding") != "" || resp.Header.Get("Content-Length") != "" {
		t.Fatalf("stale headers kept: %v", resp.Header)
	}
	if resp.ContentLength != -1 {
		t.Fatalf("ContentLength = %d, want -1", resp.ContentLength)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if !upstream.closed {
		t.Fatal("upstream body was not closed")
	}
}

// Measured 2026-11-14: Inferway answered through the leased egress with a zstd
// body while the gateway copies no Content-Encoding header, so the client got
// compressed bytes under application/json.
func TestDecodeUpstreamBodyZstd(t *testing.T) {
	want := []byte(`{"choices":[{"message":{"content":"OK"}}]}`)
	var buf bytes.Buffer
	zw, _ := zstd.NewWriter(&buf)
	if _, err := zw.Write(want); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	resp, upstream := newResp("zstd", buf.Bytes())

	decodeUpstreamBody(resp)
	assertDecoded(t, resp, want, upstream)
}

func TestDecodeUpstreamBodyBrotli(t *testing.T) {
	want := []byte(`{"ok":true}`)
	var buf bytes.Buffer
	bw := brotli.NewWriter(&buf)
	if _, err := bw.Write(want); err != nil {
		t.Fatal(err)
	}
	if err := bw.Close(); err != nil {
		t.Fatal(err)
	}
	resp, upstream := newResp("br", buf.Bytes())

	decodeUpstreamBody(resp)
	assertDecoded(t, resp, want, upstream)
}

// Go's Transport does NOT decompress gzip when the request carries its own
// Accept-Encoding (fingerprint.go always sets it), so gzip answers arrive raw.
func TestDecodeUpstreamBodyGzip(t *testing.T) {
	want := []byte(`{"ok":true}`)
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	if _, err := gw.Write(want); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	resp, upstream := newResp("gzip", buf.Bytes())

	decodeUpstreamBody(resp)
	assertDecoded(t, resp, want, upstream)
}

func TestDecodeUpstreamBodyDeflateZlibAndRaw(t *testing.T) {
	want := []byte(`{"ok":true}`)

	var zbuf bytes.Buffer
	zw := zlib.NewWriter(&zbuf)
	if _, err := zw.Write(want); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	resp, upstream := newResp("deflate", zbuf.Bytes())
	decodeUpstreamBody(resp)
	assertDecoded(t, resp, want, upstream)

	var fbuf bytes.Buffer
	fw, _ := flate.NewWriter(&fbuf, flate.DefaultCompression)
	if _, err := fw.Write(want); err != nil {
		t.Fatal(err)
	}
	if err := fw.Close(); err != nil {
		t.Fatal(err)
	}
	resp, upstream = newResp("deflate", fbuf.Bytes())
	decodeUpstreamBody(resp)
	assertDecoded(t, resp, want, upstream)
}

func TestDecodeUpstreamBodyPassesIdentityAndUnknown(t *testing.T) {
	plain := []byte(`{"ok":true}`)
	resp, upstream := newResp("", plain)
	if decodeUpstreamBody(resp) != resp || resp.Body != upstream {
		t.Fatal("identity body must be passed through untouched")
	}
	got, _ := io.ReadAll(resp.Body)
	if !bytes.Equal(got, plain) {
		t.Fatalf("identity decoded %q", got)
	}

	// Unknown coding: bytes must survive intact (the sniffer buffers nothing
	// away) and the header stays, since we cannot decode it.
	resp, _ = newResp("compress", plain)
	decodeUpstreamBody(resp)
	got, _ = io.ReadAll(resp.Body)
	if !bytes.Equal(got, plain) {
		t.Fatalf("unknown coding mangled the body: %q", got)
	}
	if resp.Header.Get("Content-Encoding") != "compress" {
		t.Fatalf("unknown coding header dropped: %v", resp.Header)
	}
}
