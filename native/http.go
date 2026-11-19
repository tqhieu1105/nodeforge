package main

import (
	"bufio"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The connection handles requests in order, so the handler does not need
// a background read while preparing a response.
type httpListener struct {
	net.Listener
	handler http.Handler
	mu      sync.Mutex
	active  map[net.Conn]struct{}
	closed  bool
	workers sync.WaitGroup
}

func newHTTPListener(listener net.Listener, handler http.Handler) *httpListener {
	return &httpListener{
		Listener: listener,
		handler:  handler,
		active:   make(map[net.Conn]struct{}),
	}
}

func (s *httpListener) serve() error {
	for {
		conn, err := s.Accept()
		if err != nil {
			return err
		}
		s.mu.Lock()
		if s.closed || len(s.active) >= 512 {
			s.mu.Unlock()
			conn.Close()
			continue
		}
		s.active[conn] = struct{}{}
		s.workers.Add(1)
		s.mu.Unlock()
		go s.connection(conn)
	}
}

func (s *httpListener) stop() {
	_ = s.Listener.Close()
	s.mu.Lock()
	s.closed = true
	for conn := range s.active {
		conn.Close()
	}
	s.mu.Unlock()
	s.workers.Wait()
}

const maxHeader = 32 << 10

// Header parsing has its own bound. The body is read from the same
// buffered connection under the individual route's size limit.
type headerReader struct {
	conn    net.Conn
	left    int
	limited bool
}

func (r *headerReader) Read(p []byte) (int, error) {
	if r.limited {
		if r.left == 0 {
			return 0, errors.New("request header is too large")
		}
		p = p[:min(len(p), r.left)]
	}
	n, err := r.conn.Read(p)
	if r.limited {
		r.left -= n
	}
	return n, err
}

type trackedBody struct {
	io.ReadCloser
	complete bool
}

func (b *trackedBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err == io.EOF {
		b.complete = true
	}
	return n, err
}

type wireResponse struct {
	headers http.Header
	media   string
	data    []byte
	status  int
}

func (w *wireResponse) Header() http.Header { return w.headers }

func (w *wireResponse) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}

func (w *wireResponse) Write(p []byte) (int, error) {
	w.WriteHeader(200)
	w.data = append(w.data, p...)
	return len(p), nil
}

func (w *wireResponse) reset() {
	clear(w.headers)
	w.status = 0
	w.data = nil
	w.media = ""
}

type headerBuffer struct{ data []byte }

func (b *headerBuffer) WriteString(s string) (int, error) {
	b.data = append(b.data, s...)
	return len(s), nil
}

func (b *headerBuffer) Write(p []byte) (int, error) {
	b.data = append(b.data, p...)
	return len(p), nil
}

var framingHeaders = map[string]bool{
	"Content-Length": true,
	"Connection":     true,
	"Date":           true,
	"Content-Type":   true,
}

func (w *wireResponse) send(conn net.Conn, request *http.Request, closeConn bool, date string, buffer []byte) error {
	status := w.status
	if status == 0 {
		status = 200
	}
	version := "HTTP/1.1 "
	if request.ProtoMajor == 1 && request.ProtoMinor == 0 {
		version = "HTTP/1.0 "
	}
	buffer = append(buffer[:0], version...)
	buffer = strconv.AppendInt(buffer, int64(status), 10)
	buffer = append(buffer, ' ')
	buffer = append(buffer, http.StatusText(status)...)
	buffer = append(buffer, "\r\nDate: "...)
	buffer = append(buffer, date...)
	buffer = append(buffer, "\r\n"...)
	bodyAllowed := status != 204 && status != 304
	if bodyAllowed {
		buffer = append(buffer, "Content-Length: "...)
		buffer = strconv.AppendInt(buffer, int64(len(w.data)), 10)
		buffer = append(buffer, "\r\n"...)
	}
	if closeConn {
		buffer = append(buffer, "Connection: close\r\n"...)
	} else if version == "HTTP/1.0 " {
		buffer = append(buffer, "Connection: keep-alive\r\n"...)
	}
	if w.media != "" {
		buffer = append(buffer, "Content-Type: "...)
		buffer = append(buffer, w.media...)
		buffer = append(buffer, "\r\n"...)
	}
	if ids := w.headers["X-Github-Request-Id"]; len(w.headers) == 1 && len(ids) == 1 {
		buffer = append(buffer, "X-Github-Request-Id: "...)
		buffer = append(buffer, ids[0]...)
		buffer = append(buffer, "\r\n"...)
	} else {
		head := headerBuffer{data: buffer}
		if err := w.headers.WriteSubset(&head, framingHeaders); err != nil {
			return err
		}
		buffer = head.data
	}
	buffer = append(buffer, "\r\n"...)
	if request.Method == "HEAD" || !bodyAllowed || len(w.data) == 0 {
		_, err := conn.Write(buffer)
		return err
	}
	if len(w.data)+len(buffer) <= cap(buffer) {
		_, err := conn.Write(append(buffer, w.data...))
		return err
	}
	chunks := net.Buffers{buffer, w.data}
	_, err := chunks.WriteTo(conn)
	return err
}

const malformedHTTP = "HTTP/1.1 400 Bad Request\r\nConnection: close\r\nContent-Type: application/json\r\nContent-Length: 25\r\n\r\n{\"message\":\"Bad Request\"}"

func (s *httpListener) connection(conn net.Conn) {
	defer func() {
		conn.Close()
		s.mu.Lock()
		delete(s.active, conn)
		s.mu.Unlock()
		s.workers.Done()
	}()
	in := &headerReader{conn: conn}
	reader := bufio.NewReaderSize(in, 4096)
	head := make([]byte, 0, 4096)
	response := wireResponse{headers: make(http.Header, 6)}
	var deadline time.Time
	var dateSecond int64
	var date string
	for {
		now := time.Now()
		// Renew in batches to avoid resetting socket timers on every request.
		if deadline.Sub(now) < 15*time.Second {
			deadline = now.Add(30 * time.Second)
			_ = conn.SetDeadline(deadline)
		}
		in.left, in.limited = maxHeader, true
		request, err := http.ReadRequest(reader)
		in.limited = false
		if err == io.EOF {
			return
		}
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				return
			}
			_, _ = io.WriteString(conn, malformedHTTP)
			return
		}
		if request.ProtoMajor != 1 || request.ProtoMinor > 1 || (request.Host == "" && request.ProtoMinor == 1) {
			_, _ = io.WriteString(conn, malformedHTTP)
			return
		}
		now = time.Now()
		if now.Unix() != dateSecond {
			dateSecond, date = now.Unix(), now.UTC().Format(http.TimeFormat)
		}
		var body *trackedBody
		if request.Body != http.NoBody {
			body = &trackedBody{ReadCloser: request.Body}
			request.Body = body
		}
		closeConn := request.Close
		if expect := request.Header.Get("Expect"); expect != "" {
			if !strings.EqualFold(expect, "100-continue") {
				sendJSON(&response, 417, fields{"message": "Expectation Failed"})
				_ = response.send(conn, request, true, date, head)
				return
			}
			if body != nil {
				if _, err := io.WriteString(conn, "HTTP/1.1 100 Continue\r\n\r\n"); err != nil {
					return
				}
			}
		}
		s.handler.ServeHTTP(&response, request)
		if body != nil && !body.complete {
			// Unread body bytes may precede the next request on this socket.
			closeConn = true
		}
		if err := response.send(conn, request, closeConn, date, head); err != nil || closeConn {
			return
		}
		response.reset()
	}
}
