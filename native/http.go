package main

import (
	"bufio"
	"bytes"
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
		if tcp, ok := conn.(*net.TCPConn); ok {
			_ = tcp.SetNoDelay(true)
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

type requestTemplate struct {
	header  []byte
	request *http.Request
	issue   issueReply
	git     gitTarget
}

// Remember complete headers only after the standard parser validates them.
// Each entry belongs to one connection and compares the complete header block.
type requestReader struct {
	reader    *bufio.Reader
	preview   *bufio.Reader
	header    bytes.Reader
	templates [8]requestTemplate
	next      int
	body      fixedBody
	current   *requestTemplate
	last      *requestTemplate
}

type fixedBody struct {
	reader *bufio.Reader
	left   int64
}

func (b *fixedBody) Read(p []byte) (int, error) {
	if b.left == 0 {
		return 0, io.EOF
	}
	n, err := b.reader.Read(p[:min(int64(len(p)), b.left)])
	b.left -= int64(n)
	if b.left == 0 {
		return n, io.EOF
	}
	if err == io.EOF {
		err = io.ErrUnexpectedEOF
	}
	return n, err
}

func (b *fixedBody) Close() error { return nil }

func (b *fixedBody) take(size int) []byte {
	if b.left != int64(size) || b.reader.Buffered() < size {
		return nil
	}
	data, _ := b.reader.Peek(size)
	_, _ = b.reader.Discard(size)
	b.left = 0
	// The returned view is valid until the connection reads again.
	return data
}

func (r *requestReader) use(entry *requestTemplate) *http.Request {
	_, _ = r.reader.Discard(len(entry.header))
	request := entry.request
	if request.ContentLength > 0 {
		r.body.reader = r.reader
		r.body.left = request.ContentLength
		request.Body = &r.body
	}
	r.current, r.last = entry, entry
	return request
}

func (r *requestReader) read() (*http.Request, error) {
	r.current = nil
	if _, err := r.reader.Peek(1); err != nil {
		return nil, err
	}
	buffer, _ := r.reader.Peek(r.reader.Buffered())
	if r.last != nil && bytes.HasPrefix(buffer, r.last.header) {
		return r.use(r.last), nil
	}
	for i := range r.templates {
		entry := &r.templates[i]
		if entry != r.last && entry.request != nil && bytes.HasPrefix(buffer, entry.header) {
			return r.use(entry), nil
		}
	}
	end := bytes.Index(buffer, []byte("\r\n\r\n"))
	if end < 0 || end+4 > 1024 {
		return http.ReadRequest(r.reader)
	}
	// Parse an independent view, so observing the header cannot read any body
	// or pipelined request. The normal parser handles unsupported framing.
	header := buffer[:end+4]
	r.header.Reset(header)
	if r.preview == nil {
		r.preview = bufio.NewReaderSize(&r.header, 1024)
	} else {
		r.preview.Reset(&r.header)
	}
	request, err := http.ReadRequest(r.preview)
	if err != nil || request.ProtoMajor != 1 || request.ProtoMinor != 1 ||
		request.Close || request.Host == "" || len(request.TransferEncoding) != 0 ||
		request.ContentLength < 0 || request.Header.Get("Expect") != "" ||
		bytes.Contains(header, []byte("\n ")) || bytes.Contains(header, []byte("\n\t")) {
		return http.ReadRequest(r.reader)
	}
	entry := &r.templates[r.next]
	r.next = (r.next + 1) % len(r.templates)
	entry.header = append(entry.header[:0], header...)
	entry.request = request
	entry.issue = issueReply{}
	entry.git = gitTarget{}
	return r.use(entry), nil
}

type wireResponse struct {
	headers http.Header
	media   string
	data    []byte
	status  int
	id      uint64
	noCache bool
	input   []byte
	request *requestTemplate
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
	w.id = 0
	w.noCache = false
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

func (w *wireResponse) send(conn net.Conn, request *http.Request, closeConn bool, buffer []byte) error {
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
	if w.id != 0 {
		buffer = append(buffer, "X-Github-Request-Id: "...)
		buffer = strconv.AppendUint(buffer, w.id, 16)
		buffer = append(buffer, "\r\n"...)
	}
	if w.noCache {
		buffer = append(buffer, "Cache-Control: no-cache\r\n"...)
	}
	if len(w.headers) != 0 {
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

func (reply issueReply) send(conn net.Conn, id uint64, scratch []byte) error {
	packet := append(scratch[:0], reply.header...)
	packet = append(packet, "X-GitHub-Request-Id: "...)
	packet = strconv.AppendUint(packet, id, 16)
	packet = append(packet, "\r\n\r\n"...)
	if len(packet)+len(reply.body) <= cap(packet) {
		_, err := conn.Write(append(packet, reply.body...))
		return err
	}
	chunks := net.Buffers{packet, reply.body}
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
	reader := requestReader{reader: bufio.NewReaderSize(in, 4096)}
	head := make([]byte, 0, 4096)
	response := wireResponse{headers: make(http.Header, 6)}
	forge, _ := s.handler.(*forge)
	var body trackedBody
	var deadline time.Time
	for {
		now := time.Now()
		// Renew in batches to avoid resetting socket timers on every request.
		if deadline.Sub(now) < 15*time.Second {
			deadline = now.Add(30 * time.Second)
			_ = conn.SetDeadline(deadline)
		}
		in.left, in.limited = maxHeader, true
		request, err := reader.read()
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
		if forge != nil && reader.current != nil {
			reply := reader.current.issue
			if reply.body != nil && reply.epoch == forge.issueEpoch.Load() {
				if reply.send(conn, forge.requests.Add(1), head) != nil {
					return
				}
				continue
			}
		}
		hasBody := request.Body != http.NoBody
		if hasBody {
			body = trackedBody{ReadCloser: request.Body}
			request.Body = &body
		}
		closeConn := request.Close
		response.request = reader.current
		if expect := request.Header.Get("Expect"); expect != "" {
			if !strings.EqualFold(expect, "100-continue") {
				sendJSON(&response, 417, fields{"message": "Expectation Failed"})
				_ = response.send(conn, request, true, head)
				return
			}
			if hasBody {
				if _, err := io.WriteString(conn, "HTTP/1.1 100 Continue\r\n\r\n"); err != nil {
					return
				}
			}
		}
		s.handler.ServeHTTP(&response, request)
		if hasBody && !body.complete {
			// Unread body bytes may precede the next request on this socket.
			closeConn = true
		}
		if err := response.send(conn, request, closeConn, head); err != nil || closeConn {
			return
		}
		response.reset()
	}
}
