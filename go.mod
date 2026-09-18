module github.com/Taurine-Technology/axon-pulse

go 1.26.7

require (
	github.com/Microsoft/go-winio v0.6.2
	github.com/Taurine-Technology/axon-contracts/gen/go v0.4.1
	github.com/mdlayher/wifi v0.8.0
	go.uber.org/goleak v1.3.0
	golang.org/x/mod v0.40.0
	golang.org/x/sys v0.47.0
	modernc.org/sqlite v1.56.0
)

require (
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/google/go-cmp v0.7.0 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/klauspost/compress v1.18.7 // indirect
	github.com/mattn/go-isatty v0.0.24 // indirect
	github.com/mdlayher/genetlink v1.4.0 // indirect
	github.com/mdlayher/netlink v1.11.2 // indirect
	github.com/mdlayher/socket v0.6.0 // indirect
	github.com/ncruces/go-strftime v1.0.0 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	golang.org/x/crypto v0.56.0 // indirect
	golang.org/x/net v0.57.0 // indirect
	golang.org/x/sync v0.22.0 // indirect
	golang.org/x/text v0.41.0 // indirect
	modernc.org/libc v1.74.4 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.11.0 // indirect
)

replace github.com/Taurine-Technology/axon-contracts/gen/go => ./third_party/axon-contracts
