package main

import (
	"context"
	"errors"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

// nodesByRole returns the cluster's servers by Nova name: "-master-" for
// masters, "-node-" for workers (default nodegroup and nodepools alike).
func (r *runner) nodesByRole(ctx context.Context, role string) ([]nodeAddr, error) {
	all, err := r.clusterNodeIPs(ctx)
	if err != nil {
		return nil, err
	}
	marker := "-node-"
	if role == "master" {
		marker = "-master-"
	}
	var out []nodeAddr
	for _, n := range all {
		if strings.Contains(n.name, marker) {
			out = append(out, n)
		}
	}
	return out, nil
}

// nodeExec runs cmd on n over SSH. Connection failures are retried (a node may
// be mid-restart); a command that ran and exited non-zero is returned as is.
func (r *runner) nodeExec(ctx context.Context, n nodeAddr, cmd string) (string, error) {
	signer, err := r.sshSigner()
	if err != nil {
		return "", err
	}
	var out string
	for attempt := 1; attempt <= 3; attempt++ {
		out, err = sshRun(ctx, n.ip, r.cfg.sshUser, signer, cmd)
		var exitErr *ssh.ExitError
		if err == nil || errors.As(err, &exitErr) {
			return out, err
		}
		select {
		case <-ctx.Done():
			return out, ctx.Err()
		case <-time.After(10 * time.Second):
		}
	}
	return out, err
}
