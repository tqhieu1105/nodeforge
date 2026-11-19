package main

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"sort"
	"strconv"
	"strings"
)

const zeroOID = "0000000000000000000000000000000000000000"

const (
	commitKind byte = 1
	treeKind   byte = 2
	blobKind   byte = 3
	tagKind    byte = 4
)

var objectKinds = [...]string{"", "commit", "tree", "blob", "tag"}

type object struct {
	kind    byte
	data    []byte
	tree    string
	parents []string
	message string
}

type objectStore map[string]*object
type treeEntry struct{ name, mode, sha string }
type fileEntry struct{ mode, sha string }

func oid(value string) bool {
	if len(value) != 40 {
		return false
	}
	for i := range len(value) {
		c := value[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func newObject(kind byte, data []byte) *object {
	obj := &object{kind: kind, data: data}
	if kind != commitKind {
		return obj
	}
	header, message, ok := strings.Cut(string(data), "\n\n")
	if !ok {
		return obj
	}
	first, rest, _ := strings.Cut(header, "\n")
	tree, hasTree := strings.CutPrefix(first, "tree ")
	if !hasTree || !oid(tree) {
		return obj
	}
	obj.tree = tree
	obj.message = strings.TrimSuffix(message, "\n")
	for line := range strings.SplitSeq(rest, "\n") {
		if parent, ok := strings.CutPrefix(line, "parent "); ok && oid(parent) {
			obj.parents = append(obj.parents, parent)
		}
	}
	return obj
}

func objectID(kind byte, data []byte) string {
	var storage [64]byte
	header := append(storage[:0], objectKinds[kind]...)
	header = append(header, ' ')
	header = strconv.AppendInt(header, int64(len(data)), 10)
	header = append(header, 0)
	hash := sha1.New()
	hash.Write(header)
	hash.Write(data)
	var digest [sha1.Size]byte
	return hex.EncodeToString(hash.Sum(digest[:0]))
}

func (objects objectStore) save(kind byte, data []byte) string {
	id := objectID(kind, data)
	if objects[id] == nil {
		objects[id] = newObject(kind, data)
	}
	return id
}

func (o *object) entries() []treeEntry {
	if o == nil || o.kind != treeKind {
		return nil
	}
	var entries []treeEntry
	for data := o.data; len(data) > 0; {
		space := bytes.IndexByte(data, ' ')
		nul := bytes.IndexByte(data, 0)
		end := nul + 1 + sha1.Size
		if space < 1 || nul <= space+1 || len(data) < end {
			fail(400, "Invalid Git tree")
		}
		entries = append(entries, treeEntry{
			name: string(data[space+1 : nul]),
			mode: string(data[:space]),
			sha:  hex.EncodeToString(data[nul+1 : end]),
		})
		data = data[end:]
	}
	return entries
}

func (o *object) target() string {
	if o == nil || o.kind != tagKind || len(o.data) < 48 || !bytes.HasPrefix(o.data, []byte("object ")) {
		return ""
	}
	target := string(o.data[7:47])
	if oid(target) {
		return target
	}
	return ""
}

func (objects objectStore) isAncestor(ancestor, tip string) bool {
	if ancestor == tip {
		return true
	}
	start := objects[tip]
	if start == nil {
		return false
	}
	for _, parent := range start.parents {
		if parent == ancestor {
			return true
		}
	}
	pending := append([]string(nil), start.parents...)
	seen := map[string]bool{}
	for len(pending) > 0 {
		index := len(pending) - 1
		sha := pending[index]
		pending = pending[:index]
		if sha == ancestor {
			return true
		}
		if seen[sha] {
			continue
		}
		seen[sha] = true
		if obj := objects[sha]; obj != nil {
			pending = append(pending, obj.parents...)
		}
	}
	return false
}

func (objects objectStore) snapshot(tip string) map[string]fileEntry {
	files := map[string]fileEntry{}
	var visit func(string, string, int)
	visit = func(tree, prefix string, depth int) {
		if depth > 128 {
			fail(400, "Git tree is too deep")
		}
		for _, entry := range objects[tree].entries() {
			path := prefix + entry.name
			if entry.mode == "40000" {
				visit(entry.sha, path+"/", depth+1)
			} else {
				files[path] = fileEntry{mode: entry.mode, sha: entry.sha}
			}
		}
	}
	if obj := objects[tip]; obj != nil && obj.tree != "" {
		visit(obj.tree, "", 0)
	}
	return files
}

func (objects objectStore) writeTree(files map[string]fileEntry) string {
	type directory struct {
		files map[string]fileEntry
		dirs  map[string]*directory
	}
	makeDir := func() *directory {
		return &directory{files: map[string]fileEntry{}, dirs: map[string]*directory{}}
	}
	root := makeDir()
	for path, entry := range files {
		parent := root
		parts := strings.Split(path, "/")
		for _, name := range parts[:len(parts)-1] {
			if parent.dirs[name] == nil {
				parent.dirs[name] = makeDir()
			}
			parent = parent.dirs[name]
		}
		parent.files[parts[len(parts)-1]] = entry
	}
	var save func(*directory) string
	save = func(dir *directory) string {
		entries := make(map[string]fileEntry, len(dir.files)+len(dir.dirs))
		for name, entry := range dir.files {
			entries[name] = entry
		}
		for name, child := range dir.dirs {
			entries[name+"/"] = fileEntry{mode: "40000", sha: save(child)}
		}
		var data []byte
		for _, name := range sortedKeys(entries) {
			entry := entries[name]
			data = append(data, entry.mode...)
			data = append(data, ' ')
			data = append(data, strings.TrimSuffix(name, "/")...)
			data = append(data, 0)
			raw, _ := hex.DecodeString(entry.sha)
			data = append(data, raw...)
		}
		return objects.save(treeKind, data)
	}
	return save(root)
}

func (r *repository) commit(files map[string]fileEntry, parent, message string) string {
	tree := r.objects.writeTree(files)
	var content strings.Builder
	content.WriteString("tree " + tree + "\n")
	if parent != "" {
		content.WriteString("parent " + parent + "\n")
	}
	login := r.info.Owner.Login
	identity := login + " <" + login + "@example.invalid> 1700000000 +0000\n"
	content.WriteString("author " + identity + "committer " + identity + "\n" + message + "\n")
	return r.objects.save(commitKind, []byte(content.String()))
}

func (r *repository) commits(tip string) []fields {
	pending := []string{tip}
	seen := map[string]bool{}
	items := []fields{}
	for len(pending) > 0 {
		index := len(pending) - 1
		sha := pending[index]
		pending = pending[:index]
		obj := r.objects[sha]
		if obj == nil || obj.tree == "" || seen[sha] {
			continue
		}
		seen[sha] = true
		items = append(items, fields{"sha": sha, "commit": fields{"message": obj.message}})
		for i := len(obj.parents) - 1; i >= 0; i-- {
			pending = append(pending, obj.parents[i])
		}
	}
	return items
}

func conflictPaths(files map[string]fileEntry) bool {
	for path := range files {
		for i := range len(path) {
			if path[i] == '/' && files[path[:i]].sha != "" {
				return true
			}
		}
	}
	return false
}

func pullList(r *repository, state string) []*pull {
	items := make([]*pull, 0, len(r.pulls))
	for _, item := range r.pulls {
		if state == "all" || state == item.State {
			items = append(items, item)
		}
	}
	sort.Slice(items, func(i, j int) bool {
		return items[i].Number > items[j].Number
	})
	return items
}
