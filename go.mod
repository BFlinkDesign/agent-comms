module github.com/BFlinkDesign/agent-comms

// The oldest Go the code compiles with.
go 1.24

// The Go that builds and tests it, in CI and for every release. Without this
// line they used whatever Go the runner image had: go1.24.13 in September 2026,
// which no longer got security fixes, since Go supports each major release only
// until there are two newer ones. An older go command downloads this toolchain
// from the Go module proxy and checks it against the Go checksum database before
// using it; Renovate proposes each newer one.
toolchain go1.27.1
