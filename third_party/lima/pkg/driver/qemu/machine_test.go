// SPDX-FileCopyrightText: Copyright The Lima Authors
// SPDX-License-Identifier: Apache-2.0

package qemu

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWHPXMachineCompatibility(t *testing.T) {
	fallback := t.TempDir()
	if err := os.WriteFile(filepath.Join(fallback, WHPXEmulatedIRQChipFile), []byte("0.2.1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, env, dir string
		emulated       bool
	}{
		{name: "default", dir: t.TempDir()},
		{name: "no instance directory"},
		{name: "explicitly on", env: "on", dir: t.TempDir()},
		{name: "setting off", env: "off", dir: t.TempDir(), emulated: true},
		{name: "instance fell back", dir: fallback, emulated: true},
	} {
		t.Setenv(WHPXEmulatedIRQChipEnv, tc.env)
		for _, accel := range []string{"whpx", "kvm", "hvf", "nvmm"} {
			want := "q35,accel=" + accel
			if accel == "whpx" && tc.emulated {
				want += ",kernel-irqchip=off"
			}
			args := appendQ35Machine(nil, accel, tc.dir)
			if got, _ := argValue(args, "-machine"); got != want {
				t.Fatalf("%s, %s: got %q, want %q", tc.name, accel, got, want)
			}
		}
	}
	// Retain Lima's explicit override contract for custom QEMU arguments.
	args := appendQ35Machine([]string{"-machine", "q35,accel=whpx,kernel-irqchip=on"}, "whpx", fallback)
	if len(args) != 2 || args[1] != "q35,accel=whpx,kernel-irqchip=on" {
		t.Fatalf("overrode explicit machine: %v", args)
	}
}
