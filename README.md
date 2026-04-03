# Magnum Bootstrap

Base implementation of the `bootstrap` reconciler binary.

Current scope:

- parse Magnum `heat-params` into a typed config model
- normalize master and worker inputs into one internal phase catalog
- persist local run journal and reconciler state
- write a stable node-local reconciler log
- execute native preview/apply modules without shelling out to the old Magnum scripts
- expose matching Pulumi component resources for the implemented module set
- emit Heat-compatible result JSON with explicit success/failure details

Current implemented modules:

- `prereq-validation`
- `client-tools`
- `kube-os-config`
- `admin-kubeconfig`
- `proxy-env`
- `health`

Current non-goal:

- full parity for every existing Magnum node phase

## Layout

- `cmd/bootstrap`
  Binary entrypoint and subcommand handling.
- `internal/config`
  `heat-params` parsing and normalized desired input.
- `internal/journal`
  Run-state journal for interrupted/in-flight runs.
- `internal/paths`
  Runtime file path contract from launcher environment.
- `internal/plan`
  Reconciler phase catalog for master and worker flows.
- `internal/host`
  Native host file, directory, export, and command reconciliation helpers.
- `internal/module`
  Native modules that own preview/apply behavior and Pulumi component shape.
- `internal/moduleapi`
  Neutral module request/result contract used by the runner and modules.
- `internal/provider`
  Input providers, starting with `heat-params`.
- `internal/pulumi`
  Pulumi SDK component graph assembly for the implemented modules.
- `internal/reconcile`
  Main orchestration flow for a single reconcile run.
- `internal/result`
  Structured result JSON output.
- `internal/state`
  Local applied state owned by the reconciler.

## Initial Commands

- `bootstrap validate-input`
  Parse `heat-params` and print the normalized role and operation.
- `bootstrap print-last-result`
  Print the last result JSON.
- `bootstrap plan --diff`
  Preview the selected phase flow and compute host changes without applying.
- `bootstrap plan --allow-partial`
  Preview implemented modules even when some phases are not migrated yet.
- `bootstrap up --diff`
  Apply the implemented module set for the selected phase flow.
- `bootstrap up --allow-partial`
  Apply only implemented modules and warn about skipped phases.
- `bootstrap run-once`
  Launcher alias for `bootstrap up`.
- `bootstrap run-periodic`
  Timer alias for `bootstrap up`.

## Runtime Contract

The launcher is expected to export:

- `MAGNUM_RECONCILE_HEAT_PARAMS_FILE`
- `MAGNUM_RECONCILE_RESULT_FILE`
- `MAGNUM_RECONCILE_LOG_FILE`
- `MAGNUM_RECONCILE_STATE_FILE`
- `MAGNUM_RECONCILE_RUN_STATE_FILE`
- `MAGNUM_RECONCILE_STATE_BACKUP_DIR`
- `MAGNUM_PULUMI_BACKEND_DIR`
- `MAGNUM_PULUMI_BACKEND_URL`
- `MAGNUM_PULUMI_BACKUP_DIR`

Defaults are aligned with the migration plan and the Magnum bootstrap
launcher.

For Heat-triggered runs, the bootstrap wrapper should print the reconciler
result JSON from `MAGNUM_RECONCILE_RESULT_FILE` to stdout so the existing
`heat-config-notify` path receives explicit `deploy_status_code`,
`deploy_stdout`, and `deploy_stderr` fields.
