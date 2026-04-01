package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

const defaultBaseURL = "https://api.koolbase.com"

type Client struct {
	baseURL    string
	apiKey     string
	httpClient *http.Client
}

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
	}
}

func (c *Client) do(method, path string, body interface{}) ([]byte, int, error) {
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

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, err
	}

	return data, resp.StatusCode, nil
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
	Name      string `json:"name"`
	Code      string `json:"code"`
	Runtime   string `json:"runtime"`
	TimeoutMs int    `json:"timeout_ms"`
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
		var errResp struct{ Error string `json:"error"` }
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
	data, _, err := c.do("POST", "/v1/projects/"+projectID+"/functions/"+name+"/invoke", InvokeRequest{Body: body})
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
		var errResp struct{ Error string `json:"error"` }
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
		var errResp struct{ Error string `json:"error"` }
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
		var errResp struct{ Error string `json:"error"` }
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
		var errResp struct{ Error string `json:"error"` }
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
		var errResp struct{ Error string `json:"error"` }
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
