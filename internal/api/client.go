package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"
)

const defaultBaseURL = "https://api.koolbase.com"

type Client struct {
	baseURL    string
	apiKey     string
	httpClient *http.Client
	// longClient serves function invocations — the only calls whose duration
	// is set by user code rather than our infrastructure. See invokeTimeout.
	longClient *http.Client
}

// invokeTimeout bounds a function invocation. Derived, not arbitrary:
// max function execution (30s, enforced at deploy) + Dart cold start
// (~8s observed) + network headroom. The 30s default on httpClient is
// deliberately unchanged — for control-plane calls, slow means broken
// and failing fast is correct. If max execution changes, change this too.
const invokeTimeout = 60 * time.Second

// BaseURL reports the API this client talks to. Needed by `koolbase create`,
// which writes it into the generated app's config so a scaffolded project
// points at the same API the CLI is using — self-hosted included.
func (c *Client) BaseURL() string { return c.baseURL }

func NewClient(baseURL, apiKey string) *Client {
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	return &Client{
		baseURL: baseURL,
		apiKey:  apiKey,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
		longClient: &http.Client{
			Timeout: invokeTimeout,
		},
	}
}

// do executes a request on the default control-plane client (30s timeout).
func (c *Client) do(method, path string, body interface{}) ([]byte, int, error) {
	return c.doWith(c.httpClient, method, path, body)
}

// doWith is the single request path; the caller chooses which client bounds
// it. Only InvokeFunction passes longClient — everything else goes through
// do and the fail-fast default.
func (c *Client) doWith(hc *http.Client, method, path string, body interface{}) ([]byte, int, error) {
	var reqBody io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, 0, err
		}
		reqBody = bytes.NewReader(data)
	}

	req, err := http.NewRequest(method, c.baseURL+path, reqBody)
	if err != nil {
		return nil, 0, err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := hc.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, err
	}

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		// A 403 with code "insufficient_scope" is a key-scope problem, not a
		// wrong-account problem: re-logging in won't help — a higher-scoped
		// key is needed. Distinguish it so the message names the real fix
		// (matters especially for API-key callers like the MCP server, which
		// cannot run `koolbase login`).
		if resp.StatusCode == http.StatusForbidden {
			var body struct {
				Code  string `json:"code"`
				Error string `json:"error"`
			}
			if json.Unmarshal(data, &body) == nil {
				switch body.Code {
				case "insufficient_scope":
					return data, resp.StatusCode, fmt.Errorf("insufficient_scope: %s", body.Error)
				case "oauth_only_account":
					return data, resp.StatusCode, fmt.Errorf("this account uses Google or Apple sign-in and has no password.\nSet a CLI password in dashboard settings (Account → Password), or authenticate with an API key instead")
				}
			}
		}
		return data, resp.StatusCode, authError(resp.StatusCode)
	}

	if resp.StatusCode == http.StatusConflict {
		// A Google/GitHub login against an email that already has a
		// password account. Re-running the browser flow won't help — the
		// user needs their existing method. Name it, so email/password
		// users aren't dead-ended (KB-6 dogfood finding).
		var body struct {
			Code  string `json:"code"`
			Error string `json:"error"`
		}
		if json.Unmarshal(data, &body) == nil && body.Code == "account_exists" {
			return data, resp.StatusCode, fmt.Errorf(
				"an account with this email already uses email/password sign-in.\n" +
					"Run `koolbase login --password` to sign in, then connect Google from dashboard settings if you want browser login")
		}
	}

	if resp.StatusCode == http.StatusNotFound {
		// The API answers 404 both for "doesn't exist" and "not yours"
		// (deliberate: access checks don't confirm resource existence). Say
		// so, and name the account, so a wrong-org login is diagnosable.
		return data, resp.StatusCode, notFoundError()
	}
	return data, resp.StatusCode, nil
}

// identityHint is set by command setup (the logged-in email from config) so
// auth failures can say WHO the CLI is acting as — a stale or wrong-account
// token then fails with an actionable message instead of a bare
// "unauthorized" (Phase 8 dogfood finding).
var identityHint string

// SetIdentityHint records the account label used in auth-failure messages.
func SetIdentityHint(email string) { identityHint = email }

func notFoundError() error {
	who := "the current account"
	if identityHint != "" {
		who = identityHint
	}
	return fmt.Errorf("not found — either it does not exist, or %s has no access to it (check `koolbase whoami`; log in with the account that owns this project if needed)", who)
}

func authError(status int) error {
	who := "an unknown account (run `koolbase whoami`)"
	if identityHint != "" {
		who = identityHint
	}
	verb := "was rejected"
	if status == http.StatusForbidden {
		verb = "lacks access to this resource"
	}
	return fmt.Errorf("authentication as %s %s — check `koolbase whoami`, or `koolbase login` with the account that owns this project", who, verb)
}

// ─── Auth ──────────────────────────────────────────────────────────────────

type LoginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type LoginResponse struct {
	AccessToken string `json:"access_token"`
	User        struct {
		ID    string `json:"id"`
		Email string `json:"email"`
		OrgID string `json:"org_id"`
	} `json:"user"`
	Error string `json:"error"`
}

func (c *Client) Login(email, password string) (*LoginResponse, error) {
	data, status, err := c.do("POST", "/v1/auth/login", LoginRequest{
		Email:    email,
		Password: password,
	})
	if err != nil {
		return nil, err
	}

	var resp LoginResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, err
	}
	if status != 200 {
		return nil, fmt.Errorf("login failed: %s", resp.Error)
	}
	return &resp, nil
}

// oauthLoginRequest is the payload for POST /v1/auth/oauth. For Google, the
// server verifies id_token against Google's JWKS; for GitHub, it verifies the
// access token by calling GitHub's API. Either way the email is derived from
// the verified identity server-side — client-supplied fields are advisory.
type oauthLoginRequest struct {
	Provider string `json:"provider"`
	IDToken  string `json:"id_token,omitempty"`
	Token    string `json:"token,omitempty"`
}

// LoginWithGoogle exchanges a Google id_token (obtained via the browser OAuth
// flow) for a Koolbase session. The API verifies the token server-side; a
// failed verification returns 401 invalid_oauth_token.
func (c *Client) LoginWithGoogle(idToken string) (*LoginResponse, error) {
	data, status, err := c.do("POST", "/v1/auth/oauth", oauthLoginRequest{
		Provider: "google",
		IDToken:  idToken,
	})
	if err != nil {
		return nil, err
	}
	var resp LoginResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, err
	}
	if status != 200 {
		return nil, fmt.Errorf("google login failed: %s", resp.Error)
	}
	return &resp, nil
}

// LoginWithGitHub exchanges a GitHub access token (obtained via the browser
// OAuth flow) for a Koolbase session. The API verifies the token by resolving
// the identity from GitHub's API; a failed verification returns 401
// invalid_oauth_token.
func (c *Client) LoginWithGitHub(accessToken string) (*LoginResponse, error) {
	data, status, err := c.do("POST", "/v1/auth/oauth", oauthLoginRequest{
		Provider: "github",
		Token:    accessToken,
	})
	if err != nil {
		return nil, err
	}
	var resp LoginResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, err
	}
	if status != 200 {
		return nil, fmt.Errorf("github login failed: %s", resp.Error)
	}
	return &resp, nil
}

// Logout invalidates the current session server-side. The client must be built
// with the session token (do() attaches it). Best-effort: the caller clears
// local config regardless of the result, so a failed/expired server logout
// still logs the user out locally.
func (c *Client) Logout() error {
	_, status, err := c.do("POST", "/v1/auth/logout", nil)
	if err != nil {
		return err
	}
	if status != 200 && status != 204 {
		return fmt.Errorf("logout failed with status %d", status)
	}
	return nil
}

// connectIdentityRequest is the payload for POST /v1/auth/identities/connect.
type connectIdentityRequest struct {
	Provider string `json:"provider"`
	Token    string `json:"token,omitempty"`
	IDToken  string `json:"id_token,omitempty"`
}

// ConnectIdentity attaches a verified provider identity to the currently
// authenticated account. Requires the client to be built with a valid session
// (do() attaches it as the bearer token). For github the token is an access
// token; for google it's an id_token.
func (c *Client) ConnectIdentity(provider, token, idToken string) error {
	data, status, err := c.do("POST", "/v1/auth/identities/connect", connectIdentityRequest{
		Provider: provider,
		Token:    token,
		IDToken:  idToken,
	})
	if err != nil {
		return err
	}
	if status == 200 {
		return nil
	}
	// Surface the server's error code/message (e.g. provider_identity_already_linked).
	var resp struct {
		Code  string `json:"code"`
		Error string `json:"error"`
	}
	_ = json.Unmarshal(data, &resp)
	msg := resp.Error
	if msg == "" {
		msg = fmt.Sprintf("connect failed with status %d", status)
	}
	return fmt.Errorf("%s", msg)
}

// ─── Projects ──────────────────────────────────────────────────────────────

type Project struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func (c *Client) ListProjects(orgID string) ([]Project, error) {
	data, status, err := c.do("GET", "/v1/organizations/"+orgID+"/projects", nil)
	if err != nil {
		return nil, err
	}
	if status != 200 {
		return nil, fmt.Errorf("failed to list projects: %s", string(data))
	}

	var projects []Project
	if err := json.Unmarshal(data, &projects); err != nil {
		return nil, err
	}
	return projects, nil
}

// ─── Functions ─────────────────────────────────────────────────────────────

type Function struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	Runtime        string `json:"runtime"`
	Version        int    `json:"version"`
	IsActive       bool   `json:"is_active"`
	TimeoutMs      int    `json:"timeout_ms"`
	LastDeployedAt string `json:"last_deployed_at"`
}

type DeployRequest struct {
	Name         string  `json:"name"`
	Code         string  `json:"code"`
	Runtime      string  `json:"runtime"`
	TimeoutMs    int     `json:"timeout_ms"`
	Pubspec      *string `json:"pubspec,omitempty"`
	RequiresAuth *bool   `json:"requires_auth,omitempty"`
}

func (c *Client) DeployFunction(projectID string, req DeployRequest) (*Function, error) {
	data, status, err := c.do("POST", "/v1/projects/"+projectID+"/functions", req)
	if err != nil {
		return nil, err
	}

	var fn Function
	if err := json.Unmarshal(data, &fn); err != nil {
		return nil, err
	}
	if status != 201 {
		var errResp struct {
			Error string `json:"error"`
		}
		json.Unmarshal(data, &errResp)
		return nil, fmt.Errorf("deploy failed: %s", errResp.Error)
	}
	return &fn, nil
}

func (c *Client) ListFunctions(projectID string) ([]Function, error) {
	data, status, err := c.do("GET", "/v1/projects/"+projectID+"/functions", nil)
	if err != nil {
		return nil, err
	}
	if status != 200 {
		return nil, fmt.Errorf("failed to list functions: %s", string(data))
	}

	var fns []Function
	if err := json.Unmarshal(data, &fns); err != nil {
		return nil, err
	}
	return fns, nil
}

func (c *Client) DeleteFunction(projectID, name string) error {
	data, status, err := c.do("DELETE", "/v1/projects/"+projectID+"/functions/"+name, nil)
	if err != nil {
		return err
	}
	if status != 200 && status != 204 {
		return fmt.Errorf("failed to delete function: %s", string(data))
	}
	return nil
}

// ─── Invoke ────────────────────────────────────────────────────────────────

type InvokeRequest struct {
	Body map[string]interface{} `json:"body"`
}

type InvokeResponse struct {
	Status int                    `json:"status"`
	Body   map[string]interface{} `json:"body"`
	LogID  string                 `json:"log_id"`
	Error  string                 `json:"error"`
}

func (c *Client) InvokeFunction(projectID, name string, body map[string]interface{}) (*InvokeResponse, error) {
	data, _, err := c.doWith(c.longClient, "POST", "/v1/projects/"+projectID+"/functions/"+name+"/invoke", InvokeRequest{Body: body})
	if err != nil {
		return nil, err
	}

	var resp InvokeResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// ─── Logs ──────────────────────────────────────────────────────────────────

type FunctionLog struct {
	ID          string `json:"id"`
	Status      string `json:"status"`
	DurationMs  int    `json:"duration_ms"`
	TriggerType string `json:"trigger_type"`
	Output      string `json:"output"`
	Error       string `json:"error"`
	CreatedAt   string `json:"created_at"`
}

func (c *Client) GetFunctionLogs(projectID, functionID string, limit int) ([]FunctionLog, error) {
	path := fmt.Sprintf("/v1/projects/%s/functions/logs?function_id=%s&limit=%d", projectID, functionID, limit)
	data, status, err := c.do("GET", path, nil)
	if err != nil {
		return nil, err
	}
	if status != 200 {
		return nil, fmt.Errorf("failed to get logs: %s", string(data))
	}

	var logs []FunctionLog
	if err := json.Unmarshal(data, &logs); err != nil {
		return nil, err
	}
	return logs, nil
}

// ─── Crons ─────────────────────────────────────────────────────────────────

type CronSchedule struct {
	ID             string `json:"id"`
	ProjectID      string `json:"project_id"`
	FunctionName   string `json:"function_name"`
	CronExpression string `json:"cron_expression"`
	Enabled        bool   `json:"enabled"`
	LastRunAt      string `json:"last_run_at"`
	NextRunAt      string `json:"next_run_at"`
	CreatedAt      string `json:"created_at"`
}

func (c *Client) ListCrons(projectID string) ([]CronSchedule, error) {
	data, status, err := c.do("GET", "/v1/projects/"+projectID+"/crons", nil)
	if err != nil {
		return nil, err
	}
	if status != 200 {
		return nil, fmt.Errorf("failed to list crons: %s", string(data))
	}
	var schedules []CronSchedule
	if err := json.Unmarshal(data, &schedules); err != nil {
		return nil, err
	}
	return schedules, nil
}

func (c *Client) CreateCron(projectID, functionName, cronExpression string) (*CronSchedule, error) {
	data, status, err := c.do("POST", "/v1/projects/"+projectID+"/crons", map[string]string{
		"function_name":   functionName,
		"cron_expression": cronExpression,
	})
	if err != nil {
		return nil, err
	}
	var schedule CronSchedule
	if err := json.Unmarshal(data, &schedule); err != nil {
		return nil, err
	}
	if status != 201 {
		var errResp struct {
			Error string `json:"error"`
		}
		json.Unmarshal(data, &errResp)
		return nil, fmt.Errorf("failed to create cron: %s", errResp.Error)
	}
	return &schedule, nil
}

func (c *Client) DeleteCron(projectID, cronID string) error {
	data, status, err := c.do("DELETE", "/v1/projects/"+projectID+"/crons/"+cronID, nil)
	if err != nil {
		return err
	}
	if status != 204 {
		return fmt.Errorf("failed to delete cron: %s", string(data))
	}
	return nil
}

func (c *Client) UpdateCron(projectID, cronID string, enabled bool) (*CronSchedule, error) {
	data, status, err := c.do("PATCH", "/v1/projects/"+projectID+"/crons/"+cronID, map[string]bool{
		"enabled": enabled,
	})
	if err != nil {
		return nil, err
	}
	var schedule CronSchedule
	if err := json.Unmarshal(data, &schedule); err != nil {
		return nil, err
	}
	if status != 200 {
		var errResp struct {
			Error string `json:"error"`
		}
		json.Unmarshal(data, &errResp)
		return nil, fmt.Errorf("failed to update cron: %s", errResp.Error)
	}
	return &schedule, nil
}

// ─── Code Push ─────────────────────────────────────────────────────────────

type Bundle struct {
	ID                string `json:"bundle_id"`
	AppID             string `json:"app_id"`
	Version           int    `json:"version"`
	BaseAppVersion    string `json:"base_app_version"`
	MaxAppVersion     string `json:"max_app_version"`
	Platform          string `json:"platform"`
	Channel           string `json:"channel"`
	Status            string `json:"status"`
	RolloutPercentage int    `json:"rollout_percentage"`
	Checksum          string `json:"checksum"`
	SizeBytes         int    `json:"size_bytes"`
	StorageKey        string `json:"storage_key"`
	CreatedAt         string `json:"created_at"`
}

type CreateBundleRequest struct {
	BaseAppVersion    string                 `json:"base_app_version"`
	MaxAppVersion     string                 `json:"max_app_version"`
	Platform          string                 `json:"platform"`
	Channel           string                 `json:"channel"`
	RolloutPercentage int                    `json:"rollout_percentage"`
	Checksum          string                 `json:"checksum"`
	Signature         string                 `json:"signature"`
	SizeBytes         int                    `json:"size_bytes"`
	Payload           map[string]interface{} `json:"payload"`
}

func (c *Client) CreateBundle(appID string, req CreateBundleRequest) (*Bundle, error) {
	data, status, err := c.do("POST", "/v1/apps/"+appID+"/bundles", req)
	if err != nil {
		return nil, err
	}
	var bundle Bundle
	if err := json.Unmarshal(data, &bundle); err != nil {
		return nil, err
	}
	if status != 201 {
		var errResp struct {
			Error string `json:"error"`
		}
		json.Unmarshal(data, &errResp)
		return nil, fmt.Errorf("failed to create bundle: %s", errResp.Error)
	}
	return &bundle, nil
}

func (c *Client) UploadBundleArtifact(appID, bundleID, zipPath string) error {
	f, err := os.Open(zipPath)
	if err != nil {
		return fmt.Errorf("could not open zip: %w", err)
	}
	defer f.Close()

	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	part, err := writer.CreateFormFile("artifact", filepath.Base(zipPath))
	if err != nil {
		return err
	}
	if _, err := io.Copy(part, f); err != nil {
		return err
	}
	writer.Close()

	url := c.baseURL + "/v1/apps/" + appID + "/bundles/" + bundleID + "/upload"
	req, err := http.NewRequest("POST", url, &buf)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("upload failed: %s", string(body))
	}
	return nil
}

func (c *Client) PublishBundle(appID, bundleID string) error {
	data, status, err := c.do("POST", "/v1/apps/"+appID+"/bundles/"+bundleID+"/publish", nil)
	if err != nil {
		return err
	}
	if status != 200 {
		var errResp struct {
			Error string `json:"error"`
		}
		json.Unmarshal(data, &errResp)
		return fmt.Errorf("publish failed: %s", errResp.Error)
	}
	return nil
}

func (c *Client) RecallBundle(appID, bundleID string) error {
	data, status, err := c.do("POST", "/v1/apps/"+appID+"/bundles/"+bundleID+"/recall", nil)
	if err != nil {
		return err
	}
	if status != 200 {
		var errResp struct {
			Error string `json:"error"`
		}
		json.Unmarshal(data, &errResp)
		return fmt.Errorf("recall failed: %s", errResp.Error)
	}
	return nil
}

func (c *Client) ListBundles(appID string) ([]Bundle, error) {
	data, status, err := c.do("GET", "/v1/apps/"+appID+"/bundles", nil)
	if err != nil {
		return nil, err
	}
	if status != 200 {
		return nil, fmt.Errorf("failed to list bundles: %s", string(data))
	}
	var resp struct {
		Bundles []Bundle `json:"bundles"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, err
	}
	return resp.Bundles, nil
}

func (c *Client) UpdateBundleChecksum(appID, bundleID, checksum string, sizeBytes int) error {
	data, status, err := c.do("PATCH", "/v1/apps/"+appID+"/bundles/"+bundleID+"/checksum", map[string]interface{}{
		"checksum":   checksum,
		"size_bytes": sizeBytes,
	})
	if err != nil {
		return err
	}
	if status != 200 {
		var errResp struct {
			Error string `json:"error"`
		}
		json.Unmarshal(data, &errResp)
		return fmt.Errorf("checksum update failed: %s", errResp.Error)
	}
	return nil
}

func (c *Client) SetBundleMandatory(appID, bundleID string, mandatory bool) error {
	data, status, err := c.do("PATCH", "/v1/apps/"+appID+"/bundles/"+bundleID+"/mandatory", map[string]bool{
		"mandatory": mandatory,
	})
	if err != nil {
		return err
	}
	if status != 200 {
		var errResp struct {
			Error string `json:"error"`
		}
		json.Unmarshal(data, &errResp)
		return fmt.Errorf("failed to update mandatory flag: %s", errResp.Error)
	}
	return nil
}

// ─── Code Push (System B: Releases + Patches) ───────────────────────────────

type Release struct {
	ID             string          `json:"release_id"`
	AppID          string          `json:"app_id"`
	BuildID        string          `json:"build_id"`
	FlutterVersion string          `json:"flutter_version"`
	Platform       string          `json:"platform"`
	AppVersion     string          `json:"app_version"`
	Channel        string          `json:"channel"`
	BuildConfig    json.RawMessage `json:"build_config,omitempty"`
	CreatedBy      string          `json:"created_by"`
	CreatedAt      string          `json:"created_at"`
}

type Patch struct {
	ID                string `json:"patch_id"`
	ReleaseID         string `json:"release_id"`
	AppID             string `json:"app_id"`
	PatchNumber       int    `json:"patch_number"`
	Status            string `json:"status"`
	RolloutPercentage int    `json:"rollout_percentage"`
	Mandatory         bool   `json:"mandatory"`
	ReleaseNotes      string `json:"release_notes,omitempty"`
	Checksum          string `json:"checksum"`
	Signature         string `json:"signature"`
	SizeBytes         int    `json:"size_bytes"`
	StorageKey        string `json:"storage_key"`
	CreatedBy         string `json:"created_by"`
	CreatedAt         string `json:"created_at"`
	PublishedAt       string `json:"published_at,omitempty"`
	RecalledAt        string `json:"recalled_at,omitempty"`
}

type CreateReleaseRequest struct {
	BuildID        string          `json:"build_id"`
	FlutterVersion string          `json:"flutter_version"`
	Platform       string          `json:"platform"`
	AppVersion     string          `json:"app_version"`
	MatchMode      string          `json:"match_mode"`
	Channel        string          `json:"channel"`
	BuildConfig    json.RawMessage `json:"build_config,omitempty"`
}

type CreatePatchRequest struct {
	ReleaseID         string `json:"release_id"`
	RolloutPercentage int    `json:"rollout_percentage"`
	Mandatory         bool   `json:"mandatory"`
	ReleaseNotes      string `json:"release_notes"`
	Checksum          string `json:"checksum"`
	Signature         string `json:"signature"`
	SizeBytes         int    `json:"size_bytes"`
}

func (c *Client) CreateRelease(appID string, req CreateReleaseRequest) (*Release, error) {
	data, status, err := c.do("POST", "/v1/apps/"+appID+"/releases", req)
	if err != nil {
		return nil, err
	}
	if status != 201 {
		var errResp struct {
			Error string `json:"error"`
		}
		json.Unmarshal(data, &errResp)
		return nil, fmt.Errorf("failed to create release: %s", errResp.Error)
	}
	var rel Release
	if err := json.Unmarshal(data, &rel); err != nil {
		return nil, err
	}
	return &rel, nil
}

func (c *Client) ListReleases(appID string) ([]Release, error) {
	data, status, err := c.do("GET", "/v1/apps/"+appID+"/releases", nil)
	if err != nil {
		return nil, err
	}
	if status != 200 {
		return nil, fmt.Errorf("failed to list releases: %s", string(data))
	}
	var resp struct {
		Releases []Release `json:"releases"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, err
	}
	return resp.Releases, nil
}

func (c *Client) CreatePatch(appID, releaseID string, req CreatePatchRequest) (*Patch, error) {
	req.ReleaseID = releaseID
	data, status, err := c.do("POST", "/v1/apps/"+appID+"/releases/"+releaseID+"/patches", req)
	if err != nil {
		return nil, err
	}
	if status != 201 {
		var errResp struct {
			Error string `json:"error"`
		}
		json.Unmarshal(data, &errResp)
		return nil, fmt.Errorf("failed to create patch: %s", errResp.Error)
	}
	var p Patch
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

func (c *Client) ListPatches(appID, releaseID string) ([]Patch, error) {
	data, status, err := c.do("GET", "/v1/apps/"+appID+"/releases/"+releaseID+"/patches", nil)
	if err != nil {
		return nil, err
	}
	if status != 200 {
		return nil, fmt.Errorf("failed to list patches: %s", string(data))
	}
	var resp struct {
		Patches []Patch `json:"patches"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, err
	}
	return resp.Patches, nil
}

func (c *Client) UploadPatchArtifact(appID, patchID, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("could not open patch: %w", err)
	}
	defer f.Close()

	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	part, err := writer.CreateFormFile("artifact", filepath.Base(path))
	if err != nil {
		return err
	}
	if _, err := io.Copy(part, f); err != nil {
		return err
	}
	writer.Close()

	url := c.baseURL + "/v1/apps/" + appID + "/patches/" + patchID + "/upload"
	req, err := http.NewRequest("POST", url, &buf)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("upload failed: %s", string(body))
	}
	return nil
}

func (c *Client) PublishPatch(appID, patchID string) error {
	data, status, err := c.do("POST", "/v1/apps/"+appID+"/patches/"+patchID+"/publish", nil)
	if err != nil {
		return err
	}
	if status != 200 {
		var errResp struct {
			Error string `json:"error"`
		}
		json.Unmarshal(data, &errResp)
		return fmt.Errorf("publish failed: %s", errResp.Error)
	}
	return nil
}

func (c *Client) RecallPatch(appID, patchID string) error {
	data, status, err := c.do("POST", "/v1/apps/"+appID+"/patches/"+patchID+"/recall", nil)
	if err != nil {
		return err
	}
	if status != 200 {
		var errResp struct {
			Error string `json:"error"`
		}
		json.Unmarshal(data, &errResp)
		return fmt.Errorf("recall failed: %s", errResp.Error)
	}
	return nil
}

// ─── Snapshot ──────────────────────────────────────────────────────────────

func (c *Client) SnapshotPull(projectID string) ([]byte, error) {
	data, status, err := c.do("GET", "/v1/projects/"+projectID+"/snapshot", nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("snapshot pull failed (%d): %s", status, string(data))
	}
	return data, nil
}

func (c *Client) SnapshotApply(projectID string, snapshot json.RawMessage, dryRun, prune, force bool) ([]byte, error) {
	path := "/v1/projects/" + projectID + "/snapshot/apply"
	sep := "?"
	add := func(k string) {
		path += sep + k + "=true"
		sep = "&"
	}
	if dryRun {
		add("dry_run")
	}
	if prune {
		add("prune")
	}
	if force {
		add("force")
	}
	data, status, err := c.do("POST", path, snapshot)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("snapshot apply failed (%d): %s", status, string(data))
	}
	return data, nil
}

// ─── Secrets ───────────────────────────────────────────────────────────────

type Secret struct {
	Name      string `json:"name"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

func (c *Client) ListSecrets(projectID string) ([]Secret, error) {
	data, status, err := c.do("GET", "/v1/projects/"+projectID+"/secrets", nil)
	if err != nil {
		return nil, err
	}
	if status != 200 {
		return nil, fmt.Errorf("failed to list secrets: %s", string(data))
	}
	// Tolerate both a bare array and a {"secrets": [...]} envelope.
	var secrets []Secret
	if err := json.Unmarshal(data, &secrets); err == nil {
		return secrets, nil
	}
	var wrapped struct {
		Secrets []Secret `json:"secrets"`
	}
	if err := json.Unmarshal(data, &wrapped); err != nil {
		return nil, err
	}
	return wrapped.Secrets, nil
}

func (c *Client) UpsertSecret(projectID, name, value string) error {
	data, status, err := c.do("POST", "/v1/projects/"+projectID+"/secrets", map[string]string{
		"name":  name,
		"value": value,
	})
	if err != nil {
		return err
	}
	if status != 200 && status != 201 {
		var errResp struct {
			Error string `json:"error"`
		}
		json.Unmarshal(data, &errResp)
		return fmt.Errorf("failed to save secret: %s", errResp.Error)
	}
	return nil
}

func (c *Client) DeleteSecret(projectID, name string) error {
	data, status, err := c.do("DELETE", "/v1/projects/"+projectID+"/secrets/"+name, nil)
	if status == 404 {
		return fmt.Errorf("secret %s does not exist", name)
	}
	if err != nil {
		return err
	}
	if status == 200 || status == 204 {
		return nil
	}
	var errResp struct {
		Error string `json:"error"`
	}
	json.Unmarshal(data, &errResp)
	if errResp.Error != "" {
		return fmt.Errorf("failed to delete secret: %s", errResp.Error)
	}
	return fmt.Errorf("failed to delete secret: %s", string(data))
}

// ─── Dead Letters ──────────────────────────────────────────────────────────

type DeadLetter struct {
	ID           string `json:"id"`
	FunctionName string `json:"function_name"`
	EventType    string `json:"event_type"`
	Collection   string `json:"collection"`
	Attempts     int    `json:"attempts"`
	LastError    string `json:"last_error"`
	FailedAt     string `json:"failed_at"`
}

func (c *Client) ListDeadLetters(projectID string, limit int) ([]DeadLetter, error) {
	path := fmt.Sprintf("/v1/projects/%s/dead-letters?limit=%d", projectID, limit)
	data, status, err := c.do("GET", path, nil)
	if err != nil {
		return nil, err
	}
	if status != 200 {
		return nil, fmt.Errorf("failed to list dead letters: %s", string(data))
	}
	// Tolerate both a bare array and a {"data": [...]} envelope.
	var letters []DeadLetter
	if err := json.Unmarshal(data, &letters); err == nil {
		return letters, nil
	}
	var wrapped struct {
		Data []DeadLetter `json:"data"`
	}
	if err := json.Unmarshal(data, &wrapped); err != nil {
		return nil, err
	}
	return wrapped.Data, nil
}

func (c *Client) ReplayDeadLetter(projectID, id string) error {
	data, status, err := c.do("POST", "/v1/projects/"+projectID+"/dead-letters/"+id+"/replay", nil)
	if err != nil {
		return err
	}
	if status != 200 {
		var errResp struct {
			Error string `json:"error"`
		}
		json.Unmarshal(data, &errResp)
		if errResp.Error != "" {
			return fmt.Errorf("failed to replay dead letter: %s", errResp.Error)
		}
		return fmt.Errorf("failed to replay dead letter: %s", string(data))
	}
	return nil
}

func (c *Client) DeleteDeadLetter(projectID, id string) error {
	data, status, err := c.do("DELETE", "/v1/projects/"+projectID+"/dead-letters/"+id, nil)
	if err != nil {
		return err
	}
	if status != 200 && status != 204 {
		return fmt.Errorf("failed to delete dead letter: %s", string(data))
	}
	return nil
}

// ─── Triggers ──────────────────────────────────────────────────────────────

type Trigger struct {
	ID           string `json:"id"`
	FunctionName string `json:"function_name"`
	EventType    string `json:"event_type"`
	Collection   string `json:"collection"`
	Enabled      bool   `json:"enabled"`
	CreatedAt    string `json:"created_at"`
}

type TriggerStat struct {
	FunctionName string `json:"function_name"`
	EventType    string `json:"event_type"`
	Collection   string `json:"collection"`
	Total        int    `json:"total"`
	Successes    int    `json:"successes"`
	Failures     int    `json:"failures"`
	Timeouts     int    `json:"timeouts"`
}

func (c *Client) ListTriggers(projectID string) ([]Trigger, error) {
	data, status, err := c.do("GET", "/v1/projects/"+projectID+"/triggers", nil)
	if err != nil {
		return nil, err
	}
	if status != 200 {
		return nil, fmt.Errorf("failed to list triggers: %s", string(data))
	}
	var triggers []Trigger
	if err := json.Unmarshal(data, &triggers); err == nil {
		return triggers, nil
	}
	var wrapped struct {
		Data []Trigger `json:"data"`
	}
	if err := json.Unmarshal(data, &wrapped); err != nil {
		return nil, err
	}
	return wrapped.Data, nil
}

func (c *Client) CreateTrigger(projectID, functionName, eventType, collection string) (*Trigger, error) {
	data, status, err := c.do("POST", "/v1/projects/"+projectID+"/triggers", map[string]string{
		"function_name": functionName,
		"event_type":    eventType,
		"collection":    collection,
	})
	if err != nil {
		return nil, err
	}
	if status != 200 && status != 201 {
		var errResp struct {
			Error string `json:"error"`
		}
		json.Unmarshal(data, &errResp)
		if errResp.Error != "" {
			return nil, fmt.Errorf("failed to create trigger: %s", errResp.Error)
		}
		return nil, fmt.Errorf("failed to create trigger: %s", string(data))
	}
	var t Trigger
	if err := json.Unmarshal(data, &t); err == nil && t.ID != "" {
		return &t, nil
	}
	var wrapped struct {
		Data Trigger `json:"data"`
	}
	if err := json.Unmarshal(data, &wrapped); err == nil && wrapped.Data.ID != "" {
		return &wrapped.Data, nil
	}
	return &t, nil
}

func (c *Client) DeleteTrigger(projectID, triggerID string) error {
	data, status, err := c.do("DELETE", "/v1/projects/"+projectID+"/triggers/"+triggerID, nil)
	if err != nil {
		return err
	}
	if status != 200 && status != 204 {
		var errResp struct {
			Error string `json:"error"`
		}
		json.Unmarshal(data, &errResp)
		if errResp.Error != "" {
			return fmt.Errorf("failed to delete trigger: %s", errResp.Error)
		}
		return fmt.Errorf("failed to delete trigger: %s", string(data))
	}
	return nil
}

func (c *Client) GetTriggerStats(projectID string) ([]TriggerStat, error) {
	data, status, err := c.do("GET", "/v1/projects/"+projectID+"/trigger-stats", nil)
	if err != nil {
		return nil, err
	}
	if status != 200 {
		return nil, fmt.Errorf("failed to get trigger stats: %s", string(data))
	}
	var stats []TriggerStat
	if err := json.Unmarshal(data, &stats); err == nil {
		return stats, nil
	}
	var wrapped struct {
		Data []TriggerStat `json:"data"`
	}
	if err := json.Unmarshal(data, &wrapped); err != nil {
		return nil, err
	}
	return wrapped.Data, nil
}

// ─── Unique Constraints ──────────────────────────────────────────────────────

type UniqueConstraint struct {
	ID              string   `json:"id"`
	ProjectID       string   `json:"project_id"`
	CollectionID    string   `json:"collection_id"`
	Fields          []string `json:"fields"`
	CaseInsensitive bool     `json:"case_insensitive"`
	CreatedAt       string   `json:"created_at"`
}

// DuplicateGroup is one set of existing records that already share the same
// value(s) for a candidate constraint — returned on a 409 so the caller can
// dedupe before declaring.
type DuplicateGroup struct {
	Values []string `json:"values"`
	Count  int      `json:"count"`
}

func (c *Client) ListUniqueConstraints(projectID, collection string) ([]UniqueConstraint, error) {
	data, status, err := c.do("GET", "/v1/projects/"+projectID+"/collections/"+collection+"/unique-constraints", nil)
	if err != nil {
		return nil, err
	}
	if status != 200 {
		return nil, fmt.Errorf("failed to list unique constraints: %s", string(data))
	}
	// Tolerate both a bare array and a {"data": [...]} envelope.
	var constraints []UniqueConstraint
	if err := json.Unmarshal(data, &constraints); err == nil {
		return constraints, nil
	}
	var wrapped struct {
		Data []UniqueConstraint `json:"data"`
	}
	if err := json.Unmarshal(data, &wrapped); err != nil {
		return nil, err
	}
	return wrapped.Data, nil
}

// CreateUniqueConstraint declares a unique constraint. On a 409 caused by
// existing duplicate values it returns the offending groups alongside the
// error so the caller can print them; the constraint is not created in that
// case.
func (c *Client) CreateUniqueConstraint(projectID, collection string, fields []string, caseInsensitive bool) (*UniqueConstraint, []DuplicateGroup, error) {
	data, status, err := c.do("POST", "/v1/projects/"+projectID+"/collections/"+collection+"/unique-constraints", map[string]interface{}{
		"fields":           fields,
		"case_insensitive": caseInsensitive,
	})
	if err != nil {
		return nil, nil, err
	}
	if status == 201 {
		var uc UniqueConstraint
		if err := json.Unmarshal(data, &uc); err != nil {
			return nil, nil, err
		}
		return &uc, nil, nil
	}
	// Non-201: parse the structured error. duplicate_values carries the
	// offending groups under details.duplicates.
	var errResp struct {
		Code    string `json:"code"`
		Error   string `json:"error"`
		Details struct {
			Duplicates []DuplicateGroup `json:"duplicates"`
		} `json:"details"`
	}
	json.Unmarshal(data, &errResp)
	if errResp.Code == "duplicate_values" {
		msg := errResp.Error
		if msg == "" {
			msg = "collection has duplicate values for these fields"
		}
		return nil, errResp.Details.Duplicates, fmt.Errorf("%s", msg)
	}
	msg := errResp.Error
	if msg == "" {
		msg = string(data)
	}
	return nil, nil, fmt.Errorf("failed to declare unique constraint: %s", msg)
}

func (c *Client) DeleteUniqueConstraint(projectID, collection, constraintID string) error {
	data, status, err := c.do("DELETE", "/v1/projects/"+projectID+"/collections/"+collection+"/unique-constraints/"+constraintID, nil)
	if err != nil {
		return err
	}
	if status != 200 && status != 204 {
		return fmt.Errorf("failed to delete unique constraint: %s", string(data))
	}
	return nil
}

func (c *Client) DeleteBundle(appID, bundleID string) error {
	data, status, err := c.do("DELETE", "/v1/apps/"+appID+"/bundles/"+bundleID, nil)
	if status == 404 {
		return fmt.Errorf("bundle %s does not exist", bundleID)
	}
	if err != nil {
		return err
	}
	if status == 200 || status == 204 {
		return nil
	}
	if status == 409 {
		return fmt.Errorf("bundle %s is published — recall it first with `koolbase bundle recall`", bundleID)
	}
	var errResp struct {
		Error string `json:"error"`
	}
	json.Unmarshal(data, &errResp)
	if errResp.Error != "" {
		return fmt.Errorf("failed to delete bundle: %s", errResp.Error)
	}
	return fmt.Errorf("failed to delete bundle: %s", string(data))
}

// ─── Vector Fields ──────────────────────────────────────────────────────────

type VectorField struct {
	ID                string  `json:"id"`
	ProjectID         string  `json:"project_id"`
	CollectionID      string  `json:"collection_id"`
	FieldName         string  `json:"field_name"`
	Dimensions        int     `json:"dimensions"`
	DistanceMetric    string  `json:"distance_metric"`
	EmbeddingProvider *string `json:"embedding_provider,omitempty"`
	EmbeddingModel    *string `json:"embedding_model,omitempty"`
	SourceField       *string `json:"source_field,omitempty"`
	LexicalConfig     string  `json:"lexical_config"`
	CreatedAt         string  `json:"created_at"`
}

type CreateVectorFieldRequest struct {
	FieldName         string  `json:"field_name"`
	Dimensions        int     `json:"dimensions"`
	DistanceMetric    string  `json:"distance_metric,omitempty"`
	EmbeddingProvider *string `json:"embedding_provider,omitempty"`
	EmbeddingModel    *string `json:"embedding_model,omitempty"`
	SourceField       *string `json:"source_field,omitempty"`
	LexicalConfig     *string `json:"lexical_config,omitempty"`
}

// UpdateEmbeddingConfigRequest carries the auto-embed triplet for a PATCH.
// All three nil pointers tells the server to clear auto-embed; otherwise
// all three must be non-nil (server enforces the all-or-none rule).
type UpdateEmbeddingConfigRequest struct {
	EmbeddingProvider *string `json:"embedding_provider"`
	EmbeddingModel    *string `json:"embedding_model"`
	SourceField       *string `json:"source_field"`
	LexicalConfig     *string `json:"lexical_config,omitempty"`
}

type EmbedAllResult struct {
	Scanned         int     `json:"scanned"`
	Enqueued        int     `json:"enqueued"`
	SkippedNoSource int     `json:"skipped_no_source"`
	AlreadyCurrent  int     `json:"already_current"`
	HasMore         bool    `json:"has_more"`
	NextCursor      *string `json:"next_cursor,omitempty"`
}

func (c *Client) ListVectorFields(projectID, collection string) ([]VectorField, error) {
	data, status, err := c.do("GET", "/v1/projects/"+projectID+"/collections/"+collection+"/vector-fields", nil)
	if err != nil {
		return nil, err
	}
	if status != 200 {
		return nil, fmt.Errorf("failed to list vector fields: %s", string(data))
	}
	var fields []VectorField
	if err := json.Unmarshal(data, &fields); err == nil {
		return fields, nil
	}
	var wrapped struct {
		Data []VectorField `json:"data"`
	}
	if err := json.Unmarshal(data, &wrapped); err != nil {
		return nil, err
	}
	return wrapped.Data, nil
}

func (c *Client) CreateVectorField(projectID, collection string, req CreateVectorFieldRequest) (*VectorField, error) {
	data, status, err := c.do("POST", "/v1/projects/"+projectID+"/collections/"+collection+"/vector-fields", req)
	if err != nil {
		return nil, err
	}
	if status != 200 && status != 201 {
		var errResp struct {
			Error string `json:"error"`
		}
		json.Unmarshal(data, &errResp)
		if errResp.Error != "" {
			return nil, fmt.Errorf("failed to create vector field: %s", errResp.Error)
		}
		return nil, fmt.Errorf("failed to create vector field: %s", string(data))
	}
	var f VectorField
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, err
	}
	return &f, nil
}

func (c *Client) UpdateVectorFieldEmbedding(projectID, collection, name string, req UpdateEmbeddingConfigRequest) (*VectorField, error) {
	data, status, err := c.do("PATCH", "/v1/projects/"+projectID+"/collections/"+collection+"/vector-fields/"+name, req)
	if err != nil {
		return nil, err
	}
	if status != 200 {
		var errResp struct {
			Error string `json:"error"`
		}
		json.Unmarshal(data, &errResp)
		if errResp.Error != "" {
			return nil, fmt.Errorf("failed to update vector field: %s", errResp.Error)
		}
		return nil, fmt.Errorf("failed to update vector field: %s", string(data))
	}
	var f VectorField
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, err
	}
	return &f, nil
}

func (c *Client) DeleteVectorField(projectID, collection, name string) error {
	data, status, err := c.do("DELETE", "/v1/projects/"+projectID+"/collections/"+collection+"/vector-fields/"+name, nil)
	if err != nil {
		return err
	}
	if status != 200 && status != 204 {
		return fmt.Errorf("failed to delete vector field: %s", string(data))
	}
	return nil
}

func (c *Client) EmbedAllVectorField(projectID, collection, name string, after *string) (*EmbedAllResult, error) {
	path := "/v1/projects/" + projectID + "/collections/" + collection + "/vector-fields/" + name + "/embed-all"
	if after != nil && *after != "" {
		path += "?after=" + url.QueryEscape(*after)
	}
	data, status, err := c.do("POST", path, nil)
	if err != nil {
		return nil, err
	}
	if status != 200 {
		var errResp struct {
			Error string `json:"error"`
		}
		json.Unmarshal(data, &errResp)
		if errResp.Error != "" {
			return nil, fmt.Errorf("failed to embed-all: %s", errResp.Error)
		}
		return nil, fmt.Errorf("failed to embed-all: %s", string(data))
	}
	var res EmbedAllResult
	if err := json.Unmarshal(data, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

// LexicalBackfillResult mirrors EmbedAllResult for the lexical-backfill
// endpoint. Note the response uses `updated` instead of `enqueued` —
// lexical writes are synchronous so the work is done by the time the
// page returns.
type LexicalBackfillResult struct {
	Scanned         int     `json:"scanned"`
	Updated         int     `json:"updated"`
	SkippedNoSource int     `json:"skipped_no_source"`
	AlreadyCurrent  int     `json:"already_current"`
	HasMore         bool    `json:"has_more"`
	NextCursor      *string `json:"next_cursor,omitempty"`
	JobID           *string `json:"job_id,omitempty"`
}

func (c *Client) LexicalBackfillVectorField(projectID, collection, name string, after *string, jobID *string) (*LexicalBackfillResult, error) {
	path := "/v1/projects/" + projectID + "/collections/" + collection + "/vector-fields/" + name + "/lexical-backfill"
	sep := "?"
	if after != nil && *after != "" {
		path += sep + "after=" + url.QueryEscape(*after)
		sep = "&"
	}
	if jobID != nil && *jobID != "" {
		path += sep + "job_id=" + url.QueryEscape(*jobID)
	}
	data, status, err := c.do("POST", path, nil)
	if err != nil {
		return nil, err
	}
	if status != 200 {
		var errResp struct {
			Error string `json:"error"`
		}
		json.Unmarshal(data, &errResp)
		if errResp.Error != "" {
			return nil, fmt.Errorf("failed to lexical-backfill: %s", errResp.Error)
		}
		return nil, fmt.Errorf("failed to lexical-backfill: %s", string(data))
	}
	var res LexicalBackfillResult
	if err := json.Unmarshal(data, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

// ─── Semantic / Lexical / Hybrid Search ────────────────────────────────────

type SearchSemanticRequest struct {
	Collection    string                 `json:"collection"`
	Field         string                 `json:"field"`
	QueryText     string                 `json:"query_text,omitempty"`
	QueryVector   []float64              `json:"query_vector,omitempty"`
	Limit         int                    `json:"limit,omitempty"`
	Where         map[string]interface{} `json:"where,omitempty"`
	Mode          string                 `json:"mode,omitempty"`
	MinSimilarity *float64               `json:"min_similarity,omitempty"`
}

type SearchHit struct {
	Record   map[string]interface{} `json:"record"`
	Distance float64                `json:"distance"`
}

type SearchSemanticResponse struct {
	Results []SearchHit `json:"results"`
	Total   int         `json:"total"`
}

func (c *Client) SearchSemantic(projectID string, req SearchSemanticRequest) (*SearchSemanticResponse, error) {
	data, status, err := c.do("POST", "/v1/projects/"+projectID+"/search-semantic", req)
	if err != nil {
		return nil, err
	}
	if status != 200 {
		var errResp struct {
			Error string `json:"error"`
		}
		json.Unmarshal(data, &errResp)
		if errResp.Error != "" {
			return nil, fmt.Errorf("search failed: %s", errResp.Error)
		}
		return nil, fmt.Errorf("search failed: %s", string(data))
	}
	var resp SearchSemanticResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// ─── Engines ───────────────────────────────────────────────────────────────

type EnginePublic struct {
	Version          string  `json:"version"`
	FlutterVersion   string  `json:"flutter_version"`
	KoolbaseRevision int     `json:"koolbase_revision"`
	HostPlatform     string  `json:"host_platform"`
	HostArch         string  `json:"host_arch"`
	TargetPlatform   string  `json:"target_platform"`
	TargetArch       string  `json:"target_arch"`
	SizeBytes        int64   `json:"size_bytes"`
	SHA256           string  `json:"sha256"`
	Status           string  `json:"status"`
	Changelog        *string `json:"changelog,omitempty"`
}

type EngineListResponse struct {
	Engines []EnginePublic `json:"engines"`
	Count   int            `json:"count"`
}

type EngineDownloadResponse struct {
	URL       string `json:"url"`
	ExpiresAt string `json:"expires_at"`
	SHA256    string `json:"sha256"`
	SizeBytes int64  `json:"size_bytes"`
	Signature string `json:"signature,omitempty"`
}

// ListEngines returns published engines, optionally filtered by platform/arch.
func (c *Client) ListEngines(hostPlatform, hostArch, targetPlatform, targetArch string) (*EngineListResponse, error) {
	q := url.Values{}
	if hostPlatform != "" {
		q.Set("host_platform", hostPlatform)
	}
	if hostArch != "" {
		q.Set("host_arch", hostArch)
	}
	if targetPlatform != "" {
		q.Set("target_platform", targetPlatform)
	}
	if targetArch != "" {
		q.Set("target_arch", targetArch)
	}
	path := "/v1/engines"
	if encoded := q.Encode(); encoded != "" {
		path += "?" + encoded
	}
	data, status, err := c.do(http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("list engines failed (%d): %s", status, string(data))
	}
	var out EngineListResponse
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetEngineDownload returns a signed download URL for an engine variant.
func (c *Client) GetEngineDownload(flutterVersion, hostPlatform, hostArch, targetPlatform, targetArch string) (*EngineDownloadResponse, error) {
	q := url.Values{}
	q.Set("host_platform", hostPlatform)
	q.Set("host_arch", hostArch)
	q.Set("target_platform", targetPlatform)
	q.Set("target_arch", targetArch)
	path := fmt.Sprintf("/v1/engines/%s/download?%s", flutterVersion, q.Encode())
	data, status, err := c.do(http.MethodGet, path, nil)
	if status == http.StatusNotFound {
		return nil, fmt.Errorf("no published engine for flutter %s (host %s/%s -> target %s/%s)", flutterVersion, hostPlatform, hostArch, targetPlatform, targetArch)
	}
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("get download failed (%d): %s", status, string(data))
	}
	var out EngineDownloadResponse
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// BaseUploadURLResponse is returned by the base-upload-url endpoint.
type BaseUploadURLResponse struct {
	UploadURL  string `json:"upload_url"`
	StorageKey string `json:"storage_key"`
}

// BaseDownloadURLResponse is returned by the base-download-url endpoint.
type BaseDownloadURLResponse struct {
	DownloadURL      string `json:"download_url"`
	BaseArtifactKey  string `json:"base_artifact_key"`
	BaseArtifactSize int64  `json:"base_artifact_size"`
}

// GetReleaseBaseUploadURL asks the server for a presigned PUT URL to upload a
// release's base libapp.so for a given ABI.
func (c *Client) GetReleaseBaseUploadURL(appID, releaseID, abi string) (*BaseUploadURLResponse, error) {
	path := "/v1/apps/" + appID + "/releases/" + releaseID + "/base-upload-url?abi=" + abi
	data, status, err := c.do(http.MethodPost, path, nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("base upload url: unexpected status %d: %s", status, string(data))
	}
	var resp BaseUploadURLResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// PutBaseArtifact uploads the base libapp.so directly to the presigned PUT URL
// (R2), bypassing the API server. contentType must match what the presign was
// generated with (application/octet-stream).
func (c *Client) PutBaseArtifact(uploadURL, filePath string) error {
	f, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("open base artifact: %w", err)
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("stat base artifact: %w", err)
	}

	req, err := http.NewRequest(http.MethodPut, uploadURL, f)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	req.ContentLength = info.Size()

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("upload base artifact: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("upload base artifact: status %d: %s", resp.StatusCode, string(b))
	}
	return nil
}

// ConfirmReleaseBase tells the server the base was uploaded at storageKey, so it
// HEADs the object, records its size, and stores the reference on the release.
func (c *Client) ConfirmReleaseBase(appID, releaseID, storageKey string) error {
	path := "/v1/apps/" + appID + "/releases/" + releaseID + "/base-confirm"
	data, status, err := c.do(http.MethodPost, path, map[string]string{"storage_key": storageKey})
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("confirm base: unexpected status %d: %s", status, string(data))
	}
	return nil
}

// GetReleaseBaseDownloadURL asks the server for a presigned GET URL for a
// release's stored base. Returns (nil, nil) when the server reports 404 (no
// stored base) so the caller can fall back to requiring --binary.
func (c *Client) GetReleaseBaseDownloadURL(appID, releaseID string) (*BaseDownloadURLResponse, error) {
	path := "/v1/apps/" + appID + "/releases/" + releaseID + "/base-download-url"
	data, status, err := c.do(http.MethodGet, path, nil)
	if status == http.StatusNotFound {
		return nil, nil // no stored base — caller falls back to --binary
	}
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("base download url: unexpected status %d: %s", status, string(data))
	}
	var resp BaseDownloadURLResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// --- organization API keys ---------------------------------------------------

type OrgKey struct {
	ID         string  `json:"id"`
	OrgID      string  `json:"org_id"`
	Name       string  `json:"name"`
	KeyPrefix  string  `json:"key_prefix"`
	Scope      string  `json:"scope"`
	CreatedAt  string  `json:"created_at"`
	LastUsedAt *string `json:"last_used_at,omitempty"`
	RevokedAt  *string `json:"revoked_at,omitempty"`
}

type MintKeyResponse struct {
	Key    OrgKey `json:"key"`
	Secret string `json:"secret"`
}

// MintKey creates an org API key. Session auth required — the server
// rejects key-authenticated minting (keys cannot mint keys).
func (c *Client) MintKey(orgID, name, scope string) (*MintKeyResponse, error) {
	data, status, err := c.do("POST", fmt.Sprintf("/v1/organizations/%s/api-keys", orgID),
		map[string]string{"name": name, "scope": scope})
	if err != nil {
		return nil, err
	}
	if status != http.StatusCreated && status != http.StatusOK {
		return nil, fmt.Errorf("mint key failed (%d): %s", status, string(data))
	}
	var out MintKeyResponse
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) ListKeys(orgID string) ([]OrgKey, error) {
	data, status, err := c.do("GET", fmt.Sprintf("/v1/organizations/%s/api-keys", orgID), nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("list keys failed (%d): %s", status, string(data))
	}
	var out []OrgKey
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *Client) RevokeKey(orgID, keyID string) error {
	data, status, err := c.do("DELETE", fmt.Sprintf("/v1/organizations/%s/api-keys/%s", orgID, keyID), nil)
	if err != nil {
		return err
	}
	if status != http.StatusOK && status != http.StatusNoContent {
		return fmt.Errorf("revoke key failed (%d): %s", status, string(data))
	}
	return nil
}
