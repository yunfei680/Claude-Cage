package vm

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/lima-vm/lima/v2/pkg/driver/qemu"
	"github.com/lima-vm/lima/v2/pkg/limatype"
	"github.com/lima-vm/lima/v2/pkg/limatype/filenames"

	"github.com/sylvinus/agent-vm/internal/version"
)

// whpxHost is the host system of a QEMU VM that uses WHPX; tests replace it.
var whpxHost = runtime.GOOS

// bootFaultInterval is how often a starting VM's serial console is checked.
var bootFaultInterval = time.Second

// bootFaultMarkers on the serial console show that the firmware or the
// kernel stopped during boot, as both can when they cannot use the
// interrupt controller. The start would otherwise only fail on its timeout.
var bootFaultMarkers = [][]byte{
	[]byte("X64 Exception Type"),
	[]byte("Kernel panic - not syncing"),
}

var errBootFault = errors.New("the VM stopped during boot (see serial.log)")

// startWithIRQChipFallback starts a WHPX VM with the hypervisor's interrupt
// controller and, when that start fails, once more with QEMU's emulated one,
// which Cage used before. The instance keeps the emulated controller until
// Cage is updated, so later starts do not wait for the same failure.
func startWithIRQChipFallback(ctx context.Context, inst *limatype.Instance, log io.Writer, start, stop func(context.Context) error) error {
	if whpxHost != "windows" || inst.VMType != limatype.QEMU || qemu.WHPXEmulatedIRQChip("") {
		return start(ctx)
	}
	marker := filepath.Join(inst.Dir, qemu.WHPXEmulatedIRQChipFile)
	if data, err := os.ReadFile(marker); err == nil {
		if strings.TrimSpace(string(data)) == version.Version {
			return start(ctx)
		}
		// A new Cage version tries the hypervisor's controller again.
		if err := os.Remove(marker); err != nil {
			return start(ctx)
		}
	}
	watched, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	go watchBootFaults(watched, filepath.Join(inst.Dir, filenames.SerialLog), time.Now(), cancel)
	err := start(watched)
	if err == nil || ctx.Err() != nil {
		return err
	}
	if cause := context.Cause(watched); errors.Is(cause, errBootFault) {
		err = errBootFault
	}
	fmt.Fprintf(log, "环境未能使用虚拟机监控程序的中断控制器启动（%v），改用 QEMU 模拟的中断控制器重试。\n", err)
	if stopErr := stop(ctx); stopErr != nil {
		return errors.Join(err, stopErr)
	}
	if writeErr := os.WriteFile(marker, []byte(version.Version+"\n"), 0o644); writeErr != nil {
		return errors.Join(err, writeErr)
	}
	if retryErr := start(ctx); retryErr != nil {
		// The emulated controller did not help either, so the cause lies
		// elsewhere: keep the faster controller for the next start.
		_ = os.Remove(marker)
		return retryErr
	}
	return nil
}

// watchBootFaults reports errBootFault through fault when the serial
// console written since begin shows a boot fault.
func watchBootFaults(ctx context.Context, path string, begin time.Time, fault context.CancelCauseFunc) {
	ticker := time.NewTicker(bootFaultInterval)
	defer ticker.Stop()
	var scan bootFaultScan
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if scan.faulted(path, begin) {
			fault(errBootFault)
			return
		}
	}
}

// bootFaultScan reads a growing console log once, keeping enough of each read
// to find a marker split across two reads.
type bootFaultScan struct {
	offset int64
	tail   []byte
}

func (s *bootFaultScan) faulted(path string, begin time.Time) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	info, err := f.Stat()
	// QEMU truncates the log when it starts; until then it belongs to the
	// previous boot.
	if err != nil || info.ModTime().Before(begin) {
		return false
	}
	if info.Size() < s.offset {
		s.offset, s.tail = 0, nil
	}
	if info.Size() == s.offset {
		return false
	}
	if _, err := f.Seek(s.offset, io.SeekStart); err != nil {
		return false
	}
	chunk, err := io.ReadAll(io.LimitReader(f, 4<<20))
	if err != nil {
		return false
	}
	s.offset += int64(len(chunk))
	data := append(s.tail, chunk...)
	for _, marker := range bootFaultMarkers {
		if bytes.Contains(data, marker) {
			return true
		}
	}
	keep := 64
	if len(data) < keep {
		keep = len(data)
	}
	s.tail = append([]byte(nil), data[len(data)-keep:]...)
	return false
}
