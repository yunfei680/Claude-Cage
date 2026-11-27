// SPDX-FileCopyrightText: Copyright The Lima Authors
// SPDX-License-Identifier: Apache-2.0

package qemu

import (
	"os"
	"path/filepath"
)

const (
	// WHPXEmulatedIRQChipEnv set to "off" restores QEMU's own interrupt
	// controller emulation on WHPX for every VM.
	WHPXEmulatedIRQChipEnv = "CAGE_WHPX_KERNEL_IRQCHIP"
	// WHPXEmulatedIRQChipFile in an instance directory does the same for that
	// VM. Cage writes it when the VM could not start with the hypervisor's
	// interrupt controller.
	WHPXEmulatedIRQChipFile = "cage-whpx-emulated-irqchip"
)

// WHPXEmulatedIRQChip reports whether a WHPX VM in instanceDir starts with
// QEMU's emulated interrupt controller (kernel-irqchip=off).
func WHPXEmulatedIRQChip(instanceDir string) bool {
	if os.Getenv(WHPXEmulatedIRQChipEnv) == "off" {
		return true
	}
	if instanceDir == "" {
		return false
	}
	_, err := os.Stat(filepath.Join(instanceDir, WHPXEmulatedIRQChipFile))
	return err == nil
}

func appendQ35Machine(args []string, accel, instanceDir string) []string {
	machine := "q35,accel=" + accel
	// QEMU 11 keeps the local APIC in the hypervisor by default: guest timers
	// and inter-processor interrupts are delivered there. With
	// kernel-irqchip=off every timer waits for QEMU's host loop instead (a
	// 1 ms guest sleep measured about 7 ms) and each interrupt exits to QEMU.
	// It was off because WHPX can refuse nested virtualization (HRESULT
	// 80370302); Cage's QEMU now continues without it.
	if accel == "whpx" && WHPXEmulatedIRQChip(instanceDir) {
		machine += ",kernel-irqchip=off"
	}
	return appendArgsIfNoConflict(args, "-machine", machine)
}
