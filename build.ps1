<#
.SYNOPSIS
    Build noapi-search-mcp.

.DESCRIPTION
    Formats, vets, tests and builds. By default it builds for this machine
    only, which is what you want while working; -All cross-compiles every
    released target, which is what the release pipeline does.

    The version is stamped into the binary with -ldflags, so `--version`
    reports something meaningful rather than "dev". Without -Version it is
    derived from git: the current tag if the commit has one, otherwise the
    short commit with a -dirty suffix when the tree has uncommitted changes.

.PARAMETER All
    Cross-compile for every released target - Windows and Linux, amd64 and
    arm64 - into dist/. Otherwise builds one binary for this machine.

.PARAMETER Version
    Version string to stamp in. Defaults to one derived from git.

.PARAMETER SkipTests
    Skip gofmt, vet and the tests. For a quick rebuild while iterating; not
    for anything you are going to hand to someone else.

.PARAMETER SelfTest
    After building, run the live compatibility checks against the upstream
    services. This makes real network requests and takes a minute or two.

.PARAMETER Clean
    Remove dist/ and the built binaries first.

.EXAMPLE
    .\build.ps1
    Build for this machine, after formatting, vetting and testing.

.EXAMPLE
    .\build.ps1 -All -Version 1.2.0
    Cross-compile every target as version 1.2.0.

.EXAMPLE
    .\build.ps1 -SkipTests -SelfTest
    Rebuild quickly, then check the scrapers still match the live services.
#>
[CmdletBinding()]
param(
    [switch]$All,
    [string]$Version,
    [switch]$SkipTests,
    [switch]$SelfTest,
    [switch]$Clean
)

# Stop on the first error, so a failed step cannot be mistaken for a
# successful build by whatever is reading the exit code.
$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

$AppName = 'noapi-search-mcp'
$Root = Split-Path -Parent $MyInvocation.MyCommand.Path
Push-Location $Root

function Write-Step($message) {
    Write-Host ""
    Write-Host "==> $message" -ForegroundColor Cyan
}

function Invoke-Step($description, [scriptblock]$action) {
    Write-Step $description
    & $action
    # A native command's failure does not throw, so the exit code is checked
    # explicitly. Without this, a failing test run would be reported as a
    # successful build.
    if ($LASTEXITCODE -ne 0) {
        throw "$description failed with exit code $LASTEXITCODE"
    }
}

# Invoke-Git runs a git command and returns its output, or $null if it failed.
#
# The error preference is lowered for the call and restored afterwards. Windows
# PowerShell 5.1 wraps anything a native command writes to stderr in an
# ErrorRecord, which under $ErrorActionPreference = 'Stop' becomes a
# terminating error - so `git describe` in a repository with no tags, which
# writes a perfectly ordinary "No names found" to stderr and is an expected
# outcome here, would otherwise abort the build.
function Invoke-Git {
    param([string[]]$Arguments)

    $previous = $ErrorActionPreference
    $ErrorActionPreference = 'Continue'
    try {
        $output = (& git @Arguments 2>&1 | Out-String)
    }
    finally {
        $ErrorActionPreference = $previous
    }
    if ($LASTEXITCODE -ne 0) { return $null }
    return $output.Trim()
}

# Resolve-Version derives a version from git when one was not given. A tagged
# commit gives the tag; anything else gives the short hash, with -dirty when
# the tree has uncommitted changes - so a binary can always be traced back to
# what produced it.
function Resolve-Version {
    if ($Version) { return $Version }
    if (-not (Get-Command git -ErrorAction SilentlyContinue)) { return 'dev' }
    if ($null -eq (Invoke-Git @('rev-parse', '--git-dir'))) { return 'dev' }

    $tag = Invoke-Git @('describe', '--tags', '--exact-match')
    if ($tag) { return $tag.TrimStart('v') }

    $commit = Invoke-Git @('rev-parse', '--short', 'HEAD')
    if (-not $commit) { return 'dev' }

    $suffix = ''
    if (Invoke-Git @('status', '--porcelain')) { $suffix = '-dirty' }
    return "dev-$commit$suffix"
}

try {
    if (-not (Get-Command go -ErrorAction SilentlyContinue)) {
        throw "Go is not installed, or not on PATH. Install it from https://go.dev/dl/"
    }

    $resolved = Resolve-Version
    Write-Host "$AppName build" -ForegroundColor Green
    Write-Host "  version   $resolved"
    Write-Host "  go        $((& go version) -replace '^go version ', '')"

    if ($Clean) {
        Write-Step 'Cleaning'
        Remove-Item -Recurse -Force -ErrorAction SilentlyContinue dist
        Remove-Item -Force -ErrorAction SilentlyContinue "$AppName.exe", $AppName
        Write-Host "  removed dist/ and any built binaries"
    }

    if (-not $SkipTests) {
        Write-Step 'Checking formatting'
        $unformatted = & gofmt -l .
        # The embedded Swagger UI assets are third-party and not Go, so they
        # are not gofmt's business.
        $unformatted = $unformatted | Where-Object { $_ -and $_ -notlike 'static*' }
        if ($unformatted) {
            $unformatted | ForEach-Object { Write-Host "  needs gofmt: $_" -ForegroundColor Yellow }
            throw "These files are not formatted. Run: gofmt -w ."
        }
        Write-Host "  all files are formatted"

        Invoke-Step 'Vetting' { & go vet ./... }
        Invoke-Step 'Testing' { & go test ./... }
    }
    else {
        Write-Host ""
        Write-Host "  skipping format, vet and tests (-SkipTests)" -ForegroundColor Yellow
    }

    # -trimpath keeps absolute build paths out of the binary; -s -w drop the
    # symbol and DWARF tables, which is most of the size.
    $ldflags = "-s -w -X main.version=$resolved"
    $buildArgs = @('build', '-trimpath', '-ldflags', $ldflags)

    if ($All) {
        # Every target the release publishes. CGO is off so each is a single
        # static file that runs without a matching libc.
        $targets = @(
            @{ os = 'windows'; arch = 'amd64'; ext = '.exe' },
            @{ os = 'windows'; arch = 'arm64'; ext = '.exe' },
            @{ os = 'linux'; arch = 'amd64'; ext = '' },
            @{ os = 'linux'; arch = 'arm64'; ext = '' }
        )
        New-Item -ItemType Directory -Force -Path dist | Out-Null

        foreach ($target in $targets) {
            $output = "dist/$AppName-$($target.os)-$($target.arch)$($target.ext)"
            Write-Step "Building $($target.os)/$($target.arch)"

            $env:GOOS = $target.os
            $env:GOARCH = $target.arch
            $env:CGO_ENABLED = '0'
            try {
                & go @buildArgs -o $output .
                if ($LASTEXITCODE -ne 0) { throw "build failed for $($target.os)/$($target.arch)" }
            }
            finally {
                # Cleared whatever happened, so a failed cross-compile does
                # not leave this shell building for the wrong platform.
                Remove-Item Env:GOOS, Env:GOARCH, Env:CGO_ENABLED -ErrorAction SilentlyContinue
            }
            $size = [math]::Round((Get-Item $output).Length / 1MB, 1)
            Write-Host "  $output  ($size MB)"
        }

        Write-Step 'Checksums'
        Push-Location dist
        try {
            $lines = Get-ChildItem -File | Where-Object { $_.Name -ne 'SHA256SUMS' } | ForEach-Object {
                "$((Get-FileHash $_.Name -Algorithm SHA256).Hash.ToLower())  $($_.Name)"
            }
            # ASCII with Unix line endings, so sha256sum -c on Linux accepts
            # the file rather than choking on a BOM or on carriage returns.
            [System.IO.File]::WriteAllText(
                (Join-Path (Get-Location) 'SHA256SUMS'),
                ($lines -join "`n") + "`n",
                [System.Text.Encoding]::ASCII)
            $lines | ForEach-Object { Write-Host "  $_" }
        }
        finally { Pop-Location }
    }
    else {
        # $IsWindows and friends are PowerShell 7 automatic variables and do
        # not exist in Windows PowerShell 5.1, where Set-StrictMode makes
        # reading one an error. The OS environment variable is set on Windows
        # in both, and on neither Linux nor macOS.
        $output = if ($env:OS -eq 'Windows_NT') { "$AppName.exe" } else { $AppName }
        Invoke-Step "Building for this machine" { & go @buildArgs -o $output . }
        $size = [math]::Round((Get-Item $output).Length / 1MB, 1)
        Write-Host "  $output  ($size MB)"

        Write-Step 'Verifying'
        & "./$output" --version
        if ($LASTEXITCODE -ne 0) { throw "the built binary would not report its version" }

        if ($SelfTest) {
            Write-Step 'Compatibility checks against the live services'
            Write-Host "  This makes real requests and takes a minute or two." -ForegroundColor DarkGray
            & "./$output" --selftest
            if ($LASTEXITCODE -ne 0) {
                # A broken check means a service changed its markup, which is
                # worth failing the build over: the binary compiles and does
                # not work. Rate limiting exits zero and does not reach here.
                throw "a compatibility check failed: a service is answering but this server cannot read it"
            }
        }
    }

    Write-Host ""
    Write-Host "Build succeeded." -ForegroundColor Green
    if (-not $All) {
        Write-Host "  Try:  ./$output --transport http     then open http://127.0.0.1:8780/docs"
    }
}
finally {
    Pop-Location
}
