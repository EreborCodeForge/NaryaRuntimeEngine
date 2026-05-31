package main

// RuntimeVersion is sent on every Go→PHP request and exposed by the CLI (-version).
const RuntimeVersion = "2.0.0"

// version is used by the CLI banner and Makefile ldflags.
var version = RuntimeVersion
