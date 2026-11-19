package main

import (
	"encoding/base64"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

const jsonMedia = "application/json; charset=utf-8"

func stateOf(r *http.Request) string {
	if state := r.URL.Query().Get("state"); state != "" {
		return state
	}
	return "open"
}

func page(r *http.Request, n int) (start, end, number int) {
	q := r.URL.Query()
	limit := integer(q.Get("per_page"))
	number = integer(q.Get("page"))
	if limit < 1 {
		limit = 30
	}
	limit = min(limit, 100)
	number = max(number, 1)
	if number-1 > n/limit {
		return n, n, number
	}
	start = min(n, (number-1)*limit)
	return start, min(n, start+limit), number
}

func (f *forge) rest(w http.ResponseWriter, req *http.Request) {
	path := req.URL.Path
	method := req.Method
	switch path {
	case "/api/v3", "/api/v3/":
		if method == "GET" {
			w.Header().Set("X-OAuth-Scopes", "repo, read:org")
			sendJSON(w, 200, fields{"current_user_url": "/api/v3/user"})
			return
		}
	case "/api/v3/meta":
		if method == "GET" {
			sendJSON(w, 200, fields{"installed_version": "1.0.0"})
			return
		}
	case "/api/v3/user":
		if method == "GET" {
			sendJSON(w, 200, user{Login: f.login})
			return
		}
	case "/api/v3/user/repos":
		switch method {
		case "POST":
			input := readInput(w, req)
			f.mu.Lock()
			info := func() repoInfo {
				defer f.mu.Unlock()
				return f.createRepo(input).info
			}()
			sendJSON(w, 201, info)
			return
		case "GET":
			f.listRepos(w, req)
			return
		}
	}
	repoPath, ok := strings.CutPrefix(path, "/api/v3/repos/")
	owner, tail, hasRepo := strings.Cut(repoPath, "/")
	if !ok || !hasRepo {
		missing()
	}
	name, sub, _ := strings.Cut(tail, "/")
	if sub == "issues" && method == "POST" {
		var input *issueInput
		if !decodeJSON(w, req, &input) || input == nil {
			fail(400, "Expected a JSON object")
		}
		f.mu.Lock()
		item := func() *issue {
			defer f.mu.Unlock()
			return f.createIssue(f.requireRepo(owner, name), *input)
		}()
		sendBytes(w, 201, jsonMedia, item.wire)
		return
	}
	if method == "GET" {
		if item, ok := strings.CutPrefix(sub, "issues/"); ok {
			if number, err := strconv.Atoi(item); err == nil {
				f.issueRead(w, owner, name, number)
				return
			}
		}
	}
	var input fields
	if method == "POST" || method == "PUT" || method == "PATCH" || (method == "DELETE" && strings.HasPrefix(sub, "contents/")) {
		input = readInput(w, req)
	}
	if method == "GET" {
		f.mu.RLock()
		defer f.mu.RUnlock()
	} else {
		f.mu.Lock()
		defer f.mu.Unlock()
	}
	repo := f.requireRepo(owner, name)
	if sub == "" {
		switch method {
		case "GET":
			sendJSON(w, 200, repo.info)
			return
		case "PATCH":
			if description, ok := input["description"].(string); ok {
				repo.info.Description = description
			}
			sendJSON(w, 200, repo.info)
			return
		case "DELETE":
			delete(f.repos, strings.ToLower(name))
			delete(f.repoIDs, repo.info.ID)
			sendBytes(w, 204, jsonMedia, nil)
			return
		}
	}
	switch sub {
	case "git/refs":
		if method == "POST" {
			ref, sha := text(input["ref"]), text(input["sha"])
			obj := repo.objects[sha]
			if !strings.HasPrefix(ref, "refs/heads/") || !validRef(ref) || obj == nil || obj.tree == "" || repo.refs[ref] != "" {
				fail(422, "Invalid branch reference")
			}
			repo.setRef(ref, sha)
			sendJSON(w, 201, fields{"ref": ref, "object": fields{"sha": sha}})
			return
		}
	case "branches":
		if method == "GET" {
			items := []fields{}
			for _, ref := range sortedKeys(repo.refs) {
				if name, ok := strings.CutPrefix(ref, "refs/heads/"); ok {
					items = append(items, fields{"name": name, "commit": fields{"sha": repo.refs[ref]}})
				}
			}
			sendJSON(w, 200, items)
			return
		}
	case "commits":
		if method == "GET" {
			tip := repo.resolve(req.URL.Query().Get("sha"))
			sendJSON(w, 200, repo.commits(tip))
			return
		}
	case "labels":
		if method == "POST" {
			name, color := text(input["name"]), text(input["color"])
			if name == "" || color == "" {
				fail(422, "Invalid label")
			}
			value := label{Name: name, Color: color}
			repo.labels[name] = value
			sendJSON(w, 201, value)
			return
		}
	case "issues":
		if method == "GET" {
			items := listIssues(repo, stateOf(req), req.URL.Query().Get("direction") == "asc")
			start, end, _ := page(req, len(items))
			sendJSON(w, 200, items[start:end])
			return
		}
	case "pulls":
		switch method {
		case "POST":
			sendJSON(w, 201, f.createPull(repo, input))
			return
		case "GET":
			items := pullList(repo, stateOf(req))
			start, end, _ := page(req, len(items))
			sendJSON(w, 200, items[start:end])
			return
		}
	}
	if sub == "contents" || strings.HasPrefix(sub, "contents/") {
		file := strings.TrimPrefix(strings.TrimPrefix(sub, "contents"), "/")
		switch method {
		case "GET":
			tip := repo.resolve(req.URL.Query().Get("ref"))
			sendJSON(w, 200, repo.getContent(file, tip))
			return
		case "PUT", "DELETE":
			status, data := repo.changeContent(file, input, method == "DELETE")
			sendJSON(w, status, data)
			return
		}
	}
	kind, numberPath, _ := strings.Cut(sub, "/")
	numberText, endpoint, _ := strings.Cut(numberPath, "/")
	number := integer(numberText)
	switch kind {
	case "issues":
		item := repo.issues[number]
		if item == nil {
			missing()
		}
		switch endpoint {
		case "":
			if method == "PATCH" {
				sendBytes(w, 200, jsonMedia, updateIssue(repo, item, input).wire)
				return
			}
		case "comments":
			switch method {
			case "GET":
				items := repo.comments[number]
				if items == nil {
					items = []comment{}
				}
				sendJSON(w, 200, items)
				return
			case "POST":
				sendJSON(w, 201, f.addComment(repo, number, input["body"]))
				return
			}
		}
	case "pulls":
		p := repo.pulls[number]
		if p == nil {
			missing()
		}
		switch {
		case endpoint == "" && method == "GET":
			sendJSON(w, 200, p)
			return
		case endpoint == "files" && method == "GET":
			sendJSON(w, 200, repo.pullFiles(p))
			return
		case endpoint == "merge" && method == "PUT":
			sendJSON(w, 200, repo.merge(p, text(input["merge_method"])))
			return
		}
	}
	missing()
}

func (f *forge) issueRead(w http.ResponseWriter, owner, name string, number int) {
	f.mu.RLock()
	repo := f.findRepo(owner, name)
	var item *issue
	if repo != nil {
		item = repo.issues[number]
	}
	f.mu.RUnlock()
	if item == nil {
		missing()
	}
	sendBytes(w, 200, jsonMedia, item.wire)
}

func (f *forge) listRepos(w http.ResponseWriter, r *http.Request) {
	f.mu.RLock()
	list := make([]repoInfo, 0, len(f.repos))
	for _, repo := range f.repos {
		list = append(list, repo.info)
	}
	f.mu.RUnlock()
	sort.Slice(list, func(i, j int) bool { return list[i].ID < list[j].ID })
	start, end, number := page(r, len(list))
	if end < len(list) {
		next := url.URL{Scheme: "http", Host: r.Host, Path: r.URL.Path}
		q := r.URL.Query()
		q.Set("page", strconv.Itoa(number+1))
		next.RawQuery = q.Encode()
		w.Header().Set("Link", "<"+next.String()+">; rel=\"next\"")
	}
	sendJSON(w, 200, list[start:end])
}

func (repo *repository) getContent(path, tip string) any {
	commit := repo.objects[tip]
	if commit == nil || commit.tree == "" {
		missing()
	}
	sha, mode, name := commit.tree, "40000", ""
	if path != "" {
		for segment := range strings.SplitSeq(path, "/") {
			if mode != "40000" {
				missing()
			}
			found := false
			for _, entry := range repo.objects[sha].entries() {
				if entry.name == segment {
					sha, mode, name = entry.sha, entry.mode, entry.name
					found = true
					break
				}
			}
			if !found {
				missing()
			}
		}
	}
	if mode == "40000" {
		items := []fields{}
		for _, entry := range repo.objects[sha].entries() {
			kind, itemPath := "file", entry.name
			if entry.mode == "40000" {
				kind = "dir"
			}
			if path != "" {
				itemPath = path + "/" + entry.name
			}
			items = append(items, fields{"type": kind, "name": entry.name, "path": itemPath, "sha": entry.sha})
		}
		return items
	}
	obj := repo.objects[sha]
	if obj == nil || obj.kind != blobKind {
		missing()
	}
	return fields{
		"type":     "file",
		"name":     name,
		"path":     path,
		"sha":      sha,
		"size":     len(obj.data),
		"encoding": "base64",
		"content":  base64.StdEncoding.EncodeToString(obj.data),
	}
}

func (repo *repository) changeContent(path string, input fields, remove bool) (int, fields) {
	message, ok := input["message"].(string)
	if !ok {
		fail(400, "Invalid file contents")
	}
	for part := range strings.SplitSeq(path, "/") {
		if part == "" || part == "." || part == ".." || strings.ContainsRune(part, 0) {
			fail(400, "Invalid file contents")
		}
	}
	branch := text(input["branch"])
	if branch == "" {
		branch = repo.info.DefaultBranch
	}
	ref := headRef(branch)
	parent := repo.refs[ref]
	if parent == "" && branch != repo.info.DefaultBranch {
		missing()
	}
	files := repo.objects.snapshot(parent)
	old := files[path]
	if remove && old.sha == "" {
		missing()
	}
	if old.sha != text(input["sha"]) {
		fail(409, "Content SHA does not match")
	}
	var blob string
	if remove {
		delete(files, path)
	} else {
		content, ok := input["content"].(string)
		if !ok {
			fail(400, "Invalid file contents")
		}
		data, err := base64.StdEncoding.DecodeString(content)
		if err != nil {
			data, err = base64.RawStdEncoding.DecodeString(content)
		}
		if err != nil {
			fail(400, "Invalid Base64 content")
		}
		blob = repo.objects.save(blobKind, data)
		mode := old.mode
		if mode == "" {
			mode = "100644"
		}
		files[path] = fileEntry{mode: mode, sha: blob}
		if conflictPaths(files) {
			fail(409, "Conflicting file path")
		}
	}
	commit := repo.commit(files, parent, message)
	repo.setRef(ref, commit)
	result := fields{"commit": fields{"sha": commit}}
	if remove {
		return 200, result
	}
	result["content"] = fields{"sha": blob, "encoding": "base64"}
	if old.sha != "" {
		return 200, result
	}
	return 201, result
}
