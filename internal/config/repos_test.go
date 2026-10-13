package config

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestResolveWildcard(t *testing.T) {
	c := &Config{Repos: []Repo{
		{Name: "org/explicit", Instructions: "special"},
		{Name: "org/*", Instructions: "default"},
	}, ExcludeRepos: []string{"org/Skipped"}}
	got := c.Resolve([]string{"org/zeta", "Org/Explicit", "other/repo", "org/alpha", "org/skipped"})
	want := []Repo{
		{Name: "org/explicit", Instructions: "special"},
		{Name: "org/alpha", Instructions: "default"},
		{Name: "org/zeta", Instructions: "default"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
	if r := c.RepoFor("org/beta"); r.Instructions != "default" {
		t.Errorf("RepoFor wildcard: got %+v", r)
	}
	if r := c.RepoFor("org/explicit"); r.Instructions != "special" {
		t.Errorf("RepoFor explicit: got %+v", r)
	}
	if r := c.RepoFor("other/repo"); r.Instructions != "" {
		t.Errorf("RepoFor unmatched: got %+v", r)
	}
}

func TestDrawsAcceptCountOrList(t *testing.T) {
	var d Draws
	if err := json.Unmarshal([]byte(`3`), &d); err != nil || len(d) != 3 {
		t.Fatalf("count: %v %v", d, err)
	}
	if err := json.Unmarshal([]byte(`[{}, {"question": "exits"}, {"temperature": 0.7}]`), &d); err != nil || len(d) != 3 || d[1].Question != "exits" || d[2].Temperature == nil || *d[2].Temperature != 0.7 {
		t.Fatalf("list: %+v %v", d, err)
	}
}
