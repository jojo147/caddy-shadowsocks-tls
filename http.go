package shadowsocks

import (
	"bytes"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"reflect"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/time/rate"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"

	"go.uber.org/zap"

	"github.com/imgk/caddy-shadowsocks-tls/outline"
)

func init() {
	caddy.RegisterModule(Handler{})
}

// Handler implements an HTTP handler that ...
type Handler struct {
	Server    string   `json:"server,omitempty"`
	ShadowBox string   `json:"shadowbox,omitempty"`
	Users     []string `json:"users,omitempty"`

	logger *zap.Logger
	limit  *rate.Limiter
	mutex  *sync.RWMutex
	users  map[string]struct{}

	proxyIP   net.IP
	proxyPort int
}

// CaddyModule returns the Caddy module information.
func (Handler) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.handlers.shadowsocks_tls",
		New: func() caddy.Module { return new(Handler) },
	}
}

// Provision implements caddy.Provisioner.
func (m *Handler) Provision(ctx caddy.Context) (err error) {
	m.logger = ctx.Logger(m)
	m.mutex = new(sync.RWMutex)
	m.users = make(map[string]struct{})

	prefix := os.Getenv("SB_API_PREFIX")
	port := os.Getenv("SB_API_PORT")
	if prefix != "" && port != "" && m.ShadowBox == "" {
		m.ShadowBox = fmt.Sprintf("https://127.0.0.1:%s/%s", port, prefix)
		m.logger.Info(fmt.Sprintf("add shadowbox server: %v", m.ShadowBox))
	}

	if m.ShadowBox != "" {
		server, er := outline.NewOutlineServer(m.ShadowBox)
		if er != nil {
			err = er
			return
		}

		if m.Server == "" {
			m.Server = fmt.Sprintf("127.0.0.1:%v", server.PortForNewAccessKeys)
		}

		m.logger.Info("add user from shadowbox server")
		for _, user := range server.Users {
			m.logger.Info(fmt.Sprintf("add new user: %v", user.Password))
			m.users[GenKey(user.Password)] = struct{}{}
		}
		m.limit = rate.NewLimiter(rate.Every(time.Second), 1)
	}

	proxyAddr, err := net.ResolveTCPAddr("tcp", m.Server)
	if err != nil {
		return
	}
	m.proxyIP = proxyAddr.IP
	m.proxyPort = proxyAddr.Port

	for _, user := range m.Users {
		m.logger.Info(fmt.Sprintf("add new user: %v", user))
		m.users[GenKey(user)] = struct{}{}
	}
	return
}

// ServeHTTP implements caddyhttp.MiddlewareHandler.
func (m *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request, next caddyhttp.Handler) error {
	if r.Method != http.MethodConnect {
		return next.ServeHTTP(w, r)
	}
	if !m.authenticate(r) {
		return next.ServeHTTP(w, r)
	}

	rw := io.ReadWriter(nil)
	switch r.ProtoMajor {
	case 1:
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			return errors.New("http hijacker error")
		}

		conn, buf, er := hijacker.Hijack()
		if er != nil {
			http.Error(w, er.Error(), http.StatusInternalServerError)
			return er
		}
		defer conn.Close()

		if n := buf.Reader.Buffered(); n > 0 {
			b := make([]byte, n)
			if _, err := io.ReadFull(buf.Reader, b); err != nil {
				panic("io.ReadFull error")
			}
			rw = &rawConn{rw: conn, Reader: bytes.NewReader(b)}
		} else {
			rw = &rawConn{rw: conn}
		}
	case 2, 3:
		rw = &rwConn{Reader: r.Body, Writer: w, Flusher: w.(http.Flusher)}
	}

	switch r.Host[:4] {
	case "tcp.":
		m.logger.Info(fmt.Sprintf("handle tcp connection from %v", r.RemoteAddr))
		if err := HandleTCP(rw, &net.TCPAddr{IP: m.proxyIP, Port: m.proxyPort}); err != nil {
			m.logger.Error(fmt.Sprintf("handle tcp error: %v", err))
		}
	case "udp.":
		m.logger.Info(fmt.Sprintf("handle udp connection from %v", r.RemoteAddr))
		if err := HandleUDP(rw, &net.UDPAddr{IP: m.proxyIP, Port: m.proxyPort}, time.Minute*3); err != nil {
			m.logger.Error(fmt.Sprintf("handle udp error: %v", err))
		}
	default:
		if _, ok := w.(http.Hijacker); !ok {
			return next.ServeHTTP(w, r)
		}
	}

	return nil
}

// Interface guards
var (
	_ caddy.Provisioner           = (*Handler)(nil)
	_ caddyhttp.MiddlewareHandler = (*Handler)(nil)
)

func StringToByteSlice(s string) []byte {
	ptr := (*reflect.StringHeader)(unsafe.Pointer(&s))
	hdr := &reflect.SliceHeader{
		Data: ptr.Data,
		Cap:  ptr.Len,
		Len:  ptr.Len,
	}
	return *(*[]byte)(unsafe.Pointer(hdr))
}

func GenKey(s string) string {
	sum := sha256.Sum224(StringToByteSlice(s))
	hex := StringToByteSlice(hex.EncodeToString(sum[:]))
	b64 := base64.StdEncoding.EncodeToString(hex)
	return fmt.Sprintf("Basic %v", b64)
}

// AuthLen is the length is http basic auth
// len(GenKey("Test1234"))
const AuthLen = 82

func (m *Handler) authenticate(r *http.Request) bool {
	auth := r.Header.Get("Proxy-Authorization")
	m.mutex.RLock()
	_, ok := m.users[auth]
	m.mutex.RUnlock()

	if ok {
		return true
	}
	if AuthLen != len(auth) || m.ShadowBox == "" {
		return false
	}

	m.mutex.Lock()
	if _, ok = m.users[auth]; ok {
		m.mutex.Unlock()
		return true
	}
	if !m.limit.Allow() {
		m.mutex.Unlock()
		return false
	}

	server, err := outline.NewOutlineServer(m.ShadowBox)
	if err != nil {
		m.logger.Error(fmt.Sprintf("connect shadowbox error: %v", err))
		return false
	}

	for user := range m.users {
		delete(m.users, user)
	}
	for _, user := range server.Users {
		m.logger.Info(fmt.Sprintf("add new user: %v", user.Password))
		m.users[GenKey(user.Password)] = struct{}{}
	}
	for _, user := range m.Users {
		m.logger.Info(fmt.Sprintf("add new user: %v", user))
		m.users[GenKey(user)] = struct{}{}
	}
	m.mutex.Unlock()

	m.mutex.RLock()
	_, ok = m.users[auth]
	m.mutex.RUnlock()
	return ok
}

type rwConn struct {
	io.Reader
	io.Writer
	Flusher http.Flusher
}

func (c *rwConn) Write(b []byte) (n int, err error) {
	n, err = c.Writer.Write(b)
	c.Flusher.Flush()
	return
}

// rawConn is ...
type rawConn struct {
	rw     net.Conn
	Reader io.Reader
	Writer io.Writer
}

// CloseWrite: *net.TCPConn and *tls.Conn
func (c *rawConn) CloseWrite() error {
	if conn, ok := c.rw.(*net.TCPConn); ok {
		return conn.CloseWrite()
	}
	if conn, ok := c.rw.(*tls.Conn); ok {
		return conn.CloseWrite()
	}
	return errors.New("conn type error")
}

// Read is ...
func (c *rawConn) Read(b []byte) (int, error) {
	if c.Reader == nil {
		return c.rw.Read(b)
	}
	n, err := c.Reader.Read(b)
	if errors.Is(err, io.EOF) {
		err = nil
		c.Reader = nil
	}
	return n, err
}

// Write is ...
func (c *rawConn) Write(b []byte) (int, error) {
	if c.Writer == nil {
		if _, err := io.WriteString(c.rw, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
			return 0, err
		}
		c.Writer = c.rw
	}
	return c.Writer.Write(b)
}

// HandleTCP is ...
func HandleTCP(rw io.ReadWriter, raddr *net.TCPAddr) error {
	rc, err := net.DialTCP("tcp", nil, raddr)
	if err != nil {
		return err
	}
	defer rc.Close()

	errCh := make(chan error, 1)
	go func(chan error) {
		_, err := io.Copy(io.Writer(rc), rw)
		if err == nil || errors.Is(err, os.ErrDeadlineExceeded) {
			rc.CloseWrite()
			errCh <- nil
			return
		}
		rc.SetDeadline(time.Now())
		rc.CloseRead()
		errCh <- err
	}(errCh)

	_, err = io.Copy(rw, io.Reader(rc))
	if err == nil || errors.Is(err, os.ErrDeadlineExceeded) {
		if conn, ok := rw.(*rawConn); ok {
			conn.CloseWrite()
		}
		rc.CloseRead()
		return <-errCh
	}
	rc.SetDeadline(time.Now())
	rc.CloseWrite()
	<-errCh

	return err
}

// HandleUDP is ...
func HandleUDP(rw io.ReadWriter, raddr *net.UDPAddr, timeout time.Duration) error {
	rc, err := net.DialUDP("udp", nil, raddr)
	if err != nil {
		return err
	}
	defer rc.Close()

	errCh := make(chan error, 1)
	go func(chan error) (err error) {
		defer func() {
			if err == nil || errors.Is(err, os.ErrDeadlineExceeded) || errors.Is(err, io.EOF) {
				errCh <- nil
				return
			}
			errCh <- err
		}()

		b := make([]byte, 16*1024)
		for {
			if _, err = io.ReadFull(rw, b[:2]); err != nil {
				break
			}
			n := int(b[0])<<8 | int(b[1])
			if _, err = io.ReadFull(rw, b[:n]); err != nil {
				break
			}
			if _, err = rc.Write(b[:n]); err != nil {
				break
			}
		}
		rc.SetDeadline(time.Now())
		return
	}(errCh)

	n := 0
	b := make([]byte, 16*1024)
	for {
		rc.SetReadDeadline(time.Now().Add(timeout))
		n, err = rc.Read(b[2:])
		if err != nil {
			break
		}
		b[0] = byte(n >> 8)
		b[1] = byte(n)
		if _, err = rw.Write(b[:2+n]); err != nil {
			break
		}
	}

	if err == nil || errors.Is(err, os.ErrDeadlineExceeded) || errors.Is(err, io.EOF) {
		return <-errCh
	}
	rc.SetDeadline(time.Now())
	<-errCh

	return err
}
