@echo off
setlocal

cd /d "%~dp0"

if not exist dist mkdir dist

echo ==^> Generating TLS certificate...
go run gencert.go
if errorlevel 1 (
    echo ERROR: certificate generation failed
    exit /b 1
)

copy /y server.crt server\ >nul
copy /y server.key server\ >nul
copy /y server.crt client\ >nul

set GOPROXY=https://goproxy.cn,direct
set GONOSUMCHECK=*
set GONOSUMDB=*

echo ==^> Building server (linux/amd64)...
cd server
if not exist go.mod go mod init pad-server
go get github.com/BurntSushi/toml@latest
go get github.com/gorilla/websocket@latest
go get github.com/prometheus/client_golang/prometheus@latest
go get github.com/prometheus/client_golang/prometheus/promhttp@latest
go get github.com/fsnotify/fsnotify@latest
go mod tidy
set GOOS=linux
set GOARCH=amd64
set CGO_ENABLED=0
go build -trimpath -ldflags="-s -w" -o ..\dist\pad-server .
if errorlevel 1 (
    echo ERROR: server build failed
    exit /b 1
)
cd ..

echo ==^> Building client (linux/amd64)...
cd client
if not exist go.mod go mod init pad-client
go get github.com/gorilla/websocket@latest
go mod tidy
set GOOS=linux
set GOARCH=amd64
set CGO_ENABLED=0
go build -trimpath -ldflags="-s -w" -o ..\dist\pad-client .
if errorlevel 1 (
    echo ERROR: client build failed
    exit /b 1
)
cd ..

del /q server\server.crt server\server.key client\server.crt 2>nul

echo.
echo Build complete. Binaries in dist\
dir dist\
echo.
echo Deploy:
echo   Server: ./pad-server -c config.toml
echo   Client: ./pad-client -addr IP:PORT -uri /abc/def -reconnect-min 5 -reconnect-max 10
echo.
echo Optional:
echo   Client with mTLS: ./pad-client -addr IP:PORT -uri /pad -reconnect-min 5 -reconnect-max 10 -mtls

endlocal
