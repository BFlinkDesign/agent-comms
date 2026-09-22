module github.com/BFlinkDesign/agent-comms

// A minor-version floor rather than a patch pin. A patch pin makes every build
// fetch that exact toolchain, which is a network dependency the collector does
// not otherwise have; a floor lets the runner's preinstalled Go satisfy it.
go 1.24
