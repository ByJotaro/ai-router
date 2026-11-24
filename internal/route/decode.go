package route

import (
	"bufio"
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
)

// decodeUpstreamBody makes a provider response readable for the client.
//
// The router always sends "Accept-Encoding: gzip, deflate, br, zstd" upstream
// (the opencode fingerprint constant, fingerprint.go), and Go's http.Transport
// only decodes gzip when IT added the header — never when the caller set it.
// So every compressed answer arrived raw, and the gateway copies no
// Content-Encoding header: clients received zstd/gzip/brotli bytes under
// "Content-Type: application/json". Measured 2026-11-14 on Inferway behind
// Cloudflare: 200, body magic 28 b5 2f fd, header absent. Decode here, once,
// for both the streaming and the buffered paths.
func decodeUpstreamBody(resp *http.Response) *http.Response {
	if resp == nil || resp.Body == nil {
		return resp
	}
	enc := strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Encoding")))
	if enc == "" {
		return resp // identity
	}
	// Everything below sniffs the frame, so a mislabelled body must not lose
	// the bytes the sniffer consumed: on a miss the buffered reader becomes the
	// body (still correct, just not decoded).
	src := bufio.NewReader(resp.Body)
	wrap := func(r io.Reader, extra ...io.Closer) *http.Response {
		resp.Body = &decodingBody{Reader: r, closers: append(extra, resp.Body)}
		resp.Header.Del("Content-Encoding")
		resp.Header.Del("Content-Length")
		resp.ContentLength = -1
		resp.Uncompressed = true
		return resp
	}
	switch enc {
	case "zstd":
		// A 20-byte frame can name a 512 MB window, and klauspost allocates
		// windowSize*2 before the first read (default maxDecodedSize 64 GiB).
		// The provider is a third party, so cap both: real error bodies use a
		// ~128 KiB window, and nothing here is worth a gigabyte of RAM.
		dec, err := zstd.NewReader(src, zstd.WithDecoderConcurrency(1),
			zstd.WithDecoderMaxMemory(16<<20), zstd.WithDecoderMaxWindow(8<<20))
		if err != nil {
			resp.Body = &decodingBody{Reader: src, closers: []io.Closer{resp.Body}}
			return resp
		}
		return wrap(dec.IOReadCloser(), errlessCloser{dec})
	case "br":
		return wrap(brotli.NewReader(src))
	case "gzip", "x-gzip":
		zr, err := gzip.NewReader(src)
		if err != nil {
			resp.Body = &decodingBody{Reader: src, closers: []io.Closer{resp.Body}}
			return resp
		}
		return wrap(zr)
	case "deflate":
		// RFC 9110 deflate is zlib-wrapped; some servers send raw flate.
		if hdr, err := src.Peek(2); err == nil && isZlibHeader(hdr) {
			if zr, err := zlib.NewReader(src); err == nil {
				return wrap(zr)
			}
		}
		return wrap(flate.NewReader(src))
	}
	return resp
}

func isZlibHeader(h []byte) bool {
	return len(h) == 2 && h[0]&0x0f == 8 && (uint16(h[0])<<8|uint16(h[1]))%31 == 0
}

// decodingBody owns the decoder and the upstream body; Close releases both, so
// a lease wrapper on the upstream body still runs exactly once.
type decodingBody struct {
	io.Reader
	closers []io.Closer
}

func (b *decodingBody) Close() error {
	var err error
	for _, c := range b.closers {
		if e := c.Close(); e != nil && err == nil {
			err = e
		}
	}
	return err
}

// errlessCloser adapts a Close() with no result (zstd.Decoder) to io.Closer.
type errlessCloser struct{ c interface{ Close() } }

func (e errlessCloser) Close() error { e.c.Close(); return nil }

// decodeBodyPrefix decompresses the head of a compressed upstream body so
// error/gate sniffers never quote frame bytes. The router asks for
// gzip/deflate/br/zstd, so a 403/400 body can be framed while the peek site
// only reads a slice of it: partial streams are expected, so a decoder error
// after some output is fine and an error before any output falls back to raw.
func decodeBodyPrefix(enc string, raw []byte) []byte {
	if len(raw) == 0 {
		return raw
	}
	// Sniff the frame first: Cloudflare answers a 4xx with a zstd body while
	// omitting Content-Encoding, so a header-only switch quoted raw frame bytes
	// as the error text ("H8&_$\"EVKU? r13&&( Po/nA{nu...", 2026-11-16).
	kind := strings.ToLower(strings.TrimSpace(enc))
	switch {
	case len(raw) >= 4 && raw[0] == 0x28 && raw[1] == 0xb5 && raw[2] == 0x2f && raw[3] == 0xfd:
		kind = "zstd"
	case len(raw) >= 3 && raw[0] == 0x28 && raw[1] == 0xb5 && raw[2] == 0x2f:
		kind = "zstd"
	case len(raw) >= 2 && raw[0] == 0x1f && raw[1] == 0x8b:
		kind = "gzip"
	}
	if kind == "" {
		return raw
	}
	br := bytes.NewReader(raw)
	var r io.Reader
	switch kind {
	case "zstd":
		d, err := zstd.NewReader(br, zstd.WithDecoderConcurrency(1),
			zstd.WithDecoderMaxMemory(16<<20), zstd.WithDecoderMaxWindow(8<<20))
		if err != nil {
			return raw
		}
		defer d.Close()
		r = d
	case "br":
		r = brotli.NewReader(br)
	case "gzip", "x-gzip":
		zr, err := gzip.NewReader(br)
		if err != nil {
			return raw
		}
		defer zr.Close()
		r = zr
	case "deflate":
		zr, err := zlib.NewReader(br)
		if err != nil {
			return raw
		}
		defer zr.Close()
		r = zr
	default:
		return raw
	}
	out, _ := io.ReadAll(io.LimitReader(r, 4<<10))
	if len(out) == 0 || !mostlyPrintable(out) {
		// Never quote frame bytes as an error excerpt: a truncated/unknown
		// compressed body used to reach the client as `H8&_$"EVKU? r13...`.
		return []byte(fmt.Sprintf("[compressed body, %d bytes]", len(raw)))
	}
	return out
}

// mostlyPrintable reports whether a decoded prefix is text worth quoting.
func mostlyPrintable(b []byte) bool {
	if len(b) == 0 {
		return false
	}
	bad := 0
	for _, c := range b {
		if c < 0x09 || (c > 0x0d && c < 0x20) || c == 0x7f {
			bad++
		}
	}
	return float64(bad)/float64(len(b)) < 0.05
}
