@echo off
setlocal
cd /d "%~dp0"
echo ============================================
echo    取消开机自启 / 停止后台运行
echo ============================================
echo.
reg delete "HKCU\Software\Microsoft\Windows\CurrentVersion\Run" /v CampusPortal /f >nul 2>&1
if errorlevel 1 (
    echo 注册表启动项: 本来就没设置
) else (
    echo 注册表启动项: 已删除
)
powershell -NoProfile -Command "$l=Join-Path ([Environment]::GetFolderPath('Startup')) 'CampusPortal.lnk'; if(Test-Path $l){ Remove-Item $l -Force; Write-Host '  旧启动文件夹快捷方式: 已删除' }"
echo.
tasklist /FI "IMAGENAME eq CampusPortal.exe" 2>nul | find /I "CampusPortal.exe" >nul
if errorlevel 1 (
    echo 后台进程: 未运行
) else (
    taskkill /IM CampusPortal.exe /F >nul 2>&1
    echo 后台进程: 已停止
)
echo.
echo 如果还想彻底清理，直接删除整个文件夹即可。
echo.
pause