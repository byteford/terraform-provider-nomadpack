// Package nomadpack wraps the nomad-pack CLI binary so the Terraform
// provider can drive pack lifecycle (plan/run/destroy) the same way an
// operator would from the command line. nomad-pack has no public,
// documented Go SDK, so shelling out to the binary is the deliberately
// chosen, stable integration point rather than importing its internal
// packages.
package nomadpack

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/hashicorp/terraform-plugin-log/tflog"
)

// Exit codes documented at:
// https://developer.hashicorp.com/nomad/tools/nomad-pack/commands/plan
const (
	planExitNoChanges    = 0
	planExitMakesChanges = 1
	planExitError        = 255
)

// CLIError is returned whenever a nomad-pack invocation exits non-zero (or,
// for Plan, with an unexpected exit code). It carries the raw captured
// output so callers can decide how to present it, instead of every call
// site re-formatting stdout/stderr by hand — which previously led to the
// same message being shown to the user two or three times over.
type CLIError struct {
	Op       string // "plan", "run", or "destroy"
	Args     []string
	ExitCode int
	Stdout   string
	Stderr   string
}

func (e *CLIError) Error() string {
	return fmt.Sprintf("nomad-pack %s exited %d: %s", e.Op, e.ExitCode, e.Output())
}

// Output returns the single most relevant captured stream for display.
// nomad-pack often writes its formatted error boxes to stdout rather than
// stderr, so stderr only wins here when it actually has content.
func (e *CLIError) Output() string {
	return strings.TrimSpace(firstNonEmpty(e.Stderr, e.Stdout))
}

// ClusterConfig holds Nomad connection details shared by every nomad-pack
// invocation. Empty fields are omitted from the command line, letting
// nomad-pack fall back to its own NOMAD_* environment variable handling.
type ClusterConfig struct {
	Address       string
	Namespace     string
	Region        string
	Token         string
	CACert        string
	ClientCert    string
	ClientKey     string
	TLSServerName string
	TLSSkipVerify bool
}

// Client executes nomad-pack subcommands.
type Client struct {
	BinaryPath string
	Cluster    ClusterConfig
}

// PackRef identifies which pack to operate on.
type PackRef struct {
	// Pack is the pack name, or a filesystem path to a pack under
	// development (per nomad-pack's own convention of accepting a
	// directory in place of a name).
	Pack     string
	Registry string
	Ref      string
}

// Deployment describes a single named instance of a rendered pack.
type Deployment struct {
	Name     string
	VarFiles []string
	Vars     map[string]string
}

// Result captures the outcome of a single CLI invocation for
// troubleshooting and for surfacing in Terraform diagnostics.
type Result struct {
	Args     []string
	Stdout   string
	Stderr   string
	ExitCode int
}

func (c *Client) binary() string {
	if c.BinaryPath == "" {
		return "nomad-pack"
	}
	return c.BinaryPath
}

func (c *Client) clusterArgs() []string {
	var args []string
	if c.Cluster.Address != "" {
		args = append(args, "--address="+c.Cluster.Address)
	}
	if c.Cluster.Namespace != "" {
		args = append(args, "--namespace="+c.Cluster.Namespace)
	}
	if c.Cluster.Region != "" {
		args = append(args, "--region="+c.Cluster.Region)
	}
	if c.Cluster.Token != "" {
		args = append(args, "--token="+c.Cluster.Token)
	}
	if c.Cluster.CACert != "" {
		args = append(args, "--ca-cert="+c.Cluster.CACert)
	}
	if c.Cluster.ClientCert != "" {
		args = append(args, "--client-cert="+c.Cluster.ClientCert)
	}
	if c.Cluster.ClientKey != "" {
		args = append(args, "--client-key="+c.Cluster.ClientKey)
	}
	if c.Cluster.TLSServerName != "" {
		args = append(args, "--tls-server-name="+c.Cluster.TLSServerName)
	}
	if c.Cluster.TLSSkipVerify {
		args = append(args, "--tls-skip-verify")
	}
	return args
}

func packArgs(pack PackRef) []string {
	var args []string
	if pack.Registry != "" {
		args = append(args, "--registry="+pack.Registry)
	}
	if pack.Ref != "" {
		args = append(args, "--ref="+pack.Ref)
	}
	return args
}

func deploymentArgs(d Deployment) []string {
	var args []string
	if d.Name != "" {
		args = append(args, "--name="+d.Name)
	}
	for _, f := range d.VarFiles {
		args = append(args, "--var-file="+f)
	}
	// Sorted iteration isn't required for correctness, but keeps the
	// resulting command line (and thus any logs) deterministic across
	// runs, which makes diffing plan output easier.
	keys := make([]string, 0, len(d.Vars))
	for k := range d.Vars {
		keys = append(keys, k)
	}
	sortStrings(keys)
	for _, k := range keys {
		args = append(args, fmt.Sprintf("--var=%s=%s", k, d.Vars[k]))
	}
	return args
}

// sortStrings avoids pulling in "sort" just for this; kept local and tiny.
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}

func (c *Client) exec(ctx context.Context, args []string) (*Result, error) {
	tflog.Debug(ctx, "executing nomad-pack", map[string]interface{}{
		"binary": c.binary(),
		"args":   redactArgs(args),
	})

	cmd := exec.CommandContext(ctx, c.binary(), args...)
	// nomad-pack's output rendering (go-glint) can emit ANSI color codes
	// if it misdetects a TTY. Force it off so error text stays clean when
	// surfaced through Terraform diagnostics.
	cmd.Env = append(os.Environ(), "NO_COLOR=1")

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	runErr := cmd.Run()

	res := &Result{
		Args:   args,
		Stdout: stdout.String(),
		Stderr: stderr.String(),
	}

	if runErr == nil {
		res.ExitCode = 0
		return res, nil
	}

	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) {
		res.ExitCode = exitErr.ExitCode()
		return res, nil
	}

	// Couldn't even start the process (binary missing, permissions, etc).
	return res, fmt.Errorf("failed to execute %q: %w", c.binary(), runErr)
}

// Plan runs `nomad-pack plan` as a read-only dry run. It never mutates the
// cluster. hasChanges reflects nomad-pack's own exit-code contract: 0 = in
// sync, 1 = changes pending. Any other exit code (255, or a documented
// override via -exit-code-error) is treated as a real error.
func (c *Client) Plan(ctx context.Context, pack PackRef, d Deployment) (hasChanges bool, diff string, res *Result, err error) {
	args := []string{"plan", pack.Pack, "--diff"}
	args = append(args, packArgs(pack)...)
	args = append(args, deploymentArgs(d)...)
	args = append(args, c.clusterArgs()...)

	res, err = c.exec(ctx, args)
	if err != nil {
		return false, "", res, err
	}

	switch res.ExitCode {
	case planExitNoChanges:
		return false, res.Stdout, res, nil
	case planExitMakesChanges:
		return true, res.Stdout, res, nil
	default:
		return false, res.Stdout, res, &CLIError{
			Op: "plan", Args: args, ExitCode: res.ExitCode,
			Stdout: res.Stdout, Stderr: res.Stderr,
		}
	}
}

// Run submits the pack via `nomad-pack run`, creating or updating every job
// it contains. detach returns immediately instead of waiting for the
// deployment/evaluation to finish, which matters for long Terraform applies
// against slow-to-heal jobs.
func (c *Client) Run(ctx context.Context, pack PackRef, d Deployment, detach bool) (*Result, error) {
	args := []string{"run", pack.Pack}
	args = append(args, packArgs(pack)...)
	args = append(args, deploymentArgs(d)...)
	args = append(args, c.clusterArgs()...)
	if detach {
		args = append(args, "--detach")
	}

	res, err := c.exec(ctx, args)
	if err != nil {
		return res, err
	}
	if res.ExitCode != 0 {
		return res, &CLIError{
			Op: "run", Args: args, ExitCode: res.ExitCode,
			Stdout: res.Stdout, Stderr: res.Stderr,
		}
	}
	return res, nil
}

// Destroy stops and purges every job nomad-pack associates with this named
// deployment, via `nomad-pack destroy` (equivalent to `stop --purge`).
func (c *Client) Destroy(ctx context.Context, pack PackRef, d Deployment, detach bool) (*Result, error) {
	args := []string{"destroy", pack.Pack}
	args = append(args, packArgs(pack)...)
	args = append(args, deploymentArgs(d)...)
	args = append(args, c.clusterArgs()...)
	if detach {
		args = append(args, "--detach")
	}

	res, err := c.exec(ctx, args)
	if err != nil {
		return res, err
	}
	if res.ExitCode != 0 {
		// Deleting something already gone shouldn't fail a Terraform
		// destroy. nomad-pack doesn't document a dedicated "not found"
		// exit code, so this matches on stderr text as a best effort.
		if strings.Contains(strings.ToLower(res.Stderr), "not found") ||
			strings.Contains(strings.ToLower(res.Stdout), "not found") {
			return res, nil
		}
		return res, &CLIError{
			Op: "destroy", Args: args, ExitCode: res.ExitCode,
			Stdout: res.Stdout, Stderr: res.Stderr,
		}
	}
	return res, nil
}

// redactArgs masks flag values that shouldn't end up in debug logs (which
// people routinely paste into GitHub issues or CI output) even though
// they're safe to pass on a local command line.
func redactArgs(args []string) []string {
	redacted := make([]string, len(args))
	for i, a := range args {
		switch {
		case strings.HasPrefix(a, "--token="):
			redacted[i] = "--token=<redacted>"
		case strings.HasPrefix(a, "--client-key="):
			redacted[i] = "--client-key=<redacted>"
		default:
			redacted[i] = a
		}
	}
	return redacted
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return "(no output)"
}
