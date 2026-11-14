param(
    [Parameter(Mandatory = $true)]
    [ValidateSet('new', 'status', 'migrate', 'seed', 'backup', 'restore', 'prune')]
    [string]$Action,
    [string]$Name,
    [int]$To = 0,
    [string]$BackupDir = $(if ($env:MEETEAM_BACKUP_DIR) { $env:MEETEAM_BACKUP_DIR } else { Join-Path $env:TEMP 'meeteam-quest7-backups' }),
    [string]$RestoreDatabase,
    [string]$BackupName,
    [int]$RetentionDays = 14,
    [switch]$Apply
)

$ErrorActionPreference = 'Stop'
$repo = (Resolve-Path (Join-Path $PSScriptRoot '..')).Path
$migrationDir = Join-Path $repo 'db/migrations'
$pgBin = if ($env:PG_BIN) { $env:PG_BIN } else { 'C:\Program Files\PostgreSQL\17\bin' }
$env:PGHOST = if ($env:PGHOST) { $env:PGHOST } else { '127.0.0.1' }
$env:PGPORT = if ($env:PGPORT) { $env:PGPORT } else { '55432' }
$env:PGUSER = if ($env:PGUSER) { $env:PGUSER } else { 'meeteam' }
$env:PGDATABASE = if ($env:PGDATABASE) { $env:PGDATABASE } else { 'meeteam_quest7' }

function Pg([string]$Tool, [string[]]$Arguments) {
    $exe = Join-Path $pgBin "$Tool.exe"
    if (-not (Test-Path -LiteralPath $exe)) {
        $cmd = Get-Command $Tool -ErrorAction Stop
        $exe = $cmd.Source
    }
    $output = & $exe @Arguments 2>&1
    if ($LASTEXITCODE -ne 0) { throw "$Tool failed (exit $LASTEXITCODE): $($output -join ' ')" }
    return $output
}

function Sql([string]$Database, [string]$Query) {
    return Pg 'psql' @('-X', '-v', 'ON_ERROR_STOP=1', '-At', '-F', '|', '-d', $Database, '-c', $Query)
}

function EnsureMigrationTable {
    Sql $env:PGDATABASE 'CREATE TABLE IF NOT EXISTS schema_migrations (version text PRIMARY KEY, checksum_sha256 char(64) NOT NULL, applied_at timestamptz NOT NULL DEFAULT now());' | Out-Null
}

function MigrationFiles {
    return @(Get-ChildItem -LiteralPath $migrationDir -Filter '*.sql' -File | Sort-Object Name)
}

function AppliedMigrations {
    $rows = @(Sql $env:PGDATABASE 'SELECT version, checksum_sha256 FROM schema_migrations ORDER BY version;')
    $map = @{}
    foreach ($row in $rows) {
        $parts = ([string]$row).Split('|')
        if ($parts.Count -eq 2) { $map[$parts[0]] = $parts[1].Trim() }
    }
    return $map
}

function ValidateBackup([string]$Directory) {
    $manifestPath = Join-Path $Directory 'manifest.json'
    if (-not (Test-Path -LiteralPath $manifestPath)) { throw "No manifest: $Directory" }
    $manifest = Get-Content -Raw -LiteralPath $manifestPath | ConvertFrom-Json
    $dumpPath = Join-Path $Directory 'database.dump'
    if (-not (Test-Path -LiteralPath $dumpPath)) { throw "No database dump: $Directory" }
    $hash = (Get-FileHash -LiteralPath $dumpPath -Algorithm SHA256).Hash
    if ($hash -ne $manifest.database_sha256) { throw "Dump checksum mismatch: $Directory" }
    Pg 'pg_restore' @('--list', $dumpPath) | Out-Null
    if ($manifest.state_sha256) {
        $statePath = Join-Path $Directory 'state.json'
        if (-not (Test-Path -LiteralPath $statePath) -or (Get-FileHash -LiteralPath $statePath -Algorithm SHA256).Hash -ne $manifest.state_sha256) {
            throw "State snapshot checksum mismatch: $Directory"
        }
    }
    return $manifest
}

switch ($Action) {
    'new' {
        if ($Name -notmatch '^[a-z][a-z0-9_]*$') { throw 'Use -Name with lowercase letters, digits and underscores.' }
        $files = MigrationFiles
        $next = if ($files.Count) { [int]$files[-1].BaseName.Substring(0, 4) + 1 } else { 1 }
        $path = Join-Path $migrationDir ('{0:D4}_{1}.sql' -f $next, $Name)
        Set-Content -LiteralPath $path -Value "-- $Name`n-- Add SQL statements here before applying this migration." -Encoding utf8 -NoNewline
        Write-Output $path
    }
    'status' {
        EnsureMigrationTable
        $applied = AppliedMigrations
        foreach ($file in (MigrationFiles)) {
            $hash = (Get-FileHash -LiteralPath $file.FullName -Algorithm SHA256).Hash
            $state = if ($applied.ContainsKey($file.Name)) { if ($applied[$file.Name] -eq $hash) { 'applied' } else { 'CHECKSUM MISMATCH' } } else { 'pending' }
            Write-Output "$state`t$($file.Name)`t$hash"
        }
        Write-Output 'Database ledger:'
        Sql $env:PGDATABASE 'SELECT version, checksum_sha256, applied_at FROM schema_migrations ORDER BY version;'
    }
    'migrate' {
        EnsureMigrationTable
        $applied = AppliedMigrations
        foreach ($file in (MigrationFiles)) {
            if ($file.Name -notmatch '^([0-9]{4})_[a-z][a-z0-9_]*\.sql$') { throw "Invalid migration name: $($file.Name)" }
            $version = [int]$Matches[1]
            $hash = (Get-FileHash -LiteralPath $file.FullName -Algorithm SHA256).Hash
            if ($applied.ContainsKey($file.Name)) {
                if ($applied[$file.Name] -ne $hash) { throw "Applied migration was edited: $($file.Name)" }
                Write-Output "already applied: $($file.Name)"
                continue
            }
            if ($To -gt 0 -and $version -gt $To) { continue }
            $record = "INSERT INTO schema_migrations(version, checksum_sha256) VALUES ('$($file.Name)', '$hash');"
            Pg 'psql' @('-X', '-v', 'ON_ERROR_STOP=1', '--single-transaction', '-d', $env:PGDATABASE, '-f', $file.FullName, '-c', $record) | Out-Null
            Write-Output "applied: $($file.Name)"
        }
    }
    'seed' {
        Pg 'psql' @('-X', '-v', 'ON_ERROR_STOP=1', '-d', $env:PGDATABASE, '-f', (Join-Path $repo 'db/seed/quest7_events.sql')) | Out-Null
        Sql $env:PGDATABASE 'SELECT id, title FROM events ORDER BY id;'
    }
    'backup' {
        New-Item -ItemType Directory -Force -Path $BackupDir | Out-Null
        $root = [IO.Path]::GetFullPath((Resolve-Path -LiteralPath $BackupDir).Path)
        $stamp = (Get-Date).ToUniversalTime().ToString('yyyyMMddTHHmmssfffZ')
        $directory = Join-Path $root "meeteam-$stamp"
        New-Item -ItemType Directory -Path $directory | Out-Null
        $dumpPath = Join-Path $directory 'database.dump'
        Pg 'pg_dump' @('-F', 'c', '-f', $dumpPath, $env:PGDATABASE) | Out-Null
        Pg 'pg_restore' @('--list', $dumpPath) | Out-Null
        $stateHash = $null
        $stateSource = Join-Path $repo 'data/state.json'
        if (Test-Path -LiteralPath $stateSource) {
            $statePath = Join-Path $directory 'state.json'
            Copy-Item -LiteralPath $stateSource -Destination $statePath
            $stateHash = (Get-FileHash -LiteralPath $statePath -Algorithm SHA256).Hash
        }
        $manifest = [ordered]@{
            created_at = (Get-Date).ToUniversalTime().ToString('o')
            database = $env:PGDATABASE
            database_sha256 = (Get-FileHash -LiteralPath $dumpPath -Algorithm SHA256).Hash
            state_sha256 = $stateHash
        }
        $manifest | ConvertTo-Json | Set-Content -LiteralPath (Join-Path $directory 'manifest.json') -Encoding utf8
        Write-Output "backup: $directory"
    }
    'restore' {
        if ($RestoreDatabase -notmatch '^meeteam_[a-z0-9_]+$' -or $RestoreDatabase -eq $env:PGDATABASE) {
            throw 'Use a new separate database name with -RestoreDatabase meeteam_... .'
        }
        if (-not $BackupName -or $BackupName -notmatch '^meeteam-[0-9]{8}T[0-9]{9}Z$') { throw 'Supply -BackupName from backup output.' }
        $root = [IO.Path]::GetFullPath((Resolve-Path -LiteralPath $BackupDir).Path)
        $directory = Join-Path $root $BackupName
        ValidateBackup $directory | Out-Null
        $exists = @(Sql 'postgres' "SELECT 1 FROM pg_database WHERE datname = '$RestoreDatabase';")
        if ($exists.Count) { throw "Restore database already exists: $RestoreDatabase" }
        Pg 'createdb' @('-T', 'template0', $RestoreDatabase) | Out-Null
        Pg 'pg_restore' @('--exit-on-error', '--no-owner', '-d', $RestoreDatabase, (Join-Path $directory 'database.dump')) | Out-Null
        Write-Output "restored: $RestoreDatabase from $BackupName"
        Sql $RestoreDatabase 'SELECT id, title, team_eligibility FROM events ORDER BY id;'
        if (Test-Path -LiteralPath (Join-Path $directory 'state.json')) {
            Write-Output "PoC state snapshot is in $directory\state.json; restore it separately to a test STATE_FILE, never over the running file."
        }
    }
    'prune' {
        if ($RetentionDays -lt 1) { throw 'RetentionDays must be at least 1.' }
        $root = [IO.Path]::GetFullPath((Resolve-Path -LiteralPath $BackupDir).Path)
        $dirs = @(Get-ChildItem -LiteralPath $root -Directory | Where-Object { $_.Name -match '^meeteam-[0-9]{8}T[0-9]{9}Z$' } | Sort-Object Name)
        if ($dirs.Count -eq 0) { throw 'No backups found.' }
        $valid = @($dirs | Where-Object { try { ValidateBackup $_.FullName | Out-Null; $true } catch { Write-Warning $_.Exception.Message; $false } })
        if ($valid.Count -eq 0) { throw 'No valid backup; refusing cleanup.' }
        $newest = $valid[-1].FullName
        $cutoff = (Get-Date).ToUniversalTime().AddDays(-$RetentionDays)
        foreach ($dir in $valid) {
            $stamp = [datetime]::ParseExact($dir.Name.Substring(8), 'yyyyMMddTHHmmssfffZ', [Globalization.CultureInfo]::InvariantCulture, [Globalization.DateTimeStyles]::AssumeUniversal)
            if ($dir.FullName -eq $newest -or $stamp -ge $cutoff) { Write-Output "KEEP`t$($dir.Name)"; continue }
            $full = [IO.Path]::GetFullPath($dir.FullName)
            if (-not $full.StartsWith($root + [IO.Path]::DirectorySeparatorChar, [StringComparison]::OrdinalIgnoreCase)) { throw "Unsafe cleanup path: $full" }
            if ($Apply) { Remove-Item -LiteralPath $full -Recurse -Force; Write-Output "DELETE`t$($dir.Name)" }
            else { Write-Output "WOULD DELETE`t$($dir.Name)" }
        }
    }
}
