@echo off
setlocal
cd /d "%~dp0"
echo ============================================
echo    校园网自动登录 - 设为开机自启
echo ============================================
echo.
if not exist "CampusPortal.exe" (
    echo [错误] 找不到 CampusPortal.exe，请把它和本脚本放在同一个文件夹。
    pause
    exit /b 1
)
if not exist "secrets.dat" (
    echo [错误] 还没设置账号密码，请先双击 1-设置账号密码.cmd
    pause
    exit /b 1
)

echo 正在写入注册表启动项...
reg add "HKCU\Software\Microsoft\Windows\CurrentVersion\Run" /v CampusPortal /t REG_SZ /d "\"%~dp0CampusPortal.exe\"" /f >nul
if errorlevel 1 (
    echo [错误] 写入注册表失败。
    pause
    exit /b 1
)

echo 清理旧的启动文件夹快捷方式...
powershell -NoProfile -Command "$l=Join-Path ([Environment]::GetFolderPath('Startup')) 'CampusPortal.lnk'; if(Test-Path $l){ Remove-Item $l -Force; Write-Host '  旧快捷方式: 已删除' } else { Write-Host '  旧快捷方式: 无' }"

echo.
reg query "HKCU\Software\Microsoft\Windows\CurrentVersion\Run" /v CampusPortal
echo.
echo 成功：以后开机登录 Windows，它会自动在后台静默运行，不弹窗、不打扰。
echo.
echo 注意：请保持本文件夹位置不变。一旦挪动，请重新运行本脚本。
echo.
tasklist /FI "IMAGENAME eq CampusPortal.exe" 2>nul | find /I "CampusPortal.exe" >nul
if not errorlevel 1 (
    echo 它现在已经在后台运行了。
    goto done
)
echo 现在就在后台启动它吗？按任意键启动，直接关窗口则跳过
pause >nul
start "" "%~dp0CampusPortal.exe"
ping -n 4 127.0.0.1 >nul
tasklist /FI "IMAGENAME eq CampusPortal.exe" 2>nul | find /I "CampusPortal.exe" >nul
if not errorlevel 1 (
    echo 已启动，后台运行中。
) else (
    echo [警告] 启动似乎失败，请把 portal.log 发给助手看看。
)
:done
echo.
echo 查看运行状态：双击 2-检查状态.cmd
echo 取消开机自启：双击 5-取消开机自启并停止.cmd
echo.
pause