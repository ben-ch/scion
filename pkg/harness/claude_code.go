// Copyright 2026 Google LLC
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

package harness

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	claudeEmbeds "github.com/GoogleCloudPlatform/scion/pkg/harness/claude"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
)

type ClaudeCode struct {
	systemPrompt string
}

func (c *ClaudeCode) Name() string {
	return "claude"
}

func (c *ClaudeCode) AdvancedCapabilities() api.HarnessAdvancedCapabilities {
	return api.HarnessAdvancedCapabilities{
		Harness: "claude",
		Limits: api.HarnessLimitCapabilities{
			MaxTurns:      api.CapabilityField{Support: api.SupportYes},
			MaxModelCalls: api.CapabilityField{Support: api.SupportNo, Reason: "This harness does not emit model-end hook events"},
			MaxDuration:   api.CapabilityField{Support: api.SupportYes},
		},
		Telemetry: api.HarnessTelemetryCapabilities{
			EnabledConfig: api.CapabilityField{Support: api.SupportYes},
			NativeEmitter: api.CapabilityField{Support: api.SupportYes},
		},
		Prompts: api.HarnessPromptCapabilities{
			SystemPrompt:      api.CapabilityField{Support: api.SupportYes},
			AgentInstructions: api.CapabilityField{Support: api.SupportYes},
		},
		Auth: api.HarnessAuthCapabilities{
			APIKey:     api.CapabilityField{Support: api.SupportYes},
			AuthFile:   api.CapabilityField{Support: api.SupportYes},
			OAuthToken: api.CapabilityField{Support: api.SupportYes},
			VertexAI:   api.CapabilityField{Support: api.SupportYes},
			Bedrock:    api.CapabilityField{Support: api.SupportYes},
		},
		Resume: api.CapabilityField{Support: api.SupportYes},
	}
}

func (c *ClaudeCode) GetEnv(agentName string, agentHome string, unixUsername string) map[string]string {
	env := make(map[string]string)

	// Load system prompt content for use in GetCommand.
	if content := c.loadSystemPrompt(agentHome); content != "" {
		c.systemPrompt = content
	}

	return env
}

func (c *ClaudeCode) GetCommand(task string, resume bool, baseArgs []string) []string {
	args := []string{"claude", "--no-chrome", "--dangerously-skip-permissions"}
	if resume {
		args = append(args, "--continue")
	}
	if c.systemPrompt != "" {
		args = append(args, "--system-prompt", c.systemPrompt)
	}
	args = append(args, baseArgs...)
	if task != "" {
		args = append(args, task)
	}
	return args
}

func (c *ClaudeCode) DefaultConfigDir() string {
	return ".claude"
}

func (c *ClaudeCode) SkillsDir() string {
	return ".claude/skills"
}

func (c *ClaudeCode) HasSystemPrompt(agentHome string) bool {
	return c.loadSystemPrompt(agentHome) != ""
}

func (c *ClaudeCode) Provision(ctx context.Context, agentName, agentDir, agentHome, agentWorkspace string) error {
	// 1. Update .claude.json project paths
	if err := c.provisionClaudeJSON(ctx, agentHome, agentWorkspace); err != nil {
		return err
	}

	// 2. Project auth-specific env vars into scion-agent.json
	scionAgentPath := filepath.Join(agentDir, "scion-agent.json")

	data, err := os.ReadFile(scionAgentPath)
	if err != nil {
		return nil // No scion-agent.json, nothing to update
	}
	var cfg api.ScionConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return fmt.Errorf("failed to parse scion-agent.json: %w", err)
	}

	var envUpdates map[string]string

	switch cfg.AuthSelectedType {
	case "api-key":
		// api-key covers two single-secret modes: ANTHROPIC_API_KEY for
		// direct Anthropic access, or AWS_BEARER_TOKEN_BEDROCK for Bedrock.
		// Both env vars are passed through; ResolveAuth picks one at launch.
		envUpdates = map[string]string{
			"ANTHROPIC_API_KEY":        "${ANTHROPIC_API_KEY}",
			"AWS_BEARER_TOKEN_BEDROCK": "${AWS_BEARER_TOKEN_BEDROCK}",
			"AWS_REGION":               "${AWS_REGION}",
			"CLAUDE_CODE_USE_BEDROCK":  "${CLAUDE_CODE_USE_BEDROCK}",
		}
	case "oauth-token":
		envUpdates = map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "${CLAUDE_CODE_OAUTH_TOKEN}"}
	case "auth-file":
		// No env updates — the credentials file is mounted into the
		// container at ~/.claude/.credentials.json and Claude Code reads
		// and refreshes it natively.
	case "vertex-ai":
		// NOTE: gcloud credentials are mounted by buildCommonRunArgs in
		// pkg/runtime/common.go, gated on !BrokerMode.  Do NOT add a
		// gcloud volume here — it would bypass the broker-mode check and
		// leak the broker operator's credentials into agent containers.
		envUpdates = map[string]string{
			"CLAUDE_CODE_USE_VERTEX":      "1",
			"ANTHROPIC_VERTEX_PROJECT_ID": "${GOOGLE_CLOUD_PROJECT}",
			"CLOUD_ML_REGION":             "${GOOGLE_CLOUD_REGION}",
		}
	case "bedrock":
		// SSO/profile mode: ~/.aws is mounted into the container by
		// buildCommonRunArgs (gated on !BrokerMode). The AWS SDK reads
		// the profile config and the cached SSO token from there.
		envUpdates = map[string]string{
			"CLAUDE_CODE_USE_BEDROCK": "1",
			"AWS_PROFILE":             "${AWS_PROFILE}",
			"AWS_REGION":              "${AWS_REGION}",
		}
	}

	if len(envUpdates) > 0 {
		if cfg.Env == nil {
			cfg.Env = make(map[string]string)
		}
		for k, v := range envUpdates {
			if _, exists := cfg.Env[k]; !exists {
				cfg.Env[k] = v
			}
		}
	}

	if len(envUpdates) > 0 {
		newData, err := json.MarshalIndent(cfg, "", "  ")
		if err != nil {
			return fmt.Errorf("failed to marshal updated config: %w", err)
		}
		if err := os.WriteFile(scionAgentPath, newData, 0644); err != nil {
			return fmt.Errorf("failed to write updated scion-agent.json: %w", err)
		}
	}

	return nil
}

func (c *ClaudeCode) provisionClaudeJSON(ctx context.Context, agentHome, agentWorkspace string) error {
	claudeJSONPath := filepath.Join(agentHome, ".claude.json")
	if _, err := os.Stat(claudeJSONPath); os.IsNotExist(err) {
		return nil
	}

	data, err := os.ReadFile(claudeJSONPath)
	if err != nil {
		return err
	}

	var claudeCfg map[string]interface{}
	if err := json.Unmarshal(data, &claudeCfg); err != nil {
		return err
	}

	containerWorkspace := "/workspace"
	// In git-clone mode the workspace is always mounted at /workspace,
	// so skip the worktree-relative path derivation.
	if api.GitCloneFromContext(ctx) != nil {
		// keep default "/workspace"
	} else if idx := strings.Index(agentWorkspace, "/.scion/agents/"); idx >= 0 {
		// Derive the container workspace path from the agent directory structure.
		// The host path is like .../grove/.scion/agents/<name>/workspace and maps
		// to /repo-root/.scion/agents/<name>/workspace inside the container.
		containerWorkspace = "/repo-root" + agentWorkspace[idx:]
	} else if repoRoot, err := util.RepoRoot(); err == nil {
		relWorkspace, err := filepath.Rel(repoRoot, agentWorkspace)
		if err == nil && !strings.HasPrefix(relWorkspace, "..") && relWorkspace != "." {
			containerWorkspace = filepath.Join("/repo-root", relWorkspace)
		}
	}

	// Update projects map
	projects, ok := claudeCfg["projects"].(map[string]interface{})
	if !ok {
		projects = make(map[string]interface{})
		claudeCfg["projects"] = projects
	}

	var projectSettings interface{}
	for _, v := range projects {
		projectSettings = v
		break
	}

	if projectSettings == nil {
		projectSettings = map[string]interface{}{
			"allowedTools":                            []interface{}{},
			"mcpContextUris":                          []interface{}{},
			"mcpServers":                              map[string]interface{}{},
			"enabledMcpjsonServers":                   []interface{}{},
			"disabledMcpjsonServers":                  []interface{}{},
			"hasTrustDialogAccepted":                  true,
			"projectOnboardingSeenCount":              1,
			"hasClaudeMdExternalIncludesApproved":     false,
			"hasClaudeMdExternalIncludesWarningShown": false,
			"exampleFiles":                            []interface{}{},
		}
	}

	newProjects := make(map[string]interface{})
	newProjects[containerWorkspace] = projectSettings
	claudeCfg["projects"] = newProjects

	newData, err := json.MarshalIndent(claudeCfg, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(claudeJSONPath, newData, 0644)
}

// ApplyAuthSettings updates .claude.json with customApiKeyResponses when using
// api-key auth. This pre-approves the API key so Claude Code does not prompt
// for confirmation.
func (c *ClaudeCode) ApplyAuthSettings(agentHome string, resolved *api.ResolvedAuth) error {
	if resolved.Method != "api-key" {
		return nil
	}
	apiKey := resolved.EnvVars["ANTHROPIC_API_KEY"]
	if apiKey == "" {
		return nil
	}

	// Extract the last 20 characters of the key as the fingerprint.
	fingerprint := apiKey
	if len(fingerprint) > 20 {
		fingerprint = fingerprint[len(fingerprint)-20:]
	}

	claudeJSONPath := filepath.Join(agentHome, ".claude.json")
	var claudeCfg map[string]interface{}

	data, err := os.ReadFile(claudeJSONPath)
	if err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("failed to read .claude.json: %w", err)
		}
		claudeCfg = make(map[string]interface{})
	} else {
		if err := json.Unmarshal(data, &claudeCfg); err != nil {
			return fmt.Errorf("failed to parse .claude.json: %w", err)
		}
	}

	claudeCfg["customApiKeyResponses"] = map[string]interface{}{
		"approved": []interface{}{fingerprint},
		"rejected": []interface{}{},
	}

	newData, err := json.MarshalIndent(claudeCfg, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal updated .claude.json: %w", err)
	}

	return os.WriteFile(claudeJSONPath, newData, 0644)
}

func (c *ClaudeCode) GetEmbedDir() string {
	return "claude"
}

func (c *ClaudeCode) GetInterruptKey() string {
	return "Escape"
}

func (c *ClaudeCode) GetHarnessEmbedsFS() (embed.FS, string) {
	return claudeEmbeds.EmbedsFS, "embeds"
}

func (c *ClaudeCode) InjectAgentInstructions(agentHome string, content []byte) error {
	dir := filepath.Join(agentHome, ".claude")
	target := filepath.Join(dir, "CLAUDE.md")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create directory for agent instructions: %w", err)
	}
	// Remove any existing instruction file with non-canonical casing (e.g.,
	// "claude.md" copied from a harness-config home directory). On case-
	// sensitive filesystems these would coexist with "CLAUDE.md" and cause
	// confusion; on case-insensitive filesystems we still want the directory
	// entry to use the canonical uppercase name.
	entries, err := os.ReadDir(dir)
	if err == nil {
		for _, e := range entries {
			if !e.IsDir() && strings.EqualFold(e.Name(), "CLAUDE.md") && e.Name() != "CLAUDE.md" {
				_ = os.Remove(filepath.Join(dir, e.Name()))
			}
		}
	}
	return os.WriteFile(target, content, 0644)
}

func (c *ClaudeCode) GetTelemetryEnv() map[string]string {
	return map[string]string{
		"CLAUDE_CODE_ENABLE_TELEMETRY": "1",
		"OTEL_METRICS_EXPORTER":        "otlp",
		"OTEL_LOGS_EXPORTER":           "otlp",
		"OTEL_EXPORTER_OTLP_PROTOCOL":  "grpc",
		"OTEL_EXPORTER_OTLP_ENDPOINT":  "http://localhost:4317",
		"OTEL_METRIC_EXPORT_INTERVAL":  "30000",
	}
}

func (c *ClaudeCode) InjectSystemPrompt(agentHome string, content []byte) error {
	target := filepath.Join(agentHome, ".claude", "system-prompt.md")
	if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
		return fmt.Errorf("failed to create directory for system prompt: %w", err)
	}
	return os.WriteFile(target, content, 0644)
}

func (c *ClaudeCode) ResolveAuth(auth api.AuthConfig) (*api.ResolvedAuth, error) {
	// Explicit selection support
	if auth.SelectedType != "" {
		switch auth.SelectedType {
		case "api-key":
			// api-key prefers ANTHROPIC_API_KEY (direct Anthropic), then
			// falls back to AWS_BEARER_TOKEN_BEDROCK (Bedrock via bearer
			// token, no SSO). The bedrock auth type covers SSO/profile mode.
			if auth.AnthropicAPIKey != "" {
				return &api.ResolvedAuth{
					Method: "api-key",
					EnvVars: map[string]string{
						"ANTHROPIC_API_KEY": auth.AnthropicAPIKey,
					},
				}, nil
			}
			if auth.AWSBedrockBearerToken != "" {
				if auth.AWSRegion == "" {
					return nil, fmt.Errorf("claude: auth type %q with AWS_BEARER_TOKEN_BEDROCK requires AWS_REGION", auth.SelectedType)
				}
				return c.resolveBedrockBearer(auth), nil
			}
			return nil, fmt.Errorf("claude: auth type %q selected but no API key found; set ANTHROPIC_API_KEY or AWS_BEARER_TOKEN_BEDROCK (+ AWS_REGION)", auth.SelectedType)
		case "oauth-token":
			if auth.ClaudeOAuthToken == "" {
				return nil, fmt.Errorf("claude: auth type %q selected but no OAuth token found; set CLAUDE_CODE_OAUTH_TOKEN (generate with `claude setup-token`)", auth.SelectedType)
			}
			return &api.ResolvedAuth{
				Method: "oauth-token",
				EnvVars: map[string]string{
					"CLAUDE_CODE_OAUTH_TOKEN": auth.ClaudeOAuthToken,
				},
			}, nil
		case "auth-file":
			if auth.ClaudeAuthFile == "" {
				return nil, fmt.Errorf("claude: auth type %q selected but no credentials file found; expected ~/.claude/.credentials.json", auth.SelectedType)
			}
			return &api.ResolvedAuth{
				Method: "auth-file",
				Files: []api.FileMapping{
					{SourcePath: auth.ClaudeAuthFile, ContainerPath: "~/.claude/.credentials.json"},
				},
			}, nil
		case "vertex-ai":
			if auth.GoogleCloudProject == "" || auth.GoogleCloudRegion == "" {
				return nil, fmt.Errorf("claude: auth type %q selected but GOOGLE_CLOUD_PROJECT and/or GOOGLE_CLOUD_REGION not set", auth.SelectedType)
			}
			return c.resolveVertexAI(auth), nil
		case "bedrock":
			if auth.AWSProfile == "" {
				return nil, fmt.Errorf("claude: auth type %q selected but AWS_PROFILE not set", auth.SelectedType)
			}
			if auth.AWSRegion == "" {
				return nil, fmt.Errorf("claude: auth type %q selected but AWS_REGION not set", auth.SelectedType)
			}
			if auth.AWSConfigDir == "" {
				return nil, fmt.Errorf("claude: auth type %q selected but ~/.aws not found; run `aws sso login --profile %s` first", auth.SelectedType, auth.AWSProfile)
			}
			return c.resolveBedrockSSO(auth), nil
		default:
			return nil, fmt.Errorf("claude: unknown auth type %q; valid types are: api-key, oauth-token, auth-file, vertex-ai, bedrock", auth.SelectedType)
		}
	}

	// Auto-detect preference order:
	// 1. ANTHROPIC_API_KEY (api-key, direct)
	// 2. AWS_BEARER_TOKEN_BEDROCK (api-key, Bedrock bearer)
	// 3. CLAUDE_CODE_OAUTH_TOKEN (oauth-token)
	// 4. ~/.claude/.credentials.json (auth-file)
	// 5. AWS_PROFILE + ~/.aws (bedrock SSO)
	// 6. ADC + GCP project/region (vertex-ai)

	// 1. Anthropic API key (direct)
	if auth.AnthropicAPIKey != "" {
		return &api.ResolvedAuth{
			Method: "api-key",
			EnvVars: map[string]string{
				"ANTHROPIC_API_KEY": auth.AnthropicAPIKey,
			},
		}, nil
	}

	// 2. AWS_BEARER_TOKEN_BEDROCK — single-secret Bedrock access via the
	//    bearer-token flow. Surfaced under api-key because it's a single
	//    static secret, like ANTHROPIC_API_KEY. Detected ahead of OAuth/
	//    auth-file because it's an unambiguous LLM-specific env var.
	if auth.AWSBedrockBearerToken != "" && auth.AWSRegion != "" {
		return c.resolveBedrockBearer(auth), nil
	}

	// 3. CLAUDE_CODE_OAUTH_TOKEN (long-lived subscription token from
	//    `claude setup-token`). Higher priority than the credentials file
	//    because it does not rotate and therefore survives long sessions.
	if auth.ClaudeOAuthToken != "" {
		return &api.ResolvedAuth{
			Method: "oauth-token",
			EnvVars: map[string]string{
				"CLAUDE_CODE_OAUTH_TOKEN": auth.ClaudeOAuthToken,
			},
		}, nil
	}

	// 4. ~/.claude/.credentials.json — rotating refresh-token store managed
	//    by Claude Code. We mount the file into the container so Claude Code
	//    can read and refresh it natively rather than scraping a snapshot.
	if auth.ClaudeAuthFile != "" {
		return &api.ResolvedAuth{
			Method: "auth-file",
			Files: []api.FileMapping{
				{SourcePath: auth.ClaudeAuthFile, ContainerPath: "~/.claude/.credentials.json"},
			},
		}, nil
	}

	// 5. AWS Bedrock SSO — requires AWS_PROFILE + AWS_REGION + ~/.aws.
	//    The user must have run `aws sso login --profile <name>` on the
	//    host first; we mount ~/.aws into the container so the SDK can
	//    use the cached SSO token.
	if auth.AWSProfile != "" && auth.AWSRegion != "" && auth.AWSConfigDir != "" {
		return c.resolveBedrockSSO(auth), nil
	}

	// 6. Vertex AI — requires project + region, plus either ADC file or
	//    a GCP service account via the metadata server (assign mode).
	hasVertexCreds := auth.GoogleAppCredentials != "" || auth.GCPMetadataMode == "assign"
	if hasVertexCreds && auth.GoogleCloudProject != "" && auth.GoogleCloudRegion != "" {
		return c.resolveVertexAI(auth), nil
	}

	return nil, fmt.Errorf("claude: no valid auth method found; set ANTHROPIC_API_KEY for direct API access, AWS_BEARER_TOKEN_BEDROCK + AWS_REGION for Bedrock bearer auth, CLAUDE_CODE_OAUTH_TOKEN (from `claude setup-token`) or ~/.claude/.credentials.json for subscription auth, AWS_PROFILE + AWS_REGION (after `aws sso login`) for Bedrock SSO, or provide ADC + GOOGLE_CLOUD_PROJECT + GOOGLE_CLOUD_REGION for Vertex AI")
}

// resolveBedrockBearer returns a ResolvedAuth using AWS_BEARER_TOKEN_BEDROCK.
// Surfaced under the "api-key" method because it's a single static secret,
// matching how api-key works for ANTHROPIC_API_KEY.
func (c *ClaudeCode) resolveBedrockBearer(auth api.AuthConfig) *api.ResolvedAuth {
	return &api.ResolvedAuth{
		Method: "api-key",
		EnvVars: map[string]string{
			"CLAUDE_CODE_USE_BEDROCK":  "1",
			"AWS_BEARER_TOKEN_BEDROCK": auth.AWSBedrockBearerToken,
			"AWS_REGION":               auth.AWSRegion,
		},
	}
}

// resolveBedrockSSO returns a ResolvedAuth using AWS_PROFILE-driven SSO.
// NOTE: ~/.aws is mounted by buildCommonRunArgs in pkg/runtime/common.go,
// gated on !BrokerMode. Do NOT add a ~/.aws volume here — it would bypass
// the broker-mode check and leak the broker operator's credentials into
// agent containers. Mirrors how Vertex AI mounts ~/.config/gcloud.
func (c *ClaudeCode) resolveBedrockSSO(auth api.AuthConfig) *api.ResolvedAuth {
	return &api.ResolvedAuth{
		Method: "bedrock",
		EnvVars: map[string]string{
			"CLAUDE_CODE_USE_BEDROCK": "1",
			"AWS_PROFILE":             auth.AWSProfile,
			"AWS_REGION":              auth.AWSRegion,
		},
	}
}

func (c *ClaudeCode) resolveVertexAI(auth api.AuthConfig) *api.ResolvedAuth {
	adcContainerPath := "~/.config/gcloud/application_default_credentials.json"
	result := &api.ResolvedAuth{
		Method: "vertex-ai",
		EnvVars: map[string]string{
			"CLAUDE_CODE_USE_VERTEX":      "1",
			"CLOUD_ML_REGION":             auth.GoogleCloudRegion,
			"ANTHROPIC_VERTEX_PROJECT_ID": auth.GoogleCloudProject,
		},
	}
	if auth.GoogleAppCredentials != "" {
		result.Files = append(result.Files, api.FileMapping{
			SourcePath:    auth.GoogleAppCredentials,
			ContainerPath: adcContainerPath,
		})
	}
	return result
}

// loadSystemPrompt reads the system prompt file from agentHome and returns
// its content if valid (non-empty and non-placeholder). Returns empty string
// if the file doesn't exist or contains only placeholder text.
func (c *ClaudeCode) loadSystemPrompt(agentHome string) string {
	if agentHome == "" {
		return ""
	}
	path := filepath.Join(agentHome, ".claude", "system-prompt.md")
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	content := strings.TrimSpace(string(data))
	if content == "" || content == "# Placeholder" {
		return ""
	}
	return content
}
