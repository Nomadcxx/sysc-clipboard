package wayland

import "testing"

func TestGeneratedClipboardProtocolsExposeInterfaceNames(t *testing.T) {
	if ExtDataControlManagerV1InterfaceName != "ext_data_control_manager_v1" {
		t.Fatalf("ext interface name = %q", ExtDataControlManagerV1InterfaceName)
	}
	if ZwlrDataControlManagerV1InterfaceName != "zwlr_data_control_manager_v1" {
		t.Fatalf("wlr interface name = %q", ZwlrDataControlManagerV1InterfaceName)
	}
}
