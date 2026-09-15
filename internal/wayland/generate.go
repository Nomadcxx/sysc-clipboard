// Package wayland contains generated data-control protocol bindings.
package wayland

//go:generate go run github.com/Nomadcxx/sysc-wayland/cmd/sysc-wayland-scanner@v0.2.1 -pkg wayland -o ext_data_control.go -i ../../protocols/ext-data-control-v1.xml
//go:generate go run github.com/Nomadcxx/sysc-wayland/cmd/sysc-wayland-scanner@v0.2.1 -pkg wayland -o wlr_data_control.go -i ../../protocols/wlr-data-control-unstable-v1.xml
