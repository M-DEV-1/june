package util

import (
	"reflect"
	"slices"
	"testing"
)

// --no-mmap is present exactly when a device is pinned and -ngl covers the model, for every combination of device flag spelling, -ngl value and extra arguments.
func TestMmapForDevice(t *testing.T) {
	devices := [][]string{nil, {"-dev", "Vulkan1"}, {"--device", "CUDA0"}}
	layers := []struct {
		args   []string
		covers bool
	}{
		{nil, false}, {[]string{"-ngl", "0"}, false}, {[]string{"-ngl", "20"}, false}, {[]string{"-ngl", "98"}, false},
		{[]string{"-ngl", "99"}, true}, {[]string{"-ngl", "999"}, true}, {[]string{"-ngl", "all"}, true}, {[]string{"--gpu-layers", "99"}, true},
	}
	extras := [][]string{nil, {"-m", "model.gguf", "--port", "8080"}, {"--embedding", "-c", "8192"}}
	for _, dev := range devices {
		for _, ngl := range layers {
			for _, extra := range extras {
				for _, withFlag := range []bool{false, true} {
					base := slices.Concat(extra, ngl.args, dev)
					in := slices.Clone(base)
					if withFlag {
						in = append(in, "--no-mmap")
					}
					wantFlag := dev != nil && ngl.covers
					got := MmapForDevice(in)

					if has := slices.Contains(got, "--no-mmap"); has != wantFlag {
						t.Errorf("MmapForDevice(%v) = %v, --no-mmap present = %v, want %v", in, got, has, wantFlag)
					}
					if n := countOf(got, "--no-mmap"); n > 1 {
						t.Errorf("MmapForDevice(%v) = %v, has --no-mmap %d times", in, got, n)
					}
					if rest := slices.DeleteFunc(slices.Clone(got), func(a string) bool { return a == "--no-mmap" }); !slices.Equal(rest, base) {
						t.Errorf("MmapForDevice(%v) changed arguments other than --no-mmap: %v", in, got)
					}
					if again := MmapForDevice(got); !slices.Equal(again, got) {
						t.Errorf("MmapForDevice is not stable: %v then %v", got, again)
					}
				}
			}
		}
	}
}

// Dropping the device after a failed start leaves a CPU argument list, so --no-mmap goes with it and the rest of the arguments stay in order.
func TestDropDeviceArgRemovesNoMmap(t *testing.T) {
	for _, flag := range []string{"-dev", "--device"} {
		args := MmapForDevice([]string{"-m", "model.gguf", "-ngl", "99", flag, "Vulkan1", "--port", "8080"})
		if !slices.Contains(args, "--no-mmap") {
			t.Fatalf("setup: %v has no --no-mmap", args)
		}
		rest, device := DropDeviceArg(args)
		if device != "Vulkan1" {
			t.Errorf("DropDeviceArg device = %q, want Vulkan1", device)
		}
		want := []string{"-m", "model.gguf", "-ngl", "99", "--port", "8080"}
		if !reflect.DeepEqual(rest, want) {
			t.Errorf("DropDeviceArg(%v) = %v, want %v", args, rest, want)
		}
	}
}

func countOf(args []string, want string) (n int) {
	for _, a := range args {
		if a == want {
			n++
		}
	}
	return n
}
