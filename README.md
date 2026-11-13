# https-chat.mateflowlabs.com-
Chatroom Fix

The chat service and its page, written in Go with the standard library only.

| Path | What it is |
| --- | --- |
| `main.go`, `messages.go`, `bots.go`, `ws.go` | The service: REST API, WebSocket, AI bots |
| `web/` | The page logic, compiled to WebAssembly |
| `index.html`, `mateflow-schemes.css` | Page markup and themes |
| `main_test.go` | Tests for the service |

## Build

Needs Go 1.18 or newer and nothing else.

```sh
go build -o mfchat .
GOOS=js GOARCH=wasm go build -ldflags="-s -w" -o main.wasm ./web
cp "$(go env GOROOT)/lib/wasm/wasm_exec.js" .   # Go 1.23 and older: misc/wasm/wasm_exec.js
```

`wasm_exec.js` ships with Go and starts `main.wasm` in the browser. It and the
few loader lines at the end of `index.html` are the only JavaScript the page
has, because a browser cannot start WebAssembly any other way. Always copy it
from the same Go version that built `main.wasm`.

## Test

```sh
go test -race ./...
go vet ./... && GOOS=js GOARCH=wasm go vet ./web
```

## Deploy

1. Replace the service binary with `mfchat` and restart it (`systemctl restart mfchat`).
2. Put `index.html`, `main.wasm`, `wasm_exec.js` and `mateflow-schemes.css`
   together in the folder the site is served from.

The service reads `MFCHAT_ADDR`, `MFCHAT_KEYS` and `MFCHAT_DATA` as before.
`MFCHAT_OLLAMA` is optional (default `http://127.0.0.1:11434`). Which bots are
switched off is kept in `bots.json`, next to the messages file.

## Nginx

Nginx sits in front of the Go service: it terminates TLS, serves the static
files and passes the API and the WebSocket through. The Go service itself
listens on localhost only. An example to adapt (folder and port are yours):

```nginx
root /var/www/mfchat;
gzip on;
gzip_types application/wasm text/css application/javascript;  # main.wasm: 2.4 MB down to 0.7 MB

location ~ ^/(api|media)/ {
    proxy_pass http://127.0.0.1:8080;
}
location = /ws {
    proxy_pass http://127.0.0.1:8080;
    proxy_http_version 1.1;
    proxy_set_header Upgrade $http_upgrade;
    proxy_set_header Connection "upgrade";
}
```

The service pings every open WebSocket each 25 seconds, so Nginx's default
60-second idle timeout does not cut quiet rooms off. If the site sends a
`Content-Security-Policy` header, its `script-src` needs `'wasm-unsafe-eval'`.
