package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/url"
	"slices"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"messh/internal/catalog"
)

func init() {
	add(&Command{
		Name:    "service add",
		Args:    []Arg{{Name: "NAME", Help: "service name (1-24 of a-z 0-9 _ -)"}, {Name: "URL", Help: "base URL of the service"}},
		Summary: "register a local service for other devices to use (needs approval per call)",
		Flags: func(fs *flag.FlagSet) {
			choiceFlag(fs, "kind", "auto", "detected kind or explicit kind in `KIND`", "auto", "mcp", "openapi", "openai", "http")
			fs.String("desc", "", "one-line description shown to agents in `TEXT`")
			fs.String("header", "", "credential header as `NAME: value`")
			fs.String("bearer", "", "bearer token as `TOKEN` (sent as Authorization: Bearer ...)")
			fs.String("header-file", "", "credential header read from a file as `NAME: PATH`")
			fs.Bool("auto-read", false, "run read-only operations without asking")
			fs.String("health", "", "health check path or URL in `PATH`")
			fs.Bool("allow-lan", false, "permit a non-loopback (private network) host")
			fs.Bool("force", false, "register even when the service does not answer now")
			listFlag(fs, "tag", "tag in `T` (repeatable)")
			listFlag(fs, "auto-tool", "operation to run without asking: MCP tool name or 'METHOD /path' in `OP` (repeatable)")
			listFlag(fs, "allow", "only permit this operation in `OP` (repeatable)")
			listFlag(fs, "deny", "never permit this operation in `OP` (repeatable)")
		},
		Output:  `{"name","kind","url","detected[...]"}: what was registered and which interfaces answered`,
		Mutates: true,
		Examples: []string{
			"messh service add voicestudio http://127.0.0.1:3900/mcp/ --desc voice",
			"messh service add unsloth http://127.0.0.1:8888 --kind openai --bearer sk-...",
		},
		Run: func(c *Context) error {
			paths, err := c.Paths()
			if err != nil {
				return err
			}
			auth, err := authFromFlags(c.String("header"), c.String("bearer"), c.String("header-file"))
			if err != nil {
				return err
			}
			svc := catalog.Service{
				Name: c.Args[0], URL: c.Args[1], Description: c.String("desc"), Tags: c.List("tag"),
				Auth: auth, Health: c.String("health"), AllowLAN: c.Bool("allow-lan"),
				Allow: c.List("allow"), Deny: c.List("deny"),
				Auto: catalog.Auto{Read: c.Bool("auto-read"), Tools: c.List("auto-tool")},
			}
			return serviceAddRun(c, paths.Root, paths.ServicesFile(), svc, c.String("kind"), c.Bool("force"))
		},
	})
	add(&Command{
		Name:    "service ls",
		Summary: "list registered services with up/down status",
		Output:  `[{name, kind, url, enabled, up, latency_ms, info}] (never credentials; times are milliseconds)`,
		Examples: []string{
			"messh service ls",
			"messh service ls --json",
		},
		Run: func(c *Context) error {
			paths, err := c.Paths()
			if err != nil {
				return err
			}
			return serviceListRun(c, paths.Root, paths.ServicesFile())
		},
	})
	add(&Command{
		Name:    "service show",
		Args:    []Arg{{Name: "NAME", Help: "registered service name"}},
		Summary: "inspect one registered service (credentials stay hidden)",
		Output:  `{"name","kind","url","description","tags[]","enabled","up","latency_ms","info",...} (never credentials)`,
		Examples: []string{
			"messh service show voicestudio",
		},
		Run: func(c *Context) error {
			paths, err := c.Paths()
			if err != nil {
				return err
			}
			return serviceShowRun(c, paths.Root, paths.ServicesFile(), c.Args[0])
		},
	})
	add(&Command{
		Name:    "service rm",
		Args:    []Arg{{Name: "NAME", Help: "registered service name"}},
		Summary: "remove a registered service",
		Output:  `{"name","removed":true}`,
		Mutates: true,
		Examples: []string{
			"messh service rm voicestudio",
		},
		Run: func(c *Context) error {
			paths, err := c.Paths()
			if err != nil {
				return err
			}
			file := paths.ServicesFile()
			sf, err := catalog.LoadFile(file)
			if err != nil {
				return err
			}
			name := c.Args[0]
			i := sf.Find(name)
			if i < 0 {
				return fmt.Errorf("no service named %q", name)
			}
			sf.Services = slices.Delete(sf.Services, i, i+1)
			if err := catalog.SaveFile(file, sf); err != nil {
				return err
			}
			return c.Emit(map[string]any{"name": name, "removed": true}, func(w io.Writer) {
				fmt.Fprintf(w, "Removed service %q. A running node notices within a couple of seconds.\n", name)
			})
		},
	})
	add(&Command{
		Name:    "service scan",
		Summary: "look for well-known local services and suggest 'service add' commands (changes nothing)",
		Output:  `[{port, kind, info, suggested_command}] (kind is "" when nothing usable answered)`,
		Examples: []string{
			"messh service scan",
			"messh service scan --json",
		},
		Run: func(c *Context) error {
			paths, err := c.Paths()
			if err != nil {
				return err
			}
			return serviceScanRun(c, paths.Root, paths.ServicesFile())
		},
	})
}

func authFromFlags(header, bearer, headerFile string) (*catalog.Auth, error) {
	n := 0
	for _, s := range []string{header, bearer, headerFile} {
		if s != "" {
			n++
		}
	}
	switch {
	case n == 0:
		return nil, nil
	case n > 1:
		return nil, errors.New("use only one of --header, --bearer, --header-file")
	case bearer != "":
		return &catalog.Auth{Header: "Authorization", Value: "Bearer " + bearer}, nil
	case header != "":
		name, value, ok := strings.Cut(header, ":")
		if !ok || strings.TrimSpace(name) == "" || strings.TrimSpace(value) == "" {
			return nil, errors.New("--header wants 'Name: value'")
		}
		return &catalog.Auth{Header: strings.TrimSpace(name), Value: strings.TrimSpace(value)}, nil
	}
	name, p, ok := strings.Cut(headerFile, ":")
	if !ok || strings.TrimSpace(name) == "" || strings.TrimSpace(p) == "" {
		return nil, errors.New("--header-file wants 'Name: PATH'")
	}
	return &catalog.Auth{Header: strings.TrimSpace(name), ValueFile: strings.TrimSpace(p)}, nil
}

type serviceAddResult struct {
	Name     string   `json:"name"`
	Kind     string   `json:"kind"`
	URL      string   `json:"url"`
	Detected []string `json:"detected"`
}

func serviceAddRun(c *Context, root, file string, svc catalog.Service, kind string, force bool) error {
	sf, err := catalog.LoadFile(file)
	if err != nil {
		return err
	}
	if sf.Find(svc.Name) >= 0 {
		return fmt.Errorf("service %q already exists; remove it first with: messh service rm %s", svc.Name, svc.Name)
	}
	// Validate the shape before touching the network.
	probeSvc := svc
	probeSvc.Kind = catalog.KindHTTP
	if err := probeSvc.Validate(); err != nil {
		return err
	}
	say := func(format string, a ...any) {
		if !c.JSON {
			fmt.Fprintf(c.Stdout, format+"\n", a...)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	detected := []string{}
	if kind == "auto" {
		d, err := catalog.Detect(ctx, svc, root)
		if err != nil {
			return fmt.Errorf("%w (start the service, or pass --kind to register it anyway with --force)", err)
		}
		detected = append(detected, d.Found...)
		say("Found:")
		for _, line := range d.Found {
			say("  - %s", line)
		}
		svc.Kind, svc.URL = d.Kind, d.URL
		for _, alt := range d.Alternatives {
			say("  (also available as %s: messh service add OTHERNAME %s --kind %s)", alt.Kind, alt.URL, alt.Kind)
		}
	} else {
		svc.Kind = kind
		if err := svc.Validate(); err != nil {
			return err
		}
		st := catalog.ProbeOnce(ctx, svc, root, 8*time.Second)
		if !st.Up {
			if !force {
				return fmt.Errorf("the service did not answer as %s: %s (use --force to register anyway)", kind, st.Error)
			}
			say("Warning: not answering as %s now (%s); registering anyway.", kind, st.Error)
		} else {
			say("Found %s service: %s", kind, st.Summary)
			if st.Summary != "" {
				detected = append(detected, st.Summary)
			}
		}
	}
	if err := svc.Validate(); err != nil {
		return err
	}
	if st := catalog.ProbeOnce(ctx, svc, root, 8*time.Second); !st.Up && kind == "auto" {
		return fmt.Errorf("detected %s but it does not pass a health probe: %s", svc.Kind, st.Error)
	}
	sf.Services = append(sf.Services, svc)
	if err := catalog.SaveFile(file, sf); err != nil {
		return err
	}
	res := serviceAddResult{Name: svc.Name, Kind: svc.Kind, URL: svc.URL, Detected: detected}
	return c.Emit(res, func(w io.Writer) {
		fmt.Fprintf(w, "Registered %q (%s). Every call from another device still asks you first", svc.Name, svc.Kind)
		if svc.Auto.Read || len(svc.Auto.Tools) > 0 {
			fmt.Fprint(w, " (except what you pre-authorised with --auto-read/--auto-tool)")
		}
		fmt.Fprintln(w, ".")
		if svc.Auth != nil {
			fmt.Fprintf(w, "The credential is stored in %s (owner-only) and is never shown to agents.\n", file)
		}
	})
}

type serviceLsEntry struct {
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	URL       string `json:"url"`
	Enabled   bool   `json:"enabled"`
	Up        bool   `json:"up"`
	LatencyMs int64  `json:"latency_ms"`
	Info      string `json:"info"`
}

func serviceListRun(c *Context, root, file string) error {
	sf, err := catalog.LoadFile(file)
	if err != nil {
		return err
	}
	out := []serviceLsEntry{}
	if len(sf.Services) == 0 {
		return c.Emit(out, func(w io.Writer) {
			fmt.Fprintln(w, "No services registered. Try: messh service scan")
		})
	}
	type row struct {
		svc catalog.Service
		st  catalog.Status
	}
	rows := make([]row, len(sf.Services))
	var wg sync.WaitGroup
	for i, s := range sf.Services {
		rows[i].svc = s
		if err := s.Validate(); err != nil {
			rows[i].st = catalog.Status{Error: "invalid: " + err.Error()}
			continue
		}
		if !s.Active() {
			rows[i].st = catalog.Status{Error: "disabled"}
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			rows[i].st = catalog.ProbeOnce(context.Background(), s, root, 5*time.Second)
		}()
	}
	wg.Wait()
	for _, r := range rows {
		e := serviceLsEntry{Name: r.svc.Name, Kind: r.svc.Kind, URL: r.svc.URL, Enabled: r.svc.Active(), Up: r.st.Up}
		if r.st.Up {
			e.LatencyMs = r.st.Latency.Milliseconds()
			e.Info = r.st.Summary
		} else {
			e.Info = r.st.Error
		}
		if e.Info == "" {
			e.Info = r.svc.Description
		}
		out = append(out, e)
	}
	return c.Emit(out, func(w io.Writer) {
		tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "NAME\tKIND\tSTATUS\tURL\tINFO")
		for _, e := range out {
			status := "down"
			if e.Up {
				status = fmt.Sprintf("up %dms", e.LatencyMs)
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", e.Name, e.Kind, status, e.URL, e.Info)
		}
		tw.Flush()
	})
}

type serviceShowResult struct {
	Name        string       `json:"name"`
	Kind        string       `json:"kind"`
	URL         string       `json:"url"`
	Description string       `json:"description,omitempty"`
	Tags        []string     `json:"tags,omitempty"`
	Health      string       `json:"health,omitempty"`
	AllowLAN    bool         `json:"allow_lan,omitempty"`
	Enabled     bool         `json:"enabled"`
	Auto        catalog.Auto `json:"auto,omitempty"`
	Allow       []string     `json:"allow,omitempty"`
	Deny        []string     `json:"deny,omitempty"`
	Up          bool         `json:"up"`
	LatencyMs   int64        `json:"latency_ms"`
	Info        string       `json:"info"`
}

func serviceShowRun(c *Context, root, file, name string) error {
	sf, err := catalog.LoadFile(file)
	if err != nil {
		return err
	}
	i := sf.Find(name)
	if i < 0 {
		return fmt.Errorf("no service named %q", name)
	}
	svc := sf.Services[i]
	res := serviceShowResult{
		Name: svc.Name, Kind: svc.Kind, URL: svc.URL, Description: svc.Description,
		Tags: svc.Tags, Health: svc.Health, AllowLAN: svc.AllowLAN, Enabled: svc.Active(),
		Auto: svc.Auto, Allow: svc.Allow, Deny: svc.Deny,
	}
	if err := svc.Validate(); err != nil {
		res.Info = "invalid: " + err.Error()
	} else if st := catalog.ProbeOnce(context.Background(), svc, root, 8*time.Second); st.Up {
		res.Up, res.LatencyMs, res.Info = true, st.Latency.Milliseconds(), st.Summary
	} else {
		res.Info = st.Error
	}
	return c.Emit(res, func(w io.Writer) {
		shown := svc
		if svc.Auth != nil {
			a := *svc.Auth
			if a.Value != "" {
				a.Value = "(set, hidden)"
			}
			shown.Auth = &a
		}
		out, _ := json.MarshalIndent(shown, "", "  ")
		fmt.Fprintln(w, string(out))
		if strings.HasPrefix(res.Info, "invalid: ") {
			fmt.Fprintln(w, "\nInvalid:", strings.TrimPrefix(res.Info, "invalid: "))
			return
		}
		if res.Up {
			fmt.Fprintf(w, "\nStatus: up (%dms) %s\n", res.LatencyMs, res.Info)
		} else {
			fmt.Fprintf(w, "\nStatus: down: %s\n", res.Info)
		}
	})
}

type serviceScanEntry struct {
	Port             string `json:"port"`
	Kind             string `json:"kind"`
	Info             string `json:"info"`
	SuggestedCommand string `json:"suggested_command"`
}

func serviceScanRun(c *Context, root, file string) error {
	sf, err := catalog.LoadFile(file)
	if err != nil {
		return err
	}
	registered := map[string]bool{}
	for _, s := range sf.Services {
		if u, err := url.Parse(s.URL); err == nil && (u.Hostname() == "127.0.0.1" || u.Hostname() == "localhost") {
			registered[u.Port()] = true
		}
	}
	type result struct {
		idx  int
		open bool
		d    *catalog.Detection
		err  error
	}
	results := make([]result, len(scanTargets))
	var wg sync.WaitGroup
	for i, t := range scanTargets {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i].idx = i
			conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", t.port), 400*time.Millisecond)
			if err != nil {
				return
			}
			conn.Close()
			results[i].open = true
			ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
			defer cancel()
			results[i].d, results[i].err = catalog.Detect(ctx, catalog.Service{Name: t.name, URL: fmt.Sprintf("http://127.0.0.1:%d", t.port)}, root)
		}()
	}
	wg.Wait()
	entries := []serviceScanEntry{}
	var lines []string
	found := 0
	for i, r := range results {
		t := scanTargets[i]
		if !r.open {
			continue
		}
		found++
		port := fmt.Sprint(t.port)
		lines = append(lines, fmt.Sprintf("127.0.0.1:%d  %s", t.port, t.note))
		if registered[port] {
			lines = append(lines, "  already registered")
		}
		d := r.d
		if r.err != nil && (d == nil || d.Kind == "") {
			if d != nil && d.AuthRequired {
				cmd := fmt.Sprintf("messh service add %s http://127.0.0.1:%d --bearer YOUR_KEY", t.name, t.port)
				entries = append(entries, serviceScanEntry{Port: port, Info: "needs credentials", SuggestedCommand: cmd})
				lines = append(lines, fmt.Sprintf("  needs credentials. Register with: %s", cmd))
			} else {
				entries = append(entries, serviceScanEntry{Port: port, Info: "no usable interface: " + r.err.Error()})
				lines = append(lines, fmt.Sprintf("  no usable interface: %v", r.err))
			}
			continue
		}
		for _, line := range d.Found {
			lines = append(lines, "  found: "+line)
		}
		name := t.name
		if d.Name != "" {
			if n := strings.Trim(strings.ToLower(d.Name), " "); n != "" {
				name = sanitizeScanName(n)
			}
		}
		cmd := fmt.Sprintf("messh service add %s %s --kind %s --desc %q", name, d.URL, d.Kind, t.note)
		entries = append(entries, serviceScanEntry{
			Port: port, Kind: string(d.Kind), Info: strings.Join(d.Found, "; "), SuggestedCommand: cmd,
		})
		lines = append(lines, "  suggest: "+cmd)
		for _, alt := range d.Alternatives {
			lines = append(lines, fmt.Sprintf("  or:      messh service add %s-%s %s --kind %s", name, alt.Kind, alt.URL, alt.Kind))
		}
	}
	if found == 0 {
		lines = append(lines, "No well-known local services answered on 127.0.0.1.")
	}
	lines = append(lines, "", "Nothing was changed. Add the ones other devices should be able to use; every call still asks you first.")
	return c.Emit(entries, func(w io.Writer) {
		for _, l := range lines {
			fmt.Fprintln(w, l)
		}
	})
}

// scanTargets are the loopback ports where well-known local services listen.
var scanTargets = []struct {
	port int
	name string
	note string
}{
	{3900, "voicestudio", "VoiceStudio backend"},
	{8888, "unsloth", "Unsloth Studio (needs its API key)"},
	{11434, "ollama", "Ollama"},
	{1234, "lmstudio", "LM Studio"},
	{9337, "meshllm", "Mesh LLM"},
	{8080, "http8080", "local web service"},
	{7860, "gradio", "Gradio app"},
}

func sanitizeScanName(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if len(out) > 24 {
		out = out[:24]
	}
	if out == "" {
		return "service"
	}
	return out
}
