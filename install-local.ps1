param(
    [string]$InstallDir = (Join-Path $env:LOCALAPPDATA 'Programs\McpHub'),
    [string]$DataDir = (Join-Path $env:LOCALAPPDATA 'McpHub'),
    [string]$SymlinkDir = (Join-Path $env:USERPROFILE '.local\bin'),
    [switch]$RegisterStartup,
    [switch]$Start,
    [switch]$NoStart,
    [switch]$NoSymlink,
    [switch]$Uninstall,
    [scriptblock]$PostStartCheck
)

# Windows PowerShell 5.1 and PowerShell 7+ compatible.
$ErrorActionPreference = 'Stop'

if ($Start -and $NoStart) {
    throw 'Cannot combine -Start and -NoStart.'
}

function Resolve-LocalPath([string]$Path) {
    $fullPath = [IO.Path]::GetFullPath($ExecutionContext.SessionState.Path.GetUnresolvedProviderPathFromPSPath($Path))
    $root = [IO.Path]::GetPathRoot($fullPath)
    if ($fullPath.Length -gt $root.Length) {
        return $fullPath.TrimEnd('\', '/')
    }
    return $fullPath
}

$InstallDir = Resolve-LocalPath $InstallDir
$DataDir = Resolve-LocalPath $DataDir
$SymlinkDir = Resolve-LocalPath $SymlinkDir

$RunKey = 'HKCU:\Software\Microsoft\Windows\CurrentVersion\Run'
$RunValueName = 'McpHub'
$HubExe = Join-Path $InstallDir 'mcp-hub.exe'
$CtlExe = Join-Path $InstallDir 'mcp-hub-ctl.exe'
$transactionId = [Guid]::NewGuid().ToString('N')
$stageDir = Join-Path $InstallDir ('.install-' + $transactionId)
$HubNew = Join-Path $stageDir 'mcp-hub.exe'
$CtlNew = Join-Path $stageDir 'mcp-hub-ctl.exe'
$ConfigPath = Join-Path $DataDir 'config.json'
$SymlinkPath = Join-Path $SymlinkDir 'mcp-hub-ctl.exe'
$ReceiptPath = Join-Path $InstallDir 'installation.json'
$HubCmdLine = "`"$HubExe`" run --config `"$ConfigPath`""

function Write-Step([string]$Message) {
    Write-Host "[mcp-hub] $Message"
}

function Get-Sha256Hex([string]$FilePath) {
    $sha = [System.Security.Cryptography.SHA256]::Create()
    try {
        $stream = [System.IO.File]::OpenRead($FilePath)
        try {
            return ([System.BitConverter]::ToString($sha.ComputeHash($stream))).Replace('-', '').ToLowerInvariant()
        } finally {
            $stream.Dispose()
        }
    } finally {
        $sha.Dispose()
    }
}

# Runs a native command without letting stderr records turn into terminating errors (PS 5.1).
function Invoke-Native([string]$Exe, [string[]]$Arguments, [switch]$StdoutOnly) {
    $prev = $ErrorActionPreference
    $ErrorActionPreference = 'Continue'
    try {
        if ($StdoutOnly) {
            $lines = & $Exe @Arguments 2>$null
        } else {
            $lines = & $Exe @Arguments 2>&1
        }
        $text = (@($lines) | ForEach-Object { "$_" }) -join "`n"
        return [pscustomobject]@{ ExitCode = $LASTEXITCODE; Output = $text }
    } catch {
        return [pscustomobject]@{ ExitCode = -1; Output = $_.Exception.Message }
    } finally {
        $ErrorActionPreference = $prev
    }
}

function Get-GoModulePath([string]$SourceRoot) {
    $goModPath = Join-Path $SourceRoot 'go.mod'
    if (-not (Test-Path -LiteralPath $goModPath -PathType Leaf)) {
        throw "go.mod not found at $goModPath"
    }
    foreach ($line in [IO.File]::ReadAllLines($goModPath)) {
        if ($line -match '^\s*module\s+(\S+)') {
            return $Matches[1].Trim('"', "'")
        }
    }
    throw "Could not determine Go module path from $goModPath"
}

function Remove-StaleStageDirs {
    if (-not (Test-Path -LiteralPath $InstallDir -PathType Container)) {
        return
    }
    $cutoff = (Get-Date).AddHours(-1)
    $staleDirs = @(Get-ChildItem -LiteralPath $InstallDir -Directory -Filter '.install-*' -Force -ErrorAction SilentlyContinue |
        Where-Object { $_.LastWriteTime -lt $cutoff })
    foreach ($dir in $staleDirs) {
        try {
            [IO.Directory]::Delete($dir.FullName, $true)
            Write-Step "Cleaned stale staging directory $($dir.Name)"
        } catch {
            Write-Warning "[mcp-hub] Could not clean stale staging directory $($dir.FullName) ($($_.Exception.Message))."
        }
    }
}

function Get-InstalledHubProcesses {
    $all = Get-CimInstance -ClassName Win32_Process -Filter "Name = 'mcp-hub.exe'"
    $all | Where-Object {
        $_.ExecutablePath -and [string]::Equals($_.ExecutablePath, $HubExe, [System.StringComparison]::OrdinalIgnoreCase)
    }
}

function Wait-InstalledHubExit([int]$TimeoutMs) {
    $deadline = (Get-Date).AddMilliseconds($TimeoutMs)
    while ((Get-Date) -lt $deadline) {
        if (@(Get-InstalledHubProcesses).Count -eq 0) {
            return $true
        }
        Start-Sleep -Milliseconds 200
    }
    return (@(Get-InstalledHubProcesses).Count -eq 0)
}

# Gracefully stops the hub running from $HubExe. Returns $true when a hub was running.
function Stop-InstalledHub {
    $procs = @(Get-InstalledHubProcesses)
    if ($procs.Count -eq 0) {
        return $false
    }
    $pids = ($procs | ForEach-Object { $_.ProcessId }) -join ', '
    Write-Step "Stopping running mcp-hub (pid $pids) ..."
    if (Test-Path -LiteralPath $ConfigPath -PathType Leaf) {
        # POST /stop is served by v0.1.0, v0.2.0, and v0.3.0 hubs alike; try the freshest CLI first.
        # Bound each call to 5s and only fall back to legacy CLI flags on exit code 2 (usage error).
        $skipInstalledCtl = $false
        if (Test-Path -LiteralPath $CtlNew -PathType Leaf) {
            $r = Invoke-Native -Exe $CtlNew -Arguments @('shutdown', '--config', $ConfigPath, '--timeout', '5s')
            if ($r.ExitCode -eq 0 -or ($r.ExitCode -ne 2 -and $r.ExitCode -ne -1)) {
                $skipInstalledCtl = $true
            }
        }
        if (-not $skipInstalledCtl -and (Test-Path -LiteralPath $CtlExe -PathType Leaf)) {
            $r = Invoke-Native -Exe $CtlExe -Arguments @('shutdown', '--config', $ConfigPath, '--timeout', '5s')
            if ($r.ExitCode -eq 2) {
                $r = Invoke-Native -Exe $CtlExe -Arguments @('stop', '--hub', '--config', $ConfigPath, '--timeout', '5s')
                if ($r.ExitCode -eq 2) {
                    [void](Invoke-Native -Exe $CtlExe -Arguments @('stop', '--config', $ConfigPath, '--timeout', '5s'))
                }
            }
        }
        if (Wait-InstalledHubExit 10000) {
            Write-Step 'mcp-hub exited gracefully.'
            return $true
        }
        Write-Warning '[mcp-hub] Graceful shutdown timed out after 10s; forcing termination.'
    }
    Get-InstalledHubProcesses | ForEach-Object {
        Stop-Process -Id $_.ProcessId -Force -ErrorAction SilentlyContinue
    }
    if (-not (Wait-InstalledHubExit 5000)) {
        throw "mcp-hub ($HubExe) is still running after Stop-Process"
    }
    return $true
}

function Start-InstalledHub {
    $res = Invoke-CimMethod -ClassName Win32_Process -MethodName Create -Arguments @{ CommandLine = $HubCmdLine }
    if ($res.ReturnValue -ne 0) {
        throw "Win32_Process.Create failed with ReturnValue=$($res.ReturnValue)"
    }
    return $res.ProcessId
}

function Wait-HubHealthy([uint32]$HubProcessId, [string]$ControlAddress, [double]$TimeoutSec) {
    if ([string]::IsNullOrWhiteSpace($ControlAddress)) {
        throw "Cannot verify mcp-hub health: control address is not initialized (missing config at $ConfigPath)."
    }
    # The unauthenticated /healthz endpoint is shared across hub versions.
    # Verify the listening PID so another instance cannot satisfy this readiness check.
    Add-Type -AssemblyName System.Net.Http
    $control = [Uri]('http://' + $ControlAddress.Trim())
    $handler = New-Object System.Net.Http.HttpClientHandler
    $handler.UseProxy = $false
    $handler.AllowAutoRedirect = $false
    $client = New-Object System.Net.Http.HttpClient($handler)
    $watch = [Diagnostics.Stopwatch]::StartNew()
    try {
        while ($watch.Elapsed.TotalSeconds -lt $TimeoutSec) {
            $process = Get-Process -Id $HubProcessId -ErrorAction SilentlyContinue
            if (!$process) {
                return $false
            }
            try {
                if ($process.HasExited) {
                    return $false
                }
                $procPath = $null
                try {
                    $procPath = $process.Path
                } catch {
                    $procPath = $null
                }
                if (![string]::IsNullOrEmpty($procPath) -and ![string]::Equals($procPath, $HubExe, [System.StringComparison]::OrdinalIgnoreCase)) {
                    return $false
                }

                $listeners = @(Get-NetTCPConnection -OwningProcess $HubProcessId -State Listen -ErrorAction SilentlyContinue)
                foreach ($listener in $listeners) {
                    $ip = [Net.IPAddress]::Parse($listener.LocalAddress)
                    if (![Net.IPAddress]::IsLoopback($ip) -or ($control.Port -ne 0 -and $listener.LocalPort -ne $control.Port)) {
                        continue
                    }
                    if ($control.Host -ne 'localhost' -and !$ip.Equals([Net.IPAddress]::Parse($control.DnsSafeHost))) {
                        continue
                    }

                    $remaining = $TimeoutSec - $watch.Elapsed.TotalSeconds
                    if ($remaining -le 0) {
                        return $false
                    }
                    $attemptTimeout = [TimeSpan]::FromSeconds([Math]::Min($remaining, 2))
                    $cancel = New-Object System.Threading.CancellationTokenSource($attemptTimeout)

                    # Build URI and Host explicitly so .NET Framework (PS 5.1) does not expand
                    # IPv6 ::1 into [0000:0000:...:0001], which loopback Host validation rejects.
                    if ($ip.AddressFamily -eq [System.Net.Sockets.AddressFamily]::InterNetworkV6) {
                        $hostHeader = "[$($ip.ToString())]:$($listener.LocalPort)"
                    } else {
                        $hostHeader = "$($ip.ToString()):$($listener.LocalPort)"
                    }
                    $uri = [Uri]("http://$hostHeader/healthz")
                    $request = New-Object System.Net.Http.HttpRequestMessage([System.Net.Http.HttpMethod]::Get, $uri)
                    $request.Headers.Host = $hostHeader
                    try {
                        $response = $client.SendAsync($request, $cancel.Token).GetAwaiter().GetResult()
                        try {
                            if ($response.IsSuccessStatusCode -and $response.Content.Headers.ContentType.MediaType -eq 'application/json') {
                                $body = $response.Content.ReadAsStringAsync().GetAwaiter().GetResult() | ConvertFrom-Json
                                $process.Refresh()
                                if ($body.ok -is [bool] -and $body.ok -and $body.PSObject.Properties['services'] -and !$process.HasExited) {
                                    return $true
                                }
                            }
                        } finally {
                            $response.Dispose()
                        }
                    } catch [System.Net.Http.HttpRequestException], [System.OperationCanceledException] {
                        # The listener may disappear while the hub is starting or shutting down.
                    } finally {
                        $request.Dispose()
                        $cancel.Dispose()
                    }
                }
            } finally {
                $process.Dispose()
            }

            $sleepMs = [Math]::Min(250, 1000 * ($TimeoutSec - $watch.Elapsed.TotalSeconds))
            if ($sleepMs -gt 0) {
                Start-Sleep -Milliseconds ([int]$sleepMs)
            }
        }
        return $false
    } finally {
        $client.Dispose()
    }
}

function Assert-NoReparseAncestor([string]$Path) {
    for ($cur = $Path; $cur; $cur = [IO.Path]::GetDirectoryName($cur)) {
        $item = Get-Item -LiteralPath $cur -Force -ErrorAction SilentlyContinue
        if ($item -and ($item.Attributes -band [IO.FileAttributes]::ReparsePoint)) {
            throw "Cannot apply a private ACL through a link or mount point: $cur"
        }
    }
}

function Get-PrivateTreeItems($Directory) {
    $Directory
    foreach ($item in Get-ChildItem -LiteralPath $Directory.FullName -Force) {
        if ($item.Attributes -band [IO.FileAttributes]::ReparsePoint) {
            throw "Cannot apply a private ACL to a link or mount point: $($item.FullName)"
        }
        if ($item.PSIsContainer) {
            Get-PrivateTreeItems $item
        } else {
            $item
        }
    }
}

function Test-PrivateDataAcl($Acl, $Sid, [bool]$RequireProtected) {
    $rules = @($Acl.GetAccessRules($true, $true, [Security.Principal.SecurityIdentifier]))
    return (!$RequireProtected -or $Acl.AreAccessRulesProtected) -and
        ($Acl.GetOwner([Security.Principal.SecurityIdentifier]) -eq $Sid) -and
        $rules.Count -eq 1 -and $rules[0].IdentityReference -eq $Sid -and
        $rules[0].AccessControlType -eq 'Allow' -and $rules[0].FileSystemRights -eq 'FullControl'
}

function Set-PrivateDataTree {
    Write-Step "Applying and checking private NTFS ACLs on $DataDir ..."
    $sid = [Security.Principal.WindowsIdentity]::GetCurrent().User
    $dirRule = New-Object Security.AccessControl.FileSystemAccessRule(
        $sid, 'FullControl', 'ContainerInherit, ObjectInherit', 'None', 'Allow'
    )
    $fileRule = New-Object Security.AccessControl.FileSystemAccessRule($sid, 'FullControl', 'Allow')

    Assert-NoReparseAncestor $DataDir
    $items = @(Get-PrivateTreeItems (Get-Item -LiteralPath $DataDir -Force))
    foreach ($item in $items) {
        if ([IO.File]::GetAttributes($item.FullName) -band [IO.FileAttributes]::ReparsePoint) {
            throw "Cannot apply a private ACL to a link or mount point: $($item.FullName)"
        }
        $actual = Get-Acl -LiteralPath $item.FullName
        if (Test-PrivateDataAcl $actual $sid ($item.FullName -eq $DataDir)) {
            continue
        }

        if ($item.PSIsContainer) {
            $acl = New-Object Security.AccessControl.DirectorySecurity
            $acl.AddAccessRule($dirRule)
        } else {
            $acl = New-Object Security.AccessControl.FileSecurity
            $acl.AddAccessRule($fileRule)
        }
        if ($actual.GetOwner([Security.Principal.SecurityIdentifier]) -ne $sid) {
            $acl.SetOwner($sid)
        }
        $acl.SetAccessRuleProtection($true, $false)
        # Write only owner/DACL changes. The PowerShell provider can also request SACL access.
        if ($PSVersionTable.PSEdition -eq 'Desktop') {
            $item.SetAccessControl($acl)
        } else {
            [IO.FileSystemAclExtensions]::SetAccessControl($item, $acl)
        }
        if (!(Test-PrivateDataAcl (Get-Acl -LiteralPath $item.FullName) $sid $true)) {
            throw "Private ACL verification failed on $($item.FullName)"
        }
    }
}

function Get-RunValue {
    $item = Get-ItemProperty -LiteralPath $RunKey -Name $RunValueName -ErrorAction SilentlyContinue
    return $item.$RunValueName
}

function Test-SameDriveRoot([string]$PathA, [string]$PathB) {
    $rootA = [IO.Path]::GetPathRoot($PathA)
    $rootB = [IO.Path]::GetPathRoot($PathB)
    return ![string]::IsNullOrEmpty($rootA) -and [string]::Equals($rootA, $rootB, [System.StringComparison]::OrdinalIgnoreCase)
}

function Test-DirInPath([string]$Dir) {
    $normDir = Resolve-LocalPath $Dir
    foreach ($entry in ($env:PATH -split ';')) {
        $trimmed = $entry.Trim().Trim('"')
        if ([string]::IsNullOrWhiteSpace($trimmed)) {
            continue
        }
        try {
            if ([string]::Equals((Resolve-LocalPath $trimmed), $normDir, [System.StringComparison]::OrdinalIgnoreCase)) {
                return $true
            }
        } catch {
            # Ignore malformed PATH entries.
        }
    }
    return $false
}

function Get-SymlinkTarget($Item) {
    if (!$Item -or $Item.LinkType -ne 'SymbolicLink') {
        return $null
    }
    $target = [string](@($Item.Target)[0])
    if (!$target) {
        return $null
    }
    if (![IO.Path]::IsPathRooted($target)) {
        $target = Join-Path $Item.DirectoryName $target
    }
    return [IO.Path]::GetFullPath($target)
}

function Test-HardLinkTo($Item, [string[]]$CandidatePaths) {
    if (!$Item -or $Item.LinkType -ne 'HardLink') {
        return $false
    }
    $driveRoot = [IO.Path]::GetPathRoot($Item.FullName)
    foreach ($rawTarget in @($Item.Target)) {
        $t = [string]$rawTarget
        if ([string]::IsNullOrWhiteSpace($t)) {
            continue
        }
        if ($t.StartsWith('\') -and -not $t.StartsWith('\\') -and $driveRoot) {
            $t = Join-Path $driveRoot ($t.TrimStart('\'))
        } elseif (![IO.Path]::IsPathRooted($t)) {
            $t = Join-Path $Item.DirectoryName $t
        }
        try {
            $full = [IO.Path]::GetFullPath($t)
        } catch {
            continue
        }
        foreach ($cand in $CandidatePaths) {
            if (![string]::IsNullOrWhiteSpace($cand) -and [string]::Equals($full, $cand, [System.StringComparison]::OrdinalIgnoreCase)) {
                return $true
            }
        }
    }
    return $false
}

function Remove-CtlSymlink {
    $item = Get-Item -LiteralPath $SymlinkPath -Force -ErrorAction SilentlyContinue
    if (!$item) {
        return
    }
    $symTarget = Get-SymlinkTarget $item
    if (($symTarget -and [string]::Equals($symTarget, $CtlExe, [System.StringComparison]::OrdinalIgnoreCase)) -or
        (Test-HardLinkTo $item @($CtlExe))) {
        [IO.File]::Delete($SymlinkPath)
        Write-Step "Removed link $SymlinkPath"
    }
}

function Install-StagedFile([string]$Source, [string]$Destination, [string]$Backup, [switch]$Atomic) {
    $entry = [pscustomobject]@{
        Destination  = $Destination
        Backup       = $Backup
        OldMoved     = $false
        NewInstalled = $false
    }
    $replacements.Add($entry)
    if (Get-Item -LiteralPath $Destination -Force -ErrorAction SilentlyContinue) {
        if ($Atomic) {
            [IO.File]::Copy($Destination, $Backup)
            [IO.File]::Replace($Source, $Destination, [NullString]::Value)
            $entry.OldMoved = $true
            $entry.NewInstalled = $true
            return
        }
        [IO.File]::Move($Destination, $Backup)
        $entry.OldMoved = $true
    }
    [IO.File]::Move($Source, $Destination)
    $entry.NewInstalled = $true
}

function Restore-StagedFiles {
    for ($i = $replacements.Count - 1; $i -ge 0; $i--) {
        $entry = $replacements[$i]
        try {
            if ($entry.NewInstalled) {
                [IO.File]::Delete($entry.Destination)
            }
            if ($entry.OldMoved) {
                [IO.File]::Move($entry.Backup, $entry.Destination)
            }
        } catch {
            $_.Exception.Message
        }
    }
}

function Update-CtlSymlink([string]$PreviousCtlBackup) {
    $item = Get-Item -LiteralPath $SymlinkPath -Force -ErrorAction SilentlyContinue
    $target = Get-SymlinkTarget $item
    if ($target -and [string]::Equals($target, $CtlExe, [System.StringComparison]::OrdinalIgnoreCase)) {
        if (-not (Test-DirInPath $SymlinkDir)) {
            Write-Step "Note: $SymlinkDir is not in PATH. Add it (or $InstallDir) to PATH to invoke mcp-hub-ctl directly."
        }
        return $SymlinkPath
    }
    $isOurHardLink = Test-HardLinkTo $item @($CtlExe, $PreviousCtlBackup)
    if ($item -and !$isOurHardLink -and (!$target -or ![string]::Equals([IO.Path]::GetFileName($target), 'mcp-hub-ctl.exe', [System.StringComparison]::OrdinalIgnoreCase))) {
        Write-Warning "[mcp-hub] $SymlinkPath already exists and was kept."
        return $null
    }
    $linkKind = $null
    [void][IO.Directory]::CreateDirectory($SymlinkDir)
    try {
        New-Item -ItemType SymbolicLink -Path $linkNew -Target $CtlExe | Out-Null
        $linkKind = 'Symlink'
    } catch {
        $symErr = $_.Exception.Message
        if (Test-SameDriveRoot $SymlinkDir $InstallDir) {
            try {
                New-Item -ItemType HardLink -Path $linkNew -Target $CtlExe | Out-Null
                $linkKind = 'HardLink'
            } catch {
                Write-Warning "[mcp-hub] Could not create symlink ($symErr) or hardlink ($($_.Exception.Message)) at $SymlinkPath."
                return $null
            }
        } else {
            Write-Warning "[mcp-hub] Could not create symlink $SymlinkPath ($symErr). Enable Developer Mode or add $InstallDir to PATH."
            return $null
        }
    }
    Install-StagedFile $linkNew $SymlinkPath $linkBackup
    Write-Step "$linkKind $SymlinkPath -> $CtlExe"
    if (-not (Test-DirInPath $SymlinkDir)) {
        Write-Step "Note: $SymlinkDir is not in PATH. Add it (or $InstallDir) to PATH to invoke mcp-hub-ctl directly."
    }
    return $SymlinkPath
}

function Stop-NewHub([uint32]$HubProcessId) {
    $process = Get-Process -Id $HubProcessId -ErrorAction SilentlyContinue
    if (!$process) {
        return
    }
    try {
        if ($process.HasExited) {
            return
        }
        $procPath = $null
        try {
            $procPath = $process.Path
        } catch {
            $procPath = $null
        }
        if ([string]::IsNullOrEmpty($procPath) -or ![string]::Equals($procPath, $HubExe, [System.StringComparison]::OrdinalIgnoreCase)) {
            throw "Could not verify the path of the new hub process (pid=$HubProcessId)."
        }
        Stop-Process -InputObject $process -Force
        if (!$process.WaitForExit(5000)) {
            throw "New hub did not exit (pid=$HubProcessId)."
        }
    } finally {
        $process.Dispose()
    }
}

function Uninstall-McpHub {
    [void](Stop-InstalledHub)
    $runValue = Get-RunValue
    if ($null -ne $runValue) {
        if ($runValue -eq $HubCmdLine) {
            Remove-ItemProperty -LiteralPath $RunKey -Name $RunValueName
            Write-Step "Removed HKCU\Run\$RunValueName"
        } else {
            Write-Warning "[mcp-hub] HKCU\Run\$RunValueName points to a different command line and was kept: $runValue"
        }
    }
    Remove-CtlSymlink
    foreach ($installedPath in @($HubExe, $CtlExe, $ReceiptPath)) {
        if (Get-Item -LiteralPath $installedPath -Force -ErrorAction SilentlyContinue) {
            [IO.File]::Delete($installedPath)
        }
    }
    if ((Test-Path -LiteralPath $InstallDir -PathType Container) -and
        !(Get-ChildItem -LiteralPath $InstallDir -Force | Select-Object -First 1)) {
        [IO.Directory]::Delete($InstallDir)
    }
    Write-Step "Removed McpHub binaries and receipt from $InstallDir (data preserved at $DataDir)."
}

function Install-McpHub {
    if (-not (Get-Command go -ErrorAction SilentlyContinue)) {
        throw 'Go toolchain ("go") was not found on PATH. Install Go 1.25+ or use a prebuilt release binary.'
    }

    $modulePath = Get-GoModulePath $PSScriptRoot
    $replacements = New-Object 'System.Collections.Generic.List[object]'
    $linkNew = "$SymlinkPath.$transactionId.new"
    $linkBackup = "$SymlinkPath.$transactionId.previous"
    $hubBackup = Join-Path $stageDir 'mcp-hub.exe.previous'
    $ctlBackup = Join-Path $stageDir 'mcp-hub-ctl.exe.previous'
    $wasRunning = $false
    $oldStopAttempted = $false
    $newPid = 0
    $runChanged = $false
    $preserveBackups = $false
    $controlAddress = $null
    $startupTimeout = 25.0

    [void][IO.Directory]::CreateDirectory($stageDir)
    try {
        $gitCommit = $null
        $gitDirty = $false
        $prevGoTmpDir = $env:GOTMPDIR
        $hadGoTmpDir = Test-Path -LiteralPath 'Env:GOTMPDIR'
        Push-Location -LiteralPath $PSScriptRoot
        try {
            $env:GOTMPDIR = $env:TEMP
            $goVersion = (& go version).Trim()

            if ((Test-Path -LiteralPath (Join-Path $PSScriptRoot '.git')) -and (Get-Command git -ErrorAction SilentlyContinue)) {
                $g = Invoke-Native -Exe 'git' -Arguments @('rev-parse', '--short', 'HEAD') -StdoutOnly
                if ($g.ExitCode -eq 0 -and -not [string]::IsNullOrWhiteSpace($g.Output)) {
                    $gitCommit = $g.Output.Trim()
                }
                $st = Invoke-Native -Exe 'git' -Arguments @('status', '--porcelain') -StdoutOnly
                if ($st.ExitCode -eq 0) {
                    $gitDirty = -not [string]::IsNullOrWhiteSpace($st.Output)
                }
            }

            $hubLdFlags = '-H=windowsgui -s -w'
            $ctlLdFlags = '-s -w'
            if (-not [string]::IsNullOrWhiteSpace($gitCommit)) {
                $commitLdFlag = "-X $modulePath/internal/hub.GitCommit=$gitCommit"
                $hubLdFlags = "$hubLdFlags $commitLdFlag"
                $ctlLdFlags = "$ctlLdFlags $commitLdFlag"
            }

            Write-Step "Building headless supervisor -> $HubNew ..."
            & go build -trimpath -ldflags $hubLdFlags -o $HubNew .\cmd\mcp-hub
            if ($LASTEXITCODE -ne 0) {
                throw "go build mcp-hub failed with exit code $LASTEXITCODE"
            }

            Write-Step "Building console CLI -> $CtlNew ..."
            & go build -trimpath -ldflags $ctlLdFlags -o $CtlNew .\cmd\mcp-hub-ctl
            if ($LASTEXITCODE -ne 0) {
                throw "go build mcp-hub-ctl failed with exit code $LASTEXITCODE"
            }
        } finally {
            if ($hadGoTmpDir) {
                $env:GOTMPDIR = $prevGoTmpDir
            } else {
                Remove-Item -LiteralPath 'Env:GOTMPDIR' -ErrorAction SilentlyContinue
            }
            Pop-Location
        }

        $version = Invoke-Native -Exe $CtlNew -Arguments @('version', '--json') -StdoutOnly
        if ($version.ExitCode -ne 0) {
            throw "staged mcp-hub-ctl version check failed ($($version.ExitCode))"
        }
        $newVersion = ($version.Output | ConvertFrom-Json).version
        if ([string]::IsNullOrWhiteSpace($newVersion)) {
            throw 'staged mcp-hub-ctl did not report a version'
        }
        Write-Step "Staged build version $newVersion"

        $wasRunning = @(Get-InstalledHubProcesses).Count -gt 0
        $shouldStart = (-not $NoStart) -and ($Start -or $wasRunning)
        if ($PostStartCheck -and !$shouldStart) {
            throw 'PostStartCheck requires a running hub or -Start (and cannot be used with -NoStart).'
        }
        if (Test-Path -LiteralPath $ConfigPath -PathType Leaf) {
            $validated = Invoke-Native -Exe $CtlNew -Arguments @('validate', '--config', $ConfigPath, '--json') -StdoutOnly
            if ($validated.ExitCode -ne 0) {
                throw "Config validation failed, see mcp-hub-ctl validate --config $ConfigPath (live hub left running)."
            }
            $configSummary = $validated.Output | ConvertFrom-Json
            $controlAddress = $configSummary.controlAddr
            $startupTimeout = [double]$configSummary.startupTimeoutSec + 10
        } elseif ($shouldStart) {
            throw "Cannot start mcp-hub: config file not found at $ConfigPath"
        } else {
            Write-Step "$ConfigPath does not exist yet. Run 'mcp-hub-ctl init' or copy config.example.json."
        }

        foreach ($path in @($DataDir, (Join-Path $DataDir 'logs'), (Join-Path $DataDir 'secrets'))) {
            Assert-NoReparseAncestor $path
            [void][IO.Directory]::CreateDirectory($path)
        }
        Set-PrivateDataTree

        if ([string]::IsNullOrWhiteSpace($controlAddress) -and @(Get-InstalledHubProcesses).Count -gt 0) {
            throw "Cannot upgrade running mcp-hub: config file not found at $ConfigPath"
        }

        $hubHash = Get-Sha256Hex $HubNew
        $ctlHash = Get-Sha256Hex $CtlNew
        $previousRunValue = Get-RunValue
        $oldStopAttempted = $wasRunning
        if (Stop-InstalledHub) {
            $wasRunning = $true
            $oldStopAttempted = $true
        }
        $shouldStart = (-not $NoStart) -and ($Start -or $wasRunning)
        if (($shouldStart -or $oldStopAttempted) -and [string]::IsNullOrWhiteSpace($controlAddress)) {
            if (Test-Path -LiteralPath $ConfigPath -PathType Leaf) {
                $validated = Invoke-Native -Exe $CtlNew -Arguments @('validate', '--config', $ConfigPath, '--json') -StdoutOnly
                if ($validated.ExitCode -ne 0) {
                    throw "Config validation failed for $ConfigPath."
                }
                $configSummary = $validated.Output | ConvertFrom-Json
                $controlAddress = $configSummary.controlAddr
                $startupTimeout = [double]$configSummary.startupTimeoutSec + 10
            } else {
                throw "Cannot start or verify running mcp-hub: config file not found at $ConfigPath"
            }
        }

        Install-StagedFile $HubNew $HubExe $hubBackup
        Install-StagedFile $CtlNew $CtlExe $ctlBackup

        if ($shouldStart) {
            $newPid = Start-InstalledHub
            Write-Step "Started supervisor (pid=$newPid), waiting for /healthz ..."
            if (!(Wait-HubHealthy $newPid $controlAddress $startupTimeout)) {
                throw "mcp-hub $newVersion did not report healthy within $startupTimeout seconds"
            }
            Write-Step 'mcp-hub is healthy.'
            if ($PostStartCheck) {
                & $PostStartCheck $ConfigPath $newPid
            }
        }

        if (!$NoSymlink) {
            $symlinkResult = Update-CtlSymlink $ctlBackup
        } else {
            $symlinkResult = $null
            $existingLink = Get-Item -LiteralPath $SymlinkPath -Force -ErrorAction SilentlyContinue
            $existingTarget = Get-SymlinkTarget $existingLink
            if (($existingTarget -and [string]::Equals($existingTarget, $CtlExe, [System.StringComparison]::OrdinalIgnoreCase)) -or
                (Test-HardLinkTo $existingLink @($CtlExe))) {
                $symlinkResult = $SymlinkPath
            }
        }
        if ($RegisterStartup) {
            Set-ItemProperty -LiteralPath $RunKey -Name $RunValueName -Value $HubCmdLine -Type String
            $runChanged = $true
            Write-Step "Registered HKCU\Run\$RunValueName -> $HubCmdLine"
        }

        $receipt = [ordered]@{
            installed_at       = (Get-Date).ToString('o')
            version            = $newVersion
            git_commit         = $gitCommit
            git_dirty          = $gitDirty
            source_dir         = $PSScriptRoot
            install_dir        = $InstallDir
            data_dir           = $DataDir
            config_path        = $ConfigPath
            go_version         = $goVersion
            startup_registered = (Get-RunValue) -eq $HubCmdLine
            symlink_path       = $symlinkResult
            binaries           = [ordered]@{
                'mcp-hub.exe'     = [ordered]@{ path = $HubExe; sha256 = $hubHash }
                'mcp-hub-ctl.exe' = [ordered]@{ path = $CtlExe; sha256 = $ctlHash }
            }
        }
        $receiptNew = Join-Path $stageDir 'installation.json'
        [IO.File]::WriteAllText($receiptNew, ($receipt | ConvertTo-Json -Depth 6), (New-Object Text.UTF8Encoding($false)))
        Install-StagedFile $receiptNew $ReceiptPath (Join-Path $stageDir 'installation.json.previous') -Atomic
    } catch {
        $installError = $_
        $rollbackErrors = New-Object 'System.Collections.Generic.List[string]'
        if ($newPid) {
            try {
                Stop-NewHub $newPid
            } catch {
                $rollbackErrors.Add($_.Exception.Message)
            }
        }
        if ($rollbackErrors.Count -eq 0) {
            foreach ($failure in @(Restore-StagedFiles)) {
                $rollbackErrors.Add($failure)
            }
        }
        if ($runChanged) {
            try {
                if ($null -eq $previousRunValue) {
                    Remove-ItemProperty -LiteralPath $RunKey -Name $RunValueName
                } else {
                    Set-ItemProperty -LiteralPath $RunKey -Name $RunValueName -Value $previousRunValue
                }
            } catch {
                $rollbackErrors.Add($_.Exception.Message)
            }
        }
        if ($oldStopAttempted -and $rollbackErrors.Count -eq 0) {
            try {
                if ([string]::IsNullOrWhiteSpace($controlAddress)) {
                    throw "Cannot verify previous hub health: config file not found at $ConfigPath."
                }
                $old = @(Get-InstalledHubProcesses)
                if ($old.Count -eq 0) {
                    $oldPid = Start-InstalledHub
                } else {
                    $oldPid = $old[0].ProcessId
                }
                if (!(Wait-HubHealthy $oldPid $controlAddress $startupTimeout)) {
                    throw "Previous hub (pid=$oldPid) did not report healthy."
                }
                Write-Step "Previous hub is healthy (pid=$oldPid)."
            } catch {
                $rollbackErrors.Add($_.Exception.Message)
            }
        }
        if ($rollbackErrors.Count -gt 0) {
            $preserveBackups = $true
            $detail = $rollbackErrors -join ', '
            throw "Installation failed: $($installError.Exception.Message). Rollback incomplete ($detail). Remaining backups: $stageDir and $linkBackup."
        }
        throw $installError
    } finally {
        if (!$preserveBackups) {
            foreach ($path in @($HubNew, $CtlNew, $linkNew, $linkBackup, $hubBackup, $ctlBackup,
                    (Join-Path $stageDir 'installation.json'), (Join-Path $stageDir 'installation.json.previous'))) {
                try {
                    if (Get-Item -LiteralPath $path -Force -ErrorAction SilentlyContinue) {
                        [IO.File]::Delete($path)
                    }
                } catch {
                    Write-Warning "[mcp-hub] Could not remove $path ($($_.Exception.Message))."
                }
            }
            try {
                if (Test-Path -LiteralPath $stageDir -PathType Container) {
                    [IO.Directory]::Delete($stageDir)
                }
            } catch {
                Write-Warning "[mcp-hub] Could not remove staging directory $stageDir ($($_.Exception.Message))."
            }
        }
    }
    Write-Step "Installed mcp-hub $newVersion -> $InstallDir"
    Write-Step "Receipt written -> $ReceiptPath"
}

$installMutex = New-Object System.Threading.Mutex($false, 'Global\McpHubInstaller')
$hasMutex = $false
try {
    try {
        $hasMutex = $installMutex.WaitOne(0)
    } catch [System.Threading.AbandonedMutexException] {
        $hasMutex = $true
    }
    if (!$hasMutex) {
        throw 'Another McpHub installer is running.'
    }
    Remove-StaleStageDirs
    if ($Uninstall) {
        Uninstall-McpHub
    } else {
        Install-McpHub
    }
} finally {
    if ($hasMutex) {
        $installMutex.ReleaseMutex()
    }
    $installMutex.Dispose()
}
