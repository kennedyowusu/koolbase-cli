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
// An agent handed {"kind":"scoped","owner_field":"created_by"} cannot justify
// generating an unscoped write. An agent handed "remember to scope writes"
// absolutely will. Every rule kind the platform has gets a representation here.
type accessRule struct {
	Kind string `json:"kind" jsonschema:"one of: public, authenticated, server_only, scoped, conditional"`
	// OwnerField is set only for kind=scoped: the field holding the owning
	// user's id. Generated writes MUST populate it and generated reads MUST
	// filter on it.
	OwnerField *string `json:"owner_field,omitempty" jsonschema:"for kind=scoped: the field carrying the owning user id"`
	// Mode and Conditions are set only for kind=conditional.
	Mode       string          `json:"mode,omitempty" jsonschema:"for kind=conditional: how conditions combine"`
	Conditions json.RawMessage `json:"conditions,omitempty" jsonschema:"for kind=conditional: the rule conditions, as declared"`
}

// collectionOut is one collection's name and governance.
//
// Deliberately carries NO field list. Koolbase collections are schemaless:
// db_collections stores a name and rules, nothing more. Emitting a typed field
// list would mean inventing one, and a hallucinated schema is worse than an
// absent one — the developer's first experience would be debugging.
type collectionOut struct {
	Name       string     `json:"name"`
	Read       accessRule `json:"read"`
	Write      accessRule `json:"write"`
	Delete     accessRule `json:"delete"`
	AppendOnly bool       `json:"append_only" jsonschema:"when true, existing records cannot be updated or deleted"`
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

// ruleFor shapes one rule string plus its satellite columns into structured
// data. owner_field rides only with `scoped`, and mode/conditions only with
// `conditional`, so the output never suggests a field is meaningful where the
// platform ignores it.
func ruleFor(kind string, ownerField *string, mode string, conditions json.RawMessage) accessRule {
	r := accessRule{Kind: kind}
	switch kind {
	case "scoped":
		r.OwnerField = ownerField
	case "conditional":
		r.Mode = mode
		if len(conditions) > 0 && string(conditions) != "null" {
			r.Conditions = conditions
		}
	}
	return r
}

// projectSnapshot converts a raw snapshot payload into the agent-facing
// description.
//
// This is an ALLOW-LIST projection, and that is the whole safety property: the
// output is built field by named field. The snapshot carries function source
// and secret names; neither has a route into the result, and neither does
// whatever the server adds next. Passing the payload through — or projecting
// by deletion rather than construction — would make every future server field
// an agent-visible one by default.
func projectSnapshot(raw []byte) (describeProjectOut, error) {
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
		out.Collections = append(out.Collections, collectionOut{
			Name:       c.Name,
			Read:       ruleFor(c.ReadRule, c.OwnerField, c.RuleMode, c.RuleConditions),
			Write:      ruleFor(c.WriteRule, c.OwnerField, c.RuleMode, c.RuleConditions),
			Delete:     ruleFor(c.DeleteRule, c.OwnerField, c.RuleMode, c.RuleConditions),
			AppendOnly: c.AppendOnly,
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

// describeProjectIn is the tool's input.
type describeProjectIn struct {
	ProjectID string `json:"project_id" jsonschema:"UUID of the project to describe. Resolve one via koolbase_list_projects if unknown."`
}

func (s *Server) addDescribeProject() {
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name: "koolbase_describe_project",
		Description: strings.TrimSpace(`
Describe a Koolbase project's shape so you can write code against it: its
collections and their access rules, storage buckets, deployed function
signatures, and environments. Call this BEFORE generating any code that reads
or writes Koolbase data.

Access rules are returned as structured data, one per verb (read/write/delete).
Rule kinds:
  public         — anyone, signed in or not
  authenticated  — any signed-in user
  scoped         — signed-in users, restricted to records they own. The rule
                   carries owner_field: reads MUST filter on it and writes MUST
                   populate it with the signed-in user's id.
  server_only    — only functions and server-side code. Client SDK calls are
                   refused; do not generate client code for this verb.
  conditional    — allowed when the rule's conditions hold; the rule carries
                   mode and conditions as declared.
A collection with append_only=true admits new records but no updates or deletes.

Koolbase collections are SCHEMALESS: a collection is a governed container, and
records carry whatever fields the application writes. This tool therefore
returns no field list or field types, because the platform holds none — do not
infer a fixed schema, and do not invent field names. Every record additionally
carries $id, $createdAt, $updatedAt and $revision.

Function bodies and secrets are deliberately never returned.`),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in describeProjectIn) (*mcp.CallToolResult, describeProjectOut, error) {
		raw, err := s.client.SnapshotPull(in.ProjectID)
		if err != nil {
			return nil, describeProjectOut{}, mapScopeErr(err)
		}
		out, err := projectSnapshot(raw)
		if err != nil {
			return nil, describeProjectOut{}, err
		}
		return nil, out, nil
	})
}
