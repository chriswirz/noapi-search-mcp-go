@echo off
rem Build noapi-search-mcp on Windows.
rem
rem This is a thin wrapper around build.ps1, which does the actual work. It
rem exists so the build can be started by double-clicking or from a plain
rem cmd.exe prompt, where running a .ps1 directly is awkward: the default
rem execution policy refuses unsigned scripts, so -ExecutionPolicy Bypass is
rem passed for this one process rather than asking anyone to change a
rem machine-wide setting.
rem
rem Every argument is forwarded, so all of build.ps1's switches work here:
rem
rem   build.cmd                       build for this machine
rem   build.cmd -All                  cross-compile every released target
rem   build.cmd -All -Version 1.2.0   ...stamped as version 1.2.0
rem   build.cmd -SkipTests            rebuild quickly, no format/vet/test
rem   build.cmd -SelfTest             then check the live services still match
rem   build.cmd -Clean                remove dist/ and the binaries first
rem
rem PowerShell 7 is preferred when it is installed, since that is what the
rem script is developed against; Windows PowerShell 5.1 runs it fine and is
rem the fallback, being present on every Windows machine.

setlocal

set "SCRIPT=%~dp0build.ps1"

where pwsh >nul 2>&1
if %ERRORLEVEL% equ 0 (
    pwsh -NoProfile -ExecutionPolicy Bypass -File "%SCRIPT%" %*
) else (
    powershell -NoProfile -ExecutionPolicy Bypass -File "%SCRIPT%" %*
)

rem The script's exit code is the build's exit code, so CI and callers can act
rem on it. endlocal would otherwise reset ERRORLEVEL before it is read.
endlocal & exit /b %ERRORLEVEL%
