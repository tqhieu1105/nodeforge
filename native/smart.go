package main

import (
	"net/http"
	"strconv"
	"strings"
)

const uploadPack = "git-upload-pack"
const receivePack = "git-receive-pack"
const flush = "0000"
const delimiter = "0001"
const packetMax = 65520

func pkt(dst []byte, body string) []byte {
	const hex = "0123456789abcdef"
	size := len(body) + 4
	if size > packetMax {
		fail(400, "Git packet is too large")
	}
	return append(append(dst, hex[size>>12], hex[size>>8&15], hex[size>>4&15], hex[size&15]), body...)
}

func sideband(dst, payload []byte) []byte {
	const hex = "0123456789abcdef"
	for len(payload) > 0 {
		n := min(len(payload), packetMax-5)
		size := n + 5
		dst = append(dst, hex[size>>12], hex[size>>8&15], hex[size>>4&15], hex[size&15], 1)
		dst = append(dst, payload[:n]...)
		payload = payload[n:]
	}
	return dst
}

func readPackets(data []byte, stopAtFlush bool) ([]string, []byte) {
	lines := make([]string, 0, 12)
	for len(data) > 0 {
		if len(data) < 4 {
			fail(400, "Truncated pkt-line")
		}
		size, err := strconv.ParseUint(string(data[:4]), 16, 16)
		if err != nil {
			fail(400, "Malformed pkt-line")
		}
		switch size {
		case 0, 1, 2:
			data = data[4:]
			if size == 0 && stopAtFlush {
				return lines, data
			}
		default:
			if size < 4 || size > packetMax || int(size) > len(data) {
				fail(400, "Malformed pkt-line")
			}
			lines = append(lines, strings.TrimSuffix(string(data[4:size]), "\n"))
			data = data[size:]
		}
	}
	if stopAtFlush {
		fail(400, "Missing pkt-line flush")
	}
	return lines, nil
}

func packetList(lines ...string) []byte {
	var b []byte
	for _, line := range lines {
		b = pkt(b, line)
	}
	return append(b, flush...)
}

var v2Advertisement = packetList("version 2\n", "agent=forge/1.0\n", "ls-refs=unborn\n", "fetch=shallow\n", "object-format=sha1\n")
var serviceUploadPrefix = append(pkt(nil, "# service="+uploadPack+"\n"), flush...)
var serviceReceivePrefix = append(pkt(nil, "# service="+receivePack+"\n"), flush...)

func (r *repository) peel(sha string) string {
	start := sha
	for i := 0; i < 64; i++ {
		next := r.objects[sha].target()
		if next == "" {
			if sha != start {
				return sha
			}
			break
		}
		sha = next
	}
	return ""
}

func (r *repository) advertise(service string, v2 bool) []byte {
	if v2 {
		return v2Advertisement
	}
	capabilities := "report-status side-band-64k delete-refs ofs-delta"
	prefix := serviceReceivePrefix
	refs := sortedKeys(r.refs)
	var b []byte
	first := true
	write := func(ref, sha string) {
		line := sha + " " + ref
		if first {
			line += "\x00" + capabilities
			first = false
		}
		b = pkt(b, line+"\n")
	}
	if service == uploadPack {
		prefix = serviceUploadPrefix
		symref := headRef(r.info.DefaultBranch)
		capabilities = "side-band-64k shallow ofs-delta no-progress symref=HEAD:" + symref
		if head := r.refs[symref]; head != "" {
			write("HEAD", head)
		}
	}
	for _, ref := range refs {
		sha := r.refs[ref]
		write(ref, sha)
		if service == uploadPack && strings.HasPrefix(ref, "refs/tags/") {
			if target := r.peel(sha); target != "" {
				write(ref+"^{}", target)
			}
		}
	}
	if first {
		write("capabilities^{}", zeroOID)
	}
	result := make([]byte, 0, len(prefix)+len(b)+4)
	result = append(result, prefix...)
	result = append(result, b...)
	return append(result, flush...)
}

func validRef(ref string) bool {
	if len(ref) > 200 || !(strings.HasPrefix(ref, "refs/heads/") || strings.HasPrefix(ref, "refs/tags/")) ||
		strings.HasSuffix(ref, ".") || strings.Contains(ref, "..") || strings.Contains(ref, "@{") {
		return false
	}
	for _, c := range ref {
		if c <= 32 || c == 127 || strings.ContainsRune("~^:?*[\\", c) {
			return false
		}
	}
	for part := range strings.SplitSeq(ref, "/") {
		if part == "" || strings.HasPrefix(part, ".") || strings.HasSuffix(part, ".lock") {
			return false
		}
	}
	return true
}

type refUpdate struct{ old, next, ref string }

func (r *repository) receive(data []byte) []byte {
	lines, payload := readPackets(data, true)
	if len(lines) == 0 {
		fail(400, "Missing ref commands")
	}
	var features string
	lines[0], features, _ = strings.Cut(lines[0], "\x00")
	updates := make([]refUpdate, 0, len(lines))
	needsPack := false
	for _, line := range lines {
		old, rest, _ := strings.Cut(line, " ")
		next, ref, _ := strings.Cut(rest, " ")
		if !oid(old) || !oid(next) || !validRef(ref) {
			fail(400, "Invalid ref command")
		}
		updates = append(updates, refUpdate{old, next, ref})
		needsPack = needsPack || next != zeroOID
	}
	unpackError := ""
	if needsPack || len(payload) > 0 {
		incoming, ok := unpack(payload, r.objects)
		if !ok {
			unpackError = "invalid pack"
		} else {
			for id, o := range incoming {
				if r.objects[id] == nil {
					r.objects[id] = o
					r.uploadCache = nil
				}
			}
		}
	}
	var b []byte
	if unpackError == "" {
		b = pkt(b, "unpack ok\n")
	} else {
		b = pkt(b, "unpack "+unpackError+"\n")
	}
	for _, u := range updates {
		current := r.refs[u.ref]
		if current == "" {
			current = zeroOID
		}
		reason := unpackError
		if reason == "" && u.next != zeroOID {
			o := r.objects[u.next]
			if o == nil || strings.HasPrefix(u.ref, "refs/heads/") && o.kind != commitKind {
				reason = "missing object"
			} else if current != zeroOID && !r.objects.isAncestor(current, u.next) {
				reason = "non-fast-forward"
			}
		}
		if reason == "" && u.old != current {
			reason = "stale ref"
		}
		if reason != "" {
			b = pkt(b, "ng "+u.ref+" "+reason+"\n")
			continue
		}
		if u.next == zeroOID {
			r.deleteRef(u.ref)
		} else {
			r.setRef(u.ref, u.next)
		}
		b = pkt(b, "ok "+u.ref+"\n")
	}
	report, side := false, false
	for cap := range strings.FieldsSeq(features) {
		report = report || cap == "report-status" || cap == "report-status-v2"
		side = side || cap == "side-band-64k"
	}
	if report {
		b = append(b, flush...)
	} else {
		b = nil
	}
	if side {
		b = append(sideband(nil, b), flush...)
	}
	return b
}

func (r *repository) listRefs(lines []string) []byte {
	var prefixes []string
	symrefs, unborn, peel := false, false, false
	for _, line := range lines {
		switch line {
		case "symrefs":
			symrefs = true
		case "unborn":
			unborn = true
		case "peel":
			peel = true
		default:
			if prefix, ok := strings.CutPrefix(line, "ref-prefix "); ok {
				prefixes = append(prefixes, prefix)
			}
		}
	}
	matches := func(ref string) bool {
		if len(prefixes) == 0 {
			return true
		}
		for _, prefix := range prefixes {
			if strings.HasPrefix(ref, prefix) {
				return true
			}
		}
		return false
	}
	var b []byte
	if matches("HEAD") {
		sym := headRef(r.info.DefaultBranch)
		if sha := r.refs[sym]; sha != "" {
			line := sha + " HEAD"
			if symrefs {
				line += " symref-target:" + sym
			}
			b = pkt(b, line+"\n")
		} else if unborn {
			b = pkt(b, "unborn HEAD symref-target:"+sym+"\n")
		}
	}
	for _, ref := range sortedKeys(r.refs) {
		if !matches(ref) {
			continue
		}
		sha := r.refs[ref]
		line := sha + " " + ref
		if peel {
			if target := r.peel(sha); target != "" {
				line += " peeled:" + target
			}
		}
		b = pkt(b, line+"\n")
	}
	return append(b, flush...)
}

type visit struct {
	sha   string
	depth int
}

func (r *repository) walk(roots []string, depth int, stops map[string]bool) ([]visit, map[string]bool) {
	pending := make([]visit, 0, len(roots))
	for _, sha := range roots {
		pending = append(pending, visit{sha, 1})
	}
	items := make([]visit, 0, len(roots))
	seen := make(map[string]bool, len(roots))
	for index := 0; index < len(pending); index++ {
		item := pending[index]
		if seen[item.sha] {
			continue
		}
		o := r.objects[item.sha]
		if o == nil {
			continue
		}
		seen[item.sha] = true
		items = append(items, item)
		switch o.kind {
		case commitKind:
			if o.tree != "" {
				pending = append(pending, visit{o.tree, item.depth})
			}
			if (depth == 0 || item.depth < depth) && !stops[item.sha] {
				for _, sha := range o.parents {
					pending = append(pending, visit{sha, item.depth + 1})
				}
			}
		case treeKind:
			for _, e := range o.entries() {
				if e.mode != "160000" {
					pending = append(pending, visit{e.sha, item.depth})
				}
			}
		case tagKind:
			if target := o.target(); target != "" {
				pending = append(pending, visit{target, item.depth})
			}
		}
	}
	return items, seen
}

func (r *repository) fetch(lines []string, v2 bool) ([]byte, bool) {
	var wants, haves []string
	shallow := map[string]bool{}
	var depth int
	done, side := false, false
	for _, line := range lines {
		token, arg, _ := strings.Cut(line, " ")
		value, _, _ := strings.Cut(arg, " ")
		switch token {
		case "want":
			if !oid(value) || r.objects[value] == nil {
				fail(400, "Unknown wanted object")
			}
			wants = append(wants, value)
			if !v2 {
				for key := range strings.FieldsSeq(arg) {
					side = side || key == "side-band-64k"
				}
			}
		case "have":
			if r.objects[value] != nil {
				haves = append(haves, value)
			}
		case "shallow":
			shallow[value] = true
		case "deepen":
			depth, _ = strconv.Atoi(value)
			depth = max(depth, 0)
		case "done":
			done = true
		}
	}
	if v2 && !done {
		b := pkt(nil, "acknowledgments\n")
		if len(haves) == 0 {
			b = pkt(b, "NAK\n")
		}
		for _, have := range haves {
			b = pkt(b, "ACK "+have+"\n")
		}
		return append(b, flush...), false
	}
	_, held := r.walk(haves, 0, shallow)
	var stops map[string]bool
	if depth == 0 {
		stops = shallow
	}
	items, seen := r.walk(wants, depth, stops)
	var boundary []byte
	objects := make([]*object, 0, len(items))
	for _, item := range items {
		o := r.objects[item.sha]
		if !held[item.sha] {
			objects = append(objects, o)
		}
		if depth == 0 || o.kind != commitKind {
			continue
		}
		complete := true
		for _, parent := range o.parents {
			if !seen[parent] {
				complete = false
				break
			}
		}
		if item.depth == depth && !complete && !shallow[item.sha] {
			boundary = pkt(boundary, "shallow "+item.sha+"\n")
		}
		if complete && shallow[item.sha] {
			boundary = pkt(boundary, "unshallow "+item.sha+"\n")
		}
	}
	data := pack(objects)
	var b []byte
	if v2 {
		if len(boundary) > 0 {
			b = pkt(b, "shallow-info\n")
			b = append(b, boundary...)
			b = append(b, delimiter...)
		}
		b = pkt(b, "packfile\n")
		return append(sideband(b, data), flush...), depth == 1 && len(haves) == 0 && len(shallow) == 0
	}
	if depth > 0 {
		b = append(boundary, flush...)
	}
	if len(haves) > 0 {
		b = pkt(b, "ACK "+haves[len(haves)-1]+"\n")
	} else {
		b = pkt(b, "NAK\n")
	}
	if side {
		return append(sideband(b, data), flush...), false
	}
	return append(b, data...), false
}

func (r *repository) upload(body []byte, v2 bool) []byte {
	cacheable := v2 && len(body) <= 1024
	if cacheable {
		if cached := r.uploadCache[string(body)]; cached != nil {
			return cached
		}
	}
	lines, _ := readPackets(body, false)
	if len(lines) == 0 {
		fail(400, "Missing upload-pack command")
	}
	var reply []byte
	save := false
	switch {
	case v2 && lines[0] == "command=ls-refs":
		reply = r.listRefs(lines[1:])
		save = true
	case v2 && lines[0] == "command=fetch" || !v2 && strings.HasPrefix(lines[0], "want "):
		reply, save = r.fetch(lines, v2)
	default:
		fail(400, "Unknown upload-pack command")
	}
	if cacheable && save && len(reply) <= 128<<10 {
		if r.uploadCache == nil || len(r.uploadCache) >= 4 {
			r.uploadCache = map[string][]byte{}
		}
		r.uploadCache[string(body)] = reply
	}
	return reply
}

func (f *forge) smartHTTP(w http.ResponseWriter, req *http.Request) {
	owner, rest, ok := strings.Cut(strings.TrimPrefix(req.URL.Path, "/"), "/")
	name, endpoint, found := strings.Cut(rest, "/")
	if !ok || !found || !strings.HasSuffix(name, ".git") {
		missing()
	}
	name = strings.TrimSuffix(name, ".git")
	service := endpoint
	if endpoint == "info/refs" {
		service = req.URL.Query().Get("service")
	}
	if service != uploadPack && service != receivePack {
		missing()
	}
	v2 := false
	if service == uploadPack {
		for version := range strings.SplitSeq(req.Header.Get("Git-Protocol"), ":") {
			if version == "version=2" {
				v2 = true
				break
			}
		}
	}
	w.Header().Set("Cache-Control", "no-cache")
	if req.Method == "GET" && endpoint == "info/refs" {
		f.mu.RLock()
		defer f.mu.RUnlock()
		repo := f.requireRepo(owner, name)
		sendBytes(w, 200, "application/x-"+service+"-advertisement", repo.advertise(service, v2))
		return
	}
	if req.Method != "POST" || endpoint != service {
		missing()
	}
	data := requestBody(w, req, 64<<20)
	media := "application/x-" + service + "-result"
	// Most upload requests hit the small per-repository cache and only need a read lock.
	if v2 && len(data) <= 1024 {
		f.mu.RLock()
		repo := f.findRepo(owner, name)
		var reply []byte
		if repo != nil {
			reply = repo.uploadCache[string(data)]
		}
		f.mu.RUnlock()
		if reply != nil {
			sendBytes(w, 200, media, reply)
			return
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	repo := f.requireRepo(owner, name)
	var result []byte
	if service == receivePack {
		result = repo.receive(data)
	} else {
		result = repo.upload(data, v2)
	}
	if len(result) > 2048 {
		w.Header().Set("Content-Length", strconv.Itoa(len(result)))
	}
	sendBytes(w, 200, media, result)
}
