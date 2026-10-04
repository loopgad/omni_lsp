$ErrorActionPreference = 'Stop'
$acceptancePath = Join-Path $PSScriptRoot 'acceptance.ps1'
$repoRoot = (Resolve-Path (Join-Path $PSScriptRoot '..')).Path
$tokens = $null
$parseErrors = $null
$ast = [System.Management.Automation.Language.Parser]::ParseFile($acceptancePath, [ref]$tokens, [ref]$parseErrors)
if ($parseErrors.Count -gt 0) {
    throw "acceptance.ps1 PowerShell parse failed: $($parseErrors[0].Message)"
}
foreach ($name in @('Get-ClientMatrixCellIds', 'Get-ClientMatrixSummaryIds', 'Get-ClientMatrixRequiredCheckIds', 'Assert-ClientMatrixContract', 'Get-S18StableExceptionLimit', 'Test-S18StableExceptionID', 'Test-S18PercentilesWithin', 'Test-S18FiniteNumber', 'Test-S18FiniteNonnegativeNumber', 'Test-S18IntegerValue', 'Get-S18PercentileNS', 'Get-S18ABBAEligibility', 'Get-S18StableABBAExceptions', 'Get-OptionalAssessmentCheckIDs', 'Assert-S18ReleaseAssessment', 'Assert-GateReleaseAssessment', 'Test-S18MatrixExceptionAccepted', 'Test-S18NormalizedObservation', 'Test-S18CompletionListEvidence', 'Test-S18EditUpdateEvidence', 'Assert-S19ReturnedLocationScale', 'Assert-S19ProcessTreeResourceEvidence', 'Test-S19ConcurrentRequestIdentity', 'Assert-S20KPIEvidence', 'Get-SoakRequiredCheckIds', 'Get-CorpusHash', 'Get-FileSha256', 'Test-CandidateBinaryIdentity', 'Test-ResolvedBinaryLock', 'Get-ResolvedBinaryEntry', 'Get-ResolvedBinaryPath', 'Test-ResolvedBinaryBinding', 'Get-RequiredResolvedBinaryNames', 'Get-WorkspaceContentHash', 'Test-FastSourceTreeIdentity', 'Test-AcceptanceFingerprintIdentity', 'Get-DirectoryContentHash', 'Get-FastEvidenceHashes', 'Assert-FastEvidenceHashes', 'Assert-StructuredReportIdentity', 'Assert-PreSoakReview', 'Get-EvidenceCheckStatus', 'Get-RequirementMatrixDecision', 'ConvertTo-S21Timestamp', 'Assert-S21ReportEvidence', 'Get-S21BucketStatus', 'Write-SoakStageFailure')) {
    $function = $ast.Find({ param($node) $node -is [System.Management.Automation.Language.FunctionDefinitionAst] -and $node.Name -eq $name }, $true)
    if (-not $function) { throw "acceptance.ps1 is missing validator helper '$name'." }
    Invoke-Expression $function.Extent.Text
}

$fingerprintFixture = 'a' * 64
if (-not (Test-AcceptanceFingerprintIdentity $fingerprintFixture $fingerprintFixture)) {
    throw 'An unchanged frozen acceptance fingerprint must remain valid.'
}
if ((Test-AcceptanceFingerprintIdentity $fingerprintFixture ('b' * 64)) -or
    (Test-AcceptanceFingerprintIdentity $null $fingerprintFixture) -or
    (Test-AcceptanceFingerprintIdentity 'missing' $fingerprintFixture)) {
    throw 'Changed or malformed acceptance fingerprints must invalidate frozen Fast evidence.'
}

$reparseFixture = Join-Path ([IO.Path]::GetTempPath()) ('omnilsp-directory-hash-reparse-' + [guid]::NewGuid().ToString('N'))
$reparseRoot = Join-Path $reparseFixture 'node_modules'
$outsideTarget = Join-Path $reparseFixture 'external-package'
try {
    New-Item -ItemType Directory -Force -Path $reparseRoot, $outsideTarget | Out-Null
    $outsideFile = Join-Path $outsideTarget 'index.js'
    Set-Content -LiteralPath $outsideFile -Value 'before' -Encoding utf8
    New-Item -ItemType Junction -Path (Join-Path $reparseRoot 'external-package') -Target $outsideTarget | Out-Null
    $externalLinkRejected = $false
    try { [void](Get-DirectoryContentHash $reparseRoot) } catch { $externalLinkRejected = $_.Exception.Message -like '*outside its dependency tree*' }
    if (-not $externalLinkRejected) {
        throw 'A dependency tree with an external reparse target must not produce an accepted fingerprint.'
    }
    Set-Content -LiteralPath $outsideFile -Value 'after' -Encoding utf8
    $mutatedExternalLinkRejected = $false
    try { [void](Get-DirectoryContentHash $reparseRoot) } catch { $mutatedExternalLinkRejected = $_.Exception.Message -like '*outside its dependency tree*' }
    if (-not $mutatedExternalLinkRejected) {
        throw 'Changing an external reparse target must never leave an accepted dependency fingerprint unchanged.'
    }

    Remove-Item -LiteralPath (Join-Path $reparseRoot 'external-package') -Force
    $insideTarget = Join-Path $reparseRoot 'inside-package'
    New-Item -ItemType Directory -Force -Path $insideTarget | Out-Null
    $insideFile = Join-Path $insideTarget 'index.js'
    Set-Content -LiteralPath $insideFile -Value 'inside before' -Encoding utf8
    New-Item -ItemType Junction -Path (Join-Path $reparseRoot 'inside-alias') -Target $insideTarget | Out-Null
    $insideBeforeHash = Get-DirectoryContentHash $reparseRoot
    Set-Content -LiteralPath $insideFile -Value 'inside after' -Encoding utf8
    $insideAfterHash = Get-DirectoryContentHash $reparseRoot
    if ($insideBeforeHash -ceq $insideAfterHash) {
        throw 'Changing an in-tree reparse target must change the dependency fingerprint.'
    }
} finally {
    if (Test-Path -LiteralPath $reparseFixture) {
        $resolvedReparseFixture = [IO.Path]::GetFullPath((Resolve-Path -LiteralPath $reparseFixture).Path)
        $tempPrefix = [IO.Path]::GetFullPath([IO.Path]::GetTempPath()).TrimEnd([IO.Path]::DirectorySeparatorChar, [IO.Path]::AltDirectorySeparatorChar) + [IO.Path]::DirectorySeparatorChar
        if (-not $resolvedReparseFixture.StartsWith($tempPrefix, [StringComparison]::OrdinalIgnoreCase) -or
            [IO.Path]::GetFileName($resolvedReparseFixture) -notlike 'omnilsp-directory-hash-reparse-*') {
            throw "Refusing to clean unexpected directory hash fixture '$resolvedReparseFixture'."
        }
        Remove-Item -LiteralPath $resolvedReparseFixture -Recurse -Force
    }
}

$exceptionMatrixRow = @{
    selected = $true; status = 'failed'; performance_exception_accepted = $true
    disposition = 'strict S18 failed; accepted under s18-evidence-qualified-v2 at P50/P95/P99=60/100/150 ms'
}
if ((Get-RequirementMatrixDecision @($exceptionMatrixRow)) -cne 'passed_with_performance_exception') {
    throw 'A validated strict S18 latency exception must remain a distinct passing matrix decision.'
}
$unacceptedMatrixRow = @{
    selected = $true; status = 'failed'; performance_exception_accepted = $false
    disposition = 'strict S18 failed; validated raw percentiles are within scoped operation limits, but the nonzero S18/S19 test-process exit blocks release acceptance'
}
if ((Get-RequirementMatrixDecision @($unacceptedMatrixRow)) -cne 'failed') {
    throw 'An individually bounded row with a nonzero process exit must remain a failed matrix row.'
}
if ((Get-RequirementMatrixDecision @($exceptionMatrixRow, @{ selected = $true; status = 'not_verified' })) -cne 'not_verified') {
    throw 'An accepted performance exception must not hide another unverified matrix row.'
}

$candidateIdentityPath = Join-Path ([IO.Path]::GetTempPath()) ('omnilsp-candidate-identity-' + [guid]::NewGuid().ToString('N') + '.bin')
try {
    [IO.File]::WriteAllBytes($candidateIdentityPath, [byte[]]@(1, 2, 3))
    $candidateIdentityHash = Get-FileSha256 $candidateIdentityPath
    if ((Test-CandidateBinaryIdentity $candidateIdentityPath $candidateIdentityHash).status -cne 'passed') {
        throw 'An unchanged candidate binary must pass the post-test identity check.'
    }
    [IO.File]::WriteAllBytes($candidateIdentityPath, [byte[]]@(1, 2, 4))
    if ((Test-CandidateBinaryIdentity $candidateIdentityPath $candidateIdentityHash).status -cne 'failed') {
        throw 'A candidate binary replaced after tests must fail the post-test identity check.'
    }
} finally {
    if (Test-Path -LiteralPath $candidateIdentityPath) { Remove-Item -LiteralPath $candidateIdentityPath -Force }
}

$resolvedBinaryFixturePath = Join-Path ([IO.Path]::GetTempPath()) ('omnilsp-resolved-binary-' + [guid]::NewGuid().ToString('N') + '.bin')
try {
    [IO.File]::WriteAllBytes($resolvedBinaryFixturePath, [byte[]]@(7, 8, 9))
    $resolvedBinaryHash = Get-FileSha256 $resolvedBinaryFixturePath
    $resolvedBinaryPin = [pscustomobject]@{
        fixture = [pscustomobject]@{ path = $resolvedBinaryFixturePath; sha256 = $resolvedBinaryHash }
    }
    if ((Test-ResolvedBinaryLock $resolvedBinaryPin).status -cne 'passed') {
        throw 'An unchanged resolved binary must match its locked path and SHA-256.'
    }
    $tamperedBinaryPin = [pscustomobject]@{
        fixture = [pscustomobject]@{ path = $resolvedBinaryFixturePath; sha256 = ('0' * 64) }
    }
    if ((Test-ResolvedBinaryLock $tamperedBinaryPin).status -cne 'failed') {
        throw 'A resolved binary with a changed SHA-256 must fail the tool lock.'
    }
    $missingBinaryPin = [pscustomobject]@{
        fixture = [pscustomobject]@{ path = ($resolvedBinaryFixturePath + '.missing'); sha256 = $resolvedBinaryHash }
    }
    if ((Test-ResolvedBinaryLock $missingBinaryPin).status -cne 'not_verified') {
        throw 'A missing pinned binary must remain not_verified.'
    }
if ((Test-ResolvedBinaryLock ([pscustomobject]@{})).status -cne 'failed') {
        throw 'An empty resolved-binary lock must fail closed.'
    }
} finally {
    if (Test-Path -LiteralPath $resolvedBinaryFixturePath) { Remove-Item -LiteralPath $resolvedBinaryFixturePath -Force }
}

$bindingFixturePath = Join-Path ([IO.Path]::GetTempPath()) ('omnilsp-resolved-binding-' + [guid]::NewGuid().ToString('N') + '.bin')
$bindingOtherPath = Join-Path ([IO.Path]::GetTempPath()) ('omnilsp-resolved-binding-other-' + [guid]::NewGuid().ToString('N') + '.bin')
try {
    [IO.File]::WriteAllBytes($bindingFixturePath, [byte[]]@(4, 5, 6))
    [IO.File]::WriteAllBytes($bindingOtherPath, [byte[]]@(4, 5, 6))
    $bindingHash = Get-FileSha256 $bindingFixturePath
    $bindingPin = [pscustomobject]@{ fixture = [pscustomobject]@{ path = $bindingFixturePath; sha256 = $bindingHash } }
    if ((Test-ResolvedBinaryLock $bindingPin @('fixture')).status -cne 'passed') {
        throw 'A required resolved binary entry must pass when its locked path and SHA-256 match.'
    }
    if ((Test-ResolvedBinaryBinding $bindingPin 'fixture' $bindingFixturePath).status -cne 'passed') {
        throw 'An actual executable at the locked path must pass the resolved binary binding contract.'
    }
    if ((Test-ResolvedBinaryBinding $bindingPin 'fixture' $bindingOtherPath).status -cne 'failed') {
        throw 'A same-content executable at a different path must fail the resolved binary binding contract.'
    }
    if ((Test-ResolvedBinaryLock $bindingPin @('missing-required-entry')).status -cne 'failed') {
        throw 'The resolved binary lock must fail when a required entry is absent.'
    }
} finally {
    foreach ($path in @($bindingFixturePath, $bindingOtherPath)) {
        if (Test-Path -LiteralPath $path) { Remove-Item -LiteralPath $path -Force }
    }
}

$clientMatrixClients = @('vscode', 'neovim', 'emacs-eglot', 'helix', 'zed', 'sublime-lsp')
$clientMatrixLanguages = @('go', 'c', 'cpp', 'rust', 'python', 'typescript', 'javascript')
$clientMatrixChecks = [Collections.Generic.List[object]]::new()
foreach ($id in @(Get-ClientMatrixRequiredCheckIds)) {
    $status = if ($id -match '^client/(helix|zed|sublime-lsp)/') { 'not_verified' } else { 'passed' }
    $clientMatrixChecks.Add([pscustomobject]@{ id = $id; status = $status })
}
$validClientMatrix = [pscustomobject]@{ checks = @($clientMatrixChecks) }
if (@(Get-ClientMatrixRequiredCheckIds).Count -ne 42 -or -not (Assert-ClientMatrixContract $validClientMatrix)) {
    throw 'The complete 42-cell client matrix must pass its structural contract while unsupported clients remain not_verified.'
}
$missingClientMatrixChecks = [Collections.Generic.List[object]]::new()
foreach ($check in $clientMatrixChecks) {
    if ($check.id -cne 'client/vscode/javascript') { $missingClientMatrixChecks.Add($check) }
}
$missingClientRejected = $false
try { Assert-ClientMatrixContract ([pscustomobject]@{ checks = @($missingClientMatrixChecks) }) } catch { $missingClientRejected = $true }
if (-not $missingClientRejected) { throw 'The client matrix contract accepted a missing language cell.' }
$falselyPassedNativeClient = [Collections.Generic.List[object]]::new()
foreach ($check in $clientMatrixChecks) {
    if ($check.id -ceq 'client/helix/go') {
        $falselyPassedNativeClient.Add([pscustomobject]@{ id = $check.id; status = 'passed' })
    } else {
        $falselyPassedNativeClient.Add($check)
    }
}
$falsePassRejected = $false
try { Assert-ClientMatrixContract ([pscustomobject]@{ checks = @($falselyPassedNativeClient) }) } catch { $falsePassRejected = $true }
if (-not $falsePassRejected) { throw 'The client matrix contract accepted a false passed status for a client without a native driver.' }

$previousWorkspaceRoot = $repoRoot
$previousEvidenceDirectory = $EvidenceDirectory
$contractWorkspace = Join-Path $repoRoot ('.tmp-acceptance-source-contract-' + [guid]::NewGuid().ToString('N'))
try {
    $repoRoot = $contractWorkspace
    $EvidenceDirectory = Join-Path $contractWorkspace 'acceptance-run'
    New-Item -ItemType Directory -Force -Path (Join-Path $contractWorkspace 'src'), $EvidenceDirectory | Out-Null
    $contractSource = Join-Path $contractWorkspace 'src/sample.go'
    Set-Content -LiteralPath $contractSource -Value 'package sample' -Encoding utf8
    $beforeEvidenceWrite = Get-WorkspaceContentHash
    Set-Content -LiteralPath (Join-Path $EvidenceDirectory 'Fast-summary.json') -Value '{"decision":"failed"}' -Encoding utf8
    if ((Get-WorkspaceContentHash) -cne $beforeEvidenceWrite) {
        throw 'A custom in-workspace evidence directory changed the workspace source fingerprint.'
    }
    $beforeCacheWrite = Get-WorkspaceContentHash
    $contractCache = Join-Path $contractWorkspace ('.tmp-gocache-contract-' + [guid]::NewGuid().ToString('N'))
    New-Item -ItemType Directory -Force -Path $contractCache | Out-Null
    Set-Content -LiteralPath (Join-Path $contractCache 'build-cache-entry') -Value 'generated cache' -Encoding utf8
    if ((Get-WorkspaceContentHash) -cne $beforeCacheWrite) {
        throw 'A generated .tmp-gocache-* directory changed the workspace source fingerprint.'
    }
    $script:FastSourceTreeHash = $beforeEvidenceWrite
    Set-Content -LiteralPath $contractSource -Value 'package sample // changed after build' -Encoding utf8
    $changedSource = Test-FastSourceTreeIdentity 'source-stability-contract'
    if ($changedSource.status -cne 'failed' -or $changedSource.expected_sha256 -ceq $changedSource.observed_sha256) {
        throw 'A source change after the frozen build snapshot was not rejected.'
    }
} finally {
    $repoRoot = $previousWorkspaceRoot
    $EvidenceDirectory = $previousEvidenceDirectory
    if (Test-Path -LiteralPath $contractWorkspace) {
        $resolvedWorkspace = (Resolve-Path -LiteralPath $contractWorkspace).Path
        $workspacePrefix = $previousWorkspaceRoot.TrimEnd([IO.Path]::DirectorySeparatorChar, [IO.Path]::AltDirectorySeparatorChar) + [IO.Path]::DirectorySeparatorChar
        if (-not $resolvedWorkspace.StartsWith($workspacePrefix, [StringComparison]::OrdinalIgnoreCase) -or
            [IO.Path]::GetFileName($resolvedWorkspace) -notlike '.tmp-acceptance-source-contract-*') {
            throw "Refusing to clean unexpected acceptance contract path '$resolvedWorkspace'."
        }
        Remove-Item -LiteralPath $resolvedWorkspace -Recurse -Force
    }
}

$contractEvidencePath = Join-Path ([IO.Path]::GetTempPath()) ('omnilsp-evidence-check-status-' + [guid]::NewGuid().ToString('N') + '.json')
try {
    [ordered]@{
        decision = 'failed'
        checks = @(
            @{ id = 'evidence/passed'; status = 'passed' }
            @{ id = 'evidence/failed'; status = 'failed' }
            @{ id = 'evidence/not-verified'; status = 'not_verified' }
        )
    } | ConvertTo-Json -Depth 4 | Set-Content -LiteralPath $contractEvidencePath -Encoding utf8
    if ((Get-EvidenceCheckStatus $contractEvidencePath @('evidence/passed')) -cne 'passed') {
        throw 'A failed report decision overwrote an individually passed evidence row.'
    }
    if ((Get-EvidenceCheckStatus $contractEvidencePath @('evidence/failed')) -cne 'failed') {
        throw 'An individually failed evidence row was not preserved.'
    }
    if ((Get-EvidenceCheckStatus $contractEvidencePath @('evidence/passed', 'evidence/missing')) -cne 'not_verified') {
        throw 'A missing evidence row was upgraded or masked by the overall report decision.'
    }
} finally {
    if (Test-Path -LiteralPath $contractEvidencePath) { Remove-Item -LiteralPath $contractEvidencePath -Force }
}

$requiredShortSoakChecks = @(Get-SoakRequiredCheckIds '30s')
foreach ($terminalId in @('soak/final-close', 'soak/coverage', 'soak/duration')) {
    if ($terminalId -notin $requiredShortSoakChecks) { throw "Soak structured gate omits terminal evidence '$terminalId'." }
}

$formulas = [ordered]@{
    precision = 'TP/(TP+FP)'
    recall = 'TP/(TP+FN)'
    falsePositiveRate = 'FP/(FP+TN)'
    falseNegativeRate = 'FN/(FN+TP)'
    refusalRate = 'refusals/requests'
    staleResultRejectionRate = 'stale_results_rejected/stale_results_presented_to_freshness_gate'
    wrongFileRate = 'wrong_file_locations/returned_locations'
    positionMappingFailureRate = 'position_mapping_failures/position_mapping_samples'
    editValidationFailureRate = 'edit_validation_failures/edit_validation_attempts'
}
$backends = @{
    go = 'golang'; c = 'ccls/clangd'; 'c++' = 'ccls/clangd'; rust = 'rustanalyzer'
    python = 'pyright'; typescript = 'typescript'; javascript = 'typescript'
}
$records = [Collections.Generic.List[object]]::new()
foreach ($language in @('Go', 'C', 'C++', 'Rust', 'Python', 'TypeScript', 'JavaScript')) {
    foreach ($feature in @('definition', 'references')) {
        $metrics = [ordered]@{}
        foreach ($name in $formulas.Keys) {
            $metrics[$name] = [ordered]@{ formula = $formulas[$name]; status = 'observed'; numerator = [long]0; denominator = [long]1; value = [double]0 }
        }
        $records.Add([ordered]@{
            dimension = [ordered]@{ feature = $feature; language = $language; backend = $backends[$language.ToLowerInvariant()]; oracle = 'pinned@1' }
            metrics = $metrics
        })
    }
}
$positionMetrics = [ordered]@{}
foreach ($name in $formulas.Keys) {
    $positionMetrics[$name] = [ordered]@{ formula = $formulas[$name]; status = 'observed'; numerator = [long]0; denominator = [long]1; value = [double]0 }
}
$records.Add([ordered]@{
    dimension = [ordered]@{ feature = 'position-mapping'; language = 'Go'; backend = 'golang'; oracle = 'gopls@1' }
    metrics = $positionMetrics
})
$renameMetrics = [ordered]@{}
foreach ($name in $formulas.Keys) {
    $renameMetrics[$name] = [ordered]@{ formula = $formulas[$name]; status = 'observed'; numerator = [long]0; denominator = [long]1; value = [double]0 }
}
$records.Add([ordered]@{
    dimension = [ordered]@{ feature = 'rename'; language = 'Go'; backend = 'golang'; oracle = 'gopls@1' }
    metrics = $renameMetrics
})

foreach ($record in $records) {
    $feature = [string]$record.dimension.feature
    $language = [string]$record.dimension.language
    $readOnly = ($feature -in @('definition', 'references') -and $language -in @('Go', 'C', 'C++', 'Rust', 'Python', 'TypeScript', 'JavaScript')) -or
        ($feature -ceq 'position-mapping' -and $language -ceq 'Go')
    if ($readOnly) {
        $metric = $record.metrics.editValidationFailureRate
        $metric.status = 'not_applicable'
        $metric.reason = 'read-only semantic locations do not produce a WorkspaceEdit'
        $metric.Remove('numerator')
        $metric.Remove('denominator')
        $metric.Remove('value')
    }
}

function New-Check($status, $records, $limitations = @()) {
    $notApplicable = @()
    foreach ($record in $records) {
        $metric = $record.metrics.editValidationFailureRate
        if ($metric.status -eq 'not_applicable') {
            $notApplicable += @{
                feature = [string]$record.dimension.feature
                language = [string]$record.dimension.language
                metric = 'editValidationFailureRate'
                reason = [string]$metric.reason
            }
        }
    }
    return ([ordered]@{
        id = 'contract-run/S20/accuracy-kpi'
        status = $status
        observed = [ordered]@{
            schema = 'omnilsp.s20.accuracy-kpi.v2'
            recordCount = [long]$records.Count
            records = @($records)
            countingUnit = 'synthetic unit test'
            limitations = @($limitations)
            notApplicable = @($notApplicable)
            duplicateDimensions = @()
        }
    } | ConvertTo-Json -Depth 32 | ConvertFrom-Json)
}

function Assert-Rejected([string] $Name, [scriptblock] $Action) {
    $rejected = $false
    try { & $Action } catch { $rejected = $true }
    if (-not $rejected) { throw "acceptance validator accepted invalid evidence: $Name" }
}

$producerReportPath = Join-Path ([IO.Path]::GetTempPath()) ('omnilsp-producer-exit-contract-' + [guid]::NewGuid().ToString('N') + '.json')
try {
    [ordered]@{
        schema_version = 1
        run_id = 'producer-exit-contract'
        candidate = @{ sha256 = 'a' * 64 }
        decision = 'not_verified'
    } | ConvertTo-Json -Depth 4 | Set-Content -LiteralPath $producerReportPath -Encoding utf8
    Assert-Rejected 'non-passed soak report with nonzero producer exit is failed' {
        Assert-StructuredReportIdentity $producerReportPath 'soak' 'producer-exit-contract' ('a' * 64) ([DateTime]::MinValue) @() '' 7
    }
    $unverifiedProducerReport = Assert-StructuredReportIdentity $producerReportPath 'soak' 'producer-exit-contract' ('a' * 64) ([DateTime]::MinValue) @() '' 0
    if ($unverifiedProducerReport.decision -cne 'not_verified') {
        throw 'A not_verified report with a zero producer exit must remain not_verified.'
    }
} finally {
    if (Test-Path -LiteralPath $producerReportPath) { Remove-Item -LiteralPath $producerReportPath -Force }
}

$previousReviewEvidenceDirectory = $EvidenceDirectory
$reviewEvidenceParent = Join-Path $repoRoot 'test/acceptance/evidence'
$reviewContractRoot = Join-Path $reviewEvidenceParent ('pre-soak-review-contract-' + [guid]::NewGuid().ToString('N'))
try {
    New-Item -ItemType Directory -Force -Path $reviewContractRoot | Out-Null
    $EvidenceDirectory = $reviewContractRoot
    $reviewCandidatePath = Join-Path $reviewContractRoot 'candidate.exe'
    $reviewSoakTestPath = Join-Path $reviewContractRoot 'soak.test.exe'
    Set-Content -LiteralPath $reviewCandidatePath -Value 'review candidate' -Encoding utf8
    Set-Content -LiteralPath $reviewSoakTestPath -Value 'review soak test' -Encoding utf8
    foreach ($name in @('fast-gates.json', 'soak-30s.json', 'soak-30s-report.json', 'soak-10m.json', 'soak-10m-report.json')) {
        Set-Content -LiteralPath (Join-Path $reviewContractRoot $name) -Value "current:$name" -Encoding utf8
    }
    $reviewRunId = 'pre-soak-review-contract'
    $reviewCandidateHash = Get-FileSha256 $reviewCandidatePath
    $reviewSoakTestHash = Get-FileSha256 $reviewSoakTestPath
    $reviewFingerprintHash = 'c' * 64
    $reviewWorkspaceHash = Get-WorkspaceContentHash
    $reviewGates = [ordered]@{
        run_id = $reviewRunId
        candidate_sha256 = $reviewCandidateHash
        soak_test_sha256 = $reviewSoakTestHash
        acceptance_fingerprint = @{ fingerprint_sha256 = $reviewFingerprintHash; workspace_sha256 = $reviewWorkspaceHash }
    } | ConvertTo-Json -Depth 5 | ConvertFrom-Json
    $reviewEvidence = [ordered]@{
        run_id = $reviewRunId
        candidate_sha256 = $reviewCandidateHash
        acceptance_fingerprint_sha256 = $reviewFingerprintHash
        workspace_sha256 = $reviewWorkspaceHash
        fast_gates_sha256 = Get-FileSha256 (Join-Path $reviewContractRoot 'fast-gates.json')
        soak_test_sha256 = $reviewSoakTestHash
        soak_test_source_sha256 = Get-FileSha256 (Join-Path $repoRoot 'test/soak/stdio_soak_test.go')
        acceptance_spec_sha256 = Get-FileSha256 (Join-Path $repoRoot 'docs/acceptance.md')
        soak_30s_stage_sha256 = Get-FileSha256 (Join-Path $reviewContractRoot 'soak-30s.json')
        soak_30s_report_sha256 = Get-FileSha256 (Join-Path $reviewContractRoot 'soak-30s-report.json')
        soak_10m_stage_sha256 = Get-FileSha256 (Join-Path $reviewContractRoot 'soak-10m.json')
        soak_10m_report_sha256 = Get-FileSha256 (Join-Path $reviewContractRoot 'soak-10m-report.json')
    }
    $reviewChecklist = [ordered]@{
        integrated_source = @{ status = 'passed'; evidence = @('workspace_sha256') }
        test_validity = @{ status = 'passed'; evidence = @('soak_test_sha256', 'soak_30s_report_sha256', 'soak_10m_report_sha256') }
        test_to_spec_mapping = @{ status = 'passed'; evidence = @('soak_test_source_sha256', 'acceptance_spec_sha256') }
        current_evidence = @{ status = 'passed'; evidence = @('fast_gates_sha256', 'soak_30s_stage_sha256', 'soak_30s_report_sha256', 'soak_10m_stage_sha256', 'soak_10m_report_sha256') }
    }
    $validReview = [ordered]@{
        schema_version = 1
        decision = 'passed'
        run_id = $reviewRunId
        candidate_sha256 = $reviewCandidateHash
        acceptance_fingerprint_sha256 = $reviewFingerprintHash
        reviewer = @{ id = 'luna-d'; name = 'Luna D'; independent = $true }
        checklist = $reviewChecklist
        evidence = $reviewEvidence
    } | ConvertTo-Json -Depth 12 | ConvertFrom-Json
    Assert-PreSoakReview $validReview $reviewGates $reviewRunId $reviewCandidatePath $reviewSoakTestPath $reviewContractRoot $repoRoot

    $wrongReviewer = $validReview | ConvertTo-Json -Depth 12 | ConvertFrom-Json
    $wrongReviewer.reviewer.independent = $false
    Assert-Rejected 'pre-soak review requires an independent Luna D reviewer' {
        Assert-PreSoakReview $wrongReviewer $reviewGates $reviewRunId $reviewCandidatePath $reviewSoakTestPath $reviewContractRoot $repoRoot
    }
    $incompleteChecklist = $validReview | ConvertTo-Json -Depth 12 | ConvertFrom-Json
    $incompleteChecklist.checklist.test_validity.status = 'not_verified'
    Assert-Rejected 'pre-soak review requires every checklist area to pass' {
        Assert-PreSoakReview $incompleteChecklist $reviewGates $reviewRunId $reviewCandidatePath $reviewSoakTestPath $reviewContractRoot $repoRoot
    }
    $missingChecklistEvidence = $validReview | ConvertTo-Json -Depth 12 | ConvertFrom-Json
    $missingChecklistEvidence.checklist.current_evidence.evidence = @('fast_gates_sha256')
    Assert-Rejected 'pre-soak checklist must cite every required evidence artifact' {
        Assert-PreSoakReview $missingChecklistEvidence $reviewGates $reviewRunId $reviewCandidatePath $reviewSoakTestPath $reviewContractRoot $repoRoot
    }
    $wrongRunReview = $validReview | ConvertTo-Json -Depth 12 | ConvertFrom-Json
    $wrongRunReview.run_id = 'different-run'
    Assert-Rejected 'pre-soak review is bound to the current run ID' {
        Assert-PreSoakReview $wrongRunReview $reviewGates $reviewRunId $reviewCandidatePath $reviewSoakTestPath $reviewContractRoot $repoRoot
    }
    $wrongCandidateReview = $validReview | ConvertTo-Json -Depth 12 | ConvertFrom-Json
    $wrongCandidateReview.evidence.candidate_sha256 = 'd' * 64
    Assert-Rejected 'pre-soak review evidence is bound to the current candidate hash' {
        Assert-PreSoakReview $wrongCandidateReview $reviewGates $reviewRunId $reviewCandidatePath $reviewSoakTestPath $reviewContractRoot $repoRoot
    }
    $wrongFingerprintReview = $validReview | ConvertTo-Json -Depth 12 | ConvertFrom-Json
    $wrongFingerprintReview.acceptance_fingerprint_sha256 = 'e' * 64
    Assert-Rejected 'pre-soak review is bound to the current acceptance fingerprint' {
        Assert-PreSoakReview $wrongFingerprintReview $reviewGates $reviewRunId $reviewCandidatePath $reviewSoakTestPath $reviewContractRoot $repoRoot
    }
    $staleEvidenceReview = $validReview | ConvertTo-Json -Depth 12 | ConvertFrom-Json
    $staleEvidenceReview.evidence.soak_10m_report_sha256 = 'f' * 64
    Assert-Rejected 'pre-soak review must cite current preflight report hashes' {
        Assert-PreSoakReview $staleEvidenceReview $reviewGates $reviewRunId $reviewCandidatePath $reviewSoakTestPath $reviewContractRoot $repoRoot
    }
} finally {
    if ($previousReviewEvidenceDirectory) { $EvidenceDirectory = $previousReviewEvidenceDirectory } else { Remove-Variable EvidenceDirectory -ErrorAction SilentlyContinue }
    if (Test-Path -LiteralPath $reviewContractRoot) {
        $resolvedReviewRoot = (Resolve-Path -LiteralPath $reviewContractRoot).Path
        $resolvedEvidenceParent = (Resolve-Path -LiteralPath $reviewEvidenceParent).Path
        $evidencePrefix = $resolvedEvidenceParent.TrimEnd([IO.Path]::DirectorySeparatorChar, [IO.Path]::AltDirectorySeparatorChar) + [IO.Path]::DirectorySeparatorChar
        if (-not $resolvedReviewRoot.StartsWith($evidencePrefix, [StringComparison]::OrdinalIgnoreCase) -or
            [IO.Path]::GetFileName($resolvedReviewRoot) -notlike 'pre-soak-review-contract-*') {
            throw "Refusing to clean unexpected review contract path '$resolvedReviewRoot'."
        }
        Remove-Item -LiteralPath $resolvedReviewRoot -Recurse -Force
    }
}

$complete = New-Check 'passed' $records
Assert-S20KPIEvidence $complete 'contract-run'

$stableSyntaxCeiling = Get-S18StableExceptionLimit 'c' 'syntax_update_after_edit'
$stableCompletionCeiling = Get-S18StableExceptionLimit 'cpp' 'completion_first_usable'
if (-not (Test-S18PercentilesWithin ([double[]]@(60, 100, 150)) ([double[]]$stableSyntaxCeiling)) -or
    -not (Test-S18PercentilesWithin ([double[]]@(80, 100, 125)) ([double[]]$stableCompletionCeiling))) {
    throw 'The exact C/C++ stable v2 completion and syntax ceilings must be accepted.'
}

function New-S18ABBAFixture([string] $CheckId, [double] $CandidateNS, [double] $UpstreamNS, [double] $OverheadNS) {
    $match = [regex]::Match($CheckId, '^.+/S18/(c|cpp)/(completion_first_usable|syntax_update_after_edit)$')
    if (-not $match.Success) { throw "Invalid test fixture ID '$CheckId'." }
    $language = $match.Groups[1].Value
    $operation = $match.Groups[2].Value
    $method = if ($operation -ceq 'completion_first_usable') { 'textDocument/completion' } else { 'textDocument/documentSymbol' }
    $raw = [double[]]::new(6000)
    for ($index = 0; $index -lt $raw.Count; $index++) { $raw[$index] = $CandidateNS }
    $rounds = [Collections.Generic.List[object]]::new()
    for ($roundIndex = 0; $roundIndex -lt 3; $roundIndex++) {
        $legs = [Collections.Generic.List[object]]::new()
        $firstStart = $roundIndex * 2000
        $secondStart = $firstStart + 1000
        foreach ($candidateLeg in @(@{ position = 0; start = $firstStart }, @{ position = 3; start = $secondStart })) {
            $requestIDs = [string[]]::new(1000)
            $writeRaw = [double[]]::new(1000)
            $waitRaw = [double[]]::new(1000)
            for ($sampleIndex = 0; $sampleIndex -lt 1000; $sampleIndex++) {
                $requestIDs[$sampleIndex] = "$CheckId/request-$($candidateLeg.start + $sampleIndex)"
                $writeRaw[$sampleIndex] = $CandidateNS - $OverheadNS
                $waitRaw[$sampleIndex] = 0
            }
            $legs.Add([pscustomobject]@{
                position = [long]$candidateLeg.position; role = 'candidate'
                sample_start = [long]$candidateLeg.start; sample_count = [long]1000
                request_ids = $requestIDs; write_raw_ns = $writeRaw; wait_raw_ns = $waitRaw
                raw_ns = @()
            })
        }
        $upstreamRaw1 = [double[]]::new(1000)
        $upstreamRaw2 = [double[]]::new(1000)
        for ($sampleIndex = 0; $sampleIndex -lt 1000; $sampleIndex++) {
            $upstreamRaw1[$sampleIndex] = $UpstreamNS
            $upstreamRaw2[$sampleIndex] = $UpstreamNS
        }
        $legs.Insert(1, [pscustomobject]@{
            position = [long]1; role = 'upstream'; sample_start = [long]0; sample_count = [long]0
            request_ids = @(); write_raw_ns = @(); wait_raw_ns = @(); raw_ns = $upstreamRaw1
        })
        $legs.Insert(2, [pscustomobject]@{
            position = [long]2; role = 'upstream'; sample_start = [long]0; sample_count = [long]0
            request_ids = @(); write_raw_ns = @(); wait_raw_ns = @(); raw_ns = $upstreamRaw2
        })
        $rounds.Add([pscustomobject]@{
            index = [long]$roundIndex; order = @('candidate', 'upstream', 'upstream', 'candidate'); legs = $legs.ToArray()
        })
    }
    $evidence = [pscustomobject]@{
        check_id = $CheckId; fixture_id = $language; operation = $operation; method = $method; rounds = $rounds.ToArray()
    }
    $sample = [pscustomobject]@{
        id = $CheckId; unit = 'ns/op'; raw = $raw; p50 = $CandidateNS; p95 = $CandidateNS; p99 = $CandidateNS; warmup = [long]32
    }
    $check = [pscustomobject]@{
        id = $CheckId; status = 'failed'; observed = [pscustomobject]@{ sample_count = [long]6000; p50_ns = $CandidateNS; p95_ns = $CandidateNS; p99_ns = $CandidateNS }
        threshold = [pscustomobject]@{ p50_ms = [long]40; p95_ms = [long]120; p99_ms = [long]250 }
    }
    if ($operation -ceq 'syntax_update_after_edit') {
        $check.threshold.p50_ms = [long]15; $check.threshold.p95_ms = [long]50; $check.threshold.p99_ms = [long]100
    }
    return [pscustomobject]@{ evidence = $evidence; sample = $sample; check = $check }
}

$completionId = 'contract-run/S18/c/completion_first_usable'
$validCompletionABBA = New-S18ABBAFixture $completionId 80000000 75000000 5000000
$eligibleCompletionABBA = Get-S18ABBAEligibility $completionId $validCompletionABBA.check $validCompletionABBA.sample $validCompletionABBA.evidence
if (-not $eligibleCompletionABBA.eligible -or $eligibleCompletionABBA.candidate_percentiles_ns[0] -ne 80000000) {
    throw 'Stable v2 must recompute a qualifying C/C++ completion exception from the ABBA raw samples.'
}
$omittedArrayABBA = New-S18ABBAFixture $completionId 80000000 75000000 5000000
foreach ($round in $omittedArrayABBA.evidence.rounds) {
    foreach ($leg in $round.legs) {
        if ($leg.role -ceq 'candidate') {
            $leg.PSObject.Properties.Remove('raw_ns')
        } else {
            $leg.PSObject.Properties.Remove('request_ids')
            $leg.PSObject.Properties.Remove('write_raw_ns')
            $leg.PSObject.Properties.Remove('wait_raw_ns')
        }
    }
}
$omittedArrayJSON = ConvertTo-Json -InputObject $omittedArrayABBA -Depth 32 -Compress
$omittedArrayRoundTrip = ConvertFrom-Json -InputObject $omittedArrayJSON -Depth 32
$omittedArrayResult = Get-S18ABBAEligibility $completionId $omittedArrayRoundTrip.check $omittedArrayRoundTrip.sample $omittedArrayRoundTrip.evidence
if (-not $omittedArrayResult.eligible) {
    throw 'ABBA validation must accept Go omitempty output for unused role-specific arrays after a JSON round trip.'
}
$strictPassingABBA = New-S18ABBAFixture $completionId 40000000 35000000 1000000
$strictPassingResult = Get-S18ABBAEligibility $completionId $strictPassingABBA.check $strictPassingABBA.sample $strictPassingABBA.evidence
if ($strictPassingResult.eligible -or $strictPassingResult.strict_overage) {
    throw 'A strict-threshold pass must not be misreported as a performance exception.'
}
$overheadFailureABBA = New-S18ABBAFixture $completionId 80000000 75000000 6000000
$overheadFailureResult = Get-S18ABBAEligibility $completionId $overheadFailureABBA.check $overheadFailureABBA.sample $overheadFailureABBA.evidence
if ($overheadFailureResult.eligible) { throw 'Candidate overhead above its P50 cap must block an exception.' }
$upstreamStrictFailureABBA = New-S18ABBAFixture $completionId 41000000 39000000 1000000
$upstreamStrictFailureResult = Get-S18ABBAEligibility $completionId $upstreamStrictFailureABBA.check $upstreamStrictFailureABBA.sample $upstreamStrictFailureABBA.evidence
if ($upstreamStrictFailureResult.eligible) { throw 'A candidate strict overage cannot qualify when the paired upstream percentile meets the strict threshold.' }
$oneBadRoundABBA = New-S18ABBAFixture $completionId 80000000 75000000 5000000
foreach ($legIndex in @(1, 2)) {
    for ($sampleIndex = 0; $sampleIndex -lt 1000; $sampleIndex++) {
        $oneBadRoundABBA.evidence.rounds[1].legs[$legIndex].raw_ns[$sampleIndex] = 30000000
    }
}
$oneBadRoundResult = Get-S18ABBAEligibility $completionId $oneBadRoundABBA.check $oneBadRoundABBA.sample $oneBadRoundABBA.evidence
if (-not $oneBadRoundResult.eligible) {
    throw 'ABBA qualification must use the three-round aggregate when an individual round differs.'
}
$strictPassingAboveExceptionCeiling = New-S18ABBAFixture $completionId 30000000 20000000 1000000
for ($roundIndex = 0; $roundIndex -lt 3; $roundIndex++) {
    foreach ($legIndex in @(0, 3)) {
        $leg = $strictPassingAboveExceptionCeiling.evidence.rounds[$roundIndex].legs[$legIndex]
        for ($sampleIndex = 900; $sampleIndex -lt 1000; $sampleIndex++) {
            $strictPassingAboveExceptionCeiling.sample.raw[$leg.sample_start + $sampleIndex] = 110000000
            $leg.write_raw_ns[$sampleIndex] = 109000000
        }
    }
}
$strictPassingAboveExceptionResult = Get-S18ABBAEligibility $completionId $strictPassingAboveExceptionCeiling.check $strictPassingAboveExceptionCeiling.sample $strictPassingAboveExceptionCeiling.evidence
if ($strictPassingAboveExceptionResult.strict_overage -or $strictPassingAboveExceptionResult.eligible) {
    throw 'A strict-pass aggregate above an exception-only ceiling must remain a strict pass, not require an exception.'
}
$mixedStrictAndExceptionCeiling = New-S18ABBAFixture $completionId 50000000 45000000 5000000
for ($roundIndex = 0; $roundIndex -lt 3; $roundIndex++) {
    foreach ($legIndex in @(0, 3)) {
        $leg = $mixedStrictAndExceptionCeiling.evidence.rounds[$roundIndex].legs[$legIndex]
        for ($sampleIndex = 0; $sampleIndex -lt 1000; $sampleIndex++) {
            $absoluteIndex = [int]$leg.sample_start + $sampleIndex
            if ($absoluteIndex -ge 3000) {
                $mixedStrictAndExceptionCeiling.sample.raw[$absoluteIndex] = 110000000
                $leg.write_raw_ns[$sampleIndex] = 105000000
            }
        }
    }
}
$mixedStrictAndExceptionCeilingResult = Get-S18ABBAEligibility $completionId $mixedStrictAndExceptionCeiling.check $mixedStrictAndExceptionCeiling.sample $mixedStrictAndExceptionCeiling.evidence
if (-not $mixedStrictAndExceptionCeilingResult.eligible -or -not $mixedStrictAndExceptionCeilingResult.strict_overage -or
    $mixedStrictAndExceptionCeilingResult.candidate_percentiles_ns[1] -le 100000000) {
    throw 'Exception ceilings must constrain only percentiles that also exceed their original strict threshold.'
}
Assert-Rejected 'negative candidate overhead must fail closed' {
    $negativeOverhead = New-S18ABBAFixture $completionId 80000000 75000000 81000000
    Get-S18ABBAEligibility $completionId $negativeOverhead.check $negativeOverhead.sample $negativeOverhead.evidence
}
Assert-Rejected 'ABBA leg order must be C/U/U/C' {
    $wrongOrder = New-S18ABBAFixture $completionId 80000000 75000000 5000000
    $wrongOrder.evidence.rounds[0].order = @('upstream', 'candidate', 'upstream', 'candidate')
    Get-S18ABBAEligibility $completionId $wrongOrder.check $wrongOrder.sample $wrongOrder.evidence
}
Assert-Rejected 'candidate request IDs must be unique' {
    $duplicateRequest = New-S18ABBAFixture $completionId 80000000 75000000 5000000
    $duplicateRequest.evidence.rounds[0].legs[3].request_ids[0] = $duplicateRequest.evidence.rounds[0].legs[0].request_ids[0]
    Get-S18ABBAEligibility $completionId $duplicateRequest.check $duplicateRequest.sample $duplicateRequest.evidence
}
$stableABBAIDs = @(
    'contract-run/S18/c/completion_first_usable',
    'contract-run/S18/c/syntax_update_after_edit',
    'contract-run/S18/cpp/completion_first_usable',
    'contract-run/S18/cpp/syntax_update_after_edit'
)
$stableABBAEvidence = [pscustomobject]@{}
$stableABBASamples = @{}
$stableABBAChecks = @{}
foreach ($id in $stableABBAIDs) {
    if ($id.EndsWith('completion_first_usable', [StringComparison]::Ordinal)) {
        $fixture = New-S18ABBAFixture $id 80000000 75000000 5000000
    } else {
        $fixture = New-S18ABBAFixture $id 60000000 55000000 5000000
    }
    Add-Member -InputObject $stableABBAEvidence -MemberType NoteProperty -Name $id -Value $fixture.evidence
    $stableABBASamples[$id] = $fixture.sample
    $stableABBAChecks[$id] = $fixture.check
}
$allStableABBAExceptions = @(Get-S18StableABBAExceptions ([pscustomobject]@{ abba_evidence = $stableABBAEvidence }) 'contract-run' $stableABBASamples $stableABBAChecks $stableABBAIDs)
if ($allStableABBAExceptions.Count -ne 4 -or @($stableABBAIDs | Where-Object { $_ -notin $allStableABBAExceptions }).Count -gt 0) {
    throw 'Stable v2 must accept only raw-evidence-qualified exceptions from the complete four-check C/C++ scope.'
}
$matrixExceptionAssessment = [pscustomobject]@{
    policy_id = 's18-evidence-qualified-v2'; execution_status = 'completed'; decision = 'passed_with_performance_exception'
    exception_check_ids = $allStableABBAExceptions; bounded_exception_check_ids = $allStableABBAExceptions
}
if (-not (Test-S18MatrixExceptionAccepted $stableABBAIDs[0] $matrixExceptionAssessment $allStableABBAExceptions 0) -or
    (Test-S18MatrixExceptionAccepted $stableABBAIDs[0] $matrixExceptionAssessment $allStableABBAExceptions 1) -or
    (Test-S18MatrixExceptionAccepted $stableABBAIDs[0] $matrixExceptionAssessment @($stableABBAIDs[1..3]) 0)) {
    throw 'Requirement matrices must retain raw-validated S18 exceptions, and reject nonzero processes or an unvalidated exception row.'
}
$validatedExceptionMatrixRow = @{
    selected = $true; status = 'failed'
    performance_exception_accepted = Test-S18MatrixExceptionAccepted $stableABBAIDs[0] $matrixExceptionAssessment $allStableABBAExceptions 0
}
$missingClientMatrixRow = @{ selected = $true; status = 'not_verified'; performance_exception_accepted = $false }
if ((Get-RequirementMatrixDecision @($validatedExceptionMatrixRow, $missingClientMatrixRow)) -cne 'not_verified') {
    throw 'A validated S18 exception must not hide an unrelated not-verified client gate when no frozen Fast gate exists yet.'
}
$strictPassEvidence = [pscustomobject]@{}
$strictPassSamples = @{}
$strictPassChecks = @{}
foreach ($id in $stableABBAIDs) {
    if ($id.EndsWith('completion_first_usable', [StringComparison]::Ordinal)) {
        $fixture = New-S18ABBAFixture $id 40000000 35000000 1000000
    } else {
        $fixture = New-S18ABBAFixture $id 15000000 14000000 1000000
    }
    Add-Member -InputObject $strictPassEvidence -MemberType NoteProperty -Name $id -Value $fixture.evidence
    $strictPassSamples[$id] = $fixture.sample
    $strictPassChecks[$id] = $fixture.check
}
if (@(Get-S18StableABBAExceptions ([pscustomobject]@{ abba_evidence = $strictPassEvidence }) 'contract-run' $strictPassSamples $strictPassChecks @()).Count -ne 0) {
    throw 'Complete ABBA evidence for strict passing results must not create performance exceptions.'
}
$strictPassOverBudget = $strictPassEvidence.PSObject.Properties[$stableABBAIDs[0]].Value
foreach ($round in $strictPassOverBudget.rounds) {
    foreach ($leg in $round.legs) {
        if ($leg.role -ceq 'candidate') {
            for ($sampleIndex = 0; $sampleIndex -lt $leg.write_raw_ns.Count; $sampleIndex++) {
                $leg.write_raw_ns[$sampleIndex] = 34000000
            }
        }
    }
}
Assert-Rejected 'stable strict-pass ABBA must still satisfy the correlated overhead budget' {
    Get-S18StableABBAExceptions ([pscustomobject]@{ abba_evidence = $strictPassEvidence }) 'contract-run' $strictPassSamples $strictPassChecks @()
}
Assert-Rejected 'stable v2 cannot pass with skipped language or missing ABBA evidence' {
    $missingABBA = [pscustomobject]@{ abba_evidence = $stableABBAEvidence }
    $missingABBA.abba_evidence.PSObject.Properties.Remove($stableABBAIDs[3])
    Get-S18StableABBAExceptions $missingABBA 'contract-run' $stableABBASamples $stableABBAChecks $stableABBAIDs
}

$overCeiling = Test-S18PercentilesWithin ([double[]]@(60.001, 100, 150)) ([double[]]$stableSyntaxCeiling)
$overCompletionCeiling = Test-S18PercentilesWithin ([double[]]@(80, 100.001, 125)) ([double[]]$stableCompletionCeiling)
$wrongLanguageLimit = Get-S18StableExceptionLimit 'go' 'syntax_update_after_edit'
$wrongOperationLimit = Get-S18StableExceptionLimit 'cpp' 'hot_hover'
if ($overCeiling -or $overCompletionCeiling -or $null -ne $wrongLanguageLimit -or $null -ne $wrongOperationLimit -or
    (Test-S18StableExceptionID 'contract-run/S18/go/completion_first_usable' 'contract-run') -or
    (Test-S18StableExceptionID 'contract-run/S18/c/completion_first_usable/extra' 'contract-run')) {
    throw 'An over-ceiling value or out-of-scope language/operation received a performance exception.'
}
$exceptionId = 'contract-run/S18/c/syntax_update_after_edit'
$exceptionReport = [ordered]@{
    run_id = 'contract-run'; decision = 'failed'; errors = @(); skips = @()
    release_assessment = @{
        policy_id = 's18-evidence-qualified-v2'; execution_status = 'completed'
        bounded_exception_check_ids = @($exceptionId)
        decision = 'passed_with_performance_exception'; exception_check_ids = @($exceptionId)
    }
}
if ((Assert-S18ReleaseAssessment $exceptionReport @($exceptionId) @($exceptionId) 'stable' 0) -cne 'passed_with_performance_exception') {
    throw 'A complete, in-bound stable performance exception did not retain its distinct decision.'
}
Assert-Rejected 'performance exceptions cannot hide report errors' {
    $withError = $exceptionReport | ConvertTo-Json -Depth 5 | ConvertFrom-Json
    $withError.errors = @('semantic evidence failed')
    Assert-S18ReleaseAssessment $withError @($exceptionId) @($exceptionId) 'stable' 0
}
Assert-Rejected 'performance exceptions cannot hide skipped tests' {
    $withSkip = $exceptionReport | ConvertTo-Json -Depth 5 | ConvertFrom-Json
    $withSkip.skips = @('upstream was unavailable')
    Assert-S18ReleaseAssessment $withSkip @($exceptionId) @($exceptionId) 'stable' 0
}
$fourExceptionIds = @(
    'contract-run/S18/c/completion_first_usable',
    'contract-run/S18/c/syntax_update_after_edit',
    'contract-run/S18/cpp/completion_first_usable',
    'contract-run/S18/cpp/syntax_update_after_edit'
)
$fourExceptionReport = [ordered]@{
    run_id = 'contract-run'; decision = 'failed'; errors = @(); skips = @()
    release_assessment = @{
        policy_id = 's18-evidence-qualified-v2'; execution_status = 'completed'
        bounded_exception_check_ids = $fourExceptionIds
        decision = 'passed_with_performance_exception'; exception_check_ids = $fourExceptionIds
    }
}
if ((Assert-S18ReleaseAssessment $fourExceptionReport $fourExceptionIds $fourExceptionIds 'stable' 0) -cne 'passed_with_performance_exception') {
    throw 'Stable v2 must permit at most the four C/C++ completion and syntax rows.'
}
Assert-Rejected 'stable v2 must reject more than four exception rows' {
    $fiveIds = @($fourExceptionIds) + 'contract-run/S18/c/another_operation'
    $overLimit = $fourExceptionReport | ConvertTo-Json -Depth 5 | ConvertFrom-Json
    $overLimit.release_assessment.exception_check_ids = $fiveIds
    $overLimit.release_assessment.bounded_exception_check_ids = $fiveIds
    Assert-S18ReleaseAssessment $overLimit $fiveIds $fiveIds 'stable' 0
}
Assert-Rejected 'nonzero test process cannot use a performance exception' { Assert-S18ReleaseAssessment $exceptionReport @($exceptionId) @($exceptionId) 'stable' 1 }
Assert-Rejected 'wrong release policy cannot use an exception' { Assert-S18ReleaseAssessment $exceptionReport @($exceptionId) @($exceptionId) 'strict' 0 }
Assert-Rejected 'strict failure outside exception coverage cannot pass' { Assert-S18ReleaseAssessment $exceptionReport @($exceptionId, 'contract-run/S18/go/syntax_update_after_edit') @($exceptionId) 'stable' 0 }
$forgedException = $exceptionReport | ConvertTo-Json -Depth 5 | ConvertFrom-Json
$forgedException.release_assessment.exception_check_ids = @('contract-run/S18/cpp/syntax_update_after_edit')
Assert-Rejected 'report exception IDs must exactly match raw failures' { Assert-S18ReleaseAssessment $forgedException @($exceptionId) @($exceptionId) 'stable' 0 }
$forgedBounded = $exceptionReport | ConvertTo-Json -Depth 5 | ConvertFrom-Json
$forgedBounded.release_assessment.bounded_exception_check_ids = @('contract-run/S18/cpp/completion_first_usable')
Assert-Rejected 'bounded exception IDs must match independently recomputed samples' { Assert-S18ReleaseAssessment $forgedBounded @($exceptionId) @($exceptionId) 'stable' 0 }
$failedWithBoundedCandidate = [ordered]@{
    run_id = 'contract-run'; decision = 'failed'; errors = @(); skips = @()
    release_assessment = @{
        policy_id = 's18-evidence-qualified-v2'; execution_status = 'completed'
        decision = 'failed'; bounded_exception_check_ids = @($exceptionId)
    }
}
if ((Assert-S18ReleaseAssessment $failedWithBoundedCandidate @('contract-run/S18/c/completion_first_usable', $exceptionId) @($exceptionId) 'stable' 0) -cne 'failed') {
    throw 'An omitted empty accepted-ID list must remain empty while bounded candidates and strict failures remain visible.'
}
Assert-Rejected 'a present null accepted-ID list is not an omitted empty list' {
    $invalidNullIDs = [ordered]@{
        run_id = 'contract-run'; decision = 'failed'; errors = @(); skips = @()
        release_assessment = @{
            policy_id = 's18-evidence-qualified-v2'; execution_status = 'completed'; decision = 'failed'
            bounded_exception_check_ids = @($exceptionId); exception_check_ids = $null
        }
    }
    Assert-S18ReleaseAssessment $invalidNullIDs @('contract-run/S18/c/completion_first_usable', $exceptionId) @($exceptionId) 'stable' 0
}
Assert-Rejected 'bounded candidates remain failed when the performance process exits nonzero' { Assert-S18ReleaseAssessment $failedWithBoundedCandidate @('contract-run/S18/c/completion_first_usable', $exceptionId) @($exceptionId) 'stable' 1 }
$incompletePerformance = [ordered]@{
    decision = 'failed'; release_assessment = @{
        policy_id = 's18-evidence-qualified-v2'; execution_status = 'incomplete'
        decision = 'not_verified'; bounded_exception_check_ids = @(); exception_check_ids = @()
    }
}
if ((Assert-S18ReleaseAssessment $incompletePerformance @() @() 'stable' 0) -cne 'not_verified') {
    throw 'Incomplete or unavailable performance evidence was not retained as not_verified.'
}

$resourceStages = @('initialized', 'returned-locations-200', 'returned-locations-800', 'returned-locations-3200', 'completed')
$resourceSnapshots = @(
    foreach ($index in 0..4) {
        @{ stage = $resourceStages[$index]; sampledAt = '2026-09-28T00:00:00Z'; rootPid = 1234L; status = 'observed'; privateBytes = 1048576L; privateBytesMetric = 'PROCESS_MEMORY_COUNTERS_EX.PrivateUsage'; processCount = 2L }
    }
)
$resourceCheck = [ordered]@{
    id = 'contract-run/S19/process-tree-resources'; status = 'passed'
    observed = @{ expected_snapshot_count = 5; private_memory_limit_bytes = 8589934592L; snapshots = $resourceSnapshots }
} | ConvertTo-Json -Depth 8 | ConvertFrom-Json
Assert-S19ProcessTreeResourceEvidence $resourceCheck
$oneHourChecks = @(Get-SoakRequiredCheckIds '1h')
if ('soak/resource-trend' -notin $oneHourChecks -or 'soak/resource-trend' -in @(Get-SoakRequiredCheckIds '10m')) {
    throw 'The 1h final soak must include resource trend evidence while short preflights remain short.'
}
$acceptanceSource = Get-Content -LiteralPath $acceptancePath -Raw
$performanceSource = Get-Content -LiteralPath (Join-Path $repoRoot 'test/perf/e2e_lsp_test.go') -Raw
$stableS18Scope = 'c/cpp completion_first_usable and syntax_update_after_edit only; raw single-request samples plus per-request candidate overhead required; strict thresholds remain authoritative'
$stdioSoakSource = Get-Content -LiteralPath (Join-Path $repoRoot 'test/soak/stdio_soak_test.go') -Raw
$shortSoakSource = Get-Content -LiteralPath (Join-Path $repoRoot 'test/soak/soak_test.go') -Raw
if ($acceptanceSource -notmatch "ValidateSet\([^)]*'Start1h'" -or
    $acceptanceSource -match "'Soak55m'|'Start24h'|'55m'|'24h'" -or
    $stdioSoakSource -match 'for example 24h|reserved_for=55m,24h' -or
    $stdioSoakSource -notmatch 'selected_long_gate=1h' -or
    $shortSoakSource -match 'release bar is a 24h') {
    throw 'The soak entry points must be Fast → 30s → 10m → Start1h, with no duplicated 55m or 24h stage.'
}
if ($acceptanceSource -notmatch "abba_evidence_schema = 'candidate-raw-with-correlated-write-wait-v1'" -or
    -not $acceptanceSource.Contains($stableS18Scope) -or -not $performanceSource.Contains($stableS18Scope) -or
    $acceptanceSource -notmatch 'candidate_min_samples_per_check = 6000' -or
    $acceptanceSource -notmatch 'overhead_pctl_ms = @\(5, 10, 20\)' -or
    $acceptanceSource -notmatch 'candidate_upstream_delta_max_ms = @\(5, 10, 20\)') {
    throw 'The stable v2 evidence design and per-request overhead limits must be included in the frozen acceptance fingerprint.'
}
$wrongResourceStage = $resourceCheck | ConvertTo-Json -Depth 8 | ConvertFrom-Json
$wrongResourceStage.observed.snapshots[1].stage = 'references-200'
Assert-Rejected 'resource evidence accepts only stages emitted by the real S19 sampler' { Assert-S19ProcessTreeResourceEvidence $wrongResourceStage }
$overMemoryLimit = $resourceCheck | ConvertTo-Json -Depth 8 | ConvertFrom-Json
$overMemoryLimit.observed.snapshots[1].privateBytes = 8589934593L
Assert-Rejected 'resource evidence rejects a process tree above 8 GiB' { Assert-S19ProcessTreeResourceEvidence $overMemoryLimit }

$previousGateEvidenceDirectory = $EvidenceDirectory
$previousGatePolicy = $S18Policy
$gateContractRoot = Join-Path ([IO.Path]::GetTempPath()) ('omnilsp-release-gate-contract-' + [guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Force -Path $gateContractRoot | Out-Null
try {
    $EvidenceDirectory = $gateContractRoot
    $S18Policy = 'stable'
    $gateRunId = 'gate-contract-run'
    $gateExceptionId = "$gateRunId/S18/c/syntax_update_after_edit"
    $gateAssessment = [ordered]@{
        policy_id = 's18-evidence-qualified-v2'; execution_status = 'completed'
        bounded_exception_check_ids = @($gateExceptionId)
        decision = 'passed_with_performance_exception'; exception_check_ids = @($gateExceptionId)
    }
    $gateSummaryPath = Join-Path $gateContractRoot 'Fast-summary.json'
    $gatePerformancePath = Join-Path $gateContractRoot 'performance.json'
    $gateSummary = [ordered]@{
        decision = 'passed_with_performance_exception'; s18_policy = 'stable'; release_assessment = $gateAssessment
        checks = @(@{ id = 'S18-S19-real-process-performance'; status = 'passed'; exitCode = 0 })
    }
    $gatePerformance = [ordered]@{
        release_assessment = $gateAssessment
        checks = @(@{ id = $gateExceptionId; status = 'failed'; observed = @{ failure_type = 'latency'; failure_types = @('latency') } })
    }
    $gateSummary | ConvertTo-Json -Depth 8 | Set-Content -LiteralPath $gateSummaryPath -Encoding utf8
    $gatePerformance | ConvertTo-Json -Depth 8 | Set-Content -LiteralPath $gatePerformancePath -Encoding utf8
    $gateEvidence = [ordered]@{ status = 'passed_with_performance_exception'; run_id = $gateRunId; release_assessment = $gateAssessment }
    Assert-GateReleaseAssessment $gateEvidence 'contract test'

    $nonzeroSummary = $gateSummary | ConvertTo-Json -Depth 8 | ConvertFrom-Json
    $nonzeroSummary.checks[0].exitCode = 1
    $nonzeroSummary | ConvertTo-Json -Depth 8 | Set-Content -LiteralPath $gateSummaryPath -Encoding utf8
    Assert-Rejected 'a report written by a nonzero test process cannot authorize the Fast gate' { Assert-GateReleaseAssessment $gateEvidence 'contract test' }
    $gateSummary | ConvertTo-Json -Depth 8 | Set-Content -LiteralPath $gateSummaryPath -Encoding utf8

    $semanticFailure = $gatePerformance | ConvertTo-Json -Depth 8 | ConvertFrom-Json
    $semanticFailure.checks[0].observed.failure_type = 'semantic'
    $semanticFailure.checks[0].observed.failure_types = @('semantic')
    $semanticFailure | ConvertTo-Json -Depth 8 | Set-Content -LiteralPath $gatePerformancePath -Encoding utf8
    Assert-Rejected 'semantic failure cannot authorize the Fast gate exception' { Assert-GateReleaseAssessment $gateEvidence 'contract test' }
} finally {
    $EvidenceDirectory = $previousGateEvidenceDirectory
    $S18Policy = $previousGatePolicy
    if (Test-Path -LiteralPath $gateContractRoot) {
        $resolvedGateContractRoot = [IO.Path]::GetFullPath($gateContractRoot)
        $tempRootPrefix = [IO.Path]::GetFullPath([IO.Path]::GetTempPath()).TrimEnd([IO.Path]::DirectorySeparatorChar, [IO.Path]::AltDirectorySeparatorChar) + [IO.Path]::DirectorySeparatorChar
        if (-not $resolvedGateContractRoot.StartsWith($tempRootPrefix, [StringComparison]::OrdinalIgnoreCase) -or
            [IO.Path]::GetFileName($resolvedGateContractRoot) -notlike 'omnilsp-release-gate-contract-*') {
            throw "Refusing to clean unexpected release-gate contract path '$resolvedGateContractRoot'."
        }
        Remove-Item -LiteralPath $resolvedGateContractRoot -Recurse -Force
    }
}

$badNotApplicable = $complete | ConvertTo-Json -Depth 32 | ConvertFrom-Json
$badNotApplicable.observed.records[0].metrics.editValidationFailureRate.status = 'observed'
Add-Member -InputObject $badNotApplicable.observed.records[0].metrics.editValidationFailureRate -NotePropertyName numerator -NotePropertyValue ([long]0)
Add-Member -InputObject $badNotApplicable.observed.records[0].metrics.editValidationFailureRate -NotePropertyName denominator -NotePropertyValue ([long]1)
Add-Member -InputObject $badNotApplicable.observed.records[0].metrics.editValidationFailureRate -NotePropertyName value -NotePropertyValue ([double]0)
Assert-Rejected 'read-only edit validation cannot be fabricated as an observed zero' { Assert-S20KPIEvidence $badNotApplicable 'contract-run' }

$badApplicable = $complete | ConvertTo-Json -Depth 32 | ConvertFrom-Json
$badApplicable.observed.records[15].metrics.editValidationFailureRate.status = 'not_applicable'
Add-Member -InputObject $badApplicable.observed.records[15].metrics.editValidationFailureRate -NotePropertyName reason -NotePropertyValue 'synthetic unsupported disposition'
Assert-Rejected 'Go rename edit validation remains applicable' { Assert-S20KPIEvidence $badApplicable 'contract-run' }

$contractRunId = 'contract-run'
$contractCandidateHash = '0000000000000000000000000000000000000000000000000000000000000000'
$contractClientReport = [ordered]@{
    schema_version = 1
    run_id = $contractRunId
    candidate = @{ sha256 = $contractCandidateHash }
    decision = 'passed'
    errors = @()
    skips = @()
    started_at = (Get-Date).ToUniversalTime().ToString('o')
    finished_at = (Get-Date).ToUniversalTime().AddSeconds(1).ToString('o')
    checks = @(@{ id = 'client/vscode/go'; status = 'passed' })
}
$contractClientPath = Join-Path ([IO.Path]::GetTempPath()) ('omnilsp-client-report-contract-' + [guid]::NewGuid().ToString('N') + '.json')
try {
    $contractClientReport | ConvertTo-Json -Depth 12 | Set-Content -LiteralPath $contractClientPath -Encoding utf8
    $validatedClient = Assert-StructuredReportIdentity $contractClientPath 'probe' $contractRunId $contractCandidateHash ([DateTime]::MinValue) @('client/vscode/go')
    if ($validatedClient.decision -ne 'passed') { throw 'A complete run-qualified report did not validate.' }
    $optionalDiagnosticCodeReport = $contractClientReport | ConvertTo-Json -Depth 12 | ConvertFrom-Json
    $optionalDiagnosticCodeReport.checks = @(@{
        id = 'client/vscode/go'; status = 'passed'; summary = 'one Go fixture passed'
        observed = @{
            subcase_names = @('go')
            subcases = @(@{
                name = 'go'; languageId = 'go'; status = 'passed'
                diagnostic = @{
                    diagnostic_scope = 'semantic_unresolved_name'; severity = 'error'; source = 'omnilsp-go'
                    message = 'undefined: omnilspMissingSymbol'; start = @{ line = [long]9; character = [long]8 }
                    end = @{ line = [long]9; character = [long]28 }
                }
            })
        }
    })
    $optionalDiagnosticCodeReport | ConvertTo-Json -Depth 12 | Set-Content -LiteralPath $contractClientPath -Encoding utf8
    $validatedClient = Assert-StructuredReportIdentity $contractClientPath 'clients' $contractRunId $contractCandidateHash ([DateTime]::MinValue) @('client/vscode/go')
    if ($validatedClient.decision -ne 'passed') { throw 'A valid client diagnostic with no optional LSP code did not validate.' }
    $duplicateClientReport = $contractClientReport | ConvertTo-Json -Depth 12 | ConvertFrom-Json
    $duplicateClientReport.checks = @($duplicateClientReport.checks) + @($duplicateClientReport.checks[0])
    $duplicateClientReport | ConvertTo-Json -Depth 12 | Set-Content -LiteralPath $contractClientPath -Encoding utf8
    Assert-Rejected 'structured report cannot contain duplicate check IDs' { Assert-StructuredReportIdentity $contractClientPath 'probe' $contractRunId $contractCandidateHash ([DateTime]::MinValue) @('client/vscode/go') }
    $wrongCandidateReport = $contractClientReport | ConvertTo-Json -Depth 12 | ConvertFrom-Json
    $wrongCandidateReport.candidate.sha256 = ('b' * 64)
    $wrongCandidateReport | ConvertTo-Json -Depth 12 | Set-Content -LiteralPath $contractClientPath -Encoding utf8
    Assert-Rejected 'structured report for a different candidate' { Assert-StructuredReportIdentity $contractClientPath 'probe' $contractRunId $contractCandidateHash ([DateTime]::MinValue) @('client/vscode/go') }
    $skippedPassReport = $contractClientReport | ConvertTo-Json -Depth 12 | ConvertFrom-Json
    $skippedPassReport.skips = @(@{ id = 'client/vscode/cpp'; reason = 'missing tool' })
    $skippedPassReport | ConvertTo-Json -Depth 12 | Set-Content -LiteralPath $contractClientPath -Encoding utf8
    Assert-Rejected 'structured report cannot pass with skipped combinations' { Assert-StructuredReportIdentity $contractClientPath 'probe' $contractRunId $contractCandidateHash ([DateTime]::MinValue) @('client/vscode/go') }
    $missingClientReport = $contractClientReport | ConvertTo-Json -Depth 12 | ConvertFrom-Json
    $missingClientReport.checks = @()
    $missingClientReport | ConvertTo-Json -Depth 12 | Set-Content -LiteralPath $contractClientPath -Encoding utf8
    Assert-Rejected 'structured report missing a required check' { Assert-StructuredReportIdentity $contractClientPath 'probe' $contractRunId $contractCandidateHash ([DateTime]::MinValue) @('client/vscode/go') }
    $notVerifiedClientReport = $contractClientReport | ConvertTo-Json -Depth 12 | ConvertFrom-Json
    $notVerifiedClientReport.decision = 'not_verified'
    $notVerifiedClientReport.checks[0].status = 'not_verified'
    $notVerifiedClientReport | ConvertTo-Json -Depth 12 | Set-Content -LiteralPath $contractClientPath -Encoding utf8
    if ((Assert-StructuredReportIdentity $contractClientPath 'probe' $contractRunId $contractCandidateHash ([DateTime]::MinValue) @('client/vscode/go')).decision -ne 'not_verified') {
        throw 'Main report identity validator upgraded not_verified client evidence.'
    }
} finally {
    if (Test-Path -LiteralPath $contractClientPath) { Remove-Item -LiteralPath $contractClientPath -Force }
}
$contractS21CandidatePath = Join-Path ([IO.Path]::GetTempPath()) ('omnilsp-s21-candidate-contract-' + [guid]::NewGuid().ToString('N') + '.exe')
Set-Content -LiteralPath $contractS21CandidatePath -Value 'synthetic candidate binary' -Encoding utf8
$contractS21CandidateHash = Get-FileSha256 $contractS21CandidatePath
$contractS21StartedAt = (Get-Date).ToUniversalTime().AddSeconds(-2)
$contractS21FinishedAt = (Get-Date).ToUniversalTime().AddSeconds(-1)
$contractS21GeneratedAt = (Get-Date).ToUniversalTime()
$contractS21 = [ordered]@{
    schema_version = 1
    kind = 's21'
    run_id = $contractRunId
    candidate_binary = $contractS21CandidatePath
    candidate_sha256 = $contractS21CandidateHash
    environment = @{ goos = 'windows'; goarch = 'amd64'; go_version = 'go1.26.0' }
    corpus_sha256 = Get-CorpusHash
    started_at = $contractS21StartedAt.ToString('o')
    finished_at = $contractS21FinishedAt.ToString('o')
    generated_at = $contractS21GeneratedAt.ToString('o')
    decision = 'passed'
    errors = @()
    skips = @()
    checks = @(@{
        id = "$contractRunId/S21/zero-error-classification"
        status = 'passed'
        observed = @{
            required_languages = @('go', 'c', 'cpp', 'rust', 'python', 'typescript', 'javascript')
            tested_languages = @('go', 'c', 'cpp', 'rust', 'python', 'typescript', 'javascript')
            skipped_languages = @{}
            failed_languages = @{}
            tested_cases = @(
                @{ language = 'go'; case = 'basic.go'; operations = 4; operation_counts = @{ hover = 1; definition = 1; references = 1; rename = 1; total = 4 } }, @{ language = 'go'; case = 'broken.go'; operations = 4; operation_counts = @{ hover = 1; definition = 1; references = 1; rename = 1; total = 4 } },
                @{ language = 'c'; case = 'basic.c'; operations = 4; operation_counts = @{ hover = 1; definition = 1; references = 1; rename = 1; total = 4 } }, @{ language = 'c'; case = 'broken.c'; operations = 4; operation_counts = @{ hover = 1; definition = 1; references = 1; rename = 1; total = 4 } },
                @{ language = 'cpp'; case = 'basic.cpp'; operations = 4; operation_counts = @{ hover = 1; definition = 1; references = 1; rename = 1; total = 4 } }, @{ language = 'cpp'; case = 'broken.cpp'; operations = 4; operation_counts = @{ hover = 1; definition = 1; references = 1; rename = 1; total = 4 } },
                @{ language = 'rust'; case = 'basic.rs'; operations = 4; operation_counts = @{ hover = 1; definition = 1; references = 1; rename = 1; total = 4 } }, @{ language = 'rust'; case = 'broken.rs'; operations = 4; operation_counts = @{ hover = 1; definition = 1; references = 1; rename = 1; total = 4 } },
                @{ language = 'python'; case = 'basic.py'; operations = 4; operation_counts = @{ hover = 1; definition = 1; references = 1; rename = 1; total = 4 } }, @{ language = 'python'; case = 'broken.py'; operations = 4; operation_counts = @{ hover = 1; definition = 1; references = 1; rename = 1; total = 4 } },
                @{ language = 'typescript'; case = 'basic.ts'; operations = 4; operation_counts = @{ hover = 1; definition = 1; references = 1; rename = 1; total = 4 } }, @{ language = 'typescript'; case = 'broken.ts'; operations = 4; operation_counts = @{ hover = 1; definition = 1; references = 1; rename = 1; total = 4 } },
                @{ language = 'javascript'; case = 'basic.js'; operations = 4; operation_counts = @{ hover = 1; definition = 1; references = 1; rename = 1; total = 4 } }, @{ language = 'javascript'; case = 'broken.js'; operations = 4; operation_counts = @{ hover = 1; definition = 1; references = 1; rename = 1; total = 4 } }
            )
            operation_counts = @{ hover = 14; definition = 14; references = 14; rename = 14; total = 56 }
            skipped_cases = @()
            error_buckets = @{ wrong_edit = 0; stale_edit = 0; wrong_file_location = 0; position_mapping_error = 0; protocol_invalid_response = 0; snapshot_mixing = 0 }
        }
    })
}
$contractS21Path = Join-Path ([IO.Path]::GetTempPath()) ('omnilsp-s21-report-contract-' + [guid]::NewGuid().ToString('N') + '.json')
try {
    $contractS21 | ConvertTo-Json -Depth 12 | Set-Content -LiteralPath $contractS21Path -Encoding utf8
    $validatedS21 = Assert-S21ReportEvidence $contractS21Path $contractRunId $contractS21CandidateHash ([DateTime]::MinValue)
    if ($validatedS21.decision -ne 'passed' -or (Get-S21BucketStatus $contractS21Path 'wrong_edit' $contractRunId $contractS21CandidateHash) -ne 'passed') {
        throw 'S21 structured evidence contract did not preserve a valid zero-error report.'
    }
    $missingCorpusHashS21 = $contractS21 | ConvertTo-Json -Depth 12 | ConvertFrom-Json
    $missingCorpusHashS21.PSObject.Properties.Remove('corpus_sha256')
    $missingCorpusHashS21 | ConvertTo-Json -Depth 12 | Set-Content -LiteralPath $contractS21Path -Encoding utf8
    Assert-Rejected 'S21 producer report must include its corpus SHA-256' { Assert-S21ReportEvidence $contractS21Path $contractRunId $contractS21CandidateHash ([DateTime]::MinValue) }
    $reportedErrorsS21 = $contractS21 | ConvertTo-Json -Depth 12 | ConvertFrom-Json
    $reportedErrorsS21.errors = @('synthetic report error')
    $reportedErrorsS21 | ConvertTo-Json -Depth 12 | Set-Content -LiteralPath $contractS21Path -Encoding utf8
    Assert-Rejected 'S21 passed report cannot retain non-empty errors' { Assert-S21ReportEvidence $contractS21Path $contractRunId $contractS21CandidateHash ([DateTime]::MinValue) }
    $reportedSkipsS21 = $contractS21 | ConvertTo-Json -Depth 12 | ConvertFrom-Json
    $reportedSkipsS21.skips = @('synthetic skipped case')
    $reportedSkipsS21 | ConvertTo-Json -Depth 12 | Set-Content -LiteralPath $contractS21Path -Encoding utf8
    Assert-Rejected 'S21 passed report cannot retain non-empty skips' { Assert-S21ReportEvidence $contractS21Path $contractRunId $contractS21CandidateHash ([DateTime]::MinValue) }
    $missingErrorsS21 = $contractS21 | ConvertTo-Json -Depth 12 | ConvertFrom-Json
    $missingErrorsS21.PSObject.Properties.Remove('errors')
    $missingErrorsS21 | ConvertTo-Json -Depth 12 | Set-Content -LiteralPath $contractS21Path -Encoding utf8
    Assert-Rejected 'S21 passed report must explicitly carry an empty errors array' { Assert-S21ReportEvidence $contractS21Path $contractRunId $contractS21CandidateHash ([DateTime]::MinValue) }
    $missingRuntimeS21 = $contractS21 | ConvertTo-Json -Depth 12 | ConvertFrom-Json
    $missingRuntimeS21.environment.PSObject.Properties.Remove('go_version')
    $missingRuntimeS21 | ConvertTo-Json -Depth 12 | Set-Content -LiteralPath $contractS21Path -Encoding utf8
    Assert-Rejected 'S21 producer report must include runtime version metadata' { Assert-S21ReportEvidence $contractS21Path $contractRunId $contractS21CandidateHash ([DateTime]::MinValue) }
    $badTimelineS21 = $contractS21 | ConvertTo-Json -Depth 12 | ConvertFrom-Json
    $badTimelineS21.started_at = $contractS21GeneratedAt.AddSeconds(1).ToString('o')
    $badTimelineS21 | ConvertTo-Json -Depth 12 | Set-Content -LiteralPath $contractS21Path -Encoding utf8
    Assert-Rejected 'S21 report timestamps must be ordered' { Assert-S21ReportEvidence $contractS21Path $contractRunId $contractS21CandidateHash ([DateTime]::MinValue) }
    $failedLanguageS21 = $contractS21 | ConvertTo-Json -Depth 12 | ConvertFrom-Json
    $failedLanguageS21.checks[0].observed.failed_languages = @{ rust = 'synthetic failed case' }
    $failedLanguageS21 | ConvertTo-Json -Depth 12 | Set-Content -LiteralPath $contractS21Path -Encoding utf8
    Assert-Rejected 'S21 passed report cannot contain failed languages' { Assert-S21ReportEvidence $contractS21Path $contractRunId $contractS21CandidateHash ([DateTime]::MinValue) }
    $skippedCaseS21 = $contractS21 | ConvertTo-Json -Depth 12 | ConvertFrom-Json
    $skippedCaseS21.checks[0].observed.skipped_cases = @(@{ language = 'rust'; case = 'broken.rs'; reason = 'synthetic skip' })
    $skippedCaseS21 | ConvertTo-Json -Depth 12 | Set-Content -LiteralPath $contractS21Path -Encoding utf8
    Assert-Rejected 'S21 passed report cannot contain skipped cases' { Assert-S21ReportEvidence $contractS21Path $contractRunId $contractS21CandidateHash ([DateTime]::MinValue) }
    $badCaseCountsS21 = $contractS21 | ConvertTo-Json -Depth 12 | ConvertFrom-Json
    $badCaseCountsS21.checks[0].observed.tested_cases[0].operation_counts.hover = 0
    $badCaseCountsS21 | ConvertTo-Json -Depth 12 | Set-Content -LiteralPath $contractS21Path -Encoding utf8
    Assert-Rejected 'S21 per-case operation counters must agree' { Assert-S21ReportEvidence $contractS21Path $contractRunId $contractS21CandidateHash ([DateTime]::MinValue) }
    $badS21 = $contractS21 | ConvertTo-Json -Depth 12 | ConvertFrom-Json
    $badS21.run_id = 'stale-run'
    $badS21 | ConvertTo-Json -Depth 12 | Set-Content -LiteralPath $contractS21Path -Encoding utf8
    Assert-Rejected 'S21 report from a different run' { Assert-S21ReportEvidence $contractS21Path $contractRunId $contractS21CandidateHash ([DateTime]::MinValue) }
    $unverifiedS21 = $contractS21 | ConvertTo-Json -Depth 12 | ConvertFrom-Json
    $unverifiedS21.decision = 'not_verified'
    $unverifiedS21.checks[0].status = 'not_verified'
    $unverifiedS21.checks[0].observed.skipped_languages = @{ python = 'toolchain missing' }
    $unverifiedS21 | ConvertTo-Json -Depth 12 | Set-Content -LiteralPath $contractS21Path -Encoding utf8
    if ((Assert-S21ReportEvidence $contractS21Path $contractRunId $contractS21CandidateHash ([DateTime]::MinValue)).decision -ne 'not_verified' -or
        (Get-S21BucketStatus $contractS21Path 'wrong_edit' $contractRunId $contractS21CandidateHash) -ne 'not_verified') {
        throw 'S21 skipped-language evidence was treated as a pass.'
    }
} finally {
    if (Test-Path -LiteralPath $contractS21Path) { Remove-Item -LiteralPath $contractS21Path -Force }
    if (Test-Path -LiteralPath $contractS21CandidatePath) { Remove-Item -LiteralPath $contractS21CandidatePath -Force }
}

$partialRecords = @($records | ConvertTo-Json -Depth 32 | ConvertFrom-Json)
$partialMetric = $partialRecords[0].metrics.staleResultRejectionRate
$partialMetric.status = 'not_verified'
$partialMetric.value = $null
Add-Member -InputObject $partialMetric -NotePropertyName reason -NotePropertyValue 'freshness gate has no direct sample'
$partialLimitations = @(@{
    feature = 'definition'; language = 'Go'; backend = 'golang'
    metric = 'staleResultRejectionRate'; reason = 'freshness gate has no direct sample'
})
$partial = New-Check 'not_verified' $partialRecords $partialLimitations
Assert-S20KPIEvidence $partial 'contract-run'

$fabricated = $partial | ConvertTo-Json -Depth 32 | ConvertFrom-Json
$fabricated.observed.records[0].metrics.staleResultRejectionRate.value = [double]0
Assert-Rejected 'not_verified metric with fabricated zero value' { Assert-S20KPIEvidence $fabricated 'contract-run' }

$badFormula = $complete | ConvertTo-Json -Depth 32 | ConvertFrom-Json
$badFormula.observed.records[0].metrics.precision.formula = '0'
Assert-Rejected 'changed KPI formula' { Assert-S20KPIEvidence $badFormula 'contract-run' }

$duplicate = $complete | ConvertTo-Json -Depth 32 | ConvertFrom-Json
$duplicate.observed.duplicateDimensions = @('definition/Go/golang')
Assert-Rejected 'duplicate dimensions' { Assert-S20KPIEvidence $duplicate 'contract-run' }

$missingPairRecords = @($records | Where-Object { $_.dimension.language -ne 'JavaScript' })
$missingPair = New-Check 'passed' $missingPairRecords
Assert-Rejected 'missing language/feature records' { Assert-S20KPIEvidence $missingPair 'contract-run' }

$location = @(@{
    uri = 'file:///workspace/target.go'
    range = @{
        start = @{ line = [long]1; character = [long]2 }
        end = @{ line = [long]1; character = [long]7 }
    }
})
if (-not (Test-S18NormalizedObservation $location 'textDocument/definition' 'Target') -or
    -not (Test-S18NormalizedObservation @(@{ label = 'TargetFunction' }) 'textDocument/completion' 'Target') -or
    (Test-S18NormalizedObservation @() 'textDocument/hover' 'Target') -or
    (Test-S18NormalizedObservation $location 'textDocument/unknown' 'Target')) {
    throw 'S18 normalized semantic evidence acceptance/rejection contract failed.'
}
$completionListEvidence = [pscustomobject]@{
    full_list_snapshot_status = 'passed'; full_list_sample_status = 'passed'
    target_candidate_sample_status = 'passed'
}
if (-not (Test-S18CompletionListEvidence 'c' 'completion_first_usable' $completionListEvidence) -or
    -not (Test-S18CompletionListEvidence 'cpp' 'completion_first_usable' $completionListEvidence) -or
    -not (Test-S18CompletionListEvidence 'go' 'completion_first_usable' ([pscustomobject]@{})) -or
    -not (Test-S18CompletionListEvidence 'cpp' 'hot_hover' ([pscustomobject]@{}))) {
    throw 'S18 completion evidence must require full-list validation only for C/C++ completion.'
}
$completionListEvidence.full_list_sample_status = 'not_verified'
if (Test-S18CompletionListEvidence 'cpp' 'completion_first_usable' $completionListEvidence) {
    throw 'S18 C/C++ completion evidence accepted an unverified full-list sample.'
}
$completionListEvidence.full_list_sample_status = 'passed'
$completionListEvidence.full_list_snapshot_status = 'failed'
if (Test-S18CompletionListEvidence 'c' 'completion_first_usable' $completionListEvidence) {
    throw 'S18 C completion evidence accepted a failed full-list snapshot.'
}
$completionListEvidence.full_list_snapshot_status = 'passed'
$completionListEvidence.target_candidate_sample_status = 'not_verified'
if (Test-S18CompletionListEvidence 'cpp' 'completion_first_usable' $completionListEvidence) {
    throw 'S18 C++ completion evidence accepted missing per-sample target comparisons.'
}
$editEvidence = [pscustomobject]@{
    status = 'passed'; changed = $true; freshness_verified = $true
    old_symbol = 'usePerf'; new_symbol = 'usePerfUpdated'
    before_symbols = @('targetPerf', 'usePerf')
    after_symbols = @('targetPerf', 'usePerfUpdated')
    upstream_after_symbols = @('targetPerf', 'usePerfUpdated')
}
if (-not (Test-S18EditUpdateEvidence $editEvidence 'usePerf' 'usePerfUpdated')) {
    throw 'S18 rename evidence must accept the expected new symbol alongside other unchanged document symbols.'
}
$editEvidence.after_symbols = @('targetPerf', 'usePerfOther')
if (Test-S18EditUpdateEvidence $editEvidence 'usePerf' 'usePerfUpdated') {
    throw 'S18 rename evidence accepted a document-symbol list without the expected renamed symbol.'
}
$editEvidence.after_symbols = @('targetPerf', 'usePerfUpdated')
$editEvidence.upstream_after_symbols = @('targetPerf', 'usePerfDifferent')
if (Test-S18EditUpdateEvidence $editEvidence 'usePerf' 'usePerfUpdated') {
    throw 'S18 rename evidence accepted candidate/upstream document-symbol disagreement.'
}
$editEvidence.upstream_after_symbols = @('targetPerf', 'usePerfUpdated')
$editEvidence.before_symbols = @('targetPerf', 'anotherSymbol')
if (Test-S18EditUpdateEvidence $editEvidence 'usePerf' 'usePerfUpdated') {
    throw 'S18 rename evidence accepted an old symbol missing from the pre-edit snapshot.'
}
$editEvidence.before_symbols = @('targetPerf', 'usePerf')
$editEvidence.after_symbols = @('targetPerf', 'usePerf', 'usePerfUpdated')
$editEvidence.upstream_after_symbols = @('targetPerf', 'usePerf', 'usePerfUpdated')
if (Test-S18EditUpdateEvidence $editEvidence 'usePerf' 'usePerfUpdated') {
    throw 'S18 rename evidence accepted a post-edit snapshot where the old symbol remained.'
}
$editEvidence.after_symbols = @('usePerfUpdated')
$editEvidence.upstream_after_symbols = @('usePerfUpdated')
if (Test-S18EditUpdateEvidence $editEvidence 'usePerf' 'usePerfUpdated') {
    throw 'S18 rename evidence accepted candidate/upstream snapshots that both dropped an unchanged symbol.'
}

function New-S19LocationScaleFixture([int] $LocationTotal) {
    $id = "$contractRunId/S19/references@$LocationTotal"
    return ([ordered]@{
        check = [ordered]@{
            id = $id
            status = 'passed'
            summary = "exact returned-location total including declaration; target=$LocationTotal"
            observed = [ordered]@{
                returned_location_total_including_declaration = [long]$LocationTotal
                expected_returned_location_count_including_declaration = [long]$LocationTotal
                observed_returned_location_counts_including_declaration = @([long]$LocationTotal, [long]$LocationTotal, [long]$LocationTotal)
                p50_ns = [long]200
                p95_ns = [long]300
                p99_ns = [long]300
            }
        }
        sample = [ordered]@{
            id = $id
            unit = 'ns/query'
            raw = @([long]100, [long]200, [long]300)
            p50 = [double]200
            p95 = [double]300
            p99 = [double]300
        }
    } | ConvertTo-Json -Depth 12 | ConvertFrom-Json)
}

foreach ($locationTotal in @(200, 800, 3200)) {
    $scaleFixture = New-S19LocationScaleFixture $locationTotal
    Assert-S19ReturnedLocationScale $scaleFixture.check $scaleFixture.sample $locationTotal
}
$legacyS19 = New-S19LocationScaleFixture 200
Add-Member -InputObject $legacyS19.check.observed -NotePropertyName result_counts -NotePropertyValue @([long]200, [long]200, [long]200)
Assert-Rejected 'S19 ambiguous legacy result-count field' { Assert-S19ReturnedLocationScale $legacyS19.check $legacyS19.sample 200 }
$wrongTotalS19 = New-S19LocationScaleFixture 200
$wrongTotalS19.check.observed.expected_returned_location_count_including_declaration = [long]201
Assert-Rejected 'S19 wrong returned-location total' { Assert-S19ReturnedLocationScale $wrongTotalS19.check $wrongTotalS19.sample 200 }
$missingCountsS19 = New-S19LocationScaleFixture 200
$missingCountsS19.check.observed.PSObject.Properties.Remove('observed_returned_location_counts_including_declaration')
Assert-Rejected 'S19 missing observed returned-location counts' { Assert-S19ReturnedLocationScale $missingCountsS19.check $missingCountsS19.sample 200 }
$mismatchedPercentileS19 = New-S19LocationScaleFixture 200
$mismatchedPercentileS19.sample.raw = @([long]100, [long]200, [long]400)
Assert-Rejected 'S19 retained samples do not match reported percentiles' { Assert-S19ReturnedLocationScale $mismatchedPercentileS19.check $mismatchedPercentileS19.sample 200 }

$referenceRequest = 12L
$completionFirstObserved = [pscustomobject]@{ reference_request_id = $referenceRequest; completion_request_id = 13L; hover_request_id = 14L }
$hoverFirstObserved = [pscustomobject]@{ reference_request_id = $referenceRequest; completion_request_id = 14L; hover_request_id = 13L }
if (-not (Test-S19ConcurrentRequestIdentity $completionFirstObserved) -or
    -not (Test-S19ConcurrentRequestIdentity $hoverFirstObserved)) {
    throw 'S19 request IDs must allow either ordering for concurrent completion and hover requests.'
}
$duplicateConcurrentID = [pscustomobject]@{ reference_request_id = $referenceRequest; completion_request_id = 13L; hover_request_id = 13L }
$skippedConcurrentID = [pscustomobject]@{ reference_request_id = $referenceRequest; completion_request_id = 14L; hover_request_id = 16L }
if ((Test-S19ConcurrentRequestIdentity $duplicateConcurrentID) -or
    (Test-S19ConcurrentRequestIdentity $skippedConcurrentID)) {
    throw 'S19 request ID validation accepted duplicate or nonconsecutive concurrent requests.'
}

$previousEvidenceDirectory = $EvidenceDirectory
$contractEvidenceDirectory = Join-Path ([IO.Path]::GetTempPath()) ('omnilsp-fast-evidence-contract-' + [guid]::NewGuid().ToString('N'))
try {
    $EvidenceDirectory = $contractEvidenceDirectory
    New-Item -ItemType Directory -Force -Path $EvidenceDirectory | Out-Null
    foreach ($name in @('Fast-summary.json', 's21.json', 'performance.json', 'semantic.json', 'persistent-go-go.json', 'persistent-typescript.json', 'persistent-python-python.json', 'persistent-javascript-javascript.json', 'persistent-c-c.json', 'persistent-cpp-cpp.json', 'scip-replay-go-go-cli.json', 'clients.json')) {
        Set-Content -LiteralPath (Join-Path $EvidenceDirectory $name) -Value "frozen:$name" -Encoding utf8
    }
    $frozen = [ordered]@{ fast_evidence_sha256 = Get-FastEvidenceHashes } | ConvertTo-Json -Depth 4 | ConvertFrom-Json
    Assert-FastEvidenceHashes $frozen 'contract test'
    Set-Content -LiteralPath (Join-Path $EvidenceDirectory 'performance.json') -Value 'tampered after Fast' -Encoding utf8
    Assert-Rejected 'mutated Fast performance report' { Assert-FastEvidenceHashes $frozen 'contract test' }

    function Write-Summary($Decision, $Checks, $Errors = @()) {
        $script:ContractSoakSummary = [ordered]@{ decision = $Decision; checks = @($Checks); errors = @($Errors) }
    }
    function Write-RequirementMatrix { [void]($script:ContractMatrixWritten = $true) }
    $CandidateBinary = Join-Path $contractEvidenceDirectory 'contract-candidate.exe'
    $testExe = Join-Path $contractEvidenceDirectory 'soak.test.exe'
    Set-Content -LiteralPath $CandidateBinary -Value 'candidate' -Encoding utf8
    Set-Content -LiteralPath $testExe -Value 'soak test' -Encoding utf8
    Set-Content -LiteralPath (Join-Path $contractEvidenceDirectory 'soak-30s-report.json') -Value 'report' -Encoding utf8
    $RunId = 'contract-soak-run'
    $script:ContractMatrixWritten = $false
    $failure = [System.Management.Automation.ErrorRecord]::new([InvalidOperationException]::new('synthetic soak failure'), 'synthetic-soak-failure', [System.Management.Automation.ErrorCategory]::OperationStopped, $null)
    Write-SoakStageFailure '30s' $failure
    $failedStage = Get-Content -LiteralPath (Join-Path $contractEvidenceDirectory 'soak-30s.json') -Raw | ConvertFrom-Json
    if ($failedStage.status -ne 'failed' -or $failedStage.error -ne 'synthetic soak failure' -or
        $failedStage.report_sha256 -ne (Get-FileSha256 (Join-Path $contractEvidenceDirectory 'soak-30s-report.json')) -or
        $script:ContractSoakSummary.decision -ne 'failed' -or -not $script:ContractMatrixWritten) {
        throw 'Soak failure did not persist its failure record, summary, and updated matrix.'
    }
} finally {
    if ($previousEvidenceDirectory) { $EvidenceDirectory = $previousEvidenceDirectory } else { Remove-Variable EvidenceDirectory -ErrorAction SilentlyContinue }
    if (Test-Path -LiteralPath $contractEvidenceDirectory) { Remove-Item -LiteralPath $contractEvidenceDirectory -Recurse -Force }
}

$persistentContractPath = Join-Path ([IO.Path]::GetTempPath()) ('omnilsp-persistent-contract-' + [guid]::NewGuid().ToString('N') + '.json')
try {
    $manifest = '{"target.go":"package fixture"}'
    $digest = 'sha256:' + [Convert]::ToHexString([Security.Cryptography.SHA256]::HashData([Text.Encoding]::UTF8.GetBytes($manifest))).ToLowerInvariant()
    $persistentContract = @{
        schema_version = 1; run_id = 'persistent-contract'; candidate = @{ sha256 = 'a' * 64 }
        decision = 'passed'; started_at = '2026-10-02T00:00:00Z'; finished_at = '2026-10-02T00:00:01Z'
        corpus = @{ sha256 = $digest }
        environment = @{ fixtureManifest = $manifest; goos = 'windows'; goarch = 'amd64'; goVersion = 'go1.26'; candidateProcess = 'real stdio'; fixtureLanguage = 'go'; backendLanguage = 'go' }
        checks = @(@{ id = 'persistent-contract/S21/go-persistent-index-only-queries'; status = 'passed'; observed = @{
            backendFound = $false; generation = [long]1
            definition = @(@{ uri = 'file:///target.go' }); references = @(@{ uri = 'file:///target.go' }, @{ uri = 'file:///use.go' }); workspaceSymbols = @(@{ name = 'Target' })
            queryTraceBefore = @{ Computations = [long]0 }; queryTraceAfter = @{ Computations = [long]0 }
            'resultMeta.workspace/symbol' = @(@{ status = 'exact'; completeness = 'complete'; evidence = @(@{ IndexGen = [long]1 }) })
            'resultMeta.textDocument/definition' = @(@{ status = 'exact'; completeness = 'complete'; evidence = @(@{ IndexGen = [long]1 }) })
            'resultMeta.textDocument/references' = @(@{ status = 'exact'; completeness = 'complete'; evidence = @(@{ IndexGen = [long]1 }) })
        } })
    }
    $persistentContract | ConvertTo-Json -Depth 12 | Set-Content -LiteralPath $persistentContractPath -Encoding utf8
    $persistentCheckId = 'persistent-contract/S21/go-persistent-index-only-queries'
    foreach ($language in @('c', 'cpp')) {
        $clangCheckId = "persistent-contract/S21/$language-persistent-index-only-queries"
        $persistentContract.checks[0].id = $clangCheckId
        $persistentContract.environment.fixtureLanguage = $language
        $persistentContract.environment.backendLanguage = 'cpp'
        $persistentContract | ConvertTo-Json -Depth 12 | Set-Content -LiteralPath $persistentContractPath -Encoding utf8
        $null = Assert-StructuredReportIdentity $persistentContractPath 'persistent' 'persistent-contract' ('a' * 64) ([DateTime]::MinValue) @($clangCheckId) '' 0
        $persistentContract.environment.backendLanguage = 'c'
        $persistentContract | ConvertTo-Json -Depth 12 | Set-Content -LiteralPath $persistentContractPath -Encoding utf8
        Assert-Rejected "wrong C/C++ backend identity for $language" { Assert-StructuredReportIdentity $persistentContractPath 'persistent' 'persistent-contract' ('a' * 64) ([DateTime]::MinValue) @($clangCheckId) '' 0 }
    }
    $persistentContract.checks[0].id = $persistentCheckId
    $persistentContract.environment.fixtureLanguage = 'go'
    $persistentContract.environment.backendLanguage = 'go'
    $persistentContract | ConvertTo-Json -Depth 12 | Set-Content -LiteralPath $persistentContractPath -Encoding utf8
    $null = Assert-StructuredReportIdentity $persistentContractPath 'persistent' 'persistent-contract' ('a' * 64) ([DateTime]::MinValue) @($persistentCheckId) '' 0
    Assert-Rejected 'persistent report written before producer failure' { Assert-StructuredReportIdentity $persistentContractPath 'persistent' 'persistent-contract' ('a' * 64) ([DateTime]::MinValue) @($persistentCheckId) '' 1 }
    $persistentContract.checks[0].observed.queryTraceBefore.Remove('Computations')
    $persistentContract | ConvertTo-Json -Depth 12 | Set-Content -LiteralPath $persistentContractPath -Encoding utf8
    Assert-Rejected 'persistent report without computation counter' { Assert-StructuredReportIdentity $persistentContractPath 'persistent' 'persistent-contract' ('a' * 64) ([DateTime]::MinValue) @($persistentCheckId) '' 0 }
    $persistentContract.checks[0].observed.queryTraceBefore.Computations = [long]0
    $persistentContract.checks[0].id = 'persistent-contract/query'
    $persistentContract | ConvertTo-Json -Depth 12 | Set-Content -LiteralPath $persistentContractPath -Encoding utf8
    Assert-Rejected 'unsupported persistent proof kind' { Assert-StructuredReportIdentity $persistentContractPath 'persistent' 'persistent-contract' ('a' * 64) ([DateTime]::MinValue) @('persistent-contract/query') '' 0 }
    $persistentContract.checks[0].id = $persistentCheckId
    $queryObserved = $persistentContract.checks[0].observed
    $cliCheckId = 'persistent-contract/S22/go-cli-scip-replay-persistence'
    $cliObserved = @{ recordedIdentity = $true; sourceGeneration = 1; importedGeneration = 2; exportedDocuments = 2; exportedOccurrences = 4 }
    foreach ($key in @('wrongConfigRejected', 'wrongContentIdentityRejected', 'wrongSourceRootRejected', 'wrongToolIdentityRejected', 'missingGenerationRejected')) {
        $cliObserved[$key] = @{ exitCode = 1; exitError = 'exit status 1'; stderr = 'expected identity refusal'; stdout = ''; completed = $true; outputLimitExceeded = $false }
    }
    $cliObserved.fixedGenerationReplay = @{ exitCode = 0; exitError = ''; stderr = ''; stdout = 'REPLAY-OK (semantic=complete)'; completed = $true; outputLimitExceeded = $false }
    $persistentContract.checks[0].id = $cliCheckId
    $persistentContract.checks[0].observed = $cliObserved
    $persistentContract | ConvertTo-Json -Depth 12 | Set-Content -LiteralPath $persistentContractPath -Encoding utf8
    $null = Assert-StructuredReportIdentity $persistentContractPath 'persistent' 'persistent-contract' ('a' * 64) ([DateTime]::MinValue) @($cliCheckId) '' 0
    $cliObserved.wrongToolIdentityRejected.completed = $false
    $persistentContract | ConvertTo-Json -Depth 12 | Set-Content -LiteralPath $persistentContractPath -Encoding utf8
    Assert-Rejected 'interrupted command reported as identity refusal' { Assert-StructuredReportIdentity $persistentContractPath 'persistent' 'persistent-contract' ('a' * 64) ([DateTime]::MinValue) @($cliCheckId) '' 0 }
    $cliObserved.wrongToolIdentityRejected.completed = $true
    $cliObserved.fixedGenerationReplay.outputLimitExceeded = $true
    $persistentContract | ConvertTo-Json -Depth 12 | Set-Content -LiteralPath $persistentContractPath -Encoding utf8
    Assert-Rejected 'truncated replay output reported as success' { Assert-StructuredReportIdentity $persistentContractPath 'persistent' 'persistent-contract' ('a' * 64) ([DateTime]::MinValue) @($cliCheckId) '' 0 }
    $persistentContract.checks[0].id = $persistentCheckId
    $persistentContract.checks[0].observed = $queryObserved
    $persistentContract.environment.fixtureManifest = '{"target.go":"tampered"}'
    $persistentContract | ConvertTo-Json -Depth 12 | Set-Content -LiteralPath $persistentContractPath -Encoding utf8
    Assert-Rejected 'persistent fixture manifest tampering' { Assert-StructuredReportIdentity $persistentContractPath 'persistent' 'persistent-contract' ('a' * 64) ([DateTime]::MinValue) @($persistentCheckId) '' 0 }
} finally {
    Remove-Item -LiteralPath $persistentContractPath -Force -ErrorAction SilentlyContinue
}

$emacsValidator = $ast.Find({ param($node) $node -is [System.Management.Automation.Language.FunctionDefinitionAst] -and $node.Name -eq 'Assert-EmacsCellEvidence' }, $true)
if (-not $emacsValidator) { throw 'Emacs native evidence validator is missing.' }
Invoke-Expression $emacsValidator.Extent.Text
$emacsCell = @{ id = 'client/emacs-eglot/go'; observed = @{ subcases = @(@{
    name = 'go'; languageId = 'go'; status = 'passed'; observed = @{
        hover = 'func SoakTarget'; completion_candidates = 'SoakTarget'; definition_count = 1; reference_count = 2
        rename_refusal = 'error -32803'; snapshot_epoch_before = 1; snapshot_epoch_after = 2
        shutdown_request = @{ responseReceived = $true }; exit_notification_sent = $true
        server_process_exit_status = 'exit/0'; diagnostic_scope = 'semantic_unresolved_name'
        diagnostic_type = 'eglot-error'; diagnostic_message = 'undefined: missing'
        diagnostic_snapshot_epoch_before = 2; diagnostic_snapshot_epoch_after = 3
    }
}) } } | ConvertTo-Json -Depth 8 | ConvertFrom-Json
Assert-EmacsCellEvidence $emacsCell 'go'
$emacsCell.observed.subcases[0].observed.diagnostic_message = ''
Assert-Rejected 'Emacs cell without consumed semantic diagnostic' { Assert-EmacsCellEvidence $emacsCell 'go' }
$emacsCell.observed.subcases[0].observed.diagnostic_message = 'undefined: missing'
$emacsCell.observed.subcases[0].observed.shutdown_request.responseReceived = $false
Assert-Rejected 'Emacs cell without graceful shutdown response' { Assert-EmacsCellEvidence $emacsCell 'go' }

Write-Output 'Acceptance report validator contract tests passed.'
