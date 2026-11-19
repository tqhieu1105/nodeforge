package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
)

type selection struct {
	name, key, on string
	args          fields
	children      []selection
}

type document struct {
	tokens []string
	pos    int
	vars   fields
}

var lexer = regexp.MustCompile("(?:[\\s,]+|#[^\\r\\n]*)|(\"(?:[^\"\\\\\\r\\n]|\\\\.)*\"|\\.\\.\\.|[_A-Za-z][_0-9A-Za-z]*|-?[0-9]+(?:\\.[0-9]+)?(?:[eE][+-]?[0-9]+)?|[!$():={}\\[\\]])")
var gqlName = regexp.MustCompile("^[_A-Za-z][_0-9A-Za-z]*$")

type graphError string

func invalidGraph(message string) { panic(graphError(message)) }

func parseDocument(query string, vars fields) *document {
	query = strings.TrimPrefix(query, "\ufeff")
	if vars == nil {
		vars = fields{}
	}
	d := &document{vars: vars}
	end := 0
	for _, match := range lexer.FindAllStringSubmatchIndex(query, -1) {
		if match[0] != end {
			invalidGraph("Invalid GraphQL token")
		}
		if match[2] >= 0 {
			d.tokens = append(d.tokens, query[match[2]:match[3]])
		}
		end = match[1]
	}
	if end != len(query) {
		invalidGraph("Invalid GraphQL token")
	}
	return d
}

func (d *document) peek() string {
	if d.pos < len(d.tokens) {
		return d.tokens[d.pos]
	}
	return ""
}

func (d *document) take() string {
	token := d.peek()
	if token == "" {
		invalidGraph("Unexpected end of GraphQL document")
	}
	d.pos++
	return token
}

func (d *document) accept(token string) bool {
	if d.peek() != token {
		return false
	}
	d.pos++
	return true
}

func (d *document) expect(token string) {
	if !d.accept(token) {
		invalidGraph("Expected " + token)
	}
}

func (d *document) name() string {
	token := d.take()
	if !gqlName.MatchString(token) {
		invalidGraph("Expected GraphQL name")
	}
	return token
}

func (d *document) value(depth int) any {
	if depth >= 32 {
		invalidGraph("GraphQL value is too deep")
	}
	switch d.peek() {
	case "$":
		d.take()
		return d.vars[d.name()]
	case "{":
		d.take()
		return d.properties("}", depth+1)
	case "[":
		d.take()
		list := []any{}
		for !d.accept("]") {
			list = append(list, d.value(depth+1))
		}
		return list
	}
	token := d.take()
	if token[0] == '"' || token[0] == '-' || (token[0] >= '0' && token[0] <= '9') || token == "null" || token == "true" || token == "false" {
		var value any
		if json.Unmarshal([]byte(token), &value) != nil {
			invalidGraph("Invalid GraphQL value")
		}
		return value
	}
	if !gqlName.MatchString(token) {
		invalidGraph("Invalid GraphQL value")
	}
	return token
}

func (d *document) properties(end string, depth int) fields {
	out := fields{}
	for !d.accept(end) {
		key := d.name()
		d.expect(":")
		out[key] = d.value(depth)
	}
	return out
}

func (d *document) variableType(depth int) {
	if depth >= 32 {
		invalidGraph("GraphQL type is too deep")
	}
	if d.accept("[") {
		d.variableType(depth + 1)
		d.expect("]")
	} else {
		d.name()
	}
	d.accept("!")
}

func (d *document) selections(depth int) []selection {
	if depth >= 32 {
		invalidGraph("GraphQL selection is too deep")
	}
	d.expect("{")
	var out []selection
	for !d.accept("}") {
		if d.accept("...") {
			d.expect("on")
			out = append(out, selection{on: d.name(), children: d.selections(depth + 1)})
			continue
		}
		key := d.name()
		s := selection{name: key, key: key}
		if d.accept(":") {
			s.name = d.name()
		}
		if d.accept("(") {
			s.args = d.properties(")", 0)
		}
		if d.peek() == "{" {
			s.children = d.selections(depth + 1)
		}
		out = append(out, s)
	}
	if len(out) == 0 {
		invalidGraph("Empty GraphQL selection")
	}
	return out
}

func (d *document) variables() {
	if !d.accept("(") {
		return
	}
	for !d.accept(")") {
		d.expect("$")
		name := d.name()
		d.expect(":")
		d.variableType(0)
		if d.accept("=") {
			fallback := d.value(0)
			if _, ok := d.vars[name]; !ok {
				d.vars[name] = fallback
			}
		}
	}
}

func (d *document) parse() (string, []selection) {
	kind := "Query"
	if d.peek() != "{" {
		switch d.name() {
		case "query":
		case "mutation":
			kind = "Mutation"
		default:
			invalidGraph("Unsupported GraphQL operation")
		}
		if d.peek() != "(" && d.peek() != "{" {
			d.name()
		}
		d.variables()
	}
	selections := d.selections(0)
	if d.peek() != "" {
		invalidGraph("Unexpected tokens after operation")
	}
	return kind, selections
}

var graphSchema = map[string]map[string]string{
	"Query":          {"viewer": "User", "repository": "Repository", "node": "Node", "__type": "__Type"},
	"Mutation":       {"createIssue": "IssuePayload", "closeIssue": "IssuePayload", "addComment": "CommentPayload", "createPullRequest": "PullPayload", "mergePullRequest": "PullPayload"},
	"Node":           {"id": ""},
	"User":           {"login": ""},
	"Repository":     {"id": "", "databaseId": "", "name": "", "nameWithOwner": "", "owner": "User", "defaultBranchRef": "Ref", "viewerPermission": "", "hasIssuesEnabled": "", "issue": "Issue", "issues": "Issues", "pullRequest": "PullRequest", "pullRequests": "PullRequests"},
	"Ref":            {"name": ""},
	"Issue":          {"id": "", "number": "", "title": "", "body": "", "state": ""},
	"PullRequest":    {"id": "", "number": "", "title": "", "body": "", "state": "", "merged": "", "mergeable": "", "headRefName": "", "baseRefName": "", "files": "Files"},
	"IssueComment":   {"id": "", "body": ""},
	"Issues":         {"nodes": "Issue", "edges": "IssueEdge", "pageInfo": "PageInfo", "totalCount": ""},
	"IssueEdge":      {"node": "Issue", "cursor": ""},
	"PullRequests":   {"nodes": "PullRequest", "edges": "PullEdge", "pageInfo": "PageInfo", "totalCount": ""},
	"PullEdge":       {"node": "PullRequest", "cursor": ""},
	"Files":          {"nodes": "File", "edges": "FileEdge", "pageInfo": "PageInfo", "totalCount": ""},
	"FileEdge":       {"node": "File", "cursor": ""},
	"File":           {"path": "", "additions": "", "deletions": ""},
	"PageInfo":       {"endCursor": "", "startCursor": "", "hasNextPage": "", "hasPreviousPage": ""},
	"__Type":         {"name": "", "kind": "", "fields": "__Field", "enumValues": "__EnumValue"},
	"__Field":        {"name": ""},
	"__EnumValue":    {"name": ""},
	"IssuePayload":   {"issue": "Issue"},
	"PullPayload":    {"pullRequest": "PullRequest"},
	"CommentPayload": {"commentEdge": "CommentEdge"},
	"CommentEdge":    {"node": "IssueComment"},
}

func validateGraph(kind string, selections []selection) {
	schema := graphSchema[kind]
	for _, s := range selections {
		if s.on != "" {
			if graphSchema[s.on] == nil {
				invalidGraph("Unknown type " + s.on)
			}
			validateGraph(s.on, s.children)
			continue
		}
		if s.name == "__typename" && len(s.children) == 0 {
			continue
		}
		nested, ok := schema[s.name]
		if !ok {
			invalidGraph(fmt.Sprintf("Cannot query field %q on type %q.", s.name, kind))
		}
		if (nested != "") != (len(s.children) > 0) {
			invalidGraph(fmt.Sprintf("Invalid selection for field %q.", s.name))
		}
		if nested != "" {
			validateGraph(nested, s.children)
		}
	}
}

type graphView struct {
	kind    string
	values  fields
	resolve func(string, fields) any
}

func view(kind string, values fields) *graphView {
	return &graphView{kind: kind, values: values}
}

func project(v *graphView, selections []selection) fields {
	if v == nil {
		return nil
	}
	out := fields{}
	for _, s := range selections {
		if s.on != "" {
			if s.on == v.kind || s.on == "Node" {
				for k, value := range project(v, s.children) {
					out[k] = value
				}
			}
			continue
		}
		if s.name == "__typename" {
			out[s.key] = v.kind
			continue
		}
		value, ok := v.values[s.name]
		if !ok && v.resolve != nil {
			value = v.resolve(s.name, s.args)
		}
		switch value := value.(type) {
		case *graphView:
			out[s.key] = project(value, s.children)
		case []*graphView:
			items := make([]fields, 0, len(value))
			for _, item := range value {
				items = append(items, project(item, s.children))
			}
			out[s.key] = items
		default:
			out[s.key] = value
		}
	}
	return out
}

func globalID(kind string, repo, number int) string {
	return base64.RawURLEncoding.EncodeToString([]byte(kind + ":" + strconv.Itoa(repo) + ":" + strconv.Itoa(number)))
}

func (f *forge) lookupID(value any, expected string) (string, *repository, int) {
	raw, err := base64.RawURLEncoding.DecodeString(text(value))
	if err != nil {
		return "", nil, 0
	}
	kind, tail, _ := strings.Cut(string(raw), ":")
	a, b, _ := strings.Cut(tail, ":")
	id, number := integer(a), integer(b)
	if (expected != "" && kind != expected) || globalID(kind, id, number) != value {
		return "", nil, 0
	}
	return kind, f.repoIDs[id], number
}

type edge struct {
	key  string
	node *graphView
}

func cursor(key string) string { return base64.RawURLEncoding.EncodeToString([]byte("cursor:" + key)) }

func connection(kind, edgeKind string, items []edge, args fields) *graphView {
	offset, count := 0, 30
	if after := text(args["after"]); after != "" {
		for i, item := range items {
			if cursor(item.key) == after {
				offset = i + 1
				break
			}
		}
	}
	if number, ok := args["first"].(float64); ok {
		count = max(0, min(100, integer(number)))
	}
	page := items[offset:min(offset+count, len(items))]
	nodes, edges := make([]*graphView, 0, len(page)), make([]*graphView, 0, len(page))
	for _, item := range page {
		nodes = append(nodes, item.node)
		edges = append(edges, view(edgeKind, fields{"node": item.node, "cursor": cursor(item.key)}))
	}
	var start, end any
	if len(page) > 0 {
		start, end = cursor(page[0].key), cursor(page[len(page)-1].key)
	}
	info := view("PageInfo", fields{
		"hasNextPage":     offset+len(page) < len(items),
		"hasPreviousPage": offset > 0,
		"startCursor":     start,
		"endCursor":       end,
	})
	return view(kind, fields{"nodes": nodes, "edges": edges, "totalCount": len(items), "pageInfo": info})
}

func issueView(r *repository, i *issue) *graphView {
	if i == nil {
		return nil
	}
	return view("Issue", fields{
		"id":     globalID("Issue", r.info.ID, i.Number),
		"number": i.Number,
		"title":  i.Title,
		"body":   i.Body,
		"state":  strings.ToUpper(i.State),
	})
}

func pullState(p *pull) string {
	if p.Merged {
		return "MERGED"
	}
	return strings.ToUpper(p.State)
}

func pullView(r *repository, p *pull) *graphView {
	if p == nil {
		return nil
	}
	v := view("PullRequest", fields{
		"id":          globalID("PullRequest", r.info.ID, p.Number),
		"number":      p.Number,
		"title":       p.Title,
		"body":        p.Body,
		"state":       pullState(p),
		"merged":      p.Merged,
		"headRefName": p.Head.Ref,
		"baseRefName": p.Base.Ref,
	})
	v.resolve = func(name string, args fields) any {
		if name == "mergeable" {
			if p.Merged {
				return "MERGEABLE"
			}
			if plan, _, _ := r.mergePlan(p); plan != nil {
				return "MERGEABLE"
			}
			return "CONFLICTING"
		}
		files := r.pullFiles(p)
		items := make([]edge, 0, len(files))
		for _, file := range files {
			path := text(file["filename"])
			items = append(items, edge{path, view("File", fields{"path": path, "additions": file["additions"], "deletions": file["deletions"]})})
		}
		return connection("Files", "FileEdge", items, args)
	}
	return v
}

func stateMatches(input any, state string) bool {
	if states, ok := input.([]any); ok {
		for _, candidate := range states {
			if text(candidate) == state {
				return true
			}
		}
		return false
	}
	return true
}

func repoView(r *repository) *graphView {
	if r == nil {
		return nil
	}
	info := r.info
	var branch *graphView
	if r.refs[headRef(info.DefaultBranch)] != "" {
		branch = view("Ref", fields{"name": info.DefaultBranch})
	}
	v := view("Repository", fields{
		"id":               globalID("Repository", info.ID, 0),
		"databaseId":       info.ID,
		"name":             info.Name,
		"nameWithOwner":    info.FullName,
		"owner":            view("User", fields{"login": info.Owner.Login}),
		"viewerPermission": "ADMIN",
		"hasIssuesEnabled": true,
		"defaultBranchRef": branch,
	})
	v.resolve = func(name string, args fields) any {
		switch name {
		case "issue":
			return issueView(r, r.issues[integer(args["number"])])
		case "pullRequest":
			return pullView(r, r.pulls[integer(args["number"])])
		case "issues":
			order, _ := args["orderBy"].(fields)
			items := []edge{}
			for _, i := range listIssues(r, "all", order["direction"] == "ASC") {
				if stateMatches(args["states"], strings.ToUpper(i.State)) {
					items = append(items, edge{strconv.Itoa(i.Number), issueView(r, i)})
				}
			}
			return connection("Issues", "IssueEdge", items, args)
		case "pullRequests":
			items := []edge{}
			for _, p := range pullList(r, "all") {
				if stateMatches(args["states"], pullState(p)) {
					items = append(items, edge{strconv.Itoa(p.Number), pullView(r, p)})
				}
			}
			return connection("PullRequests", "PullEdge", items, args)
		}
		return nil
	}
	return v
}

func typeView(name string) *graphView {
	if name == "SearchType" {
		values := []*graphView{}
		for _, item := range []string{"ISSUE", "REPOSITORY", "USER"} {
			values = append(values, view("__EnumValue", fields{"name": item}))
		}
		return view("__Type", fields{"name": name, "kind": "ENUM", "enumValues": values})
	}
	schema := graphSchema[name]
	if schema == nil {
		return nil
	}
	kind := "OBJECT"
	if name == "Node" {
		kind = "INTERFACE"
	}
	entries := make([]*graphView, 0, len(schema))
	for _, item := range sortedKeys(schema) {
		entries = append(entries, view("__Field", fields{"name": item}))
	}
	return view("__Type", fields{"name": name, "kind": kind, "fields": entries})
}

func (f *forge) queryView() *graphView {
	v := view("Query", fields{"viewer": view("User", fields{"login": f.login})})
	v.resolve = func(name string, args fields) any {
		switch name {
		case "__type":
			return typeView(text(args["name"]))
		case "repository":
			return repoView(f.findRepo(text(args["owner"]), text(args["name"])))
		case "node":
			kind, r, num := f.lookupID(args["id"], "")
			if r == nil {
				return nil
			}
			switch kind {
			case "Repository":
				return repoView(r)
			case "Issue":
				return issueView(r, r.issues[num])
			case "PullRequest":
				return pullView(r, r.pulls[num])
			}
		}
		return nil
	}
	return v
}

func (f *forge) mutationView() *graphView {
	v := view("Mutation", nil)
	v.resolve = func(name string, args fields) any {
		in, ok := args["input"].(fields)
		if !ok {
			invalidGraph("Expected mutation input")
		}
		switch name {
		case "createIssue", "createPullRequest":
			_, r, _ := f.lookupID(in["repositoryId"], "Repository")
			if r == nil {
				missing()
			}
			if name == "createIssue" {
				item := f.createIssue(r, inputIssue(in))
				return view("IssuePayload", fields{"issue": issueView(r, item)})
			}
			p := f.createPull(r, fields{"title": in["title"], "body": in["body"], "head": in["headRefName"], "base": in["baseRefName"]})
			return view("PullPayload", fields{"pullRequest": pullView(r, p)})
		case "mergePullRequest":
			_, r, num := f.lookupID(in["pullRequestId"], "PullRequest")
			if r == nil || r.pulls[num] == nil {
				missing()
			}
			p := r.pulls[num]
			r.merge(p, strings.ToLower(text(in["mergeMethod"])))
			return view("PullPayload", fields{"pullRequest": pullView(r, p)})
		case "closeIssue", "addComment":
			key := "issueId"
			if name == "addComment" {
				key = "subjectId"
			}
			_, r, num := f.lookupID(in[key], "Issue")
			if r == nil || r.issues[num] == nil {
				missing()
			}
			if name == "closeIssue" {
				i := updateIssue(r, r.issues[num], fields{"state": "closed"})
				return view("IssuePayload", fields{"issue": issueView(r, i)})
			}
			c := f.addComment(r, num, in["body"])
			node := view("IssueComment", fields{"id": globalID("IssueComment", r.info.ID, c.ID), "body": c.Body})
			return view("CommentPayload", fields{"commentEdge": view("CommentEdge", fields{"node": node})})
		}
		return nil
	}
	return v
}

func (f *forge) executeGraph(input any) (result fields) {
	defer func() {
		if e := recover(); e != nil {
			var message string
			switch e := e.(type) {
			case graphError:
				message = string(e)
			case fault:
				message = e.message
			default:
				panic(e)
			}
			result = fields{"errors": []fields{{"message": message}}}
		}
	}()
	in, _ := input.(fields)
	query := text(in["query"])
	if query == "" {
		invalidGraph("GraphQL query is required")
	}
	vars, _ := in["variables"].(fields)
	kind, selections := parseDocument(query, vars).parse()
	validateGraph(kind, selections)
	root := f.queryView()
	if kind == "Mutation" {
		root = f.mutationView()
	}
	return fields{"data": project(root, selections)}
}

func (f *forge) graphql(w http.ResponseWriter, r *http.Request) {
	var input any
	if !decodeJSON(w, r, &input) {
		fail(400, "Malformed JSON")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if batch, ok := input.([]any); ok {
		out := make([]fields, 0, len(batch))
		for _, op := range batch {
			out = append(out, f.executeGraph(op))
		}
		sendJSON(w, 200, out)
		return
	}
	sendJSON(w, 200, f.executeGraph(input))
}
