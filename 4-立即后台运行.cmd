@echo off
setlocal
cd /d "%~dp0"
echo ============================================
echo    校园网自动登录 - 立即在后台运行
echo ============================================
echo.
if not exist "secrets.dat" (
    echo [错误] 还没设置账号密码，请先双击 1-设置账号密码.cmd
    pause
    exit /b 1
)
tasklist /FI "IMAGENAME eq CampusPortal.exe" 2>nul | find /I "CampusPortal.exe" >nul
if not errorlevel 1 (
    echo 它已经在后台跑了，不需要重复启动。
    echo.
    pause
    exit /b 0
)
powershell -NoProfile -Command "Start-Process -FilePath (Join-Path '%~dp0' 'CampusPortal.exe') -WorkingDirectory '%~dp0'"
ping -n 4 127.0.0.1 >nul
tasklist /FI "IMAGENAME eq CampusPortal.exe" 2>nul | find /I "CampusPortal.exe" >nul
if not errorlevel 1 (
    echo 已在后台启动，无窗口，默默守护。
) else (
    echo [警告] 启动似乎失败，请把 portal.log 发给助手看看。
)
echo.
echo 查看状态：双击 2-检查状态.cmd
echo.
pause