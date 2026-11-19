package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type fields = map[string]any

type fault struct {
	status  int
	message string
}

func fail(status int, message string) {
	panic(fault{status: status, message: message})
}

func missing() { fail(404, "Not Found") }

func sendJSON(w http.ResponseWriter, status int, v any) {
	data, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	sendBytes(w, status, jsonMedia, data)
}

func sendBytes(w http.ResponseWriter, status int, media string, data []byte) {
	if response, ok := w.(*wireResponse); ok {
		response.media = media
		response.status = status
		response.data = data
		return
	}
	w.Header().Set("Content-Type", media)
	if status != http.StatusOK || len(data) == 0 {
		w.WriteHeader(status)
	}
	if len(data) > 0 {
		_, _ = w.Write(data)
	}
}

const jsonBodyLimit = 16 << 20

func requestBody(w http.ResponseWriter, r *http.Request, limit int64) []byte {
	size := r.ContentLength
	if size > limit {
		fail(400, "Invalid request body")
	}
	if size > 0 && size <= 16<<10 {
		data := make([]byte, int(size))
		if _, err := io.ReadFull(r.Body, data); err != nil {
			fail(400, "Invalid request body")
		}
		return data
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
	if err != nil {
		fail(400, "Invalid request body")
	}
	return data
}

func decodeJSON(w http.ResponseWriter, r *http.Request, out any) bool {
	if r.ContentLength > jsonBodyLimit {
		return false
	}
	return json.Unmarshal(requestBody(w, r, jsonBodyLimit), out) == nil
}

func readInput(w http.ResponseWriter, r *http.Request) fields {
	var input fields
	if !decodeJSON(w, r, &input) || input == nil {
		fail(400, "Expected a JSON object")
	}
	return input
}

func text(v any) string {
	value, _ := v.(string)
	return value
}

func integer(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case float64:
		if float64(int(n)) == n {
			return int(n)
		}
	case string:
		value, _ := strconv.Atoi(n)
		return value
	}
	return 0
}

type forge struct {
	mu            sync.RWMutex
	authorization string
	login         string
	repos         map[string]*repository
	repoIDs       map[int]*repository
	id            int
	requests      atomic.Uint64
}

func newForge() *forge {
	login := os.Getenv("FORGE_LOGIN")
	if login == "" {
		login = "adaiversen"
	}
	authorization := os.Getenv("MAKEFUN_SHOP_TOKEN")
	if authorization != "" {
		authorization = "Bearer " + authorization
	}
	return &forge{
		authorization: authorization,
		login:         login,
		repos:         make(map[string]*repository),
		repoIDs:       make(map[int]*repository),
	}
}

func (f *forge) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	defer func() {
		if e := recover(); e != nil {
			if err, ok := e.(fault); ok {
				sendJSON(w, err.status, fields{"message": err.message})
				return
			}
			log.Printf("request failed: %v", e)
			sendJSON(w, 500, fields{"message": "Internal Server Error"})
		}
	}()
	path := r.URL.Path
	isREST := path == "/api/v3" || strings.HasPrefix(path, "/api/v3/")
	if isREST {
		requestID := strconv.FormatUint(f.requests.Add(1), 16)
		w.Header().Set("X-GitHub-Request-Id", requestID)
	}
	if path == "/healthz" && r.Method == "GET" {
		sendBytes(w, 200, jsonMedia, []byte("{\"ok\":true}"))
		return
	}
	if f.authorization == "" || r.Header.Get("Authorization") != f.authorization {
		fail(401, "Bad credentials")
	}
	switch {
	case isREST:
		f.rest(w, r)
	case r.Method == "POST" && path == "/api/graphql":
		f.graphql(w, r)
	case r.Method == "POST" && path == "/mcp":
		f.mcp(w, r)
	case strings.Contains(path, ".git/"):
		f.smartHTTP(w, r)
	default:
		missing()
	}
}

func retryDelay(attempt int) time.Duration {
	return min(2*time.Second, 250*time.Millisecond<<min(attempt, 3))
}

func exchange(client *http.Client, method, url string, body []byte) (*http.Response, error) {
	req, err := http.NewRequest(method, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	if method == "POST" {
		req.Header.Set("Content-Type", "application/json")
	}
	return client.Do(req)
}

func register(client *http.Client, env string, port int) error {
	body := []byte(fmt.Sprintf("{\"port\":%d}", port))
	for attempt := 0; ; attempt++ {
		res, err := exchange(client, "POST", env+"/register", body)
		if err == nil {
			_, _ = io.Copy(io.Discard, res.Body)
			res.Body.Close()
			switch res.StatusCode {
			case 200, 409:
				return nil
			case 400, 405:
				return fmt.Errorf("register answered HTTP %d", res.StatusCode)
			}
		}
		time.Sleep(retryDelay(attempt))
	}
}

func waitDone(client *http.Client, env string) {
	for attempt := 0; ; attempt++ {
		res, err := exchange(client, "GET", env+"/done", nil)
		if err == nil {
			var result struct{ Done bool }
			ok := res.StatusCode == 200 && json.NewDecoder(res.Body).Decode(&result) == nil
			res.Body.Close()
			if ok && result.Done {
				return
			}
		}
		time.Sleep(retryDelay(attempt))
	}
}

func run(local bool) error {
	env := strings.TrimRight(os.Getenv("MAKEFUN_ENV_URL"), "/")
	if !local && env == "" {
		return errors.New("MAKEFUN_ENV_URL is not set")
	}
	// Keep a CPU available to the suite in its two-CPU trial.
	runtime.GOMAXPROCS(1)
	address := ":0"
	if local {
		address = "127.0.0.1:0"
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return err
	}
	server := newHTTPListener(listener, newForge())
	stopped := make(chan error, 1)
	go func() { stopped <- server.serve() }()
	port := listener.Addr().(*net.TCPAddr).Port
	if local {
		fmt.Println(port)
		return <-stopped
	}
	log.Printf("listening on port %d", port)
	client := &http.Client{Timeout: 30 * time.Second}
	if err = register(client, env, port); err != nil {
		server.stop()
		return err
	}
	waitDone(client, env)
	server.stop()
	log.Print("done")
	return nil
}

func main() {
	local := len(os.Args) == 2 && os.Args[1] == "--local"
	if err := run(local); err != nil {
		log.Fatal(err)
	}
}
