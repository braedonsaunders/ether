module github.com/braedonsaunders/ether

go 1.27.3

require (
	fyne.io/systray v1.12.2
	github.com/PuerkitoBio/goquery v1.13.0
	github.com/SherClockHolmes/webpush-go v1.4.0
	github.com/andybalholm/cascadia v1.3.5
	github.com/coder/websocket v1.8.15
	github.com/dlclark/regexp2 v1.12.0
	github.com/evanw/esbuild v0.28.2
	github.com/fastschema/qjs v0.0.6
	github.com/go-chi/chi/v5 v5.5.0
	github.com/google/uuid v1.6.0
	github.com/gorilla/websocket v1.5.3
	github.com/jackc/pgx/v5 v5.12.0
	github.com/kardianos/service v1.3.0
	github.com/mnightingale/rapidyenc v0.0.0-20261008131718-6657b7ca854e
	github.com/shirou/gopsutil/v4 v4.26.10
	github.com/skip2/go-qrcode v0.0.0-20200617195104-da1b6568686e
	github.com/srwiley/oksvg v0.0.0-20221011165216-be6e8873101c
	github.com/srwiley/rasterx v0.0.0-20220730225603-2ab79fcdd4ef
	github.com/tetratelabs/wazero v1.12.0
	github.com/ulikunitz/xz v0.5.17
	go.yaml.in/yaml/v3 v3.0.6
	golang.org/x/crypto v0.58.0
	golang.org/x/image v0.47.0
	golang.org/x/net v0.62.0
	golang.org/x/sys v0.49.0
	golang.org/x/text v0.43.0
	modernc.org/sqlite v1.61.1
)

require (
	github.com/dustin/go-humanize v1.1.0 // indirect
	github.com/ebitengine/purego v0.11.2 // indirect
	github.com/go-ole/go-ole v1.2.6 // indirect
	github.com/godbus/dbus/v5 v5.1.0 // indirect
	github.com/golang-jwt/jwt/v5 v5.2.2 // indirect
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.3 // indirect
	github.com/lufia/plan9stats v0.0.0-20211012122336-39d0f177ccd0 // indirect
	github.com/mattn/go-isatty v0.0.24 // indirect
	github.com/ncruces/go-strftime v1.1.0 // indirect
	github.com/power-devops/perfstat v0.0.0-20260805114148-88456608a4f6 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	github.com/tklauser/go-sysconf v0.4.0 // indirect
	github.com/tklauser/numcpus v0.12.0 // indirect
	github.com/yusufpapurcu/wmi v1.2.4 // indirect
	golang.org/x/sync v0.24.0 // indirect
	modernc.org/libc v1.79.0 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.12.1 // indirect
)

replace github.com/fastschema/qjs => ./third_party/qjs
