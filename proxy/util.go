package proxy

import (
	"bytes"
	"fmt"
	"net"

	"github.com/coyove/goflyway/pkg/msg64"

	"crypto/tls"
	"encoding/base64"
	"io"
	"net/http"
	_url "net/url"
	"regexp"
	"strings"
	"time"
)

const (
	socksVersion5   = byte(0x05)
	socksAddrIPv4   = 1
	socksAddrDomain = 3
	socksAddrIPv6   = 4
)

const (
	doConnect       = 1 << iota // Establish TCP tunnel
	doHTTPReq                   // Forward plain HTTP request
	doWebSocket                 // Use Websocket protocol
	doMuxWS                     // Multiplexer over WS
	doDNS                       // DNS query request
	doPartialCipher             // Partial encryption
	doDisableCipher             // No encryption
	doUDPRelay                  // UDP relay request
	doLocalRP                   // Request to ctrl server

	// Currently we have 9 options, so in clientRequest.Marshal
	// we can use "uint16" to store. If more options to be added in the future,
	// code in clientRequest.Marshal must also be changed.
)

const (
	PolicyMITM = 1 << iota
	PolicyForward
	PolicyAgent
	PolicyGlobal
	PolicyVPN
	PolicyWebSocket
	PolicyHTTPS
	PolicyKCP
	PolicyDisableUDP
	PolicyDisableLRP
)

const (
	timeoutUDP          = 30 * time.Second
	timeoutTCP          = 60 * time.Second
	timeoutDial         = 60 * time.Second
	timeoutOp           = 60 * time.Second
	invalidRequestRetry = 10
	dnsRespHeader       = "ETag"
	errConnClosedMsg    = "use of closed network connection"
	fwdURLHeader        = "X-Forwarded-Url"
)

var (
	okHTTP         = []byte("HTTP/1.0 200 Connection Established\r\n\r\n")
	okSOCKS        = []byte{socksVersion5, 0, 0, 1, 0, 0, 0, 0, 0, 0}
	http101        = []byte("HTTP/1.1 101 Switching Protocols")
	http200        = []byte("HTTP/1.1 200 OK")
	http403        = []byte("HTTP/1.1 403 Forbidden")
	udpHeaderIPv4  = []byte{0, 0, 0, 1, 0, 0, 0, 0, 0, 0}
	udpHeaderIPv6  = []byte{0, 0, 0, 4, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}
	socksHandshake = []byte{socksVersion5, 1, 0}
	dummyHeaders   = []string{"Accept-Language", "Accept-Encoding", "Cache-Control", "Connection", "Referer", "User-Agent"}
	tlsSkip        = &tls.Config{InsecureSkipVerify: true}
	hasPort        = regexp.MustCompile(`:\d+$`)
	isHTTPSSchema  = regexp.MustCompile(`^https:\/\/`)
)

type Options uint32

func (o *Options) IsSet(option uint32) bool {
	return (uint32(*o) & option) != 0
}

func (o *Options) Set(options ...uint32) {
	for _, option := range options {
		*o = Options(uint32(*o) | option)
	}
}

func (o *Options) SetBool(b bool, option uint32) {
	if b {
		o.Set(option)
	}
}

func (o *Options) UnSet(options ...uint32) {
	for _, option := range options {
		*o = Options((uint32(*o) | option) - option)
	}
}

type buffer struct{ bytes.Buffer }

func (b *buffer) R() *buffer {
	b.Buffer.Reset()
	return b
}

func (b *buffer) ToURL() (*_url.URL, error) {
	return _url.Parse(b.String())
}

func (b *buffer) Trunc(t byte) *buffer {
	if ln := b.Len(); ln > 0 {
		if b.Bytes()[ln-1] == t {
			b.Truncate(ln - 1)
		}
	}
	return b
}

func (b *buffer) Writes(ss ...string) *buffer {
	for _, s := range ss {
		b.WriteString(s)
	}
	return b
}

func (proxy *ProxyClient) addToDummies(req *http.Request) {
	for _, field := range dummyHeaders {
		if x := req.Header.Get(field); x != "" {
			proxy.dummies.Add(field, x)
		}
	}
}

func (proxy *ProxyClient) genHost() string {
	const tlds = ".com.net.org"
	if proxy.DummyDomain == "" {
		i := proxy.Rand.Intn(3) * 4
		return proxy.Cipher.Jibber() + tlds[i:i+4]
	}

	return proxy.DummyDomain
}

func (proxy *ProxyClient) encryptRequest(req *http.Request, r *clientRequest) [ivLen]byte {
	r.Auth = proxy.UserAuth
	proxy.addToDummies(req)

	var urlBuf buffer
	if proxy.Policy.IsSet(PolicyForward) {
		r.Real = req.URL.String()
		req.Header.Add(fwdURLHeader, urlBuf.Writes("http://", proxy.genHost(), "/", proxy.encryptClientRequest(r)).String())
		req.Host = proxy.Upstream
		req.URL, _ = urlBuf.R().Writes("http://", proxy.Upstream).ToURL()
	} else {
		req.Host = proxy.genHost()
		r.Real = req.URL.String()
		req.URL, _ = urlBuf.R().Writes("http://", req.Host, "/", proxy.encryptClientRequest(r)).ToURL()
	}

	if proxy.Policy.IsSet(PolicyMITM) && proxy.ProxyAuth != "" {
		x := "Basic " + base64.StdEncoding.EncodeToString([]byte(proxy.ProxyAuth))
		req.Header.Add("Proxy-Authorization", x)
		req.Header.Add("Authorization", x)
	}

	var cookies buffer
	for _, c := range req.Cookies() {
		c.Value = proxy.Cipher.Encrypt(c.Value, r.IV)
		cookies.Writes(c.String(), ";")
	}
	req.Header.Set("Cookie", cookies.Trunc(';').String())

	if origin := req.Header.Get("Origin"); origin != "" {
		req.Header.Set("Origin", proxy.Cipher.Encrypt(origin, r.IV)+".com")
	}

	if referer := req.Header.Get("Referer"); referer != "" {
		req.Header.Set("Referer", proxy.Cipher.Encrypt(referer, r.IV))
	}

	req.Body = proxy.Cipher.IO.NewReadCloser(req.Body, r.IV)
	return r.IV
}

func (proxy *ProxyServer) stripURI(uri string) string {
	if len(uri) < 1 {
		return uri
	}

	if uri[0] != '/' {
		idx := strings.Index(uri[8:], "/")
		if idx > -1 {
			uri = uri[idx+1+8:]
		} else {
			proxy.Logger.Warnf("Unexpected URI: %s", uri)
		}
	} else {
		uri = uri[1:]
	}

	return uri
}

func (proxy *ProxyServer) decryptRequest(req *http.Request, r *clientRequest) {
	var cookies buffer
	var err error

	for _, c := range req.Cookies() {
		c.Value, err = proxy.Cipher.Decrypt(c.Value, r.IV)
		if err != nil {
			proxy.Logger.Errorf("Failed to decrypt cookie: %v, %v", err, req)
			return
		}
		cookies.Writes(c.String(), ";")
	}
	req.Header.Set("Cookie", cookies.Trunc(';').String())

	if origin := req.Header.Get("Origin"); len(origin) > 4 {
		origin, err = proxy.Decrypt(origin[:len(origin)-4], r.IV)
		if err != nil {
			proxy.Logger.Errorf("Failed to decrypt origin: %v, %v", err, req)
			return
		}
		req.Header.Set("Origin", origin)
	}

	if referer := req.Header.Get("Referer"); referer != "" {
		referer, err = proxy.Decrypt(referer, r.IV)
		if err != nil {
			proxy.Logger.Errorf("Failed to decrypt referer: %v, %v", err, req)
			return
		}
		req.Header.Set("Referer", referer)
	}

	for k := range req.Header {
		if k[:3] == "Cf-" || (len(k) > 12 && strings.ToLower(k[:12]) == "x-forwarded-") {
			// ignore all cloudflare headers
			// this is needed when you use cf as the frontend:
			// gofw client -> cloudflare -> gofw server -> target host using cloudflare

			// delete all x-forwarded-... headers
			// some websites won't allow them
			req.Header.Del(k)
		}
	}

	req.Body = proxy.Cipher.IO.NewReadCloser(req.Body, r.IV)
}

func copyHeaders(dst, src http.Header, gc *Cipher, enc bool, iv [ivLen]byte) {
	for k := range dst {
		dst.Del(k)
	}

	var setcookies buffer
	for k, vs := range src {
	READ:
		for _, v := range vs {
			switch strings.ToLower(k) {
			case "set-cookie":
				if iv != [ivLen]byte{} {
					if enc {
						setcookies.Writes(v, "\n")
						continue READ
					} else {
						ei, di := strings.Index(v, "="), strings.Index(v, ";")
						if di == -1 {
							di = len(v)
						}

						v, _ = gc.Decrypt(v[ei+1:di], iv)
						if !strings.HasSuffix(v, gc.Alias) {
							continue READ
						}
						v = v[:len(v)-6]
					}
				}
			case "content-encoding", "content-type":
				if enc {
					dst.Add("X-"+k, v)
					continue READ
				} else if iv != [ivLen]byte{} {
					continue READ
				}

				// IV is nil and we are in decrypt mode
				// aka plain copy mode, so fall to the bottom
			case "x-content-encoding", "x-content-type":
				if !enc {
					dst.Add(k[2:], v)
					continue READ
				}
			}

			for _, vn := range strings.Split(v, "\n") {
				dst.Add(k, vn)
			}
		}
	}

	if setcookies.Len() > 0 && iv != [ivLen]byte{} {
		// some http proxies or middlewares will combine multiple Set-Cookie headers into one
		// but some browsers do not support this behavior
		// here we just do the combination in advance and split them when decrypting
		setcookies.Trunc('\n').WriteString(gc.Alias)
		dst.Add("Set-Cookie", gc.Jibber()+"="+gc.Encrypt(setcookies.String(), iv)+"; Domain="+gc.Jibber()+".com; HttpOnly")
	}
}

func (proxy *ProxyClient) basicAuth(token string) string {
	parts := strings.Split(token, " ")
	if len(parts) != 2 {
		return ""
	}

	pa, err := base64.StdEncoding.DecodeString(strings.TrimSpace(parts[1]))
	if err != nil {
		return ""
	}

	if s := string(pa); s == proxy.UserAuth {
		return s
	}

	return ""
}

func tryClose(b io.ReadCloser) {
	if err := b.Close(); err != nil {
		// proxy.Logger.Warnf("Can't close", err)
	}
}

func splitHostPort(host string) (string, string) {
	if idx := strings.LastIndex(host, ":"); idx > 0 {
		idx2 := strings.LastIndex(host, "]")
		if idx2 < idx {
			return strings.ToLower(host[:idx]), host[idx:]
		}

		// ipv6 without port
	}

	return strings.ToLower(host), ""
}

func readUntil(r io.Reader, eoh string) ([]byte, error) {
	buf, respbuf := make([]byte, 1), &bytes.Buffer{}
	eidx, found := 0, false

	for {
		n, err := r.Read(buf)
		if n == 1 {
			respbuf.WriteByte(buf[0])
		}

		if buf[0] == eoh[eidx] {
			if eidx++; eidx == len(eoh) {
				found = true
				break
			}
		} else {
			eidx = 0
		}

		if err != nil {
			if err != io.EOF {
				return nil, err
			}
			break
		}
	}

	if !found {
		return nil, fmt.Errorf("readUntil cannot find the pattern: %v", []byte(eoh))
	}

	return respbuf.Bytes(), nil
}

func isClosedConnErr(err error) bool {
	return strings.Contains(err.Error(), errConnClosedMsg)
}

func isTimeoutErr(err error) bool {
	if ne, ok := err.(net.Error); ok {
		return ne.Timeout()
	}

	return false
}

func (proxy *ProxyClient) encryptClientRequest(r *clientRequest) string {
	enc := proxy.Cipher.GCM.Seal(nil, r.IV[:12], r.Marshal(), nil)
	enc = append(enc, r.IV[:]...)
	return msg64.Base41Encode(enc)
}

func (proxy *ProxyServer) decryptClientRequest(url string) *clientRequest {
	buf, ok := msg64.Base41Decode(url)
	if !ok || len(buf) < ivLen {
		return nil
	}

	iv := buf[len(buf)-ivLen:]
	buf = buf[:len(buf)-ivLen]

	var err error
	buf, err = proxy.Cipher.GCM.Open(nil, iv[:12], buf, nil)
	if err != nil {
		proxy.Logger.Errorf("Failed to decrypt host: %s, %v", url, err)
		return nil
	}

	r := new(clientRequest)
	copy(r.IV[:], iv)

	if err = r.Unmarshal(buf); err != nil {
		proxy.Logger.Errorf("Failed to decrypt host: %s, %v", url, err)
		return nil
	}

	return r
}
