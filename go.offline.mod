module github.com/udaykishore/ttl-aware-bff

go 1.24.0

require (
	github.com/golang-jwt/jwt/v5 v5.3.1
	github.com/redis/go-redis/v9 v9.22.0
	go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc v0.65.0
	go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp v0.65.0
	go.opentelemetry.io/otel v1.40.0
	go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc v1.40.0
	go.opentelemetry.io/otel/exporters/otlp/otlptrace v1.40.0
	go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc v1.40.0
	go.opentelemetry.io/otel/metric v1.40.0
	go.opentelemetry.io/otel/sdk v1.40.0
	go.opentelemetry.io/otel/sdk/metric v1.40.0
	go.opentelemetry.io/otel/trace v1.40.0
	golang.org/x/sync v0.19.0
	golang.org/x/time v0.13.0
	google.golang.org/grpc v1.80.0
	google.golang.org/protobuf v1.36.12
	gopkg.in/yaml.v3 v3.0.1
)

require (
	github.com/beorn7/perks v1.0.1 // indirect
	github.com/cenkalti/backoff/v5 v5.0.3 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/felixge/httpsnoop v1.0.4 // indirect
	github.com/go-logr/logr v1.4.3 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/grpc-ecosystem/grpc-gateway/v2 v2.28.0 // indirect
	github.com/munnerz/goautoneg v0.0.0-20191010083416-a7dc8b61c822 // indirect
	github.com/prometheus/client_golang v1.23.2 // indirect
	github.com/prometheus/client_model v0.6.2 // indirect
	github.com/prometheus/common v0.67.5 // indirect
	github.com/prometheus/otlptranslator v1.0.0 // indirect
	github.com/prometheus/procfs v0.19.2 // indirect
	go.opentelemetry.io/auto/sdk v1.2.1 // indirect
	go.opentelemetry.io/proto/otlp v1.9.0 // indirect
	go.uber.org/atomic v1.11.0 // indirect
	golang.org/x/net v0.50.0 // indirect
	golang.org/x/sys v0.41.0 // indirect
	golang.org/x/text v0.34.0 // indirect
	google.golang.org/genproto/googleapis/api v0.0.0-20260209200024-4cfbd4190f57 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260209200024-4cfbd4190f57 // indirect
)

replace (
	go.opentelemetry.io/auto/sdk => github.com/open-telemetry/opentelemetry-go-instrumentation/sdk v1.2.1
	go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc => github.com/open-telemetry/opentelemetry-go-contrib/instrumentation/google.golang.org/grpc/otelgrpc v0.65.0
	go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp => github.com/open-telemetry/opentelemetry-go-contrib/instrumentation/net/http/otelhttp v0.65.0
	go.opentelemetry.io/otel => github.com/open-telemetry/opentelemetry-go v1.40.0
	go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc => github.com/open-telemetry/opentelemetry-go/exporters/otlp/otlpmetric/otlpmetricgrpc v1.40.0
	go.opentelemetry.io/otel/exporters/otlp/otlptrace => github.com/open-telemetry/opentelemetry-go/exporters/otlp/otlptrace v1.40.0
	go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc => github.com/open-telemetry/opentelemetry-go/exporters/otlp/otlptrace/otlptracegrpc v1.40.0
	go.opentelemetry.io/otel/metric => github.com/open-telemetry/opentelemetry-go/metric v1.40.0
	go.opentelemetry.io/otel/sdk => github.com/open-telemetry/opentelemetry-go/sdk v1.40.0
	go.opentelemetry.io/otel/sdk/metric => github.com/open-telemetry/opentelemetry-go/sdk/metric v1.40.0
	go.opentelemetry.io/otel/trace => github.com/open-telemetry/opentelemetry-go/trace v1.40.0
	go.opentelemetry.io/proto/otlp => github.com/open-telemetry/opentelemetry-proto-go/otlp v1.10.0
	go.uber.org/atomic => github.com/uber-go/atomic v1.11.0
	go.uber.org/goleak => github.com/uber-go/goleak v1.3.0
	go.uber.org/multierr => github.com/uber-go/multierr v1.11.0
	golang.org/x/exp => github.com/golang/exp v0.0.0-20250911091902-df9299821621
	golang.org/x/net => github.com/golang/net v0.50.0
	golang.org/x/sync => github.com/golang/sync v0.10.0
	golang.org/x/sys => github.com/golang/sys v0.38.0
	golang.org/x/text => github.com/golang/text v0.30.0
	golang.org/x/time => github.com/golang/time v0.13.0
	google.golang.org/genproto/googleapis/api => github.com/googleapis/go-genproto/googleapis/api v0.0.0-20250929231259-57b25ae835d4
	google.golang.org/genproto/googleapis/rpc => github.com/googleapis/go-genproto/googleapis/rpc v0.0.0-20250929231259-57b25ae835d4
	google.golang.org/grpc => github.com/grpc/grpc-go v1.80.0
	google.golang.org/protobuf => github.com/protocolbuffers/protobuf-go v1.36.12
	gopkg.in/check.v1 => github.com/go-check/check v0.0.0-20201130134442-10cb98267c6c
	gopkg.in/yaml.v3 => github.com/go-yaml/yaml v0.0.0-20220527083530-f6f7691b1fde
)
