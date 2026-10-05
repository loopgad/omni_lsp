[CmdletBinding()]
param(
    [ValidateSet('Fast', 'Soak30s', 'Soak10m', 'Start1h', 'SoakWorker', 'Status')]
    [string] $Phase = 'Fast',
    [string] $RunId,
    [string] $EvidenceDirectory,
    [string] $CandidateBinary,
    [ValidateSet('strict', 'stable')]
    [string] $S18Policy = 'strict'
)

$ErrorActionPreference = 'Stop'
$S18Policy = $S18Policy.ToLowerInvariant()
$repoRoot = (Resolve-Path (Join-Path $PSScriptRoot '..')).Path
if (-not $RunId) { $RunId = (Get-Date).ToUniversalTime().ToString('yyyyMMddTHHmmssZ') + '-' + [guid]::NewGuid().ToString('N').Substring(0, 8) }
if (-not $EvidenceDirectory) { $EvidenceDirectory = Join-Path $repoRoot "test/acceptance/evidence/$RunId" }
if (-not $CandidateBinary) { $CandidateBinary = Join-Path $EvidenceDirectory 'omnilsp.exe' }
$EvidenceDirectory = [IO.Path]::GetFullPath($EvidenceDirectory)
$CandidateBinary = [IO.Path]::GetFullPath($CandidateBinary)
New-Item -ItemType Directory -Force -Path $EvidenceDirectory | Out-Null

$go = $null
$script:AcceptanceOriginalPath = $env:PATH
$script:AcceptanceToolLock = $null
$script:AcceptanceResolvedPaths = [ordered]@{}
$env:GOTELEMETRY = 'off'
$env:GOCACHE = Join-Path $repoRoot '.tmp-gocache'
$env:GOMAXPROCS = '8'
$cpuModel = ''
try {
    $cpuModel = [string](Get-CimInstance Win32_Processor -ErrorAction Stop | Select-Object -First 1 -ExpandProperty Name)
} catch {
    $cpuModel = ''
}
if (-not $cpuModel) {
    try {
        $cpuModel = [string](Get-ItemProperty -LiteralPath 'Registry::HKEY_LOCAL_MACHINE\HARDWARE\DESCRIPTION\System\CentralProcessor\0' -Name ProcessorNameString -ErrorAction Stop).ProcessorNameString
    } catch {
        $cpuModel = ''
    }
}
if ($cpuModel) { $env:OMNILSP_CPU_MODEL = $cpuModel }
$localToolBin = Join-Path $repoRoot 'test/acceptance/tools/bin'
$env:PATH = "$localToolBin;$env:PATH"
$clientMatrixClients = @('vscode', 'neovim', 'emacs-eglot', 'helix', 'zed', 'sublime-lsp')
$clientMatrixLanguages = @('go', 'c', 'cpp', 'rust', 'python', 'typescript', 'javascript')

function Get-ClientMatrixCellIds {
    $ids = [Collections.Generic.List[string]]::new()
    foreach ($client in $clientMatrixClients) {
        foreach ($language in $clientMatrixLanguages) {
            $ids.Add("client/$client/$language")
        }
    }
    return @($ids)
}

function Get-ClientMatrixSummaryIds {
    $ids = [Collections.Generic.List[string]]::new()
    foreach ($client in @('vscode', 'neovim')) {
        foreach ($family in @('go', 'cpp', 'rust', 'python', 'typescript')) {
            $ids.Add("client/$client/$family")
        }
    }
    return @($ids)
}

function Get-ClientMatrixRequiredCheckIds {
    return @(Get-ClientMatrixCellIds)
}

function Assert-EmacsCellEvidence($Check, [string] $Language) {
    $expected = switch ($Language) {
        'cpp' { @('c', 'cpp') }
        'typescript' { @('javascript', 'javascriptreact', 'typescript', 'typescriptreact') }
        'javascript' { @('javascript', 'javascriptreact') }
        default { @($Language) }
    }
    $rows = @($Check.observed.subcases)
    if ($rows.Count -ne @($expected).Count -or
        [string]::Join(',', @($rows | ForEach-Object { [string]$_.name } | Sort-Object)) -cne [string]::Join(',', @($expected))) {
        throw "Emacs cell '$($Check.id)' lacks its complete native subcases."
    }
    foreach ($row in $rows) {
        $o = $row.observed
        if ([string]$row.status -cne 'passed' -or [string]$row.languageId -cne [string]$row.name -or
            [string]::IsNullOrWhiteSpace([string]$o.hover) -or [string]::IsNullOrWhiteSpace([string]$o.completion_candidates) -or
            [long]$o.definition_count -lt 1 -or [long]$o.reference_count -lt 2 -or
            [string]$o.rename_refusal -notmatch '-32803' -or
            $o.snapshot_epoch_before -isnot [long] -or $o.snapshot_epoch_after -isnot [long] -or
            [long]$o.snapshot_epoch_before -lt 0 -or
            [long]$o.snapshot_epoch_after -le [long]$o.snapshot_epoch_before -or
            $o.shutdown_request.responseReceived -isnot [bool] -or -not $o.shutdown_request.responseReceived -or
            $o.exit_notification_sent -isnot [bool] -or -not $o.exit_notification_sent -or
            [string]$o.server_process_exit_status -cne 'exit/0') {
            throw "Emacs cell '$($Check.id)' lacks semantic, sync, safe-rename or graceful-exit observations."
        }
        $scope = if ([string]$row.name -in @('c', 'cpp')) { 'clean_source_no_false_positive' } else { 'semantic_unresolved_name' }
        if ([string]$o.diagnostic_scope -cne $scope) { throw "Emacs cell '$($Check.id)' has an invalid diagnostic scope." }
        if ($scope -ceq 'clean_source_no_false_positive') {
            if ($o.published_count -isnot [long] -or $o.published_count -ne 0 -or [string]$o.deferred_capability -cne 'DEF-CCLSDIAG') {
                throw "Emacs cell '$($Check.id)' lacks the accepted clean-source diagnostic evidence."
            }
        } elseif ([string]::IsNullOrWhiteSpace([string]$o.diagnostic_type) -or
            [string]::IsNullOrWhiteSpace([string]$o.diagnostic_message) -or
            $o.diagnostic_snapshot_epoch_before -isnot [long] -or $o.diagnostic_snapshot_epoch_after -isnot [long] -or
            [long]$o.diagnostic_snapshot_epoch_before -lt 0 -or
            [long]$o.diagnostic_snapshot_epoch_after -le [long]$o.diagnostic_snapshot_epoch_before) {
            throw "Emacs cell '$($Check.id)' lacks a fresh client-visible semantic diagnostic."
        }
    }
}

function Assert-ClientMatrixContract($Report) {
    if ($null -eq $Report -or $null -eq $Report.checks) {
        throw 'Client report has no checks for the 42-cell matrix contract.'
    }
    $actual = @{}
    foreach ($check in @($Report.checks)) {
        $id = [string]$check.id
        if ([string]::IsNullOrWhiteSpace($id) -or $actual.ContainsKey($id)) {
            throw "Client report has a blank or duplicate matrix check ID '$id'."
        }
        $actual[$id] = $check
    }
    foreach ($id in @(Get-ClientMatrixRequiredCheckIds)) {
        if (-not $actual.ContainsKey($id)) {
            throw "Client report is missing required matrix check '$id'."
        }
    }
    foreach ($id in @(Get-ClientMatrixSummaryIds)) {
        if (-not $actual.ContainsKey($id)) {
            throw "Client report is missing backward-compatible family summary '$id'."
        }
    }
    foreach ($client in @('helix', 'zed', 'sublime-lsp')) {
        foreach ($language in $clientMatrixLanguages) {
            $id = "client/$client/$language"
            if ([string]$actual[$id].status -cne 'not_verified') {
                throw "Client report '$id' must remain not_verified until its native driver exists."
            }
        }
    }
    return $true
}

function Ensure-LocalToolWrappers {
    $toolRoot = Join-Path $repoRoot 'test/acceptance/tools'
    $required = @(
        'node_modules/pyright/langserver.index.js',
        'node_modules/typescript-language-server/lib/cli.mjs',
        'node_modules/typescript/bin/tsc'
    )
    foreach ($file in $required) {
        if (-not (Test-Path (Join-Path $toolRoot $file))) { return $false }
    }
    New-Item -ItemType Directory -Force -Path $localToolBin | Out-Null
    $wrapper = Join-Path $localToolBin 'acceptance-tool-wrapper.exe'
    Push-Location $repoRoot
    $oldModuleMode = $env:GO111MODULE
    try {
        $env:GO111MODULE = 'off'
        & $go build -trimpath -o $wrapper ./test/acceptance/tools/wrapper
        if ($LASTEXITCODE -ne 0) { throw "build pinned language-server wrapper failed with exit code $LASTEXITCODE" }
    } finally {
        $env:GO111MODULE = $oldModuleMode
        Pop-Location
    }
    foreach ($name in @('pyright-langserver', 'typescript-language-server', 'tsc')) {
        Copy-Item -LiteralPath $wrapper -Destination (Join-Path $localToolBin "$name.exe") -Force
    }
    return $true
}

function Get-CorpusHash {
    $root = Join-Path $repoRoot 'test/corpus/testdata'
    $files = Get-ChildItem -LiteralPath $root -File -Recurse | Sort-Object FullName
    $builder = [Text.StringBuilder]::new()
    foreach ($file in $files) {
        $relative = [IO.Path]::GetRelativePath($root, $file.FullName).Replace('\', '/')
        $hash = (Get-FileHash -Algorithm SHA256 -LiteralPath $file.FullName).Hash.ToLowerInvariant()
        [void] $builder.Append($relative).Append(" ").Append($hash).Append("`n")
    }
    $bytes = [Text.Encoding]::UTF8.GetBytes($builder.ToString())
    return [Convert]::ToHexString([Security.Cryptography.SHA256]::HashData($bytes)).ToLowerInvariant()
}

function Get-FileSha256([string] $Path) {
    return (Get-FileHash -Algorithm SHA256 -LiteralPath $Path).Hash.ToLowerInvariant()
}

function Test-CandidateBinaryIdentity([string] $Path, [string] $ExpectedHash) {
    if (-not (Test-Path -LiteralPath $Path -PathType Leaf)) {
        return [ordered]@{ id = 'candidate-binary-stability-after-tests'; status = 'failed'; detail = 'candidate binary is missing after acceptance tests' }
    }
    $observedHash = Get-FileSha256 $Path
    if ($observedHash -cne $ExpectedHash) {
        return [ordered]@{ id = 'candidate-binary-stability-after-tests'; status = 'failed'; detail = "candidate SHA-256 changed after tests: expected $ExpectedHash, observed $observedHash" }
    }
    return [ordered]@{ id = 'candidate-binary-stability-after-tests'; status = 'passed'; detail = $observedHash }
}

function Get-S18StableExceptionLimit([string] $Language, [string] $Operation) {
	if ($Language -cin @('c', 'cpp')) {
		if ($Operation -ceq 'completion_first_usable') { return @(80, 100, 125) }
		if ($Operation -ceq 'syntax_update_after_edit') { return @(60, 100, 150) }
	}
	return $null
}

function Test-S18StableExceptionID([string] $ID, [string] $RunId) {
	if ([string]::IsNullOrWhiteSpace($ID) -or [string]::IsNullOrWhiteSpace($RunId)) { return $false }
	$prefix = [regex]::Escape($RunId)
	return $ID -cmatch "^$prefix/S18/(c|cpp)/(completion_first_usable|syntax_update_after_edit)$"
}

function Test-S18PercentilesWithin([double[]] $Values, [double[]] $Limits) {
    if ($Values.Count -ne 3 -or $Limits.Count -ne 3) { return $false }
    for ($index = 0; $index -lt 3; $index++) {
        if ([double]::IsNaN($Values[$index]) -or [double]::IsInfinity($Values[$index]) -or $Values[$index] -lt 0 -or
            [double]::IsNaN($Limits[$index]) -or [double]::IsInfinity($Limits[$index]) -or $Limits[$index] -lt 0 -or
            $Values[$index] -gt $Limits[$index]) { return $false }
    }
    return $true
}

function Test-S18FiniteNumber($Value) {
    if (($Value -isnot [byte]) -and ($Value -isnot [sbyte]) -and ($Value -isnot [int16]) -and
        ($Value -isnot [uint16]) -and ($Value -isnot [int]) -and ($Value -isnot [uint32]) -and
        ($Value -isnot [long]) -and ($Value -isnot [ulong]) -and ($Value -isnot [single]) -and ($Value -isnot [double]) -and
        ($Value -isnot [decimal])) { return $false }
    $number = [double]$Value
    return -not [double]::IsNaN($number) -and -not [double]::IsInfinity($number)
}

function Test-S18FiniteNonnegativeNumber($Value) {
    return (Test-S18FiniteNumber $Value) -and [double]$Value -ge 0
}

function Test-S18IntegerValue($Value) {
    if (-not (Test-S18FiniteNonnegativeNumber $Value)) { return $false }
    $number = [double]$Value
    return $number -le [long]::MaxValue -and [Math]::Truncate($number) -eq $number
}

function Get-S18PercentileNS([double[]] $Values, [double] $Quantile) {
    if ($Values.Count -eq 0 -or $Quantile -le 0 -or $Quantile -gt 1) { throw 'S18 percentile input is empty or invalid.' }
    $ordered = [double[]]$Values.Clone()
    [Array]::Sort($ordered)
    $rank = [Math]::Ceiling($Quantile * $ordered.Count)
    return [double]$ordered[[Math]::Max(0, [int]$rank - 1)]
}

function Get-S18ABBAEligibility([string] $CheckId, $Check, $Sample, $Evidence) {
    $match = [regex]::Match($CheckId, '^.+/S18/(c|cpp)/(completion_first_usable|syntax_update_after_edit)$')
    if (-not $match.Success) { throw "ABBA evidence check '$CheckId' is outside the stable S18 scope." }
    $language = $match.Groups[1].Value
    $operation = $match.Groups[2].Value
    $exceptionLimits = Get-S18StableExceptionLimit $language $operation
    $strictLimits = if ($operation -ceq 'completion_first_usable') { @(40, 120, 250) } else { @(15, 50, 100) }
    $overheadLimits = @(5, 10, 20)
    $expectedMethod = if ($operation -ceq 'completion_first_usable') { 'textDocument/completion' } else { 'textDocument/documentSymbol' }

    if ($Sample.id -cne $CheckId -or $Sample.unit -cne 'ns/op' -or @($Sample.raw).Count -lt 6000 -or
        $Evidence.check_id -cne $CheckId -or $Evidence.fixture_id -cne $language -or
        $Evidence.operation -cne $operation -or $Evidence.method -cne $expectedMethod -or
        @($Evidence.rounds).Count -ne 3) {
        throw "ABBA evidence for '$CheckId' has missing or inconsistent candidate, operation, or round metadata."
    }

    $candidateRaw = [Collections.Generic.List[double]]::new()
    $upstreamRaw = [Collections.Generic.List[double]]::new()
    $overheadRaw = [Collections.Generic.List[double]]::new()
    $candidateStart = 0
    $seenRequestIDs = [Collections.Generic.HashSet[string]]::new([StringComparer]::Ordinal)
    $expectedOrder = @('candidate', 'upstream', 'upstream', 'candidate')

    for ($roundIndex = 0; $roundIndex -lt 3; $roundIndex++) {
        $round = $Evidence.rounds[$roundIndex]
        if (-not (Test-S18IntegerValue $round.index) -or [int]$round.index -ne $roundIndex -or
            @($round.order).Count -ne 4 -or @($round.legs).Count -ne 4) {
            throw "ABBA evidence for '$CheckId' round $roundIndex has invalid index, order, or leg count."
        }
        for ($legIndex = 0; $legIndex -lt 4; $legIndex++) {
            $leg = $round.legs[$legIndex]
            $role = $expectedOrder[$legIndex]
            if ([string]$round.order[$legIndex] -cne $role -or
                -not (Test-S18IntegerValue $leg.position) -or [int]$leg.position -ne $legIndex -or [string]$leg.role -cne $role) {
                throw "ABBA evidence for '$CheckId' round $roundIndex leg $legIndex is not in candidate/upstream/upstream/candidate order."
            }
            $requestIDs = @()
            $writeRaw = @()
            $waitRaw = @()
            $rawNS = @()
            if ($null -ne $leg.request_ids) { $requestIDs = @($leg.request_ids) }
            if ($null -ne $leg.write_raw_ns) { $writeRaw = @($leg.write_raw_ns) }
            if ($null -ne $leg.wait_raw_ns) { $waitRaw = @($leg.wait_raw_ns) }
            if ($null -ne $leg.raw_ns) { $rawNS = @($leg.raw_ns) }
            if ($role -ceq 'candidate') {
                if (-not (Test-S18IntegerValue $leg.sample_start) -or [long]$leg.sample_start -ne $candidateStart -or
                    -not (Test-S18IntegerValue $leg.sample_count) -or [long]$leg.sample_count -lt 1000 -or
                    $requestIDs.Count -ne [long]$leg.sample_count -or $writeRaw.Count -ne [long]$leg.sample_count -or
                    $waitRaw.Count -ne [long]$leg.sample_count -or $rawNS.Count -ne 0) {
                    throw "ABBA evidence for '$CheckId' round $roundIndex candidate leg has an invalid sample range or correlated timing arrays."
                }
                $end = [long]$leg.sample_start + [long]$leg.sample_count
                if ($end -lt [long]$leg.sample_start -or $end -gt @($Sample.raw).Count) {
                    throw "ABBA evidence for '$CheckId' round $roundIndex candidate range escapes the raw sample set."
                }
                for ($offset = 0; $offset -lt [long]$leg.sample_count; $offset++) {
                    $requestID = $requestIDs[$offset]
                    if ($requestID -is [string]) {
                        $requestKey = 'string:' + $requestID
                        if ([string]::IsNullOrWhiteSpace($requestID)) { throw "ABBA evidence for '$CheckId' contains an empty request ID." }
                    } elseif ($requestID -is [byte] -or $requestID -is [sbyte] -or $requestID -is [int16] -or $requestID -is [uint16] -or
                        $requestID -is [int] -or $requestID -is [uint32] -or $requestID -is [long] -or $requestID -is [ulong]) {
                        $requestKey = 'number:' + ([string]::Format([Globalization.CultureInfo]::InvariantCulture, '{0}', $requestID))
                    } elseif ($requestID -is [single] -or $requestID -is [double] -or $requestID -is [decimal]) {
                        if (-not (Test-S18FiniteNumber $requestID)) { throw "ABBA evidence for '$CheckId' has a non-finite numeric request ID." }
                        $requestKey = 'number:' + ([string]::Format([Globalization.CultureInfo]::InvariantCulture, '{0:R}', [double]$requestID))
                    } else {
                        throw "ABBA evidence for '$CheckId' contains a request ID that is not a JSON string or number."
                    }
                    if (-not $seenRequestIDs.Add($requestKey)) { throw "ABBA evidence for '$CheckId' contains duplicate candidate request IDs." }
                    $outer = [double]$Sample.raw[[long]$leg.sample_start + $offset]
                    $write = $writeRaw[$offset]
                    $wait = $waitRaw[$offset]
                    if (-not (Test-S18FiniteNumber $outer) -or $outer -le 0 -or
                        -not (Test-S18FiniteNonnegativeNumber $write) -or -not (Test-S18FiniteNonnegativeNumber $wait)) {
                        throw "ABBA evidence for '$CheckId' contains a non-finite or negative candidate timing."
                    }
                    $overhead = $outer - [double]$write - [double]$wait
                    if (-not (Test-S18FiniteNonnegativeNumber $overhead)) { throw "ABBA evidence for '$CheckId' contains a negative same-request candidate overhead." }
                    $candidateRaw.Add($outer)
                    $overheadRaw.Add($overhead)
                }
                $candidateStart = $end
            } else {
                if (-not (Test-S18IntegerValue $leg.sample_start) -or [long]$leg.sample_start -ne 0 -or
                    -not (Test-S18IntegerValue $leg.sample_count) -or [long]$leg.sample_count -ne 0 -or
                    $requestIDs.Count -ne 0 -or $writeRaw.Count -ne 0 -or $waitRaw.Count -ne 0 -or $rawNS.Count -lt 1000) {
                    throw "ABBA evidence for '$CheckId' round $roundIndex upstream leg has invalid raw samples or candidate-only fields."
                }
                foreach ($value in $rawNS) {
                    if (-not (Test-S18FiniteNumber $value) -or $value -le 0) { throw "ABBA evidence for '$CheckId' has a non-finite or non-positive upstream timing." }
                    $upstreamRaw.Add([double]$value)
                }
            }
        }
    }
    if ($candidateStart -ne @($Sample.raw).Count) { throw "ABBA candidate ranges for '$CheckId' do not partition the complete raw sample set." }

    $isEligible = $true
    $overheadWithinBudget = $true
    $strictOverageObserved = $false
    if ($candidateRaw.Count -eq 0 -or $upstreamRaw.Count -eq 0 -or $overheadRaw.Count -eq 0) {
        throw "ABBA evidence for '$CheckId' has an empty measured arm."
    }
    $candidateP = @(
        (Get-S18PercentileNS ([double[]]$candidateRaw.ToArray()) 0.50),
        (Get-S18PercentileNS ([double[]]$candidateRaw.ToArray()) 0.95),
        (Get-S18PercentileNS ([double[]]$candidateRaw.ToArray()) 0.99)
    )
    $upstreamP = @(
        (Get-S18PercentileNS ([double[]]$upstreamRaw.ToArray()) 0.50),
        (Get-S18PercentileNS ([double[]]$upstreamRaw.ToArray()) 0.95),
        (Get-S18PercentileNS ([double[]]$upstreamRaw.ToArray()) 0.99)
    )
    $overheadP = @(
        (Get-S18PercentileNS ([double[]]$overheadRaw.ToArray()) 0.50),
        (Get-S18PercentileNS ([double[]]$overheadRaw.ToArray()) 0.95),
        (Get-S18PercentileNS ([double[]]$overheadRaw.ToArray()) 0.99)
    )
    for ($percentileIndex = 0; $percentileIndex -lt 3; $percentileIndex++) {
        $strictNS = [double]$strictLimits[$percentileIndex] * 1000000.0
        $absoluteNS = [double]$exceptionLimits[$percentileIndex] * 1000000.0
        $overheadNS = [double]$overheadLimits[$percentileIndex] * 1000000.0
        if ($overheadP[$percentileIndex] -gt $overheadNS) {
            $overheadWithinBudget = $false
            $isEligible = $false
        }
        if ($candidateP[$percentileIndex] -gt $strictNS) {
            $strictOverageObserved = $true
            if ($upstreamP[$percentileIndex] -le $strictNS -or
                $candidateP[$percentileIndex] -gt $absoluteNS -or
                $candidateP[$percentileIndex] -gt $upstreamP[$percentileIndex] + $overheadNS) {
                $isEligible = $false
            }
        }
    }
    if (-not $strictOverageObserved) { $isEligible = $false }
    return [pscustomobject]@{
        eligible = $isEligible
        overhead_within_budget = $overheadWithinBudget
        candidate_percentiles_ns = $candidateP
        strict_overage = $strictOverageObserved
    }
}

function Get-S18StableABBAExceptions($Report, [string] $ExpectedRunId, $SampleById, $CheckById, [string[]] $StrictFailureIds) {
    $evidence = $Report.abba_evidence
    if ($null -eq $evidence -or $evidence -isnot [System.Management.Automation.PSCustomObject]) {
        throw 'Stable v2 performance evidence must contain all four S18 ABBA check records.'
    }
    $expectedIDs = @(
        "$ExpectedRunId/S18/c/completion_first_usable",
        "$ExpectedRunId/S18/c/syntax_update_after_edit",
        "$ExpectedRunId/S18/cpp/completion_first_usable",
        "$ExpectedRunId/S18/cpp/syntax_update_after_edit"
    )
    $evidenceProperties = @($evidence.PSObject.Properties)
    if ($evidenceProperties.Count -ne $expectedIDs.Count -or
        @($expectedIDs | Where-Object { $null -eq $evidence.PSObject.Properties[$_] }).Count -gt 0 -or
        @($evidenceProperties | Where-Object { $_.Name -cnotin $expectedIDs }).Count -gt 0) {
        throw 'Stable v2 performance evidence must contain exactly the four C/C++ completion and syntax ABBA records.'
    }
    $bounded = [Collections.Generic.List[string]]::new()
    foreach ($id in $expectedIDs) {
        if (-not $SampleById.ContainsKey($id) -or -not $CheckById.ContainsKey($id)) { throw "ABBA evidence is missing its paired S18 check or raw sample '$id'." }
        $result = Get-S18ABBAEligibility $id $CheckById[$id] $SampleById[$id] $evidence.PSObject.Properties[$id].Value
        if (-not $result.overhead_within_budget) { throw "S18 ABBA candidate '$id' exceeds the same-request overhead budget." }
        if ($result.strict_overage -and $id -notin $StrictFailureIds) { throw "S18 ABBA candidate '$id' exceeds a strict threshold but its check is not a typed strict latency failure." }
        if ($id -in $StrictFailureIds -and $result.eligible) { $bounded.Add($id) }
    }
    return @($bounded.ToArray())
}

function Assert-S18ReleaseAssessment($Report, [string[]] $StrictFailureIds, [string[]] $BoundedExceptionIds, [string] $Policy, [int] $ProcessExitCode) {
    $assessment = $Report.release_assessment
    if ($null -eq $assessment) { throw 'Performance report is missing release_assessment.' }
    $policyId = if ($Policy -ceq 'stable') { 's18-evidence-qualified-v2' } else { 's18-strict-v1' }
    if ([string]$assessment.policy_id -cne $policyId) { throw "Performance release policy '$($assessment.policy_id)' does not match '$policyId'." }
    $exceptions = @(Get-OptionalAssessmentCheckIDs $assessment 'exception_check_ids')
    $bounded = @(Get-OptionalAssessmentCheckIDs $assessment 'bounded_exception_check_ids')
    $expectedBounded = @($BoundedExceptionIds | Sort-Object -Unique)
    $recordedBounded = @($bounded | Sort-Object -Unique)
    if ($bounded.Count -ne $recordedBounded.Count -or $expectedBounded.Count -ne $BoundedExceptionIds.Count -or
        [string]::Join("`n", $expectedBounded) -cne [string]::Join("`n", $recordedBounded)) {
        throw 'Performance bounded exception IDs do not match the independently recomputed raw sample candidates.'
    }
    if ([string]$assessment.decision -cne 'passed_with_performance_exception' -and $exceptions.Count -ne 0) {
        throw 'A failed or strict performance assessment cannot claim accepted exception IDs.'
    }
    switch ([string]$assessment.decision) {
        'not_verified' {
            if ([string]$Report.decision -notin @('not_verified', 'running', 'failed') -or [string]$assessment.execution_status -ceq 'completed' -or $exceptions.Count -ne 0 -or $bounded.Count -ne 0) {
                throw 'Performance report has inconsistent incomplete evidence and release assessment.'
            }
            $releaseDecision = 'not_verified'
        }
        'passed' {
            if ([string]$assessment.execution_status -cne 'completed') { throw 'Performance execution is incomplete.' }
            if ([string]$Report.decision -cne 'passed' -or $StrictFailureIds.Count -ne 0 -or $exceptions.Count -ne 0 -or $bounded.Count -ne 0) {
                throw 'Performance release assessment passed while strict evidence failed or listed exceptions.'
            }
            $releaseDecision = 'passed'
        }
        'passed_with_performance_exception' {
            if ([string]$assessment.execution_status -cne 'completed' -or $Policy -cne 'stable' -or [string]$Report.decision -cne 'failed' -or
                $Report.errors -or $Report.skips -or $StrictFailureIds.Count -eq 0 -or
                $StrictFailureIds.Count -gt 4 -or $BoundedExceptionIds.Count -gt 4 -or $exceptions.Count -gt 4 -or
                $BoundedExceptionIds.Count -ne $StrictFailureIds.Count -or $exceptions.Count -ne $StrictFailureIds.Count) {
                throw 'Performance exception is outside the selected policy or includes incomplete/failed evidence.'
            }
            $expected = @($StrictFailureIds | Sort-Object -Unique)
            $bounded = @($BoundedExceptionIds | Sort-Object -Unique)
            $recorded = @($exceptions | Sort-Object -Unique)
            if ($expected.Count -ne $StrictFailureIds.Count -or $bounded.Count -ne $BoundedExceptionIds.Count -or
                [string]::Join("`n", $expected) -cne [string]::Join("`n", $bounded) -or
                [string]::Join("`n", $expected) -cne [string]::Join("`n", $recorded) -or
                [string]::Join("`n", $bounded) -cne [string]::Join("`n", $recorded)) {
                throw 'Performance exception IDs do not exactly match the bounded strict S18 latency failures.'
            }
            $reportRunId = [string]$Report.run_id
            if ([string]::IsNullOrWhiteSpace($reportRunId) -or @($recorded | Where-Object { -not (Test-S18StableExceptionID ([string]$_) $reportRunId) }).Count -gt 0) {
                throw 'Stable v2 performance exceptions must be run-qualified C/C++ completion or syntax latency checks.'
            }
            $releaseDecision = 'passed_with_performance_exception'
        }
        'failed' {
            if ([string]$assessment.execution_status -cne 'completed' -or [string]$Report.decision -cne 'failed' -or $exceptions.Count -ne 0) { throw 'Failed release assessment has no complete strict report failure or contains accepted exception IDs.' }
            $releaseDecision = 'failed'
        }
        default { throw "Invalid performance release decision '$($assessment.decision)'." }
    }
    if ($ProcessExitCode -ne 0) { throw "S18/S19 test process exited with code $ProcessExitCode." }
    return $releaseDecision
}

function Get-OptionalAssessmentCheckIDs($Assessment, [string] $PropertyName) {
    if ($null -eq $Assessment) { throw 'Release assessment is missing.' }
    if ($Assessment -is [System.Collections.IDictionary]) {
        if (-not $Assessment.Contains($PropertyName)) { return }
        $values = $Assessment[$PropertyName]
    } else {
        $property = $Assessment.PSObject.Properties[$PropertyName]
        if ($null -eq $property) { return }
        $values = $property.Value
    }
    if ($null -eq $values -or $values -isnot [array]) {
        throw "Release assessment '$PropertyName' must be a JSON array when present."
    }
    foreach ($value in $values) {
        if ($null -eq $value -or [string]::IsNullOrWhiteSpace([string]$value)) {
            throw "Release assessment '$PropertyName' cannot contain null or empty IDs."
        }
        [string]$value
    }
}

function Get-FastEvidenceHashes {
    $hashes = [ordered]@{}
    foreach ($name in @('Fast-summary.json', 's21.json', 'performance.json', 'semantic.json', 'persistent-go-go.json', 'persistent-typescript.json', 'persistent-python-python.json', 'persistent-javascript-javascript.json', 'persistent-c-c.json', 'persistent-cpp-cpp.json', 'scip-replay-go-go-cli.json', 'clients.json')) {
        $path = Join-Path $EvidenceDirectory $name
        if (-not (Test-Path -LiteralPath $path -PathType Leaf)) {
            throw "Fast evidence is missing required report '$name'."
        }
        $hashes[$name] = Get-FileSha256 $path
    }
    return $hashes
}

function Assert-FastEvidenceHashes($Gates, [string] $Stage) {
    $expectedNames = @('Fast-summary.json', 's21.json', 'performance.json', 'semantic.json', 'persistent-go-go.json', 'persistent-typescript.json', 'persistent-python-python.json', 'persistent-javascript-javascript.json', 'persistent-c-c.json', 'persistent-cpp-cpp.json', 'scip-replay-go-go-cli.json', 'clients.json')
    if (-not $Gates.fast_evidence_sha256) {
        throw "$Stage evidence has no frozen Fast report hashes; rerun Fast with the current harness."
    }
    $actualNames = @($Gates.fast_evidence_sha256.PSObject.Properties | ForEach-Object { $_.Name })
    if ($actualNames.Count -ne $expectedNames.Count -or @($expectedNames | Where-Object { $_ -cnotin $actualNames }).Count -gt 0) {
        throw "$Stage evidence does not contain the exact frozen Fast report set."
    }
    foreach ($name in $expectedNames) {
        $expectedHash = [string]$Gates.fast_evidence_sha256.PSObject.Properties[$name].Value
        $path = Join-Path $EvidenceDirectory $name
        if ($expectedHash -cnotmatch '^[0-9a-f]{64}$' -or -not (Test-Path -LiteralPath $path -PathType Leaf) -or
            (Get-FileSha256 $path) -cne $expectedHash) {
            throw "$Stage invalidated: frozen Fast report '$name' is missing or its SHA-256 changed."
        }
    }
}

function Assert-GateReleaseAssessment($Gates, [string] $Stage) {
    $expectedPolicyID = if ($S18Policy -ceq 'stable') { 's18-evidence-qualified-v2' } else { 's18-strict-v1' }
    $assessment = $Gates.release_assessment
    if ($null -eq $assessment -or [string]$assessment.policy_id -cne $expectedPolicyID -or
        [string]$assessment.execution_status -cne 'completed' -or
        [string]$assessment.decision -cne [string]$Gates.status) {
        throw "$Stage frozen Fast gate and release assessment do not agree with the selected S18 policy."
    }
    $summaryPath = Join-Path $EvidenceDirectory 'Fast-summary.json'
    $performancePath = Join-Path $EvidenceDirectory 'performance.json'
    $summary = Get-Content -LiteralPath $summaryPath -Raw | ConvertFrom-Json -ErrorAction Stop
    $performance = Get-Content -LiteralPath $performancePath -Raw | ConvertFrom-Json -ErrorAction Stop
    $perfSteps = @($summary.checks | Where-Object { [string]$_.id -ceq 'S18-S19-real-process-performance' })
    if ([string]$summary.decision -cne [string]$Gates.status -or [string]$summary.s18_policy -cne $S18Policy -or
        $perfSteps.Count -ne 1 -or [string]$perfSteps[0].status -cne 'passed' -or [long]$perfSteps[0].exitCode -ne 0 -or
        [string]$summary.release_assessment.decision -cne [string]$assessment.decision -or
        [string]$summary.release_assessment.policy_id -cne $expectedPolicyID -or
        [string]$summary.release_assessment.execution_status -cne 'completed' -or
        [string]$performance.release_assessment.decision -cne [string]$assessment.decision -or
        [string]$performance.release_assessment.policy_id -cne $expectedPolicyID -or
        [string]$performance.release_assessment.execution_status -cne 'completed') {
        throw "$Stage frozen summary, performance report, and Fast gate disagree or the test process did not exit zero."
    }
    $gateIDsRaw = @(Get-OptionalAssessmentCheckIDs $assessment 'exception_check_ids')
    $reportIDsRaw = @(Get-OptionalAssessmentCheckIDs $performance.release_assessment 'exception_check_ids')
    $summaryIDsRaw = @(Get-OptionalAssessmentCheckIDs $summary.release_assessment 'exception_check_ids')
    $gateBoundedRaw = @(Get-OptionalAssessmentCheckIDs $assessment 'bounded_exception_check_ids')
    $reportBoundedRaw = @(Get-OptionalAssessmentCheckIDs $performance.release_assessment 'bounded_exception_check_ids')
    $summaryBoundedRaw = @(Get-OptionalAssessmentCheckIDs $summary.release_assessment 'bounded_exception_check_ids')
    $gateIDs = @($gateIDsRaw | Sort-Object -Unique)
    $reportIDs = @($reportIDsRaw | Sort-Object -Unique)
    $summaryIDs = @($summaryIDsRaw | Sort-Object -Unique)
    $gateBounded = @($gateBoundedRaw | Sort-Object -Unique)
    $reportBounded = @($reportBoundedRaw | Sort-Object -Unique)
    $summaryBounded = @($summaryBoundedRaw | Sort-Object -Unique)
    if ([string]$assessment.decision -ceq 'passed_with_performance_exception') {
        $prefix = [regex]::Escape([string]$Gates.run_id)
        if ($S18Policy -cne 'stable' -or $gateIDs.Count -lt 1 -or $gateIDs.Count -gt 4 -or
            $gateIDsRaw.Count -ne $gateIDs.Count -or $reportIDsRaw.Count -ne $reportIDs.Count -or $summaryIDsRaw.Count -ne $summaryIDs.Count -or
            $gateBoundedRaw.Count -ne $gateBounded.Count -or $reportBoundedRaw.Count -ne $reportBounded.Count -or $summaryBoundedRaw.Count -ne $summaryBounded.Count -or
            @($gateIDs | Where-Object { -not (Test-S18StableExceptionID ([string]$_) ([string]$Gates.run_id)) }).Count -gt 0 -or
            [string]::Join("`n", $gateIDs) -cne [string]::Join("`n", $reportIDs) -or
            [string]::Join("`n", $gateIDs) -cne [string]::Join("`n", $summaryIDs) -or
            [string]::Join("`n", $gateIDs) -cne [string]::Join("`n", $gateBounded) -or
            [string]::Join("`n", $gateBounded) -cne [string]::Join("`n", $reportBounded) -or
            [string]::Join("`n", $gateBounded) -cne [string]::Join("`n", $summaryBounded)) {
            throw "$Stage performance exception IDs are invalid or differ from the frozen report."
        }
        foreach ($id in $gateIDs) {
            $matches = @($performance.checks | Where-Object {
                $failureTypes = @($_.observed.failure_types | ForEach-Object { [string]$_ })
                [string]$_.id -ceq $id -and [string]$_.status -ceq 'failed' -and
                [string]$_.observed.failure_type -ceq 'latency' -and $failureTypes.Count -eq 1 -and $failureTypes[0] -ceq 'latency'
            })
            if ($matches.Count -ne 1) { throw "$Stage exception '$id' is not a typed strict latency failure in the frozen report." }
        }
    } elseif ($gateIDs.Count -ne 0 -or $reportIDs.Count -ne 0 -or $summaryIDs.Count -ne 0 -or
        $gateBounded.Count -ne 0 -or $reportBounded.Count -ne 0 -or $summaryBounded.Count -ne 0 -or
        $gateIDsRaw.Count -ne 0 -or $reportIDsRaw.Count -ne 0 -or $summaryIDsRaw.Count -ne 0 -or
        $gateBoundedRaw.Count -ne 0 -or $reportBoundedRaw.Count -ne 0 -or $summaryBoundedRaw.Count -ne 0) {
        throw "$Stage strict Fast decision unexpectedly contains performance exception IDs."
    }
}

function Get-WorkspaceContentHash {
    $excludedDirectoryNames = @('.git', '.tmp-gocache', '.npm-cache', 'node_modules', 'evidence', 'rust-analyzer-helper-target')
    $evidenceRoot = $null
    $repoPrefix = $repoRoot.TrimEnd([IO.Path]::DirectorySeparatorChar, [IO.Path]::AltDirectorySeparatorChar) + [IO.Path]::DirectorySeparatorChar
    if ($EvidenceDirectory.StartsWith($repoPrefix, [StringComparison]::OrdinalIgnoreCase)) {
        $evidenceRoot = [IO.Path]::GetFullPath($EvidenceDirectory)
    }
    $pending = [Collections.Generic.Stack[string]]::new()
    $pending.Push($repoRoot)
    $entries = [Collections.Generic.List[string]]::new()
    while ($pending.Count -gt 0) {
        $directory = $pending.Pop()
        if ($evidenceRoot -and [string]::Equals([IO.Path]::GetFullPath($directory), $evidenceRoot, [StringComparison]::OrdinalIgnoreCase)) { continue }
        foreach ($entry in [IO.Directory]::EnumerateFileSystemEntries($directory)) {
            $attributes = [IO.File]::GetAttributes($entry)
            if (($attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0) { continue }
            if (($attributes -band [IO.FileAttributes]::Directory) -ne 0) {
                $directoryName = [IO.Path]::GetFileName($entry)
                if ($directoryName -notin $excludedDirectoryNames -and
                    -not $directoryName.StartsWith('.tmp-gocache-', [StringComparison]::Ordinal)) {
                    $pending.Push($entry)
                }
                continue
            }
            $relative = [IO.Path]::GetRelativePath($repoRoot, $entry).Replace('\', '/')
            $entries.Add("$relative $((Get-FileSha256 $entry))")
        }
    }
    $entries.Sort([StringComparer]::Ordinal)
    $bytes = [Text.Encoding]::UTF8.GetBytes(($entries -join "`n") + "`n")
    return [Convert]::ToHexString([Security.Cryptography.SHA256]::HashData($bytes)).ToLowerInvariant()
}

function Test-FastSourceTreeIdentity([string] $CheckId) {
    $observed = Get-WorkspaceContentHash
    $expected = [string]$script:FastSourceTreeHash
    $status = if ($expected -match '^[0-9a-f]{64}$' -and $observed -ceq $expected) { 'passed' } else { 'failed' }
    return [ordered]@{
        id = $CheckId; status = $status; expected_sha256 = $expected; observed_sha256 = $observed
        detail = if ($status -eq 'passed') { 'workspace content remained identical to the pre-test source snapshot' } else { 'workspace content changed after the pre-test source snapshot' }
    }
}

function Test-AcceptanceFingerprintIdentity([string] $FrozenFingerprint, [string] $CurrentFingerprint) {
    return $FrozenFingerprint -match '^[0-9a-f]{64}$' -and
        $CurrentFingerprint -match '^[0-9a-f]{64}$' -and
        $FrozenFingerprint -ceq $CurrentFingerprint
}

function Get-AcceptanceFingerprint {
    $toolRoot = Join-Path $repoRoot 'test/acceptance/tools'
    $vscodeLock = Join-Path $repoRoot 'editors/vscode/package-lock.json'
    $npmLock = Join-Path $toolRoot 'package-lock.json'
    $toolLock = Join-Path $toolRoot 'tools.lock.json'
    $fingerprint = [ordered]@{
        workspace_sha256 = Get-WorkspaceContentHash
        corpus_sha256 = Get-CorpusHash
        s18_policy = [ordered]@{
            selected = $S18Policy
            strict_targets_ms = [ordered]@{
                hot_hover = @(20, 75, 150); hot_definition = @(25, 100, 200)
                completion_first_usable = @(40, 120, 250); syntax_update_after_edit = @(15, 50, 100)
            }
            stable_exception = [ordered]@{
                policy_id = 's18-evidence-qualified-v2'; languages = @('c', 'cpp'); max_exceptions = 4
                operations = [ordered]@{
                    completion_first_usable = @(80, 100, 125)
                    syntax_update_after_edit = @(60, 100, 150)
                }
                abba_evidence_schema = 'candidate-raw-with-correlated-write-wait-v1'
                abba_rounds = 3; abba_leg_order = @('candidate', 'upstream', 'upstream', 'candidate')
                candidate_min_samples_per_leg = 1000; candidate_min_samples_per_check = 6000
                upstream_min_samples_per_leg = 1000
                overhead_pctl_ms = @(5, 10, 20); candidate_upstream_delta_max_ms = @(5, 10, 20)
                percentile_method = 'nearest-rank ceiling(q*n)-1'
                eligibility_evidence = 'raw single-request latency samples plus per-request candidate overhead evidence'
            }
        }
        tools_lock_sha256 = Get-FileSha256 $toolLock
        acceptance_package_lock_sha256 = if (Test-Path $npmLock) { Get-FileSha256 $npmLock } else { 'MISSING' }
        vscode_package_lock_sha256 = if (Test-Path $vscodeLock) { Get-FileSha256 $vscodeLock } else { 'MISSING' }
        installed_dependency_trees = [ordered]@{
            acceptance_tools = Get-DirectoryContentHash (Join-Path $toolRoot 'node_modules')
            vscode_extension = Get-DirectoryContentHash (Join-Path $repoRoot 'editors/vscode/node_modules')
            helix_runtime = Get-DirectoryContentHash (Join-Path $repoRoot 'test/acceptance/tools/bin/helix-25.07.1/runtime')
        }
        environment = Get-ToolVersions
        resolved_execution = [ordered]@{
            node_path = if ($script:AcceptanceResolvedPaths.node) { [string]$script:AcceptanceResolvedPaths.node } else { [string]$env:OMNILSP_SEMANTIC_NODE_PATH }
            typescript_path = if ($script:AcceptanceResolvedPaths.typescript) { [string]$script:AcceptanceResolvedPaths.typescript } else { [string]$env:OMNILSP_SEMANTIC_TYPESCRIPT_PATH }
            pyright_internal_path = if ($script:AcceptanceResolvedPaths -and $script:AcceptanceResolvedPaths['pyright-internal']) { [string]$script:AcceptanceResolvedPaths['pyright-internal'] } else { [string]$env:OMNILSP_SEMANTIC_PYRIGHT_INTERNAL_PATH }
            pyright_vendor_path = if ($script:AcceptanceResolvedPaths -and $script:AcceptanceResolvedPaths['pyright-vendor']) { [string]$script:AcceptanceResolvedPaths['pyright-vendor'] } else { [string]$env:OMNILSP_SEMANTIC_PYRIGHT_VENDOR_PATH }
            semantic_python_path = if ($script:AcceptanceResolvedPaths.python) { [string]$script:AcceptanceResolvedPaths.python } else { [string]$env:OMNILSP_SEMANTIC_PYTHON_PATH }
            acceptance_node_path = if ($script:AcceptanceResolvedPaths.node) { [string]$script:AcceptanceResolvedPaths.node } else { [string]$env:OMNILSP_ACCEPTANCE_NODE }
            vscode_path = if ($script:AcceptanceResolvedPaths.vscode) { [string]$script:AcceptanceResolvedPaths.vscode } else { [string]$env:OMNILSP_VSCODE_BIN }
            neovim_path = if ($script:AcceptanceResolvedPaths.neovim) { [string]$script:AcceptanceResolvedPaths.neovim } else { [string]$env:OMNILSP_NVIM_BIN }
            node_sha256 = if ($script:AcceptanceToolLock) { ([string](Get-ResolvedBinaryEntry $script:AcceptanceToolLock.resolvedBinaries 'node').sha256).ToLowerInvariant() } else { '' }
            typescript_sha256 = if ($script:AcceptanceToolLock) { ([string](Get-ResolvedBinaryEntry $script:AcceptanceToolLock.resolvedBinaries 'typescript').sha256).ToLowerInvariant() } else { '' }
        }
        executables = Get-AcceptanceToolExecutables
    }
    $canonical = $fingerprint | ConvertTo-Json -Depth 8 -Compress
    $fingerprint['fingerprint_sha256'] = [Convert]::ToHexString([Security.Cryptography.SHA256]::HashData([Text.Encoding]::UTF8.GetBytes($canonical))).ToLowerInvariant()
    return $fingerprint
}

function Get-DirectoryContentHash([string] $Path) {
    if (-not (Test-Path -LiteralPath $Path -PathType Container)) { return 'MISSING' }
    $root = [IO.Path]::GetFullPath($Path)
    if (([IO.File]::GetAttributes($root) -band [IO.FileAttributes]::ReparsePoint) -ne 0) {
        throw "Acceptance dependency tree root '$root' cannot be a reparse point."
    }
    $separator = [IO.Path]::DirectorySeparatorChar
    $rootBoundary = $root.TrimEnd([IO.Path]::DirectorySeparatorChar, [IO.Path]::AltDirectorySeparatorChar)
    $rootPrefix = $rootBoundary + $separator
    $pathComparison = if ($separator -eq '\') { [StringComparison]::OrdinalIgnoreCase } else { [StringComparison]::Ordinal }
    $pathComparer = if ($separator -eq '\') { [StringComparer]::OrdinalIgnoreCase } else { [StringComparer]::Ordinal }
    $pending = [Collections.Generic.Stack[string]]::new()
    $visitedDirectories = [Collections.Generic.HashSet[string]]::new($pathComparer)
    $pending.Push($root)
    [void]$visitedDirectories.Add($rootBoundary)
    $entries = [Collections.Generic.List[string]]::new()
    while ($pending.Count -gt 0) {
        $directory = $pending.Pop()
        foreach ($entry in [IO.Directory]::EnumerateFileSystemEntries($directory)) {
            $attributes = [IO.File]::GetAttributes($entry)
            if (($attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0) {
                $relative = [IO.Path]::GetRelativePath($root, $entry).Replace('\', '/')
                $item = Get-Item -LiteralPath $entry -Force
                $resolvedTarget = $item.ResolveLinkTarget($true)
                if ($null -eq $resolvedTarget) {
                    throw "Cannot resolve acceptance dependency link '$relative'."
                }
                $targetPath = [IO.Path]::GetFullPath($resolvedTarget.FullName)
                $targetBoundary = $targetPath.TrimEnd([IO.Path]::DirectorySeparatorChar, [IO.Path]::AltDirectorySeparatorChar)
                if (-not $targetBoundary.Equals($rootBoundary, $pathComparison) -and
                    -not $targetPath.StartsWith($rootPrefix, $pathComparison)) {
                    throw "Acceptance dependency link '$relative' resolves outside its dependency tree."
                }
                $targetCursor = $root
                foreach ($component in $targetPath.Substring($rootBoundary.Length).Split([char[]]@([IO.Path]::DirectorySeparatorChar, [IO.Path]::AltDirectorySeparatorChar), [StringSplitOptions]::RemoveEmptyEntries)) {
                    $targetCursor = Join-Path $targetCursor $component
                    if (([IO.File]::GetAttributes($targetCursor) -band [IO.FileAttributes]::ReparsePoint) -ne 0) {
                        throw "Acceptance dependency link '$relative' resolves through another reparse point."
                    }
                }
                $targetRelative = [IO.Path]::GetRelativePath($root, $targetPath).Replace('\', '/')
                $entries.Add("LINK $relative $($item.LinkType) $targetRelative")
                $targetAttributes = [IO.File]::GetAttributes($targetPath)
                if (($targetAttributes -band [IO.FileAttributes]::Directory) -ne 0) {
                    if ($visitedDirectories.Add($targetBoundary)) { $pending.Push($targetPath) }
                } else {
                    $entries.Add("LINKFILE $relative $((Get-FileSha256 $targetPath))")
                }
                continue
            }
            if (($attributes -band [IO.FileAttributes]::Directory) -ne 0) {
                $directoryPath = [IO.Path]::GetFullPath($entry)
                $directoryBoundary = $directoryPath.TrimEnd([IO.Path]::DirectorySeparatorChar, [IO.Path]::AltDirectorySeparatorChar)
                if ($visitedDirectories.Add($directoryBoundary)) { $pending.Push($directoryPath) }
                continue
            }
            $relative = [IO.Path]::GetRelativePath($root, $entry).Replace('\', '/')
            $entries.Add("$relative $((Get-FileSha256 $entry))")
        }
    }
    $entries.Sort([StringComparer]::Ordinal)
    $bytes = [Text.Encoding]::UTF8.GetBytes(($entries -join "`n") + "`n")
    return [Convert]::ToHexString([Security.Cryptography.SHA256]::HashData($bytes)).ToLowerInvariant()
}

function Get-AcceptanceToolExecutables {
    $paths = [ordered]@{}
    $lock = $script:AcceptanceToolLock
    if ($null -eq $lock) {
        $lock = Get-Content -LiteralPath (Join-Path $repoRoot 'test/acceptance/tools/tools.lock.json') -Raw | ConvertFrom-Json
    }
    foreach ($name in @(Get-RequiredResolvedBinaryNames)) {
        $entry = Get-ResolvedBinaryEntry $lock.resolvedBinaries $name
        $path = Get-ResolvedBinaryPath $lock.resolvedBinaries $name
        if ($null -eq $entry -or $null -eq $path -or -not (Test-Path -LiteralPath $path -PathType Leaf)) {
            $paths[$name] = [ordered]@{ path = 'MISSING'; sha256 = 'MISSING' }
        } else {
            $paths[$name] = [ordered]@{ path = $path; sha256 = Get-FileSha256 $path }
        }
    }
    foreach ($item in @(
        @{ key = 'pyright-wrapper'; name = 'pyright-langserver'; lockName = 'pyright-langserver' },
        @{ key = 'typescript-language-server-wrapper'; name = 'typescript-language-server'; lockName = 'typescript-language-server' },
        @{ key = 'tsc-wrapper'; name = 'tsc'; lockName = 'typescript' }
    )) {
        $wrapper = Join-Path $localToolBin ($item.name + '.exe')
        $sourceEntry = Get-ResolvedBinaryEntry $lock.resolvedBinaries $item.lockName
        $sourcePath = Get-ResolvedBinaryPath $lock.resolvedBinaries $item.lockName
        if (-not (Test-Path -LiteralPath $wrapper -PathType Leaf)) {
            $paths[$item.key] = [ordered]@{ path = 'MISSING'; sha256 = 'MISSING'; source_path = if ($sourcePath) { $sourcePath } else { 'MISSING' }; source_sha256 = if ($sourceEntry) { ([string]$sourceEntry.sha256).ToLowerInvariant() } else { 'MISSING' } }
        } else {
            $paths[$item.key] = [ordered]@{ path = [IO.Path]::GetFullPath($wrapper); sha256 = Get-FileSha256 $wrapper; source_path = if ($sourcePath) { $sourcePath } else { 'MISSING' }; source_sha256 = if ($sourceEntry) { ([string]$sourceEntry.sha256).ToLowerInvariant() } else { 'MISSING' }; node_path = [string]$env:OMNILSP_ACCEPTANCE_NODE }
        }
    }
    return $paths
}

function Assert-AcceptanceFingerprint($Gates, [string] $Stage) {
    if ([string]$Gates.status -cnotin @('passed', 'passed_with_performance_exception') -or [string]$Gates.run_id -cne $RunId) {
        throw "$Stage requires a completed Fast gate for run '$RunId'."
    }
    if ([string]$Gates.s18_policy -cne $S18Policy) {
        throw "$Stage S18 policy does not match the frozen Fast gate."
    }
    if ([string]$Gates.status -ceq 'passed_with_performance_exception' -and
        [string]$Gates.release_assessment.decision -cne 'passed_with_performance_exception') {
        throw "$Stage exception gate is missing its matching release assessment."
    }
    if ([string]$Gates.status -ceq 'passed' -and [string]$Gates.release_assessment.decision -cne 'passed') {
        throw "$Stage strict Fast gate is missing its matching release assessment."
    }
    if (-not (Test-Path -LiteralPath $CandidateBinary -PathType Leaf) -or
        (Get-FileSha256 $CandidateBinary) -cne [string]$Gates.candidate_sha256) {
        throw "$Stage invalidated: candidate binary differs from the passed Fast gate."
    }
    if (-not $Gates.acceptance_fingerprint -or -not $Gates.acceptance_fingerprint.fingerprint_sha256) {
        throw "$Stage evidence has no frozen acceptance fingerprint; rerun Fast with the current harness."
    }
    $current = Get-AcceptanceFingerprint
    if (-not (Test-AcceptanceFingerprintIdentity $Gates.acceptance_fingerprint.fingerprint_sha256 $current.fingerprint_sha256)) {
        throw "$Stage invalidated: source, corpus, configuration, lockfiles, or observed tool versions changed after Fast."
    }
    Assert-FastEvidenceHashes $Gates $Stage
    Assert-GateReleaseAssessment $Gates $Stage
}

function Assert-PreSoakReview(
    $Review,
    $Gates,
    [string] $ExpectedRunId,
    [string] $CandidatePath,
    [string] $SoakTestPath,
    [string] $EvidenceRoot,
    [string] $RepositoryRoot
) {
    if ($Review.schema_version -isnot [long] -or $Review.schema_version -ne 1 -or
        [string]$Review.decision -cne 'passed' -or [string]$Review.run_id -cne $ExpectedRunId) {
        throw 'Pre-soak review must use schema version 1, decision passed, and the current run ID.'
    }
    $candidateHash = Get-FileSha256 $CandidatePath
    $soakTestHash = Get-FileSha256 $SoakTestPath
    $fingerprintHash = [string]$Gates.acceptance_fingerprint.fingerprint_sha256
    $currentWorkspaceHash = Get-WorkspaceContentHash
    if ([string]$Review.candidate_sha256 -cne $candidateHash -or
        $candidateHash -cne [string]$Gates.candidate_sha256 -or
        [string]$Review.acceptance_fingerprint_sha256 -cne $fingerprintHash -or
        [string]$Gates.run_id -cne $ExpectedRunId -or
        $fingerprintHash -notmatch '^[0-9a-f]{64}$' -or
        $currentWorkspaceHash -cne [string]$Gates.acceptance_fingerprint.workspace_sha256) {
        throw "Pre-soak review identity does not match the current run, candidate, or acceptance fingerprint (review=$($Review.run_id)/$($Review.candidate_sha256)/$($Review.acceptance_fingerprint_sha256), gates=$($Gates.run_id)/$($Gates.candidate_sha256)/$fingerprintHash, current=$ExpectedRunId/$candidateHash, workspace=$currentWorkspaceHash/$($Gates.acceptance_fingerprint.workspace_sha256))."
    }
    if ($soakTestHash -cne [string]$Gates.soak_test_sha256) {
        throw 'Pre-soak review evidence does not match the frozen soak test executable.'
    }
    if ($Review.reviewer -isnot [System.Management.Automation.PSCustomObject] -or
        [string]$Review.reviewer.id -cne 'luna-d' -or [string]$Review.reviewer.name -cne 'Luna D' -or
        $Review.reviewer.independent -isnot [bool] -or -not $Review.reviewer.independent) {
        throw 'Pre-soak review must identify Luna D as an independent reviewer.'
    }

    $checklistRequirements = [ordered]@{
        integrated_source = @('workspace_sha256')
        test_validity = @('soak_test_sha256', 'soak_30s_report_sha256', 'soak_10m_report_sha256')
        test_to_spec_mapping = @('soak_test_source_sha256', 'acceptance_spec_sha256')
        current_evidence = @('fast_gates_sha256', 'soak_30s_stage_sha256', 'soak_30s_report_sha256', 'soak_10m_stage_sha256', 'soak_10m_report_sha256')
    }
    if ($Review.checklist -isnot [System.Management.Automation.PSCustomObject]) {
        throw 'Pre-soak review checklist is missing or not an object.'
    }
    $checklistProperties = @($Review.checklist.PSObject.Properties)
    if ($checklistProperties.Count -ne $checklistRequirements.Count -or
        @($checklistProperties | Where-Object { $_.Name -cnotin $checklistRequirements.Keys }).Count -gt 0) {
        throw 'Pre-soak review checklist must contain exactly the four required review areas.'
    }
    foreach ($checkName in $checklistRequirements.Keys) {
        $check = $Review.checklist.PSObject.Properties[$checkName].Value
        $evidenceProperty = if ($check) { $check.PSObject.Properties['evidence'] } else { $null }
        $references = if ($evidenceProperty -and $evidenceProperty.Value -is [array]) { @($evidenceProperty.Value) } else { @() }
        $expectedReferences = @($checklistRequirements[$checkName])
        if ([string]$check.status -cne 'passed' -or $references.Count -ne $expectedReferences.Count -or
            @($expectedReferences | Where-Object { $_ -cnotin $references }).Count -gt 0 -or
            @($references | Where-Object { $_ -cnotin $expectedReferences }).Count -gt 0) {
            throw "Pre-soak review checklist '$checkName' is incomplete or does not cite its required evidence."
        }
    }

    $expectedEvidence = [ordered]@{
        run_id = $ExpectedRunId
        candidate_sha256 = $candidateHash
        acceptance_fingerprint_sha256 = $fingerprintHash
        workspace_sha256 = [string]$Gates.acceptance_fingerprint.workspace_sha256
        fast_gates_sha256 = Get-FileSha256 (Join-Path $EvidenceRoot 'fast-gates.json')
        soak_test_sha256 = $soakTestHash
        soak_test_source_sha256 = Get-FileSha256 (Join-Path $RepositoryRoot 'test/soak/stdio_soak_test.go')
        acceptance_spec_sha256 = Get-FileSha256 (Join-Path $RepositoryRoot 'docs/acceptance.md')
        soak_30s_stage_sha256 = Get-FileSha256 (Join-Path $EvidenceRoot 'soak-30s.json')
        soak_30s_report_sha256 = Get-FileSha256 (Join-Path $EvidenceRoot 'soak-30s-report.json')
        soak_10m_stage_sha256 = Get-FileSha256 (Join-Path $EvidenceRoot 'soak-10m.json')
        soak_10m_report_sha256 = Get-FileSha256 (Join-Path $EvidenceRoot 'soak-10m-report.json')
    }
    if ($Review.evidence -isnot [System.Management.Automation.PSCustomObject]) {
        throw 'Pre-soak review evidence is missing or not an object.'
    }
    $evidenceProperties = @($Review.evidence.PSObject.Properties)
    if ($evidenceProperties.Count -ne $expectedEvidence.Count -or
        @($expectedEvidence.Keys | Where-Object { $null -eq $Review.evidence.PSObject.Properties[$_] }).Count -gt 0 -or
        @($evidenceProperties | Where-Object { $_.Name -cnotin $expectedEvidence.Keys }).Count -gt 0) {
        throw 'Pre-soak review must identify every current source, candidate, soak, and Fast evidence artifact.'
    }
    foreach ($key in $expectedEvidence.Keys) {
        if ([string]$Review.evidence.PSObject.Properties[$key].Value -cne [string]$expectedEvidence[$key]) {
            throw "Pre-soak review evidence '$key' does not match the current run artifacts."
        }
    }
}

function Test-S18NormalizedObservation($Value, [string] $Method, [string] $ExpectedSymbol) {
    if ($null -eq $Value -or [string]::IsNullOrWhiteSpace($ExpectedSymbol)) { return $false }
    $serialized = ConvertTo-Json -InputObject $Value -Depth 40 -Compress
    if ([string]::IsNullOrWhiteSpace($serialized) -or $serialized -in @('null', '[]', '{}')) { return $false }
    if ($Method -eq 'textDocument/definition') {
        $locations = @($Value)
        if ($locations.Count -eq 0) { return $false }
        foreach ($location in $locations) {
            if ([string]::IsNullOrWhiteSpace([string]$location.uri) -or $null -eq $location.range -or
                $location.range.start.line -isnot [long] -or $location.range.start.character -isnot [long] -or
                $location.range.end.line -isnot [long] -or $location.range.end.character -isnot [long] -or
                $location.range.end.line -lt $location.range.start.line -or
                ($location.range.end.line -eq $location.range.start.line -and $location.range.end.character -lt $location.range.start.character)) {
                return $false
            }
        }
        return $true
    }
    if ($Method -notin @('textDocument/hover', 'textDocument/completion', 'textDocument/documentSymbol')) { return $false }
    return $serialized.IndexOf($ExpectedSymbol, [StringComparison]::Ordinal) -ge 0
}

function Test-S18CompletionListEvidence([string] $Language, [string] $Operation, $DifferentialEvidence) {
    if ($Operation -cne 'completion_first_usable' -or $Language -cnotin @('c', 'cpp')) { return $true }
    return $DifferentialEvidence -is [System.Management.Automation.PSCustomObject] -and
        [string]$DifferentialEvidence.full_list_snapshot_status -ceq 'passed' -and
        [string]$DifferentialEvidence.full_list_sample_status -ceq 'passed' -and
        [string]$DifferentialEvidence.target_candidate_sample_status -ceq 'passed'
}

function Test-S18EditUpdateEvidence($Edit, [string] $ExpectedOldSymbol, [string] $ExpectedNewSymbol) {
    if ($Edit -isnot [System.Management.Automation.PSCustomObject] -or
        [string]::IsNullOrWhiteSpace($ExpectedOldSymbol) -or [string]::IsNullOrWhiteSpace($ExpectedNewSymbol)) {
        return $false
    }
    $before = @($Edit.before_symbols)
    $after = @($Edit.after_symbols)
    $upstreamAfter = @($Edit.upstream_after_symbols)
    $oldSymbolBefore = @($before | Where-Object { [string]$_ -ceq $ExpectedOldSymbol }).Count -gt 0
    $oldSymbolAfter = @($after | Where-Object { [string]$_ -ceq $ExpectedOldSymbol }).Count -gt 0
    $expectedAfter = @($before | ForEach-Object {
        if ([string]$_ -ceq $ExpectedOldSymbol) { $ExpectedNewSymbol } else { [string]$_ }
    })
    return [string]$Edit.status -ceq 'passed' -and $Edit.changed -eq $true -and $Edit.freshness_verified -eq $true -and
        [string]$Edit.old_symbol -ceq $ExpectedOldSymbol -and [string]$Edit.new_symbol -ceq $ExpectedNewSymbol -and
        $before.Count -gt 0 -and $oldSymbolBefore -and $after.Count -gt 0 -and -not $oldSymbolAfter -and $upstreamAfter.Count -gt 0 -and
        (@($after | Where-Object { [string]$_ -ceq $ExpectedNewSymbol }).Count -gt 0) -and
        (ConvertTo-Json -InputObject $after -Depth 20 -Compress) -ceq (ConvertTo-Json -InputObject $expectedAfter -Depth 20 -Compress) -and
        (ConvertTo-Json -InputObject $after -Depth 20 -Compress) -ceq (ConvertTo-Json -InputObject $upstreamAfter -Depth 20 -Compress)
}

function Assert-S19ReturnedLocationScale($Check, $Sample, [int] $ExpectedReturnedLocationTotal) {
    if ($ExpectedReturnedLocationTotal -notin @(200, 800, 3200) -or $null -eq $Check -or $null -eq $Sample -or $null -eq $Check.observed) {
        throw 'S19 returned-location scale is missing or outside the required 200/800/3200 totals.'
    }
    $expectedIdSuffix = "/S19/references@$ExpectedReturnedLocationTotal"
    if ([string]$Check.id -cne [string]$Sample.id -or -not ([string]$Check.id).EndsWith($expectedIdSuffix, [StringComparison]::Ordinal)) {
        throw "S19 check/sample identity does not match returned-location total $ExpectedReturnedLocationTotal."
    }
    $observed = $Check.observed
    $expectedSummary = "exact returned-location total including declaration; target=$ExpectedReturnedLocationTotal"
    if (-not $Check.PSObject.Properties['summary'] -or -not ([string]$Check.summary).StartsWith($expectedSummary, [StringComparison]::Ordinal)) {
        throw "S19 check summary does not identify the exact returned-location total including declaration ($ExpectedReturnedLocationTotal)."
    }
    foreach ($legacyField in @('requestedReferences', 'expectedResultCount', 'resultCounts', 'expected_result_count', 'result_counts', 'returned_location_scale', 'expected_returned_location_count', 'observed_returned_location_counts')) {
        if ($observed.PSObject.Properties[$legacyField]) { throw "S19 report contains ambiguous legacy field '$legacyField'." }
    }
    $scaleProperty = $observed.PSObject.Properties['returned_location_total_including_declaration']
    $expectedProperty = $observed.PSObject.Properties['expected_returned_location_count_including_declaration']
    $observedCountsProperty = $observed.PSObject.Properties['observed_returned_location_counts_including_declaration']
    if (-not $scaleProperty -or $scaleProperty.Value -isnot [long] -or $scaleProperty.Value -ne $ExpectedReturnedLocationTotal -or
        -not $expectedProperty -or $expectedProperty.Value -isnot [long] -or $expectedProperty.Value -ne $ExpectedReturnedLocationTotal -or
        -not $observedCountsProperty -or @($observedCountsProperty.Value).Count -ne 3 -or
        @($observedCountsProperty.Value | Where-Object { $_ -isnot [long] -or $_ -ne $ExpectedReturnedLocationTotal }).Count -gt 0) {
        throw "S19 returned-location counts including declaration do not equal exactly $ExpectedReturnedLocationTotal in all three observations."
    }
    $rawProperty = $Sample.PSObject.Properties['raw']
    if ([string]$Sample.unit -cne 'ns/query' -or -not $rawProperty -or @($rawProperty.Value).Count -ne 3 -or
        @($rawProperty.Value | Where-Object { ($_ -isnot [long] -and $_ -isnot [double]) -or [double]$_ -lt 0 }).Count -gt 0) {
        throw "S19 returned-location scale $ExpectedReturnedLocationTotal must retain exactly three nonnegative query samples."
    }
    $ordered = @($rawProperty.Value | ForEach-Object { [double]$_ } | Sort-Object)
    foreach ($percentile in @(@{ name = 'p50'; q = 0.50 }, @{ name = 'p95'; q = 0.95 }, @{ name = 'p99'; q = 0.99 })) {
        $rank = [Math]::Ceiling([double]$percentile.q * $ordered.Count)
        $measured = $ordered[[Math]::Max(0, $rank - 1)]
        $observedName = "$($percentile.name)_ns"
        $samplePercentile = $Sample.PSObject.Properties[$percentile.name]
        $checkPercentile = $observed.PSObject.Properties[$observedName]
        if (-not $samplePercentile -or ($samplePercentile.Value -isnot [long] -and $samplePercentile.Value -isnot [double]) -or
            -not $checkPercentile -or ($checkPercentile.Value -isnot [long] -and $checkPercentile.Value -isnot [double]) -or
            [double]$samplePercentile.Value -ne $measured -or [double]$checkPercentile.Value -ne $measured) {
            throw "S19 $($percentile.name) percentile does not match retained samples for returned-location total $ExpectedReturnedLocationTotal."
        }
    }
}

function Assert-S19ProcessTreeResourceEvidence($Check) {
    if ($null -eq $Check -or [string]$Check.status -cne 'passed' -or
        -not ([string]$Check.id).EndsWith('/S19/process-tree-resources', [StringComparison]::Ordinal) -or
        $Check.observed -isnot [System.Management.Automation.PSCustomObject]) {
        throw 'S19 process-tree resource evidence is missing or not a passing run-qualified check.'
    }
    $observed = $Check.observed
    $resources = @($observed.snapshots)
    $resourceLimit = $observed.private_memory_limit_bytes
    $expectedStages = @('initialized', 'returned-locations-200', 'returned-locations-800', 'returned-locations-3200', 'completed')
    if ($observed.expected_snapshot_count -isnot [long] -or $observed.expected_snapshot_count -ne 5 -or
        $resourceLimit -isnot [long] -or $resourceLimit -ne 8589934592 -or
        $resources.Count -ne 5 -or @($expectedStages | Where-Object { $stage = $_; -not ($resources | Where-Object { $_.stage -ceq $stage }) }).Count -gt 0 -or
        @($resources | Where-Object {
            $_.status -cne 'observed' -or $_.privateBytes -isnot [long] -or $_.privateBytes -lt 0 -or $_.privateBytes -gt $resourceLimit -or
            [string]$_.privateBytesMetric -cne 'PROCESS_MEMORY_COUNTERS_EX.PrivateUsage' -or
            $_.processCount -isnot [long] -or $_.processCount -lt 1 -or $_.rootPid -isnot [long] -or $_.rootPid -le 0 -or
            [string]::IsNullOrWhiteSpace([string]$_.sampledAt)
        }).Count -gt 0) {
        throw 'Performance report passed without all five process-tree snapshots under the 8 GiB private-memory limit.'
    }
}

function Test-S19ConcurrentRequestIdentity($Observed) {
    if ($null -eq $Observed) { return $false }
    $referenceID = $Observed.reference_request_id
    $completionID = $Observed.completion_request_id
    $hoverID = $Observed.hover_request_id
    if ($referenceID -isnot [long] -or $referenceID -le 0 -or
        $completionID -isnot [long] -or $completionID -le 0 -or
        $hoverID -isnot [long] -or $hoverID -le 0 -or
        $completionID -eq $hoverID) {
        return $false
    }
    $nextID = $referenceID + 1
    $lastID = $referenceID + 2
    return ($completionID -eq $nextID -and $hoverID -eq $lastID) -or
        ($completionID -eq $lastID -and $hoverID -eq $nextID)
}

function Assert-S20KPIEvidence($KpiCheck, [string] $ExpectedRunId) {
    if ($null -eq $KpiCheck -or [string]$KpiCheck.id -cne "$ExpectedRunId/S20/accuracy-kpi") {
        throw 'Semantic report is missing its run-qualified accuracy KPI check.'
    }
    $observed = $KpiCheck.observed
    if ($observed -isnot [System.Management.Automation.PSCustomObject] -or
        [string]$observed.schema -cne 'omnilsp.s20.accuracy-kpi.v2' -or
        [string]::IsNullOrWhiteSpace([string]$observed.countingUnit) -or
        $observed.recordCount -isnot [long] -or $observed.recordCount -ne @($observed.records).Count -or
        @($observed.records).Count -ne 16 -or
        -not $observed.PSObject.Properties['limitations'] -or
        -not $observed.PSObject.Properties['duplicateDimensions']) {
        throw 'S20 KPI observed payload has no supported schema, counting unit, or complete record count.'
    }
    $metrics = @(
        @{ name = 'precision'; formula = 'TP/(TP+FP)' },
        @{ name = 'recall'; formula = 'TP/(TP+FN)' },
        @{ name = 'falsePositiveRate'; formula = 'FP/(FP+TN)' },
        @{ name = 'falseNegativeRate'; formula = 'FN/(FN+TP)' },
        @{ name = 'refusalRate'; formula = 'refusals/requests' },
        @{ name = 'staleResultRejectionRate'; formula = 'stale_results_rejected/stale_results_presented_to_freshness_gate' },
        @{ name = 'wrongFileRate'; formula = 'wrong_file_locations/returned_locations' },
        @{ name = 'positionMappingFailureRate'; formula = 'position_mapping_failures/position_mapping_samples' },
        @{ name = 'editValidationFailureRate'; formula = 'edit_validation_failures/edit_validation_attempts' }
    )
    $languages = @('Go', 'C', 'C++', 'Rust', 'Python', 'TypeScript', 'JavaScript')
    $requiredPairs = @{}
    foreach ($language in $languages) { foreach ($feature in @('definition', 'references')) { $requiredPairs["$($language.ToLowerInvariant())|$feature"] = $false } }
    $requiredPairs['go|position-mapping'] = $false
    $requiredPairs['go|rename'] = $false
    if (-not $observed.PSObject.Properties['notApplicable'] -or @($observed.notApplicable).Count -ne 15) {
        throw 'S20 KPI report must list exactly the 15 read-only edit-validation dimensions as notApplicable.'
    }
    $backendByLanguage = @{
        go = 'golang'; c = 'ccls/clangd'; 'c++' = 'ccls/clangd'; rust = 'rustanalyzer'
        python = 'pyright'; typescript = 'typescript'; javascript = 'typescript'
    }
    $seenDimensions = @{}
    $sawInvalid = $false
    $sawNotVerified = $false
    $notApplicableDimensions = @{}
    foreach ($record in @($observed.records)) {
        $dimension = $record.dimension
        if ($dimension -isnot [System.Management.Automation.PSCustomObject]) { throw 'S20 KPI record is missing its dimensions.' }
        $language = [string]$dimension.language
        $feature = [string]$dimension.feature
        $backend = [string]$dimension.backend
        $oracle = [string]$dimension.oracle
        if ([string]::IsNullOrWhiteSpace($language) -or [string]::IsNullOrWhiteSpace($feature) -or
            [string]::IsNullOrWhiteSpace($backend) -or [string]::IsNullOrWhiteSpace($oracle)) {
            throw 'S20 KPI record has an empty language, feature, backend, or oracle dimension.'
        }
        $languageKey = $language.ToLowerInvariant()
        if ($languageKey -notin $backendByLanguage.Keys -or $backend -cne $backendByLanguage[$languageKey]) {
            throw "S20 KPI record has an unexpected backend '$backend' for language '$language'."
        }
        $dimensionKey = "$($feature.ToLowerInvariant())|$languageKey|$($backend.ToLowerInvariant())"
        if ($seenDimensions.ContainsKey($dimensionKey)) { throw "S20 KPI report repeats dimension '$dimensionKey'." }
        $seenDimensions[$dimensionKey] = $true
        $pairKey = "$languageKey|$($feature.ToLowerInvariant())"
        if ($requiredPairs.ContainsKey($pairKey)) { $requiredPairs[$pairKey] = $true }
        else { throw "S20 KPI report contains an unsupported feature/language pair '$pairKey'." }

        if ($record.metrics -isnot [System.Management.Automation.PSCustomObject]) { throw "S20 KPI record '$dimensionKey' has no metrics object." }
        foreach ($spec in $metrics) {
            $metricProperty = $record.metrics.PSObject.Properties[$spec.name]
            if (-not $metricProperty) { throw "S20 KPI record '$dimensionKey' is missing metric '$($spec.name)'." }
            $metric = $metricProperty.Value
            if ([string]$metric.formula -cne $spec.formula) { throw "S20 KPI metric '$($spec.name)' changed formula for '$dimensionKey'." }
            $readOnlyDimension = ($feature -in @('definition', 'references') -and $languageKey -in @('go', 'c', 'c++', 'rust', 'python', 'typescript', 'javascript')) -or
                ($feature -ceq 'position-mapping' -and $languageKey -ceq 'go')
            $shouldBeNotApplicable = $spec.name -ceq 'editValidationFailureRate' -and $readOnlyDimension
            if ($shouldBeNotApplicable -and [string]$metric.status -cne 'not_applicable') {
                throw "S20 edit-validation metric must be explicitly not_applicable for read-only dimension '$dimensionKey'."
            }
            if (-not $shouldBeNotApplicable -and [string]$metric.status -ceq 'not_applicable') {
                throw "S20 metric '$($spec.name)' cannot be not_applicable for '$dimensionKey'."
            }
            switch ([string]$metric.status) {
                'observed' {
                    if ($metric.numerator -isnot [long] -or $metric.denominator -isnot [long] -or
                        $metric.denominator -le 0 -or $metric.numerator -lt 0 -or $metric.numerator -gt $metric.denominator -or
                        ($metric.value -isnot [double] -and $metric.value -isnot [long]) -or
                        [double]::IsNaN([double]$metric.value) -or [double]::IsInfinity([double]$metric.value) -or
                        [Math]::Abs([double]$metric.value - ([double]$metric.numerator / [double]$metric.denominator)) -gt 1e-12) {
                        throw "S20 observed metric '$($spec.name)' has absent, invalid, or formula-inconsistent counts for '$dimensionKey'."
                    }
                }
                'not_verified' {
                    $sawNotVerified = $true
                    if ($metric.PSObject.Properties['value'] -and $null -ne $metric.value -or
                        [string]::IsNullOrWhiteSpace([string]$metric.reason)) {
                        throw "S20 unverified metric '$($spec.name)' has a fabricated value or no reason for '$dimensionKey'."
                    }
                    $limitationFound = @($observed.limitations | Where-Object {
                        [string]$_.feature -ceq $feature -and [string]$_.language -ceq $language -and
                        [string]$_.backend -ceq $backend -and [string]$_.metric -ceq $spec.name -and
                        [string]::IsNullOrWhiteSpace([string]$_.reason) -eq $false
                    }).Count -gt 0
                    if (-not $limitationFound) { throw "S20 unverified metric '$($spec.name)' is absent from structured limitations for '$dimensionKey'." }
                }
                'not_applicable' {
                    if (-not $shouldBeNotApplicable -or
                        [string]::IsNullOrWhiteSpace([string]$metric.reason) -or
                        [string]$metric.reason -notmatch '(?i)read-only' -or
                        [string]$metric.reason -notmatch '(?i)WorkspaceEdit' -or
                        ($metric.PSObject.Properties['value'] -and $null -ne $metric.value) -or
                        ($metric.PSObject.Properties['numerator'] -and $null -ne $metric.numerator) -or
                        ($metric.PSObject.Properties['denominator'] -and $null -ne $metric.denominator)) {
                        throw "S20 not-applicable metric '$($spec.name)' has no valid read-only disposition for '$dimensionKey'."
                    }
                    $notApplicableDimensions["$($languageKey)|$($feature.ToLowerInvariant())|$($spec.name)"] = [string]$metric.reason
                }
                'invalid' {
                    $sawInvalid = $true
                    if (($metric.PSObject.Properties['value'] -and $null -ne $metric.value) -or
                        [string]::IsNullOrWhiteSpace([string]$metric.reason)) {
                        throw "S20 invalid metric '$($spec.name)' has a value or no reason for '$dimensionKey'."
                    }
                }
                default { throw "S20 metric '$($spec.name)' has unsupported status '$($metric.status)'." }
            }
        }
    }
    foreach ($pair in $requiredPairs.Keys) { if (-not $requiredPairs[$pair]) { throw "S20 KPI report is missing required feature/language record '$pair'." } }
    $declaredNotApplicable = @{}
    foreach ($item in @($observed.notApplicable)) {
        $language = [string]$item.language
        $feature = [string]$item.feature
        $metricName = [string]$item.metric
        $reason = [string]$item.reason
        $key = "$($language.ToLowerInvariant())|$($feature.ToLowerInvariant())|$metricName"
        if ([string]::IsNullOrWhiteSpace($language) -or [string]::IsNullOrWhiteSpace($feature) -or
            [string]::IsNullOrWhiteSpace($reason) -or $metricName -cne 'editValidationFailureRate' -or
            -not $notApplicableDimensions.ContainsKey($key) -or $notApplicableDimensions[$key] -cne $reason -or
            $declaredNotApplicable.ContainsKey($key)) {
            throw "S20 notApplicable list contains an unsupported, duplicate, or mismatched dimension '$key'."
        }
        $declaredNotApplicable[$key] = $reason
    }
    if ($declaredNotApplicable.Count -ne $notApplicableDimensions.Count -or $notApplicableDimensions.Count -ne 15) {
        throw 'S20 notApplicable list does not exactly match the 15 read-only edit-validation metric cells.'
    }
    if (@($observed.duplicateDimensions).Count -ne 0) { throw 'S20 KPI report declares duplicate dimensions.' }
    if ($sawInvalid -and [string]$KpiCheck.status -cne 'failed') { throw 'S20 KPI check did not fail despite an invalid metric.' }
    if (-not $sawInvalid -and $sawNotVerified -and [string]$KpiCheck.status -cne 'not_verified') { throw 'S20 KPI check passed despite unverified metrics.' }
    if (-not $sawInvalid -and -not $sawNotVerified -and [string]$KpiCheck.status -cne 'passed') { throw 'S20 KPI check is not passed despite complete observations.' }
}

function ConvertTo-SemanticLocationKey($Location) {
    if ($null -eq $Location) { return $null }
    $uriText = [string]$Location.uri
    if ([string]::IsNullOrWhiteSpace($uriText)) { return $null }
    try {
        $uri = [Uri]::new($uriText, [UriKind]::Absolute)
    } catch {
        return $null
    }
    if ($uri.IsFile) {
        try {
            $uriKey = [IO.Path]::GetFullPath($uri.LocalPath).TrimEnd([IO.Path]::DirectorySeparatorChar, [IO.Path]::AltDirectorySeparatorChar).ToUpperInvariant()
        } catch {
            return $null
        }
    } else {
        $uriKey = $uri.GetComponents([UriComponents]::AbsoluteUri, [UriFormat]::SafeUnescaped)
    }
    $coordinates = @()
    foreach ($name in @('startLine', 'startCharacter', 'endLine', 'endCharacter')) {
        $property = $Location.PSObject.Properties[$name]
        if (-not $property -or $null -eq $property.Value) { return $null }
        try { $coordinates += [long]$property.Value } catch { return $null }
        if ($coordinates[-1] -lt 0) { return $null }
    }
    $parts = @($uriKey) + $coordinates
    return (ConvertTo-Json -InputObject $parts -Compress)
}

function Test-SemanticLocation($Locations, $Expected) {
    $expectedKey = ConvertTo-SemanticLocationKey $Expected
    if (-not $expectedKey) { return $false }
    foreach ($location in @($Locations)) {
        $locationKey = ConvertTo-SemanticLocationKey $location
        if ($locationKey -and [string]::Equals([string]$locationKey, [string]$expectedKey, [StringComparison]::Ordinal)) {
            return $true
        }
    }
    return $false
}

function Test-SemanticLocationSetsEqual($Left, $Right) {
    $leftKeys = [Collections.Generic.HashSet[string]]::new([StringComparer]::Ordinal)
    $rightKeys = [Collections.Generic.HashSet[string]]::new([StringComparer]::Ordinal)
    foreach ($location in @($Left)) {
        $key = ConvertTo-SemanticLocationKey $location
        if (-not $key) { return $false }
        [void]$leftKeys.Add([string]$key)
    }
    foreach ($location in @($Right)) {
        $key = ConvertTo-SemanticLocationKey $location
        if (-not $key) { return $false }
        [void]$rightKeys.Add([string]$key)
    }
    if ($leftKeys.Count -ne $rightKeys.Count) { return $false }
    foreach ($key in $leftKeys) {
        if (-not $rightKeys.Contains($key)) { return $false }
    }
    return $true
}

function Assert-StructuredReportIdentity(
    [string] $Path,
    [string] $Kind,
    [string] $ExpectedRunId,
    [string] $ExpectedCandidateHash,
    [DateTime] $StartedAt,
    [string[]] $RequiredCheckIds = @(),
    [string] $ExpectedDuration = '',
    [int] $ProcessExitCode = 0
) {
    if (-not (Test-Path -LiteralPath $Path)) { throw "$Kind did not write a fresh structured report." }
    if ($ProcessExitCode -ne 0) { throw "$Kind producer exited with code $ProcessExitCode; its report cannot pass." }
    $file = Get-Item -LiteralPath $Path
    if ($StartedAt -ne [DateTime]::MinValue -and $file.LastWriteTimeUtc -lt $StartedAt.AddSeconds(-2)) { throw "$Kind report predates this invocation." }
    try { $data = Get-Content -LiteralPath $Path -Raw | ConvertFrom-Json -ErrorAction Stop } catch { throw "$Kind report is invalid JSON: $($_.Exception.Message)" }
    $isPerformance = $Kind -eq 'performance'
    if ($isPerformance) { $script:FastBoundedS18ExceptionIds = @() }
    if ($data.schema_version -isnot [long] -or $data.schema_version -ne 1) {
        throw "$Kind report schema_version is missing or unsupported."
    }
    $run = [string]$data.run_id
    $candidate = [string]$data.candidate.sha256
    $candidate = $candidate -replace '^sha256:', ''
    if ($run -cne $ExpectedRunId) { throw "$Kind report run ID '$run' does not match '$ExpectedRunId'." }
    if ($candidate -cne $ExpectedCandidateHash) { throw "$Kind report candidate hash does not match the frozen candidate." }
    if ($Kind -eq 'semantic') {
        $kpiMatches = @($data.checks | Where-Object { [string]$_.id -ceq "$ExpectedRunId/S20/accuracy-kpi" })
        if ($kpiMatches.Count -ne 1) { throw 'Semantic report must contain exactly one run-qualified S20 KPI check.' }
        Assert-S20KPIEvidence $kpiMatches[0] $ExpectedRunId
    }
    if ($Kind -eq 'clients' -and
        @((Get-ClientMatrixCellIds) | Where-Object { $_ -in @($RequiredCheckIds) }).Count -eq @((Get-ClientMatrixCellIds)).Count) {
        Assert-ClientMatrixContract $data
    }
    $reportDecision = [string]$data.decision
    if (-not $isPerformance -and $reportDecision -ne 'passed') {
        if ($ProcessExitCode -ne 0) { throw "$Kind report decision '$reportDecision' accompanied by producer exit code $ProcessExitCode; classified as failed." }
        return $data
    }
    if ($isPerformance -and $reportDecision -notin @('passed', 'failed', 'not_verified', 'running')) {
        throw "Performance report has invalid decision '$reportDecision'."
    }
    if ($isPerformance -and ([string]$data.release_assessment.decision -ceq 'not_verified' -or $reportDecision -in @('not_verified', 'running'))) {
        $null = Assert-S18ReleaseAssessment $data @() @() $S18Policy 0
        $script:FastReleaseAssessment = $data.release_assessment
        if ($ProcessExitCode -ne 0) { throw "S18/S19 test process exited with code $ProcessExitCode." }
        return $data
    }
    if ($reportDecision -eq 'passed' -and ($data.errors -or $data.skips)) { throw "$Kind passed while retaining errors or skipped cases." }
    if ($Kind -eq 'persistent') {
        $manifest = [string]$data.environment.fixtureManifest
        if ([string]::IsNullOrWhiteSpace($manifest)) { throw 'Persistent report lacks its immutable fixture manifest.' }
        $digest = 'sha256:' + [Convert]::ToHexString([Security.Cryptography.SHA256]::HashData([Text.Encoding]::UTF8.GetBytes($manifest))).ToLowerInvariant()
        if ([string]$data.corpus.sha256 -cne $digest) { throw 'Persistent fixture manifest hash does not match its corpus identity.' }
        foreach ($key in @('goos', 'goarch', 'goVersion', 'candidateProcess', 'fixtureLanguage')) {
            if ([string]::IsNullOrWhiteSpace([string]$data.environment.PSObject.Properties[$key].Value)) { throw "Persistent report lacks environment '$key'." }
        }
    }
    if (-not $isPerformance -and ($data.errors -or $data.skips)) { throw "$Kind passed while retaining errors or skipped cases." }
    if ($null -eq $data.checks -or @($data.checks).Count -eq 0) { throw "$Kind passed with no checks." }
    if (-not $data.started_at -or -not $data.finished_at) { throw "$Kind passed without start/finish timestamps." }
    try {
        $reportStarted = [DateTimeOffset]::Parse([string]$data.started_at).ToUniversalTime()
        $reportFinished = [DateTimeOffset]::Parse([string]$data.finished_at).ToUniversalTime()
    } catch { throw "$Kind passed with invalid timestamps." }
    if ($reportFinished -lt $reportStarted) { throw "$Kind finish time precedes its start time." }
    $actualIds = @{}
    foreach ($check in @($data.checks)) {
        $id = [string]$check.id
        if (-not $id -or $actualIds.ContainsKey($id)) { throw "$Kind report has a blank or duplicate check ID '$id'." }
        if ([string]$check.status -cne 'passed' -and -not ($isPerformance -and $reportDecision -eq 'failed' -and [string]$check.status -ceq 'failed')) {
            throw "$Kind report contains non-terminal check '$id' with status '$($check.status)'."
        }
        $actualIds[$id] = $check
    }
    foreach ($id in $RequiredCheckIds) {
        if (-not $actualIds.ContainsKey($id)) { throw "$Kind report is missing required check '$id'." }
        if ([string]$actualIds[$id].status -ne 'passed' -and
            -not ($isPerformance -and $reportDecision -eq 'failed' -and [string]$actualIds[$id].status -ceq 'failed')) {
            throw "$Kind report has non-passing required check '$id'."
        }
    }
    if ($Kind -eq 'persistent') {
        foreach ($id in $RequiredCheckIds) {
            $observed = $actualIds[$id].observed
            if ($id -match '/S21/((go|python|javascript|c|cpp)-)?persistent-index-only-queries$') {
                $fixtureLanguage = if ($Matches[2]) { [string]$Matches[2] } else { 'typescript' }
                $backendLanguage = if ($fixtureLanguage -ceq 'javascript') { 'typescript' } elseif ($fixtureLanguage -ceq 'c') { 'cpp' } else { $fixtureLanguage }
                if ([string]$data.environment.fixtureLanguage -cne $fixtureLanguage -or [string]$data.environment.backendLanguage -cne $backendLanguage) {
                    throw "Persistent check '$id' does not identify its expected fixture and backend language."
                }
                if ($observed.backendFound -isnot [bool] -or $observed.backendFound -or
                    $observed.generation -isnot [long] -or [long]$observed.generation -le 0 -or @($observed.definition).Count -eq 0 -or
                    @($observed.references).Count -lt 2 -or @($observed.workspaceSymbols).Count -eq 0 -or
                    $observed.definitionError -or $observed.referencesError -or $observed.workspaceSymbolError -or
                    $observed.queryTraceBefore.Computations -isnot [long] -or [long]$observed.queryTraceBefore.Computations -lt 0 -or
                    $observed.queryTraceAfter.Computations -isnot [long] -or [long]$observed.queryTraceAfter.Computations -lt 0 -or
                    [long]$observed.queryTraceAfter.Computations -ne [long]$observed.queryTraceBefore.Computations) {
                    throw "Persistent check '$id' lacks backend-disabled query evidence."
                }
                foreach ($method in @('workspace/symbol', 'textDocument/definition', 'textDocument/references')) {
                    $meta = @($observed.PSObject.Properties["resultMeta.$method"].Value)
                    if ($meta.Count -ne 1 -or [string]$meta[0].status -cne 'exact' -or [string]$meta[0].completeness -cne 'complete' -or
                        @($meta[0].evidence | Where-Object { [long]$_.IndexGen -eq [long]$observed.generation }).Count -eq 0) {
                        throw "Persistent check '$id' lacks fixed-generation evidence for '$method'."
                    }
                }
            } elseif ($id -match '/S22/go-cli-scip-replay-persistence$') {
                if ($observed.recordedIdentity -isnot [bool] -or -not $observed.recordedIdentity -or
                    [long]$observed.sourceGeneration -le 0 -or [long]$observed.importedGeneration -le 0 -or
                    [long]$observed.exportedDocuments -le 0 -or [long]$observed.exportedOccurrences -le 0) {
                    throw "SCIP/replay check '$id' lacks nonempty generation-bound evidence."
                }
                foreach ($key in @('wrongConfigRejected', 'wrongContentIdentityRejected', 'wrongSourceRootRejected', 'wrongToolIdentityRejected', 'missingGenerationRejected', 'fixedGenerationReplay')) {
                    if (-not $observed.PSObject.Properties[$key]) { throw "SCIP/replay check '$id' lacks '$key'." }
                    $command = $observed.PSObject.Properties[$key].Value
                    if ($command.exitCode -isnot [long] -or $command.completed -isnot [bool] -or -not $command.completed -or
                        $command.outputLimitExceeded -isnot [bool] -or $command.outputLimitExceeded) {
                        throw "SCIP/replay check '$id/$key' lacks a complete bounded process outcome."
                    }
                    if ($key -ceq 'fixedGenerationReplay') {
                        if ([long]$command.exitCode -ne 0 -or [string]$command.exitError -or [string]$command.stdout -notmatch 'REPLAY-OK\s+.*semantic=complete') {
                            throw "SCIP/replay check '$id' does not prove successful semantic replay."
                        }
                    } elseif ([long]$command.exitCode -le 0 -or -not [string]$command.exitError -or -not [string]$command.stderr) {
                        throw "SCIP/replay check '$id/$key' does not prove the expected command refusal."
                    }
                }
            } else {
                throw "Unsupported persistent check '$id'."
            }
        }
    }
    if ($isPerformance) {
        $expectedPolicyID = if ($S18Policy -ceq 'stable') { 's18-evidence-qualified-v2' } else { 's18-strict-v1' }
        $expectedPolicyMode = $S18Policy
        if ([string]$data.environment.s18PolicyID -cne $expectedPolicyID -or
            [string]$data.environment.releasePolicyID -cne $expectedPolicyID -or
            [string]$data.environment.s18PolicyMode -cne $expectedPolicyMode -or
            [string]$data.environment.s18StableExceptionScope -cne 'c/cpp completion_first_usable and syntax_update_after_edit only; raw single-request samples plus per-request candidate overhead required; strict thresholds remain authoritative') {
            throw 'Performance report does not identify the exact frozen S18 release policy and exception scope.'
        }
        $manifestText = [string]$data.environment.s18FixtureManifest
        $manifestHash = [string]$data.environment.s18FixtureManifestSHA256
        if ([string]::IsNullOrWhiteSpace($manifestText) -or -not $manifestHash.StartsWith('sha256:', [StringComparison]::Ordinal)) {
            throw 'Performance report is missing the hashed S18 fixture manifest.'
        }
        $manifestDigest = 'sha256:' + [Convert]::ToHexString([Security.Cryptography.SHA256]::HashData([Text.Encoding]::UTF8.GetBytes($manifestText))).ToLowerInvariant()
        if ($manifestDigest -cne $manifestHash) { throw 'Performance report S18 fixture manifest hash does not match its serialized manifest.' }
        try { $manifest = $manifestText | ConvertFrom-Json -ErrorAction Stop } catch { throw "Performance report S18 fixture manifest is invalid JSON: $($_.Exception.Message)" }
        $expectedFixtureIds = @('go', 'c', 'cpp', 'rust', 'python', 'typescript', 'javascript')
        $manifestIds = @($manifest.representativeFixtures | ForEach-Object { [string]$_.id })
        if ($manifest.coverageScope -cnotmatch 'representative Tier S performance fixtures; not exhaustive' -or
            @($manifest.corpusFiles).Count -eq 0 -or $manifestIds.Count -ne $expectedFixtureIds.Count -or
            @($expectedFixtureIds | Where-Object { $_ -cnotin $manifestIds }).Count -gt 0 -or
            @($manifest.representativeFixtures | Where-Object {
                -not $_.language -or -not $_.languageId -or -not $_.queryFile -or -not $_.queryToken -or
                -not $_.targetFile -or -not $_.expectedSymbol -or @($_.openFiles).Count -eq 0 -or
                @($_.toolNames).Count -eq 0 -or -not $_.upstreamBinary
            }).Count -gt 0) {
            throw 'Performance report S18 manifest does not describe all seven representative language fixtures.'
        }
        $toolVersionNames = @('go', 'gopls', 'clangd', 'clang', 'clang++', 'rust-analyzer', 'rustc', 'cargo', 'pyright', 'python', 'typescript-language-server', 'typescript', 'node', 'npm', 'tools.lock.json')
        foreach ($name in $toolVersionNames) {
            $property = $data.environment.PSObject.Properties["toolVersion.$name"]
            if (-not $property -or [string]::IsNullOrWhiteSpace([string]$property.Value) -or [string]$property.Value -match '(?i)^(missing|unavailable)(\b|:)' ) {
                throw "Performance report is missing the locked tool version for '$name'."
            }
        }
        $currentToolLockHash = 'sha256:' + (Get-FileSha256 (Join-Path $repoRoot 'test/acceptance/tools/tools.lock.json'))
        if ([string]$data.environment.PSObject.Properties['toolVersion.tools.lock.json'].Value -cne $currentToolLockHash) {
            throw 'Performance report tool-lock hash does not match the current pinned tool lock.'
        }
        $contentHash = [string]$data.environment.corpusContentSHA256
        $runCorpusHash = [string]$data.corpus.sha256
        if ($contentHash -notmatch '^sha256:[0-9a-f]{64}$' -or $runCorpusHash -notmatch '^sha256:[0-9a-f]{64}$' -or
            [int]$data.environment.corpusFiles -ne @($manifest.corpusFiles).Count) {
            throw 'Performance report is missing its corpus content hash, run-qualified hash, or consistent file count.'
        }
        $operationLimits = @{
            hot_hover = @(20, 75, 150)
            hot_definition = @(25, 100, 200)
            completion_first_usable = @(40, 120, 250)
            syntax_update_after_edit = @(15, 50, 100)
        }
        $fixtureById = @{}
        foreach ($fixture in @($manifest.representativeFixtures)) { $fixtureById[[string]$fixture.id] = $fixture }
        $operationMethods = @{
            hot_hover = 'textDocument/hover'
            hot_definition = 'textDocument/definition'
            completion_first_usable = 'textDocument/completion'
            syntax_update_after_edit = 'textDocument/documentSymbol'
        }
        $sampleById = @{}
        foreach ($sample in @($data.samples)) {
            $sampleId = [string]$sample.id
            if (-not $sampleId -or $sampleById.ContainsKey($sampleId)) { throw 'Performance report has a blank or duplicate sample ID.' }
            $sampleById[$sampleId] = $sample
        }
        $strictFailureIds = [Collections.Generic.List[string]]::new()
        $boundedExceptionIds = [Collections.Generic.List[string]]::new()
        foreach ($language in @('go', 'c', 'cpp', 'rust', 'python', 'typescript', 'javascript')) {
            foreach ($operation in $operationLimits.Keys) {
                $sampleId = "$ExpectedRunId/S18/$language/$operation"
                if (-not $sampleById.ContainsKey($sampleId)) { throw "Performance report is missing S18 raw samples '$sampleId'." }
                $sample = $sampleById[$sampleId]
                $check = $actualIds[$sampleId]
                $expected = $operationLimits[$operation]
                $fixture = $fixtureById[$language]
                $expectedSymbol = [string]$fixture.queryToken
                if ($operation -eq 'syntax_update_after_edit') { $expectedSymbol = [string]$fixture.expectedSymbol + 'Updated' }
                $validationEvidence = $check.observed.validation
                $differentialEvidence = $check.observed.upstream_differential
                $method = $operationMethods[$operation]
                $candidateNormalizedJSON = if ($null -ne $validationEvidence.candidate_normalized) { ConvertTo-Json -InputObject $validationEvidence.candidate_normalized -Depth 40 -Compress } else { '' }
                $upstreamNormalizedJSON = if ($null -ne $validationEvidence.upstream_normalized) { ConvertTo-Json -InputObject $validationEvidence.upstream_normalized -Depth 40 -Compress } else { '' }
                $semanticEvidenceValid = $validationEvidence -is [System.Management.Automation.PSCustomObject] -and
                    $differentialEvidence -is [System.Management.Automation.PSCustomObject] -and
                    [string]$validationEvidence.expected_symbol -ceq $expectedSymbol -and
                    [string]$validationEvidence.candidate_status -ceq 'passed' -and
                    [string]$validationEvidence.candidate_normalization_status -ceq 'passed' -and
                    [string]$validationEvidence.upstream_status -ceq 'passed' -and
                    [string]$validationEvidence.upstream_normalization_status -ceq 'passed' -and
                    [string]$differentialEvidence.method -ceq $method -and
                    [string]$differentialEvidence.expected_symbol -ceq $expectedSymbol -and
                    [string]$differentialEvidence.status -ceq 'passed' -and
                    $differentialEvidence.matched -eq $true -and
                    $candidateNormalizedJSON -ceq $upstreamNormalizedJSON -and
                    (Test-S18NormalizedObservation $validationEvidence.candidate_normalized $method $expectedSymbol) -and
                    (Test-S18NormalizedObservation $validationEvidence.upstream_normalized $method $expectedSymbol) -and
                    (Test-S18CompletionListEvidence $language $operation $differentialEvidence)
                $exceptionLimit = Get-S18StableExceptionLimit $language $operation
                $reportedExceptionLimit = $check.observed.performance_exception_threshold
                if ($null -ne $exceptionLimit) {
                    if ($reportedExceptionLimit.p50_ms -isnot [long] -or $reportedExceptionLimit.p50_ms -ne $exceptionLimit[0] -or
                        $reportedExceptionLimit.p95_ms -isnot [long] -or $reportedExceptionLimit.p95_ms -ne $exceptionLimit[1] -or
                        $reportedExceptionLimit.p99_ms -isnot [long] -or $reportedExceptionLimit.p99_ms -ne $exceptionLimit[2]) {
                        throw "Performance report exception ceiling is missing or changed for '$sampleId'."
                    }
                } elseif ($null -ne $reportedExceptionLimit) {
                    throw "Performance report includes an out-of-scope exception ceiling for '$sampleId'."
                }
                if ($operation -eq 'syntax_update_after_edit') {
                    $semanticEvidenceValid = $semanticEvidenceValid -and
                        (Test-S18EditUpdateEvidence $check.observed.edit_update ([string]$fixture.expectedSymbol) $expectedSymbol)
                }
                if ($sample.unit -cne 'ns/op' -or $sample.warmup -isnot [long] -or $sample.warmup -ne 32 -or
                    @($sample.raw).Count -lt 1000 -or $check.observed.sample_count -isnot [long] -or $check.observed.sample_count -ne @($sample.raw).Count -or
                    @($sample.raw | Where-Object { ($_ -isnot [long] -and $_ -isnot [double]) -or [double]$_ -lt 0 }).Count -gt 0 -or
                    $check.threshold.p50_ms -isnot [long] -or $check.threshold.p50_ms -ne $expected[0] -or
                    $check.threshold.p95_ms -isnot [long] -or $check.threshold.p95_ms -ne $expected[1] -or
                    $check.threshold.p99_ms -isnot [long] -or $check.threshold.p99_ms -ne $expected[2] -or
                    -not $semanticEvidenceValid -or
                    ($operation -eq 'hot_hover' -and [string]$check.observed.presentation_comparison -notmatch '^(display-only Markdown difference observed|no display-only difference observed); normalized semantic content matched$')) {
                    throw "Performance report has incomplete samples, semantic evidence, or changed goal thresholds for '$sampleId'."
                }
                $ordered = @($sample.raw | ForEach-Object { [double]$_ } | Sort-Object)
                $strictExceeded = $false
                $measuredMs = [Collections.Generic.List[double]]::new()
                foreach ($percentile in @(@{ name = 'p50'; q = 0.50; threshold = 0 }, @{ name = 'p95'; q = 0.95; threshold = 1 }, @{ name = 'p99'; q = 0.99; threshold = 2 })) {
                    $rank = [Math]::Ceiling([double]$percentile.q * $ordered.Count)
                    $measured = $ordered[[Math]::Max(0, $rank - 1)]
                    $reported = [double]$sample.($percentile.name)
                    $observedName = "$($percentile.name)_ns"
                    if ($reported -ne $measured -or [double]$check.observed.$observedName -ne $measured) {
                        throw "Performance report percentile does not match raw S18 samples for '$sampleId' ($($percentile.name))."
                    }
                    $measuredMs.Add($measured / 1000000.0)
                    if ($measured -gt ([double]$expected[$percentile.threshold] * 1000000)) { $strictExceeded = $true }
                }
                if ($strictExceeded) {
                    $failureTypes = @($check.observed.failure_types | ForEach-Object { [string]$_ })
                    if ([string]$check.status -cne 'failed' -or [string]$check.observed.failure_type -cne 'latency' -or
                        $failureTypes.Count -ne 1 -or $failureTypes[0] -cne 'latency') {
                        throw "S18 strict latency failure '$sampleId' is not recorded as a typed latency failure."
                    }
                    $strictFailureIds.Add($sampleId)
                } elseif ([string]$check.status -cne 'passed') {
                    throw "S18 '$sampleId' failed despite meeting the strict latency thresholds."
                }
            }
        }
        if ($S18Policy -ceq 'stable') {
            foreach ($id in @(Get-S18StableABBAExceptions $data $ExpectedRunId $sampleById $actualIds $strictFailureIds.ToArray())) {
                $boundedExceptionIds.Add([string]$id)
            }
        } elseif ($null -ne $data.abba_evidence -and @($data.abba_evidence.PSObject.Properties).Count -gt 0) {
            throw 'Strict S18 policy cannot consume stable v2 ABBA evidence.'
        }
        foreach ($id in $RequiredCheckIds) {
            if ($id -notin $strictFailureIds -and [string]$actualIds[$id].status -cne 'passed') {
                throw "Performance report has a non-latency failure in required check '$id'."
            }
        }
        if ($strictFailureIds.Count -eq 0 -and $reportDecision -cne 'passed') {
            throw 'Performance report failed without a recomputed strict S18 latency breach.'
        }
        if ($strictFailureIds.Count -gt 0 -and $reportDecision -cne 'failed') {
            throw 'Performance report does not retain its strict S18 latency failure decision.'
        }
        $script:FastBoundedS18ExceptionIds = @($boundedExceptionIds.ToArray())
        $releaseDecision = Assert-S18ReleaseAssessment $data $strictFailureIds.ToArray() $boundedExceptionIds.ToArray() $S18Policy 0
        foreach ($requested in @(200, 800, 3200)) {
            $sampleId = "$ExpectedRunId/S19/references@$requested"
            $sample = $sampleById[$sampleId]
            $check = $actualIds[$sampleId]
            Assert-S19ReturnedLocationScale $check $sample $requested
        }
        $cancellation = $actualIds["$ExpectedRunId/S19/active-cancellation"].observed
        $progress = $actualIds["$ExpectedRunId/S19/progress"].observed
        $fairness = $actualIds["$ExpectedRunId/S19/no-starvation"].observed
        if (-not $cancellation.attempted -or -not $cancellation.progress_began -or
            $cancellation.progress_token -cne 'omnilsp-s19-cancel' -or
            [string]::IsNullOrWhiteSpace([string]$cancellation.server_terminal) -or
            $cancellation.request_id -isnot [long] -or $cancellation.request_id -le 0 -or
            $cancellation.server_error_code -isnot [long] -or $cancellation.server_error_code -ne -32800 -or
            [string]::IsNullOrWhiteSpace([string]$cancellation.caller_outcome) -or
            -not $progress.begin_observed -or -not $progress.end_observed -or
            -not $fairness.reference_was_active -or -not $fairness.reference_progress_began -or
            $fairness.reference_progress_token -cne 'omnilsp-s19-starve' -or
            -not $fairness.reference_terminal_success -or -not $fairness.reference_completed_naturally -or
            $fairness.reference_terminal -cne 'completed successfully' -or
            $fairness.reference_result_count -ne $fairness.expected_reference_result_count -or $fairness.reference_result_count -ne 3200 -or
            $fairness.reference_terminal_result_count -ne 3200 -or
            $fairness.completion_status -cne 'pass' -or $fairness.hover_status -cne 'pass' -or
            $fairness.budget_ms -isnot [long] -or $fairness.budget_ms -ne 2000 -or
            $fairness.completion_ns -isnot [long] -or $fairness.completion_ns -lt 0 -or $fairness.completion_ns -gt 2000000000 -or
            $fairness.hover_ns -isnot [long] -or $fairness.hover_ns -lt 0 -or $fairness.hover_ns -gt 2000000000 -or
            -not (Test-S19ConcurrentRequestIdentity $fairness) -or
            $fairness.reference_begin_event_index -isnot [long] -or $fairness.reference_begin_event_index -lt 0 -or
            $fairness.reference_end_event_index -isnot [long] -or $fairness.reference_end_event_index -lt 0 -or
            $fairness.completion_response_event_index -isnot [long] -or $fairness.completion_response_event_index -lt 0 -or
            $fairness.hover_response_event_index -isnot [long] -or $fairness.hover_response_event_index -lt 0 -or
            $fairness.reference_terminal_response_event_index -isnot [long] -or $fairness.reference_terminal_response_event_index -lt 0 -or
            $fairness.reference_begin_event_index -ge $fairness.completion_response_event_index -or
            $fairness.reference_begin_event_index -ge $fairness.hover_response_event_index -or
            $fairness.completion_response_event_index -ge $fairness.reference_terminal_response_event_index -or
            $fairness.hover_response_event_index -ge $fairness.reference_terminal_response_event_index -or
            $fairness.reference_end_event_index -le $fairness.completion_response_event_index -or
            $fairness.reference_end_event_index -le $fairness.hover_response_event_index) {
            throw 'Performance report passed without valid cancellation, progress, or no-starvation evidence.'
        }
        Assert-S19ProcessTreeResourceEvidence $actualIds["$ExpectedRunId/S19/process-tree-resources"]
    }
    if ($Kind -eq 'clients') {
        foreach ($language in $clientMatrixLanguages) {
            $id = "client/emacs-eglot/$language"
            if ($actualIds.ContainsKey($id)) { Assert-EmacsCellEvidence $actualIds[$id] $language }
        }
        $expectedSubcases = @{
            'client/vscode/go' = @('go')
            'client/vscode/cpp' = @('c', 'cpp')
            'client/vscode/rust' = @('rust')
            'client/vscode/python' = @('python')
            'client/vscode/typescript' = @('javascript', 'javascriptreact', 'typescript', 'typescriptreact')
            'client/neovim/go' = @('go')
            'client/neovim/cpp' = @('c', 'cpp')
            'client/neovim/rust' = @('rust')
            'client/neovim/python' = @('python')
            'client/neovim/typescript' = @('javascript', 'javascriptreact', 'typescript', 'typescriptreact')
        }
        foreach ($checkId in @(Get-ClientMatrixSummaryIds)) {
            if (-not $actualIds.ContainsKey($checkId)) { continue }
            if (-not $expectedSubcases.ContainsKey($checkId)) { throw "Client report validator has no fixed subcase contract for '$checkId'." }
            $actualSubcases = @($actualIds[$checkId].observed.subcase_names | ForEach-Object { [string]$_ })
            $expected = @($expectedSubcases[$checkId])
            if ($actualSubcases.Count -ne $expected.Count -or
                [String]::Join("`n", $actualSubcases) -cne [String]::Join("`n", $expected)) {
                throw "Client report has missing, extra, or reordered subcases for '$checkId'."
            }
            $observed = $actualIds[$checkId].observed
            $subcases = @($observed.subcases)
            if ($observed -isnot [System.Management.Automation.PSCustomObject] -or $subcases.Count -ne $expected.Count) {
                throw "Client report '$checkId' does not carry one detailed observation per fixed subcase."
            }
            foreach ($subcaseName in $expected) {
                $rows = @($subcases | Where-Object { [string]$_.name -ceq $subcaseName })
                if ($rows.Count -ne 1 -or [string]$rows[0].languageId -cne $subcaseName -or [string]$rows[0].status -cne 'passed') {
                    throw "Client report '$checkId' has an absent, remapped, or non-passing '$subcaseName' subcase."
                }
                $diagnostic = $rows[0].diagnostic
                if ($checkId.EndsWith('/cpp', [StringComparison]::Ordinal)) {
                    if ($diagnostic -isnot [System.Management.Automation.PSCustomObject] -or
                        [string]$diagnostic.diagnostic_scope -cne 'clean_source_no_false_positive' -or
                        [string]$diagnostic.deferred_capability -cne 'DEF-CCLSDIAG' -or
                        $diagnostic.published_count -isnot [long] -or $diagnostic.published_count -ne 0 -or
                        [string]$observed.diagnostic_scope -cne 'clean_source_no_false_positive' -or
                        [string]$observed.deferred_capability -cne 'DEF-CCLSDIAG') {
                        throw "Client report '$checkId' lacks its explicit clean-source diagnostic scope and accepted deferral."
                    }
                } else {
                    # LSP Diagnostic.code is optional; semantic content, source, severity, and range are required.
                    if ($diagnostic -isnot [System.Management.Automation.PSCustomObject] -or
                        [string]$diagnostic.diagnostic_scope -cne 'semantic_unresolved_name' -or
                        [string]$diagnostic.severity -cne 'error' -or
                        [string]::IsNullOrWhiteSpace([string]$diagnostic.source) -or
                        [string]::IsNullOrWhiteSpace([string]$diagnostic.message) -or
                        $diagnostic.start.line -isnot [long] -or $diagnostic.start.character -isnot [long] -or
                        $diagnostic.end.line -isnot [long] -or $diagnostic.end.character -isnot [long] -or
                        $diagnostic.end.line -lt $diagnostic.start.line -or
                        ($diagnostic.end.line -eq $diagnostic.start.line -and $diagnostic.end.character -le $diagnostic.start.character)) {
                        throw "Client report '$checkId' has missing semantic diagnostic content or range for '$subcaseName'."
                    }
                }
            }
        }
    }
    if ($Kind -eq 'semantic') {
        $contentHash = [string]$data.environment.corpusContentSHA256
        $runCorpusHash = [string]$data.corpus.sha256
        if ($contentHash -notmatch '^sha256:[0-9a-f]{64}$' -or $runCorpusHash -notmatch '^sha256:[0-9a-f]{64}$' -or
            [string]$data.corpus.name -notmatch [regex]::Escape($ExpectedRunId)) {
            throw 'Semantic report is missing its workspace corpus hash or run-qualified corpus identity.'
        }
        $lockHash = 'sha256:' + (Get-FileSha256 (Join-Path $repoRoot 'test/acceptance/tools/tools.lock.json'))
        if ([string]$data.environment.PSObject.Properties['toolVersion.tools.lock.json'].Value -cne $lockHash) {
            throw 'Semantic report tools.lock.json hash does not match the current pinned tool lock.'
        }
        foreach ($name in @('go', 'gopls', 'clangd', 'clang', 'clang++', 'rust-analyzer', 'rustc', 'cargo', 'python', 'python3', 'node', 'npm', 'pyright', 'typescript-language-server', 'typescript')) {
            $property = $data.environment.PSObject.Properties["toolVersion.$name"]
            if (-not $property -or [string]::IsNullOrWhiteSpace([string]$property.Value) -or [string]$property.Value -match '(?i)^(missing|unavailable)(\b|:)') {
                throw "Semantic report is missing an observed tool version for '$name'."
            }
        }
        foreach ($language in @('go', 'c', 'cplusplus', 'rust', 'python', 'typescript', 'javascript')) {
            $checkId = "$ExpectedRunId/S20/language/$language"
            $observed = $actualIds[$checkId].observed
            $definition = @($observed.definition)
            $references = @($observed.references)
            $upstreamDefinition = @($observed.upstreamDefinition)
            $upstreamReferences = @($observed.upstreamReferences)
            if (-not $observed.upstream -or $definition.Count -eq 0 -or $references.Count -eq 0 -or
                -not (Test-SemanticLocation $definition $observed.expectedDefinition) -or
                -not (Test-SemanticLocation $references $observed.expectedDefinition) -or
                -not (Test-SemanticLocation $references $observed.expectedCall) -or
                -not (Test-SemanticLocationSetsEqual $definition $upstreamDefinition) -or
                -not (Test-SemanticLocationSetsEqual $references $upstreamReferences)) {
                throw "Semantic report '$checkId' lacks exact candidate/upstream definition and reference locations."
            }
        }
        $unchanged = $actualIds["$ExpectedRunId/S20/unmodified-edit"].observed
        if (-not (Test-SemanticLocationSetsEqual @($unchanged.beforeDefinition) @($unchanged.afterDefinition)) -or
            -not (Test-SemanticLocationSetsEqual @($unchanged.beforeReferences) @($unchanged.afterReferences))) {
            throw 'Semantic report unchanged-edit evidence does not preserve exact locations.'
        }
        $unsaved = $actualIds["$ExpectedRunId/S20/unsaved-edit"].observed
        if (-not $unsaved.changedTextSentToServer -or $unsaved.diskWritePerformed -or
            -not (Test-SemanticLocation @($unsaved.newDefinition) $unsaved.expectedDefinition) -or
            -not (Test-SemanticLocation @($unsaved.afterReferences) $unsaved.expectedNewCall)) {
            throw 'Semantic report unsaved-edit evidence does not prove a fresh in-memory reference without disk writes.'
        }
        $unicode = $actualIds["$ExpectedRunId/S20/unicode-crlf"].observed
        if ([string]$unicode.lineEnding -cne 'CRLF' -or -not (Test-SemanticLocation @($unicode.locations) $unicode.expected)) {
            throw 'Semantic report Unicode/CRLF evidence does not contain the exact UTF-16 target location.'
        }
        $cpp = $actualIds["$ExpectedRunId/S20/cpp-build-context"].observed
        $build = $cpp.fixtureBuildConfiguration
        $cppEvidence = $cpp.candidateEvidence
        $expectedMarker = $cppEvidence.expectedCxx20MarkerDefinition
        $compileDatabaseHash = [string]$build.compileDatabaseSHA256
        if ([string]$build.providedStandard -cne '-std=c++20' -or
            [string]::IsNullOrWhiteSpace([string]$build.providedIncludePath) -or
            [string]::IsNullOrWhiteSpace([string]$build.compileDatabasePath) -or
            $compileDatabaseHash -notmatch '^[0-9a-f]{64}$' -or
            [long]$cppEvidence.diagnosticRequestedAfterDocumentVersion -ne 2 -or
            [string]$cppEvidence.diagnosticKind -cne 'full' -or
            [string]$cppEvidence.cxx20MarkerRequestError -or [string]$cppEvidence.cxx20MarkerDecodeError -or
            -not (Test-SemanticLocation @($cppEvidence.cxx20MarkerDefinitions) $expectedMarker)) {
            throw 'Semantic report C++ build-context evidence lacks the exact standard and include directory.'
        }
        $brokenDiagnostics = $actualIds["$ExpectedRunId/S20/broken-source-diagnostics"].observed
        if ($brokenDiagnostics.candidateDiagnosticsVersion -isnot [long] -or $brokenDiagnostics.candidateDiagnosticsVersion -ne 1 -or
            $brokenDiagnostics.upstreamDiagnosticsVersion -isnot [long] -or $brokenDiagnostics.upstreamDiagnosticsVersion -ne 1 -or
            @($brokenDiagnostics.candidateMalformedErrors).Count -eq 0 -or @($brokenDiagnostics.upstreamMalformedErrors).Count -eq 0 -or
            -not $brokenDiagnostics.differential.stableFieldsMatch) {
            throw 'Semantic report malformed-source diagnostics lack matching versioned severity/range evidence.'
        }
        $broken = $actualIds["$ExpectedRunId/S20/broken-source"].observed
        if ([string]$broken.outcome -notin @('exact-safe-locations', 'safe-locations-completeness-not-asserted', 'supported-malformed-source-refusal')) {
            throw 'Semantic report does not classify the broken-source outcome as safe locations or a known refusal.'
        }
        $fixed = $actualIds["$ExpectedRunId/S20/diagnostic-edit"].observed
        if ($fixed.candidateDiagnosticsVersion -isnot [long] -or $fixed.candidateDiagnosticsVersion -ne 2 -or
            $fixed.upstreamDiagnosticsVersion -isnot [long] -or $fixed.upstreamDiagnosticsVersion -ne 2 -or
            @($fixed.candidateRemainingErrors).Count -ne 0 -or @($fixed.upstreamRemainingErrors).Count -ne 0 -or
            -not $fixed.differential.stableFieldsMatch -or -not $fixed.malformedLineRemoved) {
            throw 'Semantic report repaired-source evidence lacks fresh version-2 diagnostics or retains errors.'
        }
        $rename = $actualIds["$ExpectedRunId/S20/rename-safety"].observed
        if ($rename.errorCode -isnot [long] -or $rename.errorCode -ne -32803 -or -not $rename.filesUnchanged) {
            throw 'Semantic report does not prove typed rename refusal and unchanged source files.'
        }
    }
    if ($ExpectedDuration) {
        $durationStrings = @{ '30s' = '30s'; '10m' = '10m0s'; '1h' = '1h0m0s' }
        $durationSeconds = @{ '30s' = 30; '10m' = 600; '1h' = 3600 }
        if (-not $durationStrings.ContainsKey($ExpectedDuration) -or [string]$data.limits.duration -cne $durationStrings[$ExpectedDuration] -or
            ($reportFinished - $reportStarted) -lt [TimeSpan]::FromSeconds($durationSeconds[$ExpectedDuration])) {
            throw "$Kind report duration '$($data.limits.duration)' does not match stage $ExpectedDuration."
        }
    }
    if ($isPerformance) {
        $script:FastReleaseAssessment = $data.release_assessment
        if ($ProcessExitCode -ne 0) { throw "S18/S19 test process exited with code $ProcessExitCode." }
    }
    return $data
}

function ConvertTo-S21Timestamp($Value) {
    if ($null -eq $Value) { throw 'timestamp is null' }
    if ($Value -is [DateTimeOffset]) { return $Value.ToUniversalTime() }
    if ($Value -is [DateTime]) { return ([DateTimeOffset]$Value).ToUniversalTime() }
    return [DateTimeOffset]::Parse([string]$Value, [Globalization.CultureInfo]::InvariantCulture, [Globalization.DateTimeStyles]::AssumeUniversal).ToUniversalTime()
}

function Assert-S21ReportEvidence(
    [string] $Path,
    [string] $ExpectedRunId,
    [string] $ExpectedCandidateHash,
    [DateTime] $StartedAt
) {
    if (-not (Test-Path -LiteralPath $Path -PathType Leaf)) { throw 'S21 did not write its structured report.' }
    $file = Get-Item -LiteralPath $Path
    if ($StartedAt -ne [DateTime]::MinValue -and $file.LastWriteTimeUtc -lt $StartedAt.AddSeconds(-2)) { throw 'S21 report predates this invocation.' }
    try { $data = Get-Content -LiteralPath $Path -Raw | ConvertFrom-Json -ErrorAction Stop } catch { throw "S21 report is invalid JSON: $($_.Exception.Message)" }
    if ($data.schema_version -isnot [long] -or $data.schema_version -ne 1 -or [string]$data.kind -cne 's21') {
        throw 'S21 report schema or kind is missing or unsupported.'
    }
    if ([string]$data.run_id -cne $ExpectedRunId) { throw 'S21 report run ID does not match the current Fast run.' }
    $candidateHash = [string]$data.candidate_sha256
    if ($candidateHash -cnotmatch '^[0-9a-fA-F]{64}$' -or $candidateHash.ToLowerInvariant() -cne $ExpectedCandidateHash.ToLowerInvariant()) {
        throw 'S21 report candidate SHA-256 is missing, invalid, or does not match the candidate binary.'
    }
    $candidatePath = [string]$data.candidate_binary
    if ([string]::IsNullOrWhiteSpace($candidatePath) -or -not [IO.Path]::IsPathFullyQualified($candidatePath) -or
        -not (Test-Path -LiteralPath $candidatePath -PathType Leaf) -or (Get-FileSha256 $candidatePath) -cne $candidateHash.ToLowerInvariant()) {
        throw 'S21 report candidate path is missing, non-absolute, or its binary hash does not match the report.'
    }
    $environment = $data.environment
    foreach ($environmentKey in @('goos', 'goarch', 'go_version')) {
        $environmentProperty = if ($null -ne $environment) { $environment.PSObject.Properties[$environmentKey] } else { $null }
        if (-not $environmentProperty -or [string]::IsNullOrWhiteSpace([string]$environmentProperty.Value)) {
            throw "S21 report is missing runtime environment field '$environmentKey'."
        }
    }
    $corpusHash = [string]$data.corpus_sha256
    if ($corpusHash -cnotmatch '^[0-9a-fA-F]{64}$' -or $corpusHash.ToLowerInvariant() -cne (Get-CorpusHash)) {
        throw 'S21 report corpus SHA-256 is missing, invalid, or does not match the current corpus.'
    }
    try {
        $reportStartedAt = ConvertTo-S21Timestamp $data.started_at
        $reportFinishedAt = ConvertTo-S21Timestamp $data.finished_at
        $reportGeneratedAt = ConvertTo-S21Timestamp $data.generated_at
    } catch { throw "S21 report start, finish, or generation timestamp is missing or invalid: $($_.Exception.Message)" }
    if ($reportFinishedAt -lt $reportStartedAt -or $reportGeneratedAt -lt $reportFinishedAt) {
        throw 'S21 report timestamps do not satisfy started_at <= finished_at <= generated_at.'
    }
    $errorsProperty = $data.PSObject.Properties['errors']
    $skipsProperty = $data.PSObject.Properties['skips']
    if (-not $errorsProperty -or -not $skipsProperty) { throw 'S21 report is missing its errors or skips metadata arrays.' }
    if ([string]$data.decision -notin @('passed', 'failed', 'not_verified')) { throw "S21 report has unsupported decision '$($data.decision)' ." }
    if ([string]$data.decision -ne 'passed') { return $data }
    if (@($errorsProperty.Value).Count -gt 0 -or @($skipsProperty.Value).Count -gt 0) { throw 'S21 report passed while retaining errors or skipped cases.' }
    $checkId = "$ExpectedRunId/S21/zero-error-classification"
    $checks = @($data.checks | Where-Object { [string]$_.id -ceq $checkId })
    if ($checks.Count -ne 1 -or [string]$checks[0].status -cne 'passed') { throw 'S21 passed without exactly one run-qualified, passed zero-error check.' }
    $observed = $checks[0].observed
    $required = @('go', 'c', 'cpp', 'rust', 'python', 'typescript', 'javascript')
    $declared = @($observed.required_languages | ForEach-Object { [string]$_ })
    $tested = @($observed.tested_languages | ForEach-Object { [string]$_ })
    if (@($required | Where-Object { $_ -cnotin $declared }).Count -gt 0 -or
        @($declared | Select-Object -Unique).Count -ne $required.Count -or
        @($required | Where-Object { $_ -cnotin $tested }).Count -gt 0 -or
        @($tested | Select-Object -Unique).Count -ne $required.Count -or
        -not $observed.PSObject.Properties['skipped_languages'] -or
        @($observed.skipped_languages.PSObject.Properties).Count -ne 0 -or
        -not $observed.PSObject.Properties['failed_languages'] -or
        @($observed.failed_languages.PSObject.Properties).Count -ne 0 -or
        -not $observed.PSObject.Properties['skipped_cases'] -or
        @($observed.skipped_cases).Count -ne 0) {
        throw 'S21 passed without executing each required language exactly once or contains skips.'
    }
    $cases = @($observed.tested_cases)
    if ($cases.Count -lt 6 -or @($cases | Where-Object {
        -not $_.language -or [string]$_.language -cnotin $required -or -not $_.case -or
        [long]$_.operations -ne 4 -or -not $_.PSObject.Properties['operation_counts'] -or
        [long]$_.operation_counts.hover -ne 1 -or [long]$_.operation_counts.definition -ne 1 -or
        [long]$_.operation_counts.references -ne 1 -or [long]$_.operation_counts.rename -ne 1 -or
        [long]$_.operation_counts.total -ne 4
    }).Count -gt 0) {
        throw 'S21 report is missing tested corpus cases or their operation counts.'
    }
    if (@($cases | ForEach-Object { "$($_.language)|$($_.case)" } | Select-Object -Unique).Count -ne $cases.Count) {
        throw 'S21 report repeats a corpus case.'
    }
    foreach ($language in $required) {
        if (@($cases | Where-Object { [string]$_.language -ceq $language }).Count -lt 2) {
            throw "S21 report does not cover both required corpus cases for '$language'."
        }
    }
    $operationTotal = 0L
    foreach ($operation in @('hover', 'definition', 'references', 'rename')) {
        $property = $observed.operation_counts.PSObject.Properties[$operation]
        if (-not $property -or $property.Value -isnot [long] -or $property.Value -le 0) { throw "S21 report has no positive $operation sample count." }
        $operationTotal += [long]$property.Value
    }
    if ($observed.operation_counts.total -isnot [long] -or $observed.operation_counts.total -ne $operationTotal) {
        throw 'S21 report total operation count does not match the feature counts.'
    }
    $caseOperationTotal = 0L
    foreach ($case in $cases) { $caseOperationTotal += [long]$case.operations }
    if ($caseOperationTotal -ne $operationTotal) { throw 'S21 per-case operation counts do not match feature totals.' }
    foreach ($operation in @('hover', 'definition', 'references', 'rename')) {
        if ([long]$observed.operation_counts.$operation -ne $cases.Count) {
            throw "S21 $operation total does not match the per-case operation counts."
        }
    }
    foreach ($bucket in @('wrong_edit', 'stale_edit', 'wrong_file_location', 'position_mapping_error', 'protocol_invalid_response', 'snapshot_mixing')) {
        $property = $observed.error_buckets.PSObject.Properties[$bucket]
        if (-not $property -or $property.Value -isnot [long] -or $property.Value -ne 0) { throw "S21 report bucket '$bucket' is missing or nonzero." }
    }
    return $data
}

function Get-S21BucketStatus([string] $Path, [string] $Bucket, [string] $ExpectedRunId, [string] $ExpectedCandidateHash) {
    if (-not (Test-Path -LiteralPath $Path -PathType Leaf)) { return 'not_verified' }
    try { $data = Assert-S21ReportEvidence $Path $ExpectedRunId $ExpectedCandidateHash ([DateTime]::MinValue) } catch { return 'failed' }
    $checkId = "$ExpectedRunId/S21/zero-error-classification"
    $check = @($data.checks | Where-Object { [string]$_.id -ceq $checkId })
    if ($check.Count -ne 1) { return 'not_verified' }
    $property = $check[0].observed.error_buckets.PSObject.Properties[$Bucket]
    if (-not $property -or $property.Value -isnot [long]) { return 'not_verified' }
    if ($property.Value -gt 0) { return 'failed' }
    if ([string]$data.decision -eq 'passed' -and [string]$check[0].status -eq 'passed') { return 'passed' }
    return 'not_verified'
}

function Get-ToolVersions {
    $versions = [ordered]@{}
    $lock = $script:AcceptanceToolLock
    if ($null -eq $lock) {
        $lock = Get-Content (Join-Path $repoRoot 'test/acceptance/tools/tools.lock.json') -Raw | ConvertFrom-Json
    }
    $commands = @(
        @{ key = 'go'; args = @('version') },
        @{ key = 'gopls'; args = @('version') },
        @{ key = 'vscode'; args = @('--version') },
        @{ key = 'clangd'; args = @('--version') },
        @{ key = 'clang'; args = @('--version') },
        @{ key = 'clang++'; args = @('--version') },
        @{ key = 'rust-analyzer'; args = @('--version') },
        @{ key = 'rustc'; args = @('--version') },
        @{ key = 'python'; args = @('--version') },
        @{ key = 'node'; args = @('--version') },
        @{ key = 'npm'; args = @('--version') },
        @{ key = 'cargo'; args = @('--version') }
    )
    foreach ($item in $commands) {
        $resolvedPath = Get-ResolvedBinaryPath $lock.resolvedBinaries $item.key
        if ($resolvedPath -and (Test-Path -LiteralPath $resolvedPath -PathType Leaf)) {
            if ($item.key -ceq 'vscode') {
                $installRoot = [IO.Path]::GetDirectoryName($resolvedPath)
                $packageCandidates = @(
                    (Join-Path $installRoot 'resources/app/package.json')
                )
                $packageCandidates += @(
                    Get-ChildItem -LiteralPath $installRoot -Directory -ErrorAction SilentlyContinue |
                        ForEach-Object { Join-Path $_.FullName 'resources/app/package.json' }
                )
                $packagePaths = @($packageCandidates | Where-Object { Test-Path -LiteralPath $_ -PathType Leaf })
                if ($packagePaths.Count -eq 1) {
                    $versions[$item.key] = ([string](Get-Content -LiteralPath $packagePaths[0] -Raw | ConvertFrom-Json).version).Trim()
                } else {
                    $versions[$item.key] = 'MISSING'
                }
                continue
            }
            $extension = [IO.Path]::GetExtension($resolvedPath).ToLowerInvariant()
            if ($extension -in @('.js', '.mjs')) {
                $nodePath = Get-ResolvedBinaryPath $lock.resolvedBinaries 'node'
                $output = & $nodePath $resolvedPath @($item.args) 2>&1 | Select-Object -First 3
            } else {
                $output = & $resolvedPath @($item.args) 2>&1 | Select-Object -First 3
            }
            $lines = @($output | ForEach-Object { $_.ToString().Trim() } | Where-Object { $_ -and $_ -notmatch '(?i)error acquiring upload token|upload\.token|Access is denied' })
            $versions[$item.key] = ($lines -join "`n").Trim()
        } else {
            $versions[$item.key] = 'MISSING'
        }
    }
    $localTools = Join-Path $repoRoot 'test/acceptance/tools'
    foreach ($item in @(
        @{ key = 'pyright'; path = 'node_modules/pyright/package.json' },
        @{ key = 'typescript-language-server'; path = 'node_modules/typescript-language-server/package.json' },
        @{ key = 'typescript'; path = 'node_modules/typescript/package.json' }
    )) {
        $path = Join-Path $localTools $item.path
        if (Test-Path $path) { $versions[$item.key] = (Get-Content -LiteralPath $path -Raw | ConvertFrom-Json).version } else { $versions[$item.key] = 'MISSING' }
    }
    $vscodeLanguageClient = Join-Path $repoRoot 'editors/vscode/node_modules/vscode-languageclient/package.json'
    if (Test-Path -LiteralPath $vscodeLanguageClient) { $versions['vscode-languageclient'] = (Get-Content -LiteralPath $vscodeLanguageClient -Raw | ConvertFrom-Json).version } else { $versions['vscode-languageclient'] = 'MISSING' }
    $nvimExe = Get-ResolvedBinaryPath $lock.resolvedBinaries 'neovim'
    if ($nvimExe -and (Test-Path -LiteralPath $nvimExe -PathType Leaf)) {
        $versions['neovim'] = (& $nvimExe --version | Select-Object -First 1).Trim()
    } else {
        $versions['neovim'] = 'MISSING'
    }
	$emacsExe = Get-ResolvedBinaryPath $lock.resolvedBinaries 'emacs'
	if ($emacsExe -and (Test-Path -LiteralPath $emacsExe -PathType Leaf)) {
		$versions['emacs'] = (& $emacsExe --version | Select-Object -First 1).Trim()
	} else {
		$versions['emacs'] = 'MISSING'
	}
    return $versions
}

function Test-ResolvedBinaryLock($ResolvedBinaries, [string[]] $RequiredNames = @()) {
    $mismatches = [Collections.Generic.List[string]]::new()
    $missing = [Collections.Generic.List[string]]::new()
    if ($null -eq $ResolvedBinaries) {
        return [ordered]@{ status = 'failed'; detail = 'tools.lock.json has no resolvedBinaries map' }
    }
    $properties = @($ResolvedBinaries.PSObject.Properties)
    if ($properties.Count -eq 0) {
        return [ordered]@{ status = 'failed'; detail = 'tools.lock.json resolvedBinaries map is empty' }
    }
    foreach ($requiredName in @($RequiredNames)) {
        $requiredProperty = $ResolvedBinaries.PSObject.Properties[[string]$requiredName]
        if (-not $requiredProperty) {
            $mismatches.Add("resolvedBinaries is missing required entry '$requiredName'")
        }
    }
    foreach ($property in $properties) {
        $name = [string]$property.Name
        $entry = $property.Value
        $path = [string]$entry.path
        $expected = [string]$entry.sha256
        if ([string]::IsNullOrWhiteSpace($path) -or -not [IO.Path]::IsPathRooted($path) -or $expected -cnotmatch '^[0-9a-fA-F]{64}$') {
            $mismatches.Add("$name has an invalid absolute path or SHA-256 pin")
            continue
        }
        if (-not (Test-Path -LiteralPath $path -PathType Leaf)) {
            $missing.Add("$name expected '$path', observed MISSING")
            continue
        }
        $observed = Get-FileSha256 $path
        if ($observed -cne $expected.ToLowerInvariant()) {
            $mismatches.Add("$name expected SHA-256 $($expected.ToLowerInvariant()), observed $observed at '$path'")
        }
    }
    if ($mismatches.Count -gt 0) {
        $details = @($mismatches) + @($missing)
        return [ordered]@{ status = 'failed'; detail = ($details -join '; ') }
    }
    if ($missing.Count -gt 0) {
        return [ordered]@{ status = 'not_verified'; detail = ($missing -join '; ') }
    }
    return [ordered]@{ status = 'passed'; detail = "$($properties.Count) resolved tool paths match their locked SHA-256 identities" }
}

function Get-ResolvedBinaryEntry($ResolvedBinaries, [string] $Name) {
    if ($null -eq $ResolvedBinaries -or [string]::IsNullOrWhiteSpace($Name)) { return $null }
    $property = $ResolvedBinaries.PSObject.Properties[$Name]
    if (-not $property) { return $null }
    return $property.Value
}

function Get-ResolvedBinaryPath($ResolvedBinaries, [string] $Name) {
    $entry = Get-ResolvedBinaryEntry $ResolvedBinaries $Name
    if ($null -eq $entry) { return $null }
    $path = [string]$entry.path
    if ([string]::IsNullOrWhiteSpace($path) -or -not [IO.Path]::IsPathRooted($path)) { return $null }
    return [IO.Path]::GetFullPath($path)
}

function Test-ResolvedBinaryBinding($ResolvedBinaries, [string] $Name, [string] $ActualPath) {
    $entry = Get-ResolvedBinaryEntry $ResolvedBinaries $Name
    $lockedPath = Get-ResolvedBinaryPath $ResolvedBinaries $Name
    if ($null -eq $entry -or $null -eq $lockedPath) {
        return [ordered]@{ status = 'failed'; detail = "resolvedBinaries is missing a valid path for '$Name'" }
    }
    if ([string]::IsNullOrWhiteSpace($ActualPath) -or -not [IO.Path]::IsPathRooted($ActualPath)) {
        return [ordered]@{ status = 'failed'; detail = "$Name actual executable path is not absolute: '$ActualPath'" }
    }
    $actualFullPath = [IO.Path]::GetFullPath($ActualPath)
    if (-not [string]::Equals($actualFullPath, $lockedPath, [StringComparison]::OrdinalIgnoreCase)) {
        return [ordered]@{ status = 'failed'; detail = "$Name resolved path '$actualFullPath' does not match locked path '$lockedPath'" }
    }
    if (-not (Test-Path -LiteralPath $actualFullPath -PathType Leaf)) {
        return [ordered]@{ status = 'not_verified'; detail = "$Name locked executable is missing at '$lockedPath'" }
    }
    $expected = ([string]$entry.sha256).ToLowerInvariant()
    $observed = Get-FileSha256 $actualFullPath
    if ($observed -cne $expected) {
        return [ordered]@{ status = 'failed'; detail = "$Name resolved path '$actualFullPath' has SHA-256 $observed; lock requires $expected" }
    }
    return [ordered]@{ status = 'passed'; path = $actualFullPath; sha256 = $observed; detail = "$Name resolved path and SHA-256 match tools.lock.json" }
}

function Get-RequiredResolvedBinaryNames {
    return @(
        'go', 'gopls', 'vscode', 'clang', 'clang++', 'clangd',
        'rust-analyzer', 'rust-analyzer-semantic-helper', 'rustc', 'cargo', 'rust-analyzer-proc-macro-srv',
        'python', 'node', 'npm', 'neovim', 'helix', 'emacs',
        'pyright-langserver', 'pyright-internal', 'pyright-vendor',
        'typescript-language-server', 'typescript'
    )
}

function Initialize-LockedAcceptanceEnvironment($Lock) {
    if ($null -eq $Lock -or $null -eq $Lock.resolvedBinaries) {
        return [ordered]@{ id = 'resolved-binary-binding'; status = 'failed'; detail = 'tools.lock.json has no resolvedBinaries map' }
    }
    $requiredNames = @(Get-RequiredResolvedBinaryNames)
    $lockCheck = Test-ResolvedBinaryLock $Lock.resolvedBinaries $requiredNames
    if ($lockCheck.status -cne 'passed') {
        return [ordered]@{ id = 'resolved-binary-binding'; status = [string]$lockCheck.status; detail = [string]$lockCheck.detail }
    }
    $paths = [ordered]@{}
    foreach ($name in $requiredNames) {
        $path = Get-ResolvedBinaryPath $Lock.resolvedBinaries $name
        if ($null -eq $path) {
            return [ordered]@{ id = 'resolved-binary-binding'; status = 'failed'; detail = "resolvedBinaries entry '$name' has no usable absolute path" }
        }
        $paths[$name] = $path
    }
    $script:AcceptanceToolLock = $Lock
    $script:AcceptanceResolvedPaths = $paths
    $script:go = $paths.go
    $lockedDirectories = [Collections.Generic.List[string]]::new()
    foreach ($path in @($paths.Values)) {
        $directory = [IO.Path]::GetDirectoryName([string]$path)
        if ($directory -and $lockedDirectories -notcontains $directory) { $lockedDirectories.Add($directory) }
    }
    $pathParts = @($localToolBin) + @($lockedDirectories) + @($script:AcceptanceOriginalPath -split [IO.Path]::PathSeparator | Where-Object { $_ })
    $env:PATH = ($pathParts -join [IO.Path]::PathSeparator)
    foreach ($commandName in @('go', 'gopls', 'clang', 'clang++', 'clangd', 'rust-analyzer', 'rustc', 'cargo', 'python', 'node')) {
        $resolvedCommand = Get-Command $commandName -ErrorAction SilentlyContinue
        $resolvedPath = if ($resolvedCommand) { [string]$resolvedCommand.Source } else { '' }
        $binding = Test-ResolvedBinaryBinding $Lock.resolvedBinaries $commandName $resolvedPath
        if ($binding.status -cne 'passed') {
            return [ordered]@{ id = 'resolved-binary-binding'; status = [string]$binding.status; detail = [string]$binding.detail }
        }
    }
    $env:OMNILSP_ACCEPTANCE_NODE = $paths.node
    $env:OMNILSP_ACCEPTANCE_NODE_SHA256 = ([string](Get-ResolvedBinaryEntry $Lock.resolvedBinaries 'node').sha256).ToLowerInvariant()
    $env:OMNILSP_ACCEPTANCE_LOCKED_TOOLS = '1'
    $env:OMNILSP_SEMANTIC_NODE_PATH = $paths.node
    $env:OMNILSP_SEMANTIC_TYPESCRIPT_PATH = $paths.typescript
    $env:OMNILSP_SEMANTIC_PYRIGHT_INTERNAL_PATH = $paths['pyright-internal']
    $env:OMNILSP_SEMANTIC_PYRIGHT_VENDOR_PATH = $paths['pyright-vendor']
    $env:OMNILSP_SEMANTIC_PYTHON_PATH = $paths.python
    $env:OMNILSP_RUST_SCIP_HELPER_PATH = $paths['rust-analyzer-semantic-helper']
    $env:OMNILSP_RUST_SCIP_HELPER_SHA256 = ([string](Get-ResolvedBinaryEntry $Lock.resolvedBinaries 'rust-analyzer-semantic-helper').sha256).ToLowerInvariant()
    $env:OMNILSP_RUST_SCIP_HELPER_VERSION = 'rust-analyzer ' + [string]$Lock.observed.rustAnalyzerSemanticHelper
    return [ordered]@{ id = 'resolved-binary-binding'; status = 'passed'; detail = "$($requiredNames.Count) acceptance tool paths and SHA-256 identities are locked for execution" }
}

function Test-VersionLock {
    $lockPath = Join-Path $repoRoot 'test/acceptance/tools/tools.lock.json'
    $lock = Get-Content -LiteralPath $lockPath -Raw | ConvertFrom-Json
    $actual = Get-ToolVersions
    $expected = [ordered]@{
        go = $lock.observed.go
        gopls = $lock.observed.gopls
        vscode = $lock.observed.vscode
        clangd = $lock.observed.clangd
        clang = $lock.observed.clang
        'clang++' = $lock.observed.'clang++'
        'rust-analyzer' = $lock.observed.rustAnalyzer
        rustc = $lock.observed.rustc
        python = $lock.observed.python
        node = $lock.observed.node
        npm = $lock.observed.npm
        cargo = $lock.observed.cargo
        pyright = $lock.acceptanceTools.pyright.version
        'typescript-language-server' = $lock.acceptanceTools.typescriptLanguageServer.version
        typescript = $lock.acceptanceTools.typescript.version
        'vscode-languageclient' = $lock.acceptanceTools.vscodeLanguageClient.version
        neovim = "NVIM v$($lock.acceptanceTools.neovim.version)"
		emacs = "GNU Emacs $($lock.acceptanceTools.emacs.version)"
    }
    $missing = [Collections.Generic.List[string]]::new()
    $mismatches = [Collections.Generic.List[string]]::new()
    foreach ($key in $expected.Keys) {
        $value = [string]$actual[$key]
        $wanted = [string]$expected[$key]
        if ($value -eq 'MISSING') {
            $missing.Add("$key expected '$wanted', observed MISSING")
            continue
        }
        $match = $false
        switch ($key) {
            'go' { if ($value -cmatch '(?m)^go version (?<version>\S+)') { $match = $Matches.version -ceq $wanted } }
            'gopls' { if ($value -cmatch '(?m)^(?<version>golang.org/x/tools/gopls v\S+)') { $match = $Matches.version -ceq $wanted } }
            'vscode' { $match = (($value -split "`r?`n")[0].Trim() -ceq $wanted) }
            'clangd' { if ($value -cmatch '(?m)^clangd version (?<version>\S+)') { $match = $Matches.version -ceq $wanted } }
            'clang' { if ($value -cmatch '(?m)^clang version (?<version>\S+)') { $match = $Matches.version -ceq $wanted } }
            'clang++' { if ($value -cmatch '(?m)^clang version (?<version>\S+)') { $match = $Matches.version -ceq $wanted } }
            'rust-analyzer' { if ($value -cmatch '(?m)^rust-analyzer (?<version>.+)$') { $match = $Matches.version -ceq $wanted } }
            'rustc' { if ($value -cmatch '(?m)^rustc (?<version>.+)$') { $match = $Matches.version -ceq $wanted } }
            'python' { if ($value -cmatch '(?m)^Python (?<version>\S+)') { $match = $Matches.version -ceq $wanted } }
            'node' { $match = (($value -split "`r?`n")[0].Trim() -ceq $wanted) }
            'npm' { $match = (($value -split "`r?`n")[0].Trim() -ceq $wanted) }
            'cargo' { if ($value -cmatch '(?m)^cargo (?<version>.+)$') { $match = $Matches.version -ceq $wanted } }
            'pyright' { $match = ($value.Trim() -ceq $wanted) }
            'typescript-language-server' { $match = ($value.Trim() -ceq $wanted) }
            'typescript' { $match = ($value.Trim() -ceq $wanted) }
            'vscode-languageclient' { $match = ($value.Trim() -ceq $wanted) }
            'neovim' { $match = (($value -split "`r?`n")[0].Trim() -ceq $wanted) }
			'emacs' { $match = (($value -split "`r?`n")[0].Trim() -ceq $wanted) }
        }
        if (-not $match) {
            $mismatches.Add("$key expected '$($expected[$key])', observed '$($actual[$key])'")
        }
    }
    $binaryLock = Test-ResolvedBinaryLock $lock.resolvedBinaries @(Get-RequiredResolvedBinaryNames)
    if ($binaryLock.status -ceq 'failed') {
        $mismatches.Add([string]$binaryLock.detail)
    } elseif ($binaryLock.status -ceq 'not_verified') {
        $missing.Add([string]$binaryLock.detail)
    }
    $helixRuntimePath = Join-Path $repoRoot 'test/acceptance/tools/bin/helix-25.07.1/runtime'
    $helixRuntimeHash = Get-DirectoryContentHash $helixRuntimePath
    if ($helixRuntimeHash -ceq 'MISSING') {
        $missing.Add('Helix runtime is missing')
    } elseif ($helixRuntimeHash -cne [string]$lock.acceptanceTools.helix.runtimeTreeSha256) {
        $mismatches.Add('Helix runtime tree differs from tools.lock.json')
    }
    if ($mismatches.Count -eq 0) {
        if ($missing.Count -gt 0) { return [ordered]@{ id = 'tool-version-lock'; status = 'not_verified'; detail = ($missing -join '; ') } }
        return [ordered]@{ id = 'tool-version-lock'; status = 'passed'; detail = 'all required tool versions and resolved binary SHA-256 identities match tools.lock.json' }
    }
    $details = @($mismatches) + @($missing)
    return [ordered]@{ id = 'tool-version-lock'; status = 'failed'; detail = ($details -join '; ') }
}

function Invoke-Step([string] $Name, [string] $Executable, [string[]] $Arguments) {
    $log = Join-Path $EvidenceDirectory "$Name.log"
    Write-Host "[$Name] $Executable $($Arguments -join ' ')"
    Push-Location $repoRoot
    try {
        & $Executable @Arguments 2>&1 | Tee-Object -FilePath $log | Out-Host
        $exitCode = $LASTEXITCODE
    } finally {
        Pop-Location
    }
    return [ordered]@{ id = $Name; status = $(if ($exitCode -eq 0) { 'passed' } else { 'failed' }); exitCode = $exitCode; log = [IO.Path]::GetFileName($log) }
}

function Write-Summary($Decision, $Checks, $Errors = @()) {
    $candidateHash = $null
    if (Test-Path $CandidateBinary) { $candidateHash = (Get-FileHash -Algorithm SHA256 -LiteralPath $CandidateBinary).Hash.ToLowerInvariant() }
    $versions = Get-ToolVersions
    $skips = @($versions.GetEnumerator() | Where-Object { $_.Value -eq 'MISSING' } | ForEach-Object { [ordered]@{ id = $_.Key; reason = 'required local acceptance tool is not installed or not discoverable' } })
    $summary = [ordered]@{
        schema_version = 1
        run_id = $RunId
        phase = $Phase
        started_at = $script:StartedAt.ToString('o')
        finished_at = (Get-Date).ToUniversalTime().ToString('o')
        decision = $Decision
        s18_policy = $S18Policy
        release_assessment = $script:FastReleaseAssessment
        candidate = [ordered]@{ binary = $CandidateBinary; sha256 = $candidateHash }
        source_identity = [ordered]@{
            workspace_sha256_at_finish = Get-WorkspaceContentHash
            workspace_sha256_before_tests = if ($script:FastSourceTreeHash) { $script:FastSourceTreeHash } else { 'not captured' }
            observed_executables = Get-AcceptanceToolExecutables
        }
        corpus = [ordered]@{ root = 'test/corpus/testdata'; sha256 = Get-CorpusHash }
        environment = $versions
        hardware = [ordered]@{ cpu_model = if ($env:OMNILSP_CPU_MODEL) { $env:OMNILSP_CPU_MODEL } else { 'not observed' } }
        limits = [ordered]@{ max_parallel_tasks = 8; process_tree_private_memory_bytes = 8589934592 }
        checks = @($Checks)
        errors = @($Errors)
        skips = $skips
    }
    $path = Join-Path $EvidenceDirectory "$Phase-summary.json"
    $summary | ConvertTo-Json -Depth 8 | Set-Content -LiteralPath $path -Encoding utf8
    return $path
}

function Get-OverallDecision($Checks) {
    if (@($Checks | Where-Object { $_.status -eq 'failed' }).Count -gt 0) { return 'failed' }
    if (@($Checks | Where-Object { $_.status -ne 'passed' }).Count -gt 0) { return 'not_verified' }
    if ([string]$script:FastReleaseDecision -ceq 'passed_with_performance_exception') { return 'passed_with_performance_exception' }
    return 'passed'
}

function Get-EvidenceCheckStatus([string] $Path, [string[]] $CheckIds) {
    if (-not (Test-Path -LiteralPath $Path)) { return 'not_verified' }
    try { $evidence = Get-Content -LiteralPath $Path -Raw | ConvertFrom-Json -ErrorAction Stop } catch { return 'not_verified' }
    $states = [Collections.Generic.List[string]]::new()
    foreach ($checkId in $CheckIds) {
        $matches = @($evidence.checks | Where-Object { [string]$_.id -ceq $checkId })
        if ($matches.Count -ne 1) { $states.Add('not_verified'); continue }
        $status = [string]$matches[0].status
        if ($status -ceq 'failed') { $states.Add('failed') }
        elseif ($status -ceq 'passed') { $states.Add('passed') }
        else { $states.Add('not_verified') }
    }
    if ($states.Contains('failed')) { return 'failed' }
    if ($states.Count -eq 0 -or $states.Contains('not_verified')) { return 'not_verified' }
    return 'passed'
}

function Get-MatrixEvidenceCheckStatus([string] $Path, [string[]] $CheckIds) {
    if ($script:FastEvidenceGatePresent -and -not $script:FastEvidenceIdentityValid) { return 'failed' }
    return Get-EvidenceCheckStatus $Path $CheckIds
}

function Get-SummaryCheckStatus([string] $Path, [string[]] $CheckIds) {
    if (-not (Test-Path -LiteralPath $Path)) { return 'not_verified' }
    try { $summary = Get-Content -LiteralPath $Path -Raw | ConvertFrom-Json -ErrorAction Stop } catch { return 'not_verified' }
    $states = [Collections.Generic.List[string]]::new()
    foreach ($checkId in $CheckIds) {
        $matches = @($summary.checks | Where-Object { [string]$_.id -ceq $checkId })
        if ($matches.Count -ne 1) { $states.Add('not_verified'); continue }
        $status = [string]$matches[0].status
        if ($status -ceq 'failed') { $states.Add('failed') }
        elseif ($status -ceq 'passed') { $states.Add('passed') }
        else { $states.Add('not_verified') }
    }
    if ($states.Contains('failed')) { return 'failed' }
    if ($states.Count -eq 0 -or $states.Contains('not_verified')) { return 'not_verified' }
    return 'passed'
}

function Get-RequirementMatrixDecision($Rows) {
    $selectedRows = @($Rows | Where-Object { $_.selected })
    $unacceptedFailures = @($selectedRows | Where-Object { $_.status -eq 'failed' -and -not [bool]$_.performance_exception_accepted })
    if ($unacceptedFailures.Count -gt 0) { return 'failed' }
    $unverified = @($selectedRows | Where-Object {
        $_.status -notin @('passed', 'passed_with_performance_exception') -and
        -not ($_.status -eq 'failed' -and [bool]$_.performance_exception_accepted)
    })
    if ($unverified.Count -gt 0) { return 'not_verified' }
    if (@($selectedRows | Where-Object { $_.status -eq 'passed_with_performance_exception' -or [bool]$_.performance_exception_accepted }).Count -gt 0) {
        return 'passed_with_performance_exception'
    }
    return 'passed'
}

function Test-S18MatrixExceptionAccepted([string] $CheckId, $Assessment, [string[]] $ValidatedBoundedExceptionIds, [int] $PerformanceProcessExitCode) {
    if ($PerformanceProcessExitCode -ne 0 -or $null -eq $Assessment -or
        [string]$Assessment.policy_id -cne 's18-evidence-qualified-v2' -or
        [string]$Assessment.execution_status -cne 'completed' -or
        [string]$Assessment.decision -cne 'passed_with_performance_exception') {
        return $false
    }
    $exceptions = @(Get-OptionalAssessmentCheckIDs $Assessment 'exception_check_ids')
    $bounded = @(Get-OptionalAssessmentCheckIDs $Assessment 'bounded_exception_check_ids')
    $validated = @($ValidatedBoundedExceptionIds | Sort-Object -Unique)
    if ($exceptions.Count -ne @($exceptions | Sort-Object -Unique).Count -or
        $bounded.Count -ne @($bounded | Sort-Object -Unique).Count -or
        [string]::Join("`n", @($exceptions | Sort-Object -Unique)) -cne [string]::Join("`n", @($bounded | Sort-Object -Unique)) -or
        [string]::Join("`n", @($bounded | Sort-Object -Unique)) -cne [string]::Join("`n", $validated)) {
        return $false
    }
    return $CheckId -cin $exceptions -and $CheckId -cin $validated
}

function Write-RequirementMatrix {
    $script:AcceptanceMatrixRows = [Collections.Generic.List[object]]::new()
    $fastSummary = Join-Path $EvidenceDirectory 'Fast-summary.json'
    $perfReport = Join-Path $EvidenceDirectory 'performance.json'
    $semanticReport = Join-Path $EvidenceDirectory 'semantic.json'
    $clientReport = Join-Path $EvidenceDirectory 'clients.json'
    $s21Report = Join-Path $EvidenceDirectory 's21.json'
    $matrixReleaseAssessment = $script:FastReleaseAssessment
    $matrixBoundedS18ExceptionIds = @($script:FastBoundedS18ExceptionIds)
    $matrixPerformanceExitCode = -1
    if (Test-Path -LiteralPath $fastSummary) {
        try {
            $summaryForMatrix = Get-Content -LiteralPath $fastSummary -Raw | ConvertFrom-Json -ErrorAction Stop
            $performanceStepsForMatrix = @($summaryForMatrix.checks | Where-Object { [string]$_.id -ceq 'S18-S19-real-process-performance' })
            if ($performanceStepsForMatrix.Count -eq 1 -and $null -ne $performanceStepsForMatrix[0].exitCode) {
                $matrixPerformanceExitCode = [int]$performanceStepsForMatrix[0].exitCode
            }
        } catch { $matrixPerformanceExitCode = -1 }
    }
    $matrixCandidateHash = if (Test-Path -LiteralPath $CandidateBinary) { Get-FileSha256 $CandidateBinary } else { $null }
    $candidateFingerprint = Get-AcceptanceFingerprint
    $gatesPath = Join-Path $EvidenceDirectory 'fast-gates.json'
    $fingerprint = $null
    $script:FastEvidenceGatePresent = Test-Path -LiteralPath $gatesPath
    $script:FastEvidenceIdentityValid = $false
    if (Test-Path -LiteralPath $gatesPath) {
        try {
            $matrixGates = Get-Content -LiteralPath $gatesPath -Raw | ConvertFrom-Json -ErrorAction Stop
            $fingerprint = $matrixGates.acceptance_fingerprint.fingerprint_sha256
            if ([string]$matrixGates.status -cnotin @('passed', 'passed_with_performance_exception') -or [string]$matrixGates.run_id -cne $RunId -or
                [string]$matrixGates.candidate_sha256 -cne [string]$matrixCandidateHash) {
                throw 'Fast gate status, run ID, or candidate identity does not match this requirement matrix.'
            }
            Assert-FastEvidenceHashes $matrixGates 'Requirement matrix'
            Assert-GateReleaseAssessment $matrixGates 'Requirement matrix'
            $matrixReleaseAssessment = $matrixGates.release_assessment
            $matrixBoundedS18ExceptionIds = @(Get-OptionalAssessmentCheckIDs $matrixReleaseAssessment 'bounded_exception_check_ids')
            if (-not (Test-AcceptanceFingerprintIdentity $fingerprint $candidateFingerprint.fingerprint_sha256)) {
                throw 'Frozen Fast acceptance fingerprint does not match the current source, policy, tools, and configuration.'
            }
            $script:FastEvidenceIdentityValid = $true
        } catch { $fingerprint = $null }
    }
    $addRow = {
        param([string]$Id, [string]$Goal, [string]$Requirement, [string]$Implementation, [string]$Evidence, [string]$Status, [bool]$Selected = $true, [string]$Disposition = '', [bool]$PerformanceExceptionAccepted = $false)
        if ($Status -notin @('passed', 'passed_with_performance_exception', 'failed', 'not_verified')) { $Status = 'not_verified' }
        $script:AcceptanceMatrixRows.Add([ordered]@{
            id = $Id; goal = $Goal; requirement = $Requirement; implementation = $Implementation
            evidence = $Evidence; status = $Status; selected = $Selected; disposition = $Disposition
            performance_exception_accepted = $PerformanceExceptionAccepted
        })
    }

    foreach ($gate in @(
        @{ id = 'acceptance-report-contract'; goal = 'evidence validation'; impl = 'PowerShell acceptance report validator contract tests' },
        @{ id = 'tool-version-lock'; goal = 'environment'; impl = 'Locked Go, language servers, clients, Node, and editor versions' },
        @{ id = 'local-language-server-wrappers'; goal = 'environment'; impl = 'Project-local fixed-version Pyright and TypeScript server launchers' },
        @{ id = 'source-stability-before-build'; goal = 'candidate identity'; impl = 'Workspace content hash is unchanged before candidate build' },
        @{ id = 'source-stability-after-tests'; goal = 'candidate identity'; impl = 'Workspace content hash is unchanged through all Fast checks' },
        @{ id = 'unit'; goal = 'Fast gate'; impl = 'Go packages and acceptance support code' },
        @{ id = 'race'; goal = 'Fast gate'; impl = 'Go race detector across ./...' },
        @{ id = 'vet'; goal = 'Fast gate'; impl = 'Go vet across ./...' },
        @{ id = 'build-candidate'; goal = 'candidate'; impl = 'Windows/amd64 candidate binary' },
        @{ id = 'build-soak-test'; goal = 'soak harness'; impl = 'Frozen Windows soak test executable' },
        @{ id = 's21-zero-error-classification'; goal = 'S21'; impl = 'Run-qualified structured zero-error corpus report with no skipped languages' },
        @{ id = 'persistent-go'; goal = 'persistent semantic queries'; impl = 'Candidate restart with Go backend disabled; symbols, definition and references from fixed generation' },
        @{ id = 'persistent-typescript'; goal = 'persistent semantic queries'; impl = 'Candidate restart with TypeScript backend disabled; symbols, definition and references from fixed generation' },
        @{ id = 'persistent-python'; goal = 'persistent semantic queries'; impl = 'Candidate restart with Python backend disabled; symbols, definition and references from fixed generation' },
        @{ id = 'persistent-javascript'; goal = 'persistent semantic queries'; impl = 'Qualified static checkJs project; restart with TypeScript backend disabled and read fixed generation' },
        @{ id = 'persistent-c'; goal = 'persistent semantic queries'; impl = 'Pinned C compilation database; restart with C/C++ backend disabled and read fixed generation' },
        @{ id = 'persistent-cpp'; goal = 'persistent semantic queries'; impl = 'Pinned C++ compilation database; restart with C/C++ backend disabled and read fixed generation' },
        @{ id = 'scip-replay-go'; goal = 'SCIP/replay'; impl = 'Candidate CLI roundtrip, explicit loss counts, semantic replay and identity mismatch rejection' },
        @{ id = 'fuzz-jsonrpc'; goal = 'protocol'; impl = 'JSON-RPC fuzz target' },
        @{ id = 'fuzz-position'; goal = 'position'; impl = 'LSP position fuzz target' },
        @{ id = 'fuzz-position-conversion'; goal = 'position'; impl = 'Position conversion fuzz target' },
        @{ id = 'fuzz-position-edge-cases'; goal = 'position'; impl = 'Position edge-case fuzz target' },
        @{ id = 'fuzz-multibyte-boundary'; goal = 'position'; impl = 'Multibyte boundary fuzz target' },
        @{ id = 'fuzz-did-change'; goal = 'document sync'; impl = 'didChange fuzz target' },
        @{ id = 'fuzz-config-load'; goal = 'configuration'; impl = 'Config-load fuzz target' },
        @{ id = 'fuzz-config-validate'; goal = 'configuration'; impl = 'Config-validation fuzz target' },
        @{ id = 'fuzz-uri'; goal = 'URI'; impl = 'Go backend URI fuzz target' },
        @{ id = 'fuzz-open-snapshot-corrupt-store'; goal = 'persistent index'; impl = 'Corrupt-store recovery fuzz target' },
        @{ id = 'fuzz-freshness-seal-verify'; goal = 'freshness'; impl = 'Freshness-seal fuzz target' },
        @{ id = 'fuzz-uri-parse'; goal = 'URI'; impl = 'URI parser fuzz target' },
        @{ id = 'cross-build-linux-amd64'; goal = 'platform'; impl = 'Linux/amd64 build' },
        @{ id = 'cross-build-linux-arm64'; goal = 'platform'; impl = 'Linux/arm64 build' },
        @{ id = 'cross-build-darwin-amd64'; goal = 'platform'; impl = 'Darwin/amd64 build' },
        @{ id = 'cross-build-darwin-arm64'; goal = 'platform'; impl = 'Darwin/arm64 build' }
    )) {
        $status = Get-SummaryCheckStatus $fastSummary @($gate.id)
        & $addRow "FAST/$($gate.id)" $gate.goal $gate.impl 'scripts/acceptance.ps1 -Phase Fast' "$([IO.Path]::GetFileName($fastSummary))#$($gate.id)" $status
    }
    $overallFast = 'not_verified'
    if (Test-Path -LiteralPath $fastSummary) {
        try { $overallFast = [string](Get-Content -LiteralPath $fastSummary -Raw | ConvertFrom-Json).decision } catch { $overallFast = 'not_verified' }
    }
    if ($overallFast -cin @('passed', 'passed_with_performance_exception') -and -not $script:FastEvidenceIdentityValid) {
        $overallFast = if ($script:FastEvidenceGatePresent) { 'failed' } else { 'not_verified' }
    }
    & $addRow 'FAST/overall' 'Fast gate' 'All unit, race, vet, build, fuzz, S21, semantic, performance, and client checks pass' 'scripts/acceptance.ps1 -Phase Fast' "$([IO.Path]::GetFileName($fastSummary))#decision" $overallFast

    foreach ($language in @('go', 'c', 'cpp', 'rust', 'python', 'typescript', 'javascript')) {
        foreach ($operation in @('hot_hover', 'hot_definition', 'completion_first_usable', 'syntax_update_after_edit')) {
            $checkId = "$RunId/S18/$language/$operation"
            $status = Get-MatrixEvidenceCheckStatus $perfReport @($checkId)
            $disposition = ''
            $performanceExceptionAccepted = $false
            if ($status -eq 'failed') {
                try {
                    if (Test-S18MatrixExceptionAccepted $checkId $matrixReleaseAssessment $matrixBoundedS18ExceptionIds $matrixPerformanceExitCode) {
                        $perfDataForMatrix = Get-Content -LiteralPath $perfReport -Raw | ConvertFrom-Json -ErrorAction Stop
                        $perfCheck = @($perfDataForMatrix.checks | Where-Object { [string]$_.id -ceq $checkId })
                        if ($perfCheck.Count -ne 1) { throw "exception report check '$checkId' is missing" }
                        $observedMs = @(([double]$perfCheck[0].observed.p50_ns / 1000000), ([double]$perfCheck[0].observed.p95_ns / 1000000), ([double]$perfCheck[0].observed.p99_ns / 1000000))
                        $observedText = [string]::Join('/', @($observedMs | ForEach-Object { $_.ToString('0.###', [Globalization.CultureInfo]::InvariantCulture) }))
                        $exceptionLimit = Get-S18StableExceptionLimit $language $operation
                        if ($null -eq $exceptionLimit) { throw "exception report check '$checkId' is outside the stable policy scope" }
                        $limitText = [string]::Join('/', @($exceptionLimit))
                        $disposition = "strict S18 failed; accepted under $([string]$matrixReleaseAssessment.policy_id) at P50/P95/P99=${observedText} ms within temporary ceiling ${limitText} ms after raw-sample revalidation"
                        $performanceExceptionAccepted = $true
                    }
                } catch { $disposition = '' }
            }
            if (-not $performanceExceptionAccepted -and $status -eq 'failed' -and $checkId -cin @($script:FastBoundedS18ExceptionIds)) {
                if ($matrixPerformanceExitCode -ne 0) {
                    $disposition = "strict S18 failed; raw evidence is within the temporary ceiling, but the S18/S19 test process exited $matrixPerformanceExitCode"
                } else {
                    $disposition = 'strict S18 failed; raw candidate percentiles are bounded, but the complete stable release assessment did not accept this row'
                }
            } elseif (-not $performanceExceptionAccepted -and $status -eq 'failed' -and
                $null -ne (Get-S18StableExceptionLimit $language $operation)) {
                $disposition = 'strict S18 failed; raw evidence did not qualify for the bounded release exception'
            }
            $sampleDescription = if ($S18Policy -ceq 'stable' -and $null -ne (Get-S18StableExceptionLimit $language $operation)) {
                'single-request P50/P95/P99, three ABBA rounds with 1000 samples per candidate/upstream leg and upstream correctness'
            } else {
                'single-request P50/P95/P99, 1000 raw samples and upstream correctness'
            }
            & $addRow $checkId 'S18' "$language $operation $sampleDescription" 'test/perf/e2e_lsp_test.go' "performance.json#$checkId" $status $true $disposition $performanceExceptionAccepted
        }
    }
    $s21CheckId = "$RunId/S21/zero-error-classification"
    $s21Status = Get-SummaryCheckStatus $fastSummary @('s21-zero-error-classification')
    & $addRow 'S21/zero-error-classification' 'S21' 'Required corpus languages execute hover, definition, references, and rename with no classified accuracy errors and no skips' 'test/corpus/classify_test.go' "s21.json#$s21CheckId" $s21Status
    foreach ($bucket in @(
        @{ id = 'wrong_edit'; label = 'Wrong Edit Rate' },
        @{ id = 'stale_edit'; label = 'Stale Edit Applied' },
        @{ id = 'wrong_file_location'; label = 'Wrong-file Location' },
        @{ id = 'position_mapping_error'; label = 'Position Mapping Error' },
        @{ id = 'protocol_invalid_response'; label = 'Protocol-invalid Response' },
        @{ id = 'snapshot_mixing'; label = 'Snapshot Mixing' }
    )) {
        $bucketStatus = Get-S21BucketStatus $s21Report $bucket.id $RunId ([string]$matrixCandidateHash)
        & $addRow "S21/$($bucket.id)" 'S21' "$($bucket.label) count is zero in structured corpus evidence" 'test/corpus/classify_test.go' "s21.json#$s21CheckId.observed.error_buckets.$($bucket.id)" $bucketStatus
    }
    foreach ($requested in @(200, 800, 3200)) {
        $checkId = "$RunId/S19/references@$requested"
        $status = Get-MatrixEvidenceCheckStatus $perfReport @($checkId)
        & $addRow $checkId 'S19' "Exact returned-location total including declaration at $requested with retained samples" 'test/perf/e2e_lsp_test.go' "performance.json#$checkId" $status
    }
    foreach ($item in @(
        @{ id = 'active-cancellation'; text = 'Execution-time cancellation has correlated terminal response' },
        @{ id = 'progress'; text = 'Large query progress begin/end is observable' },
        @{ id = 'no-starvation'; text = 'P0/P1 requests complete during a P2 reference query' },
        @{ id = 'process-tree-resources'; text = 'Five process-tree snapshots stay below 8 GiB' },
        @{ id = 'environment/required-metadata'; text = 'Performance environment and toolchain metadata is pinned' }
    )) {
        $checkId = "$RunId/S19/$($item.id)"
        if ($item.id -eq 'environment/required-metadata') { $checkId = "$RunId/environment/required-metadata" }
        $status = Get-MatrixEvidenceCheckStatus $perfReport @($checkId)
        & $addRow $checkId 'S19' $item.text 'test/perf/e2e_lsp_test.go' "performance.json#$checkId" $status
    }

    foreach ($checkName in @('environment-metadata', 'language/go', 'language/c', 'language/cplusplus', 'language/rust', 'language/python', 'language/typescript', 'language/javascript', 'unsaved-edit', 'unmodified-edit', 'unicode-crlf', 'cpp-build-context', 'broken-source-diagnostics', 'broken-source', 'diagnostic-edit', 'rename-safety', 'rename-local-edit-validation', 'stale-result-rejection', 'accuracy-kpi')) {
        $checkId = "$RunId/S20/$checkName"
        $status = Get-MatrixEvidenceCheckStatus $semanticReport @($checkId)
        & $addRow $checkId 'S20/S21' "Semantic, diagnostic, freshness, build-context, and safe-edit evidence: $checkName" 'test/acceptance/semantic_acceptance_test.go' "semantic.json#$checkId" $status
    }
    if (Test-Path -LiteralPath $semanticReport) {
        try {
            $semanticDataForMatrix = Get-Content -LiteralPath $semanticReport -Raw | ConvertFrom-Json -ErrorAction Stop
            $kpiChecksForMatrix = @($semanticDataForMatrix.checks | Where-Object { [string]$_.id -ceq "$RunId/S20/accuracy-kpi" })
            if ($kpiChecksForMatrix.Count -eq 1 -and $kpiChecksForMatrix[0].observed.schema -ceq 'omnilsp.s20.accuracy-kpi.v2') {
                foreach ($record in @($kpiChecksForMatrix[0].observed.records)) {
                    $dimension = $record.dimension
                    foreach ($metricName in @('precision', 'recall', 'falsePositiveRate', 'falseNegativeRate', 'refusalRate', 'staleResultRejectionRate', 'wrongFileRate', 'positionMappingFailureRate', 'editValidationFailureRate')) {
                        $metric = $record.metrics.PSObject.Properties[$metricName].Value
                        $metricDisposition = ''
                        $metricStatus = switch ([string]$metric.status) {
                            'observed' { 'passed' }
                            'not_applicable' { $metricDisposition = "not applicable: $([string]$metric.reason)"; 'passed' }
                            'invalid' { 'failed' }
                            default { 'not_verified' }
                        }
                        if ($script:FastEvidenceGatePresent -and -not $script:FastEvidenceIdentityValid) { $metricStatus = 'failed' }
                        $metricId = "S20/KPI/$($dimension.language)/$($dimension.feature)/$($dimension.backend)/$metricName"
                        $metricRequirement = "$($dimension.language) $($dimension.feature) $metricName tracked with explicit sample counts"
                        if ([string]$metric.status -ceq 'not_applicable') { $metricRequirement = "$($dimension.language) $($dimension.feature) $metricName is not applicable to a read-only result" }
                        & $addRow $metricId 'S20 accuracy KPI' $metricRequirement 'internal/telemetry/kpi.go; test/acceptance/semantic_acceptance_test.go' "semantic.json#$RunId/S20/accuracy-kpi" $metricStatus $true $metricDisposition
                    }
                }
            }
        } catch {
            & $addRow 'S20/KPI/structure' 'S20 accuracy KPI' 'Structured nine-metric report must be present and parseable' 'internal/telemetry/kpi.go; test/acceptance/semantic_acceptance_test.go' 'semantic.json#S20/accuracy-kpi' 'not_verified'
        }
    } else {
        & $addRow 'S20/KPI/structure' 'S20 accuracy KPI' 'Structured nine-metric report must be present and parseable' 'internal/telemetry/kpi.go; test/acceptance/semantic_acceptance_test.go' 'semantic.json#S20/accuracy-kpi' 'not_verified'
    }
    foreach ($client in $clientMatrixClients) {
        foreach ($language in $clientMatrixLanguages) {
            $checkId = "client/$client/$language"
            $status = Get-MatrixEvidenceCheckStatus $clientReport @($checkId)
            & $addRow $checkId 'S11/X2' "$client $language client/language cell has real evidence or an explicit not_verified disposition" 'test/acceptance/clients' "clients.json#$checkId" $status
        }
    }
    foreach ($checkId in @(Get-ClientMatrixSummaryIds)) {
        $status = Get-MatrixEvidenceCheckStatus $clientReport @($checkId)
        & $addRow $checkId 'S11/X2' "$checkId backward-compatible family summary with exact advertised subcases, goal-scoped diagnostics, semantic operations, rename safety and clean exit" 'test/acceptance/clients' "clients.json#$checkId" $status
    }
    & $addRow 'DEF-CCLSDIAG' 'ADR-0008/DEF-CCLSDIAG' 'C/C++ upstream diagnostics forwarding is tracked as accepted deferred scope; client still checks no false positive on clean source' 'internal/conformance/registry.go#DEF-CCLSDIAG' 'registry.go#DEF-CCLSDIAG' 'not_verified' $false 'accepted-deferred-to-X3; no product claim of diagnostic forwarding'

    foreach ($stage in @('30s', '10m', '1h')) {
        $stagePath = Join-Path $EvidenceDirectory "soak-$stage.json"
        $stageStatus = 'not_verified'
        $checkIds = Get-SoakRequiredCheckIds $stage
        $reportStatus = Get-MatrixEvidenceCheckStatus (Join-Path $EvidenceDirectory "soak-$stage-report.json") $checkIds
        if (Test-Path -LiteralPath $stagePath) {
            try {
                $stageData = Get-Content -LiteralPath $stagePath -Raw | ConvertFrom-Json
                Assert-SoakStage $stage
                if ($stageData.status -eq 'failed' -or $reportStatus -eq 'failed') { $stageStatus = 'failed' }
                elseif ($stageData.status -eq 'passed' -and $reportStatus -eq 'passed') { $stageStatus = 'passed' }
            } catch { $stageStatus = 'failed' }
        }
        & $addRow "SOAK/$stage" 'long-run preflight' "Continuous $stage real-stdio mixed workload, 8 workers, 8 GiB cap, cancellation and resource checks" 'test/soak/stdio_soak_test.go' "soak-$stage.json + soak-$stage-report.json" $stageStatus
        foreach ($terminal in @(
            @{ id = 'soak/final-close'; label = 'candidate and backend processes exited before the final process-tree sample' },
            @{ id = 'soak/coverage'; label = 'eight workers, semantic requests, edits, and process-tree sampling completed' },
            @{ id = 'soak/duration'; label = 'continuous active monitoring covered the requested duration' }
        )) {
            $terminalStatus = Get-MatrixEvidenceCheckStatus (Join-Path $EvidenceDirectory "soak-$stage-report.json") @($terminal.id)
            & $addRow "SOAK/$stage/$($terminal.id.Split('/')[-1])" 'soak finalization' "$stage $($terminal.label)" 'test/soak/stdio_soak_test.go' "soak-$stage-report.json#$($terminal.id)" $terminalStatus
        }
    }
    $workerStatusPath = Join-Path $EvidenceDirectory 'soak-worker-status.json'
    $workerStatusValue = 'not_verified'
    if (Test-Path -LiteralPath $workerStatusPath) {
        try {
            $workerData = Get-Content -LiteralPath $workerStatusPath -Raw | ConvertFrom-Json
            if ($workerData.status -in @('failed', 'invalidated')) { $workerStatusValue = 'failed' }
            elseif ($workerData.status -eq 'passed' -and $workerData.report) {
                if (-not $script:FastEvidenceGatePresent -or -not $script:FastEvidenceIdentityValid) { throw '1h status has no valid frozen Fast evidence.' }
                $matrixGates = Get-Content -LiteralPath $gatesPath -Raw | ConvertFrom-Json -ErrorAction Stop
                if ($workerData.run_id -cne $RunId -or $workerData.candidate_sha256 -cne $matrixGates.candidate_sha256 -or
                    $workerData.candidate_sha256 -cne $matrixCandidateHash -or
                    $workerData.soak_test_sha256 -cne $matrixGates.soak_test_sha256 -or
                    [IO.Path]::GetFileName([string]$workerData.report) -cne [string]$workerData.report) {
                    throw '1h worker identity differs from the frozen Fast gate.'
                }
                $started = [DateTimeOffset]::Parse([string]$workerData.worker_process_started_at).ToUniversalTime()
                $finished = [DateTimeOffset]::Parse([string]$workerData.finished_at).ToUniversalTime()
                if (($finished - $started) -lt [TimeSpan]::FromHours(1)) { throw '1h worker duration is shorter than 1 uninterrupted hour.' }
                $workerReportPath = Join-Path $EvidenceDirectory ([string]$workerData.report)
                if ((Get-FileSha256 $workerReportPath) -cne [string]$workerData.report_sha256) { throw '1h worker report hash changed.' }
                $null = Assert-StructuredReportIdentity $workerReportPath 'soak' $RunId ([string]$matrixGates.candidate_sha256) [DateTime]::MinValue (Get-SoakRequiredCheckIds '1h') '1h'
                $workerChecks = Get-MatrixEvidenceCheckStatus $workerReportPath (Get-SoakRequiredCheckIds '1h')
                if ($workerChecks -eq 'passed') { $workerStatusValue = 'passed' }
                elseif ($workerChecks -eq 'failed') { $workerStatusValue = 'failed' }
            }
        } catch { $workerStatusValue = 'failed' }
    }
    & $addRow 'SOAK/1h' 'one-hour final gate' 'One uninterrupted 1-hour run with same candidate/test/fingerprint and complete final report' 'scripts/acceptance.ps1 -Phase Start1h/Status; test/soak/stdio_soak_test.go' 'soak-worker-status.json + final report' $workerStatusValue
    $workerReportForMatrix = $null
    if (Test-Path -LiteralPath $workerStatusPath) {
        try {
            $workerMatrixData = Get-Content -LiteralPath $workerStatusPath -Raw | ConvertFrom-Json -ErrorAction Stop
            if ($workerMatrixData.report -and [IO.Path]::GetFileName([string]$workerMatrixData.report) -ceq [string]$workerMatrixData.report) {
                $workerReportForMatrix = Join-Path $EvidenceDirectory ([string]$workerMatrixData.report)
            }
        } catch { $workerReportForMatrix = $null }
    }
    foreach ($terminal in @(
        @{ id = 'soak/final-close'; label = 'candidate and backend processes exited before the final process-tree sample' },
        @{ id = 'soak/coverage'; label = 'eight workers, semantic requests, edits, and process-tree sampling completed' },
        @{ id = 'soak/duration'; label = 'continuous active monitoring covered the requested duration' }
    )) {
        $terminalStatus = if ($workerReportForMatrix) { Get-MatrixEvidenceCheckStatus $workerReportForMatrix @($terminal.id) } else { 'not_verified' }
        & $addRow "SOAK/1h/$($terminal.id.Split('/')[-1])" 'soak finalization' "1h $($terminal.label)" 'test/soak/stdio_soak_test.go' "soak-1h-report.json#$($terminal.id)" $terminalStatus
    }

    $decision = Get-RequirementMatrixDecision $script:AcceptanceMatrixRows
    $evidenceHashes = [ordered]@{}
    if (Test-Path -LiteralPath $EvidenceDirectory) {
        foreach ($file in Get-ChildItem -LiteralPath $EvidenceDirectory -File -Recurse | Sort-Object FullName) {
            if ($file.Name -in @('requirement-matrix.json', 'requirement-matrix.md')) { continue }
            $relative = [IO.Path]::GetRelativePath($EvidenceDirectory, $file.FullName).Replace('\', '/')
            $evidenceHashes[$relative] = Get-FileSha256 $file.FullName
        }
    }
    $matrix = [ordered]@{
        schema_version = 1; run_id = $RunId; phase = $Phase; candidate_sha256 = $matrixCandidateHash
        s18_policy = $S18Policy; candidate_fingerprint_sha256 = $candidateFingerprint.fingerprint_sha256
        acceptance_fingerprint_sha256 = $fingerprint; release_assessment = $script:FastReleaseAssessment
        evidence_sha256 = $evidenceHashes
        decision = $decision; generated_at = (Get-Date).ToUniversalTime().ToString('o')
        rows = @($script:AcceptanceMatrixRows)
    }
    $jsonPath = Join-Path $EvidenceDirectory 'requirement-matrix.json'
    $markdownPath = Join-Path $EvidenceDirectory 'requirement-matrix.md'
    $matrix | ConvertTo-Json -Depth 12 | Set-Content -LiteralPath $jsonPath -Encoding utf8
    $markdown = [Collections.Generic.List[string]]::new()
    $markdown.Add('# Acceptance requirement matrix')
    $markdown.Add('')
    $markdown.Add("Run: ``$RunId``  |  Candidate SHA-256: ``$matrixCandidateHash``  |  Decision: **$decision**")
    $markdown.Add('')
    $markdown.Add('| ID | Goal | Requirement | Implementation | Evidence | Selected | Status | Disposition |')
    $markdown.Add('|---|---|---|---|---|---:|---|---|')
    foreach ($row in $script:AcceptanceMatrixRows) {
        $values = @($row.id, $row.goal, $row.requirement, $row.implementation, $row.evidence, [string]$row.selected, $row.status, $row.disposition)
        $markdown.Add('| ' + (($values | ForEach-Object { ([string]$_).Replace('|', '\|').Replace("`n", ' ') }) -join ' | ') + ' |')
    }
    $markdown | Set-Content -LiteralPath $markdownPath -Encoding utf8
    Write-Output "Requirement matrix: $markdownPath (decision=$decision)"
}

function Invoke-Soak([string] $Duration, [string] $RunIdForSoak) {
    if (-not (Test-Path $CandidateBinary)) { throw "Candidate binary is missing: $CandidateBinary" }
    $testExe = Join-Path $EvidenceDirectory 'soak.test.exe'
    if (-not (Test-Path $testExe)) { throw "Frozen soak test executable is missing: $testExe; rerun the full Fast gate." }
    $gatePath = Join-Path $EvidenceDirectory 'fast-gates.json'
    if (-not (Test-Path $gatePath)) { throw 'Soak preflights require successful Fast acceptance evidence.' }
    $gates = Get-Content -LiteralPath $gatePath -Raw | ConvertFrom-Json
    if ($gates.status -notin @('passed', 'passed_with_performance_exception') -or $gates.run_id -ne $RunIdForSoak) { throw 'Soak preflights require a completed Fast gate from the same run ID.' }
    Assert-AcceptanceFingerprint $gates "Soak $Duration"
    $hashBefore = (Get-FileHash -Algorithm SHA256 -LiteralPath $CandidateBinary).Hash.ToLowerInvariant()
    $testHashBefore = (Get-FileHash -Algorithm SHA256 -LiteralPath $testExe).Hash.ToLowerInvariant()
    if ($hashBefore -ne $gates.candidate_sha256) { throw 'Candidate hash differs from the passed Fast gate; start a new full run.' }
    if ($testHashBefore -ne $gates.soak_test_sha256) { throw 'Soak test executable hash differs from the passed Fast gate; start a new full run.' }
    $soakJsonl = Join-Path $EvidenceDirectory "soak-$Duration.jsonl"
    $soakReport = Join-Path $EvidenceDirectory "soak-$Duration-report.json"
    Remove-Item -LiteralPath $soakJsonl, $soakReport -Force -ErrorAction SilentlyContinue
    $stageStarted = (Get-Date).ToUniversalTime()
    $env:SOAK_DURATION = $Duration
    $env:SOAK_JSONL = $soakJsonl
    $env:OMNILSP_ACCEPTANCE_REPORT = $soakReport
    $env:OMNILSP_BIN = $CandidateBinary
    $env:OMNILSP_RUN_ID = $RunIdForSoak
    # Explicit opt-in for the strict real-stdio gate (§S16); without it the
    # test skips as form-only. See docs/soak-nightly.md.
    $env:OMNILSP_SOAK_GATE = 'required'
    $result = Invoke-Step "soak-$Duration" $testExe @('-test.run=^TestSoak_RealStdioMixedWorkload$', '-test.count=1', '-test.timeout=90m')
    Remove-Item Env:SOAK_DURATION, Env:SOAK_JSONL, Env:OMNILSP_ACCEPTANCE_REPORT, Env:OMNILSP_BIN, Env:OMNILSP_RUN_ID, Env:OMNILSP_SOAK_GATE -ErrorAction SilentlyContinue
    $hashAfter = (Get-FileHash -Algorithm SHA256 -LiteralPath $CandidateBinary).Hash.ToLowerInvariant()
    if ($hashBefore -ne $hashAfter) { throw 'Candidate binary hash changed during soak.' }
    $testHashAfter = (Get-FileHash -Algorithm SHA256 -LiteralPath $testExe).Hash.ToLowerInvariant()
    if ($testHashBefore -ne $testHashAfter) { throw 'Frozen soak test executable hash changed during soak.' }
    Assert-AcceptanceFingerprint $gates "Soak $Duration completion"
    $soakChecks = Get-SoakRequiredCheckIds $Duration
    $reportData = Assert-StructuredReportIdentity $soakReport 'soak' $RunIdForSoak $hashBefore $stageStarted $soakChecks $Duration ([int]$result.exitCode)
    if ($result.status -ne 'passed') { throw "Soak $Duration failed with producer exit code $($result.exitCode); see $($result.log)." }
    if ($reportData.decision -ne 'passed') { throw "Soak report decision is '$($reportData.decision)', not 'passed'." }
    return [ordered]@{
        result = $result
        candidate_sha256 = $hashBefore
        soak_test_sha256 = $testHashBefore
        report = [IO.Path]::GetFileName($soakReport)
        report_decision = $reportData.decision
    }
}

function Get-SoakRequiredCheckIds([string] $Duration) {
    $checks = @(
        'soak/process-tree-limit', 'soak/initialize-open', 'soak/cancel', 'soak/reindex',
        'soak/semantic-workload', 'soak/backend-close', 'soak/backend-restart', 'soak/restart',
        'soak/continuity', 'soak/server-progress', 'soak/progress-lifecycle',
        'soak/resource-cap', 'soak/workers',
        'soak/candidate-identity', 'soak/worker-identity', 'soak/cancel-repeated',
        'soak/go-runtime-bounds', 'soak/final-close', 'soak/coverage', 'soak/duration'
    )
    if ($Duration -eq '1h') { $checks += 'soak/resource-trend' }
    return $checks
}

function Assert-SoakStage([string] $Stage) {
    $stagePath = Join-Path $EvidenceDirectory "soak-$Stage.json"
    if (-not (Test-Path $stagePath)) { throw "Soak $Stage evidence is missing." }
    $stageData = Get-Content -LiteralPath $stagePath -Raw | ConvertFrom-Json
    $gatePath = Join-Path $EvidenceDirectory 'fast-gates.json'
    if (-not (Test-Path $gatePath)) { throw 'Soak stage evidence requires a passed Fast gate.' }
    $gates = Get-Content -LiteralPath $gatePath -Raw | ConvertFrom-Json
    Assert-AcceptanceFingerprint $gates "Soak $Stage"
    if ($stageData.run_id -ne $RunId -or $stageData.status -ne 'passed' -or
        $stageData.candidate_sha256 -ne $gates.candidate_sha256 -or
        $stageData.soak_test_sha256 -ne $gates.soak_test_sha256) {
        throw "Soak $Stage evidence does not match this run and its frozen candidate/test hashes."
    }
    $testExe = Join-Path $EvidenceDirectory 'soak.test.exe'
    if ((Get-FileSha256 $CandidateBinary) -cne [string]$stageData.candidate_sha256 -or
        (Get-FileSha256 $testExe) -cne [string]$stageData.soak_test_sha256) {
        throw "Soak $Stage candidate or test executable differs from its frozen stage identity."
    }
    if (-not $stageData.report -or [IO.Path]::GetFileName([string]$stageData.report) -cne [string]$stageData.report) {
        throw "Soak $Stage report path is missing or not a file name inside the evidence directory."
    }
    $reportPath = [IO.Path]::GetFullPath((Join-Path $EvidenceDirectory $stageData.report))
    $evidencePrefix = [IO.Path]::GetFullPath($EvidenceDirectory).TrimEnd([char[]]@([IO.Path]::DirectorySeparatorChar, [IO.Path]::AltDirectorySeparatorChar)) + [IO.Path]::DirectorySeparatorChar
    if (-not $reportPath.StartsWith($evidencePrefix, [StringComparison]::OrdinalIgnoreCase)) { throw "Soak $Stage report path escapes the evidence directory." }
    if (-not $stageData.report_sha256 -or (Get-FileSha256 $reportPath) -ne $stageData.report_sha256) { throw "Soak $Stage report hash differs from the frozen stage record." }
    $soakChecks = Get-SoakRequiredCheckIds $Stage
    $reportData = Assert-StructuredReportIdentity $reportPath 'soak' $RunId $stageData.candidate_sha256 [DateTime]::MinValue $soakChecks $Stage
    if ($reportData.decision -ne 'passed') { throw "Soak $Stage structured report decision is '$($reportData.decision)', not passed." }
}

function Clear-SoakStageAttempt([string] $Stage) {
    Remove-Item -LiteralPath @(
        (Join-Path $EvidenceDirectory "soak-$Stage.json"),
        (Join-Path $EvidenceDirectory "soak-$Stage.jsonl"),
        (Join-Path $EvidenceDirectory "soak-$Stage-report.json")
    ) -Force -ErrorAction SilentlyContinue
}

function Write-SoakStageFailure([string] $Stage, $Failure) {
    $message = [string]$Failure.Exception.Message
    if (-not $message) { $message = [string]$Failure }
    $candidateHash = if (Test-Path -LiteralPath $CandidateBinary -PathType Leaf) { Get-FileSha256 $CandidateBinary } else { $null }
    $testExe = Join-Path $EvidenceDirectory 'soak.test.exe'
    $testHash = if (Test-Path -LiteralPath $testExe -PathType Leaf) { Get-FileSha256 $testExe } else { $null }
    $reportName = "soak-$Stage-report.json"
    $reportPath = Join-Path $EvidenceDirectory $reportName
    $reportHash = if (Test-Path -LiteralPath $reportPath -PathType Leaf) { Get-FileSha256 $reportPath } else { $null }
    [ordered]@{
        run_id = $RunId
        stage = $Stage
        status = 'failed'
        candidate_sha256 = $candidateHash
        soak_test_sha256 = $testHash
        report = if ($reportHash) { $reportName } else { $null }
        report_sha256 = $reportHash
        error = $message
        finished_at = (Get-Date).ToUniversalTime().ToString('o')
    } | ConvertTo-Json -Depth 8 | Set-Content -LiteralPath (Join-Path $EvidenceDirectory "soak-$Stage.json") -Encoding utf8
    $check = [ordered]@{ id = "soak-$Stage"; status = 'failed'; detail = $message; log = "soak-$Stage.log" }
    $null = Write-Summary 'failed' @($check) @($message)
    Write-RequirementMatrix
}

$script:StartedAt = (Get-Date).ToUniversalTime()

switch ($Phase) {
    'Fast' {
        $script:FastBoundedS18ExceptionIds = @()
        $script:FastReleaseAssessment = $null
        $script:FastReleaseDecision = $null
        Remove-Item -LiteralPath @(
            (Join-Path $EvidenceDirectory 'fast-gates.json'),
            (Join-Path $EvidenceDirectory 'Fast-summary.json'),
            (Join-Path $EvidenceDirectory 's21.json'),
            (Join-Path $EvidenceDirectory 'performance.json'),
            (Join-Path $EvidenceDirectory 'semantic.json'),
            (Join-Path $EvidenceDirectory 'clients.json')
        ) -Force -ErrorAction SilentlyContinue
        Push-Location $repoRoot
        try {
            git status --short | Set-Content -LiteralPath (Join-Path $EvidenceDirectory 'worktree-status.txt') -Encoding utf8
            git diff --stat HEAD | Set-Content -LiteralPath (Join-Path $EvidenceDirectory 'worktree-diff-stat.txt') -Encoding utf8
        } finally {
            Pop-Location
        }
        $checks = [Collections.Generic.List[object]]::new()
        $errors = [Collections.Generic.List[string]]::new()
        $powershell = (Get-Command pwsh -ErrorAction Stop).Source
        $checks.Add((Invoke-Step 'acceptance-report-contract' $powershell @('-NoLogo', '-NoProfile', '-File', (Join-Path $repoRoot 'scripts/test-acceptance-contract.ps1'))))
        $toolLock = Get-Content (Join-Path $repoRoot 'test/acceptance/tools/tools.lock.json') -Raw | ConvertFrom-Json
        $resolvedBinding = Initialize-LockedAcceptanceEnvironment $toolLock
        $checks.Add($resolvedBinding)
        if ([string]$resolvedBinding.status -cne 'passed') {
            throw "Fast gate cannot execute with unresolved acceptance tool paths: $($resolvedBinding.detail)"
        }
        $checks.Add((Test-VersionLock))
        if ([string]$checks[-1].status -cne 'passed') {
            throw "Fast gate cannot execute with an unverified locked toolchain: $($checks[-1].detail)"
        }
        if (Ensure-LocalToolWrappers) {
            $checks.Add([ordered]@{ id = 'local-language-server-wrappers'; status = 'passed'; detail = 'project-local launchers use locked npm package entrypoints' })
        } else {
            $checks.Add([ordered]@{ id = 'local-language-server-wrappers'; status = 'not_verified'; detail = 'pinned npm package entrypoints are missing' })
        }
        $script:FastSourceTreeHash = Get-WorkspaceContentHash
        $checks.Add((Invoke-Step 'unit' $go @('test', '-count=1', '-timeout=900s', './...')))
        $checks.Add((Invoke-Step 'race' $go @('test', '-race', '-count=1', '-timeout=900s', './...')))
        $checks.Add((Invoke-Step 'vet' $go @('vet', './...')))
        New-Item -ItemType Directory -Force -Path $EvidenceDirectory | Out-Null
        $checks.Add((Invoke-Step 'build-candidate' $go @('build', '-trimpath', '-o', $CandidateBinary, './cmd/omnilsp')))
        $oldGOOS = $env:GOOS
        $oldGOARCH = $env:GOARCH
        $crossBuildDirectory = Join-Path ([IO.Path]::GetTempPath()) ("omnilsp-cross-build-" + [guid]::NewGuid().ToString('N'))
        New-Item -ItemType Directory -Path $crossBuildDirectory | Out-Null
        try {
            foreach ($target in @(
                @{ goos = 'linux'; goarch = 'amd64' },
                @{ goos = 'linux'; goarch = 'arm64' },
                @{ goos = 'darwin'; goarch = 'arm64' },
                @{ goos = 'darwin'; goarch = 'amd64' }
            )) {
                $env:GOOS = $target.goos
                $env:GOARCH = $target.goarch
                $crossBuildOutput = Join-Path $crossBuildDirectory "omnilsp-$($target.goos)-$($target.goarch)"
                try {
                    $checks.Add((Invoke-Step "cross-build-$($target.goos)-$($target.goarch)" $go @('build', '-trimpath', '-o', $crossBuildOutput, './cmd/omnilsp')))
                } finally {
                    Remove-Item -LiteralPath $crossBuildOutput -Force -ErrorAction SilentlyContinue
                }
            }
        } finally {
            $env:GOOS = $oldGOOS
            $env:GOARCH = $oldGOARCH
            Remove-Item -LiteralPath $crossBuildDirectory -Force -ErrorAction SilentlyContinue
        }
        $soakTest = Join-Path $EvidenceDirectory 'soak.test.exe'
        $checks.Add((Invoke-Step 'build-soak-test' $go @('test', '-tags', 'soak', '-c', '-o', $soakTest, './test/soak/')))
        foreach ($fuzz in @(
            @{ name = 'fuzz-jsonrpc'; target = './internal/protocol/jsonrpc/'; func = 'FuzzJSONRPC' },
            @{ name = 'fuzz-position'; target = './internal/protocol/jsonrpc/'; func = 'FuzzPosition' },
            @{ name = 'fuzz-position-conversion'; target = './internal/workspace/position/'; func = 'FuzzPositionConversion' },
            @{ name = 'fuzz-position-edge-cases'; target = './internal/workspace/position/'; func = 'FuzzPositionEdgeCases' },
            @{ name = 'fuzz-multibyte-boundary'; target = './internal/workspace/position/'; func = 'FuzzMultiByteBoundary' },
            @{ name = 'fuzz-did-change'; target = './internal/runtime/server/'; func = 'FuzzDidChange' },
            @{ name = 'fuzz-config-load'; target = './internal/config/'; func = 'FuzzConfigLoad' },
            @{ name = 'fuzz-config-validate'; target = './internal/config/'; func = 'FuzzConfigValidate' },
            @{ name = 'fuzz-uri'; target = './internal/languages/golang/'; func = 'FuzzURI' },
            @{ name = 'fuzz-open-snapshot-corrupt-store'; target = './internal/index/persistent/'; func = 'FuzzOpenSnapshotCorruptStore' },
            @{ name = 'fuzz-freshness-seal-verify'; target = './internal/index/persistent/'; func = 'FuzzFreshnessSealVerify' },
            @{ name = 'fuzz-uri-parse'; target = './internal/workspace/uri/'; func = 'FuzzURIParse' }
        )) {
            $checks.Add((Invoke-Step $fuzz.name $go @('test', "-fuzz=$($fuzz.func)", '-fuzztime=10s', $fuzz.target)))
        }
        $checks.Add((Test-FastSourceTreeIdentity 'source-stability-before-build'))
        if (($checks | Where-Object { $_.status -eq 'failed' }).Count -gt 0) {
            $summaryPath = Write-Summary 'failed' $checks $errors
            Write-RequirementMatrix
            throw "Core Go execution gate failed. Tooling availability is reported separately; evidence: $summaryPath"
        }

        $scoreLog = Join-Path $EvidenceDirectory 'quick-score-baseline.log'
        Push-Location $repoRoot
        try { & $CandidateBinary verify 2>&1 | Set-Content -LiteralPath $scoreLog -Encoding utf8 } finally { Pop-Location }

        $binaryHash = (Get-FileHash -Algorithm SHA256 -LiteralPath $CandidateBinary).Hash.ToLowerInvariant()
        $env:OMNILSP_BIN = $CandidateBinary
        $env:OMNILSP_ACCEPTANCE = '1'
        $env:OMNILSP_RUN_ID = $RunId
        $env:OMNILSP_CANDIDATE_SHA256 = $binaryHash
        $env:OMNILSP_S21_GATE = 'required'
        $env:OMNILSP_S21_REPORT = Join-Path $EvidenceDirectory 's21.json'
        Remove-Item -LiteralPath $env:OMNILSP_S21_REPORT -Force -ErrorAction SilentlyContinue
        $s21Started = (Get-Date).ToUniversalTime()
        $s21Step = Invoke-Step 's21-zero-error-classification' $go @('test', '-v', '-count=1', '-timeout=10m', './test/corpus', '-run', '^TestS21_ZeroErrorClassification$')
        $s21Report = $env:OMNILSP_S21_REPORT
        if (-not (Test-Path -LiteralPath $s21Report -PathType Leaf)) {
            if ($s21Step.exitCode -eq 0) { $s21Step.status = 'not_verified' }
            $s21Step.detail = 'S21 corpus test did not write its structured zero-error report'
        } else {
            try {
                $s21Data = Assert-S21ReportEvidence $s21Report $RunId $binaryHash $s21Started
                if ($s21Step.exitCode -ne 0 -and [string]$s21Data.decision -ne 'passed') {
                    $s21Step.status = 'failed'
                    $s21Step.detail = "S21 producer exited with code $($s21Step.exitCode) and structured decision '$($s21Data.decision)'"
                } elseif ([string]$s21Data.decision -eq 'failed') {
                    $s21Step.status = 'failed'
                    $s21Step.detail = 'structured S21 decision: failed'
                } elseif ([string]$s21Data.decision -eq 'not_verified') {
                    $s21Step.status = 'not_verified'
                    $s21Step.detail = 'structured S21 decision: not_verified'
                } elseif ($s21Step.exitCode -ne 0) {
                    $s21Step.status = 'failed'
                    $s21Step.detail = 'S21 process exited nonzero despite a passed report'
                }
            } catch {
                $s21Step.status = 'failed'
                $s21Step.detail = $_.Exception.Message
            }
        }
        $checks.Add($s21Step)
        $env:OMNILSP_PERF_REPORT = Join-Path $EvidenceDirectory 'performance.json'
        $env:OMNILSP_S18_POLICY = $S18Policy
        Remove-Item -LiteralPath $env:OMNILSP_PERF_REPORT -Force -ErrorAction SilentlyContinue
        $perfStarted = (Get-Date).ToUniversalTime()
        $checks.Add((Invoke-Step 'S18-S19-real-process-performance' $go @('test', '-count=1', '-timeout=60m', './test/perf/')))
        $perfReport = $env:OMNILSP_PERF_REPORT
        $perfIds = [Collections.Generic.List[string]]::new()
        foreach ($language in @('go', 'c', 'cpp', 'rust', 'python', 'typescript', 'javascript')) {
            foreach ($operation in @('hot_hover', 'hot_definition', 'completion_first_usable', 'syntax_update_after_edit')) {
                $perfIds.Add("$RunId/S18/$language/$operation")
            }
        }
        foreach ($requested in @(200, 800, 3200)) { $perfIds.Add("$RunId/S19/references@$requested") }
        foreach ($checkId in @('active-cancellation', 'progress', 'no-starvation', 'process-tree-resources')) { $perfIds.Add("$RunId/S19/$checkId") }
        $perfIds.Add("$RunId/environment/required-metadata")
        if (-not (Test-Path $perfReport)) {
            if ($checks[-1].exitCode -eq 0) { $checks[-1].status = 'not_verified' }
            $checks[-1].detail = 'acceptance test did not write its structured JSON report'
        } else {
            try {
                $perfData = Assert-StructuredReportIdentity $perfReport 'performance' $RunId $binaryHash $perfStarted $perfIds.ToArray() '' ([int]$checks[-1].exitCode)
                $perfDecision = [string]$perfData.release_assessment.decision
                $script:FastReleaseAssessment = $perfData.release_assessment
            } catch {
                $checks[-1].status = 'failed'
                $checks[-1].detail = $_.Exception.Message
                $perfDecision = 'invalid'
            }
            if ($perfDecision -in @('failed', 'invalid')) {
                $checks[-1].status = 'failed'
                if (-not $checks[-1].detail) { $checks[-1].detail = "structured report decision: $perfDecision" }
            } elseif ($perfDecision -eq 'not_verified') {
                $checks[-1].status = 'not_verified'
                $checks[-1].detail = "structured report decision: $perfDecision"
            } elseif ($perfDecision -eq 'passed_with_performance_exception') {
                $script:FastReleaseDecision = $perfDecision
                if ($checks[-1].exitCode -ne 0) {
                    $checks[-1].status = 'failed'
                    $checks[-1].detail = 'test process exited nonzero despite a bounded latency exception assessment'
                } else {
                    $checks[-1].status = 'passed'
                    $checks[-1].detail = 'strict S18 threshold failures are preserved and independently verified under the selected stability policy'
                }
            } elseif ($perfDecision -eq 'passed') {
                $script:FastReleaseDecision = $perfDecision
                if ($checks[-1].exitCode -ne 0) {
                    $checks[-1].status = 'failed'
                    $checks[-1].detail = 'test process exited nonzero despite a passing report'
                }
            } elseif ($checks[-1].exitCode -ne 0) {
                $checks[-1].status = 'failed'
                $checks[-1].detail = 'test process exited nonzero despite its structured release assessment'
            } else {
                $checks[-1].status = 'failed'
                $checks[-1].detail = "unsupported or missing performance release assessment: $perfDecision"
            }
        }
        $env:OMNILSP_ACCEPTANCE_REPORT = Join-Path $EvidenceDirectory 'semantic.json'
        Remove-Item -LiteralPath $env:OMNILSP_ACCEPTANCE_REPORT -Force -ErrorAction SilentlyContinue
        $semanticStarted = (Get-Date).ToUniversalTime()
        $checks.Add((Invoke-Step 'S20-real-process-semantic-acceptance' $go @('test', '-run', '^TestS20_RealProcessSemanticAcceptance$', '-count=1', '-timeout=60m', './test/acceptance/')))
        $semanticReport = $env:OMNILSP_ACCEPTANCE_REPORT
        if (-not (Test-Path $semanticReport)) {
            if ($checks[-1].exitCode -eq 0) { $checks[-1].status = 'not_verified' }
            $checks[-1].detail = 'semantic acceptance test did not write its structured JSON report'
        } else {
            $semanticIds = @(
                "$RunId/S20/environment-metadata", "$RunId/S20/language/go", "$RunId/S20/language/c",
                "$RunId/S20/language/cplusplus", "$RunId/S20/language/rust", "$RunId/S20/language/python",
                "$RunId/S20/language/typescript", "$RunId/S20/language/javascript", "$RunId/S20/unsaved-edit",
                "$RunId/S20/unmodified-edit", "$RunId/S20/unicode-crlf", "$RunId/S20/cpp-build-context",
                "$RunId/S20/broken-source-diagnostics", "$RunId/S20/broken-source", "$RunId/S20/diagnostic-edit",
                "$RunId/S20/rename-safety", "$RunId/S20/rename-local-edit-validation",
                "$RunId/S20/stale-result-rejection", "$RunId/S20/accuracy-kpi"
            )
            try {
                $semanticData = Assert-StructuredReportIdentity $semanticReport 'semantic' $RunId $binaryHash $semanticStarted $semanticIds '' ([int]$checks[-1].exitCode)
                $semanticDecision = $semanticData.decision
            } catch {
                $checks[-1].status = 'failed'
                $checks[-1].detail = $_.Exception.Message
                $semanticDecision = 'invalid'
            }
            if ($semanticDecision -in @('failed', 'invalid')) {
                $checks[-1].status = 'failed'
                $checks[-1].detail = "structured report decision: $semanticDecision"
            } elseif ($semanticDecision -ne 'passed') {
                $checks[-1].status = 'not_verified'
                $checks[-1].detail = "structured report decision: $semanticDecision"
            } elseif ($checks[-1].exitCode -ne 0) {
                $checks[-1].status = 'failed'
                $checks[-1].detail = 'test process exited nonzero despite a passing report'
            }
        }
        # Separate producers prevent persistent evidence from overwriting S20.
        foreach ($stage in @(
            @{ name = 'persistent-go'; test = 'TestS21GoCandidatePersistentQueriesSurviveDisabledBackend'; suffix = '-go'; id = "$RunId/S21/go-persistent-index-only-queries" },
            @{ name = 'persistent-typescript'; test = 'TestS21CandidatePersistentQueriesSurviveDisabledBackend'; suffix = ''; id = "$RunId/S21/persistent-index-only-queries" },
            @{ name = 'persistent-python'; test = 'TestS21PythonCandidatePersistentQueriesSurviveDisabledBackend'; suffix = '-python'; id = "$RunId/S21/python-persistent-index-only-queries" },
            @{ name = 'persistent-javascript'; test = 'TestS21JavaScriptCandidatePersistentQueriesSurviveDisabledBackend'; suffix = '-javascript'; id = "$RunId/S21/javascript-persistent-index-only-queries" },
            @{ name = 'persistent-c'; test = 'TestS21CCandidatePersistentQueriesSurviveDisabledBackend'; suffix = '-c'; id = "$RunId/S21/c-persistent-index-only-queries" },
            @{ name = 'persistent-cpp'; test = 'TestS21CppCandidatePersistentQueriesSurviveDisabledBackend'; suffix = '-cpp'; id = "$RunId/S21/cpp-persistent-index-only-queries" },
            @{ name = 'scip-replay-go'; test = 'TestCandidateGoCLIInteropAndSemanticReplayPersistence'; suffix = '-go-cli'; id = "$RunId/S22/go-cli-scip-replay-persistence" }
        )) {
            $baseReport = Join-Path $EvidenceDirectory ($stage.name + '.json')
            $env:OMNILSP_ACCEPTANCE_REPORT = $baseReport
            $stageReport = [IO.Path]::ChangeExtension($baseReport, $null) + $stage.suffix + '.json'
            Remove-Item -LiteralPath $stageReport -Force -ErrorAction SilentlyContinue
            $stageStarted = (Get-Date).ToUniversalTime()
            $checks.Add((Invoke-Step $stage.name $go @('test', '-run', ('^' + $stage.test + '$'), '-count=1', '-timeout=10m', './test/acceptance/')))
            try {
                $stageData = Assert-StructuredReportIdentity $stageReport 'persistent' $RunId $binaryHash $stageStarted @($stage.id) '' ([int]$checks[-1].exitCode)
                if ($checks[-1].exitCode -ne 0) { throw 'Persistent producer exited nonzero.' }
                $checks[-1].status = [string]$stageData.decision
                $checks[-1].detail = "structured persistent decision: $($stageData.decision)"
            } catch {
                $checks[-1].status = 'failed'
                $checks[-1].detail = $_.Exception.Message
            }
        }
        $env:OMNILSP_RUN_CLIENT_ACCEPTANCE = '1'
        $env:OMNILSP_VSCODE_BIN = [string]$script:AcceptanceResolvedPaths.vscode
        $env:OMNILSP_NVIM_BIN = [string]$script:AcceptanceResolvedPaths.neovim
        $env:OMNILSP_ACCEPTANCE_REPORT = Join-Path $EvidenceDirectory 'clients.json'
        Remove-Item -LiteralPath $env:OMNILSP_ACCEPTANCE_REPORT -Force -ErrorAction SilentlyContinue
        $clientsStarted = (Get-Date).ToUniversalTime()
        $checks.Add((Invoke-Step 'client-matrix-42' $go @('test', '-tags', 'clients', '-run', '^TestRealClientMatrix$', '-count=1', '-timeout=30m', './test/acceptance/clients/')))
        $clientReport = $env:OMNILSP_ACCEPTANCE_REPORT
        if (-not (Test-Path $clientReport)) {
            if ($checks[-1].exitCode -eq 0) { $checks[-1].status = 'not_verified' }
            $checks[-1].detail = 'client test did not write its structured JSON report'
        } else {
            $clientIds = @(Get-ClientMatrixRequiredCheckIds)
            try {
                $clientData = Assert-StructuredReportIdentity $clientReport 'clients' $RunId $binaryHash $clientsStarted $clientIds '' ([int]$checks[-1].exitCode)
                Assert-ClientMatrixContract $clientData
                $clientDecision = $clientData.decision
            } catch {
                $checks[-1].status = 'failed'
                $checks[-1].detail = $_.Exception.Message
                $clientDecision = 'invalid'
            }
            if ($clientDecision -in @('failed', 'invalid')) {
                $checks[-1].status = 'failed'
                $checks[-1].detail = "structured report decision: $clientDecision"
            } elseif ($clientDecision -ne 'passed') {
                $checks[-1].status = 'not_verified'
                $checks[-1].detail = "structured report decision: $clientDecision"
            } elseif ($checks[-1].exitCode -ne 0) {
                $checks[-1].status = 'failed'
                $checks[-1].detail = 'test process exited nonzero despite a passing report'
            }
        }
        Remove-Item Env:OMNILSP_ACCEPTANCE, Env:OMNILSP_BIN, Env:OMNILSP_PERF_REPORT, Env:OMNILSP_ACCEPTANCE_REPORT, Env:OMNILSP_RUN_ID, Env:OMNILSP_RUN_CLIENT_ACCEPTANCE, Env:OMNILSP_VSCODE_BIN, Env:OMNILSP_NVIM_BIN, Env:OMNILSP_ACCEPTANCE_NODE, Env:OMNILSP_ACCEPTANCE_NODE_SHA256, Env:OMNILSP_ACCEPTANCE_LOCKED_TOOLS, Env:OMNILSP_SEMANTIC_NODE_PATH, Env:OMNILSP_SEMANTIC_TYPESCRIPT_PATH, Env:OMNILSP_SEMANTIC_PYRIGHT_INTERNAL_PATH, Env:OMNILSP_SEMANTIC_PYRIGHT_VENDOR_PATH, Env:OMNILSP_SEMANTIC_PYTHON_PATH, Env:OMNILSP_CANDIDATE_SHA256, Env:OMNILSP_S21_GATE, Env:OMNILSP_S21_REPORT, Env:OMNILSP_S18_POLICY -ErrorAction SilentlyContinue
        Remove-Item Env:OMNILSP_RUST_SCIP_HELPER_PATH, Env:OMNILSP_RUST_SCIP_HELPER_VERSION, Env:OMNILSP_RUST_SCIP_HELPER_SHA256 -ErrorAction SilentlyContinue
        $env:PATH = $script:AcceptanceOriginalPath

        $checks.Add((Test-FastSourceTreeIdentity 'source-stability-after-tests'))
        $checks.Add((Test-CandidateBinaryIdentity $CandidateBinary $binaryHash))
        $decision = Get-OverallDecision $checks
        $summaryPath = Write-Summary $decision $checks $errors
        if ($decision -in @('passed', 'passed_with_performance_exception')) {
            $gates = [ordered]@{
                run_id = $RunId
                status = $decision
                s18_policy = $S18Policy
                release_assessment = $script:FastReleaseAssessment
                candidate_sha256 = $binaryHash
                soak_test_sha256 = (Get-FileHash -Algorithm SHA256 -LiteralPath $soakTest).Hash.ToLowerInvariant()
                acceptance_fingerprint = Get-AcceptanceFingerprint
                fast_evidence_sha256 = Get-FastEvidenceHashes
                evidence = $EvidenceDirectory
                completed_at = (Get-Date).ToUniversalTime().ToString('o')
            }
            $gates | ConvertTo-Json -Depth 16 | Set-Content -LiteralPath (Join-Path $EvidenceDirectory 'fast-gates.json') -Encoding utf8
        }
        Write-RequirementMatrix
        $matrixDecision = [string](Get-Content -LiteralPath (Join-Path $EvidenceDirectory 'requirement-matrix.json') -Raw | ConvertFrom-Json -ErrorAction Stop).decision
        if ($matrixDecision -cne $decision) {
            Remove-Item -LiteralPath (Join-Path $EvidenceDirectory 'fast-gates.json') -Force -ErrorAction SilentlyContinue
            $checks.Add([ordered]@{ id = 'requirement-matrix-consistency'; status = 'failed'; exitCode = 1; detail = "Fast decision '$decision' does not match requirement matrix '$matrixDecision'" })
            $decision = 'failed'
            $summaryPath = Write-Summary $decision $checks $errors
            Write-RequirementMatrix
            throw "Fast summary and requirement matrix decisions disagree. The Fast gate has been invalidated; evidence: $summaryPath"
        }
        if ($decision -notin @('passed', 'passed_with_performance_exception')) { throw "One or more fast release gates failed or were not verified. Evidence: $summaryPath" }
        Write-Output "Fast gates passed ($decision). Candidate SHA-256: $binaryHash"
        Write-Output "Evidence: $EvidenceDirectory"
    }
    'Soak30s' {
        Clear-SoakStageAttempt '30s'
        try {
            $soak = Invoke-Soak '30s' $RunId
            $null = Write-Summary 'passed' @([ordered]@{ id = 'soak-30s'; status = 'passed' })
            [ordered]@{ run_id = $RunId; stage = '30s'; status = 'passed'; candidate_sha256 = $soak.candidate_sha256; soak_test_sha256 = $soak.soak_test_sha256; report = $soak.report; report_sha256 = Get-FileSha256 (Join-Path $EvidenceDirectory $soak.report); finished_at = (Get-Date).ToUniversalTime().ToString('o') } | ConvertTo-Json -Depth 8 | Set-Content -LiteralPath (Join-Path $EvidenceDirectory 'soak-30s.json') -Encoding utf8
            Write-RequirementMatrix
        } catch {
            $failure = $_
            Write-SoakStageFailure '30s' $failure
            throw $failure
        }
    }
    'Soak10m' {
        Clear-SoakStageAttempt '10m'
        try {
            Assert-SoakStage '30s'
            $soak = Invoke-Soak '10m' $RunId
            $null = Write-Summary 'passed' @([ordered]@{ id = 'soak-10m'; status = 'passed' })
            [ordered]@{ run_id = $RunId; stage = '10m'; status = 'passed'; candidate_sha256 = $soak.candidate_sha256; soak_test_sha256 = $soak.soak_test_sha256; report = $soak.report; report_sha256 = Get-FileSha256 (Join-Path $EvidenceDirectory $soak.report); finished_at = (Get-Date).ToUniversalTime().ToString('o') } | ConvertTo-Json -Depth 8 | Set-Content -LiteralPath (Join-Path $EvidenceDirectory 'soak-10m.json') -Encoding utf8
            Write-RequirementMatrix
        } catch {
            $failure = $_
            Write-SoakStageFailure '10m' $failure
            throw $failure
        }
    }
    'Start1h' {
        $statusPath = Join-Path $EvidenceDirectory 'soak-worker-status.json'
        $startLockPath = Join-Path $EvidenceDirectory 'soak-worker-start.lock'
        $startLock = $null
        try { $startLock = [IO.File]::Open($startLockPath, [IO.FileMode]::OpenOrCreate, [IO.FileAccess]::ReadWrite, [IO.FileShare]::None) }
        catch { throw 'Another Start1h invocation is in progress for this evidence directory.' }
        try {
        if (Test-Path -LiteralPath $statusPath) {
            $previousStatus = Get-Content -LiteralPath $statusPath -Raw | ConvertFrom-Json
            if ($previousStatus.status -in @('starting', 'running')) {
                if (-not $previousStatus.worker_pid) { throw 'An unresolved soak worker startup exists; run Status to reconcile it before retrying.' }
                $previousProcess = Get-Process -Id ([int]$previousStatus.worker_pid) -ErrorAction SilentlyContinue
                if ($previousProcess -and $previousStatus.worker_process_started_at -and
                    $previousProcess.StartTime.ToUniversalTime().ToString('o') -eq $previousStatus.worker_process_started_at) {
                    throw "1h soak worker PID $($previousStatus.worker_pid) is already running for this evidence directory."
                }
            } elseif ($previousStatus.status -eq 'passed') {
                throw 'A 1h soak already passed for this evidence directory; use a new RunId for another candidate run.'
            }
            $archiveName = 'soak-worker-status-previous-' + (Get-Date).ToUniversalTime().ToString('yyyyMMddTHHmmssZ') + '-' + [guid]::NewGuid().ToString('N').Substring(0, 6) + '.json'
            Copy-Item -LiteralPath $statusPath -Destination (Join-Path $EvidenceDirectory $archiveName)
        }
        foreach ($stage in @('30s', '10m')) { Assert-SoakStage $stage }
        $gatePath = Join-Path $EvidenceDirectory 'fast-gates.json'
        $gates = Get-Content $gatePath -Raw | ConvertFrom-Json
        if ($gates.run_id -ne $RunId) { throw '1h soak requires the same RunId as the passed Fast gate.' }
        Assert-AcceptanceFingerprint $gates '1h startup'
        $reviewPath = Join-Path $EvidenceDirectory 'pre-soak-review.json'
        if (-not (Test-Path $reviewPath)) { throw '1h soak requires a passed independent Luna D pre-soak review in pre-soak-review.json.' }
        $review = Get-Content -LiteralPath $reviewPath -Raw | ConvertFrom-Json
        $candidateHash = (Get-FileHash -Algorithm SHA256 -LiteralPath $CandidateBinary).Hash.ToLowerInvariant()
        if ($candidateHash -ne $gates.candidate_sha256) { throw 'Candidate hash differs from the fast-gate candidate; start a new full run.' }
        $testExe = Join-Path $EvidenceDirectory 'soak.test.exe'
        $testHash = (Get-FileHash -Algorithm SHA256 -LiteralPath $testExe).Hash.ToLowerInvariant()
        Assert-PreSoakReview $review $gates $RunId $CandidateBinary $testExe $EvidenceDirectory $repoRoot
        Assert-AcceptanceFingerprint $gates '1h startup review validation'
        $workerArgs = @('-NoLogo', '-NoProfile', '-ExecutionPolicy', 'Bypass', '-File', $PSCommandPath, '-Phase', 'SoakWorker', '-RunId', $RunId, '-EvidenceDirectory', $EvidenceDirectory, '-CandidateBinary', $CandidateBinary, '-S18Policy', $S18Policy)
        $stdout = Join-Path $EvidenceDirectory 'soak-worker.stdout.log'
        $stderr = Join-Path $EvidenceDirectory 'soak-worker.stderr.log'
        $workerStatus = [ordered]@{ run_id = $RunId; worker_pid = $null; candidate_sha256 = $candidateHash; soak_test_sha256 = $testHash; acceptance_fingerprint_sha256 = $gates.acceptance_fingerprint.fingerprint_sha256; started_at = (Get-Date).ToUniversalTime().ToString('o'); duration = '1h'; status = 'starting'; error = $null }
        $workerStatus | ConvertTo-Json | Set-Content -LiteralPath $statusPath -Encoding utf8
        $worker = Start-Process -FilePath (Get-Command pwsh -ErrorAction Stop).Source -ArgumentList $workerArgs -WorkingDirectory $repoRoot -WindowStyle Hidden -RedirectStandardOutput $stdout -RedirectStandardError $stderr -PassThru
        $workerStatus.worker_pid = $worker.Id
        $workerStatus.worker_process_started_at = $worker.StartTime.ToUniversalTime().ToString('o')
        $workerStatus.status = 'running'
        $workerStatus | ConvertTo-Json | Set-Content -LiteralPath $statusPath -Encoding utf8
        Write-RequirementMatrix
        Write-Output "Started hidden 1h soak worker PID $($worker.Id). Do not sleep or restart the machine. Evidence: $EvidenceDirectory"
        } finally {
            if ($startLock) { $startLock.Dispose() }
        }
    }
    'SoakWorker' {
        $statusPath = Join-Path $EvidenceDirectory 'soak-worker-status.json'
        $workerOutcome = 'passed'
        $workerError = $null
        $workerStatus = $null
        try {
            for ($attempt = 0; $attempt -lt 120; $attempt++) {
                if (Test-Path -LiteralPath $statusPath) {
                    $pendingStatus = Get-Content -LiteralPath $statusPath -Raw | ConvertFrom-Json
                    if ($pendingStatus.status -eq 'running' -and [int]$pendingStatus.worker_pid -eq $PID -and
                        $pendingStatus.run_id -ceq $RunId -and [string]$pendingStatus.duration -ceq '1h') {
                        $workerStatus = $pendingStatus
                        break
                    }
                }
                Start-Sleep -Milliseconds 250
            }
            if (-not $workerStatus) { throw 'Parent did not publish this worker PID and start time within 30 seconds.' }
            $soak = Invoke-Soak '1h' $RunId
            $workerStatus.candidate_sha256 = $soak.candidate_sha256
            $workerStatus.soak_test_sha256 = $soak.soak_test_sha256
            $workerStatus.report = $soak.report
            $workerStatus.report_sha256 = Get-FileSha256 (Join-Path $EvidenceDirectory $soak.report)
        } catch {
            $workerOutcome = 'failed'
            $workerError = $_.Exception.Message
            throw
        } finally {
            if ($workerStatus) {
                $workerStatus.status = $workerOutcome
                if ($workerError) { $workerStatus.error = $workerError }
                $workerStatus.finished_at = (Get-Date).ToUniversalTime().ToString('o')
                $workerStatus | ConvertTo-Json -Depth 6 | Set-Content -LiteralPath $statusPath -Encoding utf8
            } elseif (Test-Path -LiteralPath $statusPath) {
                try {
                    $pendingStatus = Get-Content -LiteralPath $statusPath -Raw | ConvertFrom-Json -ErrorAction Stop
                    if ([int]$pendingStatus.worker_pid -eq $PID) {
                        $workerStatus = $pendingStatus
                        $workerStatus.status = 'failed'
                        $workerStatus.error = if ($workerError) { $workerError } else { 'worker failed before initializing its status record' }
                        $workerStatus.finished_at = (Get-Date).ToUniversalTime().ToString('o')
                        $workerStatus | ConvertTo-Json -Depth 6 | Set-Content -LiteralPath $statusPath -Encoding utf8
                    }
                } catch { Write-Warning "Could not reconcile failed soak worker status: $($_.Exception.Message)" }
            }
            try { Write-RequirementMatrix } catch { Write-Warning "Could not refresh requirement matrix after 1h worker completion: $($_.Exception.Message)" }
        }
    }
    'Status' {
        $statusPath = Join-Path $EvidenceDirectory 'soak-worker-status.json'
        if (-not (Test-Path $statusPath)) { throw "No soak worker record at $statusPath" }
        $workerStatus = Get-Content $statusPath -Raw | ConvertFrom-Json
        if ($workerStatus.run_id -ne $RunId) { throw 'Status RunId does not match the continuous soak worker.' }
        if ([string]$workerStatus.duration -cne '1h') { throw 'Status record is not for the required continuous 1h final stage.' }
        $workerProcess = if ($workerStatus.worker_pid) { Get-Process -Id $workerStatus.worker_pid -ErrorAction SilentlyContinue } else { $null }
        $processMatches = $workerProcess -and $workerStatus.worker_process_started_at -and
            ($workerProcess.StartTime.ToUniversalTime().ToString('o') -eq $workerStatus.worker_process_started_at)
        if ($workerStatus.status -in @('starting', 'running') -and -not $processMatches) {
            $workerStatus.status = 'invalidated'
            $workerStatus.error = 'worker exited without a final report; this continuous run cannot pass'
            $workerStatus.finished_at = (Get-Date).ToUniversalTime().ToString('o')
            $workerStatus | ConvertTo-Json -Depth 6 | Set-Content -LiteralPath $statusPath -Encoding utf8
        }
        if ($workerStatus.status -in @('starting', 'running')) {
            try {
                $gateData = Get-Content -LiteralPath (Join-Path $EvidenceDirectory 'fast-gates.json') -Raw | ConvertFrom-Json
                Assert-AcceptanceFingerprint $gateData '1h Status'
                $currentCandidateHash = Get-FileSha256 $CandidateBinary
                $currentTestHash = Get-FileSha256 (Join-Path $EvidenceDirectory 'soak.test.exe')
                if ($currentCandidateHash -ne $workerStatus.candidate_sha256 -or
                    $currentTestHash -ne $workerStatus.soak_test_sha256 -or
                    $gateData.acceptance_fingerprint.fingerprint_sha256 -ne $workerStatus.acceptance_fingerprint_sha256) {
                    throw 'frozen candidate, soak executable, or acceptance fingerprint changed during the run'
                }
            } catch {
                $workerStatus.status = 'invalidated'
                $workerStatus.error = 'continuous-run identity changed: ' + $_.Exception.Message
                $workerStatus.finished_at = (Get-Date).ToUniversalTime().ToString('o')
                $workerStatus | ConvertTo-Json -Depth 6 | Set-Content -LiteralPath $statusPath -Encoding utf8
            }
        }
        if ($workerStatus.status -eq 'passed') {
            $invalidReason = $null
            try {
                $started = [DateTimeOffset]::Parse($workerStatus.worker_process_started_at).ToUniversalTime()
                $finished = [DateTimeOffset]::Parse($workerStatus.finished_at).ToUniversalTime()
                if (($finished - $started) -lt [TimeSpan]::FromHours(1)) { $invalidReason = 'worker elapsed time is shorter than 1 continuous hour' }
                $testExe = Join-Path $EvidenceDirectory 'soak.test.exe'
                $testHash = (Get-FileHash -Algorithm SHA256 -LiteralPath $testExe).Hash.ToLowerInvariant()
                $candidateHash = (Get-FileHash -Algorithm SHA256 -LiteralPath $CandidateBinary).Hash.ToLowerInvariant()
                if ($testHash -ne $workerStatus.soak_test_sha256 -or $candidateHash -ne $workerStatus.candidate_sha256) { $invalidReason = 'candidate or soak test hash changed after the completed run' }
                $gateData = Get-Content -LiteralPath (Join-Path $EvidenceDirectory 'fast-gates.json') -Raw | ConvertFrom-Json
                Assert-AcceptanceFingerprint $gateData '1h Status'
                if ($gateData.run_id -ne $RunId -or $workerStatus.run_id -ne $RunId -or $workerStatus.status -ne 'passed') {
                    $invalidReason = 'completed worker status does not identify this Fast-gated run'
                }
                if ($candidateHash -ne $gateData.candidate_sha256 -or $testHash -ne $gateData.soak_test_sha256 -or
                    $workerStatus.candidate_sha256 -ne $candidateHash -or $workerStatus.soak_test_sha256 -ne $testHash -or
                    $workerStatus.acceptance_fingerprint_sha256 -ne $gateData.acceptance_fingerprint.fingerprint_sha256) {
                    $invalidReason = 'completed worker identity differs from the frozen Fast gate'
                }
                if (-not $workerStatus.report -or [IO.Path]::GetFileName([string]$workerStatus.report) -cne [string]$workerStatus.report) {
                    throw '1h worker report path is missing or not a file name inside the evidence directory.'
                }
                $reportPath = Join-Path $EvidenceDirectory $workerStatus.report
                $fullReportPath = [IO.Path]::GetFullPath($reportPath)
                $evidencePrefix = [IO.Path]::GetFullPath($EvidenceDirectory).TrimEnd([char[]]@([IO.Path]::DirectorySeparatorChar, [IO.Path]::AltDirectorySeparatorChar)) + [IO.Path]::DirectorySeparatorChar
                if (-not $fullReportPath.StartsWith($evidencePrefix, [StringComparison]::OrdinalIgnoreCase)) {
                    throw '1h report path escapes the evidence directory.'
                }
                $soakReport = Assert-StructuredReportIdentity $fullReportPath 'soak' $RunId $candidateHash [DateTime]::MinValue (Get-SoakRequiredCheckIds '1h') '1h'
                if ([string]$soakReport.decision -cne 'passed') { $invalidReason = '1h soak structured report is not passed' }
                if (-not $workerStatus.report_sha256 -or (Get-FileSha256 $fullReportPath) -ne $workerStatus.report_sha256) {
                    $invalidReason = '1h soak report hash differs from the worker completion record'
                }
            } catch {
                $invalidReason = 'completed soak evidence could not be validated: ' + $_.Exception.Message
            }
            if ($invalidReason) {
                $workerStatus.status = 'invalidated'
                $workerStatus.error = $invalidReason
                $workerStatus | ConvertTo-Json -Depth 6 | Set-Content -LiteralPath $statusPath -Encoding utf8
            }
        }
        Write-RequirementMatrix
        $workerStatus | ConvertTo-Json -Depth 6
    }
}
