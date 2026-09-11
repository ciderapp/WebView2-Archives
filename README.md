# WebView2 Archives

An unofficial, automated archive of [Microsoft Edge WebView2](https://developer.microsoft.com/en-us/microsoft-edge/webview2/) **Fixed Version** runtime cabinets.

Microsoft only keeps a short sliding window of Fixed Version packages on the official download page. This repository polls that catalog and republishes each version as a GitHub Release so older builds remain downloadable after Microsoft removes them.

## What is archived

Each GitHub Release is one Fixed Version, with cabinets for:

- `x86`
- `x64`
- `arm64`

Filenames match Microsoft's:

```text
Microsoft.WebView2.FixedVersionRuntime.<version>.<arch>.cab
```

Every release also includes `SHA256SUMS.txt` and `source.json` (original Microsoft URLs and fetch time). The GitHub **latest** release additionally has stable aliases (`x64.cab`, `x86.cab`, `arm64.cab`) and `latest-version.txt`.

Evergreen standalone installers and the bootstrapper are **not** mirrored here. Use Microsoft's stable links if you need those:

| Package | Link |
| --- | --- |
| Bootstrapper | https://go.microsoft.com/fwlink/p/?LinkId=2124703 |
| Standalone x86 | https://go.microsoft.com/fwlink/?linkid=2099617 |
| Standalone x64 | https://go.microsoft.com/fwlink/?linkid=2124701 |
| Standalone ARM64 | https://go.microsoft.com/fwlink/?linkid=2099616 |

## How often it runs

GitHub Actions runs on a cron every 6 hours (`17 */6 * * *`, UTC) and on demand via **Actions → Archive WebView2 → Run workflow**.

Scheduled runs only fire after this workflow file is on the default branch (`main`). Overlapping jobs do not cancel each other.

The first successful run seeds every version Microsoft currently lists. Later runs only publish versions that are not already archived.

## Download a version

Browse [Releases](https://github.com/ciderapp/WebView2-Archives/releases).

Always-current permalinks (GitHub `/releases/latest`, updated whenever a newer Fixed Version is archived):

```text
https://github.com/ciderapp/WebView2-Archives/releases/latest/download/x64.cab
https://github.com/ciderapp/WebView2-Archives/releases/latest/download/x86.cab
https://github.com/ciderapp/WebView2-Archives/releases/latest/download/arm64.cab
https://github.com/ciderapp/WebView2-Archives/releases/latest/download/latest-version.txt
```

```bash
curl -fsSL -O "https://github.com/ciderapp/WebView2-Archives/releases/latest/download/x64.cab"
curl -fsSL "https://github.com/ciderapp/WebView2-Archives/releases/latest/download/latest-version.txt"
```

Unversioned Microsoft-style names are also attached to the latest release:

```text
https://github.com/ciderapp/WebView2-Archives/releases/latest/download/Microsoft.WebView2.FixedVersionRuntime.x64.cab
```

Pin a specific version instead:

```bash
gh release download 153.0.4234.32 --repo ciderapp/WebView2-Archives
curl -L -O "https://github.com/ciderapp/WebView2-Archives/releases/download/153.0.4234.32/Microsoft.WebView2.FixedVersionRuntime.153.0.4234.32.x64.cab"
```

Verify the download:

```bash
sha256sum -c SHA256SUMS.txt
```

## Expand a cabinet

Do **not** extract the `.cab` with File Explorer. That can break the folder layout WebView2 expects.

On Windows:

```bat
expand Microsoft.WebView2.FixedVersionRuntime.153.0.4234.32.x64.cab -F:* C:\WebView2
```

That produces a folder such as:

```text
C:\WebView2\Microsoft.WebView2.FixedVersionRuntime.153.0.4234.32.x64\msedgewebview2.exe
```

Point your app at that folder:

- Win32: `CreateCoreWebView2EnvironmentWithOptions` `browserExecutableFolder`
- WPF / WinForms: `CoreWebView2Environment.CreateAsync(browserExecutableFolder, ...)`
- Any loader that honors it: `WEBVIEW2_BROWSER_EXECUTABLE_FOLDER`

See Microsoft's [Fixed Version distribution](https://learn.microsoft.com/en-us/microsoft-edge/webview2/concepts/distribution#details-about-the-fixed-version-runtime-distribution-mode) notes for permissions required on Windows 10 (runtime 120+).

## Machine-readable catalog

After each archive run the workflow updates two files on `main`:

- [`catalog.json`](catalog.json) — every archived version
- [`latest.json`](latest.json) — the newest archived version, plus `/releases/latest/download` permalinks

Raw URLs:

```text
https://raw.githubusercontent.com/ciderapp/WebView2-Archives/main/catalog.json
https://raw.githubusercontent.com/ciderapp/WebView2-Archives/main/latest.json
```

### `catalog.json` schema

```json
{
  "generated_at": "2026-09-11T19:00:00Z",
  "source": "https://developer.microsoft.com/microsoft-edge/api/webview2",
  "latest": "153.0.4234.32",
  "releases": [
    {
      "version": "153.0.4234.32",
      "tag": "153.0.4234.32",
      "html_url": "https://github.com/ciderapp/WebView2-Archives/releases/tag/153.0.4234.32",
      "published_at": "2026-09-11T19:00:00Z",
      "assets": [
        {
          "architecture": "x64",
          "filename": "Microsoft.WebView2.FixedVersionRuntime.153.0.4234.32.x64.cab",
          "size": 308367262,
          "sha256": "...",
          "browser_download_url": "https://github.com/ciderapp/WebView2-Archives/releases/download/153.0.4234.32/Microsoft.WebView2.FixedVersionRuntime.153.0.4234.32.x64.cab"
        }
      ]
    }
  ]
}
```

`catalog.json` and `latest.json` stay empty until the first workflow run publishes releases.

## Official Microsoft links

- [Download the WebView2 Runtime](https://developer.microsoft.com/en-us/microsoft-edge/webview2/)
- [Distribute your app and the WebView2 Runtime](https://learn.microsoft.com/en-us/microsoft-edge/webview2/concepts/distribution)
- [Evergreen vs. Fixed Version](https://learn.microsoft.com/en-us/microsoft-edge/webview2/concepts/evergreen-vs-fixed-version)
- [WebView2 Runtime release notes](https://learn.microsoft.com/en-us/microsoft-edge/webview2/release-notes/runtime/)

Prefer Microsoft's page whenever the version you need is still listed there.

## How the pipeline works

1. `.github/workflows/archive.yml` runs on a 6-hour cron (and manually).
2. `cmd/archive` (Go) reads Microsoft's public catalog:
   - `https://developer.microsoft.com/microsoft-edge/api/webview2`
   - fallback `https://explore.microsoft.com/microsoft-edge/api/webview2`
3. Versions that already have a GitHub Release are skipped.
4. Missing cabinets across versions are downloaded in one [aria2](https://aria2.github.io/) session (16 connections per file, up to 6 files at once, resume enabled), then checksummed in parallel.
5. GitHub Releases are published oldest-first. The highest archived version is marked as GitHub’s **latest** release and given stable aliases (`x64.cab`, `x86.cab`, `arm64.cab`) so `/releases/latest/download/x64.cab` always follows the newest archive.
6. `catalog.json` and `latest.json` are rebuilt from this repo's releases.

## Disclaimer

This is an **unofficial archive**. It is not affiliated with, endorsed by, or sponsored by Microsoft Corporation.

Microsoft, Microsoft Edge, WebView2, and related marks are trademarks of Microsoft Corporation. This project does not claim any ownership of those marks or of the WebView2 runtime.

The `.cab` files attached to GitHub Releases are **Microsoft software**. Downloading or using them remains subject to Microsoft's license terms and EULA. The MIT license in this repository covers only our scripts, workflows, and documentation — not Microsoft binaries.

Packages are provided as-is, with no warranty. Use at your own risk.

## License

[MIT](LICENSE) for original repository files (scripts, workflows, docs). Microsoft binaries are excluded.
