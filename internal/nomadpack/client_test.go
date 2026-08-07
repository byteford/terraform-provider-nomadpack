package nomadpack

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestClusterArgs(t *testing.T) {
	c := &Client{
		Cluster: ClusterConfig{
			Address:       "https://nomad.example:4646",
			Namespace:     "prod",
			Region:        "eu-west",
			Token:         "secret-token",
			TLSSkipVerify: true,
		},
	}

	got := c.clusterArgs()
	want := []string{
		"--address=https://nomad.example:4646",
		"--namespace=prod",
		"--region=eu-west",
		"--token=secret-token",
		"--tls-skip-verify",
	}

	assertEqualArgs(t, got, want)
}

func TestClusterArgsEmpty(t *testing.T) {
	c := &Client{}
	got := c.clusterArgs()
	if len(got) != 0 {
		t.Fatalf("expected no args for empty ClusterConfig, got %v", got)
	}
}

func TestPackArgs(t *testing.T) {
	got := packArgs(PackRef{Pack: "authentik", Registry: "homelab", Ref: "v0.3.1"})
	want := []string{"--registry=homelab", "--ref=v0.3.1"}
	assertEqualArgs(t, got, want)
}

func TestPackArgsRefOnly(t *testing.T) {
	got := packArgs(PackRef{Pack: "authentik", Ref: "v0.3.1"})
	want := []string{"--ref=v0.3.1"}
	assertEqualArgs(t, got, want)
}

func TestDeploymentArgsVarsAreSortedDeterministically(t *testing.T) {
	d := Deployment{
		Name: "authentik-prod",
		Vars: map[string]string{
			"replicas":      "2",
			"postgres_host": "10.0.10.5",
			"admin_email":   "ops@example.com",
		},
		VarFiles: []string{"vars/authentik.hcl"},
	}

	// Run twice: map iteration order is randomized by Go, so this catches
	// any accidental reliance on map order instead of the explicit sort.
	first := deploymentArgs(d)
	second := deploymentArgs(d)

	want := []string{
		"--name=authentik-prod",
		"--var-file=vars/authentik.hcl",
		"--var=admin_email=ops@example.com",
		"--var=postgres_host=10.0.10.5",
		"--var=replicas=2",
	}

	assertEqualArgs(t, first, want)
	assertEqualArgs(t, second, want)
}

func TestDeploymentArgsNoNameNoVars(t *testing.T) {
	got := deploymentArgs(Deployment{})
	if len(got) != 0 {
		t.Fatalf("expected no args for empty Deployment, got %v", got)
	}
}

func TestPlanExitZeroButDiffShowsRealChange(t *testing.T) {
	// Real output pattern that surfaced in the wild: nomad-pack exits 0
	// ("no changes") despite marking one job's "Job:" line as changed.
	// Exit code 0 must not be trusted blindly.
	output := `+/- Job: "prometheus"
+/- Task Group: "prometheus" (1 in-place update)
  +/- Service {
      Name: "prometheus"
    +/- Tags {
      + Tags: "traefik.http.routers.prometheus.rule=Host(` + "`prometheus.sandford.hous`" + `)"
      - Tags: "traefik.http.routers.prometheus.rule=Host(` + "`prometheus.sandford.house`" + `)"
      }
    }
`
	c := &Client{BinaryPath: fakeNomadPack(t, 0, output)}

	hasChanges, diff, _, err := c.Plan(context.Background(), PackRef{Pack: "monitoring"}, Deployment{Name: "monitoring"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !hasChanges {
		t.Fatal("expected hasChanges=true despite exit code 0, because the Job line is marked +/-")
	}
	if !strings.Contains(diff, "prometheus.sandford.hous") {
		t.Fatalf("expected the trimmed diff to include the real change, got: %q", diff)
	}
}

func TestPlanRoutineChurnAcrossManyJobsStaysFalse(t *testing.T) {
	// The exact regression this guards against: multiple jobs each show a
	// bare "(N in-place update)" Task Group annotation with no +/- marker
	// on the Job line itself and no nested diff content — nomad-pack's
	// routine per-plan churn (observed correlating with its own injected
	// metadata), not a real change. None of these should count.
	output := `Job: "alloy"
Task Group: "alloy" (2 in-place update)
  Task: "alloy"
» Scheduler dry-run:
- All tasks successfully allocated.
To submit the job with version verification run:
nomad-pack run monitoring --check-index=6881775 [options]
Job: "grafana"
Task Group: "grafana" (1 in-place update)
  Task: "grafana"
» Scheduler dry-run:
- All tasks successfully allocated.
Job: "loki"
Task Group: "loki" (1 in-place update)
  Task: "loki"
» Scheduler dry-run:
- All tasks successfully allocated.
`
	c := &Client{BinaryPath: fakeNomadPack(t, 0, output)}

	hasChanges, _, _, err := c.Plan(context.Background(), PackRef{Pack: "monitoring"}, Deployment{Name: "monitoring"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if hasChanges {
		t.Fatal("expected hasChanges=false: no job's \"Job:\" line carries a +/-/+/- change marker")
	}
}

func TestFilterChangedJobBlocksDropsUnmarkedJobsAndKeepsMarkedOnes(t *testing.T) {
	// Modeled directly on real nomad-pack output for a 4-job pack where
	// only one job (prometheus) actually changed — including nomad-pack's
	// own quirk of concatenating job blocks with no separating newline
	// (".Job:" with no newline between them).
	output := `Job: "alloy"
Task Group: "alloy" (2 in-place update)
» Scheduler dry-run:
- All tasks successfully allocated.
To submit the job with version verification run:
nomad-pack run monitoring --check-index=6881775 [options]
potentially invalid.Job: "grafana"
Task Group: "grafana" (1 in-place update)
» Scheduler dry-run:
- All tasks successfully allocated.
potentially invalid.+/- Job: "prometheus"
+/- Task Group: "prometheus" (1 in-place update)
  +/- Service {
    + Tags: "traefik...prometheus.sandford.hous"
    - Tags: "traefik...prometheus.sandford.house"
    }
potentially invalid.Job: "loki"
Task Group: "loki" (1 in-place update)
» Scheduler dry-run:
- All tasks successfully allocated.
`
	changed, anyChanged := filterChangedJobBlocks(output)
	if !anyChanged {
		t.Fatal("expected anyChanged=true: prometheus is marked +/-")
	}
	if strings.Contains(changed, `Job: "alloy"`) || strings.Contains(changed, `Job: "grafana"`) || strings.Contains(changed, `Job: "loki"`) {
		t.Fatalf("expected unmarked job blocks to be dropped, got: %q", changed)
	}
	if !strings.Contains(changed, `+/- Job: "prometheus"`) {
		t.Fatalf("expected the marked prometheus block to be kept, got: %q", changed)
	}
}

func TestFilterChangedJobBlocksNoMarkedJobs(t *testing.T) {
	changed, anyChanged := filterChangedJobBlocks(`Job: "x"
Task Group: "x" (1 in-place update)
`)
	if anyChanged {
		t.Fatalf("expected anyChanged=false, got true with changed=%q", changed)
	}
	if changed != "" {
		t.Fatalf("expected empty changed text, got %q", changed)
	}
}

func TestFilterChangedJobBlocksNoJobsAtAll(t *testing.T) {
	changed, anyChanged := filterChangedJobBlocks("Plan succeeded")
	if anyChanged || changed != "" {
		t.Fatalf("expected no change detected on text with no Job: lines, got changed=%q anyChanged=%v", changed, anyChanged)
	}
}

// TestPlanExitCodes exercises Plan()'s exit-code interpretation against a
// fake nomad-pack binary (a shell script) so it doesn't need a real Nomad
// cluster or the actual nomad-pack tool installed. This locks in the
// contract documented at
// https://developer.hashicorp.com/nomad/tools/nomad-pack/commands/plan:
// 0 = no changes, 1 = changes, 255 = error.
func TestPlanExitCodes(t *testing.T) {
	tests := []struct {
		name           string
		exitCode       int
		wantHasChanges bool
		wantErr        bool
	}{
		{name: "no changes", exitCode: 0, wantHasChanges: false, wantErr: false},
		{name: "changes pending", exitCode: 1, wantHasChanges: true, wantErr: false},
		{name: "error", exitCode: 255, wantHasChanges: false, wantErr: true},
		{name: "unexpected code treated as error", exitCode: 42, wantHasChanges: false, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &Client{BinaryPath: fakeNomadPack(t, tt.exitCode, "diff output here")}

			hasChanges, _, res, err := c.Plan(context.Background(), PackRef{Pack: "example"}, Deployment{Name: "dev"})

			if tt.wantErr && err == nil {
				t.Fatalf("expected an error, got none (res=%+v)", res)
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if hasChanges != tt.wantHasChanges {
				t.Fatalf("hasChanges = %v, want %v", hasChanges, tt.wantHasChanges)
			}
		})
	}
}

func TestRunFailurePropagatesOutput(t *testing.T) {
	c := &Client{BinaryPath: fakeNomadPack(t, 1, "boom: job register failed")}

	res, err := c.Run(context.Background(), PackRef{Pack: "example"}, Deployment{Name: "dev"}, false)
	if err == nil {
		t.Fatal("expected an error from a non-zero exit code")
	}
	if res.ExitCode != 1 {
		t.Fatalf("ExitCode = %d, want 1", res.ExitCode)
	}
}

func TestDestroyTreatsNotFoundAsSuccess(t *testing.T) {
	c := &Client{BinaryPath: fakeNomadPack(t, 1, "Error: deployment not found")}

	_, err := c.Destroy(context.Background(), PackRef{Pack: "example"}, Deployment{Name: "dev"}, false)
	if err != nil {
		t.Fatalf("expected destroy of an already-gone deployment to succeed, got: %v", err)
	}
}

// fakeNomadPack writes a tiny shell script standing in for the nomad-pack
// binary: it echoes the given output to stdout and exits with the given
// code, regardless of arguments. Returns the script's path for use as
// Client.BinaryPath.
func fakeNomadPack(t *testing.T, exitCode int, output string) string {
	t.Helper()

	dir := t.TempDir()
	path := filepath.Join(dir, "nomad-pack")

	script := "#!/bin/sh\necho '" + output + "'\nexit " + strconv.Itoa(exitCode) + "\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("failed to write fake nomad-pack script: %v", err)
	}
	return path
}

func assertEqualArgs(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("arg count mismatch:\n got:  %v\n want: %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("arg %d mismatch:\n got:  %v\n want: %v", i, got, want)
		}
	}
}
