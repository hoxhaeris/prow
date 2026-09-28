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
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"text/tabwriter"

	"sigs.k8s.io/prow/pkg/config/secret"
)

// changeAction enumerates the mutations peribolos performs during a run.
type changeAction string

const (
	actionAdd    changeAction = "add"
	actionUpdate changeAction = "update"
	actionRemove changeAction = "remove"
)

// Change kinds. These mirror the objects peribolos reconciles.
const (
	kindOrgMeta      = "org_meta"
	kindOrgMember    = "org_member"
	kindTeam         = "team"
	kindTeamMember   = "team_member"
	kindTeamRepo     = "team_repo"
	kindRepo         = "repo"
	kindCollaborator = "collaborator"
)

// change is a single planned or applied mutation. The same value is recorded in
// dry-run and confirm mode: --confirm only gates the mutating API call, not the
// decision to make it, so what the report describes equals what peribolos does.
type change struct {
	// Kind is the type of object changed (see the kind* constants).
	Kind string `json:"kind"`
	// Org is the organization the change belongs to.
	Org string `json:"org"`
	// Scope is the containing object for nested changes: the team name for
	// team_member and team_repo changes, and the repo name for collaborator
	// changes. It is empty for org-level changes.
	Scope string `json:"scope,omitempty"`
	// Target is the object acted on: a login, team name or repo name.
	Target string `json:"target"`
	// Action is add, update or remove.
	Action changeAction `json:"action"`
	// Before and After describe the change. They hold non-sensitive descriptors
	// (roles, permission levels, or the names of changed fields), never
	// credential values. Report output is additionally run through
	// secret.Censor before it is written.
	Before any `json:"before,omitempty"`
	After  any `json:"after,omitempty"`
}

// changeReport accumulates the changes decided during a run. It is safe for
// concurrent use. A nil *changeReport is a valid no-op recorder, so callers (and
// tests) that do not care about reporting may pass nil.
type changeReport struct {
	mu      sync.Mutex
	dryRun  bool
	changes []change
}

func newChangeReport(dryRun bool) *changeReport {
	return &changeReport{dryRun: dryRun}
}

// record appends c. Callers only record real mutations, so no-ops never reach
// here. A nil receiver is a no-op.
func (r *changeReport) record(c change) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.changes = append(r.changes, c)
}

// membershipRoles returns the (super, member) role labels for a membership kind.
func membershipRoles(kind string) (super, member string) {
	if kind == kindTeamMember {
		return "maintainer", "member"
	}
	return "admin", "member"
}

// recordMemberAdd records the effect of an adder call: an add for a new member,
// or an update when an existing member changes role (e.g. member -> admin). have
// must use normalized logins, matching user.
func (r *changeReport) recordMemberAdd(kind, org, scope, user string, have memberships, super bool) {
	superLabel, memberLabel := membershipRoles(kind)
	after := memberLabel
	if super {
		after = superLabel
	}
	switch {
	case have.super.Has(user):
		r.record(change{Kind: kind, Org: org, Scope: scope, Target: user, Action: actionUpdate, Before: superLabel, After: after})
	case have.members.Has(user):
		r.record(change{Kind: kind, Org: org, Scope: scope, Target: user, Action: actionUpdate, Before: memberLabel, After: after})
	default:
		r.record(change{Kind: kind, Org: org, Scope: scope, Target: user, Action: actionAdd, After: after})
	}
}

// recordMemberRemove records a membership removal, capturing the prior role when
// known. have must use normalized logins, matching user.
func (r *changeReport) recordMemberRemove(kind, org, scope, user string, have memberships) {
	superLabel, memberLabel := membershipRoles(kind)
	var before any
	switch {
	case have.super.Has(user):
		before = superLabel
	case have.members.Has(user):
		before = memberLabel
	}
	r.record(change{Kind: kind, Org: org, Scope: scope, Target: user, Action: actionRemove, Before: before})
}

// reportOutput is the machine-readable form written by --report-path.
type reportOutput struct {
	DryRun  bool                 `json:"dry_run"`
	Summary map[changeAction]int `json:"summary"`
	Changes []change             `json:"changes"`
}

// output returns a deterministic snapshot: changes sorted by (kind, org, scope,
// target, action), plus a per-action tally. Determinism comes from this sort,
// never from the order changes were recorded in.
func (r *changeReport) output() reportOutput {
	r.mu.Lock()
	defer r.mu.Unlock()

	sorted := make([]change, len(r.changes))
	copy(sorted, r.changes)
	sort.Slice(sorted, func(i, j int) bool {
		a, b := sorted[i], sorted[j]
		switch {
		case a.Kind != b.Kind:
			return a.Kind < b.Kind
		case a.Org != b.Org:
			return a.Org < b.Org
		case a.Scope != b.Scope:
			return a.Scope < b.Scope
		case a.Target != b.Target:
			return a.Target < b.Target
		default:
			return a.Action < b.Action
		}
	})

	summary := map[changeAction]int{
		actionAdd:    0,
		actionUpdate: 0,
		actionRemove: 0,
	}
	for _, c := range sorted {
		summary[c.Action]++
	}
	return reportOutput{DryRun: r.dryRun, Summary: summary, Changes: sorted}
}

// writeJSON writes the machine-readable report. Registered secrets are censored.
func (r *changeReport) writeJSON(w io.Writer) error {
	raw, err := json.MarshalIndent(r.output(), "", "  ")
	if err != nil {
		return err
	}
	raw = append(secret.Censor(raw), '\n')
	_, err = w.Write(raw)
	return err
}

// writeHuman writes a human-readable summary: a headline tally, an aligned table
// of every change, and (in dry-run) a hint to re-run with --confirm. Registered
// secrets are censored.
func (r *changeReport) writeHuman(w io.Writer) error {
	out := r.output()
	var buf bytes.Buffer

	verb := "Applied"
	if out.DryRun {
		verb = "Planned"
	}

	if len(out.Changes) == 0 {
		fmt.Fprintf(&buf, "%s changes: none.\n", verb)
		_, err := w.Write(secret.Censor(buf.Bytes()))
		return err
	}

	fmt.Fprintf(&buf, "%s changes: %d to add, %d to update, %d to remove.\n\n",
		verb, out.Summary[actionAdd], out.Summary[actionUpdate], out.Summary[actionRemove])

	tw := tabwriter.NewWriter(&buf, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ACTION\tKIND\tORG\tSCOPE\tTARGET\tCHANGE")
	for _, c := range out.Changes {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			c.Action, c.Kind, c.Org, dashIfEmpty(c.Scope), c.Target, formatDelta(c))
	}
	tw.Flush()

	if out.DryRun {
		fmt.Fprint(&buf, "\nDry run: re-run with --confirm to apply.\n")
	}

	_, err := w.Write(secret.Censor(buf.Bytes()))
	return err
}

func dashIfEmpty(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// formatDelta renders the CHANGE column for a single change.
func formatDelta(c change) string {
	switch c.Action {
	case actionAdd:
		return stringify(c.After)
	case actionRemove:
		return stringify(c.Before)
	default: // update
		switch {
		case c.Before == nil && c.After == nil:
			return ""
		case c.Before == nil:
			return stringify(c.After)
		case c.After == nil:
			return stringify(c.Before)
		default:
			return fmt.Sprintf("%s -> %s", stringify(c.Before), stringify(c.After))
		}
	}
}

// stringify renders a before/after value. String slices (used for lists of
// changed field names) are comma-joined; everything else uses its default format.
func stringify(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case []string:
		return strings.Join(t, ", ")
	default:
		return fmt.Sprintf("%v", t)
	}
}
