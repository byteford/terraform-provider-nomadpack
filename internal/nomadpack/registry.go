package nomadpack

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"sync"
)

// registryMu serializes every registry command run by this process.
// nomad-pack stages each `registry add` in one shared directory
// (<cache>/nomad-pack-tmp) and deletes it afterwards, and `registry list`
// reads every directory in the cache, staging dir included. Terraform
// applies independent resources in parallel, so without this two
// nomadpack_registry resources could delete each other's checkout (failed
// applies, or packs cached from the wrong source) or make a list fail.
//
// This only covers one provider process: a second terraform run, or
// another provider instance, working on the same cache is not excluded.
var registryMu sync.Mutex

// Registry identifies one pinned ref of a pack registry in nomad-pack's
// global cache.
type Registry struct {
	Name   string
	Source string // git URL, as accepted by `nomad-pack registry add`
	Ref    string // tag or SHA; never empty or "latest" (see ValidateRegistryRef)
}

// RegistryEntry is one row of `nomad-pack registry list`.
type RegistryEntry struct {
	Name string
	Ref  string
}

// registryNameRe is deliberately strict. nomad-pack uses the name as a
// directory under its cache (path.Join), so "." and ".." would address the
// cache itself or escape it, and "/" nests it. Whitespace and "|" would
// stop parseRegistryList from finding the row, and a leading "-" would be
// read as a flag. Starting with a letter or digit rules out "." and "..".
var registryNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// ValidateRegistryName rejects names that are unsafe as a nomad-pack cache
// directory name or that the list parser could not match again.
func ValidateRegistryName(name string) error {
	if !registryNameRe.MatchString(name) {
		return fmt.Errorf("registry name %q is not allowed: use letters, digits, '.', '_' and '-', starting with a letter or digit", name)
	}
	return nil
}

// ValidateRegistryRef rejects refs that would make `registry add`/`delete`
// act on nomad-pack's moving @latest. Per nomad-pack's docs, adding without
// a ref (or with @latest) overwrites the global cache's current @latest, so
// the registry would silently change underneath every other deployment
// that uses it. An explicit ref is idempotent instead.
//
// Branches cannot be told apart from tags syntactically, so they are not
// rejected here; the resource docs say to use a tag or SHA.
func ValidateRegistryRef(ref string) error {
	switch strings.ToLower(strings.TrimSpace(ref)) {
	case "":
		return fmt.Errorf("ref must be a tag or commit SHA, got an empty value")
	case "latest", "@latest":
		return fmt.Errorf("ref must be a tag or commit SHA, not %q: nomad-pack overwrites its cached @latest when no pinned ref is given", ref)
	}
	return nil
}

// RegistryAdd runs `nomad-pack registry add <name> <source> --ref=<ref>`.
// With an explicit ref this is idempotent. It never receives the Nomad
// cluster flags: registry commands only touch the local cache.
func (c *Client) RegistryAdd(ctx context.Context, r Registry) (*Result, error) {
	if err := ValidateRegistryName(r.Name); err != nil {
		return nil, err
	}
	if err := ValidateRegistryRef(r.Ref); err != nil {
		return nil, err
	}

	registryMu.Lock()
	defer registryMu.Unlock()
	return c.registryAdd(ctx, r)
}

// registryAdd is RegistryAdd for callers that already hold registryMu and
// have validated r.
func (c *Client) registryAdd(ctx context.Context, r Registry) (*Result, error) {
	args := []string{"registry", "add", r.Name, r.Source, "--ref=" + r.Ref}
	res, err := c.exec(ctx, args)
	if err != nil {
		return res, err
	}
	if res.ExitCode != 0 {
		return res, &CLIError{
			Op: "registry add", Args: args, ExitCode: res.ExitCode,
			Stdout: res.Stdout, Stderr: res.Stderr,
		}
	}
	return res, nil
}

// RegistryList runs `nomad-pack registry list` and returns its rows.
func (c *Client) RegistryList(ctx context.Context) ([]RegistryEntry, error) {
	registryMu.Lock()
	defer registryMu.Unlock()
	return c.registryList(ctx)
}

// registryList is RegistryList for callers that already hold registryMu.
func (c *Client) registryList(ctx context.Context) ([]RegistryEntry, error) {
	args := []string{"registry", "list"}
	res, err := c.exec(ctx, args)
	if err != nil {
		return nil, err
	}
	if res.ExitCode != 0 {
		return nil, &CLIError{
			Op: "registry list", Args: args, ExitCode: res.ExitCode,
			Stdout: res.Stdout, Stderr: res.Stderr,
		}
	}
	return parseRegistryList(res.Stdout), nil
}

// RegistryExists reports whether `registry list` shows name at ref. A failing
// list is an error, never "not present": the caller would otherwise treat a
// transient failure as a wiped cache.
func (c *Client) RegistryExists(ctx context.Context, name, ref string) (bool, error) {
	registryMu.Lock()
	defer registryMu.Unlock()
	return c.registryExists(ctx, name, ref)
}

// registryExists is RegistryExists for callers that already hold registryMu.
func (c *Client) registryExists(ctx context.Context, name, ref string) (bool, error) {
	entries, err := c.registryList(ctx)
	if err != nil {
		return false, err
	}
	for _, e := range entries {
		if e.Name == name && refMatches(e.Ref, ref) {
			return true, nil
		}
	}
	return false, nil
}

// RegistryDelete removes one ref of a registry via
// `nomad-pack registry delete <name> --ref=<ref>`. The ref is mandatory:
// without it nomad-pack deletes the whole registry, every ref included.
// A registry that is already gone is success (checked via list first, so no
// error-text matching is needed): destroy must work after a wiped cache.
func (c *Client) RegistryDelete(ctx context.Context, name, ref string) (*Result, error) {
	if err := ValidateRegistryName(name); err != nil {
		return nil, err
	}
	if err := ValidateRegistryRef(ref); err != nil {
		return nil, err
	}

	registryMu.Lock()
	defer registryMu.Unlock()

	exists, err := c.registryExists(ctx, name, ref)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, nil
	}

	args := []string{"registry", "delete", name, "--ref=" + ref}
	res, err := c.exec(ctx, args)
	if err != nil {
		return res, err
	}
	if res.ExitCode != 0 {
		// nomad-pack keeps a per-ref metadata.json after its own ref-scoped
		// delete, so `list` can still show a registry whose packs are gone,
		// and deleting it again fails with this message. Nothing is left to
		// delete, which is what destroy wants.
		if strings.Contains(strings.ToLower(res.Stderr+" "+res.Stdout), "no packs found matching arguments") {
			return res, nil
		}
		return res, &CLIError{
			Op: "registry delete", Args: args, ExitCode: res.ExitCode,
			Stdout: res.Stdout, Stderr: res.Stderr,
		}
	}
	return res, nil
}

// refMatches compares a ref as printed by `registry list` with the
// configured one, by applying to the configured ref the same transformation
// nomad-pack applies before printing.
func refMatches(listed, want string) bool {
	return listed == want || listed == listedRef(want)
}

// listedRef mirrors nomad-pack's formatSHA1Reference (internal/cli/
// formatters.go): a ref is printed unchanged unless it is 40 characters long
// or made only of hex digits, in which case only its first 8 characters are
// printed.
func listedRef(ref string) string {
	if len(ref) != 40 && strings.Trim(strings.ToLower(ref), "0123456789abcdef") != "" {
		return ref
	}
	if len(ref) > 8 {
		return ref[:8]
	}
	return ref
}

// parseRegistryList extracts (name, ref) from `nomad-pack registry list`.
// The output is a borderless table whose header is
// "REGISTRY NAME  REF  LOCAL_REF  REGISTRY_URL". Parsing is deliberately
// tolerant: columns may be separated by spaces or "|", separator rows are
// skipped, and only the first two columns are read (LOCAL_REF and the URL
// can be blank on caches created by old nomad-pack versions). Rows are
// only read after the header, and rows with more than four fields are
// prose (e.g. nomad-pack's pre-0.1 cache warning), not table rows.
func parseRegistryList(out string) []RegistryEntry {
	var entries []RegistryEntry
	inTable := false
	for _, line := range strings.Split(out, "\n") {
		if strings.Trim(line, "-+=|_ \t\r") == "" {
			continue // blank line or table separator
		}
		fields := strings.Fields(strings.ReplaceAll(line, "|", " "))

		if !inTable {
			if len(fields) >= 2 && strings.EqualFold(fields[0], "REGISTRY") && strings.EqualFold(fields[1], "NAME") {
				inTable = true
			}
			continue
		}
		if len(fields) < 2 || len(fields) > 4 {
			continue
		}
		entries = append(entries, RegistryEntry{Name: fields[0], Ref: fields[1]})
	}
	return entries
}

var (
	nonNameChars  = regexp.MustCompile(`[^A-Za-z0-9._]+`)
	urlScheme     = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]*://`)
	maxSlugLength = 100
)

// RegistryNameForSource derives the nomad-pack registry name used for a
// deployment's registry_source. A URL can't be the name itself (nomad-pack
// uses it as a cache directory name), so the name is a lower-case slug of
// the host and path: "github.com-byteford-nomad-packs".
//
// The name ends up in jobs' pack.registry meta, and a meta change restarts
// the job, so it must come out the same for every machine and every
// spelling of one repository. The scheme, user and password, a trailing
// ".git" or "/", a "?query", and letter case are therefore all ignored:
// "git@github.com:Org/repo.git" and "https://github.com/org/repo" give the
// same name. That also keeps credentials out of the name.
//
// Two different repositories only share a name if their host and path
// flatten to the same text (for example "host/a/b" and "host/a-b"); the
// first one cached under a ref then wins silently, so avoid that.
//
// Changing this function renames the registry of every existing
// deployment and restarts their jobs.
func RegistryNameForSource(source string) string {
	s := strings.TrimSpace(source)
	s = strings.TrimPrefix(s, "git::")
	if i := strings.IndexAny(s, "?#"); i >= 0 {
		s = s[:i]
	}
	s = urlScheme.ReplaceAllString(s, "")
	if i := strings.Index(s, "@"); i >= 0 && !strings.Contains(s[:i], "/") {
		s = s[i+1:] // user@host, user:password@host
	}
	s = strings.TrimRight(s, "/")
	s = strings.TrimSuffix(s, ".git")
	s = strings.ToLower(s)

	slug := strings.Trim(nonNameChars.ReplaceAllString(s, "-"), "-.")
	if len(slug) > maxSlugLength {
		slug = strings.Trim(slug[:maxSlugLength], "-.")
	}
	if slug == "" {
		slug = "registry"
	}
	return slug
}

// ensureRegistry makes a pack's registry available in nomad-pack's cache
// when the pack is configured with a RegistrySource, and returns the PackRef
// with Registry set to the derived name. Without a RegistrySource it is a
// no-op. Doing this before every plan/run/stop/destroy is what lets
// `terraform plan` preview a pack from a registry (or ref) that is not in
// the cache yet, and re-adds one after the cache was wiped.
//
// The registry is only added when `registry list` does not already show it,
// so a cached registry costs no network call (and if list output were ever
// unparseable the worst case is an extra, idempotent add). Check and add
// happen under registryMu, so deployments that share a source clone it once.
func (c *Client) ensureRegistry(ctx context.Context, pack PackRef) (PackRef, error) {
	if pack.RegistrySource == "" {
		return pack, nil
	}
	if err := ValidateRegistryRef(pack.Ref); err != nil {
		return pack, fmt.Errorf("registry_source requires a pinned ref: %w", err)
	}

	pack.Registry = RegistryNameForSource(pack.RegistrySource)
	reg := Registry{Name: pack.Registry, Source: pack.RegistrySource, Ref: pack.Ref}

	registryMu.Lock()
	defer registryMu.Unlock()

	exists, err := c.registryExists(ctx, reg.Name, reg.Ref)
	if err != nil {
		return pack, err
	}
	if exists {
		return pack, nil
	}
	if _, err := c.registryAdd(ctx, reg); err != nil {
		return pack, err
	}
	return pack, nil
}
