[CmdletBinding()]
param()

$ErrorActionPreference = 'Stop'
$repoRoot = (Resolve-Path (Join-Path $PSScriptRoot '..')).Path
$toolRoot = Join-Path $repoRoot 'test/acceptance/tools'
$lock = Get-Content (Join-Path $toolRoot 'tools.lock.json') -Raw | ConvertFrom-Json
$binRoot = Join-Path $toolRoot 'bin'
$go = (Get-Command go -ErrorAction Stop).Source
$env:GOCACHE = Join-Path $env:TEMP 'omnilsp-go-cache-acceptance'
$env:GOTELEMETRY = 'off'
$runId = if ($env:OMNILSP_RUN_ID) { $env:OMNILSP_RUN_ID } else { (Get-Date).ToUniversalTime().ToString('yyyyMMddTHHmmssZ') + '-tools' }
$evidenceDir = Join-Path $repoRoot "test/acceptance/evidence/$runId"

function Write-ToolInstallReport([string] $Decision, [string] $NeovimStatus, [string] $Detail) {
    New-Item -ItemType Directory -Force -Path $evidenceDir | Out-Null
    $report = [ordered]@{
        schema_version = 1
        run_id = $runId
        decision = $Decision
        checks = @(
            [ordered]@{ id = 'npm-project-tools'; status = 'passed'; detail = 'locked packages installed by npm ci' },
            [ordered]@{ id = 'neovim-pinned-install'; status = $NeovimStatus; detail = $Detail }
        )
        finished_at = (Get-Date).ToUniversalTime().ToString('o')
    }
    $report | ConvertTo-Json -Depth 8 | Set-Content -LiteralPath (Join-Path $evidenceDir 'tool-install.json') -Encoding utf8
}

if (-not (Get-Command npm -ErrorAction SilentlyContinue)) {
    throw 'Node.js/npm is required to install the pinned language-server tools.'
}

$cachePath = Join-Path $toolRoot '.npm-cache'
$editorRoot = Join-Path $repoRoot 'editors/vscode'
npm ci --prefix $editorRoot --cache $cachePath
if ($LASTEXITCODE -ne 0) { throw "npm ci for the VS Code extension failed with exit code $LASTEXITCODE" }
npm ci --prefix $toolRoot --cache $cachePath
if ($LASTEXITCODE -ne 0) { throw "npm ci failed with exit code $LASTEXITCODE" }

New-Item -ItemType Directory -Force -Path $binRoot | Out-Null
$wrapper = Join-Path $binRoot 'acceptance-tool-wrapper.exe'
$oldModuleMode = $env:GO111MODULE
try {
    # This tiny launcher imports only the standard library. Building it outside
    # module mode keeps the installer self-contained and avoids writing to a
    # global module cache.
    $env:GO111MODULE = 'off'
    & $go build -trimpath -o $wrapper ./test/acceptance/tools/wrapper
    if ($LASTEXITCODE -ne 0) { throw "build pinned language-server wrapper failed with exit code $LASTEXITCODE" }
} finally {
    $env:GO111MODULE = $oldModuleMode
}
foreach ($name in @('pyright-langserver', 'typescript-language-server', 'tsc')) {
    Copy-Item -LiteralPath $wrapper -Destination (Join-Path $binRoot "$name.exe") -Force
}

$nvim = $lock.acceptanceTools.neovim
$archive = Join-Path $binRoot 'nvim-win64.zip'
$nvimRoot = Join-Path $binRoot "nvim-$($nvim.version)"
$nvimExe = Join-Path $nvimRoot 'nvim-win64/bin/nvim.exe'
New-Item -ItemType Directory -Force -Path $binRoot | Out-Null

try {
    if (-not (Test-Path $archive)) {
        Invoke-WebRequest -Uri $nvim.url -OutFile $archive
    }
    $actualHash = (Get-FileHash -Algorithm SHA256 -LiteralPath $archive).Hash.ToLowerInvariant()
    if ($actualHash -ne $nvim.sha256) {
        throw "Neovim archive SHA-256 mismatch: expected $($nvim.sha256), got $actualHash"
    }

    if (-not (Test-Path $nvimExe)) {
        Expand-Archive -LiteralPath $archive -DestinationPath $nvimRoot -Force
    }
    if (-not (Test-Path $nvimExe)) {
        throw "Pinned Neovim $($nvim.version) archive did not contain the expected executable: $nvimExe"
    }
    $nvimVersionOutput = & $nvimExe --version | Select-Object -First 1
    if ($nvimVersionOutput.Trim() -cne "NVIM v$($nvim.version)") {
        throw "Unexpected Neovim version: $nvimVersionOutput"
    }
} catch {
    Write-ToolInstallReport 'not_verified' 'not_verified' $_.Exception.Message
    throw
}

$pyright = Join-Path $toolRoot 'node_modules/.bin/pyright-langserver.cmd'
$tsls = Join-Path $toolRoot 'node_modules/.bin/typescript-language-server.cmd'
if (-not (Test-Path $pyright) -or -not (Test-Path $tsls)) {
    throw 'One or more pinned language-server entry points were not installed.'
}

Write-Output "Pinned acceptance tools installed under $toolRoot"
Write-Output "Neovim: $nvimVersionOutput"
Write-Output "Pyright: $($lock.acceptanceTools.pyright.version)"
Write-Output "typescript-language-server: $($lock.acceptanceTools.typescriptLanguageServer.version)"
Write-Output "TypeScript: $($lock.acceptanceTools.typescript.version)"
Write-ToolInstallReport 'passed' 'passed' "Neovim $nvimVersionOutput; SHA-256 $actualHash"
