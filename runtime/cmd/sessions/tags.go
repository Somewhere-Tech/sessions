package main

import (
	"fmt"
	"sort"
	"strings"

	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

type tagsResponse struct {
	Tags map[string]string `json:"tags"`
}

func (a *app) cmdTags(args []string) error {
	if len(args) == 0 || strings.TrimSpace(args[0]) == "" {
		return fail(1, "usage: sessions tags <session> [key=value ...] [--remove key ...] [--clear]")
	}
	id, err := a.resolveSessionID(args[0])
	if err != nil {
		return err
	}
	args = args[1:]
	var current tagsResponse
	path := "/api/sessions/" + escapeID(id) + "/tags"
	if err := a.getJSON(path, &current); err != nil {
		return err
	}
	if current.Tags == nil {
		current.Tags = make(map[string]string)
	}
	mutated := false
	for index := 0; index < len(args); index++ {
		switch args[index] {
		case "--clear":
			current.Tags = make(map[string]string)
			mutated = true
		case "--remove":
			if index+1 >= len(args) || strings.HasPrefix(args[index+1], "--") {
				return fail(1, "--remove needs a tag key")
			}
			delete(current.Tags, strings.ToLower(strings.TrimSpace(args[index+1])))
			mutated = true
			index++
		default:
			key, value, found := strings.Cut(args[index], "=")
			if !found {
				return fail(1, "invalid tag %q; use key=value, --remove key, or --clear", args[index])
			}
			current.Tags[strings.ToLower(strings.TrimSpace(key))] = value
			mutated = true
		}
	}
	normalized, err := state.NormalizeTags(current.Tags)
	if err != nil {
		return fail(1, "%s", err)
	}
	current.Tags = normalized
	if mutated {
		if err := a.putJSON(path, current, &current, 2); err != nil {
			return err
		}
	}
	if a.wantJSON {
		return writeJSON(a.stdout, current, true)
	}
	if len(current.Tags) == 0 {
		_, err := fmt.Fprintln(a.stdout, "(no tags)")
		return err
	}
	keys := make([]string, 0, len(current.Tags))
	for key := range current.Tags {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		fmt.Fprintf(a.stdout, "%s=%s\n", key, current.Tags[key])
	}
	return nil
}
