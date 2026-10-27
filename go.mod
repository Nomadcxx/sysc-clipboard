module github.com/Nomadcxx/sysc-clipboard

// The dependency scan runs on the toolchain this directive names, and the
// standard library has to carry the fixes the scan looks for: go1.26.10 is
// the release with GO-2026-6809 and GO-2026-6617 fixed. CI reads this
// directive for the toolchain it installs, so the floor here is the floor
// everywhere.
go 1.26.10

require (
	github.com/Nomadcxx/sysc-wayland v0.3.2
	github.com/godbus/dbus/v5 v5.2.4
	golang.org/x/sys v0.49.0
)
