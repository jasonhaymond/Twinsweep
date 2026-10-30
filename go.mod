module twinsweep

go 1.27.0

// Build with Go 1.27.2 or newer: 1.27.0 has net/http fixes missing
// (GO-2026-6611/6612/6613/6617). With the default GOTOOLCHAIN=auto, older Go
// installs download and use this toolchain automatically.
toolchain go1.27.2

require golang.org/x/sys v0.49.0
