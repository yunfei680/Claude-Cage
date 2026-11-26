// SPDX-FileCopyrightText: Copyright The Lima Authors
// SPDX-License-Identifier: Apache-2.0

package qemu

import "testing"

func TestWHPXMachineCompatibility(t *testing.T) {
	for _, emulated := range []bool{false, true} {
		if emulated {
			t.Setenv(whpxEmulatedIRQChip, "off")
		} else {
			t.Setenv(whpxEmulatedIRQChip, "")
		}
		for _, accel := range []string{"whpx", "kvm", "hvf", "nvmm"} {
			want := "q35,accel=" + accel
			if accel == "whpx" && emulated {
				want += ",kernel-irqchip=off"
			}
			args := appendQ35Machine(nil, accel)
			if got, _ := argValue(args, "-machine"); got != want {
				t.Fatalf("%s (emulated irqchip %v): got %q, want %q", accel, emulated, got, want)
			}
		}
	}
	// Retain Lima's explicit override contract for custom QEMU arguments.
	args := appendQ35Machine([]string{"-machine", "q35,accel=whpx,kernel-irqchip=on"}, "whpx")
	if len(args) != 2 || args[1] != "q35,accel=whpx,kernel-irqchip=on" {
		t.Fatalf("overrode explicit machine: %v", args)
	}
}
