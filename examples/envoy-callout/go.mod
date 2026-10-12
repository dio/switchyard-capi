module github.com/dio/switchyard-capi/examples/envoy-callout

go 1.25.6

require (
	github.com/dio/switchyard-capi/go v0.0.0
	github.com/envoyproxy/envoy/source/extensions/dynamic_modules v0.0.0-20260521055639-0d6e3c60aa55
)

replace github.com/dio/switchyard-capi/go => ../../go
