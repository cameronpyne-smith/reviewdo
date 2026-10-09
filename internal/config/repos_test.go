package config

import (
	"reflect"
	"testing"
)

func TestResolveWildcard(t *testing.T) {
	c := &Config{Repos: []Repo{
		{Name: "org/explicit", Instructions: "special"},
		{Name: "org/*", Instructions: "default"},
	}}
	got := c.Resolve([]string{"org/zeta", "Org/Explicit", "other/repo", "org/alpha"})
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
