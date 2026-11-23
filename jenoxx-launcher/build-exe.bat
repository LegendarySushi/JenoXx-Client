@echo off
cd /d "%~dp0"
rmdir /s /q modsrc 2>nul
mkdir modsrc
type nul > modsrc\.keep
copy /y ..\jenoxx-client\build.gradle modsrc\ >nul
copy /y ..\jenoxx-client\gradle.properties modsrc\ >nul
copy /y ..\jenoxx-client\settings.gradle modsrc\ >nul
xcopy /e /i /q ..\jenoxx-client\src modsrc\src >nul
if defined JENOXX_CLIENT_ID (echo %JENOXX_CLIENT_ID%> client_id.txt) else (echo.> client_id.txt)
set GOOS=windows
set GOARCH=amd64
go build -ldflags="-s -w -H=windowsgui" -o JenoXxClient.exe .
echo Fertig: JenoXxClient.exe
