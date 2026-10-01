package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"text/tabwriter"
	"time"

	"messh/internal/control"
	"messh/internal/llmproxy"
)

func init() {
	add(&Command{
		Name:    "llm ls",
		Summary: "list OpenAI-compatible model services on the mesh and their local base URLs",
		Output:  `[{device, handle, device_id, online, service, description, up, base_url, error}]`,
		Examples: []string{
			"messh llm ls",
		},
		Run: func(c *Context) error { return runLLMList(c) },
	})
	add(&Command{
		Name: "llm config",
		Args: []Arg{
			{Name: "DEVICE", Help: "device that runs the model service"},
			{Name: "SERVICE", Help: "model service on DEVICE"},
		},
		Summary: "print provider config that lets an agent here use SERVICE on DEVICE",
		Flags: func(fs *flag.FlagSet) {
			choiceFlag(fs, "for", "omp", "which client config to print", "omp", "pi", "openai")
			fs.String("agent", "", "agent whose messh token is the API key (`NAME`; default: the --for harness name)")
			listFlag(fs, "model", "model `ID` to configure (repeatable; default: ask the service)")
			fs.Duration("timeout", 30*time.Second, "how long to wait for the model list (`DURATION`; the owner may have to approve it)")
		},
		Output: `{for, provider, base_url, api_key_command, models[], config (YAML text for omp, provider object for pi, setup text for openai; models[] is empty when the list could not be fetched)}`,
		Person: "the owner of DEVICE may have to approve listing models",
		Waits:  "up to --timeout for the service's model list when --model is not given",
		Examples: []string{
			"messh llm config desktop unsloth",
			"messh llm config desktop unsloth --for pi",
			"messh llm config desktop unsloth --for openai --agent omp --model llama-3",
		},
		Run: func(c *Context) error { return runLLMConfig(c) },
	})
}

func llmServices(c *control.Client, device string) ([]llmproxy.Listing, error) {
	path := llmproxy.ServicesPath
	if device != "" {
		path += "?device=" + url.QueryEscape(device)
	}
	var rows []llmproxy.Listing
	err := c.Do(context.Background(), http.MethodGet, path, nil, &rows)
	return rows, err
}

func runLLMList(c *Context) error {
	cl, _, err := c.Node()
	if err != nil {
		return err
	}
	rows, err := llmServices(cl, "")
	if err != nil {
		return err
	}
	if rows == nil {
		rows = []llmproxy.Listing{}
	}
	var b strings.Builder
	tw := tabwriter.NewWriter(&b, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "DEVICE\tSERVICE\tSTATUS\tBASE URL")
	found := false
	for _, r := range rows {
		if r.Service == "" {
			fmt.Fprintf(tw, "%s\t-\tnot reachable: %s\t\n", r.Handle, oneLine(r.Error))
			continue
		}
		found = true
		status := "down"
		if r.Up {
			status = "up"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", r.Handle, r.Service, status, r.BaseURL)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if !found {
		b.WriteString("\nNo OpenAI-compatible services found. Register one on the device that runs it:\n" +
			"  messh service add unsloth http://127.0.0.1:8888 --kind openai --bearer sk-...\n")
		return c.Emit(rows, func(w io.Writer) { io.WriteString(w, b.String()) })
	}
	b.WriteString("\nUse the base URL with the agent's messh token as API key; `messh llm config DEVICE SERVICE` prints ready-made config.\n")
	return c.Emit(rows, func(w io.Writer) { io.WriteString(w, b.String()) })
}

func runLLMConfig(c *Context) error {
	cl, paths, err := c.Node()
	if err != nil {
		return err
	}
	device, service := c.Args[0], c.Args[1]
	harness := c.String("for")
	agent := c.String("agent")
	models := c.List("model")
	timeout := c.Duration("timeout")

	switch harness {
	case "omp", "pi":
		if agent == "" {
			agent = harness
		}
	case "openai":
		if agent == "" {
			return errors.New("--for openai needs --agent NAME (the agent whose token the client will use)")
		}
	default:
		return fmt.Errorf("--for must be omp, pi or openai, not %q", harness)
	}
	if _, err := paths.AgentToken(agent); err != nil {
		return fmt.Errorf("agent %q is not registered on this device: run `messh agent add %s` first", agent, agent)
	}

	rows, err := llmServices(cl, device)
	if err != nil {
		return err
	}
	var svc *llmproxy.Listing
	var have []string
	for i, r := range rows {
		if r.Service == "" && r.Error != "" {
			return fmt.Errorf("cannot ask %s for its services: %s", r.Device, r.Error)
		}
		if r.Service == service {
			svc = &rows[i]
		}
		have = append(have, r.Service)
	}
	if svc == nil {
		if len(have) == 0 {
			return fmt.Errorf("%s has no OpenAI-compatible services (register one there with `messh service add NAME URL --kind openai`)", device)
		}
		return fmt.Errorf("%s has no OpenAI-compatible service %q (it has: %s)", device, service, strings.Join(have, ", "))
	}

	var modelErr error
	if len(models) == 0 {
		fmt.Fprintf(c.Stderr, "Asking %s for the models of %s (if %s shows an approval prompt, answer it there)...\n", svc.Device, svc.Service, svc.Device)
		models, modelErr = fetchModels(svc.BaseURL, cl.Token, timeout)
		if modelErr != nil {
			fmt.Fprintf(c.Stderr, "Could not list the models: %v\n", modelErr)
		}
	}
	if models == nil {
		models = []string{}
	}

	stateDir := ""
	if c.Set("state") {
		stateDir = paths.Root
	}
	tokenCmd := tokenCommand(agent, false, stateDir)
	provider := "messh-" + svc.Handle + "-" + svc.Service
	wait := fmt.Sprintf("Each request waits until the owner of %s approves it (up to 10 minutes); pick \"Always allow\" with "+
		"\"this model\" or \"any model on %s\" so later requests start at once. A client that times out and retries asks again.",
		svc.Device, svc.Service)

	var human strings.Builder
	var cfgVal any
	switch harness {
	case "omp":
		yml := ompModelsYAML(provider, svc.BaseURL, tokenCmd, models)
		fmt.Fprint(&human, yml)
		fmt.Fprintf(&human, "\nMerge this into ~/.omp/agent/models.yml (under the existing \"providers:\" key, if any). The \"!\" makes omp\n"+
			"run the command for the key, so the token is never stored in the file. Select a model with /model or\n"+
			"--model %s/<id>.\n%s\n", provider, wait)
		if len(models) == 0 {
			fmt.Fprintf(&human, "The model list could not be fetched, so omp discovers it from %s/models on start; that request needs\n"+
				"approval too unless %s's entry sets auto.read. Rerun with --model ID to list models explicitly.\n", svc.BaseURL, svc.Service)
		}
		cfgVal = yml
	case "pi":
		if len(models) == 0 {
			return fmt.Errorf("pi needs the model IDs: rerun with --model ID for each model (%v)", modelErr)
		}
		out, err := piModelsJSON(provider, svc.BaseURL, tokenCmd, models)
		if err != nil {
			return err
		}
		fmt.Fprintf(&human, "%s\n\nMerge this into ~/.pi/agent/models.json (under \"providers\"). The \"!\" makes pi run the command for\n"+
			"the key at request time. compat turns off the developer role and reasoning_effort, which local servers\n"+
			"often reject; remove it if %s supports them.\n%s\n", out, svc.Service, wait)
		var obj any
		if err := json.Unmarshal([]byte(out), &obj); err != nil {
			return err
		}
		cfgVal = obj
	case "openai":
		fmt.Fprintf(&human, "Base URL  %s\nAPI key   the output of `%s` (sent as Authorization: Bearer; x-api-key works too)\n", svc.BaseURL, tokenCmd)
		if len(models) > 0 {
			fmt.Fprintf(&human, "Models    %s\n", strings.Join(models, ", "))
		}
		fmt.Fprintf(&human, "\nFor the OpenAI SDKs and most tools:\n  export OPENAI_BASE_URL=%s\n  export OPENAI_API_KEY=\"$(%s)\"\n"+
			"PowerShell:\n  $env:OPENAI_BASE_URL = '%s'\n  $env:OPENAI_API_KEY = (%s)\n"+
			"Anthropic-style clients: base URL %s (POST /v1/messages), same key as x-api-key.\n\n%s\n"+
			"Keep client timeouts at 10 minutes or more while prompts are pending (the OpenAI SDKs default to 10 minutes).\n",
			svc.BaseURL, tokenCmd, svc.BaseURL, tokenCmd, strings.TrimSuffix(svc.BaseURL, "/v1"), wait)
		cfgVal = human.String()
	}
	return c.Emit(map[string]any{
		"for":             harness,
		"provider":        provider,
		"base_url":        svc.BaseURL,
		"api_key_command": tokenCmd,
		"models":          models,
		"config":          cfgVal,
	}, func(w io.Writer) {
		io.WriteString(w, human.String())
	})
}

// fetchModels asks the service for its model IDs through this node's proxy,
// as the CLI agent.
func fetchModels(baseURL, token string, timeout time.Duration) ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/models", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, fmt.Errorf("no answer within %s (an approval may still be pending; --timeout waits longer)", timeout)
		}
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(data, &e) == nil && e.Error.Message != "" {
			return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, e.Error.Message)
		}
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, oneLine(string(data)))
	}
	var list struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, fmt.Errorf("unexpected /v1/models reply: %v", err)
	}
	var ids []string
	for _, m := range list.Data {
		if m.ID != "" {
			ids = append(ids, m.ID)
		}
	}
	if len(ids) == 0 {
		return nil, errors.New("the service lists no models")
	}
	return ids, nil
}

// ompModelsYAML renders an omp models.yml provider (docs: models.md, "Command-resolved secrets").
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

// yamlString quotes s as a YAML double-quoted scalar (JSON strings are valid ones).
func yamlString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// piModelsJSON renders a pi models.json provider (pi docs/models.md).
func piModelsJSON(provider, baseURL, tokenCmd string, models []string) (string, error) {
	type model struct {
		ID string `json:"id"`
	}
	type compat struct {
		SupportsDeveloperRole   bool `json:"supportsDeveloperRole"`
		SupportsReasoningEffort bool `json:"supportsReasoningEffort"`
	}
	type prov struct {
		BaseURL string  `json:"baseUrl"`
		API     string  `json:"api"`
		APIKey  string  `json:"apiKey"`
		Compat  compat  `json:"compat"`
		Models  []model `json:"models"`
	}
	p := prov{BaseURL: baseURL, API: "openai-completions", APIKey: "!" + tokenCmd}
	for _, m := range models {
		p.Models = append(p.Models, model{ID: m})
	}
	out, err := json.MarshalIndent(map[string]map[string]prov{"providers": {provider: p}}, "", "  ")
	return string(out), err
}
