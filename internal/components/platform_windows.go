//go:build windows

package components

import (
	"context"
	"encoding/binary"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"

	"june/internal/util"
)

// displayClassKey is the device-class key every display adapter's driver writes its description, vendor, version and memory size under, one numbered subkey per adapter. It is readable without admin, unlike WMI's view of the same thing, which also costs a COM round trip.
const displayClassKey = `SYSTEM\CurrentControlSet\Control\Class\{4d36e968-e325-11ce-bfc1-08002be10318}`

// detectGPU reads every display adapter from the registry, then asks nvidia-smi for the NVIDIA card's exact driver version and memory, which the registry only gives as the Windows driver version and, on some drivers, not at all. Output: the card a GPU build would run on, and false when none was found.
func detectGPU() (GPU, bool) {
	gpus := registryGPUs()
	if smi, ok := nvidiaSMI(); ok {
		replaced := false
		for i := range gpus {
			if gpus[i].Vendor == "nvidia" {
				gpus[i] = smi
				replaced = true
				break
			}
		}
		if !replaced {
			gpus = append(gpus, smi)
		}
	}
	return pickGPU(gpus)
}

// registryGPUs lists the display adapters under displayClassKey, skipping Microsoft's own basic and remote display drivers, which are no card at all.
func registryGPUs() []GPU {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, displayClassKey, registry.ENUMERATE_SUB_KEYS)
	if err != nil {
		return nil
	}
	names, _ := k.ReadSubKeyNames(-1)
	k.Close()
	var out []GPU
	for _, name := range names {
		// The numbered subkeys are the adapters; "Properties" and "Configuration" are not, and a normal user may not open them anyway.
		if len(name) != 4 || strings.Trim(name, "0123456789") != "" {
			continue
		}
		sub, err := registry.OpenKey(registry.LOCAL_MACHINE, displayClassKey+`\`+name, registry.QUERY_VALUE)
		if err != nil {
			continue
		}
		desc, _, _ := sub.GetStringValue("DriverDesc")
		provider, _, _ := sub.GetStringValue("ProviderName")
		version, _, _ := sub.GetStringValue("DriverVersion")
		mem := registryBytes(sub, "HardwareInformation.qwMemorySize")
		if mem == 0 {
			mem = registryBytes(sub, "HardwareInformation.MemorySize")
		}
		sub.Close()
		if desc == "" || strings.EqualFold(provider, "Microsoft") {
			continue
		}
		g := GPU{Vendor: vendorOf(provider + " " + desc), Name: desc, VRAMMB: int(mem >> 20), Driver: version}
		if g.Vendor == "nvidia" {
			g.Driver = nvidiaDriverFromWindows(version)
		}
		out = append(out, g)
	}
	return out
}

// vendorOf names a card's maker from whatever its driver calls it.
func vendorOf(s string) string {
	low := strings.ToLower(s)
	switch {
	case strings.Contains(low, "nvidia"):
		return "nvidia"
	case strings.Contains(low, "advanced micro devices"), strings.Contains(low, "amd"), strings.Contains(low, "ati technologies"), strings.Contains(low, "radeon"):
		return "amd"
	case strings.Contains(low, "intel"):
		return "intel"
	}
	return "other"
}

// registryBytes reads a memory size the driver stored either as a number or as raw little-endian bytes, both of which drivers in the wild do.
func registryBytes(k registry.Key, name string) uint64 {
	if v, _, err := k.GetIntegerValue(name); err == nil {
		return v
	}
	b, _, err := k.GetBinaryValue(name)
	if err != nil {
		return 0
	}
	switch {
	case len(b) >= 8:
		return binary.LittleEndian.Uint64(b)
	case len(b) >= 4:
		return uint64(binary.LittleEndian.Uint32(b))
	}
	return 0
}

// nvidiaDriverFromWindows turns the Windows driver version NVIDIA registers ("32.0.15.9579") into the version NVIDIA itself publishes ("595.79"): the last five digits of the last two fields. Input: the registry's DriverVersion. Output: NVIDIA's version, or the input unchanged when it is not in that shape.
func nvidiaDriverFromWindows(v string) string {
	fields := strings.Split(v, ".")
	if len(fields) != 4 {
		return v
	}
	digits := fields[2] + fields[3]
	if len(digits) < 5 {
		return v
	}
	digits = digits[len(digits)-5:]
	return strings.TrimLeft(digits[:3], "0") + "." + digits[3:]
}

// nvidiaSMI asks the NVIDIA driver's own tool for the card's name, driver version and memory. It ships in System32 with every current NVIDIA driver; a machine without one simply has no NVIDIA card to report.
func nvidiaSMI() (GPU, bool) {
	bin := filepath.Join(os.Getenv("SystemRoot"), "System32", "nvidia-smi.exe")
	if !util.Exists(bin) {
		return GPU{}, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "--query-gpu=name,driver_version,memory.total", "--format=csv,noheader,nounits")
	util.HideConsole(cmd)
	out, err := cmd.Output()
	if err != nil {
		return GPU{}, false
	}
	return parseNvidiaSMI(string(out))
}

// vulkanPresent reports whether the Vulkan loader is installed, which every current GPU driver puts in System32. llama.cpp's Vulkan backend loads it at run time, and without it the build falls back to its CPU backends.
func vulkanPresent() bool {
	return util.Exists(filepath.Join(os.Getenv("SystemRoot"), "System32", "vulkan-1.dll"))
}

// freeBytes is the space the current user may still write on dir's volume, honouring disk quotas. Output: 0 when it cannot be read.
func freeBytes(dir string) int64 {
	p, err := windows.UTF16PtrFromString(existingDir(dir))
	if err != nil {
		return 0
	}
	var free, total, totalFree uint64
	if err := windows.GetDiskFreeSpaceEx(p, &free, &total, &totalFree); err != nil {
		return 0
	}
	return int64(free)
}

// diskFull reports whether a write failed because the volume, or the user's quota on it, is full.
func diskFull(err error) bool {
	return errors.Is(err, windows.ERROR_DISK_FULL) || errors.Is(err, windows.ERROR_HANDLE_DISK_FULL)
}

// ciPolicyKey holds Smart App Control's state in VerifiedAndReputablePolicyState: 0 off, 1 on, 2 evaluation (Windows watching how the PC is used before it decides whether to turn it on). A normal user may read it.
const ciPolicyKey = `SYSTEM\CurrentControlSet\Control\CI\Policy`

// smartAppControl reads whether Smart App Control is on, which blocks every program and DLL that is neither signed nor known to Microsoft's cloud, with no exception for one program; the whisper.cpp, llama.cpp and sherpa-onnx builds June downloads are unsigned. Output: "on", "evaluation", "off" (also for a Windows without the feature), or "unknown" when the value cannot be read.
func smartAppControl() string {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, ciPolicyKey, registry.QUERY_VALUE)
	if errors.Is(err, registry.ErrNotExist) {
		return "off"
	}
	if err != nil {
		return "unknown"
	}
	defer k.Close()
	v, _, err := k.GetIntegerValue("VerifiedAndReputablePolicyState")
	switch {
	case errors.Is(err, registry.ErrNotExist):
		return "off"
	case err != nil:
		return "unknown"
	case v == 0:
		return "off"
	case v == 1:
		return "on"
	case v == 2:
		return "evaluation"
	}
	return "unknown"
}

// memoryStatusEx is MEMORYSTATUSEX, which golang.org/x/sys/windows does not wrap.
type memoryStatusEx struct {
	Length               uint32
	MemoryLoad           uint32
	TotalPhys            uint64
	AvailPhys            uint64
	TotalPageFile        uint64
	AvailPageFile        uint64
	TotalVirtual         uint64
	AvailVirtual         uint64
	AvailExtendedVirtual uint64
}

var procGlobalMemoryStatusEx = windows.NewLazySystemDLL("kernel32.dll").NewProc("GlobalMemoryStatusEx")

// totalRAMMB is the machine's physical memory in MB, or 0 when Windows will not say.
func totalRAMMB() int {
	var m memoryStatusEx
	m.Length = uint32(unsafe.Sizeof(m))
	if r, _, _ := procGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&m))); r == 0 {
		return 0
	}
	return int(m.TotalPhys >> 20)
}
