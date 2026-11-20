package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strconv"
	"unicode/utf8"
)

func readIssueInput(w http.ResponseWriter, req *http.Request) issueInput {
	body := requestBody(w, req, jsonBodyLimit)
	if len(body) <= 4096 {
		if input, ok := plainIssueInput(body); ok {
			return input
		}
	}
	var input *issueInput
	if json.Unmarshal(body, &input) != nil || input == nil {
		fail(400, "Expected a JSON object")
	}
	return *input
}

// Decode the two ordinary string fields without reflection. The full JSON
// decoder still handles escaping, labels, and all other representations.
func plainIssueInput(body []byte) (issueInput, bool) {
	var input issueInput
	body = bytes.Trim(body, " \t\r\n")
	if len(body) < 2 || body[0] != '{' || body[len(body)-1] != '}' {
		return input, false
	}
	body = bytes.TrimLeft(body[1:len(body)-1], " \t\r\n")
	seenTitle, seenBody := false, false
	for field := 0; field < 2; field++ {
		var target *string
		switch {
		case !seenTitle && bytes.HasPrefix(body, []byte("\"title\"")):
			seenTitle, target, body = true, &input.Title, body[7:]
		case !seenBody && bytes.HasPrefix(body, []byte("\"body\"")):
			seenBody, target, body = true, &input.Body, body[6:]
		default:
			return issueInput{}, false
		}
		body = bytes.TrimLeft(body, " \t\r\n")
		if len(body) < 3 || body[0] != ':' {
			return issueInput{}, false
		}
		body = bytes.TrimLeft(body[1:], " \t\r\n")
		if len(body) < 2 || body[0] != '"' {
			return issueInput{}, false
		}
		body = body[1:]
		end := bytes.IndexByte(body, '"')
		if end < 0 {
			return issueInput{}, false
		}
		value := body[:end]
		for _, c := range value {
			if c < ' ' || c == '\\' || c >= utf8.RuneSelf {
				return issueInput{}, false
			}
		}
		*target = string(value)
		body = bytes.TrimLeft(body[end+1:], " \t\r\n")
		if field == 0 {
			if len(body) == 0 || body[0] != ',' {
				return issueInput{}, false
			}
			body = bytes.TrimLeft(body[1:], " \t\r\n")
		}
	}
	return input, len(body) == 0
}

func (item *issue) jsonBytes() []byte {
	// Labeled issues use the general encoder. The common no-label response
	// has a fixed shape and the same string escaping as encoding/json.
	if len(item.Labels) != 0 {
		data, _ := json.Marshal(item)
		return data
	}
	out := make([]byte, 0, 90+len(item.Title)+len(item.Body))
	out = append(out, "{\"id\":"...)
	out = strconv.AppendInt(out, int64(item.ID), 10)
	out = append(out, ",\"number\":"...)
	out = strconv.AppendInt(out, int64(item.Number), 10)
	out = append(out, ",\"title\":"...)
	out = appendJSONString(out, item.Title)
	out = append(out, ",\"body\":"...)
	out = appendJSONString(out, item.Body)
	out = append(out, ",\"state\":"...)
	out = appendJSONString(out, item.State)
	return append(out, ",\"labels\":[]}"...)
}

func appendJSONString(out []byte, text string) []byte {
	const hex = "0123456789abcdef"
	out = append(out, '"')
	start := 0
	for i := 0; i < len(text); {
		c := text[i]
		if c >= utf8.RuneSelf {
			r, n := utf8.DecodeRuneInString(text[i:])
			var escaped string
			switch {
			case r == utf8.RuneError && n == 1:
				escaped = "\ufffd"
			case r == '\u2028':
				escaped = "\\u2028"
			case r == '\u2029':
				escaped = "\\u2029"
			}
			if escaped != "" {
				out = append(out, text[start:i]...)
				out = append(out, escaped...)
				start = i + n
			}
			i += n
			continue
		}
		if c >= ' ' && c != '"' && c != '\\' && c != '<' && c != '>' && c != '&' {
			i++
			continue
		}
		out = append(out, text[start:i]...)
		switch c {
		case '"', '\\':
			out = append(out, '\\', c)
		case '\b':
			out = append(out, '\\', 'b')
		case '\f':
			out = append(out, '\\', 'f')
		case '\n':
			out = append(out, '\\', 'n')
		case '\r':
			out = append(out, '\\', 'r')
		case '\t':
			out = append(out, '\\', 't')
		default:
			out = append(out, '\\', 'u', '0', '0', hex[c>>4], hex[c&15])
		}
		i++
		start = i
	}
	out = append(out, text[start:]...)
	return append(out, '"')
}
