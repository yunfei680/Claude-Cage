// SPDX-FileCopyrightText: Copyright The Lima Authors
// SPDX-License-Identifier: Apache-2.0

package qemu

import "os"

// whpxEmulatedIRQChip names the setting that restores QEMU's own interrupt
// controller emulation on WHPX.
const whpxEmulatedIRQChip = "CAGE_WHPX_KERNEL_IRQCHIP"

func appendQ35Machine(args []string, accel string) []string {
	machine := "q35,accel=" + accel
	// QEMU 11 keeps the local APIC in the hypervisor by default: guest timers
	// and inter-processor interrupts are delivered there. With
	// kernel-irqchip=off every timer waits for QEMU's host loop instead (a
	// 1 ms guest sleep measured about 8 ms) and each interrupt exits to QEMU.
	// It was off because WHPX can refuse nested virtualization (HRESULT
	// 80370302); Cage's QEMU now continues without it. Setting
	// CAGE_WHPX_KERNEL_IRQCHIP=off restores the emulated interrupt controller.
	if accel == "whpx" && os.Getenv(whpxEmulatedIRQChip) == "off" {
		machine += ",kernel-irqchip=off"
	}
	return appendArgsIfNoConflict(args, "-machine", machine)
}
