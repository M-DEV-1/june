package components

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// GPU is the graphics card a component's build was chosen for. VRAMMB is 0 when the driver does not say.
type GPU struct {
	Vendor string `json:"vendor"`
	Name   string `json:"name"`
	VRAMMB int    `json:"vram_mb"`
	Driver string `json:"driver"`
}

// Platform is what GET /components reports about the machine, and what picks each feature's build. GPU is nil when no card could be read; FreeBytes and RAMMB are 0 when unknown. SmartAppControl is Windows' Smart App Control, "on", "evaluation", "off" or "unknown" (always "off" elsewhere), for the window to warn on the cards before a download it would block. noCUDA says the CUDA whisper build already failed its test on this card and driver.
type Platform struct {
	OS              string `json:"os"`
	GPU             *GPU   `json:"gpu,omitempty"`
	Vulkan          bool   `json:"vulkan"`
	FreeBytes       int64  `json:"free_bytes"`
	RAMMB           int    `json:"ram_mb"`
	SmartAppControl string `json:"smart_app_control"`
	noCUDA          bool
}

// CUDA's minimums for the whisper.cpp CUDA 12.4 build. The runtime it ships needs an R528 driver or newer on Windows, and whisper-medium on the card takes about 2.2 GB, so a card with less than 3 GB would fail to allocate and fall back to the CPU on every run anyway.
const (
	cudaMinDriver = 528
	cudaMinVRAMMB = 3 * 1024
)

// meets reports whether the machine has what a build needs: "cuda" for an NVIDIA card whisper.cpp's CUDA build can run on, "vulkan" for a Vulkan loader, "" for nothing.
func (p Platform) meets(need string) bool {
	switch need {
	case "":
		return true
	case "cuda":
		g := p.GPU
		if g == nil || p.noCUDA || g.Vendor != "nvidia" || g.VRAMMB < cudaMinVRAMMB {
			return false
		}
		major, _, _ := strings.Cut(g.Driver, ".")
		n, err := strconv.Atoi(major)
		return err == nil && n >= cudaMinDriver
	case "vulkan":
		return p.Vulkan
	}
	return false
}

// detectPlatform reads the machine: its GPU, whether a Vulkan loader is installed, how much memory it has, and how much room is left on the volume dataDir is on. The GPU and memory never change under a running daemon, so the caller keeps them; free space is read fresh each time.
func detectPlatform(dataDir string) Platform {
	p := Platform{OS: runtime.GOOS, Vulkan: vulkanPresent(), RAMMB: totalRAMMB()}
	if g, ok := detectGPU(); ok {
		p.GPU = &g
	}
	p.FreeBytes = freeBytes(dataDir)
	return p
}

// existingDir walks up from dir to the nearest directory that exists, since the data directory's components folder may not have been made yet when free space is asked for. Output: that directory, or dir itself when nothing above it exists.
func existingDir(dir string) string {
	for d := dir; ; {
		if info, err := os.Stat(d); err == nil && info.IsDir() {
			return d
		}
		parent := filepath.Dir(d)
		if parent == d {
			return dir
		}
		d = parent
	}
}

// pickGPU returns the card a GPU build would run on: an NVIDIA card if there is one, since that is the only vendor a GPU build here is chosen for, and otherwise the one with the most memory, which on a laptop with two is the discrete one.
func pickGPU(gpus []GPU) (GPU, bool) {
	if len(gpus) == 0 {
		return GPU{}, false
	}
	best := gpus[0]
	for _, g := range gpus[1:] {
		if (g.Vendor == "nvidia") != (best.Vendor == "nvidia") {
			if g.Vendor == "nvidia" {
				best = g
			}
			continue
		}
		if g.VRAMMB > best.VRAMMB {
			best = g
		}
	}
	return best, true
}

// parseNvidiaSMI reads the first card of nvidia-smi's --query-gpu=name,driver_version,memory.total --format=csv,noheader,nounits output. Output: the card, and false when the output has no line in that shape.
func parseNvidiaSMI(out string) (GPU, bool) {
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Split(strings.TrimSpace(line), ",")
		if len(fields) != 3 {
			continue
		}
		mem, err := strconv.Atoi(strings.TrimSpace(fields[2]))
		if err != nil {
			continue
		}
		return GPU{Vendor: "nvidia", Name: strings.TrimSpace(fields[0]), Driver: strings.TrimSpace(fields[1]), VRAMMB: mem}, true
	}
	return GPU{}, false
}
