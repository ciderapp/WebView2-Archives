package main

// Manual source mode ("repack").
//
// This mode turns Microsoft Edge full update packages
// (MicrosoftEdge_<ARCH>_<version>.exe, as returned by the Edge update API)
// into cabinets laid out like Microsoft's Fixed Version cabinets. It exists
// for emergencies where Microsoft has not yet published a Fixed Version for a
// build that is needed. Releases produced here are always GitHub prereleases,
// never "latest", and are kept out of latest.json, the latest-version.txt
// alias and catalog.json "latest". Rise only serves them through an explicit
// pin.
//
// The default is a dry run: download, verify SHA256, extract, validate, build
// the cabinets and sidecar files locally, and print what would be published.
// Publishing needs both -publish and -acknowledge-license-risk.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	repackKind         = "evergreen-repack"
	repackTagSeparator = "-evergreen-"
)

var (
	// Exact Microsoft download hosts seen for Fixed Version cabinets and for
	// Edge full packages returned by the Edge update API
	// (msedge.api.cdp.microsoft.com ... ?action=GenerateDownloadInfo).
	repackAllowedHosts = map[string]bool{
		"msedge.sf.dl.delivery.mp.microsoft.com":    true,
		"msedge.b.tlu.dl.delivery.mp.microsoft.com": true,
		"msedge.f.tlu.dl.delivery.mp.microsoft.com": true,
	}
	// Any other host under this suffix is also accepted.
	repackAllowedSuffix = ".dl.delivery.mp.microsoft.com"

	// Test hook: lets unit tests serve fixtures from httptest servers.
	repackHostAllowedOverride func(host string) bool

	repackVersionRe = regexp.MustCompile(`^\d+\.\d+\.\d+\.\d+$`)
	repackChannelRe = regexp.MustCompile(`^(stable|beta|dev|canary)$`)
	repackSHA256Re  = regexp.MustCompile(`^[0-9a-f]{64}$`)

	// Files that exist in the Edge browser package but not in Microsoft's
	// Fixed Version cabinet or the Evergreen WebView2 install. Removed from the
	// repacked folder.
	repackPruneFiles = []string{"msedge.exe"}
)

type repackInput struct {
	Arch   string
	URL    string
	File   string // optional local copy; SHA256 is still enforced
	SHA256 string
}

type repackOptions struct {
	Version     string
	Channel     string
	Inputs      []repackInput
	OutDir      string
	Publish     bool
	AckLicense  bool
	Repo        string
	KeepStaging bool
}

type repackResult struct {
	Tag       string
	Dir       string
	Cabs      []repackCab
	SumsPath  string
	Source    string
	NotesPath string
}

type repackCab struct {
	Arch        string
	Path        string
	Filename    string
	SHA256      string
	Size        int64
	InputURL    string
	InputName   string
	InputSHA256 string
	InputSize   int64
	Warnings    []string
}

func repackTag(version, channel string) string {
	return version + repackTagSeparator + channel
}

func isRepackTag(tag string) bool {
	return strings.Contains(tag, repackTagSeparator)
}

func parseRepackFlags(args []string) (repackOptions, error) {
	fs := flag.NewFlagSet("repack", flag.ContinueOnError)
	var opts repackOptions
	fs.StringVar(&opts.Version, "version", "", "runtime version, for example 156.0.4314.30 (required)")
	fs.StringVar(&opts.Channel, "channel", "", "Edge channel the package came from: stable, beta, dev or canary (required)")
	fs.StringVar(&opts.OutDir, "out", ".downloads/repack", "working and output directory")
	fs.BoolVar(&opts.Publish, "publish", false, "publish a GitHub prerelease (default is a dry run)")
	fs.BoolVar(&opts.AckLicense, "acknowledge-license-risk", false, "required together with -publish; see README")
	fs.BoolVar(&opts.KeepStaging, "keep-staging", false, "keep extracted folders after building cabinets")
	urls := map[string]*string{}
	files := map[string]*string{}
	sums := map[string]*string{}
	for _, arch := range expectedArches {
		urls[arch] = fs.String(arch+"-url", "", "Microsoft URL of the "+arch+" full package")
		files[arch] = fs.String(arch+"-file", "", "local copy of the "+arch+" full package (optional, hash still enforced)")
		sums[arch] = fs.String(arch+"-sha256", "", "expected SHA256 (hex) of the "+arch+" full package")
	}
	if err := fs.Parse(args); err != nil {
		return opts, err
	}
	if fs.NArg() > 0 {
		return opts, fmt.Errorf("unexpected arguments: %v", fs.Args())
	}
	opts.Version = strings.TrimSpace(opts.Version)
	opts.Channel = strings.ToLower(strings.TrimSpace(opts.Channel))
	if !repackVersionRe.MatchString(opts.Version) {
		return opts, fmt.Errorf("-version must look like 156.0.4314.30, got %q", opts.Version)
	}
	if !repackChannelRe.MatchString(opts.Channel) {
		return opts, fmt.Errorf("-channel must be stable, beta, dev or canary, got %q", opts.Channel)
	}
	for _, arch := range expectedArches {
		u := strings.TrimSpace(*urls[arch])
		f := strings.TrimSpace(*files[arch])
		s := strings.ToLower(strings.TrimSpace(*sums[arch]))
		if u == "" && f == "" && s == "" {
			continue
		}
		if u == "" {
			return opts, fmt.Errorf("-%s-url is required for %s (it is recorded in source.json)", arch, arch)
		}
		if err := checkRepackURL(u); err != nil {
			return opts, fmt.Errorf("-%s-url: %w", arch, err)
		}
		if !repackSHA256Re.MatchString(s) {
			return opts, fmt.Errorf("-%s-sha256 must be 64 hex characters", arch)
		}
		opts.Inputs = append(opts.Inputs, repackInput{Arch: arch, URL: u, File: f, SHA256: s})
	}
	if len(opts.Inputs) == 0 {
		return opts, errors.New("at least one of -x64-url, -x86-url or -arm64-url is required")
	}
	if opts.Publish && !opts.AckLicense {
		return opts, errors.New("-publish also requires -acknowledge-license-risk (read the README section on repacks first)")
	}
	return opts, nil
}

// checkRepackURL enforces http(s), a Microsoft download host and no
// credentials, IP literals or custom ports.
func checkRepackURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("scheme %q is not allowed", u.Scheme)
	}
	if u.User != nil {
		return errors.New("credentials in URL are not allowed")
	}
	host := strings.ToLower(u.Hostname())
	if repackHostAllowedOverride != nil {
		if repackHostAllowedOverride(host) {
			return nil
		}
		return fmt.Errorf("host %q is not allowed", host)
	}
	if u.Port() != "" {
		return errors.New("explicit ports are not allowed")
	}
	if net.ParseIP(host) != nil {
		return errors.New("IP address hosts are not allowed")
	}
	if repackAllowedHosts[host] || strings.HasSuffix(host, repackAllowedSuffix) {
		return nil
	}
	return fmt.Errorf("host %q is not an allowed Microsoft download host", host)
}

func runRepack(args []string) error {
	opts, err := parseRepackFlags(args)
	if err != nil {
		return err
	}
	opts.Repo = os.Getenv("GITHUB_REPOSITORY")
	if opts.Publish {
		if opts.Repo == "" {
			return errors.New("GITHUB_REPOSITORY is required with -publish")
		}
		if ghToken() == "" {
			return errors.New("GH_TOKEN or GITHUB_TOKEN is required with -publish")
		}
		if _, err := exec.LookPath("gh"); err != nil {
			return fmt.Errorf("gh is required on PATH: %w", err)
		}
	}
	for _, tool := range []string{"7z", "gcab"} {
		if _, err := exec.LookPath(tool); err != nil {
			return fmt.Errorf("%s is required on PATH (apt-get install 7zip gcab): %w", tool, err)
		}
	}
	if opts.Publish {
		if err := checkRepackTagFree(opts.Repo, opts.Version, opts.Channel); err != nil {
			return err
		}
	}
	res, err := buildRepack(context.Background(), opts)
	if err != nil {
		return err
	}
	printRepackSummary(opts, res)
	if !opts.Publish {
		log.Print("Dry run complete. Nothing was published. Re-run with -publish -acknowledge-license-risk to create the prerelease.")
		return nil
	}
	return publishRepack(opts.Repo, res)
}

func buildRepack(ctx context.Context, opts repackOptions) (*repackResult, error) {
	tag := repackTag(opts.Version, opts.Channel)
	dir, err := filepath.Abs(filepath.Join(opts.OutDir, tag))
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	res := &repackResult{Tag: tag, Dir: dir}
	inputs := append([]repackInput(nil), opts.Inputs...)
	sort.Slice(inputs, func(i, j int) bool { return archIndex(inputs[i].Arch) < archIndex(inputs[j].Arch) })
	for _, in := range inputs {
		cab, err := repackOne(ctx, dir, opts, in)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", in.Arch, err)
		}
		res.Cabs = append(res.Cabs, cab)
	}
	if res.SumsPath, res.Source, err = writeRepackSidecars(dir, opts, tag, res.Cabs); err != nil {
		return nil, err
	}
	res.NotesPath = filepath.Join(dir, "RELEASE_NOTES.md")
	if err := os.WriteFile(res.NotesPath, []byte(repackNotes(opts, tag, res.Cabs)), 0o644); err != nil {
		return nil, err
	}
	return res, nil
}

func repackOne(ctx context.Context, dir string, opts repackOptions, in repackInput) (repackCab, error) {
	cab := repackCab{Arch: in.Arch, InputURL: in.URL}
	name, _ := filenameFromURL(in.URL)
	if name == "" || name == "." || name == "/" || !strings.Contains(name, ".") {
		// Edge update API URLs end in a bare file GUID.
		name = fmt.Sprintf("MicrosoftEdge_%s_%s.exe", strings.ToUpper(in.Arch), opts.Version)
	}
	cab.InputName = name
	inputPath := filepath.Join(dir, "input", in.Arch, name)
	if in.File != "" {
		if err := copyFile(in.File, inputPath); err != nil {
			return cab, err
		}
	} else {
		log.Printf("Downloading %s package from %s", in.Arch, in.URL)
		if err := downloadVerified(ctx, in.URL, inputPath, in.SHA256); err != nil {
			return cab, err
		}
	}
	sum, err := sha256File(inputPath)
	if err != nil {
		return cab, err
	}
	if sum != in.SHA256 {
		_ = os.Remove(inputPath)
		return cab, fmt.Errorf("SHA256 mismatch for %s: expected %s, got %s", name, in.SHA256, sum)
	}
	info, err := os.Stat(inputPath)
	if err != nil {
		return cab, err
	}
	cab.InputSHA256 = sum
	cab.InputSize = info.Size()
	log.Printf("  %s verified (%s)", name, sum)

	work := filepath.Join(dir, "work", in.Arch)
	_ = os.RemoveAll(work)
	runtimeDir, err := extractEdgePackage(inputPath, work, opts.Version)
	if err != nil {
		return cab, err
	}
	folderName := fmt.Sprintf("Microsoft.WebView2.FixedVersionRuntime.%s.%s", opts.Version, in.Arch)
	staging := filepath.Join(dir, "staging", in.Arch)
	_ = os.RemoveAll(staging)
	if err := os.MkdirAll(staging, 0o755); err != nil {
		return cab, err
	}
	folder := filepath.Join(staging, folderName)
	if err := os.Rename(runtimeDir, folder); err != nil {
		return cab, err
	}
	for _, p := range repackPruneFiles {
		_ = os.Remove(filepath.Join(folder, p))
	}
	warnings, err := validateRuntimeFolder(folder, opts.Version, in.Arch)
	if err != nil {
		return cab, err
	}
	cab.Warnings = warnings
	for _, w := range warnings {
		log.Printf("  warning: %s", w)
	}

	cab.Filename = folderName + ".cab"
	cab.Path = filepath.Join(dir, cab.Filename)
	_ = os.Remove(cab.Path)
	cmd := exec.Command("gcab", "-c", "-z", cab.Path, folderName)
	cmd.Dir = staging
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return cab, fmt.Errorf("gcab failed: %w", err)
	}
	if err := verifyCabLayout(cab.Path, folderName); err != nil {
		return cab, err
	}
	if cab.SHA256, err = sha256File(cab.Path); err != nil {
		return cab, err
	}
	if info, err = os.Stat(cab.Path); err != nil {
		return cab, err
	}
	cab.Size = info.Size()
	if !opts.KeepStaging {
		_ = os.RemoveAll(work)
		_ = os.RemoveAll(staging)
		// Drop the parent folders once the last arch is done (no-op if not empty).
		_ = os.Remove(filepath.Dir(work))
		_ = os.Remove(filepath.Dir(staging))
	}
	log.Printf("  built %s (%.1f MiB) %s", cab.Filename, float64(cab.Size)/(1024*1024), cab.SHA256)
	return cab, nil
}

// downloadVerified streams url to dest while hashing and only keeps the file
// when the SHA256 matches. Every redirect hop must also pass checkRepackURL.
func downloadVerified(ctx context.Context, raw, dest, wantSHA string) error {
	if err := checkRepackURL(raw); err != nil {
		return err
	}
	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("too many redirects")
			}
			return checkRepackURL(req.URL.String())
		},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d from %s", resp.StatusCode, req.URL.Host)
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	part := dest + ".part"
	f, err := os.Create(part)
	if err != nil {
		return err
	}
	h := sha256.New()
	_, copyErr := io.Copy(io.MultiWriter(f, h), resp.Body)
	closeErr := f.Close()
	if copyErr != nil || closeErr != nil {
		_ = os.Remove(part)
		if copyErr != nil {
			return copyErr
		}
		return closeErr
	}
	got := hex.EncodeToString(h.Sum(nil))
	if got != wantSHA {
		_ = os.Remove(part)
		return fmt.Errorf("SHA256 mismatch: expected %s, got %s (download discarded)", wantSHA, got)
	}
	return os.Rename(part, dest)
}

// extractEdgePackage unpacks MicrosoftEdge_<ARCH>_<ver>.exe. The installer
// carries .rsrc/B7/MSEDGE.PACKED.7Z, which holds MSEDGE.7z, which holds
// Chrome-bin/<version>/. Depending on the 7-Zip build the outer step yields
// either the packed archive or MSEDGE.7z directly, so nested archives are
// opened until Chrome-bin/<version> appears.
func extractEdgePackage(input, work, version string) (string, error) {
	current := input
	for depth := 0; depth < 4; depth++ {
		out := filepath.Join(work, fmt.Sprintf("x%d", depth))
		if err := run7z(current, out); err != nil {
			return "", err
		}
		if dir := findChromeBin(out, version); dir != "" {
			return dir, nil
		}
		next := findNestedArchive(out)
		if next == "" {
			break
		}
		current = next
	}
	return "", fmt.Errorf("Chrome-bin/%s not found in package; is this a full (not delta) package for that version?", version)
}

func run7z(archive, out string) error {
	if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}
	cmd := exec.Command("7z", "x", "-y", "-bd", "-o"+out, archive)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("7z x %s: %w\n%s", filepath.Base(archive), err, strings.TrimSpace(string(output)))
	}
	return nil
}

func findChromeBin(root, version string) string {
	var found string
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || found != "" {
			return nil
		}
		if d.IsDir() && d.Name() == version && strings.EqualFold(filepath.Base(filepath.Dir(path)), "Chrome-bin") {
			found = path
			return filepath.SkipAll
		}
		return nil
	})
	return found
}

func findNestedArchive(root string) string {
	var best string
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		lower := strings.ToLower(d.Name())
		if lower == "msedge.7z" {
			best = path
			return filepath.SkipAll
		}
		if strings.HasSuffix(lower, ".7z") && best == "" {
			best = path
		}
		return nil
	})
	return best
}

// validateRuntimeFolder fails when the files clients and DRM need are missing
// and warns about softer layout differences.
func validateRuntimeFolder(folder, version, arch string) ([]string, error) {
	check := func(kind, rel string) error {
		info, err := os.Stat(filepath.Join(folder, filepath.FromSlash(rel)))
		if err != nil || info.IsDir() || info.Size() == 0 {
			return fmt.Errorf("%s file missing or empty: %s", kind, rel)
		}
		return nil
	}
	required := []string{
		"msedgewebview2.exe",
		"WidevineCdm/manifest.json",
		"WidevineCdm/_platform_specific/win_" + arch + "/widevinecdm.dll",
	}
	for _, rel := range required {
		if err := check("required", rel); err != nil {
			return nil, err
		}
	}
	var warnings []string
	for _, rel := range []string{
		version + ".manifest",
		"msedge.dll",
		"msedgewebview2.exe.sig",
		"WidevineCdm/_platform_specific/win_" + arch + "/widevinecdm.dll.sig",
		"WidevineCdm/LICENSE",
	} {
		if err := check("expected", rel); err != nil {
			warnings = append(warnings, err.Error())
		}
	}
	if info, err := os.Stat(filepath.Join(folder, "EBWebView")); err != nil || !info.IsDir() {
		warnings = append(warnings, "EBWebView folder missing")
	}
	return warnings, nil
}

// verifyCabLayout lists the cabinet and checks every entry sits under the
// single top-level folder clients look for (Microsoft.WebView2.FixedVersion*).
func verifyCabLayout(cabPath, folderName string) error {
	out, err := exec.Command("gcab", "-t", cabPath).Output()
	if err != nil {
		return fmt.Errorf("gcab -t: %w", err)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) == 0 || lines[0] == "" {
		return errors.New("cabinet is empty")
	}
	prefix := folderName + `\`
	seen := map[string]bool{}
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, prefix) {
			return fmt.Errorf("unexpected cabinet entry outside %s: %s", folderName, line)
		}
		seen[strings.ToLower(strings.TrimPrefix(line, prefix))] = true
	}
	for _, rel := range []string{`msedgewebview2.exe`, `widevinecdm\manifest.json`} {
		if !seen[rel] {
			return fmt.Errorf("cabinet is missing %s", rel)
		}
	}
	return nil
}

func writeRepackSidecars(dir string, opts repackOptions, tag string, cabs []repackCab) (string, string, error) {
	type inputOut struct {
		Filename string `json:"filename"`
		URL      string `json:"url"`
		SHA256   string `json:"sha256"`
		Size     int64  `json:"size"`
	}
	type buildOut struct {
		Architecture string   `json:"architecture"`
		Filename     string   `json:"filename"`
		SHA256       string   `json:"sha256"`
		Size         int64    `json:"size"`
		Input        inputOut `json:"input"`
		Warnings     []string `json:"warnings,omitempty"`
	}
	src := struct {
		Kind      string     `json:"kind"`
		Version   string     `json:"version"`
		Tag       string     `json:"tag"`
		Channel   string     `json:"channel"`
		CreatedAt string     `json:"created_at"`
		Tooling   string     `json:"tooling"`
		Notice    string     `json:"notice"`
		Builds    []buildOut `json:"builds"`
	}{
		Kind:      repackKind,
		Version:   opts.Version,
		Tag:       tag,
		Channel:   opts.Channel,
		CreatedAt: time.Now().UTC().Truncate(time.Second).Format(time.RFC3339),
		Tooling:   "cmd/archive repack (7z extract, gcab -z MSZIP cabinet)",
		Notice:    "Unofficial repack of a Microsoft Edge full update package. Not a Microsoft Fixed Version. Microsoft and Google license terms apply.",
	}
	var lines []string
	for _, c := range cabs {
		lines = append(lines, c.SHA256+"  "+c.Filename)
		src.Builds = append(src.Builds, buildOut{
			Architecture: c.Arch,
			Filename:     c.Filename,
			SHA256:       c.SHA256,
			Size:         c.Size,
			Input:        inputOut{Filename: c.InputName, URL: c.InputURL, SHA256: c.InputSHA256, Size: c.InputSize},
			Warnings:     c.Warnings,
		})
	}
	sums := filepath.Join(dir, "SHA256SUMS.txt")
	source := filepath.Join(dir, "source.json")
	if err := os.WriteFile(sums, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		return "", "", err
	}
	raw, err := jsonIndent(src)
	if err != nil {
		return "", "", err
	}
	if err := os.WriteFile(source, raw, 0o644); err != nil {
		return "", "", err
	}
	return sums, source, nil
}

func repackNotes(opts repackOptions, tag string, cabs []repackCab) string {
	var b strings.Builder
	fmt.Fprintf(&b, "**UNOFFICIAL repack** of the Microsoft Edge %s channel full package **%s** into Fixed Version style cabinets.\n\n", opts.Channel, opts.Version)
	b.WriteString("This is not a Microsoft Fixed Version release. It is a prerelease and is never marked latest. Rise serves it only through an explicit pin.\n\n")
	b.WriteString("| Architecture | File | SHA-256 | Size | Input package SHA-256 |\n| --- | --- | --- | --- | --- |\n")
	for _, c := range cabs {
		fmt.Fprintf(&b, "| %s | `%s` | `%s` | %.1f MiB | `%s` |\n", c.Arch, c.Filename, c.SHA256, float64(c.Size)/(1024*1024), c.InputSHA256)
	}
	b.WriteString("\nOriginal Microsoft URLs and hashes are in `source.json`.\n\n")
	b.WriteString("These binaries are Microsoft software and include the Google Widevine CDM. See the repository README (Manual repacks) for the license risks.\n")
	return b.String()
}

func printRepackSummary(opts repackOptions, res *repackResult) {
	log.Printf("Repack %s ready in %s", res.Tag, res.Dir)
	for _, c := range res.Cabs {
		log.Printf("  %s  %s", c.SHA256, c.Filename)
	}
	files := repackUploadFiles(res)
	log.Printf("Would run: gh release create %s --prerelease --latest=false --title %q --notes-file %s %s",
		res.Tag, repackTitle(opts.Version, opts.Channel), res.NotesPath, strings.Join(files, " "))
}

func repackTitle(version, channel string) string {
	return fmt.Sprintf("UNOFFICIAL WebView2 repack %s (%s channel)", version, channel)
}

func repackUploadFiles(res *repackResult) []string {
	var files []string
	for _, c := range res.Cabs {
		files = append(files, c.Path)
	}
	return append(files, res.SumsPath, res.Source)
}

func checkRepackTagFree(repo, version, channel string) error {
	published, err := existingReleases(repo)
	if err != nil {
		return err
	}
	tag := repackTag(version, channel)
	if _, ok := published[tag]; ok {
		return fmt.Errorf("release %s already exists; delete it by hand first if you really want to rebuild it", tag)
	}
	if rel, ok := published[version]; ok && len(cabAssets(rel)) > 0 {
		return fmt.Errorf("an official Fixed Version release %s already exists; use it instead of a repack", version)
	}
	return nil
}

func publishRepack(repo string, res *repackResult) error {
	version := strings.SplitN(res.Tag, repackTagSeparator, 2)[0]
	channel := strings.SplitN(res.Tag, repackTagSeparator, 2)[1]
	args := []string{
		"release", "create", res.Tag,
		"--repo", repo,
		"--prerelease",
		"--latest=false",
		"--title", repackTitle(version, channel),
		"--notes-file", res.NotesPath,
	}
	args = append(args, repackUploadFiles(res)...)
	log.Printf("Publishing prerelease %s", res.Tag)
	return runGHStream(args...)
}
