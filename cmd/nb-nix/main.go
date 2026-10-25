// Command nb-nix plans and applies declarative NetBird management settings.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"

	"github.com/jeiang/nb-nix/internal/engine"
	"github.com/jeiang/nb-nix/internal/nbapi"
)

const usage = `nb-nix: declarative NetBird management settings

Usage:
  nb-nix status  [--server URL]
  nb-nix import  [--server URL] [-o FILE]
  nb-nix plan    -c DESIRED.json [-o PLAN.json] [--server URL] [--detailed-exitcode]
  nb-nix apply   PLAN.json | -c DESIRED.json  [--allow-risky] [--rotate KIND/NAME]...

The API token comes from $NETBIRD_API_TOKEN or --token-file. The server
defaults to $NETBIRD_SERVER, then the "server" of the desired state.
Every command except --help refuses to talk to a server that does not run
the pinned NetBird version unless --ignore-version is given.
`

// exitChanges is the plan exit code for "changes present" with --detailed-exitcode.
const exitChanges = 2

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	code, err := run(ctx, os.Args[1:], os.Stdout)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}

type common struct {
	server        string
	tokenFile     string
	ignoreVersion bool
}

func (c *common) register(fs *flag.FlagSet) {
	fs.StringVar(&c.server, "server", os.Getenv("NETBIRD_SERVER"), "management server URL")
	fs.StringVar(&c.tokenFile, "token-file", "", "file with the API token (default $NETBIRD_API_TOKEN)")
	fs.BoolVar(&c.ignoreVersion, "ignore-version", false, "talk to a server that runs another NetBird version")
}

func (c *common) client(ctx context.Context, fallbackServer string) (*nbapi.Client, string, error) {
	server := c.server
	if server == "" {
		server = fallbackServer
	}
	if server == "" {
		return nil, "", errors.New("no server: set --server, $NETBIRD_SERVER or netbird.server")
	}
	token := os.Getenv("NETBIRD_API_TOKEN")
	if c.tokenFile != "" {
		b, err := os.ReadFile(c.tokenFile)
		if err != nil {
			return nil, "", fmt.Errorf("read token: %w", err)
		}
		token = strings.TrimSpace(string(b))
	}
	if token == "" {
		return nil, "", errors.New("no API token: set $NETBIRD_API_TOKEN or --token-file")
	}
	cl := nbapi.New(server, token)
	if !c.ignoreVersion {
		if err := cl.CheckVersion(ctx); err != nil {
			return nil, "", err
		}
	}
	return cl, server, nil
}

type rotateFlag map[string]bool

func (r rotateFlag) String() string { return "" }
func (r rotateFlag) Set(v string) error {
	if !strings.Contains(v, "/") {
		return fmt.Errorf("want KIND/NAME, got %q", v)
	}
	r[v] = true
	return nil
}

func run(ctx context.Context, args []string, stdout io.Writer) (int, error) {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fmt.Fprint(stdout, usage)
		return 0, nil
	}
	cmd, args := args[0], args[1:]
	fs := flag.NewFlagSet("nb-nix "+cmd, flag.ContinueOnError)
	var c common
	c.register(fs)
	switch cmd {
	case "status":
		if err := fs.Parse(args); err != nil {
			return 2, err
		}
		return 0, status(ctx, &c, stdout)
	case "import":
		out := fs.String("o", "", "write the Nix module here instead of stdout")
		if err := fs.Parse(args); err != nil {
			return 2, err
		}
		return 0, importCmd(ctx, &c, *out, stdout)
	case "plan":
		desired := fs.String("c", "", "desired-state JSON (from the Nix module)")
		out := fs.String("o", "", "write the plan file here")
		detailed := fs.Bool("detailed-exitcode", false, "exit 2 when the plan has changes")
		if err := fs.Parse(args); err != nil {
			return 2, err
		}
		p, err := makePlan(ctx, &c, *desired)
		if err != nil {
			return 1, err
		}
		p.Render(stdout)
		if *out != "" {
			if err := writePlan(*out, p); err != nil {
				return 1, err
			}
		}
		if *detailed && len(p.Ops) > 0 {
			return exitChanges, nil
		}
		return 0, nil
	case "apply":
		desired := fs.String("c", "", "plan and apply this desired-state JSON in one step")
		allowRisky := fs.Bool("allow-risky", false, "make risky changes (deletes, replacements, address changes)")
		rotate := rotateFlag{}
		fs.Var(rotate, "rotate", "send the secrets of KIND/NAME even if unchanged (repeatable)")
		if err := fs.Parse(args); err != nil {
			return 2, err
		}
		var p *engine.Plan
		var err error
		switch {
		case *desired != "" && fs.NArg() == 0:
			p, err = makePlan(ctx, &c, *desired)
			if err == nil {
				p.Render(stdout)
			}
		case *desired == "" && fs.NArg() == 1:
			p, err = readPlan(fs.Arg(0))
		default:
			return 2, errors.New("apply takes a plan file or -c DESIRED.json")
		}
		if err != nil {
			return 1, err
		}
		cl, _, err := c.client(ctx, p.Server)
		if err != nil {
			return 1, err
		}
		if len(p.Ops) == 0 {
			fmt.Fprintln(stdout, "No changes.")
			return 0, nil
		}
		return 0, engine.Apply(ctx, cl, p, engine.ApplyOptions{AllowRisky: *allowRisky, Rotate: rotate, Log: stdout})
	}
	return 2, fmt.Errorf("unknown command %q\n\n%s", cmd, usage)
}

func status(ctx context.Context, c *common, w io.Writer) error {
	pinned := nbapi.PinnedVersion
	cl, server, err := c.client(ctx, "")
	if err != nil {
		return err
	}
	got, err := cl.ServerVersion(ctx)
	if err != nil {
		return err
	}
	var accounts []struct {
		ID string `json:"id"`
	}
	if err := cl.Do(ctx, "GET", "/accounts", nil, &accounts); err != nil {
		return err
	}
	fmt.Fprintf(w, "server:  %s\nnetbird: %s (nb-nix built for %s)\n", server, got, pinned)
	for _, a := range accounts {
		fmt.Fprintf(w, "account: %s\n", a.ID)
	}
	return nil
}

func importCmd(ctx context.Context, c *common, out string, stdout io.Writer) error {
	cl, server, err := c.client(ctx, "")
	if err != nil {
		return err
	}
	snap, err := engine.Fetch(ctx, cl)
	if err != nil {
		return err
	}
	version, err := cl.ServerVersion(ctx)
	if err != nil {
		return err
	}
	w := stdout
	if out != "" {
		f, err := os.Create(out)
		if err != nil {
			return err
		}
		defer f.Close()
		w = f
	}
	return engine.Import(w, snap, server, version)
}

func makePlan(ctx context.Context, c *common, desiredPath string) (*engine.Plan, error) {
	if desiredPath == "" {
		return nil, errors.New("-c DESIRED.json is required")
	}
	data, err := os.ReadFile(desiredPath)
	if err != nil {
		return nil, err
	}
	want, err := engine.ParseDesired(data)
	if err != nil {
		return nil, err
	}
	cl, server, err := c.client(ctx, want.Server)
	if err != nil {
		return nil, err
	}
	snap, err := engine.Fetch(ctx, cl)
	if err != nil {
		return nil, err
	}
	p, err := engine.MakePlan(snap, want)
	if err != nil {
		return nil, err
	}
	p.Server = server
	p.Version, _ = cl.ServerVersion(ctx)
	return p, nil
}

func writePlan(path string, p *engine.Plan) error {
	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o600)
}

func readPlan(path string) (*engine.Plan, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var p engine.Plan
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.UseNumber()
	if err := dec.Decode(&p); err != nil {
		return nil, fmt.Errorf("parse plan: %w", err)
	}
	return &p, nil
}
