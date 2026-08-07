package nomadpack

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
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
		"-address=https://nomad.example:4646",
		"-namespace=prod",
		"-region=eu-west",
		"-token=secret-token",
		"-tls-skip-verify",
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
	want := []string{"-registry=homelab", "-ref=v0.3.1"}
	assertEqualArgs(t, got, want)
}

func TestPackArgsRefOnly(t *testing.T) {
	got := packArgs(PackRef{Pack: "authentik", Ref: "v0.3.1"})
	want := []string{"-ref=v0.3.1"}
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
		"-name=authentik-prod",
		"-var-file=vars/authentik.hcl",
		"-var=admin_email=ops@example.com",
		"-var=postgres_host=10.0.10.5",
		"-var=replicas=2",
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
