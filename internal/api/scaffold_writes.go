package api

import (
	"encoding/json"
	"fmt"
)

// Writes needed by `koolbase create`. Kept apart from the read-only MCP
// surface: these mint real resources and are metered by the API's PlanLimit
// middleware, so their errors matter to the caller in a way reads' do not.

// CreateProject creates a project in the given organization. The slug is
// derived server-side from the name.
//
// A 402 means the caller is at their plan's project limit — surface it as
// that, not as a generic failure: "Free allows 2 projects" is actionable,
// "failed to create project" is not.
func (c *Client) CreateProject(orgID, name string) (*Project, error) {
	data, status, err := c.do("POST", "/v1/organizations/"+orgID+"/projects",
		map[string]any{"name": name})
	if err != nil {
		return nil, err
	}
	if status == 402 {
		return nil, fmt.Errorf("project limit reached on your current plan: %s", string(data))
	}
	if status == 409 {
		return nil, fmt.Errorf("a project with that name already exists in this organization")
	}
	if status != 200 && status != 201 {
		return nil, fmt.Errorf("failed to create project (%d): %s", status, string(data))
	}
	var p Project
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// CreateEnvironment creates an environment in a project and returns it,
// including the public key a generated app initializes with.
//
// A newly created project has NO environments — the project-create path does
// not seed one — so `koolbase create` makes this call itself.
//
// The environment's SECRET key is deliberately not returned by the API (see
// hatchway-api 7ea896b); obtaining one requires an explicit rotation. A
// generated client app needs only the public key.
func (c *Client) CreateEnvironment(projectID, name string) (*Environment, error) {
	data, status, err := c.do("POST", "/v1/projects/"+projectID+"/environments",
		map[string]any{"name": name})
	if err != nil {
		return nil, err
	}
	if status == 402 {
		return nil, fmt.Errorf("environment limit reached on your current plan: %s", string(data))
	}
	if status != 200 && status != 201 {
		return nil, fmt.Errorf("failed to create environment (%d): %s", status, string(data))
	}
	var env Environment
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, err
	}
	return &env, nil
}
