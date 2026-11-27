package vm

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lima-vm/lima/v2/pkg/driver/qemu"
	"github.com/lima-vm/lima/v2/pkg/limatype"
	"github.com/lima-vm/lima/v2/pkg/limatype/filenames"

	"github.com/sylvinus/agent-vm/internal/version"
)

type fallbackRun struct {
	t        *testing.T
	inst     *limatype.Instance
	results  []error // returned by successive starts; a nil entry succeeds
	emulated []bool  // whether QEMU would have used kernel-irqchip=off, per start
	stops    int
	log      bytes.Buffer
}

func newFallbackRun(t *testing.T, results ...error) *fallbackRun {
	t.Helper()
	saved, savedVersion, savedInterval := whpxHost, version.Version, bootFaultInterval
	whpxHost, version.Version, bootFaultInterval = "windows", "0.2.1", 10*time.Millisecond
	t.Cleanup(func() { whpxHost, version.Version, bootFaultInterval = saved, savedVersion, savedInterval })
	t.Setenv(qemu.WHPXEmulatedIRQChipEnv, "")
	return &fallbackRun{t: t, inst: &limatype.Instance{VMType: limatype.QEMU, Dir: t.TempDir()}, results: results}
}

func (r *fallbackRun) start(ctx context.Context) error {
	r.emulated = append(r.emulated, qemu.WHPXEmulatedIRQChip(r.inst.Dir))
	if len(r.emulated) > len(r.results) {
		r.t.Fatalf("unexpected start %d", len(r.emulated))
	}
	return r.results[len(r.emulated)-1]
}

func (r *fallbackRun) stop(context.Context) error { r.stops++; return nil }

func (r *fallbackRun) run(ctx context.Context) error {
	return startWithIRQChipFallback(ctx, r.inst, &r.log, r.start, r.stop)
}

func (r *fallbackRun) marker() (string, bool) {
	data, err := os.ReadFile(filepath.Join(r.inst.Dir, qemu.WHPXEmulatedIRQChipFile))
	return strings.TrimSpace(string(data)), err == nil
}

func sameModes(a, b []bool) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestIRQChipFallbackStartsFastFirst(t *testing.T) {
	r := newFallbackRun(t, nil)
	if err := r.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !sameModes(r.emulated, []bool{false}) || r.stops != 0 {
		t.Fatalf("starts %v, stops %d", r.emulated, r.stops)
	}
	if _, ok := r.marker(); ok {
		t.Fatal("a successful start left a fallback marker")
	}
}

func TestIRQChipFallbackRetriesEmulated(t *testing.T) {
	r := newFallbackRun(t, errors.New("did not receive an event with the `running` status"), nil)
	if err := r.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !sameModes(r.emulated, []bool{false, true}) || r.stops != 1 {
		t.Fatalf("starts %v, stops %d", r.emulated, r.stops)
	}
	if got, ok := r.marker(); !ok || got != "0.2.1" {
		t.Fatalf("marker %q, %v", got, ok)
	}
	if !strings.Contains(r.log.String(), "QEMU 模拟的中断控制器") {
		t.Fatalf("no notice: %q", r.log.String())
	}
	// The next start of this version goes straight to the emulated controller.
	r.results = append(r.results, nil)
	if err := r.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !sameModes(r.emulated, []bool{false, true, true}) || r.stops != 1 {
		t.Fatalf("starts %v, stops %d", r.emulated, r.stops)
	}
	// An update tries the hypervisor's controller again.
	version.Version = "0.2.2"
	r.results = append(r.results, nil)
	if err := r.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !sameModes(r.emulated, []bool{false, true, true, false}) {
		t.Fatalf("starts %v", r.emulated)
	}
	if _, ok := r.marker(); ok {
		t.Fatal("the update kept the old fallback")
	}
}

func TestIRQChipFallbackUnrelatedFailure(t *testing.T) {
	first, second := errors.New("proxy unavailable"), errors.New("proxy still unavailable")
	r := newFallbackRun(t, first, second)
	if err := r.run(context.Background()); !errors.Is(err, second) {
		t.Fatalf("got %v", err)
	}
	if !sameModes(r.emulated, []bool{false, true}) {
		t.Fatalf("starts %v", r.emulated)
	}
	if _, ok := r.marker(); ok {
		t.Fatal("kept the emulated controller although it did not help")
	}
}

func TestIRQChipFallbackRespectsSettingAndOtherHosts(t *testing.T) {
	failed := errors.New("failed")
	r := newFallbackRun(t, failed)
	t.Setenv(qemu.WHPXEmulatedIRQChipEnv, "off")
	if err := r.run(context.Background()); !errors.Is(err, failed) || len(r.emulated) != 1 || r.stops != 0 {
		t.Fatalf("setting off: %v, starts %v, stops %d", err, r.emulated, r.stops)
	}

	r = newFallbackRun(t, failed)
	whpxHost = "darwin"
	if err := r.run(context.Background()); !errors.Is(err, failed) || len(r.emulated) != 1 || r.stops != 0 {
		t.Fatalf("macOS: %v, starts %v, stops %d", err, r.emulated, r.stops)
	}

	r = newFallbackRun(t, failed)
	r.inst.VMType = limatype.WSL2
	if err := r.run(context.Background()); !errors.Is(err, failed) || len(r.emulated) != 1 {
		t.Fatalf("WSL2: %v, starts %v", err, r.emulated)
	}
}

func TestIRQChipFallbackKeepsCancelledStart(t *testing.T) {
	r := newFallbackRun(t, context.Canceled)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := r.run(ctx); !errors.Is(err, context.Canceled) || len(r.emulated) != 1 || r.stops != 0 {
		t.Fatalf("got %v, starts %v, stops %d", err, r.emulated, r.stops)
	}
}

func TestIRQChipFallbackStopsOnBootFault(t *testing.T) {
	r := newFallbackRun(t)
	serial := filepath.Join(r.inst.Dir, filenames.SerialLog)
	// The previous boot's log must not count.
	if err := os.WriteFile(serial, []byte("!!!! X64 Exception Type - 0D(#GP)\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(serial, old, old); err != nil {
		t.Fatal(err)
	}
	starts := 0
	start := func(ctx context.Context) error {
		starts++
		if starts == 2 {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
		if err := os.WriteFile(serial, []byte("BdsDxe: starting Boot0001\n!!!! X64 Exception Type - 0E(#PF - Page-Fault)\n"), 0o644); err != nil {
			t.Error(err)
		}
		<-ctx.Done()
		return ctx.Err()
	}
	err := startWithIRQChipFallback(context.Background(), r.inst, &r.log, start, r.stop)
	if err != nil || starts != 2 || r.stops != 1 {
		t.Fatalf("got %v, starts %d, stops %d", err, starts, r.stops)
	}
	if !strings.Contains(r.log.String(), "serial.log") {
		t.Fatalf("notice does not name the fault: %q", r.log.String())
	}
}

func TestBootFaultScan(t *testing.T) {
	path := filepath.Join(t.TempDir(), "serial.log")
	begin := time.Now().Add(-time.Second)
	var scan bootFaultScan
	if scan.faulted(path, begin) {
		t.Fatal("a missing log faulted")
	}
	write := func(data string, appendTo bool) {
		t.Helper()
		flags := os.O_CREATE | os.O_WRONLY | os.O_TRUNC
		if appendTo {
			flags = os.O_CREATE | os.O_WRONLY | os.O_APPEND
		}
		f, err := os.OpenFile(path, flags, 0o644)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.WriteString(data); err != nil {
			t.Fatal(err)
		}
		f.Close()
	}
	write("[    0.000000] Linux version 6.12\n[    1.2] Kernel pa", false)
	if scan.faulted(path, begin) {
		t.Fatal("faulted on a partial marker")
	}
	write("nic - not syncing: IO-APIC + timer doesn't work!\n", true)
	if !scan.faulted(path, begin) {
		t.Fatal("missed a marker split across reads")
	}
	// A restarted QEMU truncates the log.
	scan = bootFaultScan{}
	write(strings.Repeat("boot\n", 1000), false)
	if scan.faulted(path, begin) {
		t.Fatal("faulted on a clean boot")
	}
	write("short\n", false)
	if scan.faulted(path, begin) || scan.offset != int64(len("short\n")) {
		t.Fatalf("truncated log not rescanned from the start: offset %d", scan.offset)
	}
}
