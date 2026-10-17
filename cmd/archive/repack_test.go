package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckRepackURL(t *testing.T) {
	ok := []string{
		"http://msedge.f.tlu.dl.delivery.mp.microsoft.com/filestreamingservice/files/d84e5244-0188-4708-816a-ea8103378ecb?P1=1&P2=404",
		"https://msedge.sf.dl.delivery.mp.microsoft.com/filestreamingservice/files/x/Microsoft.WebView2.FixedVersionRuntime.1.2.3.4.x64.cab",
		"http://msedge.b.tlu.dl.delivery.mp.microsoft.com/filestreamingservice/files/abc",
		"http://other.dl.delivery.mp.microsoft.com/files/abc",
	}
	for _, raw := range ok {
		if err := checkRepackURL(raw); err != nil {
			t.Errorf("expected %s to be allowed: %v", raw, err)
		}
	}
	bad := []string{
		"https://msedge.sf.dl.delivery.mp.microsoft.com.evil.example/x",
		"https://dl.delivery.mp.microsoft.com.evil.example/x",
		"https://evil.example/msedge.sf.dl.delivery.mp.microsoft.com",
		"https://xdl.delivery.mp.microsoft.com/x",
		"ftp://msedge.sf.dl.delivery.mp.microsoft.com/x",
		"https://user:pw@msedge.sf.dl.delivery.mp.microsoft.com/x",
		"https://msedge.sf.dl.delivery.mp.microsoft.com:8443/x",
		"http://127.0.0.1/x",
		"https://github.com/ciderapp/WebView2-Archives/releases/download/x/y.cab",
	}
	for _, raw := range bad {
		if err := checkRepackURL(raw); err == nil {
			t.Errorf("expected %s to be rejected", raw)
		}
	}
}

const fakeSHA = "85030b0e334a04453d688847af64f6f3caad1ec78af6dcebcb3d80fe461336b1"

func TestParseRepackFlags(t *testing.T) {
	u := "http://msedge.f.tlu.dl.delivery.mp.microsoft.com/filestreamingservice/files/abc"
	opts, err := parseRepackFlags([]string{"-version", "156.0.4314.30", "-channel", "Beta", "-x64-url", u, "-x64-sha256", strings.ToUpper(fakeSHA)})
	if err != nil {
		t.Fatal(err)
	}
	if opts.Publish || len(opts.Inputs) != 1 || opts.Inputs[0].Arch != "x64" || opts.Inputs[0].SHA256 != fakeSHA || opts.Channel != "beta" {
		t.Fatalf("unexpected options: %+v", opts)
	}
	cases := map[string][]string{
		"no arch":          {"-version", "156.0.4314.30", "-channel", "beta"},
		"no sha":           {"-version", "156.0.4314.30", "-channel", "beta", "-x64-url", u},
		"short sha":        {"-version", "156.0.4314.30", "-channel", "beta", "-x64-url", u, "-x64-sha256", "abc"},
		"sha without url":  {"-version", "156.0.4314.30", "-channel", "beta", "-x86-sha256", fakeSHA},
		"bad host":         {"-version", "156.0.4314.30", "-channel", "beta", "-x64-url", "https://example.com/x.exe", "-x64-sha256", fakeSHA},
		"bad version":      {"-version", "156.0.4314", "-channel", "beta", "-x64-url", u, "-x64-sha256", fakeSHA},
		"bad channel":      {"-version", "156.0.4314.30", "-channel", "nightly", "-x64-url", u, "-x64-sha256", fakeSHA},
		"publish no ack":   {"-version", "156.0.4314.30", "-channel", "beta", "-x64-url", u, "-x64-sha256", fakeSHA, "-publish"},
		"stray positional": {"-version", "156.0.4314.30", "-channel", "beta", "-x64-url", u, "-x64-sha256", fakeSHA, "extra"},
	}
	for name, args := range cases {
		if _, err := parseRepackFlags(args); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
	if _, err := parseRepackFlags([]string{"-version", "156.0.4314.30", "-channel", "beta", "-x64-url", u, "-x64-sha256", fakeSHA, "-publish", "-acknowledge-license-risk"}); err != nil {
		t.Errorf("publish with ack should parse: %v", err)
	}
}

func allowLoopback(t *testing.T) {
	t.Helper()
	repackHostAllowedOverride = func(host string) bool { return host == "127.0.0.1" }
	t.Cleanup(func() { repackHostAllowedOverride = nil })
}

func sha(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func TestDownloadVerified(t *testing.T) {
	allowLoopback(t)
	body := []byte("not really an installer")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(body) }))
	defer srv.Close()
	dir := t.TempDir()
	dest := filepath.Join(dir, "pkg.exe")

	if err := downloadVerified(context.Background(), srv.URL+"/f", dest, fakeSHA); err == nil || !strings.Contains(err.Error(), "mismatch") {
		t.Fatalf("expected mismatch error, got %v", err)
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatalf("mismatched download must not be kept")
	}
	if _, err := os.Stat(dest + ".part"); !os.IsNotExist(err) {
		t.Fatalf("partial file must be removed")
	}
	if err := downloadVerified(context.Background(), srv.URL+"/f", dest, sha(body)); err != nil {
		t.Fatalf("expected success: %v", err)
	}
}

func TestDownloadRedirectToDisallowedHost(t *testing.T) {
	repackHostAllowedOverride = func(host string) bool { return host == "127.0.0.1" }
	t.Cleanup(func() { repackHostAllowedOverride = nil })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://evil.example/pkg.exe", http.StatusFound)
	}))
	defer srv.Close()
	err := downloadVerified(context.Background(), srv.URL+"/f", filepath.Join(t.TempDir(), "x"), fakeSHA)
	if err == nil || !strings.Contains(err.Error(), "not allowed") {
		t.Fatalf("expected redirect to be refused, got %v", err)
	}
}

type fixtureOpts struct {
	version     string
	arch        string
	noWidevine  bool
	noWebView   bool
	packedLayer bool // add .rsrc/B7/MSEDGE.PACKED.7Z nesting like the real installer
}

// makeFakeEdgePackage builds a tiny archive with the same nesting as a real
// MicrosoftEdge_<ARCH>_<ver>.exe: outer -> MSEDGE.7z -> Chrome-bin/<ver>/...
func makeFakeEdgePackage(t *testing.T, o fixtureOpts) []byte {
	t.Helper()
	root := t.TempDir()
	ver := filepath.Join(root, "Chrome-bin", o.version)
	files := map[string]string{
		o.version + ".manifest":  "<assembly/>",
		"msedge.dll":             "dll",
		"msedge.exe":             "browser",
		"msedgewebview2.exe.sig": "sig",
		"EBWebView/" + o.arch + "/EmbeddedBrowserWebView.dll": "eb",
		"Locales/en-US.pak": "pak",
	}
	if !o.noWebView {
		files["msedgewebview2.exe"] = "webview"
	}
	if !o.noWidevine {
		files["WidevineCdm/manifest.json"] = `{"version":"4.10.3050.1"}`
		files["WidevineCdm/LICENSE"] = "Google"
		files["WidevineCdm/_platform_specific/win_"+o.arch+"/widevinecdm.dll"] = "cdm"
		files["WidevineCdm/_platform_specific/win_"+o.arch+"/widevinecdm.dll.sig"] = "sig"
	}
	for rel, content := range files {
		p := filepath.Join(ver, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	sevenZip := func(dir, out string, inputs ...string) {
		args := append([]string{"a", "-bd", "-y", out}, inputs...)
		cmd := exec.Command("7z", args...)
		cmd.Dir = dir
		if b, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("7z a: %v\n%s", err, b)
		}
	}
	sevenZip(root, filepath.Join(root, "MSEDGE.7z"), "Chrome-bin")
	inner := "MSEDGE.7z"
	if o.packedLayer {
		packed := filepath.Join(root, ".rsrc", "B7")
		_ = os.MkdirAll(packed, 0o755)
		sevenZip(root, filepath.Join(packed, "MSEDGE.PACKED.7Z"), "MSEDGE.7z")
		inner = ".rsrc"
	}
	outer := filepath.Join(root, "outer.7z")
	sevenZip(root, outer, inner)
	b, err := os.ReadFile(outer)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func needTools(t *testing.T) {
	t.Helper()
	for _, tool := range []string{"7z", "gcab"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not installed", tool)
		}
	}
}

func TestBuildRepackEndToEnd(t *testing.T) {
	needTools(t)
	allowLoopback(t)
	const version = "156.0.4314.30"
	pkgs := map[string][]byte{
		"x64":   makeFakeEdgePackage(t, fixtureOpts{version: version, arch: "x64"}),
		"arm64": makeFakeEdgePackage(t, fixtureOpts{version: version, arch: "arm64", packedLayer: true}),
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(pkgs[strings.TrimPrefix(r.URL.Path, "/files/")])
	}))
	defer srv.Close()

	opts := repackOptions{Version: version, Channel: "beta", OutDir: t.TempDir()}
	for _, arch := range []string{"arm64", "x64"} {
		opts.Inputs = append(opts.Inputs, repackInput{Arch: arch, URL: srv.URL + "/files/" + arch, SHA256: sha(pkgs[arch])})
	}
	res, err := buildRepack(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if res.Tag != "156.0.4314.30-evergreen-beta" {
		t.Fatalf("tag = %s", res.Tag)
	}
	if len(res.Cabs) != 2 || res.Cabs[0].Arch != "x64" || res.Cabs[1].Arch != "arm64" {
		t.Fatalf("unexpected cabs: %+v", res.Cabs)
	}
	for _, c := range res.Cabs {
		want := "Microsoft.WebView2.FixedVersionRuntime." + version + "." + c.Arch + ".cab"
		if c.Filename != want || !cabNameRe.MatchString(c.Filename) {
			t.Fatalf("cab name %s, want %s", c.Filename, want)
		}
		out, err := exec.Command("gcab", "-t", c.Path).Output()
		if err != nil {
			t.Fatal(err)
		}
		listing := string(out)
		folder := strings.TrimSuffix(want, ".cab") + `\`
		for _, rel := range []string{"msedgewebview2.exe", `WidevineCdm\manifest.json`, `WidevineCdm\_platform_specific\win_` + c.Arch + `\widevinecdm.dll`, `EBWebView\` + c.Arch + `\EmbeddedBrowserWebView.dll`} {
			if !strings.Contains(listing, folder+rel) {
				t.Errorf("%s missing %s\n%s", c.Filename, rel, listing)
			}
		}
		if strings.Contains(listing, folder+"msedge.exe\n") {
			t.Errorf("%s still contains msedge.exe", c.Filename)
		}
		if got, _ := sha256File(c.Path); got != c.SHA256 {
			t.Errorf("recorded sha mismatch for %s", c.Filename)
		}
	}

	sums, _ := os.ReadFile(res.SumsPath)
	for _, c := range res.Cabs {
		if !strings.Contains(string(sums), c.SHA256+"  "+c.Filename) {
			t.Errorf("SHA256SUMS.txt missing %s", c.Filename)
		}
	}
	var src map[string]any
	raw, _ := os.ReadFile(res.Source)
	if err := json.Unmarshal(raw, &src); err != nil {
		t.Fatal(err)
	}
	if src["kind"] != "evergreen-repack" || src["channel"] != "beta" || src["version"] != version || src["created_at"] == "" {
		t.Fatalf("bad source.json: %s", raw)
	}
	builds := src["builds"].([]any)
	first := builds[0].(map[string]any)["input"].(map[string]any)
	if first["sha256"] != sha(pkgs["x64"]) || !strings.HasPrefix(first["url"].(string), srv.URL) {
		t.Fatalf("source.json input not recorded: %v", first)
	}
	if first["filename"] != "MicrosoftEdge_X64_156.0.4314.30.exe" {
		t.Fatalf("unexpected input filename %v", first["filename"])
	}
	notes, _ := os.ReadFile(res.NotesPath)
	if !strings.Contains(string(notes), "UNOFFICIAL") {
		t.Fatalf("release notes must say UNOFFICIAL")
	}
	for _, f := range repackUploadFiles(res) {
		base := filepath.Base(f)
		if base == "latest-version.txt" || base == "x64.cab" || strings.HasPrefix(base, "Microsoft.WebView2.FixedVersionRuntime.x") {
			t.Fatalf("repack must never upload latest aliases: %s", base)
		}
	}
}

func TestBuildRepackFailsClosed(t *testing.T) {
	needTools(t)
	allowLoopback(t)
	const version = "156.0.4314.30"
	cases := map[string]fixtureOpts{
		"no widevine": {version: version, arch: "x64", noWidevine: true},
		"no webview":  {version: version, arch: "x64", noWebView: true},
		"wrong ver":   {version: "156.0.4314.23", arch: "x64"},
	}
	for name, fo := range cases {
		pkg := makeFakeEdgePackage(t, fo)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(pkg) }))
		opts := repackOptions{Version: version, Channel: "beta", OutDir: t.TempDir(),
			Inputs: []repackInput{{Arch: "x64", URL: srv.URL + "/p", SHA256: sha(pkg)}}}
		if _, err := buildRepack(context.Background(), opts); err == nil {
			t.Errorf("%s: expected failure", name)
		}
		srv.Close()
	}
	// Hash mismatch with a local file never reaches extraction.
	pkg := makeFakeEdgePackage(t, fixtureOpts{version: version, arch: "x64"})
	local := filepath.Join(t.TempDir(), "pkg.exe")
	_ = os.WriteFile(local, pkg, 0o644)
	opts := repackOptions{Version: version, Channel: "beta", OutDir: t.TempDir(),
		Inputs: []repackInput{{Arch: "x64", URL: "http://127.0.0.1/p", File: local, SHA256: fakeSHA}}}
	if _, err := buildRepack(context.Background(), opts); err == nil || !strings.Contains(err.Error(), "mismatch") {
		t.Errorf("expected mismatch for local file, got %v", err)
	}
}

func TestRepacksNeverBecomeLatest(t *testing.T) {
	cab := func(v, arch string) ghAsset {
		return ghAsset{Name: "Microsoft.WebView2.FixedVersionRuntime." + v + "." + arch + ".cab"}
	}
	published := map[string]*ghRelease{
		"155.0.4283.62": {TagName: "155.0.4283.62", Assets: []ghAsset{cab("155.0.4283.62", "x64")}},
		"156.0.4314.30-evergreen-beta": {TagName: "156.0.4314.30-evergreen-beta", Prerelease: true,
			Assets: []ghAsset{cab("156.0.4314.30", "x64")}},
		"157.0.1.1": {TagName: "157.0.1.1", Prerelease: true, Assets: []ghAsset{cab("157.0.1.1", "x64")}},
	}
	v, _, err := newestArchived(published)
	if err != nil || v != "155.0.4283.62" {
		t.Fatalf("newestArchived = %s, %v", v, err)
	}
	up, err := upcomingLatest(published, nil)
	if err != nil || up != "155.0.4283.62" {
		t.Fatalf("upcomingLatest = %s, %v", up, err)
	}
	if !isRepackTag("156.0.4314.30-evergreen-beta") || isRepackTag("156.0.4314.30") {
		t.Fatal("isRepackTag")
	}
	if officialRelease("156.0.4314.30-evergreen-beta", &ghRelease{}) {
		t.Fatal("repack tag must not count as official")
	}
	_ = url.URL{}
}
