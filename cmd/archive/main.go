package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	catalogPrimary  = "https://developer.microsoft.com/microsoft-edge/api/webview2"
	catalogFallback = "https://explore.microsoft.com/microsoft-edge/api/webview2"
	officialPage    = "https://developer.microsoft.com/en-us/microsoft-edge/webview2/"
	userAgent       = "ciderapp-WebView2-Archives/1.0 (+https://github.com/ciderapp/WebView2-Archives)"
	browserUA       = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0.0.0 Safari/537.36"
)

var (
	catalogURLs    = []string{catalogPrimary, catalogFallback}
	userAgents     = []string{userAgent, browserUA}
	expectedArches = []string{"x86", "x64", "arm64"}
	cabNameRe      = regexp.MustCompile(`(?i)^Microsoft\.WebView2\.FixedVersionRuntime\.(?P<version>\d+(?:\.\d+){3})\.(?P<arch>x86|x64|arm64)\.cab$`)
)

type catalogEntry struct {
	Version string  `json:"version"`
	Builds  []build `json:"builds"`
}

type build struct {
	Architecture string `json:"architecture"`
	URL          string `json:"url"`
}

type ghRelease struct {
	TagName     string    `json:"tag_name"`
	HTMLURL     string    `json:"html_url"`
	PublishedAt string    `json:"published_at"`
	Assets      []ghAsset `json:"assets"`
}

type ghAsset struct {
	Name               string `json:"name"`
	Size               int64  `json:"size"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

type artifact struct {
	Architecture string
	Filename     string
	Path         string
	SourceURL    string
	SHA256       string
	Size         int64
}

type downloadJob struct {
	URL      string
	Filename string
	DestDir  string
}

func main() {
	log.SetFlags(0)
	if err := run(); err != nil {
		log.Fatalf("error: %v", err)
	}
}

func run() error {
	repo := os.Getenv("GITHUB_REPOSITORY")
	if repo == "" {
		return fmt.Errorf("GITHUB_REPOSITORY is required")
	}
	if os.Getenv("GH_TOKEN") == "" && os.Getenv("GITHUB_TOKEN") == "" {
		return fmt.Errorf("GH_TOKEN or GITHUB_TOKEN is required")
	}
	if _, err := exec.LookPath("aria2c"); err != nil {
		return fmt.Errorf("aria2c is required on PATH: %w", err)
	}
	if _, err := exec.LookPath("gh"); err != nil {
		return fmt.Errorf("gh is required on PATH: %w", err)
	}

	root, err := os.Getwd()
	if err != nil {
		return err
	}
	downloadRoot := filepath.Join(root, ".downloads")

	catalog, err := fetchMicrosoftCatalog()
	if err != nil {
		return err
	}
	sort.Slice(catalog, func(i, j int) bool {
		return versionLess(catalog[i].Version, catalog[j].Version)
	})
	log.Printf("Microsoft versions: %s", joinVersions(catalog))

	published, err := existingReleases(repo)
	if err != nil {
		return err
	}

	type pendingVersion struct {
		entry     catalogEntry
		release   *ghRelease
		missing   []build
		already   map[string]ghAsset
		dir       string
		artifacts []artifact
	}

	var pending []pendingVersion
	var jobs []downloadJob
	for _, entry := range catalog {
		release := published[entry.Version]
		already := cabAssets(release)
		var missing []build
		for _, b := range entry.Builds {
			arch := strings.ToLower(b.Architecture)
			if arch == "" || b.URL == "" {
				continue
			}
			name, err := filenameFromURL(b.URL)
			if err != nil || !cabNameRe.MatchString(name) {
				log.Printf("  Skipping unexpected filename from %s", b.URL)
				continue
			}
			if _, ok := already[arch]; ok {
				log.Printf("  %s %s already archived", entry.Version, arch)
				continue
			}
			missing = append(missing, build{Architecture: arch, URL: b.URL})
		}
		if len(missing) == 0 {
			if release != nil {
				log.Printf("Release %s is complete", entry.Version)
				continue
			}
			log.Printf("No cabinets listed for %s", entry.Version)
			continue
		}

		dir := filepath.Join(downloadRoot, entry.Version)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		item := pendingVersion{
			entry:   entry,
			release: release,
			missing: missing,
			already: already,
			dir:     dir,
		}
		hashes := map[string]string{}
		if release != nil {
			hashes = hashesFromRelease(*release)
			for arch, asset := range already {
				sourceURL := buildURLForArch(entry, arch)
				if sourceURL == "" {
					sourceURL = asset.BrowserDownloadURL
				}
				sha := hashes[asset.Name]
				art := artifact{
					Architecture: arch,
					Filename:     asset.Name,
					SourceURL:    sourceURL,
					SHA256:       sha,
					Size:         asset.Size,
				}
				if sha == "" {
					log.Printf("  Re-fetching %s to hash", asset.Name)
					jobs = append(jobs, downloadJob{
						URL:      asset.BrowserDownloadURL,
						Filename: asset.Name,
						DestDir:  dir,
					})
					art.Path = filepath.Join(dir, asset.Name)
				}
				item.artifacts = append(item.artifacts, art)
			}
		}
		for _, b := range missing {
			name, _ := filenameFromURL(b.URL)
			jobs = append(jobs, downloadJob{URL: b.URL, Filename: name, DestDir: dir})
			item.artifacts = append(item.artifacts, artifact{
				Architecture: b.Architecture,
				Filename:     name,
				Path:         filepath.Join(dir, name),
				SourceURL:    b.URL,
			})
		}
		pending = append(pending, item)
	}

	if len(jobs) > 0 {
		log.Printf("Downloading %d cabinet(s) with aria2 (parallel + split)", len(jobs))
		if err := downloadWithAria2(downloadRoot, jobs); err != nil {
			return err
		}
	}

	archivedAny := false
	for i := range pending {
		item := &pending[i]
		if err := finalizeArtifacts(item.artifacts); err != nil {
			return err
		}
		sortArtifacts(item.artifacts)
		sumsPath, sourcePath, err := writeSidecarFiles(item.dir, item.entry.Version, item.artifacts)
		if err != nil {
			return err
		}
		if err := createOrUpdateRelease(repo, item.entry.Version, item.artifacts, []string{sumsPath, sourcePath}, false); err != nil {
			return err
		}
		archivedAny = true
	}

	local := map[string][]artifact{}
	for i := range pending {
		local[pending[i].entry.Version] = pending[i].artifacts
	}
	if err := promoteLatest(repo, downloadRoot, local); err != nil {
		return err
	}
	if _, err := rebuildIndexes(root, repo); err != nil {
		return err
	}
	if archivedAny {
		log.Print("Archive run complete (new or updated releases published)")
	} else {
		log.Print("Archive run complete (no new cabinets)")
	}
	return nil
}

func fetchMicrosoftCatalog() ([]catalogEntry, error) {
	var errs []string
	client := &http.Client{Timeout: 30 * time.Second}
	for _, ua := range userAgents {
		for _, raw := range catalogURLs {
			req, err := http.NewRequest(http.MethodGet, raw, nil)
			if err != nil {
				errs = append(errs, err.Error())
				continue
			}
			req.Header.Set("User-Agent", ua)
			req.Header.Set("Accept", "application/json")
			req.Header.Set("Referer", officialPage)
			resp, err := client.Do(req)
			if err != nil {
				errs = append(errs, fmt.Sprintf("%s: %v", raw, err))
				continue
			}
			body, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			if err != nil {
				errs = append(errs, fmt.Sprintf("%s: %v", raw, err))
				continue
			}
			if resp.StatusCode != http.StatusOK {
				errs = append(errs, fmt.Sprintf("%s: HTTP %d", raw, resp.StatusCode))
				continue
			}
			var parsed []catalogEntry
			if err := json.Unmarshal(body, &parsed); err != nil {
				errs = append(errs, fmt.Sprintf("%s: %v", raw, err))
				continue
			}
			var versions []catalogEntry
			for _, entry := range parsed {
				if entry.Version == "" || len(entry.Builds) == 0 {
					continue
				}
				versions = append(versions, entry)
			}
			if len(versions) > 0 {
				log.Printf("Fetched Microsoft catalog from %s (%d version(s))", raw, len(versions))
				return versions, nil
			}
			errs = append(errs, raw+": no usable versions")
		}
	}
	return nil, fmt.Errorf("unable to fetch WebView2 catalog:\n%s", strings.Join(errs, "\n"))
}

func existingReleases(repo string) (map[string]*ghRelease, error) {
	out, err := runGH("api", "repos/"+repo+"/releases", "--paginate")
	if err != nil {
		return nil, err
	}
	var list []ghRelease
	if err := json.Unmarshal(out, &list); err != nil {
		return nil, fmt.Errorf("parse releases: %w", err)
	}
	releases := make(map[string]*ghRelease, len(list))
	for i := range list {
		if list[i].TagName != "" {
			rel := list[i]
			releases[rel.TagName] = &rel
		}
	}
	return releases, nil
}

func cabAssets(release *ghRelease) map[string]ghAsset {
	found := map[string]ghAsset{}
	if release == nil {
		return found
	}
	for _, asset := range release.Assets {
		m := cabNameRe.FindStringSubmatch(asset.Name)
		if m == nil {
			continue
		}
		found[strings.ToLower(m[2])] = asset
	}
	return found
}

func hashesFromRelease(release ghRelease) map[string]string {
	for _, asset := range release.Assets {
		if asset.Name != "SHA256SUMS.txt" || asset.BrowserDownloadURL == "" {
			continue
		}
		text, err := fetchText(asset.BrowserDownloadURL)
		if err != nil {
			return map[string]string{}
		}
		hashes := map[string]string{}
		for _, line := range strings.Split(text, "\n") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				hashes[fields[len(fields)-1]] = fields[0]
			}
		}
		return hashes
	}
	return map[string]string{}
}

func fetchText(raw string) (string, error) {
	req, err := http.NewRequest(http.MethodGet, raw, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "text/plain")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return string(body), nil
}

func downloadWithAria2(downloadRoot string, jobs []downloadJob) error {
	if err := os.MkdirAll(downloadRoot, 0o755); err != nil {
		return err
	}
	inputPath := filepath.Join(downloadRoot, "aria2-input.txt")
	var b strings.Builder
	for _, job := range jobs {
		if err := os.MkdirAll(job.DestDir, 0o755); err != nil {
			return err
		}
		fmt.Fprintf(&b, "%s\n  dir=%s\n  out=%s\n", job.URL, job.DestDir, job.Filename)
	}
	if err := os.WriteFile(inputPath, []byte(b.String()), 0o644); err != nil {
		return err
	}

	args := []string{
		"--input-file=" + inputPath,
		"--max-concurrent-downloads=6",
		"--max-connection-per-server=16",
		"--split=16",
		"--min-split-size=8M",
		"--continue=true",
		"--auto-file-renaming=false",
		"--allow-overwrite=true",
		"--file-allocation=none",
		"--timeout=60",
		"--retry-wait=2",
		"--max-tries=5",
		"--summary-interval=15",
		"--console-log-level=notice",
		"--user-agent=" + userAgent,
		"--header=Referer: " + officialPage,
		"--header=Accept: application/octet-stream",
	}
	cmd := exec.Command("aria2c", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("aria2c failed: %w", err)
	}
	return nil
}

func finalizeArtifacts(artifacts []artifact) error {
	var (
		mu  sync.Mutex
		err error
		wg  sync.WaitGroup
	)
	for i := range artifacts {
		art := &artifacts[i]
		if art.Path == "" {
			if art.SHA256 == "" {
				return fmt.Errorf("missing hash for %s", art.Filename)
			}
			continue
		}
		wg.Add(1)
		go func(a *artifact) {
			defer wg.Done()
			info, statErr := os.Stat(a.Path)
			if statErr != nil {
				mu.Lock()
				if err == nil {
					err = fmt.Errorf("downloaded file missing: %s: %w", a.Path, statErr)
				}
				mu.Unlock()
				return
			}
			if info.Size() == 0 {
				mu.Lock()
				if err == nil {
					err = fmt.Errorf("empty download: %s", a.Path)
				}
				mu.Unlock()
				return
			}
			sum, hashErr := sha256File(a.Path)
			if hashErr != nil {
				mu.Lock()
				if err == nil {
					err = hashErr
				}
				mu.Unlock()
				return
			}
			a.SHA256 = sum
			a.Size = info.Size()
			log.Printf("  %s (%.1f MiB) %s", a.Filename, float64(a.Size)/(1024*1024), a.SHA256)
		}(art)
	}
	wg.Wait()
	return err
}

func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func writeSidecarFiles(dir, version string, artifacts []artifact) (string, string, error) {
	var lines []string
	type sourceBuild struct {
		Architecture string `json:"architecture"`
		Filename     string `json:"filename"`
		URL          string `json:"url"`
		SHA256       string `json:"sha256"`
		Size         int64  `json:"size"`
	}
	source := struct {
		Version      string        `json:"version"`
		FetchedAt    string        `json:"fetched_at"`
		Catalog      string        `json:"catalog"`
		OfficialPage string        `json:"official_page"`
		Builds       []sourceBuild `json:"builds"`
	}{
		Version:      version,
		FetchedAt:    time.Now().UTC().Truncate(time.Second).Format(time.RFC3339),
		Catalog:      catalogPrimary,
		OfficialPage: officialPage,
	}
	for _, art := range artifacts {
		lines = append(lines, art.SHA256+"  "+art.Filename)
		source.Builds = append(source.Builds, sourceBuild{
			Architecture: art.Architecture,
			Filename:     art.Filename,
			URL:          art.SourceURL,
			SHA256:       art.SHA256,
			Size:         art.Size,
		})
	}
	sumsPath := filepath.Join(dir, "SHA256SUMS.txt")
	sourcePath := filepath.Join(dir, "source.json")
	if err := os.WriteFile(sumsPath, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		return "", "", err
	}
	raw, err := json.MarshalIndent(source, "", "  ")
	if err != nil {
		return "", "", err
	}
	if err := os.WriteFile(sourcePath, append(raw, '\n'), 0o644); err != nil {
		return "", "", err
	}
	return sumsPath, sourcePath, nil
}

func releaseNotes(version string, artifacts []artifact) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Microsoft Edge WebView2 Fixed Version Runtime **%s**\n\n", version)
	b.WriteString("Unofficial archive of the Fixed Version cabinets published by Microsoft.\n\n")
	b.WriteString("| Architecture | File | SHA-256 | Size |\n| --- | --- | --- | --- |\n")
	for _, art := range artifacts {
		fmt.Fprintf(&b, "| %s | `%s` | `%s` | %.1f MiB |\n", art.Architecture, art.Filename, art.SHA256, float64(art.Size)/(1024*1024))
	}
	fmt.Fprintf(&b, "\nSource catalog: %s\nOfficial download page: %s\n\n", catalogPrimary, officialPage)
	b.WriteString("These binaries are Microsoft software. See the repository README for the legal disclaimer.\n")
	return b.String()
}

func createOrUpdateRelease(repo, version string, artifacts []artifact, sidecar []string, makeLatest bool) error {
	notesPath := filepath.Join(filepath.Dir(sidecar[0]), "RELEASE_NOTES.md")
	if err := os.WriteFile(notesPath, []byte(releaseNotes(version, artifacts)), 0o644); err != nil {
		return err
	}
	var files []string
	for _, art := range artifacts {
		if art.Path != "" {
			files = append(files, art.Path)
		}
	}
	files = append(files, sidecar...)

	view := exec.Command("gh", "release", "view", version, "--repo", repo)
	if token := ghToken(); token != "" {
		view.Env = append(os.Environ(), "GH_TOKEN="+token)
	}
	if err := view.Run(); err != nil {
		args := []string{
			"release", "create", version,
			"--repo", repo,
			"--title", "WebView2 Fixed Version " + version,
			"--notes-file", notesPath,
		}
		if makeLatest {
			args = append(args, "--latest")
		} else {
			args = append(args, "--latest=false")
		}
		args = append(args, files...)
		log.Printf("Creating release %s", version)
		return runGHStream(args...)
	}

	log.Printf("Updating existing release %s", version)
	edit := []string{
		"release", "edit", version,
		"--repo", repo,
		"--title", "WebView2 Fixed Version " + version,
		"--notes-file", notesPath,
	}
	if makeLatest {
		edit = append(edit, "--latest")
	}
	if err := runGHStream(edit...); err != nil {
		return err
	}
	upload := append([]string{"release", "upload", version, "--repo", repo, "--clobber"}, files...)
	return runGHStream(upload...)
}

func markLatest(repo, version string) error {
	log.Printf("Marking %s as GitHub latest release", version)
	return runGHStream("release", "edit", version, "--repo", repo, "--latest")
}

func newestArchived(releases map[string]*ghRelease) (string, *ghRelease, error) {
	var tags []string
	for tag, rel := range releases {
		if _, err := parseVersion(tag); err != nil {
			continue
		}
		if len(cabAssets(rel)) == 0 {
			continue
		}
		tags = append(tags, tag)
	}
	if len(tags) == 0 {
		return "", nil, fmt.Errorf("no archived releases to mark as latest")
	}
	sort.Slice(tags, func(i, j int) bool {
		return versionLess(tags[i], tags[j])
	})
	newest := tags[len(tags)-1]
	return newest, releases[newest], nil
}

func latestAssetURL(repo, name string) string {
	return "https://github.com/" + repo + "/releases/latest/download/" + name
}

func aliasesCurrent(release *ghRelease, version string) bool {
	if release == nil {
		return false
	}
	names := map[string]string{}
	for _, asset := range release.Assets {
		names[asset.Name] = asset.BrowserDownloadURL
	}
	for _, arch := range expectedArches {
		if names[arch+".cab"] == "" {
			return false
		}
	}
	raw, ok := names["latest-version.txt"]
	if !ok {
		return false
	}
	text, err := fetchText(raw)
	if err != nil {
		return false
	}
	return strings.TrimSpace(text) == version
}

func promoteLatest(repo, downloadRoot string, local map[string][]artifact) error {
	published, err := existingReleases(repo)
	if err != nil {
		return err
	}
	version, release, err := newestArchived(published)
	if err != nil {
		return err
	}
	if err := markLatest(repo, version); err != nil {
		return err
	}
	if aliasesCurrent(release, version) {
		log.Printf("Stable /releases/latest/download aliases already point at %s", version)
		return nil
	}
	return ensureLatestAliases(repo, downloadRoot, version, local[version])
}

func ensureLatestAliases(repo, downloadRoot, version string, local []artifact) error {
	sources := map[string]string{}
	for _, art := range local {
		if art.Path != "" {
			if _, err := os.Stat(art.Path); err == nil {
				sources[art.Architecture] = art.Path
			}
		}
	}

	needDownload := false
	for _, arch := range expectedArches {
		if sources[arch] == "" {
			needDownload = true
			break
		}
	}
	if needDownload {
		srcDir := filepath.Join(downloadRoot, "latest-source", version)
		if err := os.MkdirAll(srcDir, 0o755); err != nil {
			return err
		}
		log.Printf("Fetching %s cabinets to build stable latest aliases", version)
		if err := runGHStream(
			"release", "download", version,
			"--repo", repo,
			"--dir", srcDir,
			"--pattern", "Microsoft.WebView2.FixedVersionRuntime.*.cab",
			"--clobber",
		); err != nil {
			return err
		}
		entries, err := os.ReadDir(srcDir)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			m := cabNameRe.FindStringSubmatch(entry.Name())
			if m == nil {
				continue
			}
			sources[strings.ToLower(m[2])] = filepath.Join(srcDir, entry.Name())
		}
	}

	aliasDir := filepath.Join(downloadRoot, "latest-aliases")
	if err := os.MkdirAll(aliasDir, 0o755); err != nil {
		return err
	}
	versionFile := filepath.Join(aliasDir, "latest-version.txt")
	if err := os.WriteFile(versionFile, []byte(version+"\n"), 0o644); err != nil {
		return err
	}
	toUpload := []string{versionFile}
	for _, arch := range expectedArches {
		src := sources[arch]
		if src == "" {
			return fmt.Errorf("missing %s cabinet while promoting latest", arch)
		}
		for _, name := range []string{
			arch + ".cab",
			"Microsoft.WebView2.FixedVersionRuntime." + arch + ".cab",
		} {
			dst := filepath.Join(aliasDir, name)
			if err := copyFile(src, dst); err != nil {
				return err
			}
			toUpload = append(toUpload, dst)
		}
	}

	log.Printf("Uploading stable latest aliases for %s", version)
	args := append([]string{"release", "upload", version, "--repo", repo, "--clobber"}, toUpload...)
	return runGHStream(args...)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}

func rebuildIndexes(root, repo string) (bool, error) {
	published, err := existingReleases(repo)
	if err != nil {
		return false, err
	}
	type assetOut struct {
		Architecture        string `json:"architecture"`
		Filename            string `json:"filename"`
		Size                int64  `json:"size"`
		SHA256              string `json:"sha256"`
		BrowserDownloadURL  string `json:"browser_download_url"`
	}
	type releaseOut struct {
		Version     string     `json:"version"`
		Tag         string     `json:"tag"`
		HTMLURL     string     `json:"html_url"`
		PublishedAt string     `json:"published_at"`
		Assets      []assetOut `json:"assets"`
	}
	var entries []releaseOut
	for tag, release := range published {
		if _, err := parseVersion(tag); err != nil {
			continue
		}
		hashes := hashesFromRelease(*release)
		var assets []assetOut
		for _, asset := range release.Assets {
			m := cabNameRe.FindStringSubmatch(asset.Name)
			if m == nil {
				continue
			}
			assets = append(assets, assetOut{
				Architecture:       strings.ToLower(m[2]),
				Filename:           asset.Name,
				Size:               asset.Size,
				SHA256:             hashes[asset.Name],
				BrowserDownloadURL: asset.BrowserDownloadURL,
			})
		}
		sort.Slice(assets, func(i, j int) bool {
			return archIndex(assets[i].Architecture) < archIndex(assets[j].Architecture)
		})
		if len(assets) == 0 {
			continue
		}
		entries = append(entries, releaseOut{
			Version:     tag,
			Tag:         tag,
			HTMLURL:     release.HTMLURL,
			PublishedAt: release.PublishedAt,
			Assets:      assets,
		})
	}
	sort.Slice(entries, func(i, j int) bool {
		return !versionLess(entries[i].Version, entries[j].Version)
	})

	generatedAt := time.Now().UTC().Truncate(time.Second).Format(time.RFC3339)
	var latestVersion, latestHTML string
	var latestAssets []assetOut
	if len(entries) > 0 {
		latestVersion = entries[0].Version
		latestHTML = entries[0].HTMLURL
		latestAssets = entries[0].Assets
	}
	catalog := map[string]any{
		"generated_at": generatedAt,
		"source":       catalogPrimary,
		"latest":       latestVersion,
		"releases":     entries,
	}
	permalinks := map[string]string{}
	if latestVersion != "" {
		permalinks = map[string]string{
			"x86":     latestAssetURL(repo, "x86.cab"),
			"x64":     latestAssetURL(repo, "x64.cab"),
			"arm64":   latestAssetURL(repo, "arm64.cab"),
			"version": latestAssetURL(repo, "latest-version.txt"),
		}
	}
	latest := map[string]any{
		"generated_at": generatedAt,
		"version":      latestVersion,
		"html_url":     latestHTML,
		"permalinks":   permalinks,
		"assets":       latestAssets,
	}
	changed, err := writeJSONIfChanged(filepath.Join(root, "catalog.json"), catalog)
	if err != nil {
		return false, err
	}
	latestChanged, err := writeJSONIfChanged(filepath.Join(root, "latest.json"), latest)
	if err != nil {
		return false, err
	}
	changed = changed || latestChanged
	if changed {
		log.Print("Updated catalog.json and latest.json")
	} else {
		log.Print("Catalog index unchanged")
	}
	return changed, nil
}

func writeJSONIfChanged(path string, data map[string]any) (bool, error) {
	incoming := cloneWithoutGeneratedAt(data)
	if raw, err := os.ReadFile(path); err == nil {
		var current map[string]any
		if json.Unmarshal(raw, &current) == nil {
			if mapsEqualJSON(cloneWithoutGeneratedAt(current), incoming) {
				return false, nil
			}
		}
	}
	out, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return false, err
	}
	return true, os.WriteFile(path, append(out, '\n'), 0o644)
}

func cloneWithoutGeneratedAt(data map[string]any) map[string]any {
	out := make(map[string]any, len(data))
	for k, v := range data {
		if k != "generated_at" {
			out[k] = v
		}
	}
	return out
}

func mapsEqualJSON(a, b map[string]any) bool {
	left, err1 := json.Marshal(a)
	right, err2 := json.Marshal(b)
	return err1 == nil && err2 == nil && string(left) == string(right)
}

func runGH(args ...string) ([]byte, error) {
	cmd := exec.Command("gh", args...)
	if token := ghToken(); token != "" {
		cmd.Env = append(os.Environ(), "GH_TOKEN="+token)
	}
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return nil, fmt.Errorf("gh %s: %s", strings.Join(args, " "), strings.TrimSpace(string(ee.Stderr)))
		}
		return nil, err
	}
	return out, nil
}

func runGHStream(args ...string) error {
	cmd := exec.Command("gh", args...)
	if token := ghToken(); token != "" {
		cmd.Env = append(os.Environ(), "GH_TOKEN="+token)
	}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("gh %s: %w", strings.Join(args, " "), err)
	}
	return nil
}

func ghToken() string {
	if v := os.Getenv("GH_TOKEN"); v != "" {
		return v
	}
	return os.Getenv("GITHUB_TOKEN")
}

func filenameFromURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	name, err := url.PathUnescape(filepath.Base(u.Path))
	if err != nil {
		return "", err
	}
	return name, nil
}

func buildURLForArch(entry catalogEntry, arch string) string {
	for _, b := range entry.Builds {
		if strings.EqualFold(b.Architecture, arch) {
			return b.URL
		}
	}
	return ""
}

func sortArtifacts(artifacts []artifact) {
	sort.Slice(artifacts, func(i, j int) bool {
		return archIndex(artifacts[i].Architecture) < archIndex(artifacts[j].Architecture)
	})
}

func archIndex(arch string) int {
	for i, item := range expectedArches {
		if item == arch {
			return i
		}
	}
	return len(expectedArches)
}

func parseVersion(value string) ([]int, error) {
	parts := strings.Split(value, ".")
	out := make([]int, len(parts))
	for i, part := range parts {
		n, err := strconv.Atoi(part)
		if err != nil {
			return nil, err
		}
		out[i] = n
	}
	return out, nil
}

func versionLess(a, b string) bool {
	pa, errA := parseVersion(a)
	pb, errB := parseVersion(b)
	if errA != nil || errB != nil {
		return a < b
	}
	n := len(pa)
	if len(pb) < n {
		n = len(pb)
	}
	for i := 0; i < n; i++ {
		if pa[i] != pb[i] {
			return pa[i] < pb[i]
		}
	}
	return len(pa) < len(pb)
}

func joinVersions(entries []catalogEntry) string {
	parts := make([]string, len(entries))
	for i, entry := range entries {
		parts[i] = entry.Version
	}
	return strings.Join(parts, ", ")
}
