//go:build linux

package components

import (
	"bufio"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"june/internal/util"
)

// pciVendors maps the PCI vendor ids sysfs reports to the names Platform uses.
var pciVendors = map[string]string{"0x10de": "nvidia", "0x1002": "amd", "0x8086": "intel"}

// detectGPU reads every display device the kernel's DRM layer knows from sysfs, then asks nvidia-smi for the NVIDIA card's name, driver and memory, which sysfs does not carry. Output: the card a GPU build would run on, and false when none was found.
func detectGPU() (GPU, bool) {
	var gpus []GPU
	cards, _ := filepath.Glob("/sys/class/drm/card[0-9]*")
	for _, card := range cards {
		// card0-HDMI-A-1 and the like are the card's connectors, not more cards.
		if strings.Contains(filepath.Base(card), "-") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(card, "device", "vendor"))
		if err != nil {
			continue
		}
		vendor, ok := pciVendors[strings.TrimSpace(string(raw))]
		if !ok {
			vendor = "other"
		}
		g := GPU{Vendor: vendor}
		// amdgpu publishes the card's memory; the others do not.
		if b, err := os.ReadFile(filepath.Join(card, "device", "mem_info_vram_total")); err == nil {
			if n, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64); err == nil {
				g.VRAMMB = int(n >> 20)
			}
		}
		gpus = append(gpus, g)
	}
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

// nvidiaSMI asks the NVIDIA driver's own tool, wherever the distribution put it on PATH, for the card's name, driver version and memory.
func nvidiaSMI() (GPU, bool) {
	bin, err := exec.LookPath("nvidia-smi")
	if err != nil {
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

// vulkanLoaderDirs are where distributions install libvulkan.so.1: Debian and Ubuntu's multiarch directories, Fedora's lib64 and Arch's plain lib.
var vulkanLoaderDirs = []string{"/usr/lib/x86_64-linux-gnu", "/lib/x86_64-linux-gnu", "/usr/lib/aarch64-linux-gnu", "/usr/lib64", "/usr/lib", "/lib64"}

// vulkanPresent reports whether the Vulkan loader is installed. llama.cpp's Vulkan build loads its Vulkan backend (libggml-vulkan.so, the only library of it that links libvulkan) at run time and falls back to its CPU backends without it, the same as on Windows, so this only decides which tarball is fetched: without a loader the CPU build is the same program at half the download.
func vulkanPresent() bool {
	for _, d := range vulkanLoaderDirs {
		if util.Exists(filepath.Join(d, "libvulkan.so.1")) {
			return true
		}
	}
	return false
}

// freeBytes is the space an unprivileged user may still write on dir's filesystem. Output: 0 when it cannot be read.
func freeBytes(dir string) int64 {
	var st unix.Statfs_t
	if err := unix.Statfs(existingDir(dir), &st); err != nil {
		return 0
	}
	return int64(st.Bavail) * int64(st.Bsize)
}

// diskFull reports whether a write failed because the filesystem, or the user's quota on it, is full.
func diskFull(err error) bool {
	return errors.Is(err, unix.ENOSPC) || errors.Is(err, unix.EDQUOT)
}

// smartAppControl is "off": it is a Windows feature.
func smartAppControl() string { return "off" }

// totalRAMMB reads MemTotal from /proc/meminfo. Output: the machine's memory in MB, or 0 when it cannot be read.
func totalRAMMB() int {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) >= 2 && fields[0] == "MemTotal:" {
			kb, err := strconv.ParseInt(fields[1], 10, 64)
			if err != nil {
				return 0
			}
			return int(kb >> 10)
		}
	}
	return 0
}
