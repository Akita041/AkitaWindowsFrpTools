@echo off
setlocal
set "ROOT=%~dp0"

echo Cleaning AkitaFrp build outputs under:
echo   %ROOT%

if exist "%ROOT%Server\bin\" rmdir /s /q "%ROOT%Server\bin"
if exist "%ROOT%Server\obj\" rmdir /s /q "%ROOT%Server\obj"
if exist "%ROOT%Client\bin\" rmdir /s /q "%ROOT%Client\bin"
if exist "%ROOT%Client\obj\" rmdir /s /q "%ROOT%Client\obj"

if exist "%ROOT%Server\publish\" rmdir /s /q "%ROOT%Server\publish"
if exist "%ROOT%Client\publish\" rmdir /s /q "%ROOT%Client\publish"

echo Done. Build outputs and generated configuration files were removed.
endlocal
