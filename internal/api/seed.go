package api

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// Seed artifact types. These mirror the server's, deliberately as a separate
// declaration rather than a shared module: the CLI and the server version
// independently, and a shape mismatch should surface as a decode error rather
// than a compile-time coupling that forces them to move together.

type SeedManifest struct {
	Version     int              `json:"version"`
	Collections []SeedCollection `json:"collections"`
}

type SeedCollection struct {
	Name string   `json:"name"`
	File string   `json:"file"`
	Key  []string `json:"key"`
}

type SeedFile struct {
	Collection string           `json:"collection"`
	Key        []string         `json:"key"`
	Rows       []map[string]any `json:"rows"`
}

type SeedApplyRequest struct {
	Collection     SeedCollection `json:"collection"`
	File           SeedFile       `json:"file"`
	OnConflict     string         `json:"on_conflict,omitempty"`
	ForceConflicts bool           `json:"force_conflicts,omitempty"`
	AdoptExisting  bool           `json:"adopt_existing,omitempty"`
}

type SeedFieldDivergence struct {
	Field    string `json:"field"`
	InFile   any    `json:"in_file"`
	InTarget any    `json:"in_target"`
}

type SeedRowPlan struct {
	Collection  string                `json:"collection"`
	KeyValue    []string              `json:"key_value"`
	Action      string                `json:"action"`
	RecordID    string                `json:"record_id,omitempty"`
	Divergences []SeedFieldDivergence `json:"divergences,omitempty"`
	Detail      string                `json:"detail,omitempty"`
}

type SeedApplyResult struct {
	TargetProjectID string        `json:"target_project_id"`
	Collection      string        `json:"collection"`
	DryRun          bool          `json:"dry_run"`
	Status          string        `json:"status"`
	Plan            []SeedRowPlan `json:"plan"`
	// Rows carries the per-row apply outcome. Not rendered: the plan already
	// names every row and what happened to it, and duplicating the server's
	// ItemResult here would couple this package to a shape it does not use.
	Rows []map[string]any `json:"rows"`
}

// SeedApply plans or applies one collection's reference data.
func (c *Client) SeedApply(projectID string, req SeedApplyRequest, dryRun bool) (SeedApplyResult, error) {
	path := "/v1/projects/" + projectID + "/seed/apply"
	if dryRun {
		path += "?dry_run=true"
	}
	// do marshals the body itself. Passing pre-marshalled bytes would
	// base64-encode them into a JSON string and the server would receive a
	// string where an object belongs.
	data, status, err := c.do("POST", path, req)
	if err != nil {
		return SeedApplyResult{}, err
	}
	if status != http.StatusOK {
		// The server's refusals carry the reason -- which rows conflict, which
		// need adopting, why a key is not unique -- so it is surfaced verbatim
		// rather than reduced to a status code.
		var e struct {
			Error   string         `json:"error"`
			Code    string         `json:"code"`
			Details map[string]any `json:"details"`
		}
		if json.Unmarshal(data, &e) == nil && e.Error != "" {
			if rows, ok := e.Details["rows"]; ok {
				return SeedApplyResult{}, fmt.Errorf("%s (%v)", e.Error, rows)
			}
			if problems, ok := e.Details["problems"]; ok {
				return SeedApplyResult{}, fmt.Errorf("%s: %v", e.Error, problems)
			}
			return SeedApplyResult{}, fmt.Errorf("%s", e.Error)
		}
		return SeedApplyResult{}, fmt.Errorf("seed apply failed (%d): %s", status, data)
	}
	var res SeedApplyResult
	if err := json.Unmarshal(data, &res); err != nil {
		return SeedApplyResult{}, fmt.Errorf("could not read the server's response: %w", err)
	}
	if res.Status == "" {
		return SeedApplyResult{}, fmt.Errorf("the server returned an unrecognised response shape; this CLI and the server are out of step")
	}
	return res, nil
}
