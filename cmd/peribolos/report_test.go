/*
Copyright 2026 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"k8s.io/apimachinery/pkg/util/sets"

	"sigs.k8s.io/prow/pkg/config/org"
	"sigs.k8s.io/prow/pkg/github"
)

// TestChangeReportNilSafe verifies that a nil *changeReport is a usable no-op
// recorder, so callers that do not care about reporting can pass nil.
func TestChangeReportNilSafe(t *testing.T) {
	var r *changeReport
	// None of these must panic.
	r.record(change{Kind: kindOrgMember, Org: "o", Target: "u", Action: actionAdd})
	r.recordMemberAdd(kindOrgMember, "o", "", "u", memberships{}, true)
	r.recordMemberRemove(kindOrgMember, "o", "", "u", memberships{})
}

func TestChangeReportOutput(t *testing.T) {
	r := newChangeReport(true)
	// Record out of order and across kinds; output must be deterministically sorted.
	r.record(change{Kind: kindTeam, Org: "o", Target: "zebra", Action: actionRemove})
	r.record(change{Kind: kindOrgMember, Org: "o", Target: "bob", Action: actionRemove, Before: "admin"})
	r.record(change{Kind: kindOrgMember, Org: "o", Target: "alice", Action: actionAdd, After: "member"})
	r.record(change{Kind: kindTeam, Org: "o", Target: "apple", Action: actionAdd})

	out := r.output()

	if !out.DryRun {
		t.Errorf("DryRun = false, want true")
	}
	wantSummary := map[changeAction]int{actionAdd: 2, actionUpdate: 0, actionRemove: 2}
	if diff := cmp.Diff(wantSummary, out.Summary); diff != "" {
		t.Errorf("summary mismatch (-want +got):\n%s", diff)
	}
	want := []change{
		{Kind: kindOrgMember, Org: "o", Target: "alice", Action: actionAdd, After: "member"},
		{Kind: kindOrgMember, Org: "o", Target: "bob", Action: actionRemove, Before: "admin"},
		{Kind: kindTeam, Org: "o", Target: "apple", Action: actionAdd},
		{Kind: kindTeam, Org: "o", Target: "zebra", Action: actionRemove},
	}
	if diff := cmp.Diff(want, out.Changes); diff != "" {
		t.Errorf("changes not sorted as expected (-want +got):\n%s", diff)
	}
}

func TestRecordMemberAdd(t *testing.T) {
	// have: carol is a plain member, dave is a super (admin/maintainer).
	have := memberships{members: sets.New("carol"), super: sets.New("dave")}

	cases := []struct {
		name  string
		kind  string
		user  string
		super bool
		want  change
	}{
		{
			name: "new org member",
			kind: kindOrgMember, user: "erin", super: false,
			want: change{Kind: kindOrgMember, Org: "o", Target: "erin", Action: actionAdd, After: "member"},
		},
		{
			name: "new org admin",
			kind: kindOrgMember, user: "erin", super: true,
			want: change{Kind: kindOrgMember, Org: "o", Target: "erin", Action: actionAdd, After: "admin"},
		},
		{
			name: "promote member to admin",
			kind: kindOrgMember, user: "carol", super: true,
			want: change{Kind: kindOrgMember, Org: "o", Target: "carol", Action: actionUpdate, Before: "member", After: "admin"},
		},
		{
			name: "demote admin to member",
			kind: kindOrgMember, user: "dave", super: false,
			want: change{Kind: kindOrgMember, Org: "o", Target: "dave", Action: actionUpdate, Before: "admin", After: "member"},
		},
		{
			name: "new team maintainer uses team role labels",
			kind: kindTeamMember, user: "erin", super: true,
			want: change{Kind: kindTeamMember, Org: "o", Scope: "t", Target: "erin", Action: actionAdd, After: "maintainer"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newChangeReport(true)
			scope := ""
			if tc.kind == kindTeamMember {
				scope = "t"
			}
			r.recordMemberAdd(tc.kind, "o", scope, tc.user, have, tc.super)
			if diff := cmp.Diff([]change{tc.want}, r.output().Changes); diff != "" {
				t.Errorf("recordMemberAdd mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestRecordMemberRemove(t *testing.T) {
	have := memberships{members: sets.New("carol"), super: sets.New("dave")}
	cases := []struct {
		name string
		user string
		want change
	}{
		{name: "remove admin captures prior role", user: "dave", want: change{Kind: kindOrgMember, Org: "o", Target: "dave", Action: actionRemove, Before: "admin"}},
		{name: "remove member captures prior role", user: "carol", want: change{Kind: kindOrgMember, Org: "o", Target: "carol", Action: actionRemove, Before: "member"}},
		{name: "remove unknown has no prior role", user: "ghost", want: change{Kind: kindOrgMember, Org: "o", Target: "ghost", Action: actionRemove}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newChangeReport(true)
			r.recordMemberRemove(kindOrgMember, "o", "", tc.user, have)
			if diff := cmp.Diff([]change{tc.want}, r.output().Changes); diff != "" {
				t.Errorf("recordMemberRemove mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestFormatDelta(t *testing.T) {
	cases := []struct {
		name string
		c    change
		want string
	}{
		{name: "add value", c: change{Action: actionAdd, After: github.Write}, want: "write"},
		{name: "remove value", c: change{Action: actionRemove, Before: github.Read}, want: "read"},
		{name: "update before and after", c: change{Action: actionUpdate, Before: "member", After: "admin"}, want: "member -> admin"},
		{name: "update field-name list", c: change{Action: actionUpdate, After: []string{"name", "description"}}, want: "name, description"},
		{name: "add with no value", c: change{Action: actionAdd}, want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := formatDelta(tc.c); got != tc.want {
				t.Errorf("formatDelta() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestWriteHumanEmpty(t *testing.T) {
	cases := []struct {
		name   string
		dryRun bool
		want   string
	}{
		{name: "dry run", dryRun: true, want: "Planned changes: none.\n"},
		{name: "confirm", dryRun: false, want: "Applied changes: none.\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := newChangeReport(tc.dryRun).writeHuman(&buf); err != nil {
				t.Fatalf("writeHuman: %v", err)
			}
			if got := buf.String(); got != tc.want {
				t.Errorf("writeHuman() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestWriteHumanWithChanges(t *testing.T) {
	r := newChangeReport(true)
	r.record(change{Kind: kindOrgMember, Org: "o", Target: "alice", Action: actionAdd, After: "admin"})
	r.record(change{Kind: kindCollaborator, Org: "o", Scope: "repo1", Target: "bob", Action: actionUpdate, Before: github.Read, After: github.Write})

	var buf bytes.Buffer
	if err := r.writeHuman(&buf); err != nil {
		t.Fatalf("writeHuman: %v", err)
	}
	got := buf.String()

	for _, want := range []string{
		"Planned changes: 1 to add, 1 to update, 0 to remove.",
		"ACTION",
		"alice",
		"read -> write",
		"Dry run: re-run with --confirm to apply.",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("writeHuman output missing %q; full output:\n%s", want, got)
		}
	}
}

func TestWriteJSON(t *testing.T) {
	r := newChangeReport(false)
	r.record(change{Kind: kindRepo, Org: "o", Target: "repo1", Action: actionUpdate, After: []string{"has_issues"}})

	var buf bytes.Buffer
	if err := r.writeJSON(&buf); err != nil {
		t.Fatalf("writeJSON: %v", err)
	}

	var got struct {
		DryRun  bool           `json:"dry_run"`
		Summary map[string]int `json:"summary"`
		Changes []struct {
			Kind   string   `json:"kind"`
			Org    string   `json:"org"`
			Target string   `json:"target"`
			Action string   `json:"action"`
			After  []string `json:"after"`
		} `json:"changes"`
	}
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal report JSON: %v\n%s", err, buf.String())
	}
	if got.DryRun {
		t.Errorf("dry_run = true, want false")
	}
	if got.Summary["update"] != 1 || got.Summary["add"] != 0 || got.Summary["remove"] != 0 {
		t.Errorf("unexpected summary: %v", got.Summary)
	}
	if len(got.Changes) != 1 {
		t.Fatalf("got %d changes, want 1", len(got.Changes))
	}
	c := got.Changes[0]
	if c.Kind != kindRepo || c.Target != "repo1" || c.Action != string(actionUpdate) {
		t.Errorf("unexpected change: %+v", c)
	}
	if diff := cmp.Diff([]string{"has_issues"}, c.After); diff != "" {
		t.Errorf("after mismatch (-want +got):\n%s", diff)
	}
}

// TestConfigureTeamReposRecordsChanges is an end-to-end check that configureTeamRepos
// records add/update/remove changes with the correct before/after permissions.
func TestConfigureTeamReposRecordsChanges(t *testing.T) {
	client := &fakeTeamRepoClient{repos: map[string][]github.Repo{"team": {
		{Name: "keep", Permissions: github.RepoPermissions{Pull: true}},                              // read, unchanged
		{Name: "promote", Permissions: github.RepoPermissions{Pull: true, Triage: true, Push: true}}, // write -> admin
		{Name: "obsolete", Permissions: github.RepoPermissions{Pull: true}},                          // read, removed
	}}}
	team := org.Team{Repos: map[string]github.RepoPermissionLevel{
		"keep":    github.Read,
		"promote": github.Admin,
		"new":     github.Write,
	}}

	report := newChangeReport(true)
	if err := configureTeamRepos(client, map[string]github.Team{"team": {ID: 1, Slug: "team"}}, "team", "org", team, report); err != nil {
		t.Fatalf("configureTeamRepos: %v", err)
	}

	want := []change{
		{Kind: kindTeamRepo, Org: "org", Scope: "team", Target: "new", Action: actionAdd, After: github.Write},
		{Kind: kindTeamRepo, Org: "org", Scope: "team", Target: "obsolete", Action: actionRemove, Before: github.Read},
		{Kind: kindTeamRepo, Org: "org", Scope: "team", Target: "promote", Action: actionUpdate, Before: github.Write, After: github.Admin},
	}
	if diff := cmp.Diff(want, report.output().Changes); diff != "" {
		t.Errorf("recorded team_repo changes mismatch (-want +got):\n%s", diff)
	}
}

// TestConfigureCollaboratorsRecordsChanges is an end-to-end check that
// configureCollaborators records add/update/remove changes for direct collaborators.
func TestConfigureCollaboratorsRecordsChanges(t *testing.T) {
	client := &fakeCollaboratorClient{
		collaborators: map[string]github.RepoPermissionLevel{
			"keep":     github.Read,  // unchanged
			"promote":  github.Read,  // read -> write
			"obsolete": github.Write, // removed
		},
		members: sets.New[string](),
	}
	repo := org.Repo{Collaborators: map[string]github.RepoPermissionLevel{
		"keep":    github.Read,
		"promote": github.Write,
		"new":     github.Admin,
	}}

	report := newChangeReport(true)
	if err := configureCollaborators(client, "test-org", "test-repo", repo, report); err != nil {
		t.Fatalf("configureCollaborators: %v", err)
	}

	want := []change{
		{Kind: kindCollaborator, Org: "test-org", Scope: "test-repo", Target: "new", Action: actionAdd, After: github.Admin},
		{Kind: kindCollaborator, Org: "test-org", Scope: "test-repo", Target: "obsolete", Action: actionRemove, Before: github.Write},
		{Kind: kindCollaborator, Org: "test-org", Scope: "test-repo", Target: "promote", Action: actionUpdate, Before: github.Read, After: github.Write},
	}
	if diff := cmp.Diff(want, report.output().Changes); diff != "" {
		t.Errorf("recorded collaborator changes mismatch (-want +got):\n%s", diff)
	}
}
