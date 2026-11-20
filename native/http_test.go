package main

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
)

func TestRequestReaderReusesHeadersAndReadsCurrentBody(t *testing.T) {
	post := "POST /api/v3/user/repos HTTP/1.1\r\nHost: forge\r\nAuthorization: Bearer one\r\nContent-Length: 3\r\n\r\n"
	get := "GET /api/v3/user HTTP/1.1\r\nHost: forge\r\nAuthorization: Bearer two\r\n\r\n"
	reader := requestReader{reader: bufio.NewReader(strings.NewReader(post + "one" + get + post + "two" + get))}
	for index, want := range []struct {
		method, authorization, body string
	}{
		{"POST", "Bearer one", "one"},
		{"GET", "Bearer two", ""},
		{"POST", "Bearer one", "two"},
		{"GET", "Bearer two", ""},
	} {
		request, err := reader.read()
		if err != nil {
			t.Fatalf("request %d: %v", index, err)
		}
		if request.Method != want.method || request.Header.Get("Authorization") != want.authorization {
			t.Fatalf("request %d: method %q, authorization %q", index, request.Method, request.Header.Get("Authorization"))
		}
		body := trackedBody{ReadCloser: request.Body}
		request.Body = &body
		got := requestBody(&wireResponse{}, request, 16)
		if !body.complete || string(got) != want.body {
			t.Fatalf("request %d: body %q, complete %t", index, got, body.complete)
		}
	}
	if _, err := reader.read(); err != io.EOF {
		t.Fatalf("end of connection: %v", err)
	}
}

func TestRequestReaderFallsBackForChunkedBodies(t *testing.T) {
	chunked := "POST /mcp HTTP/1.1\r\nHost: forge\r\nTransfer-Encoding: chunked\r\n\r\n3\r\nabc\r\n0\r\n\r\n"
	reader := requestReader{reader: bufio.NewReader(strings.NewReader(chunked + chunked))}
	for i := range 2 {
		request, err := reader.read()
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		if _, ok := request.Body.(*fixedBody); ok {
			t.Fatalf("request %d: chunked body was treated as fixed length", i)
		}
		body, err := io.ReadAll(request.Body)
		if err != nil || string(body) != "abc" {
			t.Fatalf("request %d: body %q, error %v", i, body, err)
		}
	}
}

func TestFixedBodyRejectsTruncation(t *testing.T) {
	body := fixedBody{reader: bufio.NewReader(strings.NewReader("ab")), left: 3}
	if data, err := io.ReadAll(&body); err != io.ErrUnexpectedEOF || string(data) != "ab" {
		t.Fatalf("body %q, error %v", data, err)
	}
}

func TestRequestTemplateKeepsSeparateAuthorization(t *testing.T) {
	first := "GET /healthz HTTP/1.1\r\nHost: forge\r\nAuthorization: Bearer alpha\r\n\r\n"
	second := "GET /healthz HTTP/1.1\r\nHost: forge\r\nAuthorization: Bearer beta\r\n\r\n"
	reader := requestReader{reader: bufio.NewReader(strings.NewReader(first + second + first))}
	for _, want := range []string{"Bearer alpha", "Bearer beta", "Bearer alpha"} {
		request, err := reader.read()
		if err != nil {
			t.Fatal(err)
		}
		if request.Body != http.NoBody || request.Header.Get("Authorization") != want {
			t.Fatalf("expected bodyless request with %q, got %q", want, request.Header.Get("Authorization"))
		}
	}
}

func TestIssueResponseCacheInvalidatesOnUpdateAndDelete(t *testing.T) {
	t.Setenv("MAKEFUN_SHOP_TOKEN", "test-token")
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	forge := newForge()
	repo := forge.createRepo(fields{"name": "cached"})
	item := forge.createIssue(repo, issueInput{Title: "before"})
	server := newHTTPListener(listener, forge)
	defer server.stop()
	go func() { _ = server.serve() }()
	conn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	reader := bufio.NewReader(conn)
	head := "GET /api/v3/repos/" + forge.login + "/cached/issues/1 HTTP/1.1\r\nHost: forge\r\nAuthorization: Bearer test-token\r\n\r\n"
	read := func(status int, contains string) {
		t.Helper()
		if _, err := io.WriteString(conn, head); err != nil {
			t.Fatal(err)
		}
		response, err := http.ReadResponse(reader, &http.Request{Method: "GET"})
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(response.Body)
		response.Body.Close()
		if err != nil || response.StatusCode != status || !strings.Contains(string(body), contains) ||
			response.Header.Get("X-GitHub-Request-Id") == "" {
			t.Fatalf("status %d, body %q, error %v", response.StatusCode, body, err)
		}
	}
	read(200, "before")
	read(200, "before")
	forge.mu.Lock()
	forge.updateIssue(repo, item, fields{"title": "after"})
	forge.mu.Unlock()
	read(200, "after")
	read(200, "after")
	deleteRequest, err := http.NewRequest("DELETE", "http://"+listener.Addr().String()+"/api/v3/repos/"+forge.login+"/cached", nil)
	if err != nil {
		t.Fatal(err)
	}
	deleteRequest.Header.Set("Authorization", "Bearer test-token")
	response, err := http.DefaultClient.Do(deleteRequest)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("delete status %d", response.StatusCode)
	}
	read(404, "Not Found")
}
