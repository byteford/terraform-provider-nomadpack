package nomadpack

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// nomad-pack keeps its registry cache in $XDG_CACHE_HOME/nomad/packs on
// Linux, and records the path of the pack it ran in every job's pack.path
// meta. A cache_dir that is the same on every machine therefore keeps that
// meta (and so the job) from changing when Terraform runs somewhere else.
func fakeEnvNomadPack(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "nomad-pack")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho \"XDG=${XDG_CACHE_HOME-unset}\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

func TestCacheDirIsPassedAsXDGCacheHome(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", "/somewhere/else")
	c := &Client{BinaryPath: fakeEnvNomadPack(t), CacheDir: "/var/cache/homelab"}

	res, err := c.exec(context.Background(), []string{"registry", "list"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := strings.TrimSpace(res.Stdout); got != "XDG=/var/cache/homelab" {
		t.Fatalf("subprocess saw %q, want XDG=/var/cache/homelab (CacheDir must override the inherited value)", got)
	}
}

func TestCacheDirUnsetLeavesTheEnvironmentAlone(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", "/somewhere/else")
	c := &Client{BinaryPath: fakeEnvNomadPack(t)}

	res, err := c.exec(context.Background(), []string{"registry", "list"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := strings.TrimSpace(res.Stdout); got != "XDG=/somewhere/else" {
		t.Fatalf("subprocess saw %q, want the inherited XDG=/somewhere/else", got)
	}
}

func TestCacheDirReachesEveryRegistryAndPackCommand(t *testing.T) {
	// Registry commands and pack commands must use the same cache, or a
	// registry added by one would be invisible to the other.
	dir := t.TempDir()
	bin := filepath.Join(dir, "nomad-pack")
	log := filepath.Join(dir, "env.log")
	script := "#!/bin/sh\necho \"$1 XDG=$XDG_CACHE_HOME\" >> '" + log + "'\n" +
		"if [ \"$1 $2\" = \"registry list\" ]; then echo 'No registries present in the cache.'; fi\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	c := &Client{BinaryPath: bin, CacheDir: "/var/cache/homelab"}
	ctx := context.Background()

	_, _ = c.RegistryAdd(ctx, Registry{Name: "homelab", Source: "github.com/x/y", Ref: "v1"})
	_, _ = c.RegistryList(ctx)
	_, _, _, _ = c.Plan(ctx, PackRef{Pack: "p", Registry: "homelab", Ref: "v1"}, Deployment{Name: "p"})
	_, _ = c.Run(ctx, PackRef{Pack: "p", Registry: "homelab", Ref: "v1"}, Deployment{Name: "p"}, false)
	_, _ = c.Stop(ctx, PackRef{Pack: "p", Registry: "homelab", Ref: "v1"}, Deployment{Name: "p"}, false)
	_, _ = c.Destroy(ctx, PackRef{Pack: "p", Registry: "homelab", Ref: "v1"}, Deployment{Name: "p"}, false)

	b, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 6 {
		t.Fatalf("expected 6 nomad-pack invocations, got %d: %q", len(lines), lines)
	}
	for _, l := range lines {
		if !strings.HasSuffix(l, "XDG=/var/cache/homelab") {
			t.Errorf("invocation %q did not get the cache dir", l)
		}
	}
}

func TestCacheDirHonouredOnThisOS(t *testing.T) {
	// os.UserCacheDir (what nomad-pack uses) only reads XDG_CACHE_HOME on
	// Unix-like systems other than macOS, iOS, Windows and Plan 9.
	tests := map[string]bool{
		"linux": true, "freebsd": true, "openbsd": true,
		"darwin": false, "ios": false, "windows": false, "plan9": false,
	}
	for goos, want := range tests {
		if got := CacheDirHonoured(goos); got != want {
			t.Errorf("CacheDirHonoured(%q) = %v, want %v", goos, got, want)
		}
	}
}
