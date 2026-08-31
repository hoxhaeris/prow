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
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"testing"

	"sigs.k8s.io/prow/pkg/config/org"
	"sigs.k8s.io/prow/pkg/github"
)

func intPtr(i int) *int { return &i }

func mustRawJSON(t *testing.T, v interface{}) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("failed to marshal test fixture: %v", err)
	}
	return b
}

// fakeRulesetClient implements the rulesetClient interface for tests. It models
// the GitHub API split where the list endpoint returns ruleset summaries and the
// get endpoint returns full details (bypass actors, conditions, rules).
type fakeRulesetClient struct {
	// existing is the current server-side state, keyed by ruleset ID.
	existing map[int]github.Ruleset
	// users maps login -> numeric id for GetUser.
	users map[string]int
	// apps is returned by ListAppInstallationsForOrg.
	apps []github.AppInstallation

	// error injection
	listErr     error
	getErr      error
	createErr   error
	updateErr   error
	deleteErr   error
	getUserErr  error
	listAppsErr error

	// recorded mutations
	created []github.RulesetRequest
	updated map[int]github.RulesetRequest
	deleted []int

	nextID int
}

func (f *fakeRulesetClient) ListRepoRulesets(org, repo string) ([]github.Ruleset, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	var out []github.Ruleset
	for _, rs := range f.existing {
		// The list endpoint returns summaries only; details come from Get.
		out = append(out, github.Ruleset{ID: rs.ID, Name: rs.Name, Enforcement: rs.Enforcement, Target: rs.Target})
	}
	return out, nil
}

func (f *fakeRulesetClient) GetRepoRuleset(org, repo string, id int) (*github.Ruleset, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	rs, ok := f.existing[id]
	if !ok {
		return nil, fmt.Errorf("ruleset %d not found", id)
	}
	return &rs, nil
}

func (f *fakeRulesetClient) CreateRepoRuleset(org, repo string, req github.RulesetRequest) (*github.Ruleset, error) {
	if f.createErr != nil {
		return nil, f.createErr
	}
	f.created = append(f.created, req)
	f.nextID++
	return &github.Ruleset{ID: f.nextID, Name: req.Name}, nil
}

func (f *fakeRulesetClient) UpdateRepoRuleset(org, repo string, id int, req github.RulesetRequest) (*github.Ruleset, error) {
	if f.updateErr != nil {
		return nil, f.updateErr
	}
	if f.updated == nil {
		f.updated = map[int]github.RulesetRequest{}
	}
	f.updated[id] = req
	return &github.Ruleset{ID: id, Name: req.Name}, nil
}

func (f *fakeRulesetClient) DeleteRepoRuleset(org, repo string, id int) error {
	if f.deleteErr != nil {
		return f.deleteErr
	}
	f.deleted = append(f.deleted, id)
	return nil
}

func (f *fakeRulesetClient) ListAppInstallationsForOrg(org string) ([]github.AppInstallation, error) {
	if f.listAppsErr != nil {
		return nil, f.listAppsErr
	}
	return f.apps, nil
}

func (f *fakeRulesetClient) GetUser(login string) (*github.User, error) {
	if f.getUserErr != nil {
		return nil, f.getUserErr
	}
	id, ok := f.users[login]
	if !ok {
		return nil, fmt.Errorf("user %s not found", login)
	}
	return &github.User{Login: login, ID: id}, nil
}

func (f *fakeRulesetClient) createdNames() []string {
	var names []string
	for _, r := range f.created {
		names = append(names, r.Name)
	}
	sort.Strings(names)
	return names
}

func (f *fakeRulesetClient) updatedIDs() []int {
	var ids []int
	for id := range f.updated {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	return ids
}

func (f *fakeRulesetClient) deletedIDs() []int {
	ids := append([]int(nil), f.deleted...)
	sort.Ints(ids)
	return ids
}

func TestConfigureRepoRulesets(t *testing.T) {
	// A status-check rule whose live (Get) form carries an extra field GitHub adds
	// that peribolos never sends. Idempotent comparison must ignore it.
	wantStatusParams := mustRawJSON(t, map[string]interface{}{
		"strict_required_status_checks_policy": false,
		"required_status_checks":               []map[string]interface{}{{"context": "ci/test"}},
	})
	liveStatusParams := mustRawJSON(t, map[string]interface{}{
		"strict_required_status_checks_policy": false,
		"do_not_enforce_on_create":             false, // extra field only present in responses
		"required_status_checks":               []map[string]interface{}{{"context": "ci/test"}},
	})

	cases := []struct {
		name     string
		want     []org.RepoRuleset
		existing []github.Ruleset
		allTeams []github.Team
		users    map[string]int
		apps     []github.AppInstallation

		listErr   error
		getErr    error
		createErr error
		updateErr error
		deleteErr error

		wantCreated []string
		wantUpdated []int
		wantDeleted []int
		wantErr     bool
		verify      func(t *testing.T, f *fakeRulesetClient)
	}{
		{
			name:        "creates missing ruleset",
			want:        []org.RepoRuleset{{Name: "peribolos/policy", Enforcement: "active"}},
			wantCreated: []string{"peribolos/policy"},
		},
		{
			name: "skips ruleset already up to date",
			want: []org.RepoRuleset{{Name: "peribolos/policy", Enforcement: "active"}},
			existing: []github.Ruleset{
				{ID: 1, Name: "peribolos/policy", Enforcement: "active", Target: "branch"},
			},
		},
		{
			name: "updates changed ruleset",
			want: []org.RepoRuleset{{Name: "peribolos/policy", Enforcement: "active"}},
			existing: []github.Ruleset{
				{ID: 7, Name: "peribolos/policy", Enforcement: "disabled", Target: "branch"},
			},
			wantUpdated: []int{7},
		},
		{
			name: "deletes managed ruleset no longer desired",
			want: []org.RepoRuleset{{Name: "peribolos/policy", Enforcement: "active"}},
			existing: []github.Ruleset{
				{ID: 1, Name: "peribolos/policy", Enforcement: "active", Target: "branch"},
				{ID: 2, Name: "peribolos/stale", Enforcement: "active", Target: "branch"},
			},
			wantDeleted: []int{2},
		},
		{
			name: "empty config deletes all managed rulesets",
			want: nil,
			existing: []github.Ruleset{
				{ID: 3, Name: "peribolos/policy", Enforcement: "active", Target: "branch"},
			},
			wantDeleted: []int{3},
		},
		{
			name: "leaves unmanaged rulesets untouched",
			want: []org.RepoRuleset{{Name: "peribolos/policy", Enforcement: "active"}},
			existing: []github.Ruleset{
				{ID: 1, Name: "manually-created", Enforcement: "active", Target: "branch"},
			},
			wantCreated: []string{"peribolos/policy"},
			// manually-created is not prefixed, so it must not be deleted.
		},
		{
			name: "resolves team slug in bypass actor on create",
			want: []org.RepoRuleset{{
				Name:        "peribolos/policy",
				Enforcement: "active",
				BypassActors: []org.RepoRulesetBypassActor{
					{ActorType: "Team", TeamSlug: "admins", BypassMode: "always"},
				},
			}},
			allTeams:    []github.Team{{Slug: "admins", ID: 99}},
			wantCreated: []string{"peribolos/policy"},
			verify: func(t *testing.T, f *fakeRulesetClient) {
				if len(f.created) != 1 || len(f.created[0].BypassActors) != 1 {
					t.Fatalf("expected one created ruleset with one bypass actor, got %+v", f.created)
				}
				got := f.created[0].BypassActors[0]
				if got.ActorID == nil || *got.ActorID != 99 {
					t.Errorf("team slug not resolved to id 99: %+v", got)
				}
			},
		},
		{
			name: "resolves user login in bypass actor on create",
			want: []org.RepoRuleset{{
				Name:        "peribolos/policy",
				Enforcement: "active",
				BypassActors: []org.RepoRulesetBypassActor{
					{ActorType: "User", UserLogin: "octocat", BypassMode: "always"},
				},
			}},
			users:       map[string]int{"octocat": 583231},
			wantCreated: []string{"peribolos/policy"},
			verify: func(t *testing.T, f *fakeRulesetClient) {
				got := f.created[0].BypassActors[0]
				if got.ActorID == nil || *got.ActorID != 583231 {
					t.Errorf("user login not resolved to id 583231: %+v", got)
				}
			},
		},
		{
			name: "idempotent with rules, bypass actors and conditions despite response-only fields",
			want: []org.RepoRuleset{{
				Name:        "peribolos/policy",
				Enforcement: "active",
				BypassActors: []org.RepoRulesetBypassActor{
					{ActorType: "OrganizationAdmin", ActorID: intPtr(1), BypassMode: "always"},
				},
				Conditions: &github.RulesetConditions{
					RefName: &github.RulesetRefNameCondition{Include: []string{"refs/heads/main"}, Exclude: []string{}},
				},
				Rules: []github.RulesetRule{
					{Type: "required_status_checks", Parameters: wantStatusParams},
				},
			}},
			existing: []github.Ruleset{{
				ID:          1,
				Name:        "peribolos/policy",
				Enforcement: "active",
				Target:      "branch",
				BypassActors: []github.RulesetBypassActor{
					{ActorType: "OrganizationAdmin", ActorID: intPtr(1), BypassMode: "always"},
				},
				Conditions: &github.RulesetConditions{
					RefName: &github.RulesetRefNameCondition{Include: []string{"refs/heads/main"}, Exclude: []string{}},
				},
				Rules: []github.RulesetRule{
					{Type: "required_status_checks", Parameters: liveStatusParams},
				},
			}},
			// Nothing to do: create/update/delete all empty.
		},
		{
			name:    "list error is returned",
			want:    []org.RepoRuleset{{Name: "peribolos/policy", Enforcement: "active"}},
			listErr: errors.New("boom"),
			wantErr: true,
		},
		{
			name: "get error is aggregated",
			want: []org.RepoRuleset{{Name: "peribolos/policy", Enforcement: "active"}},
			existing: []github.Ruleset{
				{ID: 1, Name: "peribolos/policy", Enforcement: "active", Target: "branch"},
			},
			getErr:  errors.New("boom"),
			wantErr: true,
		},
		{
			name:      "create error is aggregated",
			want:      []org.RepoRuleset{{Name: "peribolos/a", Enforcement: "active"}, {Name: "peribolos/b", Enforcement: "active"}},
			createErr: errors.New("boom"),
			wantErr:   true,
		},
		{
			name: "delete error is aggregated",
			want: nil,
			existing: []github.Ruleset{
				{ID: 1, Name: "peribolos/policy", Enforcement: "active", Target: "branch"},
			},
			deleteErr: errors.New("boom"),
			wantErr:   true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fc := &fakeRulesetClient{
				existing:  map[int]github.Ruleset{},
				users:     tc.users,
				apps:      tc.apps,
				listErr:   tc.listErr,
				getErr:    tc.getErr,
				createErr: tc.createErr,
				updateErr: tc.updateErr,
				deleteErr: tc.deleteErr,
				nextID:    100,
			}
			for _, rs := range tc.existing {
				fc.existing[rs.ID] = rs
			}

			err := configureRepoRulesets(fc, "org", "repo", tc.want, tc.allTeams)
			if (err != nil) != tc.wantErr {
				t.Fatalf("configureRepoRulesets() error = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.wantErr {
				return
			}

			if got := fc.createdNames(); !stringsEqual(got, tc.wantCreated) {
				t.Errorf("created = %v, want %v", got, tc.wantCreated)
			}
			if got := fc.updatedIDs(); !intsEqual(got, tc.wantUpdated) {
				t.Errorf("updated = %v, want %v", got, tc.wantUpdated)
			}
			if got := fc.deletedIDs(); !intsEqual(got, tc.wantDeleted) {
				t.Errorf("deleted = %v, want %v", got, tc.wantDeleted)
			}
			if tc.verify != nil {
				tc.verify(t, fc)
			}
		})
	}
}

func stringsEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func intsEqual(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestToRulesetRequest(t *testing.T) {
	cases := []struct {
		name         string
		rs           org.RepoRuleset
		teamsBySlug  map[string]github.Team
		usersByLogin map[string]int
		appsBySlug   map[string]int
		verify       func(t *testing.T, req github.RulesetRequest)
	}{
		{
			name: "defaults target to branch",
			rs:   org.RepoRuleset{Name: "peribolos/x", Enforcement: "active"},
			verify: func(t *testing.T, req github.RulesetRequest) {
				if req.Target != "branch" {
					t.Errorf("target = %q, want branch", req.Target)
				}
			},
		},
		{
			name: "keeps explicit target",
			rs:   org.RepoRuleset{Name: "peribolos/x", Enforcement: "active", Target: "tag"},
			verify: func(t *testing.T, req github.RulesetRequest) {
				if req.Target != "tag" {
					t.Errorf("target = %q, want tag", req.Target)
				}
			},
		},
		{
			name: "empty bypass actors serialize as non-nil empty slice",
			rs:   org.RepoRuleset{Name: "peribolos/x", Enforcement: "active"},
			verify: func(t *testing.T, req github.RulesetRequest) {
				if req.BypassActors == nil {
					t.Error("BypassActors must be non-nil so an empty list clears actors on update")
				}
				if len(req.BypassActors) != 0 {
					t.Errorf("BypassActors = %v, want empty", req.BypassActors)
				}
			},
		},
		{
			name: "resolves team slug to id",
			rs: org.RepoRuleset{Name: "peribolos/x", Enforcement: "active", BypassActors: []org.RepoRulesetBypassActor{
				{ActorType: "Team", TeamSlug: "admins", BypassMode: "always"},
			}},
			teamsBySlug: map[string]github.Team{"admins": {Slug: "admins", ID: 5}},
			verify: func(t *testing.T, req github.RulesetRequest) {
				if len(req.BypassActors) != 1 || req.BypassActors[0].ActorID == nil || *req.BypassActors[0].ActorID != 5 {
					t.Errorf("team slug not resolved: %+v", req.BypassActors)
				}
			},
		},
		{
			name: "skips unresolved team slug",
			rs: org.RepoRuleset{Name: "peribolos/x", Enforcement: "active", BypassActors: []org.RepoRulesetBypassActor{
				{ActorType: "Team", TeamSlug: "ghosts", BypassMode: "always"},
			}},
			verify: func(t *testing.T, req github.RulesetRequest) {
				if len(req.BypassActors) != 0 {
					t.Errorf("unresolved team should be skipped, got %+v", req.BypassActors)
				}
			},
		},
		{
			name: "resolves user login to id",
			rs: org.RepoRuleset{Name: "peribolos/x", Enforcement: "active", BypassActors: []org.RepoRulesetBypassActor{
				{ActorType: "User", UserLogin: "octocat", BypassMode: "pull_request"},
			}},
			usersByLogin: map[string]int{"octocat": 7},
			verify: func(t *testing.T, req github.RulesetRequest) {
				if len(req.BypassActors) != 1 || req.BypassActors[0].ActorID == nil || *req.BypassActors[0].ActorID != 7 {
					t.Errorf("user login not resolved: %+v", req.BypassActors)
				}
			},
		},
		{
			name: "skips unresolved user login",
			rs: org.RepoRuleset{Name: "peribolos/x", Enforcement: "active", BypassActors: []org.RepoRulesetBypassActor{
				{ActorType: "User", UserLogin: "nobody", BypassMode: "always"},
			}},
			verify: func(t *testing.T, req github.RulesetRequest) {
				if len(req.BypassActors) != 0 {
					t.Errorf("unresolved user should be skipped, got %+v", req.BypassActors)
				}
			},
		},
		{
			name: "resolves app slug to id",
			rs: org.RepoRuleset{Name: "peribolos/x", Enforcement: "active", BypassActors: []org.RepoRulesetBypassActor{
				{ActorType: "Integration", AppSlug: "my-app", BypassMode: "always"},
			}},
			appsBySlug: map[string]int{"my-app": 11},
			verify: func(t *testing.T, req github.RulesetRequest) {
				if len(req.BypassActors) != 1 || req.BypassActors[0].ActorID == nil || *req.BypassActors[0].ActorID != 11 {
					t.Errorf("app slug not resolved: %+v", req.BypassActors)
				}
			},
		},
		{
			name: "passes through pre-resolved actor id",
			rs: org.RepoRuleset{Name: "peribolos/x", Enforcement: "active", BypassActors: []org.RepoRulesetBypassActor{
				{ActorType: "OrganizationAdmin", ActorID: intPtr(1), BypassMode: "always"},
			}},
			verify: func(t *testing.T, req github.RulesetRequest) {
				if len(req.BypassActors) != 1 || req.BypassActors[0].ActorID == nil || *req.BypassActors[0].ActorID != 1 {
					t.Errorf("pre-resolved actor id not preserved: %+v", req.BypassActors)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := toRulesetRequest(tc.rs, tc.teamsBySlug, tc.usersByLogin, tc.appsBySlug)
			if req.Name != tc.rs.Name {
				t.Errorf("name = %q, want %q", req.Name, tc.rs.Name)
			}
			if req.Enforcement != tc.rs.Enforcement {
				t.Errorf("enforcement = %q, want %q", req.Enforcement, tc.rs.Enforcement)
			}
			tc.verify(t, req)
		})
	}
}

func TestRulesetNeedsUpdate(t *testing.T) {
	base := github.Ruleset{
		Name:        "peribolos/policy",
		Enforcement: "active",
		Target:      "branch",
	}
	baseReq := github.RulesetRequest{
		Name:         "peribolos/policy",
		Enforcement:  "active",
		Target:       "branch",
		BypassActors: []github.RulesetBypassActor{},
	}

	cases := []struct {
		name string
		have github.Ruleset
		want github.RulesetRequest
		need bool
	}{
		{name: "identical", have: base, want: baseReq, need: false},
		{
			name: "name differs",
			have: base,
			want: github.RulesetRequest{Name: "peribolos/other", Enforcement: "active", Target: "branch"},
			need: true,
		},
		{
			name: "enforcement differs",
			have: base,
			want: github.RulesetRequest{Name: "peribolos/policy", Enforcement: "evaluate", Target: "branch"},
			need: true,
		},
		{
			name: "target differs",
			have: base,
			want: github.RulesetRequest{Name: "peribolos/policy", Enforcement: "active", Target: "tag"},
			need: true,
		},
		{
			name: "bypass actor count differs",
			have: base,
			want: github.RulesetRequest{Name: "peribolos/policy", Enforcement: "active", Target: "branch", BypassActors: []github.RulesetBypassActor{
				{ActorType: "Team", ActorID: intPtr(1), BypassMode: "always"},
			}},
			need: true,
		},
		{
			name: "bypass actor content differs",
			have: github.Ruleset{Name: "peribolos/policy", Enforcement: "active", Target: "branch", BypassActors: []github.RulesetBypassActor{
				{ActorType: "Team", ActorID: intPtr(1), BypassMode: "always"},
			}},
			want: github.RulesetRequest{Name: "peribolos/policy", Enforcement: "active", Target: "branch", BypassActors: []github.RulesetBypassActor{
				{ActorType: "Team", ActorID: intPtr(2), BypassMode: "always"},
			}},
			need: true,
		},
		{
			name: "bypass actors equal regardless of order",
			have: github.Ruleset{Name: "peribolos/policy", Enforcement: "active", Target: "branch", BypassActors: []github.RulesetBypassActor{
				{ActorType: "Team", ActorID: intPtr(1), BypassMode: "always"},
				{ActorType: "User", ActorID: intPtr(2), BypassMode: "pull_request"},
			}},
			want: github.RulesetRequest{Name: "peribolos/policy", Enforcement: "active", Target: "branch", BypassActors: []github.RulesetBypassActor{
				{ActorType: "User", ActorID: intPtr(2), BypassMode: "pull_request"},
				{ActorType: "Team", ActorID: intPtr(1), BypassMode: "always"},
			}},
			need: false,
		},
		{
			name: "conditions differ",
			have: github.Ruleset{Name: "peribolos/policy", Enforcement: "active", Target: "branch", Conditions: &github.RulesetConditions{
				RefName: &github.RulesetRefNameCondition{Include: []string{"refs/heads/main"}},
			}},
			want: github.RulesetRequest{Name: "peribolos/policy", Enforcement: "active", Target: "branch", BypassActors: []github.RulesetBypassActor{}, Conditions: &github.RulesetConditions{
				RefName: &github.RulesetRefNameCondition{Include: []string{"refs/heads/release"}},
			}},
			need: true,
		},
		{
			name: "conditions equal regardless of order",
			have: github.Ruleset{Name: "peribolos/policy", Enforcement: "active", Target: "branch", Conditions: &github.RulesetConditions{
				RefName: &github.RulesetRefNameCondition{Include: []string{"refs/heads/main", "refs/heads/release"}},
			}},
			want: github.RulesetRequest{Name: "peribolos/policy", Enforcement: "active", Target: "branch", BypassActors: []github.RulesetBypassActor{}, Conditions: &github.RulesetConditions{
				RefName: &github.RulesetRefNameCondition{Include: []string{"refs/heads/release", "refs/heads/main"}},
			}},
			need: false,
		},
		{
			name: "rule count differs",
			have: base,
			want: github.RulesetRequest{Name: "peribolos/policy", Enforcement: "active", Target: "branch", BypassActors: []github.RulesetBypassActor{}, Rules: []github.RulesetRule{
				{Type: "deletion"},
			}},
			need: true,
		},
		{
			name: "rule type missing",
			have: github.Ruleset{Name: "peribolos/policy", Enforcement: "active", Target: "branch", Rules: []github.RulesetRule{
				{Type: "deletion"},
			}},
			want: github.RulesetRequest{Name: "peribolos/policy", Enforcement: "active", Target: "branch", BypassActors: []github.RulesetBypassActor{}, Rules: []github.RulesetRule{
				{Type: "non_fast_forward"},
			}},
			need: true,
		},
		{
			name: "rule params differ",
			have: github.Ruleset{Name: "peribolos/policy", Enforcement: "active", Target: "branch", Rules: []github.RulesetRule{
				{Type: "pull_request", Parameters: mustRawJSON(t, map[string]interface{}{"required_approving_review_count": 1})},
			}},
			want: github.RulesetRequest{Name: "peribolos/policy", Enforcement: "active", Target: "branch", BypassActors: []github.RulesetBypassActor{}, Rules: []github.RulesetRule{
				{Type: "pull_request", Parameters: mustRawJSON(t, map[string]interface{}{"required_approving_review_count": 2})},
			}},
			need: true,
		},
		{
			name: "rule params match ignoring response-only fields",
			have: github.Ruleset{Name: "peribolos/policy", Enforcement: "active", Target: "branch", Rules: []github.RulesetRule{
				{Type: "pull_request", Parameters: mustRawJSON(t, map[string]interface{}{
					"required_approving_review_count": 1,
					"allowed_merge_methods":           []string{"merge", "squash"},
				})},
			}},
			want: github.RulesetRequest{Name: "peribolos/policy", Enforcement: "active", Target: "branch", BypassActors: []github.RulesetBypassActor{}, Rules: []github.RulesetRule{
				{Type: "pull_request", Parameters: mustRawJSON(t, map[string]interface{}{"required_approving_review_count": 1})},
			}},
			need: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := rulesetNeedsUpdate(tc.have, tc.want); got != tc.need {
				t.Errorf("rulesetNeedsUpdate() = %v, want %v", got, tc.need)
			}
		})
	}
}

func TestRuleParamsMatch(t *testing.T) {
	cases := []struct {
		name  string
		have  json.RawMessage
		want  json.RawMessage
		match bool
	}{
		{name: "both empty", have: nil, want: nil, match: true},
		{name: "have empty want set", have: nil, want: mustRawJSON(t, map[string]int{"a": 1}), match: false},
		{name: "have set want empty", have: mustRawJSON(t, map[string]int{"a": 1}), want: nil, match: false},
		{
			name:  "want subset of have",
			have:  mustRawJSON(t, map[string]interface{}{"a": 1, "b": 2}),
			want:  mustRawJSON(t, map[string]interface{}{"a": 1}),
			match: true,
		},
		{
			name:  "want key missing in have",
			have:  mustRawJSON(t, map[string]interface{}{"a": 1}),
			want:  mustRawJSON(t, map[string]interface{}{"c": 3}),
			match: false,
		},
		{
			name:  "value mismatch",
			have:  mustRawJSON(t, map[string]interface{}{"a": 1}),
			want:  mustRawJSON(t, map[string]interface{}{"a": 2}),
			match: false,
		},
		{
			name:  "nested slice matches",
			have:  mustRawJSON(t, map[string]interface{}{"contexts": []string{"a", "b"}}),
			want:  mustRawJSON(t, map[string]interface{}{"contexts": []string{"a", "b"}}),
			match: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ruleParamsMatch(tc.have, tc.want); got != tc.match {
				t.Errorf("ruleParamsMatch() = %v, want %v", got, tc.match)
			}
		})
	}
}

// allowedActors extracts the resolved dismissal_restriction.allowed_actors from a
// pull_request rule's parameters for assertion.
func allowedActors(t *testing.T, rule github.RulesetRule) []map[string]interface{} {
	t.Helper()
	var params map[string]interface{}
	if err := json.Unmarshal(rule.Parameters, &params); err != nil {
		t.Fatalf("unmarshal params: %v", err)
	}
	dr, ok := params["dismissal_restriction"].(map[string]interface{})
	if !ok {
		return nil
	}
	actors, ok := dr["allowed_actors"].([]interface{})
	if !ok {
		return nil
	}
	var out []map[string]interface{}
	for _, a := range actors {
		out = append(out, a.(map[string]interface{}))
	}
	return out
}

func pullRequestRuleWithActors(t *testing.T, actors []map[string]interface{}) github.RulesetRule {
	t.Helper()
	return github.RulesetRule{
		Type: "pull_request",
		Parameters: mustRawJSON(t, map[string]interface{}{
			"dismissal_restriction": map[string]interface{}{
				"enabled":        true,
				"allowed_actors": actors,
			},
		}),
	}
}

func TestResolveRuleActorSlugs(t *testing.T) {
	teamsBySlug := map[string]github.Team{"admins": {Slug: "admins", ID: 5}}
	usersByLogin := map[string]int{"octocat": 7}

	t.Run("resolves team slug in dismissal restriction", func(t *testing.T) {
		rules := []github.RulesetRule{pullRequestRuleWithActors(t, []map[string]interface{}{
			{"team_slug": "admins", "type": "Team"},
		})}
		got := allowedActors(t, resolveRuleActorSlugs(rules, teamsBySlug, usersByLogin)[0])
		if len(got) != 1 || got[0]["type"] != "Team" || got[0]["id"].(float64) != 5 {
			t.Errorf("team slug not resolved: %+v", got)
		}
		if _, ok := got[0]["team_slug"]; ok {
			t.Errorf("team_slug should be stripped after resolution: %+v", got[0])
		}
	})

	t.Run("resolves user login in dismissal restriction", func(t *testing.T) {
		rules := []github.RulesetRule{pullRequestRuleWithActors(t, []map[string]interface{}{
			{"user_login": "octocat", "type": "User"},
		})}
		got := allowedActors(t, resolveRuleActorSlugs(rules, teamsBySlug, usersByLogin)[0])
		if len(got) != 1 || got[0]["type"] != "User" || got[0]["id"].(float64) != 7 {
			t.Errorf("user login not resolved: %+v", got)
		}
	})

	t.Run("drops unresolved team slug", func(t *testing.T) {
		rules := []github.RulesetRule{pullRequestRuleWithActors(t, []map[string]interface{}{
			{"team_slug": "ghosts", "type": "Team"},
		})}
		got := allowedActors(t, resolveRuleActorSlugs(rules, teamsBySlug, usersByLogin)[0])
		if len(got) != 0 {
			t.Errorf("unresolved team should be dropped, got %+v", got)
		}
	})

	t.Run("keeps actor with numeric id and strips slug keys", func(t *testing.T) {
		rules := []github.RulesetRule{pullRequestRuleWithActors(t, []map[string]interface{}{
			{"id": float64(42), "type": "Team"},
		})}
		got := allowedActors(t, resolveRuleActorSlugs(rules, teamsBySlug, usersByLogin)[0])
		if len(got) != 1 || got[0]["id"].(float64) != 42 {
			t.Errorf("numeric-id actor should be preserved: %+v", got)
		}
	})

	t.Run("leaves non pull_request rules unchanged", func(t *testing.T) {
		rules := []github.RulesetRule{{Type: "required_signatures"}}
		got := resolveRuleActorSlugs(rules, teamsBySlug, usersByLogin)
		if !reflect.DeepEqual(got, rules) {
			t.Errorf("non pull_request rule changed: %+v", got)
		}
	})

	t.Run("leaves pull_request without dismissal restriction unchanged", func(t *testing.T) {
		rules := []github.RulesetRule{{
			Type:       "pull_request",
			Parameters: mustRawJSON(t, map[string]interface{}{"required_approving_review_count": 2}),
		}}
		got := resolveRuleActorSlugs(rules, teamsBySlug, usersByLogin)
		if string(got[0].Parameters) != string(rules[0].Parameters) {
			t.Errorf("params should be unchanged: %s", got[0].Parameters)
		}
	})
}

func TestResolveActorLookups(t *testing.T) {
	t.Run("resolves user logins and app slugs", func(t *testing.T) {
		fc := &fakeRulesetClient{
			users: map[string]int{"u1": 1, "u2": 2},
			apps:  []github.AppInstallation{{ID: 10, AppSlug: "app1"}},
		}
		rulesets := []org.RepoRuleset{{BypassActors: []org.RepoRulesetBypassActor{
			{ActorType: "User", UserLogin: "u1"},
			{ActorType: "User", UserLogin: "u2"},
			{ActorType: "Integration", AppSlug: "app1"},
		}}}
		users, apps := resolveActorLookups(fc, "org", rulesets)
		if !reflect.DeepEqual(users, map[string]int{"u1": 1, "u2": 2}) {
			t.Errorf("users = %v", users)
		}
		if !reflect.DeepEqual(apps, map[string]int{"app1": 10}) {
			t.Errorf("apps = %v", apps)
		}
	})

	t.Run("skips users that fail to resolve", func(t *testing.T) {
		fc := &fakeRulesetClient{getUserErr: errors.New("not found")}
		rulesets := []org.RepoRuleset{{BypassActors: []org.RepoRulesetBypassActor{
			{ActorType: "User", UserLogin: "u1"},
		}}}
		users, _ := resolveActorLookups(fc, "org", rulesets)
		if len(users) != 0 {
			t.Errorf("expected no resolved users, got %v", users)
		}
	})

	t.Run("no lookups when no user or app actors", func(t *testing.T) {
		fc := &fakeRulesetClient{
			getUserErr:  errors.New("should not be called"),
			listAppsErr: errors.New("should not be called"),
		}
		rulesets := []org.RepoRuleset{{BypassActors: []org.RepoRulesetBypassActor{
			{ActorType: "Team", TeamSlug: "admins"},
			{ActorType: "OrganizationAdmin", ActorID: intPtr(1)},
		}}}
		users, apps := resolveActorLookups(fc, "org", rulesets)
		if len(users) != 0 || len(apps) != 0 {
			t.Errorf("expected empty lookups, got users=%v apps=%v", users, apps)
		}
	})
}

func TestEqualRulesetConditions(t *testing.T) {
	cases := []struct {
		name  string
		a, b  *github.RulesetConditions
		equal bool
	}{
		{name: "both nil", a: nil, b: nil, equal: true},
		{name: "one nil", a: nil, b: &github.RulesetConditions{}, equal: false},
		{name: "both empty refname", a: &github.RulesetConditions{}, b: &github.RulesetConditions{}, equal: true},
		{
			name:  "equal include ignoring order",
			a:     &github.RulesetConditions{RefName: &github.RulesetRefNameCondition{Include: []string{"a", "b"}}},
			b:     &github.RulesetConditions{RefName: &github.RulesetRefNameCondition{Include: []string{"b", "a"}}},
			equal: true,
		},
		{
			name:  "different include",
			a:     &github.RulesetConditions{RefName: &github.RulesetRefNameCondition{Include: []string{"a"}}},
			b:     &github.RulesetConditions{RefName: &github.RulesetRefNameCondition{Include: []string{"b"}}},
			equal: false,
		},
		{
			name:  "different exclude",
			a:     &github.RulesetConditions{RefName: &github.RulesetRefNameCondition{Exclude: []string{"a"}}},
			b:     &github.RulesetConditions{RefName: &github.RulesetRefNameCondition{Exclude: []string{"b"}}},
			equal: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := equalRulesetConditions(tc.a, tc.b); got != tc.equal {
				t.Errorf("equalRulesetConditions() = %v, want %v", got, tc.equal)
			}
		})
	}
}

func TestPtrIntVal(t *testing.T) {
	if got := ptrIntVal(nil); got != 0 {
		t.Errorf("ptrIntVal(nil) = %d, want 0", got)
	}
	if got := ptrIntVal(intPtr(9)); got != 9 {
		t.Errorf("ptrIntVal(9) = %d, want 9", got)
	}
}
