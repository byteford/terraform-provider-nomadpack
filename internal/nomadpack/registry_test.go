package nomadpack

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeNomadPackScript writes a nomad-pack stand-in that records every
// invocation's arguments (one line per call) to the returned log path, then
// runs body. Unlike fakeNomadPack it lets a test assert exactly what was sent.
func fakeNomadPackScript(t *testing.T, body string) (binary, argLog string) {
	t.Helper()

	dir := t.TempDir()
	binary = filepath.Join(dir, "nomad-pack")
	argLog = filepath.Join(dir, "args.log")

	script := "#!/bin/sh\necho \"$*\" >> '" + argLog + "'\n" + body + "\n"
	if err := os.WriteFile(binary, []byte(script), 0o755); err != nil {
		t.Fatalf("failed to write fake nomad-pack script: %v", err)
	}
	return binary, argLog
}

func readArgLog(t *testing.T, argLog string) []string {
	t.Helper()
	b, err := os.ReadFile(argLog)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("reading arg log: %v", err)
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

func TestValidateRegistryRef(t *testing.T) {
	tests := []struct {
		ref     string
		wantErr bool
	}{
		{"v0.3.1", false},
		{"pack-name/v0.4.2", false},
		{"0123456789abcdef0123456789abcdef01234567", false},
		{"", true},
		{"   ", true},
		{"latest", true},
		{"LATEST", true},
		{"@latest", true},
		{" latest ", true},
	}
	for _, tt := range tests {
		err := ValidateRegistryRef(tt.ref)
		if (err != nil) != tt.wantErr {
			t.Errorf("ValidateRegistryRef(%q) error = %v, wantErr %v", tt.ref, err, tt.wantErr)
		}
	}
}

func TestRegistryAddSendsPinnedRefAndNoClusterFlags(t *testing.T) {
	bin, argLog := fakeNomadPackScript(t, "exit 0")
	c := &Client{BinaryPath: bin, Cluster: ClusterConfig{Address: "http://nomad:4646", Token: "s3cret"}}

	_, err := c.RegistryAdd(context.Background(), Registry{
		Name: "homelab", Source: "github.com/byteford/nomad-packs", Ref: "v1.2.3",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := readArgLog(t, argLog)
	want := []string{"registry add homelab github.com/byteford/nomad-packs --ref=v1.2.3"}
	assertEqualArgs(t, got, want)
}

func TestRegistryAddRefusesUnpinnedRefWithoutRunning(t *testing.T) {
	// Without an explicit ref nomad-pack overwrites the global @latest, so
	// the client must never get as far as exec'ing it.
	for _, ref := range []string{"", "latest", "@latest"} {
		bin, argLog := fakeNomadPackScript(t, "exit 0")
		c := &Client{BinaryPath: bin}

		_, err := c.RegistryAdd(context.Background(), Registry{Name: "homelab", Source: "github.com/x/y", Ref: ref})
		if err == nil {
			t.Fatalf("ref %q: expected error, got nil", ref)
		}
		if got := readArgLog(t, argLog); len(got) != 0 {
			t.Fatalf("ref %q: nomad-pack was invoked: %v", ref, got)
		}
	}
}

func TestRegistryAddSurfacesCLIError(t *testing.T) {
	bin, _ := fakeNomadPackScript(t, "echo 'could not install registry' >&2\nexit 1")
	c := &Client{BinaryPath: bin}

	_, err := c.RegistryAdd(context.Background(), Registry{Name: "homelab", Source: "github.com/x/y", Ref: "v1"})
	cliErr, ok := err.(*CLIError)
	if !ok {
		t.Fatalf("expected *CLIError, got %T: %v", err, err)
	}
	if cliErr.Op != "registry add" || cliErr.ExitCode != 1 || !strings.Contains(cliErr.Output(), "could not install registry") {
		t.Fatalf("unexpected CLIError: %+v", cliErr)
	}
}

func TestParseRegistryList(t *testing.T) {
	tests := []struct {
		name string
		out  string
		want []RegistryEntry
	}{
		{
			name: "whitespace separated table",
			out: `REGISTRY NAME  REF       LOCAL_REF  REGISTRY_URL
homelab        v1.2.3    9f8e7d6c   github.com/byteford/nomad-packs
community      latest    1a2b3c4d   github.com/hashicorp/nomad-pack-community-registry
`,
			want: []RegistryEntry{
				{Name: "homelab", Ref: "v1.2.3"},
				{Name: "community", Ref: "latest"},
			},
		},
		{
			name: "pipe separated table with separator row",
			out: ` REGISTRY NAME | REF    | LOCAL_REF | REGISTRY_URL
---------------+--------+-----------+-----------------------------
 homelab       | v1.2.3 | 9f8e7d6c  | github.com/byteford/nomad-packs
`,
			want: []RegistryEntry{{Name: "homelab", Ref: "v1.2.3"}},
		},
		{
			name: "empty cache message is not a registry",
			out:  "No registries present in the cache.\n",
			want: nil,
		},
		{
			name: "pre-0.1 cache warning after the table is ignored",
			out: `REGISTRY NAME  REF     LOCAL_REF  REGISTRY_URL
homelab        v1.2.3
It appears that you have a cache created before Nomad-Pack 0.1 (hence the
`,
			want: []RegistryEntry{{Name: "homelab", Ref: "v1.2.3"}},
		},
		{
			name: "no header at all yields nothing",
			out:  "something unexpected\n",
			want: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseRegistryList(tt.out)
			if len(got) != len(tt.want) {
				t.Fatalf("got %+v, want %+v", got, tt.want)
			}
			for i := range got {
				if got[i].Name != tt.want[i].Name || got[i].Ref != tt.want[i].Ref {
					t.Fatalf("entry %d: got %+v, want %+v", i, got[i], tt.want[i])
				}
			}
		})
	}
}

const listWithHomelab = `REGISTRY NAME  REF       LOCAL_REF  REGISTRY_URL
homelab        v1.2.3    9f8e7d6c   github.com/byteford/nomad-packs
homelab        01234567  01234567   github.com/byteford/nomad-packs
`

func TestRegistryExists(t *testing.T) {
	bin, argLog := fakeNomadPackScript(t, "cat <<'EOF'\n"+listWithHomelab+"EOF")
	c := &Client{BinaryPath: bin}

	tests := []struct {
		name, ref string
		want      bool
	}{
		{"homelab", "v1.2.3", true},
		{"homelab", "v9.9.9", false}, // same registry, other ref
		{"other", "v1.2.3", false},   // other registry, same ref
		{"homelab", "v1.2", false},   // prefix of a tag must not match
		// nomad-pack list shortens SHA refs to 8 chars, so a full 40-char
		// SHA in config must still match its shortened row.
		{"homelab", "0123456789abcdef0123456789abcdef01234567", true},
		{"homelab", "0123456aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", false},
	}
	for _, tt := range tests {
		got, err := c.RegistryExists(context.Background(), tt.name, tt.ref)
		if err != nil {
			t.Fatalf("RegistryExists(%q,%q): %v", tt.name, tt.ref, err)
		}
		if got != tt.want {
			t.Errorf("RegistryExists(%q,%q) = %v, want %v", tt.name, tt.ref, got, tt.want)
		}
	}
	for _, line := range readArgLog(t, argLog) {
		if line != "registry list" {
			t.Errorf("unexpected invocation %q", line)
		}
	}
}

func TestRegistryExistsEmptyCache(t *testing.T) {
	bin, _ := fakeNomadPackScript(t, "echo 'No registries present in the cache.'")
	c := &Client{BinaryPath: bin}

	got, err := c.RegistryExists(context.Background(), "homelab", "v1.2.3")
	if err != nil || got {
		t.Fatalf("RegistryExists = %v, %v; want false, nil", got, err)
	}
}

func TestRegistryExistsListFailureIsAnError(t *testing.T) {
	// A failing list must not be read as "registry missing": Read would
	// then drop it from state and re-add on every transient failure.
	bin, _ := fakeNomadPackScript(t, "echo boom >&2\nexit 1")
	c := &Client{BinaryPath: bin}

	if _, err := c.RegistryExists(context.Background(), "homelab", "v1.2.3"); err == nil {
		t.Fatal("expected error when registry list fails")
	}
}

func TestRegistryDeleteScopesToRef(t *testing.T) {
	// `delete <name>` with no --ref removes the whole registry (every ref),
	// so the ref must always be passed.
	body := `if [ "$1 $2" = "registry list" ]; then
cat <<'EOF'
` + listWithHomelab + `EOF
fi`
	bin, argLog := fakeNomadPackScript(t, body)
	c := &Client{BinaryPath: bin}

	if _, err := c.RegistryDelete(context.Background(), "homelab", "v1.2.3"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertEqualArgs(t, readArgLog(t, argLog), []string{"registry list", "registry delete homelab --ref=v1.2.3"})
}

func TestRegistryDeleteAlreadyGoneIsSuccessAndRunsNothing(t *testing.T) {
	// Wiped cache: delete would exit non-zero; destroy must still succeed.
	bin, argLog := fakeNomadPackScript(t, "echo 'No registries present in the cache.'")
	c := &Client{BinaryPath: bin}

	if _, err := c.RegistryDelete(context.Background(), "homelab", "v1.2.3"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertEqualArgs(t, readArgLog(t, argLog), []string{"registry list"})
}

func TestRegistryDeleteRefusesEmptyRef(t *testing.T) {
	bin, argLog := fakeNomadPackScript(t, "exit 0")
	c := &Client{BinaryPath: bin}

	if _, err := c.RegistryDelete(context.Background(), "homelab", ""); err == nil {
		t.Fatal("expected error for empty ref")
	}
	if got := readArgLog(t, argLog); len(got) != 0 {
		t.Fatalf("nomad-pack was invoked: %v", got)
	}
}
