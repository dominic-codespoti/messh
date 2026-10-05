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
		Summary: "print OpenAI-compatible config for SERVICE on DEVICE",
		Flags: func(fs *flag.FlagSet) {
			fs.String("agent", "", "required; agent whose messh token is the API key")
			listFlag(fs, "model", "model `ID` to configure (repeatable; default: ask the service)")
			fs.Duration("timeout", 30*time.Second, "how long to wait for the model list (`DURATION`; the owner may have to approve it)")
		},
		Output: `{provider, base_url, api_key_command, models, config}`,
		Person: "the owner of DEVICE may have to approve listing models",
		Waits:  "up to --timeout for the service's model list when --model is not given",
		Examples: []string{
			"messh llm config desktop unsloth --agent model-client --model llama-3",
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
	agent := c.String("agent")
	if !c.Set("agent") || agent == "" {
		return usageErrorf("--agent NAME is required (the agent whose token the client will use)")
	}
	models := c.List("model")
	timeout := c.Duration("timeout")

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

	var text strings.Builder
	fmt.Fprintf(&text, "Base URL  %s\nAPI key   the output of %s (sent as Authorization: Bearer; x-api-key works too)\n", svc.BaseURL, tokenCmd)
	if len(models) > 0 {
		fmt.Fprintf(&text, "Models    %s\n", strings.Join(models, ", "))
	}
	fmt.Fprintf(&text, "\nFor the OpenAI SDKs and most tools:\n  export OPENAI_BASE_URL=%s\n  export OPENAI_API_KEY=\"$(%s)\"\n"+
		"PowerShell:\n  $env:OPENAI_BASE_URL = '%s'\n  $env:OPENAI_API_KEY = (%s)\n"+
		"Anthropic-style clients: base URL %s (POST /v1/messages), same key as x-api-key.\n\n%s\n"+
		"Keep client timeouts at 10 minutes or more while prompts are pending (the OpenAI SDKs default to 10 minutes).\n",
		svc.BaseURL, tokenCmd, svc.BaseURL, tokenCmd, strings.TrimSuffix(svc.BaseURL, "/v1"), wait)
	return c.Emit(map[string]any{
		"provider":        provider,
		"base_url":        svc.BaseURL,
		"api_key_command": tokenCmd,
		"models":          models,
		"config":          text.String(),
	}, func(w io.Writer) {
		io.WriteString(w, text.String())
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
