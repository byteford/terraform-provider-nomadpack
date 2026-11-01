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
	"path/filepath"
	"regexp"
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

	// WorkingDir, if set, is used as the working directory for every
	// nomad-pack subprocess invocation. Left empty, the subprocess
	// inherits the provider process's own working directory — which
	// Terraform does not guarantee matches the directory you actually
	// run `terraform apply` from, since it launches provider plugin
	// binaries as separate subprocesses. Relative paths in `pack` or
	// `var_files` that look correct can still fail to resolve as a
	// result. Setting this explicitly (typically to `path.root` via the
	// provider's working_directory attribute) removes that ambiguity
	// entirely rather than relying on implicit inheritance.
	WorkingDir string

	// CacheDir, if set, is passed to every nomad-pack invocation as
	// XDG_CACHE_HOME, which moves nomad-pack's registry cache to
	// <CacheDir>/nomad/packs. nomad-pack records the path of the pack it
	// deployed in each job's pack.path meta, and a meta change restarts the
	// job, so the same CacheDir on every machine that runs Terraform keeps
	// that value (and the job) from changing when the machine does. Must be
	// absolute. Only honoured where nomad-pack reads XDG_CACHE_HOME; see
	// CacheDirHonoured.
	CacheDir string
}

// CacheDirHonoured reports whether nomad-pack on the given GOOS reads
// XDG_CACHE_HOME to place its cache. It follows os.UserCacheDir, which
// nomad-pack uses: macOS, iOS, Windows and Plan 9 use their own locations.
func CacheDirHonoured(goos string) bool {
	switch goos {
	case "darwin", "ios", "windows", "plan9":
		return false
	}
	return true
}

// PackRef identifies which pack to operate on.
type PackRef struct {
	// Pack is the pack name, or a filesystem path to a pack under
	// development (per nomad-pack's own convention of accepting a
	// directory in place of a name).
	Pack     string
	Registry string
	Ref      string

	// RegistrySource, if set, is the git URL of the registry. The registry
	// name is then derived from it (RegistryNameForSource, overriding
	// Registry) and the registry is added to nomad-pack's cache at Ref
	// before any command runs; see ensureRegistry. Ref must be pinned.
	RegistrySource string
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

// looksLikeFilesystemPath mirrors the same heuristic the Terraform
// resource layer uses (a path separator means "filesystem path", not a
// registry pack name) — kept as a small local duplicate rather than a
// cross-package dependency, since it's a one-line check.
func looksLikeFilesystemPath(s string) bool {
	return strings.ContainsAny(s, "/\\")
}

// resolvePath resolves a relative filesystem path to an absolute one,
// against WorkingDir if set, otherwise the provider process's own working
// directory. Non-path values (registry pack names) and already-absolute
// paths pass through unchanged.
//
// This matters beyond "does the file exist": nomad-pack appears to use
// the literal pack path string as part of a job's tracked identity, so a
// relative and absolute path pointing at the exact same directory can be
// treated as two different deployments — surfacing as errors like
// `job with id "x" already exists and is part of deployment "<other
// string>"` the moment the same directory gets referenced with a
// differently-spelled path than whatever was originally used for
// `nomad-pack run` (e.g. importing with a relative path when the
// original deploy used an absolute one). Always resolving to a single
// canonical absolute form before nomad-pack ever sees it removes that
// whole class of ambiguity, rather than requiring the person to
// remember and exactly match whatever spelling was used the first time.
func (c *Client) resolvePath(p string) string {
	if p == "" || !looksLikeFilesystemPath(p) || filepath.IsAbs(p) {
		return p
	}
	base := c.WorkingDir
	if base == "" {
		wd, err := os.Getwd()
		if err != nil {
			return p
		}
		base = wd
	}
	return filepath.Clean(filepath.Join(base, p))
}

// resolvedPackRef returns pack with Pack resolved via resolvePath.
func (c *Client) resolvedPackRef(pack PackRef) PackRef {
	pack.Pack = c.resolvePath(pack.Pack)
	return pack
}

// resolvedDeployment returns d with every VarFiles entry resolved via
// resolvePath. Vars (key=value overrides) are left untouched — a var's
// value might happen to look like a path, but the provider has no
// general way to know that's what it semantically represents.
func (c *Client) resolvedDeployment(d Deployment) Deployment {
	if len(d.VarFiles) == 0 {
		return d
	}
	resolved := make([]string, len(d.VarFiles))
	for i, f := range d.VarFiles {
		resolved[i] = c.resolvePath(f)
	}
	d.VarFiles = resolved
	return d
}

func (c *Client) exec(ctx context.Context, args []string) (*Result, error) {
	tflog.Debug(ctx, "executing nomad-pack", map[string]interface{}{
		"binary":      c.binary(),
		"args":        redactArgs(args),
		"working_dir": c.WorkingDir,
	})

	cmd := exec.CommandContext(ctx, c.binary(), args...)
	if c.WorkingDir != "" {
		cmd.Dir = c.WorkingDir
	}
	// nomad-pack's output rendering (go-glint) can emit ANSI color codes
	// if it misdetects a TTY. Force it off so error text stays clean when
	// surfaced through Terraform diagnostics.
	cmd.Env = append(os.Environ(), "NO_COLOR=1")
	if c.CacheDir != "" {
		// Later entries win, so this overrides an inherited XDG_CACHE_HOME.
		cmd.Env = append(cmd.Env, "XDG_CACHE_HOME="+c.CacheDir)
	}

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
// cluster. hasChanges primarily follows nomad-pack's own exit-code
// contract (0 = in sync, 1 = changes pending), but a 0 exit is not fully
// trusted on its own: nomad-pack has a known history of its reported exit
// code disagreeing with its own printed diff (see
// https://github.com/hashicorp/nomad-pack/issues/59 for one documented
// instance of this class of bug). So a 0 exit is cross-checked against
// the diff text — specifically, whether nomad-pack marked any individual
// job's "Job:" line with a `+`/`-`/`+/-` change prefix, which is nomad's
// own per-job diff verdict. That's a deliberately narrower signal than
// "any Task Group shows an in-place update count": in practice nomad-pack
// reports a routine in-place-update count for essentially every job on
// essentially every plan (observed to correlate with its own injected
// metadata churning, e.g. a timestamp field, independent of whether the
// job's actual spec changed) — so treating that count alone as "a change"
// produces constant false positives. The `+/-` prefix on the Job line
// itself doesn't. The returned diff is also trimmed to only the job
// blocks nomad-pack actually marked as changed, dropping the unmarked
// (routine, uninformative) job blocks so warnings shown to the user
// aren't a wall of unrelated boilerplate.
func (c *Client) Plan(ctx context.Context, pack PackRef, d Deployment) (hasChanges bool, diff string, res *Result, err error) {
	pack = c.resolvedPackRef(pack)
	d = c.resolvedDeployment(d)
	if pack, err = c.ensureRegistry(ctx, pack); err != nil {
		return false, "", nil, err
	}

	args := []string{"plan", pack.Pack, "--diff"}
	args = append(args, packArgs(pack)...)
	args = append(args, deploymentArgs(d)...)
	args = append(args, c.clusterArgs()...)

	res, err = c.exec(ctx, args)
	if err != nil {
		return false, "", res, err
	}

	changedBlocks, anyChanged := filterChangedJobBlocks(res.Stdout)

	switch res.ExitCode {
	case planExitNoChanges:
		if anyChanged {
			return true, changedBlocks, res, nil
		}
		return false, res.Stdout, res, nil
	case planExitMakesChanges:
		if anyChanged {
			return true, changedBlocks, res, nil
		}
		// Exit code says changes are pending but no job's "Job:" line was
		// marked — output format didn't match what was expected (e.g. a
		// future nomad-pack version). Fall back to the full raw text
		// rather than silently showing nothing.
		return true, res.Stdout, res, nil
	default:
		return false, res.Stdout, res, &CLIError{
			Op: "plan", Args: args, ExitCode: res.ExitCode,
			Stdout: res.Stdout, Stderr: res.Stderr,
		}
	}
}

// jobHeaderRe matches a `Job:` line in nomad-pack's plan output, capturing
// an optional leading `+`, `-`, or `+/-` change marker. nomad-pack applies
// this marker to the top-level Job line whenever anything nested under it
// differs — the same convention `nomad job plan` itself uses — making it a
// more precise "did this job actually change" signal than any per-Task-
// Group annotation.
var jobHeaderRe = regexp.MustCompile(`(\+/-|\+|-)?\s*Job:\s*"([^"]*)"`)

// filterChangedJobBlocks splits nomad-pack's plan output into per-job
// blocks and keeps only the ones for jobs nomad-pack actually marked
// changed. nomad-pack sometimes concatenates consecutive jobs' output
// with no separating newline at all (observed in practice as e.g.
// `...potentially invalid.Job: "alloy"...`), so blocks are found by
// scanning for `Job:` occurrences directly rather than splitting on
// lines.
func filterChangedJobBlocks(output string) (changed string, anyChanged bool) {
	matches := jobHeaderRe.FindAllStringSubmatchIndex(output, -1)
	if len(matches) == 0 {
		return "", false
	}

	var kept []string
	for i, m := range matches {
		start := m[0]
		end := len(output)
		if i+1 < len(matches) {
			end = matches[i+1][0]
		}

		marker := ""
		if m[2] != -1 {
			marker = output[m[2]:m[3]]
		}
		if marker == "" {
			continue // Unmarked job block — routine churn, not a real change.
		}
		kept = append(kept, strings.TrimSpace(output[start:end]))
	}

	if len(kept) == 0 {
		return "", false
	}
	return strings.Join(kept, "\n\n"), true
}

// Run submits the pack via `nomad-pack run`, creating or updating every job
// it contains. detach returns immediately instead of waiting for the
// deployment/evaluation to finish, which matters for long Terraform applies
// against slow-to-heal jobs.
func (c *Client) Run(ctx context.Context, pack PackRef, d Deployment, detach bool) (*Result, error) {
	pack = c.resolvedPackRef(pack)
	d = c.resolvedDeployment(d)
	pack, err := c.ensureRegistry(ctx, pack)
	if err != nil {
		return nil, err
	}

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

// isNotFoundOutput reports whether a nomad-pack failure's output indicates
// the target job/deployment is simply already gone (e.g. reaped by
// Nomad's garbage collector after a manual stop, or removed some other
// way outside Terraform) rather than a real error. nomad-pack doesn't
// document a dedicated exit code for this, and its wording isn't
// consistent between commands — `destroy` has been seen saying "not
// found", while `stop` says `no job(s) with prefix or id "X" found`
// (note: no contiguous "not found" substring, so a naive check for that
// phrase alone misses it) — so several observed phrasings are matched
// here rather than just one.
func isNotFoundOutput(res *Result) bool {
	combined := strings.ToLower(res.Stderr + " " + res.Stdout)
	for _, phrase := range []string{"not found", "no job(s)", "does not exist"} {
		if strings.Contains(combined, phrase) {
			return true
		}
	}
	return false
}

// Destroy stops and purges every job nomad-pack associates with this named
// deployment, via `nomad-pack destroy` (equivalent to `stop --purge`).
func (c *Client) Destroy(ctx context.Context, pack PackRef, d Deployment, detach bool) (*Result, error) {
	pack = c.resolvedPackRef(pack)
	d = c.resolvedDeployment(d)
	pack, err := c.ensureRegistry(ctx, pack)
	if err != nil {
		return nil, err
	}

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
		// destroy.
		if isNotFoundOutput(res) {
			return res, nil
		}
		return res, &CLIError{
			Op: "destroy", Args: args, ExitCode: res.ExitCode,
			Stdout: res.Stdout, Stderr: res.Stderr,
		}
	}
	return res, nil
}

// Stop stops (but does not purge) every job nomad-pack associates with this
// named deployment, via `nomad-pack stop` (no `--purge`). Unlike Destroy,
// nomad-pack keeps tracking the deployment afterward — it stays queryable
// via `nomad-pack status`/`plan` (until Nomad's own garbage collector
// eventually reaps it) and can be brought back with a subsequent `run`
// against the same name, without needing to recreate the Terraform
// resource. This is the "stop without destroying" operation.
func (c *Client) Stop(ctx context.Context, pack PackRef, d Deployment, detach bool) (*Result, error) {
	pack = c.resolvedPackRef(pack)
	d = c.resolvedDeployment(d)
	pack, err := c.ensureRegistry(ctx, pack)
	if err != nil {
		return nil, err
	}

	args := []string{"stop", pack.Pack}
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
		// Already stopped-and-gone (e.g. Nomad's GC reaped the job since
		// it was last stopped, whether by this provider or by hand)
		// shouldn't fail Terraform's attempt to reconcile toward
		// stopped=true — there's nothing left to stop, which is exactly
		// the state being asked for.
		if isNotFoundOutput(res) {
			return res, nil
		}
		return res, &CLIError{
			Op: "stop", Args: args, ExitCode: res.ExitCode,
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
