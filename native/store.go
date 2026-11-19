package main

import (
	"encoding/json"
	"regexp"
	"sort"
	"strings"
)

type user struct {
	Login string `json:"login"`
}
type repoInfo struct {
	ID            int    `json:"id"`
	Name          string `json:"name"`
	Private       bool   `json:"private"`
	Description   string `json:"description"`
	DefaultBranch string `json:"default_branch"`
	Owner         user   `json:"owner"`
	FullName      string `json:"full_name"`
}
type label struct {
	Name  string `json:"name"`
	Color string `json:"color"`
}
type issue struct {
	ID     int     `json:"id"`
	Number int     `json:"number"`
	Title  string  `json:"title"`
	Body   string  `json:"body"`
	State  string  `json:"state"`
	Labels []label `json:"labels"`
	wire   []byte
}
type issueInput struct {
	Title  string   `json:"title"`
	Body   string   `json:"body"`
	Labels []string `json:"labels"`
}
type comment struct {
	ID   int    `json:"id"`
	Body string `json:"body"`
}
type branch struct {
	Ref string `json:"ref"`
	SHA string `json:"sha"`
}
type pull struct {
	ID       int     `json:"id"`
	Number   int     `json:"number"`
	Title    string  `json:"title"`
	Body     string  `json:"body"`
	State    string  `json:"state"`
	Head     branch  `json:"head"`
	Base     branch  `json:"base"`
	Merged   bool    `json:"merged"`
	MergedAt *string `json:"merged_at"`
	MergeSHA *string `json:"merge_commit_sha"`
}
type repository struct {
	info        repoInfo
	refs        map[string]string
	objects     objectStore
	labels      map[string]label
	issues      map[int]*issue
	comments    map[int][]comment
	pulls       map[int]*pull
	number      int
	uploadCache map[string][]byte
}

var repoName = regexp.MustCompile("^[a-zA-Z0-9._-]{1,100}$")

func (f *forge) nextID() int          { f.id++; return f.id }
func (r *repository) nextNumber() int { r.number++; return r.number }

func (f *forge) createRepo(in fields) *repository {
	name := text(in["name"])
	if !repoName.MatchString(name) || name == "." || name == ".." {
		fail(422, "Invalid repository name")
	}
	key := strings.ToLower(name)
	if f.repos[key] != nil {
		fail(422, "Repository already exists")
	}
	private, _ := in["private"].(bool)
	info := repoInfo{f.nextID(), name, private, text(in["description"]), "main", user{f.login}, f.login + "/" + name}
	r := &repository{info: info, refs: map[string]string{}, objects: objectStore{}, labels: map[string]label{},
		issues: map[int]*issue{}, comments: map[int][]comment{}, pulls: map[int]*pull{}}
	f.repos[key] = r
	f.repoIDs[info.ID] = r
	return r
}

func (f *forge) findRepo(owner, name string) *repository {
	if !strings.EqualFold(owner, f.login) {
		return nil
	}
	return f.repos[strings.ToLower(name)]
}
func (f *forge) requireRepo(owner, name string) *repository {
	r := f.findRepo(owner, name)
	if r == nil {
		missing()
	}
	return r
}

func inputIssue(in fields) issueInput {
	i := issueInput{Title: text(in["title"]), Body: text(in["body"])}
	if raw, exists := in["labels"]; exists {
		labels, ok := raw.([]any)
		if !ok {
			fail(422, "Invalid labels")
		}
		for _, l := range labels {
			i.Labels = append(i.Labels, text(l))
		}
	}
	return i
}

func (f *forge) createIssue(r *repository, in issueInput) *issue {
	if strings.TrimSpace(in.Title) == "" {
		fail(422, "Issue title is required")
	}
	labels := make([]label, 0, len(in.Labels))
	for _, name := range in.Labels {
		l, ok := r.labels[name]
		if !ok {
			fail(422, "Unknown label")
		}
		labels = append(labels, l)
	}
	i := &issue{ID: f.nextID(), Number: r.nextNumber(), Title: in.Title, Body: in.Body, State: "open", Labels: labels}
	i.wire, _ = json.Marshal(i)
	r.issues[i.Number] = i
	return i
}

func updateIssue(r *repository, current *issue, in fields) *issue {
	next := *current
	if state := text(in["state"]); state == "open" || state == "closed" {
		next.State = state
	}
	if title, ok := in["title"].(string); ok {
		next.Title = title
	}
	if body, ok := in["body"].(string); ok {
		next.Body = body
	}
	next.wire, _ = json.Marshal(&next)
	r.issues[next.Number] = &next
	return &next
}

func (f *forge) addComment(r *repository, number int, input any) comment {
	body := text(input)
	if strings.TrimSpace(body) == "" {
		fail(422, "Comment body is required")
	}
	item := comment{f.nextID(), body}
	r.comments[number] = append(r.comments[number], item)
	return item
}

func listIssues(r *repository, state string, ascending bool) []*issue {
	items := make([]*issue, 0, len(r.issues))
	for _, i := range r.issues {
		if state == "all" || i.State == state {
			items = append(items, i)
		}
	}
	sort.Slice(items, func(a, b int) bool {
		if ascending {
			return items[a].Number < items[b].Number
		}
		return items[a].Number > items[b].Number
	})
	return items
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func headRef(name string) string { return "refs/heads/" + name }
func (r *repository) setRef(ref, sha string) {
	if r.refs[ref] != sha {
		r.refs[ref] = sha
		r.uploadCache = nil
	}
}
func (r *repository) deleteRef(ref string) {
	if _, ok := r.refs[ref]; ok {
		delete(r.refs, ref)
		r.uploadCache = nil
	}
}
func (r *repository) resolve(ref string) string {
	if ref == "" {
		ref = r.info.DefaultBranch
	}
	for _, key := range []string{ref, headRef(ref), "refs/tags/" + ref} {
		if sha := r.refs[key]; sha != "" {
			return sha
		}
	}
	if r.objects[ref] != nil {
		return ref
	}
	return ""
}
