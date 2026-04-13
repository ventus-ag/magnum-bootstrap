package hostresource

import (
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"syscall"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"github.com/ventus-ag/magnum-bootstrap/internal/host"
)

type OwnershipSpec struct {
	Path          string
	Owner         string
	Group         string
	Recursive     bool
	SkipIfMissing bool
}

type OwnershipResource struct {
	pulumi.ResourceState
}

type OwnershipState struct {
	Exists         bool
	Owner          string
	Group          string
	RecursiveMatch bool
}

func (spec OwnershipSpec) Apply(executor *host.Executor) (ApplyResult, error) {
	uid, gid, resolveErr := resolveIDs(spec.Owner, spec.Group)
	if !executor.Apply && resolveErr != nil {
		return ApplyResult{Changes: []host.Change{{
			Action:  host.ActionUpdate,
			Path:    spec.Path,
			Summary: fmt.Sprintf("set ownership on %s to %s:%s", spec.Path, spec.Owner, spec.Group),
		}}, Changed: true}, nil
	}
	if resolveErr != nil {
		return ApplyResult{}, resolveErr
	}
	needsApply, err := spec.needsApply(uid, gid)
	if err != nil {
		return ApplyResult{}, err
	}
	if !needsApply {
		return ApplyResult{}, nil
	}
	change := host.Change{
		Action:  host.ActionUpdate,
		Path:    spec.Path,
		Summary: fmt.Sprintf("set ownership on %s to %s:%s", spec.Path, spec.Owner, spec.Group),
	}
	if executor.Apply {
		args := make([]string, 0, 4)
		if spec.Recursive {
			args = append(args, "-R")
		}
		args = append(args, spec.Owner+":"+spec.Group, spec.Path)
		if err := executor.Run("chown", args...); err != nil {
			return ApplyResult{}, err
		}
	}
	return ApplyResult{Changes: []host.Change{change}, Changed: true}, nil
}

func (spec OwnershipSpec) Observe(_ *host.Executor) (OwnershipState, error) {
	info, err := os.Stat(spec.Path)
	if err != nil {
		if os.IsNotExist(err) {
			return OwnershipState{}, nil
		}
		return OwnershipState{}, err
	}
	owner, group, err := lookupOwnerGroup(info)
	if err != nil {
		return OwnershipState{}, err
	}
	state := OwnershipState{Exists: true, Owner: owner, Group: group, RecursiveMatch: true}
	if spec.Recursive {
		uid, gid, err := resolveIDs(spec.Owner, spec.Group)
		if err != nil {
			return OwnershipState{}, err
		}
		needsApply, err := spec.needsApply(uid, gid)
		if err != nil {
			return OwnershipState{}, err
		}
		state.RecursiveMatch = !needsApply
	}
	return state, nil
}

func (spec OwnershipSpec) Diff(state OwnershipState) DriftResult {
	var reasons []string
	if !state.Exists {
		if !spec.SkipIfMissing {
			reasons = append(reasons, "path missing for ownership enforcement")
		}
		return newDriftResult(reasons...)
	}
	if state.Owner != spec.Owner {
		reasons = append(reasons, "owner differs")
	}
	if state.Group != spec.Group {
		reasons = append(reasons, "group differs")
	}
	if spec.Recursive && !state.RecursiveMatch {
		reasons = append(reasons, "recursive ownership differs")
	}
	return newDriftResult(reasons...)
}

func (spec OwnershipSpec) Register(ctx *pulumi.Context, name string, opts ...pulumi.ResourceOption) (pulumi.Resource, error) {
	res := &OwnershipResource{}
	if err := ctx.RegisterComponentResource("magnum:host:Ownership", name, res, opts...); err != nil {
		return nil, err
	}
	outputs := pulumi.Map{
		"path":          pulumi.String(spec.Path),
		"owner":         pulumi.String(spec.Owner),
		"group":         pulumi.String(spec.Group),
		"recursive":     pulumi.Bool(spec.Recursive),
		"skipIfMissing": pulumi.Bool(spec.SkipIfMissing),
	}
	executor := host.NewExecutor(false, nil)
	state, err := spec.Observe(executor)
	if err != nil {
		outputs["observeError"] = pulumi.String(err.Error())
	} else {
		drift := spec.Diff(state)
		outputs["observedExists"] = pulumi.Bool(state.Exists)
		outputs["observedOwner"] = pulumi.String(state.Owner)
		outputs["observedGroup"] = pulumi.String(state.Group)
		outputs["observedRecursiveMatch"] = pulumi.Bool(state.RecursiveMatch)
		outputs["drifted"] = pulumi.Bool(drift.Changed)
		outputs["driftReasons"] = pulumiStringArray(drift.Reasons)
	}
	if err := ctx.RegisterResourceOutputs(res, outputs); err != nil {
		return nil, err
	}
	return res, nil
}

func (spec OwnershipSpec) needsApply(uid, gid int) (bool, error) {
	if _, err := os.Stat(spec.Path); err != nil {
		if os.IsNotExist(err) && spec.SkipIfMissing {
			return false, nil
		}
		return false, err
	}
	if spec.Recursive {
		needs := false
		err := filepath.Walk(spec.Path, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			mismatch, err := ownershipMismatch(info, uid, gid)
			if err != nil {
				return err
			}
			if mismatch {
				needs = true
				return filepath.SkipAll
			}
			return nil
		})
		if err == filepath.SkipAll {
			return true, nil
		}
		return needs, err
	}
	info, err := os.Stat(spec.Path)
	if err != nil {
		return false, err
	}
	return ownershipMismatch(info, uid, gid)
}

func ownershipMismatch(info os.FileInfo, uid, gid int) (bool, error) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return false, fmt.Errorf("unsupported stat type for %s", info.Name())
	}
	return int(stat.Uid) != uid || int(stat.Gid) != gid, nil
}

func resolveIDs(owner, group string) (int, int, error) {
	u, err := user.Lookup(owner)
	if err != nil {
		return 0, 0, err
	}
	g, err := user.LookupGroup(group)
	if err != nil {
		return 0, 0, err
	}
	uid, err := strconv.Atoi(u.Uid)
	if err != nil {
		return 0, 0, err
	}
	gid, err := strconv.Atoi(g.Gid)
	if err != nil {
		return 0, 0, err
	}
	return uid, gid, nil
}

func lookupOwnerGroup(info os.FileInfo) (string, string, error) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return "", "", fmt.Errorf("unsupported stat type for %s", info.Name())
	}
	owner := strconv.Itoa(int(stat.Uid))
	group := strconv.Itoa(int(stat.Gid))
	if u, err := user.LookupId(owner); err == nil {
		owner = u.Username
	}
	if g, err := user.LookupGroupId(group); err == nil {
		group = g.Name
	}
	return owner, group, nil
}
