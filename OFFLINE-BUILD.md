# Building without a module proxy

This repository was written in an environment with no route to
`proxy.golang.org`. That constraint left one artefact behind, and this file
explains it so nobody has to reverse-engineer it.

## What you almost certainly want

Nothing special:

```bash
go mod tidy      # writes go.sum on first run
make build
make test
```

`go.mod` is the canonical module file: ordinary import paths, no `replace`
directives, no surprises. `go.sum` is not committed, because the only checksums
that could be produced in the build environment were for the mirror paths
described below, and shipping those would break a normal build in a way that is
tedious to diagnose. `go mod tidy` regenerates it correctly against the real
proxy in a few seconds, and CI does exactly that.

## The offline overlay

`go.offline.mod` and `go.offline.sum` are a second module file carrying a
`replace` block that redirects every vanity import path to the GitHub
repository it is actually served from — `golang.org/x/sync` to
`github.com/golang/sync`, `google.golang.org/grpc` to `github.com/grpc/grpc-go`,
`go.opentelemetry.io/otel` to `github.com/open-telemetry/opentelemetry-go`, and
so on. With them, `go` needs nothing but `github.com`:

```bash
make build-offline
make test-offline
```

which is shorthand for passing `-modfile=go.offline.mod`. Go's `-modfile` flag
exists precisely for this: an alternate module file used for one invocation,
leaving the committed `go.mod` untouched.

Two consequences worth knowing:

- The dependency **versions** in `go.offline.mod` are pinned lower than the
  latest releases. Several of the newest OpenTelemetry, gRPC and `golang.org/x`
  modules declare `go 1.25`, and the build environment had Go 1.24 with no way
  to fetch a newer toolchain. The pinned set is the most recent one that builds
  on Go 1.24. On a normal machine you can raise them freely.
- Because the replacements are module-path redirects rather than directory
  replacements, `go.offline.sum` records checksums against the mirror paths.
  That is why it is a separate file rather than the committed `go.sum`.

## Keeping the two in step

`go.offline.mod` must list the same requirements as `go.mod`. If you add a
dependency, add it to both — and if the new dependency has a vanity import
path, add a `replace` for it too. `make deps-check` compares the `require`
blocks and fails if they have drifted.
