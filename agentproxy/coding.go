package agentproxy

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// A request body is read as its sender meant it: the wire bytes decoded once
// for the Content-Encoding it names, as mitmproxy's decoded content was. A body
// that changes is encoded again the same way, so the header and the bytes it
// describes always agree; one that does not change goes on as it came.

// coding is a request body's content coding as the edge reads it.
type coding string

const (
	identity   coding = ""
	gzipCoding coding = "gzip"
	zlibCoding coding = "deflate"     // RFC 9110's deflate: a zlib stream
	rawDeflate coding = "deflate-raw" // a bare deflate stream some senders call deflate
)

var errTooLarge = errors.New("the decoded body is over the size the edge reads")

// requestCoding is the coding h names: one coding, or none. Anything else
// (br, zstd, a chain of codings) is an error: the edge cannot read it.
func requestCoding(h http.Header) (coding, error) {
	vals := h.Values("Content-Encoding")
	var named []string
	for _, v := range vals {
		for _, c := range strings.Split(v, ",") {
			if c = strings.ToLower(strings.TrimSpace(c)); c != "" && c != "identity" {
				named = append(named, c)
			}
		}
	}
	switch {
	case len(named) == 0:
		return identity, nil
	case len(named) > 1:
		return identity, fmt.Errorf("content coding %q is not one the edge reads", strings.Join(named, ", "))
	case named[0] == "gzip" || named[0] == "x-gzip":
		return gzipCoding, nil
	case named[0] == "deflate":
		return zlibCoding, nil
	}
	return identity, fmt.Errorf("content coding %q is not one the edge reads", named[0])
}

// decodeBody is wire decoded under h's coding, at most limit bytes, and the
// coding it was read with (a deflate body may turn out to be a bare stream).
func decodeBody(wire []byte, h http.Header, limit int64) ([]byte, coding, error) {
	c, err := requestCoding(h)
	if err != nil {
		return nil, c, err
	}
	var r io.Reader
	switch c {
	case identity:
		if int64(len(wire)) > limit {
			return nil, c, errTooLarge
		}
		return wire, c, nil
	case gzipCoding:
		zr, err := gzip.NewReader(bytes.NewReader(wire))
		if err != nil {
			return nil, c, err
		}
		r = zr
	case zlibCoding:
		zr, err := zlib.NewReader(bytes.NewReader(wire))
		if err != nil {
			c, r = rawDeflate, flate.NewReader(bytes.NewReader(wire))
		} else {
			r = zr
		}
	}
	plain, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, c, err
	}
	if int64(len(plain)) > limit {
		return nil, c, errTooLarge
	}
	return plain, c, nil
}

// encodeBody is plain encoded under c, the coding it was decoded from.
func encodeBody(plain []byte, c coding) []byte {
	var b bytes.Buffer
	var w io.WriteCloser
	switch c {
	case gzipCoding:
		w = gzip.NewWriter(&b)
	case zlibCoding:
		w = zlib.NewWriter(&b)
	case rawDeflate:
		w, _ = flate.NewWriter(&b, flate.DefaultCompression)
	default:
		return plain
	}
	w.Write(plain) // a bytes.Buffer never fails a write
	w.Close()
	return b.Bytes()
}

// readUpTo reads r's body whole when it is at most limit bytes. A longer body
// is not buffered past the limit: what was read is put back in front of the
// rest, so the request still sends every byte as it came, and whole is false.
func readUpTo(r *http.Request, limit int64) (b []byte, whole bool, err error) {
	if r.Body == nil {
		return []byte{}, true, nil
	}
	b, err = io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		r.Body.Close()
		return nil, false, err
	}
	if int64(len(b)) <= limit {
		r.Body.Close()
		return b, true, nil
	}
	r.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(b), r.Body), r.Body}
	return nil, false, nil
}
