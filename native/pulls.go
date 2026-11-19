package main

import (
	"bytes"
	"math"
	"strconv"
	"strings"
)

func (f *forge) createPull(r *repository, input fields) *pull {
	title, head, base := text(input["title"]), text(input["head"]), text(input["base"])
	if strings.TrimSpace(title) == "" || head == "" || base == "" || head == base {
		fail(422, "Invalid pull request")
	}
	headSHA, baseSHA := r.refs[headRef(head)], r.refs[headRef(base)]
	if headSHA == "" || baseSHA == "" {
		fail(422, "Unknown head or base branch")
	}
	item := &pull{
		ID:     f.nextID(),
		Number: r.nextNumber(),
		Title:  title,
		Body:   text(input["body"]),
		State:  "open",
		Head:   branch{Ref: head, SHA: headSHA},
		Base:   branch{Ref: base, SHA: baseSHA},
	}
	r.pulls[item.Number] = item
	return item
}

func distances(objects objectStore, tip string) (map[string]int, []string) {
	queue := []string{tip}
	distance := map[string]int{tip: 0}
	for i := 0; i < len(queue); i++ {
		sha := queue[i]
		item := objects[sha]
		if item == nil {
			continue
		}
		for _, parent := range item.parents {
			if _, seen := distance[parent]; !seen {
				distance[parent] = distance[sha] + 1
				queue = append(queue, parent)
			}
		}
	}
	return distance, queue
}

func mergeBase(objects objectStore, base, head string) string {
	fromBase, _ := distances(objects, base)
	fromHead, order := distances(objects, head)
	best, score := "", math.MaxInt
	for _, sha := range order {
		if distance, found := fromBase[sha]; found && distance+fromHead[sha] < score {
			best, score = sha, distance+fromHead[sha]
		}
	}
	return best
}

type pathChange struct {
	path          string
	before, after fileEntry
}

func (r *repository) pullChanges(base, head string) (string, []pathChange) {
	ancestor := mergeBase(r.objects, base, head)
	before, after := r.objects.snapshot(ancestor), r.objects.snapshot(head)
	paths := make(map[string]struct{}, len(before))
	for path := range before {
		paths[path] = struct{}{}
	}
	for path := range after {
		paths[path] = struct{}{}
	}
	var changes []pathChange
	for _, path := range sortedKeys(paths) {
		if prev, next := before[path], after[path]; prev != next {
			changes = append(changes, pathChange{path: path, before: prev, after: next})
		}
	}
	return ancestor, changes
}

func objectLines(o *object) []string {
	if o == nil || len(o.data) == 0 || bytes.IndexByte(o.data, 0) >= 0 {
		return nil
	}
	lines := strings.SplitAfter(string(o.data), "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

func diffLines(before, after []string) (int, int) {
	for len(before) > 0 && len(after) > 0 && before[0] == after[0] {
		before, after = before[1:], after[1:]
	}
	for len(before) > 0 && len(after) > 0 && before[len(before)-1] == after[len(after)-1] {
		before, after = before[:len(before)-1], after[:len(after)-1]
	}
	common := 0
	if len(before) > 0 && len(after) > 0 && len(before) <= 1_000_000/len(after) {
		lengths := make([]int, len(after)+1)
		for _, oldLine := range before {
			diagonal := 0
			for j, newLine := range after {
				above := lengths[j+1]
				if oldLine == newLine {
					lengths[j+1] = diagonal + 1
				} else {
					lengths[j+1] = max(above, lengths[j])
				}
				diagonal = above
			}
		}
		common = lengths[len(after)]
	}
	return len(after) - common, len(before) - common
}

func (r *repository) pullFiles(p *pull) []fields {
	base, head := p.Base.SHA, p.Head.SHA
	if !p.Merged {
		if sha := r.refs[headRef(p.Base.Ref)]; sha != "" {
			base = sha
		}
		if sha := r.refs[headRef(p.Head.Ref)]; sha != "" {
			head = sha
		}
	}
	_, changes := r.pullChanges(base, head)
	files := make([]fields, 0, len(changes))
	for _, change := range changes {
		status, sha := "modified", change.after.sha
		switch {
		case change.before.sha == "":
			status = "added"
		case change.after.sha == "":
			status, sha = "removed", change.before.sha
		}
		before := objectLines(r.objects[change.before.sha])
		after := objectLines(r.objects[change.after.sha])
		additions, deletions := diffLines(before, after)
		files = append(files, fields{
			"filename":  change.path,
			"status":    status,
			"additions": additions,
			"deletions": deletions,
			"changes":   additions + deletions,
			"sha":       sha,
		})
	}
	return files
}

func (r *repository) mergePlan(p *pull) (map[string]fileEntry, string, string) {
	base, head := r.refs[headRef(p.Base.Ref)], r.refs[headRef(p.Head.Ref)]
	if base == "" || head == "" {
		return nil, "", ""
	}
	ancestor, changes := r.pullChanges(base, head)
	if ancestor == "" {
		return nil, "", ""
	}
	files := r.objects.snapshot(base)
	for _, change := range changes {
		current := files[change.path]
		if current != change.before && current != change.after {
			return nil, "", ""
		}
		if change.after.sha == "" {
			delete(files, change.path)
		} else {
			files[change.path] = change.after
		}
	}
	if conflictPaths(files) {
		return nil, "", ""
	}
	return files, base, head
}

func (r *repository) merge(p *pull, method string) fields {
	if (method != "" && method != "squash") || p.State != "open" {
		fail(405, "Pull request cannot be merged")
	}
	files, base, head := r.mergePlan(p)
	if files == nil {
		fail(405, "Pull request has merge conflicts")
	}
	message := p.Title + " (#" + strconv.Itoa(p.Number) + ")"
	sha := r.commit(files, base, message)
	r.setRef(headRef(p.Base.Ref), sha)
	stamp := "2023-11-14T22:13:20Z"
	p.State, p.Merged, p.MergedAt, p.MergeSHA = "closed", true, &stamp, &sha
	p.Base.SHA, p.Head.SHA = base, head
	return fields{"sha": sha, "merged": true, "message": "Pull request successfully merged"}
}
