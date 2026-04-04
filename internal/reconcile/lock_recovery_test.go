package reconcile

import (
	"reflect"
	"testing"
)

func TestExtractLockOwnerPIDsDeduplicatesAndSorts(t *testing.T) {
	text := `error: the stack is currently locked by 2 lock(s).
file:///var/lib/magnum/pulumi/.pulumi/locks/organization/magnum-bootstrap/node-a/1.json?no_tmp_dir=true: created by root@node-a (pid 42) at 2026-04-04T01:21:50Z
file:///var/lib/magnum/pulumi/.pulumi/locks/organization/magnum-bootstrap/node-a/2.json?no_tmp_dir=true: created by root@node-a (pid 7) at 2026-04-04T01:22:50Z
file:///var/lib/magnum/pulumi/.pulumi/locks/organization/magnum-bootstrap/node-a/3.json?no_tmp_dir=true: created by root@node-a (pid 42) at 2026-04-04T01:23:50Z`

	got := extractLockOwnerPIDs(text)
	want := []int{7, 42}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("expected pids %v, got %v", want, got)
	}
}

func TestClassifyLockOwnerPIDsSeparatesStaleFromActive(t *testing.T) {
	text := `stderr: error: the stack is currently locked by 2 lock(s).
file:///var/lib/magnum/pulumi/.pulumi/locks/organization/magnum-bootstrap/node-a/1.json?no_tmp_dir=true: created by root@node-a (pid 101) at 2026-04-04T01:21:50Z
file:///var/lib/magnum/pulumi/.pulumi/locks/organization/magnum-bootstrap/node-a/2.json?no_tmp_dir=true: created by root@node-a (pid 202) at 2026-04-04T01:22:50Z`

	stale, active := classifyLockOwnerPIDs(text, func(pid int) bool {
		return pid == 202
	})

	if want := []int{101}; !reflect.DeepEqual(stale, want) {
		t.Fatalf("expected stale pids %v, got %v", want, stale)
	}
	if want := []int{202}; !reflect.DeepEqual(active, want) {
		t.Fatalf("expected active pids %v, got %v", want, active)
	}
}

func TestClassifyLockOwnerPIDsHandlesMissingPIDs(t *testing.T) {
	text := `stderr: error: the stack is currently locked by 1 lock(s).`

	stale, active := classifyLockOwnerPIDs(text, func(pid int) bool { return false })
	if len(stale) != 0 || len(active) != 0 {
		t.Fatalf("expected no parsed pids, got stale=%v active=%v", stale, active)
	}
}
