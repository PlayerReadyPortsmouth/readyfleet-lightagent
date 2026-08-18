module github.com/playerreadyportsmouth/readyfleet/agent

go 1.25.0

require (
	github.com/gorilla/websocket v1.5.3
	github.com/playerreadyportsmouth/readyfleet/proto v0.0.0-00010101000000-000000000000
)

require (
	github.com/aymanbagabas/go-pty v0.2.3
	github.com/google/uuid v1.6.0
	github.com/jchv/go-webview2 v0.0.0-20260205173254-56598839c808
	github.com/yusufpapurcu/wmi v1.2.4
	golang.org/x/sys v0.44.0
	gopkg.in/natefinch/lumberjack.v2 v2.2.1
)

require (
	github.com/creack/pty v1.1.24 // indirect
	github.com/go-ole/go-ole v1.2.6 // indirect
	github.com/jchv/go-winloader v0.0.0-20250406163304-c1995be93bd1 // indirect
	github.com/u-root/u-root v0.16.0 // indirect
	golang.org/x/crypto v0.51.0 // indirect
)

replace github.com/playerreadyportsmouth/readyfleet/proto => ../proto
