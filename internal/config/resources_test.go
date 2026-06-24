package config

import "testing"

func TestParallelismFor(t *testing.T) {
	cases := []struct {
		name   string
		memMiB int
		numCPU int
		want   int
	}{
		// CPU not limiting (plenty of cores) — exercise the RAM tiers.
		{"unknown-ram", 0, 16, 4},
		{"2gib-vc2", 1953, 16, 1}, // the OOM case: serialize
		{"just-under-2.5gib", 2559, 16, 1},
		{"3gib", 3072, 16, 2},
		{"6gib", 6144, 16, 4},
		{"12gib", 12288, 16, 8},
		{"32gib", 32768, 16, 10}, // capped at maxAutoParallelism
		{"boundary-2560", 2560, 16, 2},
		{"boundary-4096", 4096, 16, 4},
		{"boundary-8192", 8192, 16, 8},
		{"boundary-16384", 16384, 16, 10},

		// CPU clamp: 2 vCPU -> ceiling of 4, never below 1.
		{"2gib-2cpu", 1953, 2, 1},
		{"32gib-2cpu", 32768, 2, 4},        // 10 clamped to 2*2
		{"32gib-1cpu", 32768, 1, 2},        // clamped to 2*1
		{"12gib-2cpu", 12288, 2, 4},        // 8 clamped to 4
		{"zero-cpu-ignored", 32768, 0, 10}, // numCPU<=0 -> no clamp
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := parallelismFor(tc.memMiB, tc.numCPU); got != tc.want {
				t.Fatalf("parallelismFor(%d,%d) = %d, want %d", tc.memMiB, tc.numCPU, got, tc.want)
			}
		})
	}
}

func TestAutoParallelismSane(t *testing.T) {
	// On any real host the result must be within [1, maxAutoParallelism].
	if p := AutoParallelism(); p < 1 || p > maxAutoParallelism {
		t.Fatalf("AutoParallelism() = %d, out of range [1,%d]", p, maxAutoParallelism)
	}
}
