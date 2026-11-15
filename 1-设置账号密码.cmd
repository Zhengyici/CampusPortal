@echo off
cd /d "%~dp0"
echo ============================================
echo    校园网自动登录 - 设置账号密码
echo ============================================
echo.
if not exist "CampusPortal.exe" (
    echo [错误] 找不到 CampusPortal.exe，请把它和本脚本放在同一个文件夹。
    pause
    exit /b 1
)
CampusPortal.exe -setup
echo.
echo --------------------------------------------
echo 设置完成后，请双击 3-设为开机自启.cmd
echo --------------------------------------------
pause