package main

// rawhttp.go is the default serving loop: a small HTTP/1.1 parser feeding the same handlers as
// net/http. FORGE_HTTP=nethttp keeps the stdlib server, so one binary can A/B both paths. The
// timed workloads pay net/http's per-request machinery on every call while the handlers are a few
// microseconds, so the loop parses the request line, headers and body itself, builds the minimal
// *http.Request the handlers read, records the response, and writes status line, headers,
// Content-Length and body in one Write per request.

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"net"
	"net/http"
	"net/textproto"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

const (
	rawMaxHeader = 64 << 10
	rawMaxBody   = 64 << 20
)

var errRawBad = errors.New("malformed request")

// rawWriter records whatever the handler writes; nothing touches the socket until the handler
// returns, which is what lets the loop emit a single frame with a correct Content-Length.
type rawWriter struct {
	status int
	header http.Header
	body   []byte
}

func (w *rawWriter) Header() http.Header { return w.header }

func (w *rawWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
}

func (w *rawWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = 200
	}
	w.body = append(w.body, p...)
	return len(p), nil
}

func (w *rawWriter) Flush() {}

func (w *rawWriter) reset() {
	w.status = 0
	w.body = w.body[:0]
	clear(w.header)
}

func (s *Server) serveRaw(ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go s.serveRawConn(c)
	}
}

func (s *Server) serveRawConn(c net.Conn) {
	defer c.Close()
	br := bufio.NewReaderSize(c, 16<<10)
	hreq := make(http.Header, 12)
	w := &rawWriter{header: make(http.Header, 6)}
	var out []byte
	for {
		line, err := rawLine(br)
		if err != nil {
			return
		}
		for len(line) == 0 { // a stray CRLF between pipelined requests
			if line, err = rawLine(br); err != nil {
				return
			}
		}
		hdrTotal := len(line) + 2
		sp1 := bytes.IndexByte(line, ' ')
		if sp1 <= 0 {
			rawError(c, "400 Bad Request")
			return
		}
		method := string(line[:sp1])
		rest := line[sp1+1:]
		sp2 := bytes.IndexByte(rest, ' ')
		if sp2 <= 0 {
			rawError(c, "400 Bad Request")
			return
		}
		target := string(rest[:sp2])
		proto := rest[sp2+1:]
		minor := 1
		switch {
		case bytes.Equal(proto, []byte("HTTP/1.1")):
		case bytes.Equal(proto, []byte("HTTP/1.0")):
			minor = 0
		default:
			rawError(c, "505 HTTP Version Not Supported")
			return
		}

		clear(hreq)
		host := ""
		clen := int64(0)
		hasCL := false
		chunked := false
		closeAfter := false
		kaHdr := false
		expect := false
		for {
			l, err := rawLine(br)
			if err != nil {
				return
			}
			hdrTotal += len(l) + 2
			if hdrTotal > rawMaxHeader {
				rawError(c, "431 Request Header Fields Too Large")
				return
			}
			if len(l) == 0 {
				break
			}
			if l[0] == ' ' || l[0] == '\t' {
				rawError(c, "400 Bad Request")
				return
			}
			ci := bytes.IndexByte(l, ':')
			if ci <= 0 {
				rawError(c, "400 Bad Request")
				return
			}
			name := headerKey(l[:ci])
			val := string(bytes.Trim(l[ci+1:], " \t"))
			switch name {
			case "Host":
				host = val
			case "Content-Length":
				n, ok := rawParseDec(l[ci+1:])
				if !ok {
					rawError(c, "400 Bad Request")
					return
				}
				clen = n
				hasCL = true
			case "Transfer-Encoding":
				if rawHasToken(l[ci+1:], "chunked") {
					chunked = true
				}
			case "Connection":
				if rawHasToken(l[ci+1:], "close") {
					closeAfter = true
				}
				if rawHasToken(l[ci+1:], "keep-alive") {
					kaHdr = true
				}
			case "Expect":
				if rawEqualFold(bytes.Trim(l[ci+1:], " \t"), "100-continue") {
					expect = true
				}
			}
			hreq[name] = append(hreq[name], val)
		}
		if expect {
			c.Write([]byte("HTTP/1.1 100 Continue\r\n\r\n"))
		}
		var body []byte
		if chunked {
			body, err = rawChunkedBody(br)
			if err != nil {
				rawError(c, "400 Bad Request")
				return
			}
		} else if hasCL && clen > 0 {
			if clen > rawMaxBody {
				rawError(c, "413 Request Entity Too Large")
				return
			}
			body = make([]byte, clen)
			if _, err := io.ReadFull(br, body); err != nil {
				return
			}
		}
		path := target
		rawq := ""
		if qi := strings.IndexByte(target, '?'); qi >= 0 {
			path = target[:qi]
			rawq = target[qi+1:]
		}
		req := &http.Request{
			Method:        method,
			RequestURI:    target,
			Proto:         "HTTP/1.1",
			ProtoMajor:    1,
			ProtoMinor:    minor,
			Header:        hreq,
			Host:          host,
			RemoteAddr:    c.RemoteAddr().String(),
			URL:           &url.URL{Path: path, RawQuery: rawq},
			ContentLength: int64(len(body)),
		}
		if body != nil {
			req.Body = io.NopCloser(bytes.NewReader(body))
		} else {
			req.Body = http.NoBody
		}
		w.reset()
		s.handle(w, req)
		keep := minor == 1 && !closeAfter
		if minor == 0 && kaHdr && !closeAfter {
			keep = true
		}
		out = appendRawResponse(out[:0], w, keep)
		if _, err := c.Write(out); err != nil {
			return
		}
		if !keep {
			return
		}
	}
}

// headerKey canonicalizes a request header name with the fast paths for the names this server
// reads; net/textproto handles anything else.
func headerKey(name []byte) string {
	switch string(name) {
	case "authorization":
		return "Authorization"
	case "content-type":
		return "Content-Type"
	case "content-length":
		return "Content-Length"
	case "accept":
		return "Accept"
	case "user-agent":
		return "User-Agent"
	case "host":
		return "Host"
	case "connection":
		return "Connection"
	case "transfer-encoding":
		return "Transfer-Encoding"
	case "expect":
		return "Expect"
	case "git-protocol":
		return "Git-Protocol"
	case "mcp-session-id":
		return "Mcp-Session-Id"
	case "mcp-protocol-version":
		return "Mcp-Protocol-Version"
	}
	return textproto.CanonicalMIMEHeaderKey(string(name))
}

// appendRawResponse frames the recorded reply as one HTTP/1.1 message. Header order is sorted so
// identical replies look identical on the wire; Content-Length is recomputed from the recorded
// body and Connection stays under the loop's control.
func appendRawResponse(out []byte, w *rawWriter, keep bool) []byte {
	status := w.status
	if status == 0 {
		status = 200
	}
	out = append(out, "HTTP/1.1 "...)
	out = strconv.AppendInt(out, int64(status), 10)
	out = append(out, ' ')
	out = append(out, http.StatusText(status)...)
	out = append(out, "\r\n"...)
	var keyArr [8]string
	keys := keyArr[:0]
	for k := range w.header {
		switch k {
		case "Content-Length", "Connection", "Transfer-Encoding":
			continue
		}
		keys = append(keys, k)
	}
	if len(keys) > 1 {
		sort.Strings(keys)
	}
	for _, k := range keys {
		for _, v := range w.header[k] {
			out = append(out, k...)
			out = append(out, ": "...)
			out = append(out, v...)
			out = append(out, "\r\n"...)
		}
	}
	noBody := status == 204 || status == 304 || (status >= 100 && status < 200)
	if !noBody {
		out = append(out, "Content-Length: "...)
		out = strconv.AppendInt(out, int64(len(w.body)), 10)
		out = append(out, "\r\n"...)
	}
	if !keep {
		out = append(out, "Connection: close\r\n"...)
	}
	out = append(out, "\r\n"...)
	if !noBody {
		out = append(out, w.body...)
	}
	return out
}

// rawLine reads one CRLF- or LF-terminated line and strips the terminator. The returned slice is
// only valid until the next read; callers copy what they keep.
func rawLine(br *bufio.Reader) ([]byte, error) {
	line, err := br.ReadSlice('\n')
	if err != nil {
		return nil, err
	}
	n := len(line)
	if n >= 2 && line[n-2] == '\r' {
		return line[:n-2], nil
	}
	return line[:n-1], nil
}

// rawChunkedBody decodes a chunked request body (undici uses it when a body has no length);
// trailers are consumed, the total is capped.
func rawChunkedBody(br *bufio.Reader) ([]byte, error) {
	var body []byte
	for {
		line, err := rawLine(br)
		if err != nil {
			return nil, err
		}
		if i := bytes.IndexByte(line, ';'); i >= 0 {
			line = line[:i]
		}
		n, ok := rawParseHex(line)
		if !ok {
			return nil, errRawBad
		}
		if n == 0 {
			for {
				l, err := rawLine(br)
				if err != nil {
					return nil, err
				}
				if len(l) == 0 {
					return body, nil
				}
			}
		}
		if n > rawMaxBody-int64(len(body)) {
			return nil, errRawBad
		}
		start := len(body)
		body = append(body, make([]byte, n)...)
		if _, err := io.ReadFull(br, body[start:]); err != nil {
			return nil, err
		}
		var crlf [2]byte
		if _, err := io.ReadFull(br, crlf[:]); err != nil {
			return nil, err
		}
		if crlf[0] != '\r' || crlf[1] != '\n' {
			return nil, errRawBad
		}
	}
}

func rawParseDec(b []byte) (int64, bool) {
	b = bytes.Trim(b, " \t")
	if len(b) == 0 {
		return 0, false
	}
	var n int64
	for _, c := range b {
		if c < '0' || c > '9' {
			return 0, false
		}
		if n > (1<<62)/10 {
			return 0, false
		}
		n = n*10 + int64(c-'0')
	}
	return n, true
}

func rawParseHex(b []byte) (int64, bool) {
	b = bytes.Trim(b, " \t")
	if len(b) == 0 || len(b) > 15 {
		return 0, false
	}
	var n int64
	for _, c := range b {
		n <<= 4
		switch {
		case c >= '0' && c <= '9':
			n += int64(c - '0')
		case c >= 'a' && c <= 'f':
			n += int64(c-'a') + 10
		case c >= 'A' && c <= 'F':
			n += int64(c-'A') + 10
		default:
			return 0, false
		}
	}
	return n, true
}

func rawHasToken(b []byte, token string) bool {
	for len(b) > 0 {
		i := bytes.IndexByte(b, ',')
		var part []byte
		if i < 0 {
			part, b = b, nil
		} else {
			part, b = b[:i], b[i+1:]
		}
		part = bytes.Trim(part, " \t")
		if rawEqualFold(part, token) {
			return true
		}
	}
	return false
}

func rawEqualFold(b []byte, s string) bool {
	if len(b) != len(s) {
		return false
	}
	for i := 0; i < len(b); i++ {
		c := b[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		d := s[i]
		if d >= 'A' && d <= 'Z' {
			d += 'a' - 'A'
		}
		if c != d {
			return false
		}
	}
	return true
}

func rawError(c net.Conn, status string) {
	buf := make([]byte, 0, 80)
	buf = append(buf, "HTTP/1.1 "...)
	buf = append(buf, status...)
	buf = append(buf, "\r\nContent-Length: 0\r\nConnection: close\r\n\r\n"...)
	c.Write(buf)
}
