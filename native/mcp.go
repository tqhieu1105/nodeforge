package main

import (
	"encoding/json"
	"net/http"
)

const mcpRevision = "2026-07-28"

type tool struct {
	name        string
	description string
	fields      map[string]string
	required    []string
}

var mcpTools = []tool{
	{"create_repo", "Create a repository for the authenticated user.", map[string]string{"name": "string", "description": "string", "private": "boolean"}, []string{"name"}},
	{"get_repo", "Read a repository by owner and name.", map[string]string{"owner": "string", "repo": "string"}, []string{"owner", "repo"}},
	{"create_issue", "Open an issue in a repository.", map[string]string{"owner": "string", "repo": "string", "title": "string", "body": "string"}, []string{"owner", "repo", "title"}},
	{"list_issues", "List issues in a repository with an optional state filter.", map[string]string{"owner": "string", "repo": "string", "state": "string"}, []string{"owner", "repo"}},
	{"create_pull_request", "Open a pull request from one branch into another.", map[string]string{"owner": "string", "repo": "string", "title": "string", "head": "string", "base": "string", "body": "string"}, []string{"owner", "repo", "title", "head", "base"}},
	{"merge_pull_request", "Squash merge an open pull request into its base branch.", map[string]string{"owner": "string", "repo": "string", "pull_number": "integer"}, []string{"owner", "repo", "pull_number"}},
}

var toolList = func() fields {
	items := make([]fields, 0, len(mcpTools))
	for _, t := range mcpTools {
		properties := fields{}
		for key, kind := range t.fields {
			properties[key] = fields{"type": kind}
		}
		items = append(items, fields{"name": t.name, "description": t.description,
			"inputSchema": fields{"type": "object", "properties": properties, "required": t.required}})
	}
	return fields{"tools": items}
}()

func rpcError(id any, code int, message string) fields {
	return fields{"jsonrpc": "2.0", "id": id, "error": fields{"code": code, "message": message}}
}

func (f *forge) runTool(name string, args fields) (result fields) {
	defer func() {
		if e := recover(); e != nil {
			if err, ok := e.(fault); ok {
				result = fields{"isError": true, "content": []fields{{"type": "text", "text": err.message}}}
			} else {
				panic(e)
			}
		}
	}()
	f.mu.Lock()
	defer f.mu.Unlock()
	var value any
	if name == "create_repo" {
		value = f.createRepo(args).info
	} else {
		repo := f.requireRepo(text(args["owner"]), text(args["repo"]))
		switch name {
		case "get_repo":
			value = repo.info
		case "create_issue":
			value = f.createIssue(repo, inputIssue(args))
		case "list_issues":
			state := text(args["state"])
			if state == "" {
				state = "open"
			}
			value = fields{"issues": listIssues(repo, state, false)}
		case "create_pull_request":
			value = *f.createPull(repo, args)
		case "merge_pull_request":
			p := repo.pulls[integer(args["pull_number"])]
			if p == nil {
				missing()
			}
			value = repo.merge(p, "squash")
		}
	}
	display, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return fields{"content": []fields{{"type": "text", "text": string(display)}}, "structuredContent": json.RawMessage(display)}
}

func (f *forge) mcp(w http.ResponseWriter, req *http.Request) {
	w.Header().Set("Mcp-Protocol-Version", mcpRevision)
	var body any
	if !decodeJSON(w, req, &body) {
		sendJSON(w, 400, rpcError(nil, -32700, "Parse error"))
		return
	}
	rpc, ok := body.(fields)
	method, methodOK := rpc["method"].(string)
	id, hasID := rpc["id"]
	idOK := true
	switch id.(type) {
	case nil, string, float64:
	default:
		idOK = false
	}
	if !ok || rpc["jsonrpc"] != "2.0" || !methodOK || !idOK {
		sendJSON(w, 200, rpcError(nil, -32600, "Invalid request"))
		return
	}
	if !hasID {
		sendBytes(w, 202, jsonMedia, nil)
		return
	}
	send := func(result any) { sendJSON(w, 200, fields{"jsonrpc": "2.0", "id": id, "result": result}) }
	reject := func(code int, message string) { sendJSON(w, 200, rpcError(id, code, message)) }
	switch method {
	case "server/discover":
		send(fields{"supportedVersions": []string{mcpRevision}, "capabilities": fields{"tools": fields{}},
			"_meta": fields{"io.modelcontextprotocol/serverInfo": fields{"name": "makefun-forge", "version": "1.0.0"}}})
	case "tools/list":
		send(toolList)
	case "tools/call":
		params, _ := rpc["params"].(fields)
		name := text(params["name"])
		var t *tool
		for i := range mcpTools {
			if mcpTools[i].name == name {
				t = &mcpTools[i]
				break
			}
		}
		if t == nil {
			reject(-32602, "Unknown tool")
			return
		}
		args, ok := params["arguments"].(fields)
		if !ok && params["arguments"] != nil {
			reject(-32602, "Tool arguments must be an object")
			return
		}
		for _, key := range t.required {
			if _, ok := args[key]; !ok {
				reject(-32602, "Missing required argument "+key)
				return
			}
		}
		for _, key := range sortedKeys(t.fields) {
			value, exists := args[key]
			if !exists {
				continue
			}
			valid := false
			switch t.fields[key] {
			case "string":
				_, valid = value.(string)
			case "boolean":
				_, valid = value.(bool)
			case "integer":
				n, ok := value.(float64)
				valid = ok && float64(int(n)) == n
			}
			if !valid {
				reject(-32602, "Invalid argument "+key)
				return
			}
		}
		send(f.runTool(name, args))
	default:
		reject(-32601, "Method not found")
	}
}
