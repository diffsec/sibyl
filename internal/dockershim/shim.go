// Package dockershim puts a Docker Engine API socket in front of the Podman
// socket. It passes every call through to Podman, except the two BuildKit
// endpoints that Podman does not have:
//
//   - /session carries a build session: registry logins, and the build
//     context for /grpc builds. The shim keeps the session, as dockerd does.
//   - /grpc is the BuildKit control API. The shim answers Info, ListWorkers,
//     Solve and Status. Solve pulls the build context through the session,
//     and builds it with Podman's /build endpoint, which runs Buildah.
//
// Arcane builds with BuildKit only, so without the shim every Arcane build on
// Podman fails with "unable to upgrade to h2c, received 404".
package dockershim

import (
	"bufio"
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"regexp"
	"sync"
	"time"

	controlapi "github.com/moby/buildkit/api/services/control"
	"github.com/moby/buildkit/session"
	"google.golang.org/grpc"
)

// Shim is the HTTP handler for the shim socket.
type Shim struct {
	log    *slog.Logger
	sm     *session.Manager
	grpc   *grpc.Server
	proxy  *httputil.ReverseProxy
	podman *http.Client
}

// New returns a shim for the Podman socket at upstream.
func New(upstream string, log *slog.Logger) (*Shim, error) {
	sm, err := session.NewManager()
	if err != nil {
		return nil, err
	}
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", upstream)
		},
		DisableCompression: true,
	}
	s := &Shim{
		log: log,
		sm:  sm,
		proxy: &httputil.ReverseProxy{
			Rewrite: func(pr *httputil.ProxyRequest) {
				pr.Out.URL.Scheme, pr.Out.URL.Host = "http", "podman"
				pr.Out.Host = pr.In.Host
			},
			Transport: tr,
			// Logs, events and attach stream. Send each write at once.
			FlushInterval: -1,
		},
		podman: &http.Client{Transport: tr},
	}
	s.grpc = grpc.NewServer()
	controlapi.RegisterControlServer(s.grpc, newControl(s))
	return s, nil
}

// buildkitPath matches /session and /grpc, with or without an API version.
var buildkitPath = regexp.MustCompile(`^(?:/v[0-9]+(?:\.[0-9]+)*)?/(session|grpc)$`)

func (s *Shim) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	m := buildkitPath.FindStringSubmatch(r.URL.Path)
	if m == nil || r.Method != http.MethodPost {
		s.proxy.ServeHTTP(w, r)
		return
	}
	if r.Header.Get("Upgrade") != "h2c" {
		http.Error(w, "this endpoint needs Upgrade: h2c", http.StatusBadRequest)
		return
	}
	// The session and the gRPC connection outlive the HTTP request.
	ctx := context.WithoutCancel(r.Context())
	if m[1] == "session" {
		if err := s.sm.HandleHTTPRequest(ctx, w, r); err != nil {
			s.log.Warn("build session", "err", err)
		}
		return
	}
	conn, err := upgrade(w, r)
	if err != nil {
		s.log.Warn("grpc upgrade", "err", err)
		return
	}
	// The client speaks HTTP/2 without TLS after the upgrade.
	var protos http.Protocols
	protos.SetUnencryptedHTTP2(true)
	srv := &http.Server{
		Handler:           s.grpc,
		Protocols:         &protos,
		ReadHeaderTimeout: 30 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	_ = srv.Serve(newOneConnListener(conn))
}

// oneConnListener hands out one connection, then waits until it closes.
type oneConnListener struct {
	conn   chan net.Conn
	closed chan struct{}
	once   sync.Once
	addr   net.Addr
}

func newOneConnListener(c net.Conn) *oneConnListener {
	l := &oneConnListener{conn: make(chan net.Conn, 1), closed: make(chan struct{}), addr: c.LocalAddr()}
	l.conn <- &notifyConn{Conn: c, l: l}
	return l
}

func (l *oneConnListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conn:
		return c, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

func (l *oneConnListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return nil
}

func (l *oneConnListener) Addr() net.Addr { return l.addr }

// notifyConn closes its listener when it closes, so Serve returns.
type notifyConn struct {
	net.Conn
	l *oneConnListener
}

func (c *notifyConn) Close() error {
	err := c.Conn.Close()
	_ = c.l.Close()
	return err
}

// upgrade takes over the connection and answers 101 Switching Protocols.
func upgrade(w http.ResponseWriter, r *http.Request) (net.Conn, error) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		return nil, errors.New("the connection cannot be taken over")
	}
	conn, rw, err := hj.Hijack()
	if err != nil {
		return nil, err
	}
	resp := &http.Response{StatusCode: http.StatusSwitchingProtocols, ProtoMajor: 1, ProtoMinor: 1, Header: http.Header{}}
	resp.Header.Set("Connection", "Upgrade")
	resp.Header.Set("Upgrade", r.Header.Get("Upgrade"))
	if err := resp.Write(conn); err != nil {
		_ = conn.Close()
		return nil, err
	}
	if rw.Reader.Buffered() > 0 {
		return &bufferedConn{Conn: conn, r: rw.Reader}, nil
	}
	return conn, nil
}

// bufferedConn reads the bytes that the HTTP server read ahead first.
type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.r.Read(p) }
