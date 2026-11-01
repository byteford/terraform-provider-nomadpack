package nomadpack

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

func TestValidateRegistryName(t *testing.T) {
	tests := []struct {
		name    string
		wantErr bool
	}{
		{"homelab", false},
		{"my_registry.v2-test", false},
		{"A1", false},
		{"", true},
		{".", true},  // path.Join would resolve to the cache dir itself
		{"..", true}, // path.Join would escape the cache dir
		{".hidden", true},
		{"-x", true},
		{"a/b", true},
		{`a\b`, true},
		{"a b", true},
		{" a", true},
		{"a\tb", true},
		{"a|b", true}, // breaks the `registry list` table parser
		{"a@b", true},
	}
	for _, tt := range tests {
		err := ValidateRegistryName(tt.name)
		if (err != nil) != tt.wantErr {
			t.Errorf("ValidateRegistryName(%q) error = %v, wantErr %v", tt.name, err, tt.wantErr)
		}
	}
}

func TestRegistryCommandsRefuseUnsafeNameWithoutRunning(t *testing.T) {
	bin, argLog := fakeNomadPackScript(t, "exit 0")
	c := &Client{BinaryPath: bin}

	if _, err := c.RegistryAdd(context.Background(), Registry{Name: "..", Source: "github.com/x/y", Ref: "v1"}); err == nil {
		t.Error("RegistryAdd: expected error for name ..")
	}
	if _, err := c.RegistryDelete(context.Background(), "..", "v1"); err == nil {
		t.Error("RegistryDelete: expected error for name ..")
	}
	if got := readArgLog(t, argLog); len(got) != 0 {
		t.Fatalf("nomad-pack was invoked: %v", got)
	}
}

func TestRefMatches(t *testing.T) {
	// `registry list` prints a ref through nomad-pack's formatSHA1Reference:
	// refs that are 40 chars long or all hex are cut to 8 chars, anything
	// else is printed unchanged.
	const sha40 = "0123456789abcdef0123456789abcdef01234567"
	const tag40 = "release-candidate-with-a-long-name-0001x" // 40 chars, not hex
	if len(tag40) != 40 {
		t.Fatalf("test bug: tag40 has %d chars", len(tag40))
	}
	tests := []struct {
		listed, want string
		match        bool
	}{
		{"v1.2.3", "v1.2.3", true},
		{"v1.2", "v1.2.3", false},
		{"01234567", sha40, true},
		{"0123456a", sha40, false},
		{"abcdef012345"[:8], "abcdef012345", true}, // 12-char short SHA is also cut to 8
		{"dead", "dead", true},
		{"dead", "dead-release", false}, // a hex-looking tag must not prefix-match a longer tag
		{"release-", tag40, true},       // 40-char non-hex tag is cut to 8 as well
		{"release-", "release-other", false},
	}
	for _, tt := range tests {
		if got := refMatches(tt.listed, tt.want); got != tt.match {
			t.Errorf("refMatches(%q, %q) = %v, want %v", tt.listed, tt.want, got, tt.match)
		}
	}
}

func TestRegistryDeleteMetadataOnlyEntryIsSuccess(t *testing.T) {
	// After an earlier ref-scoped delete nomad-pack leaves metadata.json, so
	// `list` still shows the row while `delete` fails with this message.
	body := `if [ "$1 $2" = "registry list" ]; then
cat <<'EOF'
` + listWithHomelab + `EOF
elif [ "$1 $2" = "registry delete" ]; then
echo 'No packs found matching arguments' >&2
exit 1
fi`
	bin, argLog := fakeNomadPackScript(t, body)
	c := &Client{BinaryPath: bin}

	if _, err := c.RegistryDelete(context.Background(), "homelab", "v1.2.3"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertEqualArgs(t, readArgLog(t, argLog), []string{"registry list", "registry delete homelab --ref=v1.2.3"})
}

func TestRegistryDeleteOtherFailuresStillError(t *testing.T) {
	body := `if [ "$1 $2" = "registry list" ]; then
cat <<'EOF'
` + listWithHomelab + `EOF
else
echo 'permission denied' >&2
exit 1
fi`
	bin, _ := fakeNomadPackScript(t, body)
	c := &Client{BinaryPath: bin}

	if _, err := c.RegistryDelete(context.Background(), "homelab", "v1.2.3"); err == nil {
		t.Fatal("expected error")
	}
}

// nomad-pack stages every `registry add` in the shared <cache>/nomad-pack-tmp
// and removes it afterwards, and `registry list` would read that staging
// dir as a registry. Terraform runs independent resources in parallel, so
// the client must not let two registry commands overlap in this process.
func TestRegistryCommandsAreSerialized(t *testing.T) {
	dir := t.TempDir()
	overlap := filepath.Join(dir, "OVERLAP")
	// mkdir is atomic: if it fails another invocation is still running.
	body := `if mkdir '` + dir + `/busy' 2>/dev/null; then
sleep 0.15
rmdir '` + dir + `/busy'
else
echo overlap > '` + overlap + `'
fi
if [ "$1 $2" = "registry list" ]; then echo 'No registries present in the cache.'; fi`
	bin, _ := fakeNomadPackScript(t, body)
	c := &Client{BinaryPath: bin}

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(3)
		go func() {
			defer wg.Done()
			_, _ = c.RegistryAdd(context.Background(), Registry{Name: "a", Source: "github.com/x/a", Ref: "v1"})
		}()
		go func() {
			defer wg.Done()
			_, _ = c.RegistryAdd(context.Background(), Registry{Name: "b", Source: "github.com/x/b", Ref: "v1"})
		}()
		go func() {
			defer wg.Done()
			_, _ = c.RegistryExists(context.Background(), "a", "v1")
		}()
	}
	wg.Wait()

	if _, err := os.Stat(overlap); err == nil {
		t.Fatal("two nomad-pack registry commands ran at the same time")
	}
}

func TestRegistryNameForSource(t *testing.T) {
	sources := []string{
		"github.com/byteford/nomad-packs",
		"https://github.com/byteford/nomad-packs.git",
		"git::https://github.com/byteford/nomad-packs.git?ref=main",
		"git@github.com:byteford/nomad-packs.git",
		"ssh://git@example.com:2222/team/packs",
		"/srv/git/packs with space",
		strings.Repeat("a/", 300),
	}
	seen := map[string]string{}
	for _, src := range sources {
		name := RegistryNameForSource(src)
		if err := ValidateRegistryName(name); err != nil {
			t.Errorf("RegistryNameForSource(%q) = %q, not a valid registry name: %v", src, name, err)
		}
		if len(name) > 120 {
			t.Errorf("RegistryNameForSource(%q) is %d chars long", src, len(name))
		}
		if again := RegistryNameForSource(src); again != name {
			t.Errorf("RegistryNameForSource(%q) not deterministic: %q then %q", src, name, again)
		}
		if other, dup := seen[name]; dup {
			t.Errorf("sources %q and %q share the name %q", other, src, name)
		}
		seen[name] = src
	}
	// Readable: the host and repo are visible in the name.
	if got := RegistryNameForSource("https://github.com/byteford/nomad-packs.git"); !strings.HasPrefix(got, "github.com-byteford-nomad-packs-") {
		t.Errorf("name %q should start with the host and repo", got)
	}
	// Different strings that slug the same must still differ.
	if RegistryNameForSource("github.com/a/b") == RegistryNameForSource("github.com/a-b") {
		t.Error("github.com/a/b and github.com/a-b must not collide")
	}
}

const testSource = "github.com/byteford/nomad-packs"

// listBody makes the fake answer `registry list` with the derived registry
// when it is already "cached" (marker file present) and an empty cache
// otherwise; `registry add` creates the marker.
func sourceFakeBody(t *testing.T, cached bool, failAdd bool) (body, marker string) {
	t.Helper()
	marker = filepath.Join(t.TempDir(), "cached")
	if cached {
		if err := os.WriteFile(marker, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	name := RegistryNameForSource(testSource)
	add := "touch '" + marker + "'"
	if failAdd {
		add = "echo 'could not install registry' >&2; exit 1"
	}
	body = `case "$1 $2" in
"registry list")
  if [ -e '` + marker + `' ]; then
    printf 'REGISTRY NAME  REF  LOCAL_REF  REGISTRY_URL\n` + name + `  v1.2.3  9f8e7d6c  ` + testSource + `\n'
  else
    echo 'No registries present in the cache.'
  fi ;;
"registry add") ` + add + ` ;;
esac`
	return body, marker
}

func TestPlanWithRegistrySourceAddsMissingRegistryFirst(t *testing.T) {
	body, _ := sourceFakeBody(t, false, false)
	bin, argLog := fakeNomadPackScript(t, body)
	c := &Client{BinaryPath: bin}
	name := RegistryNameForSource(testSource)

	_, _, _, err := c.Plan(context.Background(),
		PackRef{Pack: "mosquitto", RegistrySource: testSource, Ref: "v1.2.3"},
		Deployment{Name: "mosquitto"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertEqualArgs(t, readArgLog(t, argLog), []string{
		"registry list",
		"registry add " + name + " " + testSource + " --ref=v1.2.3",
		"plan mosquitto --diff --registry=" + name + " --ref=v1.2.3 --name=mosquitto",
	})
}

func TestPlanWithRegistrySourceSkipsAddWhenCached(t *testing.T) {
	body, _ := sourceFakeBody(t, true, false)
	bin, argLog := fakeNomadPackScript(t, body)
	c := &Client{BinaryPath: bin}
	name := RegistryNameForSource(testSource)

	_, _, _, err := c.Plan(context.Background(),
		PackRef{Pack: "mosquitto", RegistrySource: testSource, Ref: "v1.2.3"},
		Deployment{Name: "mosquitto"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertEqualArgs(t, readArgLog(t, argLog), []string{
		"registry list",
		"plan mosquitto --diff --registry=" + name + " --ref=v1.2.3 --name=mosquitto",
	})
}

func TestEveryPackCommandEnsuresTheRegistry(t *testing.T) {
	name := RegistryNameForSource(testSource)
	pack := PackRef{Pack: "mosquitto", RegistrySource: testSource, Ref: "v1.2.3"}
	dep := Deployment{Name: "mosquitto"}
	ctx := context.Background()

	tests := map[string]struct {
		call func(c *Client) error
		want string
	}{
		"run": {func(c *Client) error { _, err := c.Run(ctx, pack, dep, false); return err },
			"run mosquitto --registry=" + name + " --ref=v1.2.3 --name=mosquitto"},
		"stop": {func(c *Client) error { _, err := c.Stop(ctx, pack, dep, false); return err },
			"stop mosquitto --registry=" + name + " --ref=v1.2.3 --name=mosquitto"},
		"destroy": {func(c *Client) error { _, err := c.Destroy(ctx, pack, dep, false); return err },
			"destroy mosquitto --registry=" + name + " --ref=v1.2.3 --name=mosquitto"},
	}
	for op, tt := range tests {
		t.Run(op, func(t *testing.T) {
			body, _ := sourceFakeBody(t, false, false)
			bin, argLog := fakeNomadPackScript(t, body)
			if err := tt.call(&Client{BinaryPath: bin}); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			got := readArgLog(t, argLog)
			assertEqualArgs(t, got, []string{
				"registry list",
				"registry add " + name + " " + testSource + " --ref=v1.2.3",
				tt.want,
			})
		})
	}
}

func TestRegistrySourceAddFailureStopsBeforeNomadPackCommand(t *testing.T) {
	body, _ := sourceFakeBody(t, false, true)
	bin, argLog := fakeNomadPackScript(t, body)
	c := &Client{BinaryPath: bin}

	_, _, _, err := c.Plan(context.Background(),
		PackRef{Pack: "mosquitto", RegistrySource: testSource, Ref: "v1.2.3"}, Deployment{Name: "mosquitto"})
	if err == nil || !strings.Contains(err.Error(), "could not install registry") {
		t.Fatalf("expected the registry add error, got %v", err)
	}
	for _, l := range readArgLog(t, argLog) {
		if strings.HasPrefix(l, "plan ") {
			t.Fatalf("plan ran although the registry could not be added: %v", readArgLog(t, argLog))
		}
	}
}

func TestRegistrySourceRequiresPinnedRef(t *testing.T) {
	for _, ref := range []string{"", "latest", "@latest"} {
		bin, argLog := fakeNomadPackScript(t, "exit 0")
		c := &Client{BinaryPath: bin}
		_, _, _, err := c.Plan(context.Background(),
			PackRef{Pack: "mosquitto", RegistrySource: testSource, Ref: ref}, Deployment{Name: "mosquitto"})
		if err == nil {
			t.Errorf("ref %q: expected error", ref)
		}
		if got := readArgLog(t, argLog); len(got) != 0 {
			t.Errorf("ref %q: nomad-pack was invoked: %v", ref, got)
		}
	}
}

func TestConcurrentCommandsAddTheSameRegistryOnce(t *testing.T) {
	// Terraform refreshes deployments in parallel; deployments sharing a
	// registry_source must not each clone it.
	body, _ := sourceFakeBody(t, false, false)
	bin, argLog := fakeNomadPackScript(t, body)
	c := &Client{BinaryPath: bin}

	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, _, _ = c.Plan(context.Background(),
				PackRef{Pack: "mosquitto", RegistrySource: testSource, Ref: "v1.2.3"}, Deployment{Name: "mosquitto"})
		}()
	}
	wg.Wait()

	adds := 0
	for _, l := range readArgLog(t, argLog) {
		if strings.HasPrefix(l, "registry add ") {
			adds++
		}
	}
	if adds != 1 {
		t.Fatalf("registry add ran %d times, want 1", adds)
	}
}

func TestPlainRegistryAndLocalPacksDoNotTouchTheCache(t *testing.T) {
	bin, argLog := fakeNomadPackScript(t, "exit 0")
	c := &Client{BinaryPath: bin}
	_, _, _, _ = c.Plan(context.Background(), PackRef{Pack: "mosquitto", Registry: "homelab", Ref: "v1"}, Deployment{Name: "m"})
	_, _, _, _ = c.Plan(context.Background(), PackRef{Pack: "/mnt/e/packs/mosquitto"}, Deployment{Name: "m"})
	for _, l := range readArgLog(t, argLog) {
		if strings.HasPrefix(l, "registry ") {
			t.Fatalf("unexpected registry command: %q", l)
		}
	}
}
