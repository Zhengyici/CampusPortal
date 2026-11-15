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
echo 正在创建开机启动快捷方式...
powershell -NoProfile -ExecutionPolicy Bypass -Command "$d='%~dp0'.TrimEnd('\'); $s=[Environment]::GetFolderPath('Startup'); $l=Join-Path $s 'CampusPortal.lnk'; $w=New-Object -ComObject WScript.Shell; $k=$w.CreateShortcut($l); $k.TargetPath=Join-Path $d 'CampusPortal.exe'; $k.WorkingDirectory=$d; $k.WindowStyle=7; $k.Description='校园网自动登录守护'; $k.Save(); if(Test-Path $l){'OK'}else{'FAIL'}"
echo.
powershell -NoProfile -Command "$l=Join-Path ([Environment]::GetFolderPath('Startup')) 'CampusPortal.lnk'; if(Test-Path $l){ $w=New-Object -ComObject WScript.Shell; $k=$w.CreateShortcut($l); Write-Host ('启动项: ' + $k.TargetPath) } else { Write-Host '启动项: 创建失败' }"
echo.
echo 成功：以后开机登录 Windows，它会自动在后台静默运行，不弹窗。
echo.
tasklist /FI "IMAGENAME eq CampusPortal.exe" 2>nul | find /I "CampusPortal.exe" >nul
if not errorlevel 1 (
    echo 它现在已经在后台运行了。
    goto done
)
echo 现在就在后台启动它吗？按任意键启动，直接关窗口则跳过
pause >nul
powershell -NoProfile -Command "Start-Process -FilePath (Join-Path '%~dp0' 'CampusPortal.exe') -WorkingDirectory '%~dp0'"
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