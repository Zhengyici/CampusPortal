@echo off
setlocal
cd /d "%~dp0"
echo ============================================
echo    校园网自动登录 - 检查状态
echo ============================================
echo.
echo [后台进程]
tasklist /FI "IMAGENAME eq CampusPortal.exe" 2>nul | find /I "CampusPortal.exe"
if errorlevel 1 echo   未在运行
echo.
echo [开机自启]
powershell -NoProfile -Command "$l=Join-Path ([Environment]::GetFolderPath('Startup')) 'CampusPortal.lnk'; if(Test-Path $l){ Write-Host '   已设置' } else { Write-Host '   未设置' }"
echo.
echo [诊断]
CampusPortal.exe -diag
echo.
echo --------------------------------------------
echo 最近日志（最后 15 行）:
echo --------------------------------------------
powershell -NoProfile -Command "Get-Content '%~dp0portal.log' -Tail 15 -ErrorAction SilentlyContinue"
echo.
pause