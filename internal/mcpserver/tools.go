package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/kennedyowusu/koolbase-cli/internal/api"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// registerTools wires every MCP tool onto the server. Tools are added in
// ascending order of privilege; each is a thin adapter over the existing
// api.Client, so the backend's authorization (org ownership, key scope,
// plan limits) stays authoritative. No business logic lives here.
func (s *Server) registerTools() {
	s.addWhoami()
	s.addListProjects()
	s.addGetProject()
	s.addDescribeProject()
	s.addGetCollection()
	s.addSdkConventions()
	s.addListEnvironments()
	s.addListFlags()
	s.addSetFlag()
	s.addListConfigs()
	s.addSetConfig()
	s.addListPatches()
	if s.opts.EnableCodepushMutations {
		s.addPublishPatch()
		s.addRecallPatch()
	}
}

// --- whoami -----------------------------------------------------------------

// whoamiOut is the structured result of the whoami tool. Field docs become
// the output schema via jsonschema struct tags.
type whoamiOut struct {
	Type    string `json:"type" jsonschema:"principal kind: api_key or user"`
	OrgID   string `json:"org_id" jsonschema:"the organization this principal acts in"`
	OrgName string `json:"org_name" jsonschema:"human-readable organization name"`
	Scope   string `json:"scope,omitempty" jsonschema:"for api_key principals: read, write, or admin — the ceiling on all operations"`
}

// addWhoami registers the whoami tool: returns the identity and scope of the
// key or session the server is running as. Read-only, no input. This is also
// the server's own startup probe to learn its scope ceiling.
func (s *Server) addWhoami() {
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "koolbase_whoami",
		Description: "Return the identity the Koolbase MCP server is acting as: principal type, organization, and (for API keys) the scope ceiling that limits what every other tool can do.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, whoamiOut, error) {
		w, err := s.client.Whoami()
		if err != nil {
			return nil, whoamiOut{}, err
		}
		return nil, whoamiOut{
			Type:    w.Type,
			OrgID:   w.OrgID,
			OrgName: w.OrgName,
			Scope:   w.Scope,
		}, nil
	})
}

// --- list_projects ----------------------------------------------------------

type projectSummary struct {
	ID   string `json:"id" jsonschema:"project UUID"`
	Name string `json:"name" jsonschema:"project name"`
}

type listProjectsOut struct {
	Projects []projectSummary `json:"projects" jsonschema:"projects in the caller's organization"`
}

func (s *Server) addListProjects() {
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "koolbase_list_projects",
		Description: "List all projects in the organization the server is acting for. Takes no arguments — the organization is determined by the configured API key.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, listProjectsOut, error) {
		orgID, err := s.org()
		if err != nil {
			return nil, listProjectsOut{}, err
		}
		projects, err := s.client.ListProjects(orgID)
		if err != nil {
			return nil, listProjectsOut{}, err
		}
		out := listProjectsOut{Projects: make([]projectSummary, 0, len(projects))}
		for _, p := range projects {
			out.Projects = append(out.Projects, projectSummary{ID: p.ID, Name: p.Name})
		}
		return nil, out, nil
	})
}

// --- get_project ------------------------------------------------------------

type getProjectIn struct {
	ProjectID string `json:"project_id" jsonschema:"UUID of the project to fetch"`
}

type getProjectOut struct {
	ID        string `json:"id"`
	OrgID     string `json:"org_id"`
	Name      string `json:"name"`
	Slug      string `json:"slug"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

func (s *Server) addGetProject() {
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "koolbase_get_project",
		Description: "Fetch a single Koolbase project by its UUID: identity, slug, and timestamps.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in getProjectIn) (*mcp.CallToolResult, getProjectOut, error) {
		p, err := s.client.GetProject(in.ProjectID)
		if err != nil {
			return nil, getProjectOut{}, err
		}
		return nil, getProjectOut{
			ID: p.ID, OrgID: p.OrgID, Name: p.Name, Slug: p.Slug,
			CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt,
		}, nil
	})
}

// --- list_flags -------------------------------------------------------------

type listFlagsIn struct {
	EnvironmentID string `json:"environment_id" jsonschema:"UUID of the environment whose flags to list. Resolve one via koolbase_list_environments if unknown."`
}

type flagSummary struct {
	ID                string `json:"id"`
	Key               string `json:"key" jsonschema:"the flag's programmatic key"`
	Enabled           bool   `json:"enabled"`
	RolloutPercentage int    `json:"rollout_percentage"`
	KillSwitch        bool   `json:"kill_switch"`
	Description       string `json:"description"`
}

type listFlagsOut struct {
	Flags []flagSummary `json:"flags"`
}

func (s *Server) addListFlags() {
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "koolbase_list_flags",
		Description: "List the feature flags of a Koolbase environment, with their enabled state, rollout percentage, and kill-switch status.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in listFlagsIn) (*mcp.CallToolResult, listFlagsOut, error) {
		flags, err := s.client.ListFlags(in.EnvironmentID)
		if err != nil {
			return nil, listFlagsOut{}, err
		}
		out := listFlagsOut{Flags: make([]flagSummary, 0, len(flags))}
		for _, f := range flags {
			out.Flags = append(out.Flags, flagSummary{
				ID: f.ID, Key: f.Key, Enabled: f.Enabled,
				RolloutPercentage: f.RolloutPercentage,
				KillSwitch:        f.KillSwitch, Description: f.Description,
			})
		}
		return nil, out, nil
	})
}

// --- list_environments (helper for resolving env_ids) -----------------------

type listEnvironmentsIn struct {
	ProjectID string `json:"project_id" jsonschema:"UUID of the project whose environments to list"`
}

type environmentSummary struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Slug string `json:"slug"`
}

type listEnvironmentsOut struct {
	Environments []environmentSummary `json:"environments"`
}

func (s *Server) addListEnvironments() {
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "koolbase_list_environments",
		Description: "List the environments of a Koolbase project (e.g. production, staging). Use this to resolve an environment_id before calling environment-scoped tools like koolbase_list_flags.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in listEnvironmentsIn) (*mcp.CallToolResult, listEnvironmentsOut, error) {
		envs, err := s.client.ListEnvironments(in.ProjectID)
		if err != nil {
			return nil, listEnvironmentsOut{}, err
		}
		out := listEnvironmentsOut{Environments: make([]environmentSummary, 0, len(envs))}
		for _, e := range envs {
			out.Environments = append(out.Environments, environmentSummary{ID: e.ID, Name: e.Name, Slug: e.Slug})
		}
		return nil, out, nil
	})
}

// --- set_flag (first write tool) --------------------------------------------

type setFlagIn struct {
	EnvironmentID     string  `json:"environment_id" jsonschema:"UUID of the environment the flag belongs to"`
	FlagID            string  `json:"flag_id" jsonschema:"UUID of the flag to update (from koolbase_list_flags)"`
	Enabled           *bool   `json:"enabled,omitempty" jsonschema:"if set, turn the flag on or off; omit to leave unchanged"`
	RolloutPercentage *int    `json:"rollout_percentage,omitempty" jsonschema:"if set, target rollout 0-100; omit to leave unchanged"`
	KillSwitch        *bool   `json:"kill_switch,omitempty" jsonschema:"if set, engage or release the kill switch; omit to leave unchanged"`
	Description       *string `json:"description,omitempty" jsonschema:"if set, replace the description; omit to leave unchanged"`
}

type setFlagOut struct {
	ID                string `json:"id"`
	Key               string `json:"key"`
	Enabled           bool   `json:"enabled"`
	RolloutPercentage int    `json:"rollout_percentage"`
	KillSwitch        bool   `json:"kill_switch"`
	Description       string `json:"description"`
}

// addSetFlag registers the first write tool. It merges the caller's specified
// fields onto the flag's current state before PUTting, so an agent can change
// one field without resetting the others (the API route is a full replace).
//
// Requires a write-scoped key; a read key yields 403 insufficient_scope from
// the API, which this tool rewrites into an actionable message.
func (s *Server) addSetFlag() {
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name: "koolbase_set_flag",
		Description: "Update a Koolbase feature flag's state (enabled, rollout percentage, kill switch, description). " +
			"Only the fields you provide are changed; others keep their current values. " +
			"This changes live app behavior for users on this environment. Requires a write-scoped API key.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in setFlagIn) (*mcp.CallToolResult, setFlagOut, error) {
		// Read current state so omitted fields are preserved (route is a full replace).
		flags, err := s.client.ListFlags(in.EnvironmentID)
		if err != nil {
			return nil, setFlagOut{}, mapScopeErr(err)
		}
		var cur *api.Flag
		for i := range flags {
			if flags[i].ID == in.FlagID {
				cur = &flags[i]
				break
			}
		}
		if cur == nil {
			return nil, setFlagOut{}, fmt.Errorf("flag %s not found in environment %s", in.FlagID, in.EnvironmentID)
		}

		// Merge: start from current, overlay only what the caller specified.
		req := api.UpdateFlagRequest{
			Enabled:           cur.Enabled,
			RolloutPercentage: cur.RolloutPercentage,
			KillSwitch:        cur.KillSwitch,
			Description:       cur.Description,
		}
		if in.Enabled != nil {
			req.Enabled = *in.Enabled
		}
		if in.RolloutPercentage != nil {
			req.RolloutPercentage = *in.RolloutPercentage
		}
		if in.KillSwitch != nil {
			req.KillSwitch = *in.KillSwitch
		}
		if in.Description != nil {
			req.Description = *in.Description
		}

		updated, err := s.client.UpdateFlag(in.FlagID, req)
		if err != nil {
			return nil, setFlagOut{}, mapScopeErr(err)
		}
		return nil, setFlagOut{
			ID: updated.ID, Key: updated.Key, Enabled: updated.Enabled,
			RolloutPercentage: updated.RolloutPercentage,
			KillSwitch:        updated.KillSwitch, Description: updated.Description,
		}, nil
	})
}

// mapScopeErr rewrites the API's 403 insufficient_scope into an actionable
// message naming the fix, and passes other errors through unchanged.
func mapScopeErr(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	if strings.Contains(msg, "insufficient_scope") || strings.Contains(msg, "(403)") {
		required := "write"
		if strings.Contains(msg, "requires admin") {
			required = "admin"
		}
		return fmt.Errorf("this API key's scope is insufficient for this operation (requires %s). "+
			"Mint a key with scope %s (koolbase keys create --scope %s, or the dashboard's API Keys tab) "+
			"and set KOOLBASE_API_KEY to it", required, required, required)
	}
	return err
}

// decodeJSON turns a json.RawMessage into a plain any (string, float64, bool,
// map, or slice) so it serializes as its real JSON value rather than raw
// bytes. On malformed input it returns nil rather than failing the tool.
func decodeJSON(raw []byte) any {
	if len(raw) == 0 {
		return nil
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil
	}
	return v
}

// --- list_configs -----------------------------------------------------------

type listConfigsIn struct {
	EnvironmentID string `json:"environment_id" jsonschema:"UUID of the environment whose remote configs to list"`
}

type configSummary struct {
	ID          string `json:"id"`
	Key         string `json:"key" jsonschema:"the config's programmatic key"`
	Value       any    `json:"value" jsonschema:"current value (string, number, boolean, or object per value_type)"`
	ValueType   string `json:"value_type" jsonschema:"string, number, boolean, or json"`
	Description string `json:"description"`
}

type listConfigsOut struct {
	Configs []configSummary `json:"configs"`
}

func (s *Server) addListConfigs() {
	// The Value field is `any`; the SDK infers it as the boolean `true`
	// schema, which Claude Desktop's validator rejects (dropping ALL tools).
	// Build the schema ourselves and force an object-form accept-anything
	// schema for `value`.
	outSchema, serr := jsonschema.For[listConfigsOut](nil)
	if serr == nil && outSchema.Properties["configs"] != nil && outSchema.Properties["configs"].Items != nil {
		outSchema.Properties["configs"].Items.Properties["value"] = &jsonschema.Schema{
			Description: "current value (string, number, boolean, or object per value_type)",
		}
	}
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:         "koolbase_list_configs",
		Description:  "List the remote-config entries of a Koolbase environment, with their current values and types. Remote config lets you change app behavior/content without shipping an app update.",
		OutputSchema: outSchema,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in listConfigsIn) (*mcp.CallToolResult, listConfigsOut, error) {
		configs, err := s.client.ListConfigs(in.EnvironmentID)
		if err != nil {
			return nil, listConfigsOut{}, mapScopeErr(err)
		}
		out := listConfigsOut{Configs: make([]configSummary, 0, len(configs))}
		for _, c := range configs {
			out.Configs = append(out.Configs, configSummary{
				ID: c.ID, Key: c.Key, Value: decodeJSON(c.Value),
				ValueType: c.ValueType, Description: c.Description,
			})
		}
		return nil, out, nil
	})
}

// --- set_config -------------------------------------------------------------

type setConfigIn struct {
	EnvironmentID string  `json:"environment_id" jsonschema:"UUID of the environment the config belongs to"`
	ConfigID      string  `json:"config_id" jsonschema:"UUID of the config to update (from koolbase_list_configs)"`
	Value         *string `json:"value,omitempty" jsonschema:"if set, the new value as a JSON literal matching the config's type: a quoted string like \"hello\", a number like 42, a boolean true/false, or a JSON object/array. Omit to leave unchanged."`
	Description   *string `json:"description,omitempty" jsonschema:"if set, replace the description; omit to leave unchanged"`
}

type setConfigOut struct {
	ID          string `json:"id"`
	Key         string `json:"key"`
	Value       any    `json:"value"`
	ValueType   string `json:"value_type"`
	Description string `json:"description"`
}

func (s *Server) addSetConfig() {
	// Same `any`-typed Value as list_configs: force an object-form schema
	// so Claude Desktop's validator accepts the manifest.
	outSchema, serr := jsonschema.For[setConfigOut](nil)
	if serr == nil && outSchema.Properties != nil {
		outSchema.Properties["value"] = &jsonschema.Schema{
			Description: "the config's value after the update",
		}
	}
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:         "koolbase_set_config",
		OutputSchema: outSchema,
		Description: "Update a Koolbase remote-config value or description. Only fields you provide change; others keep current values. " +
			"The value must be a JSON literal matching the config's value_type (string/number/boolean/json) — the server rejects a type mismatch. " +
			"This changes live app behavior/content for users on this environment. Requires a write-scoped API key.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in setConfigIn) (*mcp.CallToolResult, setConfigOut, error) {
		configs, err := s.client.ListConfigs(in.EnvironmentID)
		if err != nil {
			return nil, setConfigOut{}, mapScopeErr(err)
		}
		var cur *api.Config
		for i := range configs {
			if configs[i].ID == in.ConfigID {
				cur = &configs[i]
				break
			}
		}
		if cur == nil {
			return nil, setConfigOut{}, fmt.Errorf("config %s not found in environment %s", in.ConfigID, in.EnvironmentID)
		}

		req := api.UpdateConfigRequest{
			Value:       cur.Value,
			Description: cur.Description,
		}
		if in.Value != nil {
			// Validate the caller's value is well-formed JSON before sending;
			// the server additionally checks it against the config's type.
			raw := json.RawMessage(*in.Value)
			if !json.Valid(raw) {
				return nil, setConfigOut{}, fmt.Errorf("value is not valid JSON: %q — use a JSON literal like \"text\", 42, true, or {\"k\":\"v\"}", *in.Value)
			}
			req.Value = raw
		}
		if in.Description != nil {
			req.Description = *in.Description
		}

		updated, err := s.client.UpdateConfig(in.ConfigID, req)
		if err != nil {
			return nil, setConfigOut{}, mapScopeErr(err)
		}
		return nil, setConfigOut{
			ID: updated.ID, Key: updated.Key, Value: decodeJSON(updated.Value),
			ValueType: updated.ValueType, Description: updated.Description,
		}, nil
	})
}

// --- list_patches -----------------------------------------------------------

type listPatchesIn struct {
	ProjectID string `json:"project_id" jsonschema:"UUID of the project (app) whose code-push patches to list"`
	ReleaseID string `json:"release_id,omitempty" jsonschema:"optional: limit to one release's patches. Omit to list patches across all releases of the app."`
}

type patchSummary struct {
	PatchID           string `json:"patch_id"`
	ReleaseID         string `json:"release_id"`
	BuildID           string `json:"build_id" jsonschema:"the base build these patches apply on top of"`
	Platform          string `json:"platform"`
	AppVersion        string `json:"app_version"`
	PatchNumber       int    `json:"patch_number"`
	Status            string `json:"status" jsonschema:"draft, published, or recalled"`
	RolloutPercentage int    `json:"rollout_percentage"`
	Mandatory         bool   `json:"mandatory"`
	SizeBytes         int    `json:"size_bytes"`
	ReleaseNotes      string `json:"release_notes,omitempty"`
	CreatedAt         string `json:"created_at"`
	PublishedAt       string `json:"published_at,omitempty"`
	RecalledAt        string `json:"recalled_at,omitempty"`
}

type listPatchesOut struct {
	Patches []patchSummary `json:"patches"`
}

func (s *Server) addListPatches() {
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name: "koolbase_list_patches",
		Description: "List code-push (OTA) patches for a Koolbase app, with their status (draft/published/recalled), rollout percentage, and target build. " +
			"Omit release_id to see patches across all releases. Read-only — does not publish or recall anything.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in listPatchesIn) (*mcp.CallToolResult, listPatchesOut, error) {
		// Resolve which releases to scan: one, or all for the app. Always
		// fetch real release records (no GetRelease endpoint exists, so
		// filter the list) — a bare {ID} stub would leave build_id,
		// platform, and app_version empty in the enrichment below.
		rels, err := s.client.ListReleases(in.ProjectID)
		if err != nil {
			return nil, listPatchesOut{}, mapScopeErr(err)
		}
		var releases []api.Release
		if in.ReleaseID != "" {
			for _, r := range rels {
				if r.ID == in.ReleaseID {
					releases = append(releases, r)
					break
				}
			}
			if len(releases) == 0 {
				return nil, listPatchesOut{}, fmt.Errorf("release %s not found in project %s", in.ReleaseID, in.ProjectID)
			}
		} else {
			releases = rels
		}

		// Index release metadata (build_id/platform/version) to enrich patches.
		relMeta := make(map[string]api.Release, len(releases))
		for _, r := range releases {
			relMeta[r.ID] = r
		}

		out := listPatchesOut{Patches: []patchSummary{}}
		for _, r := range releases {
			patches, err := s.client.ListPatches(in.ProjectID, r.ID)
			if err != nil {
				return nil, listPatchesOut{}, mapScopeErr(err)
			}
			meta := relMeta[r.ID]
			for _, p := range patches {
				out.Patches = append(out.Patches, patchSummary{
					PatchID: p.ID, ReleaseID: p.ReleaseID,
					BuildID: meta.BuildID, Platform: meta.Platform, AppVersion: meta.AppVersion,
					PatchNumber: p.PatchNumber, Status: p.Status,
					RolloutPercentage: p.RolloutPercentage, Mandatory: p.Mandatory,
					SizeBytes: p.SizeBytes, ReleaseNotes: p.ReleaseNotes,
					CreatedAt: p.CreatedAt, PublishedAt: p.PublishedAt, RecalledAt: p.RecalledAt,
				})
			}
		}
		return nil, out, nil
	})
}

// --- publish_patch / recall_patch (gated: --enable-codepush-mutations) ------

type publishPatchIn struct {
	ProjectID string `json:"project_id" jsonschema:"UUID of the project (app) the patch belongs to"`
	PatchID   string `json:"patch_id" jsonschema:"ID of the DRAFT patch to publish (from koolbase_list_patches)"`
}

type patchActionOut struct {
	PatchID           string `json:"patch_id"`
	Status            string `json:"status"`
	RolloutPercentage int    `json:"rollout_percentage" jsonschema:"the blast radius: percentage of devices that will receive this patch"`
	Mandatory         bool   `json:"mandatory"`
}

// patchState re-reads a patch after a mutation so the tool reports verified
// state, not the mutation's own claim.
func (s *Server) patchState(projectID, patchID string) (patchActionOut, error) {
	// One bounded retry: the re-read races the mutation's commit path, and a
	// transient 404/miss milliseconds after a successful mutation is noise,
	// not truth (observed live on the first recall proof).
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(attempt) * 500 * time.Millisecond)
		}
		patches, err := s.client.ListPatches(projectID, "")
		if err != nil {
			lastErr = err
			continue
		}
		for _, p := range patches {
			if p.ID == patchID {
				return patchActionOut{
					PatchID: p.ID, Status: p.Status,
					RolloutPercentage: p.RolloutPercentage, Mandatory: p.Mandatory,
				}, nil
			}
		}
		lastErr = fmt.Errorf("patch %s not in listing", patchID)
	}
	return patchActionOut{}, fmt.Errorf("state re-read failed after retries: %w", lastErr)
}

func (s *Server) addPublishPatch() {
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name: "koolbase_publish_patch",
		Description: "PUBLISH a draft code-push patch, making it LIVE: real devices download and " +
			"apply it on their next check. The patch ships at the rollout percentage fixed when it " +
			"was created (returned in the result — inspect it via koolbase_list_patches BEFORE " +
			"publishing and confirm the blast radius with the user). Requires an admin-scoped key. " +
			"If anything looks wrong after publishing, koolbase_recall_patch is the emergency brake.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in publishPatchIn) (*mcp.CallToolResult, patchActionOut, error) {
		if err := s.client.PublishPatch(in.ProjectID, in.PatchID); err != nil {
			return nil, patchActionOut{}, mapScopeErr(err)
		}
		out, err := s.patchState(in.ProjectID, in.PatchID)
		if err != nil {
			return nil, patchActionOut{}, fmt.Errorf("published, but state re-read failed: %w", err)
		}
		return nil, out, nil
	})
}

type recallPatchIn struct {
	ProjectID string `json:"project_id" jsonschema:"UUID of the project (app) the patch belongs to"`
	PatchID   string `json:"patch_id" jsonschema:"ID of the published patch to recall"`
}

func (s *Server) addRecallPatch() {
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name: "koolbase_recall_patch",
		Description: "RECALL a published code-push patch: devices revert to the prior patch on " +
			"their next cold launch. This is the emergency brake — use it immediately when a live " +
			"patch is misbehaving. Requires an admin-scoped key.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in recallPatchIn) (*mcp.CallToolResult, patchActionOut, error) {
		if err := s.client.RecallPatch(in.ProjectID, in.PatchID); err != nil {
			return nil, patchActionOut{}, mapScopeErr(err)
		}
		out, err := s.patchState(in.ProjectID, in.PatchID)
		if err != nil {
			return nil, patchActionOut{}, fmt.Errorf("recalled, but state re-read failed: %w", err)
		}
		return nil, out, nil
	})
}

// --- describe_project -------------------------------------------------------
//
// The schema layer: the tool that lets an agent generate code against a real
// project instead of guessing at it. Everything here is a PROJECTION of the
// snapshot artifact, never a passthrough — see projectSnapshot below.

// accessRule is a collection's rule for one verb, as DATA rather than prose.
//
// An agent handed {"kind":"scoped","owner_field":"user_id","caller_key":"id"}
// cannot justify generating an unscoped write. An agent handed "remember to
// scope writes" absolutely will. Every rule kind the platform has gets a
// representation here — see ruleFor, which fails loudly on an unknown kind
// rather than emitting a bare one an agent cannot act on.
type accessRule struct {
	Kind string `json:"kind" jsonschema:"one of: public, authenticated, owner, scoped, conditional, server_only"`

	// OwnerField and CallerKey are the two halves of an ownership check, set
	// for kind=owner and kind=scoped. The record's OwnerField must equal the
	// signed-in caller's CallerKey.
	//
	// For kind=owner these are always created_by/id: the server stamps
	// created_by on insert and the client neither sets nor can override it.
	// For kind=scoped they come from the collection's owner_field spec, which
	// is a BINDING, not a field name: "user_id=$caller.id" binds the record's
	// user_id to the caller's id, and bare "user_id" is the legacy same-name
	// form binding record.user_id to context user_id.
	OwnerField string `json:"owner_field,omitempty" jsonschema:"the RECORD field carrying ownership — filter reads on it, populate writes with it (except kind=owner, where the server stamps it)"`
	CallerKey  string `json:"caller_key,omitempty" jsonschema:"the signed-in caller's property that owner_field must equal, e.g. id"`

	// Mode and Conditions are set only for kind=conditional.
	Mode       string          `json:"mode,omitempty" jsonschema:"for kind=conditional: how conditions combine"`
	Conditions json.RawMessage `json:"conditions,omitempty" jsonschema:"for kind=conditional: the rule conditions, as declared"`
}

// uniqueConstraintOut is one declared uniqueness rule. Generated writes MUST
// NOT create a second record sharing these field values.
type uniqueConstraintOut struct {
	Fields          []string `json:"fields" jsonschema:"the field combination that must be unique across the collection"`
	CaseInsensitive bool     `json:"case_insensitive"`
}

// collectionOut is one collection's name and governance.
//
// Deliberately carries NO general field list. Koolbase collections are
// schemaless: db_collections stores a name and rules, nothing more. Emitting a
// typed field list would mean inventing one, and a hallucinated schema is
// worse than an absent one — the developer's first experience would be
// debugging. The ONLY declared field names the platform holds are the ones
// inside unique constraints, which are therefore included.
type collectionOut struct {
	Name       string     `json:"name"`
	Read       accessRule `json:"read"`
	Write      accessRule `json:"write"`
	Delete     accessRule `json:"delete"`
	AppendOnly bool       `json:"append_only" jsonschema:"when true, existing records cannot be updated or deleted"`

	// UniqueConstraints are the collection's declared uniqueness rules — the
	// only field names the platform holds for a schemaless collection, and
	// therefore the only ones an agent may treat as known to exist.
	UniqueConstraints []uniqueConstraintOut `json:"unique_constraints"`
}

type bucketOut struct {
	Name              string   `json:"name"`
	Public            bool     `json:"public"`
	AccessMode        string   `json:"access_mode"`
	MaxSizeBytes      *int64   `json:"max_size_bytes,omitempty"`
	MaxFileSizeBytes  *int64   `json:"max_file_size_bytes,omitempty"`
	AllowedMimeTypes  []string `json:"allowed_mime_types,omitempty"`
	VersioningEnabled bool     `json:"versioning_enabled"`
}

// functionOut is a deployed function's SIGNATURE. Never its body.
//
// The snapshot embeds function source so a clone can redeploy without the
// source repository. An agent needs to know `settle` exists and requires auth;
// it has no business receiving the implementation. Source and pubspec are
// dropped by omission from this type — see projectSnapshot.
type functionOut struct {
	Name         string `json:"name"`
	Runtime      string `json:"runtime"`
	TimeoutMs    int    `json:"timeout_ms"`
	RequiresAuth bool   `json:"requires_auth"`
	Enabled      bool   `json:"enabled"`
}

type environmentOut struct {
	Name string `json:"name"`
	Slug string `json:"slug"`
}

type describeProjectOut struct {
	ProjectID    string           `json:"project_id"`
	Collections  []collectionOut  `json:"collections"`
	Environments []environmentOut `json:"environments"`
	Buckets      []bucketOut      `json:"buckets"`
	Functions    []functionOut    `json:"functions"`
}

// snapshotShape is the subset of the server's snapshot artifact this tool
// reads. It is deliberately NOT the server's Snapshot type: naming only the
// fields we consume means a new server-side section (secrets, crons, whatever
// ships next) is invisible here until someone chooses to expose it. Silence is
// the safe default for a surface an agent reads.
type snapshotShape struct {
	SourceProjectID string `json:"source_project_id"`
	Collections     []struct {
		Name           string          `json:"name"`
		ReadRule       string          `json:"read_rule"`
		WriteRule      string          `json:"write_rule"`
		DeleteRule     string          `json:"delete_rule"`
		OwnerField     *string         `json:"owner_field"`
		RuleMode       string          `json:"rule_mode"`
		RuleConditions json.RawMessage `json:"rule_conditions"`
		AppendOnly     bool            `json:"append_only"`
	} `json:"collections"`
	Environments []struct {
		Name string `json:"name"`
		Slug string `json:"slug"`
	} `json:"environments"`
	Buckets []struct {
		Name              string   `json:"name"`
		Public            bool     `json:"public"`
		AccessMode        string   `json:"access_mode"`
		MaxSizeBytes      *int64   `json:"max_size_bytes"`
		MaxFileSizeBytes  *int64   `json:"max_file_size_bytes"`
		AllowedMimeTypes  []string `json:"allowed_mime_types"`
		VersioningEnabled bool     `json:"versioning_enabled"`
	} `json:"buckets"`
	Functions []struct {
		Name         string `json:"name"`
		Runtime      string `json:"runtime"`
		TimeoutMs    int    `json:"timeout_ms"`
		RequiresAuth bool   `json:"requires_auth"`
		Enabled      bool   `json:"enabled"`
	} `json:"functions"`
}

// parseOwnerFieldSpec splits an owner_field spec into the record field and the
// caller-context key it binds against. This mirrors the server's own
// parseOwnerField (internal/database/service.go) deliberately and exactly: a
// second dialect here would mean the agent and the enforcement path disagree
// about what a rule says.
func parseOwnerFieldSpec(spec string) (recordField, callerKey string) {
	const marker = "=$caller."
	if i := strings.Index(spec, marker); i >= 0 {
		return spec[:i], spec[i+len(marker):]
	}
	return spec, spec
}

// ruleFor shapes one rule string plus its satellite columns into structured
// data, so each kind carries exactly the fields that are meaningful for it.
//
// An unknown kind returns an error rather than a bare {"kind":"..."}: a kind
// this function does not understand is one whose ownership semantics the agent
// cannot act on, and emitting it silently is how `owner` reached production
// output unmapped. Fail loudly instead.
func ruleFor(kind string, ownerField *string, mode string, conditions json.RawMessage) (accessRule, error) {
	r := accessRule{Kind: kind}
	switch kind {
	case "public", "authenticated", "server_only":
		// No satellites: these are decided by authentication state alone.
	case "owner":
		// Ownership is the record's server-stamped created_by, compared to the
		// caller's id. There is no per-collection owner_field for this kind.
		r.OwnerField = "created_by"
		r.CallerKey = "id"
	case "scoped":
		if ownerField == nil || *ownerField == "" {
			// The server fails closed on this (denyAllFilter); say so rather
			// than describing a rule that admits nothing as if it admits
			// something.
			return accessRule{}, fmt.Errorf("collection has rule kind %q but no owner_field; the server denies all access to it", kind)
		}
		r.OwnerField, r.CallerKey = parseOwnerFieldSpec(*ownerField)
	case "conditional":
		r.Mode = mode
		if len(conditions) > 0 && string(conditions) != "null" {
			r.Conditions = conditions
		}
	default:
		return accessRule{}, fmt.Errorf("unknown access rule kind %q — the platform's rule vocabulary has grown and koolbase_describe_project has not been updated", kind)
	}
	return r, nil
}

// fetchConstraints returns a collection's declared unique constraints; nil
// means "do not fetch" (tests, or a future flag). Failures are returned as
// errors rather than silently emitting a description with constraints absent —
// absent-but-existing constraints would have an agent generate duplicate
// writes believing them legal.
type fetchConstraints func(collection string) ([]api.UniqueConstraint, error)

// projectSnapshot converts a raw snapshot payload into the agent-facing
// description, enriching each collection with its unique constraints via the
// supplied fetch function.
//
// This is an ALLOW-LIST projection, and that is the whole safety property: the
// output is built field by named field. The snapshot carries function source
// and secret names; neither has a route into the result, and neither does
// whatever the server adds next. Passing the payload through — or projecting
// by deletion rather than construction — would make every future server field
// an agent-visible one by default.
func projectSnapshot(raw []byte, fetch fetchConstraints) (describeProjectOut, error) {
	var snap snapshotShape
	if err := json.Unmarshal(raw, &snap); err != nil {
		return describeProjectOut{}, fmt.Errorf("failed to parse project snapshot: %w", err)
	}

	out := describeProjectOut{
		ProjectID:    snap.SourceProjectID,
		Collections:  []collectionOut{},
		Environments: []environmentOut{},
		Buckets:      []bucketOut{},
		Functions:    []functionOut{},
	}

	for _, c := range snap.Collections {
		read, err := ruleFor(c.ReadRule, c.OwnerField, c.RuleMode, c.RuleConditions)
		if err != nil {
			return describeProjectOut{}, fmt.Errorf("collection %q read rule: %w", c.Name, err)
		}
		write, err := ruleFor(c.WriteRule, c.OwnerField, c.RuleMode, c.RuleConditions)
		if err != nil {
			return describeProjectOut{}, fmt.Errorf("collection %q write rule: %w", c.Name, err)
		}
		del, err := ruleFor(c.DeleteRule, c.OwnerField, c.RuleMode, c.RuleConditions)
		if err != nil {
			return describeProjectOut{}, fmt.Errorf("collection %q delete rule: %w", c.Name, err)
		}

		constraints := []uniqueConstraintOut{}
		if fetch != nil {
			ucs, err := fetch(c.Name)
			if err != nil {
				return describeProjectOut{}, fmt.Errorf("collection %q unique constraints: %w", c.Name, err)
			}
			for _, uc := range ucs {
				constraints = append(constraints, uniqueConstraintOut{
					Fields: uc.Fields, CaseInsensitive: uc.CaseInsensitive,
				})
			}
		}

		out.Collections = append(out.Collections, collectionOut{
			Name:              c.Name,
			Read:              read,
			Write:             write,
			Delete:            del,
			AppendOnly:        c.AppendOnly,
			UniqueConstraints: constraints,
		})
	}
	for _, e := range snap.Environments {
		out.Environments = append(out.Environments, environmentOut{Name: e.Name, Slug: e.Slug})
	}
	for _, b := range snap.Buckets {
		out.Buckets = append(out.Buckets, bucketOut{
			Name: b.Name, Public: b.Public, AccessMode: b.AccessMode,
			MaxSizeBytes: b.MaxSizeBytes, MaxFileSizeBytes: b.MaxFileSizeBytes,
			AllowedMimeTypes: b.AllowedMimeTypes, VersioningEnabled: b.VersioningEnabled,
		})
	}
	for _, f := range snap.Functions {
		out.Functions = append(out.Functions, functionOut{
			Name: f.Name, Runtime: f.Runtime, TimeoutMs: f.TimeoutMs,
			RequiresAuth: f.RequiresAuth, Enabled: f.Enabled,
		})
	}
	return out, nil
}

// --- get_collection (drill-down) --------------------------------------------

type getCollectionIn struct {
	ProjectID  string `json:"project_id" jsonschema:"UUID of the project the collection belongs to"`
	Collection string `json:"collection" jsonschema:"the collection's name, as returned by koolbase_describe_project"`
}

type getCollectionOut struct {
	ProjectID  string        `json:"project_id"`
	Collection collectionOut `json:"collection"`
}

// addGetCollection registers the per-collection drill-down, so work on one
// screen does not drag the whole project into the agent's context. Same
// projection as describe_project — the two tools cannot disagree about what a
// collection looks like because they share one.
func (s *Server) addGetCollection() {
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name: "koolbase_get_collection",
		Description: strings.TrimSpace(`
Fetch ONE collection's access rules and unique constraints, by name. Use this
instead of koolbase_describe_project when working on a single screen or
feature, so the full project description does not consume context.

The result has the same shape and semantics as one entry of
koolbase_describe_project's collections array — see that tool's description
for the rule-kind vocabulary and the schemaless model.`),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in getCollectionIn) (*mcp.CallToolResult, getCollectionOut, error) {
		raw, err := s.client.SnapshotPull(in.ProjectID)
		if err != nil {
			return nil, getCollectionOut{}, mapScopeErr(err)
		}
		out, err := projectSnapshot(raw, func(collection string) ([]api.UniqueConstraint, error) {
			// Only the requested collection's constraints are worth a fetch.
			if collection != in.Collection {
				return nil, nil
			}
			return s.client.ListUniqueConstraints(in.ProjectID, collection)
		})
		if err != nil {
			return nil, getCollectionOut{}, mapScopeErr(err)
		}
		names := make([]string, 0, len(out.Collections))
		for _, c := range out.Collections {
			if c.Name == in.Collection {
				return nil, getCollectionOut{ProjectID: out.ProjectID, Collection: c}, nil
			}
			names = append(names, c.Name)
		}
		return nil, getCollectionOut{}, fmt.Errorf("collection %q not found in project %s — existing collections: %s",
			in.Collection, in.ProjectID, strings.Join(names, ", "))
	})
}

// describeProjectIn is the tool's input.
type describeProjectIn struct {
	ProjectID string `json:"project_id" jsonschema:"UUID of the project to describe. Resolve one via koolbase_list_projects if unknown."`
}

func (s *Server) addDescribeProject() {
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name: "koolbase_describe_project",
		Description: strings.TrimSpace(`
Describe a Koolbase project's shape so you can write code against it: its
collections with their access rules and unique constraints, storage buckets,
deployed function signatures, and environments. Call this BEFORE generating
any code that reads or writes Koolbase data.

Access rules are returned as structured data, one per verb (read/write/delete).
Rule kinds:
  public         — anyone, signed in or not
  authenticated  — any signed-in user
  owner          — only the user who created the record. The server stamps
                   created_by on insert; client code neither sets nor can
                   override it. Generated reads should expect other users'
                   records to be invisible.
  scoped         — signed-in users, restricted to records they own, via an
                   explicit binding. The rule carries owner_field (the RECORD
                   field) and caller_key (the caller property it must equal):
                   owner_field="user_id", caller_key="id" means reads MUST
                   filter where user_id equals the signed-in user's id, and
                   writes MUST populate user_id with it.
  conditional    — allowed when the rule's conditions hold; the rule carries
                   mode and conditions as declared.
  server_only    — only functions and server-side code. Client SDK calls are
                   refused; do not generate client code for this verb.
A collection with append_only=true admits new records but no updates or deletes.

Koolbase collections are SCHEMALESS: a collection is a governed container, and
records carry whatever fields the application writes. This tool therefore
returns no general field list or field types, because the platform holds none —
do not infer a fixed schema, and do not invent field names. The only declared
field names are those in unique_constraints: those fields exist by declaration,
and a write MUST NOT duplicate an existing record's values for any constraint's
field combination. Every record additionally carries $id, $createdAt,
$updatedAt and $revision.

Function bodies and secrets are deliberately never returned.`),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in describeProjectIn) (*mcp.CallToolResult, describeProjectOut, error) {
		raw, err := s.client.SnapshotPull(in.ProjectID)
		if err != nil {
			return nil, describeProjectOut{}, mapScopeErr(err)
		}
		out, err := projectSnapshot(raw, func(collection string) ([]api.UniqueConstraint, error) {
			return s.client.ListUniqueConstraints(in.ProjectID, collection)
		})
		if err != nil {
			return nil, describeProjectOut{}, mapScopeErr(err)
		}
		return nil, out, nil
	})
}

// --- sdk_conventions (the idioms, as a callable) -----------------------------
//
// describe_project tells an agent WHAT exists; this tells it HOW to talk to
// it. Authored from the SDK sources directly (hatchway_flutter and
// koolbase-react-native), not from docs prose — the code is the ground truth
// and the docs are downstream of it. Semantic claims (stale-while-revalidate,
// the offline contract, the signed-out throw) are shared between platforms in
// sdkSharedConventions so the two dialects cannot drift apart on meaning.

const sdkSharedConventions = `
# Koolbase SDK — semantics shared by every platform

INITIALIZATION. Koolbase is a singleton. Initialize once at app startup with
the project's config, then access subsystems as properties: Koolbase.auth,
Koolbase.db, Koolbase.storage, Koolbase.realtime, Koolbase.functions. Never
construct or inject a client object; there is no client to pass around.

AUTH STATE. Koolbase.auth.currentUser is a SYNCHRONOUS property returning the
signed-in user or null — no await needed to read it. The user's id is the .id
property. Subscribe to auth changes for reactive UI. Call restoreSession() at
startup to resume a persisted session before deciding the user is signed out.

QUERIES ARE STALE-WHILE-REVALIDATE. A query returns the cached result
immediately when one exists, then refreshes from the network in the
background. Design UI for data that can arrive twice: render the first
result, update when the refresh lands. Do not treat the first result as
final.

WRITES ARE OFFLINE-AWARE. Inserts made offline are queued per-user and sync
when connectivity returns. Records carry server-assigned system fields: $id,
$createdAt, $updatedAt, $revision (id/createdAt/updatedAt/revision on the
model objects). Never invent or overwrite these.

THE OFFLINE CONTRACT — verified behavior, do not soften it in generated code:
- pendingWrites() and conflicts() THROW an unauthenticated error when no user
  is signed in. They do not return an empty list. Any code path that calls
  them while possibly signed out must handle the throw.
- Conflicts do not expire. Something in the UI must surface them
  (watch/subscribe to conflicts) or they sit unresolved forever.
- Conflict resolution is explicit: resolve with the local version, the server
  version, a merge, or discard. Resolvers require a signed-in user.
- After connectivity returns, syncPendingWrites() drains the queue; observe
  pendingWrites before/after if the UI needs a sync indicator.

ACCESS RULES ARE SERVER-ENFORCED. The rules from koolbase_describe_project are
enforced server-side; client code cannot bypass them. Generated code should
COOPERATE with them: filter scoped reads on the rule's owner_field equal to
the caller's caller_key value, populate scoped writes with it, and never
generate client calls against a server_only verb. A 401 clears the stored
session — treat it as "signed out", not as a retryable error.
`

const sdkFlutterConventions = `
# Flutter dialect — package koolbase_flutter (import 'package:koolbase_flutter/koolbase_flutter.dart')

Initialize:
    await Koolbase.initialize(KoolbaseConfig(...));

Auth:
    final user = Koolbase.auth.currentUser;      // KoolbaseUser?, sync
    final id = user?.id;                          // String
    Koolbase.auth.authStateChanges                // Stream<KoolbaseUser?>
    await Koolbase.auth.restoreSession();
    await Koolbase.auth.login(...); / signUp(...); / logout();

Queries — a BUILDER CHAIN off collection():
    final result = await Koolbase.db
        .collection('expenses')
        .where('user_id', isEqualTo: user.id)     // named param isEqualTo
        .orderBy('created_at', descending: true)
        .limit(20)
        .get();                                    // Future<QueryResult>
    final records = result.records;                // List<KoolbaseRecord>
    // .stream on the same query emits refreshed results (SWR second arrival).

Records — system fields are typed properties, custom fields live in .data:
    record.id / record.createdAt / record.updatedAt / record.createdBy
    record.data['your_field']                      // Map<String, dynamic>
    record.revision                                // int?, pass back on update/
                                                   // delete for a conditional
                                                   // write (fails if changed)

Writes:
    await Koolbase.db.insert(collection: 'expenses', data: {...});
    await Koolbase.db.upsert(...);
    await Koolbase.db.deleteWhere(...);
    await Koolbase.db.batch([...]);

Offline surface:
    await Koolbase.db.pendingWrites();            // THROWS signed out
    Koolbase.db.watchPendingWrites();             // Stream<List<PendingWrite>>
    await Koolbase.db.conflicts();                // THROWS signed out
    Koolbase.db.watchConflicts();                 // Stream<List<KoolbaseConflict>>
    await Koolbase.db.syncPendingWrites();
    // Resolve on the conflict object: resolveWithLocal / resolveWithMerge / ...

Flags and config:
    Koolbase.isEnabled('flag_key');               // bool, sync
    Koolbase.configString('key', fallback: '');

BEHAVIORAL WIDGETS — prefer these over hand-writing the same behavior.
The SDK ships components that encode the semantics above correctly; using
them means three correct lines instead of thirty plausible ones:

    KoolbaseAuthGate(                              // auth branching, done right
      signedIn: (context, user) => HomeScreen(),
      signedOut: (context) => LoginScreen(),
      // restoring: optional slot; defaults to a spinner. The gate calls
      // restoreSession() once at mount, so returning users never see a
      // login flash. RestoreResult.offline counts as signed IN.
    )
    // Descendants read auth state without statics, rebuilding on change:
    KoolbaseAuthScope.of(context).user             // KoolbaseUser?
    KoolbaseAuthScope.of(context).restoredOffline  // offline-restore banner

    KoolbaseCollectionList(                        // SWR list, done right
      collection: 'expenses',
      query: (q) => q.where('user_id', isEqualTo: user.id)
                     .orderBy('created_at', descending: true),
      itemBuilder: (context, record) => ExpenseTile(record),
      // Optional slots — exact signatures:
      //   empty:   (BuildContext context) => Widget
      //   loading: (BuildContext context) => Widget
      //   error:   (BuildContext context, Object error,
      //             Future<void> Function() retry) => Widget
      // Owns ListView + pull-to-refresh; handles both SWR arrivals and
      // keeps stale records over a failed refresh (error slot only shows
      // for a FIRST load with nothing to display).
    )
    // The query callback runs for EVERY fetch with a FRESH query and must
    // be deterministic — never retain or reuse a query instance
    // (KoolbaseQuery mutates, and stream identity derives from filters).
    // For custom scroll layouts, drive KoolbaseCollectionController
    // directly; it owns the fetch/stream lifecycle widget-free.
`

const sdkReactNativeConventions = `
# React Native dialect — package @koolbase/react-native (import { Koolbase } from '@koolbase/react-native')

Initialize:
    await Koolbase.initialize({ ... });

Auth:
    const user = Koolbase.auth.currentUser;       // KoolbaseUser | null, sync
    const id = user?.id;                          // string
    const unsubscribe = Koolbase.auth.onAuthStateChange(listener);
    await Koolbase.auth.restoreSession();
    await Koolbase.auth.login({...}); / register({...}); / logout();

Queries — METHODS WITH AN OPTIONS OBJECT, NOT a builder chain. The Flutter
SDK's chained query builder does not exist in this SDK; do not generate one:
    const result = await Koolbase.db.query('expenses', {
      filters: { user_id: user.id },
      orderBy: 'created_at',
      limit: 20,
    });
    // Same stale-while-revalidate semantics as Flutter: cached first,
    // background refresh after.

Writes:
    await Koolbase.db.insert('expenses', { ... });
    await Koolbase.db.update('expenses', recordId, { ... });
    await Koolbase.db.upsert('expenses', match, { ... });
    await Koolbase.db.deleteWhere('expenses', filters);
    await Koolbase.db.batch(operations);

Offline surface:
    await Koolbase.db.pendingWrites();            // THROWS signed out
    await Koolbase.db.conflicts();                // THROWS signed out
    await Koolbase.db.syncPendingWrites();
`

type sdkConventionsIn struct {
	Platform string `json:"platform" jsonschema:"which client SDK the code targets: flutter or react_native"`
}

type sdkConventionsOut struct {
	Platform    string `json:"platform"`
	Conventions string `json:"conventions" jsonschema:"the SDK idioms to follow when generating code for this platform — read fully before writing any Koolbase client code"`
}

// addSdkConventions registers the idioms tool: the HOW that pairs with
// describe_project's WHAT. Returned as content the agent reads before writing
// code, per platform, so a Flutter chain never appears in TypeScript and an
// options object never appears in Dart.
func (s *Server) addSdkConventions() {
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name: "koolbase_sdk_conventions",
		Description: strings.TrimSpace(`
Return the Koolbase client SDK's idioms for one platform: initialization, auth
access, the query pattern (and its stale-while-revalidate semantics), the
offline-aware write path, and the conflict surface. Call this BEFORE writing
any code that uses the Koolbase SDK, and follow it exactly — the two platforms
share semantics but have DIFFERENT call shapes (Flutter chains queries;
React Native passes an options object). Pair with koolbase_describe_project:
that tool says what exists and who may touch it, this one says how to call it.`),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in sdkConventionsIn) (*mcp.CallToolResult, sdkConventionsOut, error) {
		content, err := sdkConventionsFor(in.Platform)
		if err != nil {
			return nil, sdkConventionsOut{}, err
		}
		return nil, sdkConventionsOut{Platform: in.Platform, Conventions: content}, nil
	})
}

// sdkConventionsFor assembles the conventions content for one platform:
// shared semantics first (the claims both dialects must agree on), then the
// platform's own call shapes. Unknown platforms fail naming the valid ones so
// a guessing agent self-corrects in one turn.
func sdkConventionsFor(platform string) (string, error) {
	var dialect string
	switch platform {
	case "flutter":
		dialect = sdkFlutterConventions
	case "react_native":
		dialect = sdkReactNativeConventions
	default:
		return "", fmt.Errorf("unknown platform %q — valid platforms: flutter, react_native", platform)
	}
	return strings.TrimSpace(sdkSharedConventions) + "\n\n" + strings.TrimSpace(dialect), nil
}
