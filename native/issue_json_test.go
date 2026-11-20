package main

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestIssueJSONMatchesStandardEncoding(t *testing.T) {
	for _, title := range []string{
		"plain ASCII",
		"雪 and é",
		"line\nreturn\rtab\tbell\a backspace\b form feed\f",
		"quote \" slash \\ markup <>&",
		"separators \u2028\u2029",
		string([]byte{0xff, 0x7f, 0x00}) + " malformed UTF-8",
	} {
		item := issue{ID: 10, Number: 12, Title: title, Body: title, State: "open", Labels: []label{}}
		want, err := json.Marshal(&item)
		if err != nil {
			t.Fatal(err)
		}
		if got := item.jsonBytes(); !bytes.Equal(got, want) {
			t.Errorf("standard encoding %s, fast encoding %s", want, got)
		}
	}
}

func TestPlainIssueInput(t *testing.T) {
	for _, body := range []string{
		"{\"title\":\"one\",\"body\":\"two\"}",
		"{\n \"body\" : \"two\" , \"title\" : \"one\" } ",
	} {
		input, ok := plainIssueInput([]byte(body))
		if !ok || input.Title != "one" || input.Body != "two" {
			t.Fatalf("parsed %q as %+v, accepted %t", body, input, ok)
		}
	}
	for _, body := range []string{
		"{\"title\":\"one\",\"body\":\"escaped \\\" inside\"}",
		"{\"title\":\"one\",\"body\":\"two\",\"labels\":[]}",
		"{\"title\":\"one\",\"body\":\"two\",}",
		"{\"title\":\"one\",\"title\":\"two\"}",
		"{\"title\":\"one\",\"body\":\"two\" \"extra\"}",
	} {
		if _, ok := plainIssueInput([]byte(body)); ok {
			t.Errorf("expected standard JSON decoder for %q", body)
		}
	}
}
