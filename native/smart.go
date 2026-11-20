package main

import (
	"net/http"
	"strconv"
	"strings"
)

const (
	uploadPack  = "git-upload-pack"
	receivePack = "git-receive-pack"
	flush       = "0000"
	delimiter   = "0001"
	packetMax   = 65520
)

func packetHeader(dst []byte, size int) []byte {
	const hex = "0123456789abcdef"
	return append(dst, hex[size>>12], hex[size>>8&15], hex[size>>4&15], hex[size&15])
}

func pkt(dst []byte, body string) []byte {
	size := len(body) + 4
	if size > packetMax {
		fail(400, "Git packet is too large")
	}
	return append(packetHeader(dst, size), body...)
}

func sideband(dst, payload []byte) []byte {
	for len(payload) > 0 {
		n := min(len(payload), packetMax-5)
		dst = append(packetHeader(dst, n+5), 1)
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

var v2Advertisement = packetList(
	"version 2\n",
	"ls-refs=unborn\n",
	"fetch=shallow\n",
)
var serviceUploadPrefix = packetList("# service=" + uploadPack + "\n")
var serviceReceivePrefix = packetList("# service=" + receivePack + "\n")

const receiveCapabilities = "report-status side-band-64k delete-refs ofs-delta"

var emptyReceiveAdvertisement = receiveAdvertisement(zeroOID, "capabilities^{}")

func receiveAdvertisement(sha, ref string) []byte {
	reply := make([]byte, 0, len(serviceReceivePrefix)+len(sha)+len(ref)+len(receiveCapabilities)+11)
	reply = append(reply, serviceReceivePrefix...)
	reply = pkt(reply, sha+" "+ref+"\x00"+receiveCapabilities+"\n")
	return append(reply, flush...)
}

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
	if service == receivePack {
		switch len(r.refs) {
		case 0:
			return emptyReceiveAdvertisement
		case 1:
			for ref, sha := range r.refs {
				return receiveAdvertisement(sha, ref)
			}
		}
	}
	capabilities := receiveCapabilities
	prefix := serviceReceivePrefix
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
	for _, ref := range sortedKeys(r.refs) {
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

func (r *repository) updateRef(update refUpdate) string {
	current := r.refs[update.ref]
	if current == "" {
		current = zeroOID
	}
	if update.next != zeroOID {
		obj := r.objects[update.next]
		if obj == nil || (strings.HasPrefix(update.ref, "refs/heads/") && obj.kind != commitKind) {
			return "missing object"
		}
		if current != zeroOID && !r.objects.isAncestor(current, update.next) {
			return "non-fast-forward"
		}
	}
	if update.old != current {
		return "stale ref"
	}
	if update.next == zeroOID {
		r.deleteRef(update.ref)
	} else {
		r.setRef(update.ref, update.next)
	}
	return ""
}

func (r *repository) receive(data []byte) []byte {
	lines, payload := readPackets(data, true)
	if len(lines) == 0 {
		fail(400, "Missing ref commands")
	}
	var features string
	lines[0], features, _ = strings.Cut(lines[0], "\x00")
	var first [1]refUpdate
	updates := first[:0]
	needsPack := false
	for _, line := range lines {
		old, rest, _ := strings.Cut(line, " ")
		next, ref, _ := strings.Cut(rest, " ")
		if !oid(old) || !oid(next) || !validRef(ref) {
			fail(400, "Invalid ref command")
		}
		updates = append(updates, refUpdate{old: old, next: next, ref: ref})
		needsPack = needsPack || next != zeroOID
	}
	unpackError := ""
	if needsPack || len(payload) > 0 {
		var storage [8]packedObject
		incoming, ok := unpack(payload, r.objects, storage[:0])
		if !ok {
			unpackError = "invalid pack"
		} else {
			for _, item := range incoming {
				if r.objects[item.id] == nil {
					r.objects[item.id] = item.object
					r.uploadCache = nil
				}
			}
		}
	}
	var scratch [128]byte
	b := scratch[:0]
	if unpackError == "" {
		b = pkt(b, "unpack ok\n")
	} else {
		b = pkt(b, "unpack "+unpackError+"\n")
	}
	for _, update := range updates {
		reason := unpackError
		if reason == "" {
			reason = r.updateRef(update)
		}
		if reason != "" {
			b = pkt(b, "ng "+update.ref+" "+reason+"\n")
			continue
		}
		b = pkt(b, "ok "+update.ref+"\n")
	}
	report, side := false, false
	for capability := range strings.FieldsSeq(features) {
		report = report || capability == "report-status" || capability == "report-status-v2"
		side = side || capability == "side-band-64k"
	}
	if report {
		b = append(b, flush...)
	} else {
		b = nil
	}
	if side {
		framed := make([]byte, 0, len(b)+9)
		return append(sideband(framed, b), flush...)
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
		pending = append(pending, visit{sha: sha, depth: 1})
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
				pending = append(pending, visit{sha: o.tree, depth: item.depth})
			}
			if (depth == 0 || item.depth < depth) && !stops[item.sha] {
				for _, sha := range o.parents {
					pending = append(pending, visit{sha: sha, depth: item.depth + 1})
				}
			}
		case treeKind:
			for _, entry := range o.entries() {
				if entry.mode != "160000" {
					pending = append(pending, visit{sha: entry.sha, depth: item.depth})
				}
			}
		case tagKind:
			if target := o.target(); target != "" {
				pending = append(pending, visit{sha: target, depth: item.depth})
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
	case (v2 && lines[0] == "command=fetch") || (!v2 && strings.HasPrefix(lines[0], "want ")):
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

type gitTarget struct {
	owner, name, service, media string
	v2, advertise               bool
}

func parseGitTarget(req *http.Request) gitTarget {
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
	target := gitTarget{owner: owner, name: name, service: service, v2: v2}
	if req.Method == "GET" && endpoint == "info/refs" {
		target.advertise = true
		target.media = "application/x-" + service + "-advertisement"
	} else if req.Method == "POST" && endpoint == service {
		target.media = "application/x-" + service + "-result"
	} else {
		missing()
	}
	return target
}

func (f *forge) smartHTTP(w http.ResponseWriter, req *http.Request) {
	var target gitTarget
	response, wired := w.(*wireResponse)
	if wired {
		response.noCache = true
		if response.request != nil {
			target = response.request.git
		}
	} else {
		w.Header().Set("Cache-Control", "no-cache")
	}
	if target.service == "" {
		target = parseGitTarget(req)
		if wired && response.request != nil {
			response.request.git = target
		}
	}
	if target.advertise {
		f.mu.RLock()
		defer f.mu.RUnlock()
		repo := f.requireRepo(target.owner, target.name)
		sendBytes(w, 200, target.media, repo.advertise(target.service, target.v2))
		return
	}
	data := requestBody(w, req, 64<<20)
	// Cached replies are immutable and remain safe to send after unlocking.
	if target.v2 && len(data) <= 1024 {
		f.mu.RLock()
		repo := f.findRepo(target.owner, target.name)
		var reply []byte
		if repo != nil {
			reply = repo.uploadCache[string(data)]
		}
		f.mu.RUnlock()
		if reply != nil {
			sendBytes(w, 200, target.media, reply)
			return
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	repo := f.requireRepo(target.owner, target.name)
	var result []byte
	if target.service == receivePack {
		result = repo.receive(data)
	} else {
		result = repo.upload(data, target.v2)
	}
	if !wired && len(result) > 2048 {
		w.Header().Set("Content-Length", strconv.Itoa(len(result)))
	}
	sendBytes(w, 200, target.media, result)
}
