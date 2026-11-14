package helps

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strings"
	"time"

	utls "github.com/refraction-networking/utls"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/proxyutil"
	log "github.com/sirupsen/logrus"
	"golang.org/x/net/proxy"
)

type traeUTLSHTTP1RoundTripper struct {
	base   http.RoundTripper
	dialer proxy.Dialer
}

func (t *traeUTLSHTTP1RoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if req == nil || req.URL == nil || req.URL.Scheme != "https" {
		return t.base.RoundTrip(req)
	}
	hostname := req.URL.Hostname()
	port := req.URL.Port()
	if port == "" {
		port = "443"
	}
	conn, errDial := t.dialer.Dial("tcp", net.JoinHostPort(hostname, port))
	if errDial != nil {
		return nil, errDial
	}
	tlsConn := utls.UClient(conn, &utls.Config{ServerName: hostname}, utls.HelloCustom)
	spec := traeRustlsLikeClientHelloSpec(hostname)
	if errPreset := tlsConn.ApplyPreset(&spec); errPreset != nil {
		if errClose := conn.Close(); errClose != nil {
			log.Debugf("trae utls: close connection after preset failure: %v", errClose)
		}
		return nil, errPreset
	}
	if errHandshake := tlsConn.Handshake(); errHandshake != nil {
		if errClose := conn.Close(); errClose != nil {
			log.Debugf("trae utls: close connection after handshake failure: %v", errClose)
		}
		return nil, errHandshake
	}

	outReq := req.Clone(req.Context())
	outReq.RequestURI = ""
	if errWrite := writeTraeRawHTTPRequest(tlsConn, outReq); errWrite != nil {
		if errClose := tlsConn.Close(); errClose != nil {
			log.Debugf("trae utls: close connection after request failure: %v", errClose)
		}
		return nil, errWrite
	}
	resp, errRead := http.ReadResponse(bufio.NewReader(tlsConn), outReq)
	if errRead != nil {
		if errClose := tlsConn.Close(); errClose != nil {
			log.Debugf("trae utls: close connection after response failure: %v", errClose)
		}
		return nil, errRead
	}
	resp.Body = &readCloserWithConn{ReadCloser: resp.Body, conn: tlsConn}
	return resp, nil
}

func writeTraeRawHTTPRequest(w io.Writer, req *http.Request) error {
	body, errRead := readTraeRequestBody(req)
	if errRead != nil {
		return errRead
	}
	requestURI := req.URL.RequestURI()
	if requestURI == "" {
		requestURI = "/"
	}
	if _, errWrite := fmt.Fprintf(w, "%s %s HTTP/1.1\r\n", req.Method, requestURI); errWrite != nil {
		return errWrite
	}
	orderedHeaders := []string{
		"Version",
		"X-App-Id",
		"X-IDE-Function",
		"X-IDE-Version-Code",
		"X-Flow-Traceparent",
		"X-Custom-Repo-Urls",
		"Accept",
		"Authorization",
		"Content-Type",
		"Originator",
		"User-Agent",
	}
	written := make(map[string]struct{}, len(orderedHeaders)+3)
	for _, key := range orderedHeaders {
		if errWrite := writeTraeHeaderValues(w, req.Header, key, written); errWrite != nil {
			return errWrite
		}
	}
	if errWrite := writeTraeAdditionalHeaders(w, req.Header, written); errWrite != nil {
		return errWrite
	}
	host := strings.TrimSpace(req.Host)
	if host == "" && req.URL != nil {
		host = req.URL.Host
	}
	if host != "" {
		if _, errWrite := fmt.Fprintf(w, "host: %s\r\n", host); errWrite != nil {
			return errWrite
		}
	}
	if body != nil {
		if _, errWrite := fmt.Fprintf(w, "content-length: %d\r\n", len(body)); errWrite != nil {
			return errWrite
		}
	}
	if _, errWrite := io.WriteString(w, "\r\n"); errWrite != nil {
		return errWrite
	}
	if len(body) > 0 {
		_, errWrite := w.Write(body)
		return errWrite
	}
	return nil
}

func readTraeRequestBody(req *http.Request) ([]byte, error) {
	if req == nil || req.Body == nil || req.Body == http.NoBody {
		return nil, nil
	}
	body, errRead := io.ReadAll(req.Body)
	if errClose := req.Body.Close(); errClose != nil && errRead == nil {
		errRead = errClose
	}
	if errRead != nil {
		return nil, errRead
	}
	return body, nil
}

func writeTraeHeaderValues(w io.Writer, headers http.Header, key string, written map[string]struct{}) error {
	values := headers.Values(key)
	if len(values) == 0 {
		return nil
	}
	lowerKey := strings.ToLower(key)
	written[lowerKey] = struct{}{}
	for _, value := range values {
		if _, errWrite := fmt.Fprintf(w, "%s: %s\r\n", lowerKey, value); errWrite != nil {
			return errWrite
		}
	}
	return nil
}

func writeTraeAdditionalHeaders(w io.Writer, headers http.Header, written map[string]struct{}) error {
	keys := make([]string, 0, len(headers))
	for key := range headers {
		lowerKey := strings.ToLower(key)
		if _, ok := written[lowerKey]; ok {
			continue
		}
		switch lowerKey {
		case "accept-encoding", "connection", "content-length", "host":
			continue
		}
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		return strings.ToLower(keys[i]) < strings.ToLower(keys[j])
	})
	for _, key := range keys {
		if errWrite := writeTraeHeaderValues(w, headers, key, written); errWrite != nil {
			return errWrite
		}
	}
	return nil
}

func traeRustlsLikeClientHelloSpec(hostname string) utls.ClientHelloSpec {
	return utls.ClientHelloSpec{
		TLSVersMin: utls.VersionTLS12,
		TLSVersMax: utls.VersionTLS13,
		CipherSuites: []uint16{
			0x1302, 0x1303, 0x1301, 0xc02c, 0xc030, 0x009f, 0xcca9, 0xcca8,
			0xccaa, 0xc02b, 0xc02f, 0x009e, 0xc024, 0xc028, 0x006b, 0xc023,
			0xc027, 0x0067, 0xc00a, 0xc014, 0x0039, 0xc009, 0xc013, 0x0033,
			0x009d, 0x009c, 0x003d, 0x003c, 0x0035, 0x002f,
		},
		CompressionMethods: []uint8{0},
		Extensions: []utls.TLSExtension{
			&utls.RenegotiationInfoExtension{Renegotiation: utls.RenegotiateNever},
			&utls.SNIExtension{ServerName: hostname},
			&utls.SupportedPointsExtension{SupportedPoints: []uint8{0, 1, 2}},
			&utls.SupportedCurvesExtension{Curves: []utls.CurveID{4588, utls.X25519, utls.CurveP256, 30, utls.CurveP384, utls.CurveP521, 256, 257}},
			&utls.SessionTicketExtension{},
			&utls.GenericExtension{Id: 22},
			&utls.ExtendedMasterSecretExtension{},
			&utls.SignatureAlgorithmsExtension{SupportedSignatureAlgorithms: []utls.SignatureScheme{
				0x0905, 0x0906, 0x0904, 0x0403, 0x0503, 0x0603, 0x0807, 0x0808,
				0x081a, 0x081b, 0x081c, 0x0809, 0x080a, 0x080b, 0x0804, 0x0805,
				0x0806, 0x0401, 0x0501, 0x0601, 0x0303, 0x0301, 0x0302, 0x0402,
				0x0502, 0x0602,
			}},
			&utls.SupportedVersionsExtension{Versions: []uint16{utls.VersionTLS13, utls.VersionTLS12}},
			&utls.PSKKeyExchangeModesExtension{Modes: []uint8{1}},
			&utls.KeyShareExtension{KeyShares: []utls.KeyShare{{Group: 4588}, {Group: utls.X25519}}},
		},
	}
}

type readCloserWithConn struct {
	io.ReadCloser
	conn net.Conn
}

func (r *readCloserWithConn) Close() error {
	errBody := r.ReadCloser.Close()
	errConn := r.conn.Close()
	if errBody != nil {
		return errBody
	}
	return errConn
}

// NewTraeHTTPClient uses TRAE CLI's TLS shape while forcing HTTP/1.1.
func NewTraeHTTPClient(ctx context.Context, cfg *config.Config, auth *cliproxyauth.Auth, timeout time.Duration) *http.Client {
	proxyURL := ""
	if auth != nil {
		proxyURL = strings.TrimSpace(auth.ProxyURL)
	}
	if proxyURL == "" && cfg != nil {
		proxyURL = strings.TrimSpace(cfg.ProxyURL)
	}
	if ctx != nil {
		if roundTripper, ok := ctx.Value("cliproxy.roundtripper").(http.RoundTripper); ok && roundTripper != nil {
			return &http.Client{Transport: roundTripper}
		}
	}

	var dialer proxy.Dialer = proxy.Direct
	var base http.RoundTripper = http.DefaultTransport
	if proxyURL != "" {
		proxyDialer, mode, errBuild := proxyutil.BuildDialer(proxyURL)
		if errBuild != nil {
			log.Errorf("trae utls: failed to configure proxy dialer for %q: %v", proxyutil.Redact(proxyURL), errBuild)
		} else if mode != proxyutil.ModeInherit && proxyDialer != nil {
			dialer = proxyDialer
		}
		if transport := buildProxyTransport(proxyURL); transport != nil {
			base = transport
		}
	}
	client := &http.Client{Transport: &traeUTLSHTTP1RoundTripper{base: base, dialer: dialer}}
	if timeout > 0 {
		client.Timeout = timeout
	}
	return client
}
