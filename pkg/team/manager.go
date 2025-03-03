// Copyright 2021 Authors of Cilium
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package team

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strings"

	gh "github.com/google/go-github/v33/github"
	"github.com/shurcooL/githubv4"

	"github.com/cilium/team-manager/pkg/comparator"
	"github.com/cilium/team-manager/pkg/config"
	"github.com/cilium/team-manager/pkg/github"
	"github.com/cilium/team-manager/pkg/slices"
	"github.com/cilium/team-manager/pkg/terminal"
)

type Manager struct {
	owner       string
	ghClient    *gh.Client
	gqlGHClient *githubv4.Client
}

func NewManager(ghClient *gh.Client, gqlGHClient *githubv4.Client, owner string) *Manager {
	return &Manager{
		owner:       owner,
		ghClient:    ghClient,
		gqlGHClient: gqlGHClient,
	}
}

// GetCurrentConfig returns a *config.Config by querying the organization teams.
// It will not populate the excludedMembers from CodeReviewAssignments as GH
// does not provide an API of such field.
func (tm *Manager) GetCurrentConfig(ctx context.Context) (*config.Config, error) {
	c := &config.Config{
		Organization: tm.owner,
		Teams:        map[string]config.TeamConfig{},
		Members:      map[string]config.User{},
	}

	variables := map[string]interface{}{}

	result, err := tm.query(ctx, variables)
	if err != nil {
		return nil, fmt.Errorf("failed to query github api: %w", err)
	}

	requeryTeams := false
	for {
		if requeryTeams {
			result, err = tm.query(ctx, variables)
			if err != nil {
				return nil, fmt.Errorf("failed to requery teams: %w", err)
			}
			requeryTeams = false
		}

		for _, t := range result.Organization.Teams.Nodes {
			strTeamName := string(t.Name)
			teamCfg, ok := c.Teams[strTeamName]
			if !ok {
				var cra config.CodeReviewAssignment
				if t.ReviewRequestDelegationEnabled {
					cra = config.CodeReviewAssignment{
						Algorithm:       config.TeamReviewAssignmentAlgorithm(t.ReviewRequestDelegationAlgorithm),
						Enabled:         bool(t.ReviewRequestDelegationEnabled),
						NotifyTeam:      bool(t.ReviewRequestDelegationNotifyTeam),
						TeamMemberCount: int(t.ReviewRequestDelegationMemberCount),
					}
				}
				teamCfg = config.TeamConfig{
					ID:                   fmt.Sprintf("%v", t.ID),
					CodeReviewAssignment: cra,
				}
			}

			requeryMembers := false
			for {
				// Requery of members shouldn't override the teams result
				innerResult := result
				if requeryMembers {
					innerResult, err = tm.query(ctx, variables)
					if err != nil {
						return nil, fmt.Errorf("failed to requery team members: %w", err)
					}
					requeryMembers = false
				}
				// Find team in result - especially important after requerying
				teamNode, err := innerResult.Organization.Teams.WithID(t.ID)
				if err != nil {
					return nil, err
				}
				for _, member := range teamNode.Members.Nodes {
					strLogin := string(member.Login)
					teamCfg.Members = append(teamCfg.Members, strLogin)
					c.Members[strLogin] = config.User{
						ID:   fmt.Sprintf("%v", member.ID),
						Name: string(member.Name),
					}
				}
				sort.Strings(teamCfg.Members)
				c.Teams[strTeamName] = teamCfg
				if !teamNode.Members.PageInfo.HasNextPage {
					break
				}
				requeryMembers = true
				variables["membersCursor"] = githubv4.NewString(teamNode.Members.PageInfo.EndCursor)
			}
		}
		if !result.Organization.Teams.PageInfo.HasNextPage {
			break
		}
		requeryTeams = true
		variables["teamsCursor"] = githubv4.NewString(result.Organization.Teams.PageInfo.EndCursor)
		// Clear the membersCursor as we are only using it when querying over members
		variables["membersCursor"] = (*githubv4.String)(nil)
	}
	return c, nil
}

func (tm *Manager) query(ctx context.Context, additionalVariables map[string]interface{}) (queryResult, error) {
	var q queryResult
	variables := map[string]interface{}{
		"repositoryOwner": githubv4.String(tm.owner),
		"teamsCursor":     (*githubv4.String)(nil), // Null after argument to get first page.
		"membersCursor":   (*githubv4.String)(nil), // Null after argument to get first page.
	}

	for k, v := range additionalVariables {
		variables[k] = v
	}

	err := tm.gqlGHClient.Query(ctx, &q, variables)
	if err != nil {
		return queryResult{}, err
	}

	return q, nil
}

//	{
//	 organization(login: "cilium") {
//	   teams(first: 100) {
//	     nodes {
//	       members(first: 100) {
//	         nodes {
//	           id
//	           login
//	         }
//	       }
//	     }
//	   }
//	 }
//	}
type queryResult struct {
	Organization struct {
		Teams Teams `graphql:"teams(first: 100, after: $teamsCursor)"`
	} `graphql:"organization(login: $repositoryOwner)"`
}

type Teams struct {
	Nodes    []team
	PageInfo struct {
		EndCursor   githubv4.String
		HasNextPage githubv4.Boolean
	}
}

func (t Teams) WithID(id githubv4.ID) (team, error) {
	for _, n := range t.Nodes {
		if n.ID == id {
			return n, nil
		}
	}

	return team{}, fmt.Errorf("team with id %q not found", id)
}

type team struct {
	Members struct {
		Nodes    []teamMember
		PageInfo struct {
			EndCursor   githubv4.String
			HasNextPage githubv4.Boolean
		}
	} `graphql:"members(first: 100, after: $membersCursor)"`
	ID                                 githubv4.ID
	DatabaseID                         githubv4.Int
	Name                               githubv4.String
	ReviewRequestDelegationEnabled     githubv4.Boolean
	ReviewRequestDelegationAlgorithm   githubv4.String
	ReviewRequestDelegationMemberCount githubv4.Int
	ReviewRequestDelegationNotifyTeam  githubv4.Boolean
}

type teamMember struct {
	ID    githubv4.ID
	Login githubv4.String
	Name  githubv4.String
}

// SyncTeamMembers adds and removes the given login names into the given team
// name.
func (tm *Manager) SyncTeamMembers(ctx context.Context, teamName string, add, remove []string) error {
	for _, user := range add {
		fmt.Printf("Adding member %s to team %s\n", user, teamName)
		if _, _, err := tm.ghClient.Teams.AddTeamMembershipBySlug(ctx, tm.owner, slug(teamName), user, &gh.TeamAddTeamMembershipOptions{Role: "member"}); err != nil {
			return err
		}
	}
	for _, user := range remove {
		fmt.Printf("Removing member %s from team %s\n", user, teamName)
		if _, err := tm.ghClient.Teams.RemoveTeamMembershipBySlug(ctx, tm.owner, slug(teamName), user); err != nil {
			return err
		}
	}
	return nil
}

// SyncTeamReviewAssignment updates the review assignment into GH for the given
// team name with the given team ID.
func (tm *Manager) SyncTeamReviewAssignment(ctx context.Context, teamID githubv4.ID, input github.UpdateTeamReviewAssignmentInput) error {
	var m struct {
		UpdateTeamReviewAssignment struct {
			Team struct {
				ID githubv4.ID
			}
		} `graphql:"updateTeamReviewAssignment(input: $input)"`
	}
	input.ID = teamID
	return tm.gqlGHClient.Mutate(ctx, &m, input, nil)
}

func (tm *Manager) SyncTeams(ctx context.Context, localCfg *config.Config, force bool, dryRun bool) (*config.Config, error) {
	upstreamCfg, err := tm.GetCurrentConfig(ctx)
	if err != nil {
		return nil, err
	}

	type teamChange struct {
		add, remove []string
	}
	teamChanges := map[string]teamChange{}

	for localTeamName, localTeam := range localCfg.Teams {
		// Since we can't get the list of excluded members from GH we have
		// to back it up and re-added it again at the end of this for-loop.
		backExcludedMembers := localTeam.CodeReviewAssignment.ExcludedMembers

		localTeam.CodeReviewAssignment.ExcludedMembers = nil
		if !reflect.DeepEqual(localTeam, upstreamCfg.Teams[localTeamName]) {
			cmp := comparator.CompareWithNames(localTeam, upstreamCfg.Teams[localTeamName], "local", "remote")
			fmt.Printf("Local config out of sync with upstream: %s\n", cmp)
			toAdd := slices.NotIn(localTeam.Members, upstreamCfg.Teams[localTeamName].Members)
			toDel := slices.NotIn(upstreamCfg.Teams[localTeamName].Members, localTeam.Members)
			if len(toAdd) != 0 || len(toDel) != 0 {
				teamChanges[localTeamName] = teamChange{
					add:    toAdd,
					remove: toDel,
				}
			}
		}
		localTeam.CodeReviewAssignment.ExcludedMembers = backExcludedMembers
	}

	if len(teamChanges) != 0 {
		fmt.Printf("Going to submit the following changes:\n")
		for teamName, teamCfg := range teamChanges {
			fmt.Printf(" Team: %s\n", teamName)
			fmt.Printf("    Adding members: %s\n", strings.Join(teamCfg.add, ", "))
			fmt.Printf("  Removing members: %s\n", strings.Join(teamCfg.remove, ", "))
		}
		yes := force
		if !force {
			yes, err = terminal.AskForConfirmation("Continue?")
			if err != nil {
				return nil, err
			}
		}
		if yes {
			for teamName, teamCfg := range teamChanges {
				if !dryRun {
					if err := tm.SyncTeamMembers(ctx, teamName, teamCfg.add, teamCfg.remove); err != nil {
						fmt.Fprintf(os.Stderr, "[ERROR]:  Unable to sync team %s: %s\n", teamName, err)
						continue
					}
				}
				teamMembers := map[string]struct{}{}
				for _, member := range localCfg.Teams[teamName].Members {
					teamMembers[member] = struct{}{}
				}
				for _, rmMember := range teamCfg.remove {
					delete(teamMembers, rmMember)
				}
				for _, addMember := range teamCfg.add {
					teamMembers[addMember] = struct{}{}
				}
				team := localCfg.Teams[teamName]
				team.Members = make([]string, 0, len(teamMembers))
				for teamMember := range teamMembers {
					team.Members = append(team.Members, teamMember)
				}
				localCfg.Teams[teamName] = team
			}
		}
	}

	yes := force
	if !force {
		yes, err = terminal.AskForConfirmation("Do you want to update CodeReviewAssignments?")
		if err != nil {
			return nil, err
		}
	}
	if yes {
		teamNames := make([]string, 0, len(localCfg.Teams))
		for teamName := range localCfg.Teams {
			teamNames = append(teamNames, teamName)
		}
		sort.Strings(teamNames)
		for _, teamName := range teamNames {
			storedTeam := localCfg.Teams[teamName]
			cra := storedTeam.CodeReviewAssignment
			usersIDs := getExcludedUsers(teamName, localCfg.Members, cra.ExcludedMembers, localCfg.ExcludeCRAFromAllTeams)

			input := github.UpdateTeamReviewAssignmentInput{
				Algorithm:             cra.Algorithm,
				Enabled:               githubv4.Boolean(cra.Enabled),
				ExcludedTeamMemberIDs: usersIDs,
				NotifyTeam:            githubv4.Boolean(cra.NotifyTeam),
				TeamMemberCount:       githubv4.Int(cra.TeamMemberCount),
			}
			fmt.Printf("Excluding members from team: %s\n", teamName)
			if !dryRun {
				err := tm.SyncTeamReviewAssignment(ctx, storedTeam.ID, input)
				if err != nil {
					fmt.Fprintf(os.Stderr, "[ERROR]: Unable to sync team excluded members %s: %s\n", teamName, err)
				}
			}
		}
	}

	return localCfg, nil
}

// getExcludedUsers returns a list of all users that should be excluded for the
// given team.
func getExcludedUsers(teamName string, members map[string]config.User, excTeamMembers []config.ExcludedMember, excAllTeams []string) []githubv4.ID {
	m := make(map[githubv4.ID]struct{}, len(excTeamMembers)+len(excAllTeams))
	for _, member := range excTeamMembers {
		user, ok := members[member.Login]
		if !ok {
			fmt.Printf("[ERROR] user %q from team %s, not found in the list of team members in the organization\n", member.Login, teamName)
			continue
		}
		m[user.ID] = struct{}{}
	}
	for _, member := range excAllTeams {
		user, ok := members[member]
		if !ok {
			// Ignore if it doesn't belong to the team
			continue
		}
		m[user.ID] = struct{}{}
	}

	memberIDs := make([]githubv4.ID, 0, len(m))
	for memberID := range m {
		memberIDs = append(memberIDs, memberID)
	}
	return memberIDs
}

// slug returns the slug version of the team name. This simply replaces all
// characters that are not in the following regex `[^a-z0-9]+` with a `-`.
// It's a simplistic versions of the official's GitHub slug transformation since
// GitHub changes accents characters as well, for example 'ä' to 'a'.
func slug(s string) string {
	s = strings.ToLower(s)

	re := regexp.MustCompile("[^a-z0-9]+")
	s = re.ReplaceAllString(s, "-")

	s = strings.Trim(s, "-")
	return s
}
