@echo off
setlocal
cd /d "%~dp0"
echo ============================================
echo    取消开机自启 / 停止后台运行
echo ============================================
echo.
powershell -NoProfile -Command "$l=Join-Path ([Environment]::GetFolderPath('Startup')) 'CampusPortal.lnk'; if(Test-Path $l){ Remove-Item $l -Force; Write-Host '开机启动项: 已删除' } else { Write-Host '开机启动项: 未设置' }"
powershell -NoProfile -Command "$k='HKCU:\Software\Microsoft\Windows\CurrentVersion\Run'; $k2=$k+'\LenovoDisabled'; foreach($p in @(@($k,'CampusPortal'),@($k2,'CampusPortal'))){ try{ Remove-ItemProperty -Path $p[0] -Name $p[1] -ErrorAction Stop } catch {} }; Write-Host '注册表启动项: 已清理'"
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