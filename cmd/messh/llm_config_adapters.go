package main

import (
	"encoding/json"
	"fmt"
	"strings"
)

// renderLLMClientConfig keeps harness-specific configuration out of the mesh
// lookup and client-selection flow.
func renderLLMClientConfig(harness, provider, baseURL, tokenCmd string, models []string, modelErr error, wait, service string) (string, any, error) {
	switch harness {
	case "omp":
		config := ompModelsYAML(provider, baseURL, tokenCmd, models)
		var text strings.Builder
		text.WriteString(config)
		fmt.Fprintf(&text, "\nMerge this into ~/.omp/agent/models.yml (under the existing \"providers:\" key, if any). The \"!\" makes omp\n"+
			"run the command for the key, so the token is never stored in the file. Select a model with /model or\n--model %s/<id>.\n%s\n", provider, wait)
		if len(models) == 0 {
			fmt.Fprintf(&text, "The model list could not be fetched, so omp discovers it from %s/models on start; that request needs\n"+
				"approval too unless %s's entry sets auto.read. Rerun with --model ID to list models explicitly.\n", baseURL, service)
		}
		return text.String(), config, nil
	case "pi":
		if len(models) == 0 {
			return "", nil, fmt.Errorf("pi needs the model IDs: rerun with --model ID for each model (%v)", modelErr)
		}
		config, err := piModelsJSON(provider, baseURL, tokenCmd, models)
		if err != nil {
			return "", nil, err
		}
		var obj any
		if err := json.Unmarshal([]byte(config), &obj); err != nil {
			return "", nil, err
		}
		text := fmt.Sprintf("%s\n\nMerge this into ~/.pi/agent/models.json (under \"providers\"). The \"!\" makes pi run the command for\n"+
			"the key at request time. compat turns off the developer role and reasoning_effort, which local servers\n"+
			"often reject; remove it if %s supports them.\n%s\n", config, service, wait)
		return text, obj, nil
	case "openai":
		var text strings.Builder
		fmt.Fprintf(&text, "Base URL  %s\nAPI key   the output of %s (sent as Authorization: Bearer; x-api-key works too)\n", baseURL, tokenCmd)
		if len(models) > 0 {
			fmt.Fprintf(&text, "Models    %s\n", strings.Join(models, ", "))
		}
		fmt.Fprintf(&text, "\nFor the OpenAI SDKs and most tools:\n  export OPENAI_BASE_URL=%s\n  export OPENAI_API_KEY=\"$(%s)\"\n"+
			"PowerShell:\n  $env:OPENAI_BASE_URL = '%s'\n  $env:OPENAI_API_KEY = (%s)\n"+
			"Anthropic-style clients: base URL %s (POST /v1/messages), same key as x-api-key.\n\n%s\n"+
			"Keep client timeouts at 10 minutes or more while prompts are pending (the OpenAI SDKs default to 10 minutes).\n",
			baseURL, tokenCmd, baseURL, tokenCmd, strings.TrimSuffix(baseURL, "/v1"), wait)
		return text.String(), text.String(), nil
	default:
		return "", nil, fmt.Errorf("unsupported client config %q", harness)
	}
}

// ompModelsYAML renders an omp models.yml provider.
func ompModelsYAML(provider, baseURL, tokenCmd string, models []string) string {
	var b strings.Builder
	b.WriteString("providers:\n")
	fmt.Fprintf(&b, "  %s:\n", provider)
	fmt.Fprintf(&b, "    baseUrl: %s\n", yamlString(baseURL))
	fmt.Fprintf(&b, "    apiKey: %s\n", yamlString("!"+tokenCmd))
	b.WriteString("    api: openai-completions\n")
	if len(models) == 0 {
		b.WriteString("    discovery:\n      type: openai-models-list\n")
		return b.String()
	}
	b.WriteString("    models:\n")
	for _, m := range models {
		fmt.Fprintf(&b, "      - id: %s\n", yamlString(m))
	}
	return b.String()
}

func yamlString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// piModelsJSON renders a pi models.json provider.
func piModelsJSON(provider, baseURL, tokenCmd string, models []string) (string, error) {
	type model struct {
		ID string `json:"id"`
	}
	type compat struct {
		SupportsDeveloperRole   bool `json:"supportsDeveloperRole"`
		SupportsReasoningEffort bool `json:"supportsReasoningEffort"`
	}
	type providerConfig struct {
		BaseURL string  `json:"baseUrl"`
		API     string  `json:"api"`
		APIKey  string  `json:"apiKey"`
		Compat  compat  `json:"compat"`
		Models  []model `json:"models"`
	}
	p := providerConfig{BaseURL: baseURL, API: "openai-completions", APIKey: "!" + tokenCmd}
	for _, m := range models {
		p.Models = append(p.Models, model{ID: m})
	}
	out, err := json.MarshalIndent(map[string]map[string]providerConfig{"providers": {provider: p}}, "", "  ")
	return string(out), err
}
