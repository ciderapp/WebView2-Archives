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

- [`catalog.json`](catalog.json): every archived version
- [`latest.json`](latest.json): the newest archived version, plus `/releases/latest/download` permalinks

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

Manual repacks (see below) are listed only under an optional `prereleases` key in `catalog.json`, each with `kind: "evergreen-repack"`, `channel` and `prerelease: true`. They never appear in `releases`, `latest` or `latest.json`.

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
4. Missing cabinets across versions are downloaded in one [aria2](https://aria2.github.io/) session (16 connections per file, up to 16 files at once, resume enabled), then checksummed in parallel.
5. GitHub Releases are created in parallel. Cabinet uploads share a pool of 8 connections so multiple versions ship at once. The highest archived version is marked as GitHub’s **latest** release and given stable aliases (`x64.cab`, `x86.cab`, `arm64.cab`) during that same publish so `/releases/latest/download/x64.cab` always follows the newest archive.
6. `catalog.json` and `latest.json` are rebuilt from this repo's releases.

## Manual repacks (unofficial, emergency only)

`cmd/archive repack` builds Fixed Version style cabinets from Microsoft Edge **full update packages** (`MicrosoftEdge_<ARCH>_<version>.exe`) for builds where Microsoft has not published a Fixed Version yet. Read the license section below before publishing anything.

What it does, per architecture (any subset of `x64`, `x86`, `arm64`):

1. Checks the URL: `http` or `https`, no credentials, no IP address, no custom port, and a Microsoft download host only (`msedge.sf.dl.delivery.mp.microsoft.com`, `msedge.b.tlu.dl.delivery.mp.microsoft.com`, `msedge.f.tlu.dl.delivery.mp.microsoft.com`, or any other `*.dl.delivery.mp.microsoft.com`). Every redirect hop is checked the same way.
2. Downloads the package and computes SHA256 while streaming. The expected SHA256 is mandatory. On any mismatch the download is deleted and the run fails. (The Edge update API hands out plain `http` URLs, so the hash is the only integrity check.)
3. Extracts it with 7-Zip (`MicrosoftEdge_*.exe` contains `MSEDGE.PACKED.7Z`, which contains `MSEDGE.7z`, which contains `Chrome-bin/<version>/`).
4. Renames `Chrome-bin/<version>/` to `Microsoft.WebView2.FixedVersionRuntime.<version>.<arch>/` (the folder name the Cider client looks for after `expand -F:*`) and removes `msedge.exe`, which neither the Fixed Version cabinet nor the Evergreen WebView2 install ships.
5. Fails unless `msedgewebview2.exe`, `WidevineCdm/manifest.json` and `WidevineCdm/_platform_specific/win_<arch>/widevinecdm.dll` are present. Missing `<version>.manifest`, `msedge.dll`, `.sig` files, `WidevineCdm/LICENSE` or `EBWebView/` are reported as warnings in the log and in `source.json`.
6. Builds `Microsoft.WebView2.FixedVersionRuntime.<version>.<arch>.cab` with `gcab -z` (MSZIP) and checks that every entry sits under that single folder.
7. Writes `SHA256SUMS.txt` (of the output cabinets), `source.json` (`kind: evergreen-repack`, version, tag, channel, `created_at`, original Microsoft URLs, input filenames, sizes and SHA256s) and `RELEASE_NOTES.md`.

Dependencies: Go 1.25, `7z` (7-Zip or p7zip) and `gcab` on `PATH`. `gh` plus `GH_TOKEN` and `GITHUB_REPOSITORY` are only needed with `-publish`.

### Safety rails

- **Dry run by default.** Without `-publish` nothing is sent to GitHub; the tool prints the exact `gh release create` it would run.
- `-publish` also requires `-acknowledge-license-risk`.
- Releases are tagged `<version>-evergreen-<channel>` (for example `156.0.4314.30-evergreen-beta`), created with `--prerelease --latest=false`, and never receive the `x64.cab` / `latest-version.txt` aliases.
- The scheduled archive job ignores prereleases and `-evergreen-` tags when choosing GitHub latest and the aliases. `catalog.json` lists repacks under a separate `prereleases` key (with `kind`, `channel`, `prerelease: true`), never in `releases` and never as `latest`. `latest.json` never mentions them. Rise follow-latest therefore cannot pick one up.
- Publishing refuses to run if the repack tag already exists, or if an official Fixed Version release with the same version exists.
- The workflow (`.github/workflows/repack.yml`) is `workflow_dispatch` only. There is no schedule.

### Example: 156.0.4314.30 (Beta channel)

Get current URLs first. The Edge update API signs them and they expire (the ones below were issued Oct 17, 2026 and expire around 2026-10-24 04:08 UTC). The API is served with a certificate from Microsoft Root Certificate Authority 2011, which is not in most Linux CA bundles, so pass it explicitly:

```bash
curl -fsSL -o msroot2011.crt https://www.microsoft.com/pki/certs/MicRooCerAut2011_2011_03_22.crt
openssl x509 -inform der -in msroot2011.crt -out msroot2011.pem
for arch in x64 x86 arm64; do
  curl -fsS --cacert msroot2011.pem -X POST -H 'Content-Length: 0' \
    "https://msedge.api.cdp.microsoft.com/api/v1.1/internal/contents/Browser/namespaces/Default/names/msedge-beta-win-$arch/versions/156.0.4314.30/files?action=GenerateDownloadInfo&foregroundPriority=true" \
    | jq -r --arg a "$(echo $arch | tr a-z A-Z)" '.[] | select(.FileId == "MicrosoftEdge_\($a)_156.0.4314.30.exe") | [.FileId, .SizeInBytes, .Hashes.Sha256, .Url] | @tsv'
done
```

(The API returns `Hashes.Sha256` as base64. Convert it to hex with `echo '<base64>' | base64 -d | od -An -tx1 | tr -d ' \n'`.) Use only the full package (`MicrosoftEdge_<ARCH>_<version>.exe`), never a delta (`..._<version>_<from>.exe`).

Published values for 156.0.4314.30 (Beta):

| Arch | File | Size | SHA256 |
| --- | --- | --- | --- |
| x64 | `MicrosoftEdge_X64_156.0.4314.30.exe` | 208249208 | `85030b0e334a04453d688847af64f6f3caad1ec78af6dcebcb3d80fe461336b1` |
| x86 | `MicrosoftEdge_X86_156.0.4314.30.exe` | 184266104 | `ddc0c6e63288cacea84d761451e868adcb3870bb6d052038c3c5fc25afd467ce` |
| arm64 | `MicrosoftEdge_ARM64_156.0.4314.30.exe` | 211680120 | `4dfee5ee2281ad32c048f4141584a7387c1d04ed0d0a9b17cdd8cae47bfb6bb9` |

Dry run (recommended first step, builds everything locally under `.downloads/repack/`):

```bash
go build -o /tmp/webview2-archive ./cmd/archive
/tmp/webview2-archive repack -version 156.0.4314.30 -channel beta \
  -x64-url 'http://msedge.f.tlu.dl.delivery.mp.microsoft.com/filestreamingservice/files/d84e5244-0188-4708-816a-ea8103378ecb?P1=1792814930&P2=404&P3=2&P4=Q3duZm%2fLwyKGPhbwXpNPa7HEUBu1yHVOJwH10sJOP2%2fDM87aNPDUf0f9wA9CfazV2QuYuK9ehpSqqhlJ%2fkLkLQ%3d%3d' \
  -x64-sha256 85030b0e334a04453d688847af64f6f3caad1ec78af6dcebcb3d80fe461336b1 \
  -x86-url 'http://msedge.f.tlu.dl.delivery.mp.microsoft.com/filestreamingservice/files/f711bf05-ce76-4867-9b18-584161832fed?P1=1792814930&P2=404&P3=2&P4=GVTVfuL7tJv9hS75u0U32d%2b738ngJjCEUJZRPGUUhRhCKTNWYiMR73LKBi2OVqCGKFX0K1ZeKH7aCxN%2fHiPuyw%3d%3d' \
  -x86-sha256 ddc0c6e63288cacea84d761451e868adcb3870bb6d052038c3c5fc25afd467ce \
  -arm64-url 'http://msedge.f.tlu.dl.delivery.mp.microsoft.com/filestreamingservice/files/20fea05f-b0e9-4d40-9e16-98e16dfcbd7d?P1=1792814931&P2=404&P3=2&P4=nUzKqUim3nj%2f7qz8Xs53wiI1DfOvXSF6s0Og6ggRjSKMlqGJTyTZcAV90rmy0MKY235T%2fWnbd%2bluxUM%2bkbkUGw%3d%3d' \
  -arm64-sha256 4dfee5ee2281ad32c048f4141584a7387c1d04ed0d0a9b17cdd8cae47bfb6bb9
```

Test the cabinets on real Windows x64 and arm64 machines (expand, point `WEBVIEW2_BROWSER_EXECUTABLE_FOLDER` at the folder, play DRM content) before publishing. To publish, add `-publish -acknowledge-license-risk` (with `GH_TOKEN` and `GITHUB_REPOSITORY=ciderapp/WebView2-Archives` set). If the package is already on disk, `-<arch>-file <path>` skips the download; the SHA256 is still enforced and the URL is still recorded.

From GitHub Actions: **Actions > Repack WebView2 (manual, unofficial) > Run workflow**, with `version`, `channel`, the `*_url` / `*_sha256` inputs, and `publish` left off for the dry run.

### Serving a repack from Rise

Rise never selects a repack on its own. An operator pins it with explicit package URLs (`PUT /api/v1/updates/webview2` on the management API):

```json
{
  "runtimeVersion": "156.0.4314.30",
  "package_x64": "https://github.com/ciderapp/WebView2-Archives/releases/download/156.0.4314.30-evergreen-beta/Microsoft.WebView2.FixedVersionRuntime.156.0.4314.30.x64.cab",
  "package_arm64": "https://github.com/ciderapp/WebView2-Archives/releases/download/156.0.4314.30-evergreen-beta/Microsoft.WebView2.FixedVersionRuntime.156.0.4314.30.arm64.cab"
}
```

Because `runtimeVersion` is not in `catalog.json` `releases`, Rise serves the pinned record as stored. If Microsoft later publishes an official Fixed Version with the same number, Rise serves the official cabinets for that pin. Unpin with `{"followLatest": true}`.

### License risk (read before publishing)

- These are Beta channel (or other pre-release) Microsoft Edge builds, not a Microsoft Fixed Version release.
- Fixed Version license, section 2(b)(iii): "You must acquire all code, including any code obtained from a Microsoft URL, directly from Microsoft, including a Microsoft download site". Section 3(e): you may not "share, publish, distribute, or lease the software, provide the software as a stand-alone offering for others to use".
- Evergreen WebView2 Runtime license, section 8: "You may obtain updates only from Microsoft or authorized sources."
- The bundled `WidevineCdm/LICENSE` (Google): "You may not use, modify, sell, or otherwise distribute the Software without a separate license agreement with Google."
- Republishing a repack as a GitHub release is redistribution of Microsoft and Google binaries outside the Fixed Version program. Treat it as a short-lived stopgap, and remove it once Microsoft publishes the Fixed Version.

## Disclaimer

This is an **unofficial archive**. It is not affiliated with, endorsed by, or sponsored by Microsoft Corporation.

Microsoft, Microsoft Edge, WebView2, and related marks are trademarks of Microsoft Corporation. This project does not claim any ownership of those marks or of the WebView2 runtime.

The `.cab` files attached to GitHub Releases are **Microsoft software**. Downloading or using them remains subject to Microsoft's license terms and EULA. The MIT license in this repository covers only our scripts, workflows, and documentation, not Microsoft binaries.

Packages are provided as-is, with no warranty. Use at your own risk.

## License

[MIT](LICENSE) for original repository files (scripts, workflows, docs). Microsoft binaries are excluded.
