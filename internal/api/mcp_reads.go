package api

import (
	"encoding/json"
	"fmt"
)

// ProjectDetail is the useful subset of GET /v1/projects/{id}. Branding and
// email-template fields are intentionally omitted — an agent reasoning about
// projects needs identity and timestamps, not SMTP config.
type ProjectDetail struct {
	ID        string `json:"id"`
	OrgID     string `json:"org_id"`
	Name      string `json:"name"`
	Slug      string `json:"slug"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

// GetProject fetches a single project by ID.
func (c *Client) GetProject(projectID string) (*ProjectDetail, error) {
	data, status, err := c.do("GET", "/v1/projects/"+projectID, nil)
	if err != nil {
		return nil, err
	}
	if status != 200 {
		return nil, fmt.Errorf("get project failed (%d): %s", status, string(data))
	}
	var p ProjectDetail
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// Environment is the subset of a project environment an agent needs to
// address env-scoped resources (flags, configs).
type Environment struct {
	ID        string `json:"id"`
	ProjectID string `json:"project_id"`
	Name      string `json:"name"`
	Slug      string `json:"slug"`

	// PublicKey is the pk_live_ key a client app initializes with — needed by
	// `koolbase create` to write a working config. The secret key is NOT here
	// and must never be added: the API stopped returning it (hatchway-api
	// 7ea896b) precisely because every caller who could read a project was
	// receiving every environment credential.
	PublicKey string `json:"public_key"`
}

// ListEnvironments returns the environments of a project. Needed so an agent
// can resolve an env_id before calling env-scoped tools like list_flags.
func (c *Client) ListEnvironments(projectID string) ([]Environment, error) {
	data, status, err := c.do("GET", "/v1/projects/"+projectID+"/environments", nil)
	if err != nil {
		return nil, err
	}
	if status != 200 {
		return nil, fmt.Errorf("list environments failed (%d): %s", status, string(data))
	}
	var envs []Environment
	if err := json.Unmarshal(data, &envs); err != nil {
		return nil, err
	}
	return envs, nil
}

// Flag is a feature flag as returned by GET /v1/environments/{env_id}/flags.
type Flag struct {
	ID                string `json:"id"`
	EnvironmentID     string `json:"environment_id"`
	Key               string `json:"key"`
	Enabled           bool   `json:"enabled"`
	RolloutPercentage int    `json:"rollout_percentage"`
	KillSwitch        bool   `json:"kill_switch"`
	Description       string `json:"description"`
}

// ListFlags returns the feature flags of an environment.
func (c *Client) ListFlags(envID string) ([]Flag, error) {
	data, status, err := c.do("GET", "/v1/environments/"+envID+"/flags", nil)
	if err != nil {
		return nil, err
	}
	if status != 200 {
		return nil, fmt.Errorf("list flags failed (%d): %s", status, string(data))
	}
	var flags []Flag
	if err := json.Unmarshal(data, &flags); err != nil {
		return nil, err
	}
	return flags, nil
}

// UpdateFlagRequest is the full-replace body PUT /flags/{flag_id} expects.
// All four fields are always sent; callers wanting partial semantics must
// read current state and merge before calling (the MCP set_flag tool does).
type UpdateFlagRequest struct {
	Enabled           bool   `json:"enabled"`
	RolloutPercentage int    `json:"rollout_percentage"`
	KillSwitch        bool   `json:"kill_switch"`
	Description       string `json:"description"`
}

// UpdateFlag replaces a feature flag's mutable fields. Returns the updated
// flag. A read-scoped key receives 403 insufficient_scope here (surfaced by
// the status/body in the error), which callers should relay clearly.
func (c *Client) UpdateFlag(flagID string, req UpdateFlagRequest) (*Flag, error) {
	data, status, err := c.do("PUT", "/v1/flags/"+flagID, req)
	if err != nil {
		return nil, err
	}
	if status != 200 {
		return nil, fmt.Errorf("update flag failed (%d): %s", status, string(data))
	}
	var f Flag
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, err
	}
	return &f, nil
}

// Config is a remote-config entry from GET /v1/environments/{env_id}/configs.
// Value is raw JSON: a string config is "text", a number is 42, a bool is
// true, a json config is an object/array. ValueType records which.
type Config struct {
	ID            string          `json:"id"`
	EnvironmentID string          `json:"environment_id"`
	Key           string          `json:"key"`
	Value         json.RawMessage `json:"value"`
	ValueType     string          `json:"value_type"`
	Description   string          `json:"description"`
}

// ListConfigs returns the remote-config entries of an environment.
func (c *Client) ListConfigs(envID string) ([]Config, error) {
	data, status, err := c.do("GET", "/v1/environments/"+envID+"/configs", nil)
	if err != nil {
		return nil, err
	}
	if status != 200 {
		return nil, fmt.Errorf("list configs failed (%d): %s", status, string(data))
	}
	var configs []Config
	if err := json.Unmarshal(data, &configs); err != nil {
		return nil, err
	}
	return configs, nil
}

// UpdateConfigRequest is the body PUT /v1/configs/{config_id} expects. Value
// is raw JSON matching the config's value_type. Full replace of value +
// description; callers wanting partial semantics merge before calling.
type UpdateConfigRequest struct {
	Value       json.RawMessage `json:"value"`
	Description string          `json:"description"`
}

// UpdateConfig replaces a remote config's value and description. A read-scoped
// key receives insufficient_scope here (surfaced via the error).
func (c *Client) UpdateConfig(configID string, req UpdateConfigRequest) (*Config, error) {
	data, status, err := c.do("PUT", "/v1/configs/"+configID, req)
	if err != nil {
		return nil, err
	}
	if status != 200 {
		return nil, fmt.Errorf("update config failed (%d): %s", status, string(data))
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}
