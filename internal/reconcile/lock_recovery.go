package reconcile

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/pulumi/pulumi/sdk/v3/go/auto"

	"github.com/ventus-ag/magnum-bootstrap/internal/logging"
)

var lockPIDPattern = regexp.MustCompile(`\(pid ([0-9]+)\)`)

func runWithAutoCancel[T any](ctx context.Context, logger *logging.Logger, stack *auto.Stack, stackName string, operation string, run func() (T, error)) (T, error) {
	result, err := run()
	if err == nil {
		return result, nil
	}
	if stack == nil || !auto.IsConcurrentUpdateError(err) {
		return result, err
	}

	stalePIDs, activePIDs := classifyLockOwnerPIDs(err.Error(), localProcessExists)
	if len(activePIDs) > 0 {
		if logger != nil {
			logger.Warnf("pulumi %s blocked by active stack lock stack=%s activePids=%s; not auto-canceling",
				operation, stackName, formatPIDList(activePIDs))
		}
		return result, err
	}
	if len(stalePIDs) == 0 {
		if logger != nil {
			logger.Warnf("pulumi %s blocked by stack lock stack=%s but no lock owner pid was found; not auto-canceling",
				operation, stackName)
		}
		return result, err
	}

	if logger != nil {
		logger.Warnf("pulumi %s found stale stack lock stack=%s stalePids=%s; running pulumi cancel",
			operation, stackName, formatPIDList(stalePIDs))
	}

	cancelStart := time.Now()
	cancelErr := stack.Cancel(ctx)
	if cancelErr != nil {
		if logger != nil {
			logger.Errorf("pulumi cancel failed stack=%s duration=%s stalePids=%s err=%v",
				stackName, formatDuration(time.Since(cancelStart)), formatPIDList(stalePIDs), cancelErr)
		}
	} else if logger != nil {
		logger.Warnf("pulumi cancel completed stack=%s duration=%s stalePids=%s",
			stackName, formatDuration(time.Since(cancelStart)), formatPIDList(stalePIDs))
	}

	if logger != nil {
		logger.Infof("retrying pulumi %s after stale lock recovery stack=%s", operation, stackName)
	}

	retryResult, retryErr := run()
	if retryErr != nil && cancelErr != nil {
		return retryResult, fmt.Errorf("retry after stale lock recovery failed: %w (cancel err: %v)", retryErr, cancelErr)
	}
	return retryResult, retryErr
}

func classifyLockOwnerPIDs(text string, processExists func(int) bool) ([]int, []int) {
	if processExists == nil {
		processExists = localProcessExists
	}

	pids := extractLockOwnerPIDs(text)
	stalePIDs := make([]int, 0, len(pids))
	activePIDs := make([]int, 0, len(pids))
	for _, pid := range pids {
		if processExists(pid) {
			activePIDs = append(activePIDs, pid)
		} else {
			stalePIDs = append(stalePIDs, pid)
		}
	}
	return stalePIDs, activePIDs
}

func extractLockOwnerPIDs(text string) []int {
	matches := lockPIDPattern.FindAllStringSubmatch(text, -1)
	if len(matches) == 0 {
		return nil
	}

	seen := make(map[int]struct{}, len(matches))
	pids := make([]int, 0, len(matches))
	for _, match := range matches {
		pid, err := strconv.Atoi(match[1])
		if err != nil || pid <= 0 {
			continue
		}
		if _, ok := seen[pid]; ok {
			continue
		}
		seen[pid] = struct{}{}
		pids = append(pids, pid)
	}
	sort.Ints(pids)
	return pids
}

func localProcessExists(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}

func formatPIDList(pids []int) string {
	if len(pids) == 0 {
		return "none"
	}

	parts := make([]string, 0, len(pids))
	for _, pid := range pids {
		parts = append(parts, strconv.Itoa(pid))
	}
	return strings.Join(parts, ",")
}
