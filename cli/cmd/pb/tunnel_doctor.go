package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/buildinfo"
	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/health"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/supportbundle"
	"github.com/pinksaucepasta/paperboat/shared/httptransport"
	"github.com/spf13/cobra"
)

func collectTunnelDoctorLocalReport(ctx context.Context) (localDoctorReport, error) {
	if ctx == nil {
		return localDoctorReport{}, context.Canceled
	}
	if err := ctx.Err(); err != nil {
		return localDoctorReport{}, err
	}
	report := localDoctorReport{HostRuntime: "unavailable", IdentityState: "unavailable", CredentialState: "unavailable"}
	stateRoot, err := previewRuntimeStateRoot()
	if err != nil {
		return report, nil
	}
	if privateReferenceAvailable(filepath.Join(stateRoot, "machine-identity.json")) && privateReferenceAvailable(filepath.Join(stateRoot, "machine-registration.json")) {
		report.IdentityState = "available"
	}
	if privateReferenceAvailable(filepath.Join(stateRoot, "machine-control.json")) {
		report.CredentialState = "available"
	}
	localBody, err := readOwnerOnlyFile(filepath.Join(stateRoot, "runtime", "worker-local.json"), 4096)
	if err != nil {
		return report, nil
	}
	var local struct {
		Schema        string `json:"schema"`
		ListenAddress string `json:"listen_address"`
	}
	decoder := json.NewDecoder(bytes.NewReader(localBody))
	decoder.DisallowUnknownFields()
	var extra any
	decodeErr := decoder.Decode(&local)
	extraErr := decoder.Decode(&extra)
	host, port, splitErr := net.SplitHostPort(local.ListenAddress)
	address := net.ParseIP(host)
	endpointURL, endpointErr := tunnelHostRuntimeURL(local.ListenAddress, "/healthz")
	if decodeErr != nil || extraErr != io.EOF || local.Schema != "paperboat.worker-local/v1" || splitErr != nil || address == nil || !address.IsLoopback() || port == "" || endpointErr != nil {
		report.HostRuntime = "invalid_local_endpoint"
		return report, nil
	}
	probeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	request, requestErr := http.NewRequestWithContext(probeCtx, http.MethodGet, endpointURL, nil)
	if requestErr != nil {
		return report, nil
	}
	client, clientErr := httptransport.NewLoopbackClient(2 * time.Second)
	if clientErr != nil {
		return report, nil
	}
	defer client.CloseIdleConnections()
	response, err := client.Do(request)
	if err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return localDoctorReport{}, contextErr
		}
		return report, nil
	}
	defer response.Body.Close()
	contentTypes := response.Header.Values("Content-Type")
	live, decodeErr := decodeTunnelDoctorHealth(response.Body, 64<<10)
	if response.StatusCode != http.StatusOK || len(contentTypes) != 1 || !strings.EqualFold(strings.TrimSpace(contentTypes[0]), "application/json") || decodeErr != nil || !live {
		report.HostRuntime = "unhealthy"
		return report, nil
	}
	report.HostRuntime = "ready"
	return report, nil
}

func decodeTunnelDoctorHealth(reader io.Reader, maximumBytes int64) (bool, error) {
	if reader == nil || maximumBytes < 1 {
		return false, api.ErrUnsafeTunnelResponse
	}
	body, err := io.ReadAll(io.LimitReader(reader, maximumBytes+1))
	if err != nil || int64(len(body)) > maximumBytes {
		return false, api.ErrUnsafeTunnelResponse
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return false, api.ErrUnsafeTunnelResponse
	}
	found := false
	live := false
	for decoder.More() {
		nameToken, tokenErr := decoder.Token()
		name, ok := nameToken.(string)
		if tokenErr != nil || !ok || name != "live" || found {
			return false, api.ErrUnsafeTunnelResponse
		}
		if err := decoder.Decode(&live); err != nil {
			return false, api.ErrUnsafeTunnelResponse
		}
		found = true
	}
	if token, err = decoder.Token(); err != nil || token != json.Delim('}') || !found {
		return false, api.ErrUnsafeTunnelResponse
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return false, api.ErrUnsafeTunnelResponse
	}
	return live, nil
}

func privateReferenceAvailable(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && ownerOnlyRegularFile(path, info)
}

func tunnelStatusCommand() *cobra.Command {
	command := tunnelCommand("status <tunnel>", "Show tunnel health", cobra.ExactArgs(1), func(command *cobra.Command, args []string) error {
		if ephemeral, _ := command.Flags().GetBool("ephemeral"); ephemeral {
			return runPreviewStatusCobra(command, args)
		}
		return runTunnelStatus(command, args)
	})
	command.Flags().Bool("watch", false, "watch for health changes")
	command.Flags().Duration("interval", time.Second, "watch polling interval (250ms-1m)")
	tunnelJSONFlag(command)
	return command
}

// tunnelStatusText is the human status: the unchanged "id code summary" line, then, when the tunnel is
// not ready, what to do about it and whether Paperboat keeps retrying by itself.
func tunnelStatusText(health api.TunnelHealth) string {
	text := fmt.Sprintf("%s\t%s\t%s", health.ResourceID, health.OverallCode, health.Summary)
	if health.OverallCode == "ready" {
		return text
	}
	if health.RepairAction != "" && health.RepairAction != "No action required." {
		text += "\n  What to do: " + health.RepairAction
	}
	if health.Retrying {
		text += "\n  Paperboat keeps retrying; this clears by itself once the cause is fixed."
	}
	return text
}

func runTunnelStatus(command *cobra.Command, args []string) error {
	watch, _ := command.Flags().GetBool("watch")
	interval, _ := command.Flags().GetDuration("interval")
	if interval < 250*time.Millisecond || interval > time.Minute {
		return errors.New("interval must be between 250ms and 1m")
	}
	client, ctx, err := tunnelClient(command)
	if err != nil {
		return err
	}
	tunnelID, err := resolveTunnelSelectorForCommand(ctx, client, args[0])
	if err != nil {
		return err
	}
	last := ""
	for {
		out, e := retryTunnelRead(ctx, func() (api.TunnelHealth, error) {
			return client.TunnelStatusV1(ctx, tunnelID)
		})
		if e != nil {
			return e
		}
		signatureBytes, marshalErr := json.Marshal(out)
		if marshalErr != nil {
			return fmt.Errorf("encode health update: %w", marshalErr)
		}
		signature := string(signatureBytes)
		if !watch || signature != last {
			if e = tunnelOutput(command, out, tunnelStatusText(out)); e != nil {
				return e
			}
			last = signature
		}
		if !watch {
			return nil
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func tunnelDoctorCommand() *cobra.Command {
	command := tunnelCommand("doctor <tunnel>", "Diagnose tunnel health", cobra.ExactArgs(1), runTunnelDoctor)
	command.Flags().String("bundle", "", "absolute path for a local support-bundle preview")
	command.Flags().Bool("write-bundle", false, "write the exact previewed bundle (requires --bundle)")
	command.Flags().Int64("bundle-max-bytes", tunnelDoctorBundleDefaultBytes, "maximum bundle bytes (64KiB-8MiB)")
	tunnelJSONFlag(command)
	return command
}

type tunnelDoctorBundlePreview struct {
	Path       string                 `json:"path"`
	SizeBytes  int64                  `json:"size_bytes"`
	SHA256     string                 `json:"sha256"`
	Redactions int                    `json:"redactions"`
	Manifest   supportbundle.Manifest `json:"manifest"`
}

type tunnelDoctorOutput struct {
	Schema  string                     `json:"schema"`
	Health  api.TunnelHealth           `json:"health"`
	Bundle  *tunnelDoctorBundlePreview `json:"bundle,omitempty"`
	Written *supportbundle.WriteResult `json:"written,omitempty"`
}

type tunnelDoctorEvidence struct {
	Schema string                      `json:"schema"`
	Checks []tunnelDoctorEvidenceCheck `json:"checks"`
}

type tunnelDoctorEvidenceCheck struct {
	Code    string `json:"code"`
	Status  string `json:"status"`
	Summary string `json:"summary"`
	Value   string `json:"value,omitempty"`
}

func runTunnelDoctor(command *cobra.Command, args []string) error {
	bundlePath, _ := command.Flags().GetString("bundle")
	writeBundle, _ := command.Flags().GetBool("write-bundle")
	maximumBytes, _ := command.Flags().GetInt64("bundle-max-bytes")
	if writeBundle && bundlePath == "" {
		return errors.New("--write-bundle requires --bundle with an absolute new output path")
	}
	if bundlePath == "" && command.Flags().Changed("bundle-max-bytes") {
		return errors.New("--bundle-max-bytes requires --bundle")
	}
	if bundlePath != "" {
		if maximumBytes < tunnelDoctorBundleMinBytes || maximumBytes > tunnelDoctorBundleMaxBytes {
			return errors.New("bundle-max-bytes must be between 65536 and 8388608")
		}
		if !filepath.IsAbs(bundlePath) || filepath.Clean(bundlePath) != bundlePath {
			return errors.New("bundle path must be absolute and clean")
		}
		if err := supportbundle.ValidateOutputPath(bundlePath); err != nil {
			return err
		}
	}

	client, ctx, err := tunnelClient(command)
	if err != nil {
		return err
	}
	tunnelID, err := resolveTunnelSelectorForCommand(ctx, client, args[0])
	if err != nil {
		return err
	}
	health, err := retryTunnelRead(ctx, func() (api.TunnelHealth, error) {
		return client.TunnelStatusV1(ctx, tunnelID)
	})
	if err != nil {
		return err
	}
	jsonOutput, _ := command.Flags().GetBool("json")
	if bundlePath == "" {
		if jsonOutput {
			return tunnelOutput(command, health, "")
		}
		return writeTunnelDoctorHealth(command.OutOrStdout(), health)
	}

	builder, preview, err := buildTunnelDoctorBundle(ctx, health, maximumBytes)
	if err != nil {
		return err
	}
	bundlePreview := tunnelDoctorBundlePreview{
		Path: bundlePath, SizeBytes: preview.SizeBytes(), SHA256: preview.SHA256(),
		Redactions: preview.Manifest.Redactions, Manifest: preview.Manifest,
	}
	result := tunnelDoctorOutput{Schema: "paperboat.tunnel_doctor.v1", Health: health, Bundle: &bundlePreview}
	if !jsonOutput {
		if err := writeTunnelDoctorHealth(command.OutOrStdout(), health); err != nil {
			return err
		}
		if err := writeTunnelDoctorBundlePreview(command.OutOrStdout(), bundlePreview, writeBundle); err != nil {
			return err
		}
	}
	if writeBundle {
		written, writeErr := builder.Write(ctx, preview, bundlePath)
		if writeErr != nil {
			return writeErr
		}
		result.Written = &written
		if !jsonOutput {
			_, err = fmt.Fprintf(command.OutOrStdout(), "Bundle written: %s\n", written.Path)
			return err
		}
	}
	if jsonOutput {
		return json.NewEncoder(command.OutOrStdout()).Encode(result)
	}
	return nil
}

func writeTunnelDoctorHealth(writer io.Writer, health api.TunnelHealth) error {
	if _, err := fmt.Fprintf(writer, "Tunnel %s: %s\n%s\n", health.ResourceID, health.OverallCode, health.Summary); err != nil {
		return err
	}
	for _, dimension := range []struct {
		name  string
		value api.TunnelHealthDimension
	}{{"service", health.Dimensions.Service}, {"edge", health.Dimensions.Edge}, {"config", health.Dimensions.Config}, {"route", health.Dimensions.Route}, {"origin", health.Dimensions.Origin}, {"dns", health.Dimensions.DNS}, {"certificate", health.Dimensions.Certificate}, {"access", health.Dimensions.Access}, {"update", health.Dimensions.Update}} {
		if _, err := fmt.Fprintf(writer, "%-12s %s", dimension.name, dimension.value.Status); err != nil {
			return err
		}
		if dimension.value.Code != "" {
			if _, err := fmt.Fprintf(writer, " (%s)", dimension.value.Code); err != nil {
				return err
			}
		}
		if _, err := fmt.Fprintln(writer); err != nil {
			return err
		}
	}
	if health.RepairAction != "" {
		_, err := fmt.Fprintf(writer, "Repair: %s\n", health.RepairAction)
		return err
	}
	return nil
}

func writeTunnelDoctorBundlePreview(writer io.Writer, preview tunnelDoctorBundlePreview, writing bool) error {
	lines := []string{
		"Support bundle preview:",
		fmt.Sprintf("Path: %s", preview.Path),
		fmt.Sprintf("Schema: %s", preview.Manifest.SchemaVersion),
		fmt.Sprintf("Format: %s", preview.Manifest.Format),
		fmt.Sprintf("Limits: items=%d item_bytes=%d total_bytes=%d collector_timeout_ms=%d total_timeout_ms=%d", preview.Manifest.Limits.MaxItems, preview.Manifest.Limits.MaxItemBytes, preview.Manifest.Limits.MaxTotalBytes, preview.Manifest.Limits.PerCollectorTimeoutMillis, preview.Manifest.Limits.TotalTimeoutMillis),
		fmt.Sprintf("Items: %d", len(preview.Manifest.Items)),
		fmt.Sprintf("Size: %d bytes", preview.SizeBytes),
		fmt.Sprintf("SHA-256: %s", preview.SHA256),
		fmt.Sprintf("Redactions: %d", preview.Redactions),
		"Collectors:",
	}
	for _, collector := range preview.Manifest.Collectors {
		line := fmt.Sprintf("- %s: %s", collector.Name, collector.Result)
		if collector.ErrorCode != "" {
			line += fmt.Sprintf(" (%s)", collector.ErrorCode)
		}
		lines = append(lines, line)
	}
	lines = append(lines, "Inventory:")
	for _, item := range preview.Manifest.Items {
		lines = append(lines, fmt.Sprintf("- %s: %s, %d bytes, sha256:%s, redactions=%d", item.Path, item.Result, item.SizeBytes, item.SHA256, item.Redactions))
	}
	if writing {
		lines = append(lines, "Explicit write requested; writing the exact previewed bytes.")
	} else {
		lines = append(lines, "No file was written. Review this preview, then add --write-bundle to write it locally.")
	}
	_, err := fmt.Fprintln(writer, strings.Join(lines, "\n"))
	return err
}

func buildTunnelDoctorBundle(ctx context.Context, health api.TunnelHealth, maximumBytes int64) (*supportbundle.Builder, supportbundle.Preview, error) {
	healthBody, err := json.Marshal(health)
	if err != nil {
		return nil, supportbundle.Preview{}, fmt.Errorf("encode tunnel health evidence: %w", err)
	}
	var (
		hostDiagnostics     tunnelHostDiagnostics
		hostDiagnosticsErr  error
		hostDiagnosticsDone bool
	)
	loadHostDiagnostics := func(collectorCtx context.Context) (tunnelHostDiagnostics, error) {
		if !hostDiagnosticsDone {
			hostDiagnostics, hostDiagnosticsErr = tunnelDoctorHostDiagnosticsForCommand(collectorCtx)
			hostDiagnosticsDone = true
		}
		return hostDiagnostics, hostDiagnosticsErr
	}
	collectors := []supportbundle.Collector{
		supportbundle.CollectorFunc{CollectorName: "host_diagnostics", CollectFunc: func(collectorCtx context.Context) ([]supportbundle.CollectedItem, error) {
			diagnostics, collectErr := loadHostDiagnostics(collectorCtx)
			if collectErr != nil {
				if collectorCtx.Err() != nil {
					return nil, errors.Join(collectErr, collectorCtx.Err(), context.Cause(collectorCtx))
				}
				// Hostd versions before the typed diagnostics endpoint are a
				// supported installation state. The bundle builder records this
				// bounded collector failure while retaining all other evidence.
				return nil, collectErr
			}
			body, encodeErr := json.Marshal(diagnostics)
			if encodeErr != nil {
				return nil, encodeErr
			}
			return []supportbundle.CollectedItem{{Path: "diagnostics/host-runtime.json", Kind: supportbundle.ItemKindText, Data: append([]byte(nil), body...)}}, nil
		}},
		supportbundle.CollectorFunc{CollectorName: "local_evidence", CollectFunc: func(collectorCtx context.Context) ([]supportbundle.CollectedItem, error) {
			local, collectErr := tunnelDoctorLocalReportForCommand(collectorCtx)
			if collectErr != nil {
				return nil, collectErr
			}
			var diagnostics *tunnelHostDiagnostics
			if value, diagnosticsErr := loadHostDiagnostics(collectorCtx); diagnosticsErr == nil {
				diagnostics = &value
			} else if collectorCtx.Err() != nil {
				return nil, errors.Join(diagnosticsErr, collectorCtx.Err(), context.Cause(collectorCtx))
			}
			evidence := buildTunnelDoctorEvidenceWithHost(collectorCtx, local, health, diagnostics)
			if err := collectorCtx.Err(); err != nil {
				return nil, err
			}
			evidenceBody, encodeErr := json.Marshal(evidence)
			if encodeErr != nil {
				return nil, encodeErr
			}
			return []supportbundle.CollectedItem{{Path: "diagnostics/local-evidence.json", Kind: supportbundle.ItemKindText, Data: append([]byte(nil), evidenceBody...)}}, nil
		}},
		supportbundle.CollectorFunc{CollectorName: "tunnel_health", CollectFunc: func(context.Context) ([]supportbundle.CollectedItem, error) {
			return []supportbundle.CollectedItem{{Path: "health/tunnel.json", Kind: supportbundle.ItemKindText, Data: append([]byte(nil), healthBody...)}}, nil
		}},
	}
	limits := supportbundle.Defaults()
	limits.MaxItems = 8
	limits.MaxTotalBytes = maximumBytes
	if limits.MaxItemBytes > maximumBytes {
		limits.MaxItemBytes = maximumBytes
	}
	builder, err := supportbundle.New(supportbundle.Config{Limits: limits}, collectors...)
	if err != nil {
		return nil, supportbundle.Preview{}, err
	}
	preview, err := builder.Preview(ctx)
	if err == nil {
		if collectionErr := preview.CollectionError(); collectionErr != nil {
			errorreport.Current().ObserveFailure(ctx, "paperboat-cli", "tunnel", "command", "command_failed", collectionErr)
		}
	}
	return builder, preview, err
}

func buildTunnelDoctorEvidenceWithHost(ctx context.Context, local localDoctorReport, tunnelHealth api.TunnelHealth, localDiagnostics *tunnelHostDiagnostics) tunnelDoctorEvidence {
	dnsReady := false
	if ctx != nil && ctx.Err() == nil {
		dnsReady = tunnelDoctorDNSCheckForCommand(ctx)
	}
	runtimeCheck := doctorRuntimeCheck(local.HostRuntime)
	clockCheck := tunnelDoctorUnavailableCheck("clock_sanity", "A trusted server clock comparison is not exposed by the tunnel health contract.")
	transportCheck := tunnelDoctorUnavailableCheck("transport_fallback", "Per-transport fallback evidence is not exposed by the local runtime contract.")
	configurationCheck := tunnelDoctorUnavailableCheck("configuration_generation", "Desired and applied generation numbers are not exposed by the tunnel health response.")
	rollbackCheck := tunnelDoctorUnavailableCheck("rollback_slot", "Detailed rollback-slot evidence is not exposed by the local update contract.")
	if localDiagnostics != nil {
		if localDiagnostics.Health.Schema == health.HealthSchemaV1 {
			runtimeCheck = doctorTypedRuntimeCheck(localDiagnostics.Health)
			clockCheck = doctorTypedClockCheck(localDiagnostics.Health)
		}
		transportCheck = doctorTypedTransportCheck(*localDiagnostics)
		configurationCheck = doctorTypedConfigurationCheck(*localDiagnostics)
		rollbackCheck = doctorTypedRollbackCheck(*localDiagnostics)
	}
	edgeStatus := tunnelHealth.Dimensions.Edge.Status
	originStatus := tunnelHealth.Dimensions.Origin.Status
	routeStatus := tunnelHealth.Dimensions.Route.Status
	dnsStatus := tunnelHealth.Dimensions.DNS.Status
	certificateStatus := tunnelHealth.Dimensions.Certificate.Status
	updateStatus := tunnelHealth.Dimensions.Update.Status
	if localDiagnostics != nil && localDiagnostics.Health.Schema == health.HealthSchemaV1 {
		edgeStatus = doctorLocalDimensionStatus(edgeStatus, localDiagnostics.Health, health.DimensionEdge)
		originStatus = doctorLocalDimensionStatus(originStatus, localDiagnostics.Health, health.DimensionOrigin)
		routeStatus = doctorLocalDimensionStatus(routeStatus, localDiagnostics.Health, health.DimensionRoute)
		dnsStatus = doctorLocalDimensionStatus(dnsStatus, localDiagnostics.Health, health.DimensionDNS)
		certificateStatus = doctorLocalDimensionStatus(certificateStatus, localDiagnostics.Health, health.DimensionCertificate)
		updateStatus = doctorLocalDimensionStatus(updateStatus, localDiagnostics.Health, health.DimensionUpdate)
	}
	checks := []tunnelDoctorEvidenceCheck{
		{Code: "service_installed", Status: "unavailable", Summary: "Native service installation evidence is not available to this user-scoped command."},
		{Code: "service_enabled", Status: "unavailable", Summary: "Native service enablement evidence is not available to this user-scoped command."},
		runtimeCheck,
		{Code: "cli_version", Status: "ready", Summary: "The local CLI version is available.", Value: buildinfo.Version},
		{Code: "daemon_version", Status: "unavailable", Summary: "The daemon version is not exposed by the local health contract."},
		{Code: "connector_version", Status: "unavailable", Summary: "The connector version is not exposed by the local health contract."},
		doctorStateCheck(local.IdentityState),
		doctorCredentialCheck(local.CredentialState),
		clockCheck,
		doctorDNSCheck(dnsReady),
		doctorReportedCheck("edge_reachability", edgeStatus, "Edge reachability is reported by the control plane."),
		transportCheck,
		configurationCheck,
		doctorReportedCheck("origin_connection", originStatus, "Origin connection state is reported by tunnel health."),
		doctorReportedCheck("origin_tls", originStatus, "Origin TLS verification state is reported by tunnel health."),
		doctorReportedCheck("route_state", routeStatus, "Route state is reported by tunnel health."),
		doctorReportedCheck("domain_state", dnsStatus, "Domain and DNS state is reported by tunnel health."),
		doctorReportedCheck("certificate_state", certificateStatus, "Certificate state is reported by tunnel health."),
		doctorReportedCheck("update_health", updateStatus, "Update health is reported by tunnel health."),
		rollbackCheck,
		{Code: "resource_limits", Status: "ready", Summary: "Bounded platform capacity evidence is available.", Value: fmt.Sprintf("%s/%s cpu=%d", runtime.GOOS, runtime.GOARCH, runtime.NumCPU())},
	}
	return tunnelDoctorEvidence{Schema: "paperboat.tunnel_doctor_evidence.v1", Checks: checks}
}

func doctorLocalDimensionStatus(remote string, snapshot health.HealthSnapshot, dimension health.Dimension) string {
	if strings.TrimSpace(remote) != "" {
		return remote
	}
	return string(snapshot.Dimensions.Get(dimension).Status)
}

func tunnelDoctorUnavailableCheck(code, summary string) tunnelDoctorEvidenceCheck {
	return tunnelDoctorEvidenceCheck{Code: code, Status: "unavailable", Summary: summary}
}

func doctorTypedRuntimeCheck(snapshot health.HealthSnapshot) tunnelDoctorEvidenceCheck {
	check := tunnelDoctorEvidenceCheck{Code: "service_running", Summary: "The local typed runtime health snapshot is available."}
	check.Value = snapshot.Overall.Code
	switch snapshot.Overall.Status {
	case health.StatusReady, health.StatusNotApplicable:
		check.Status = "ready"
	case health.StatusDegraded, health.StatusUnknown:
		check.Status = "degraded"
	case health.StatusDown:
		check.Status = "degraded"
	default:
		check.Status = "unavailable"
	}
	return check
}

func doctorTypedClockCheck(snapshot health.HealthSnapshot) tunnelDoctorEvidenceCheck {
	if snapshot.UpdatedAt.IsZero() {
		return tunnelDoctorUnavailableCheck("clock_sanity", "The local typed health snapshot has no update timestamp.")
	}
	return tunnelDoctorEvidenceCheck{Code: "clock_sanity", Status: "ready", Summary: "The local typed health snapshot has a valid bounded timestamp.", Value: snapshot.UpdatedAt.UTC().Format(time.RFC3339)}
}

func doctorTypedTransportCheck(diagnostics tunnelHostDiagnostics) tunnelDoctorEvidenceCheck {
	var connected, failed bool
	transports := make([]string, 0, 2)
	for _, series := range diagnostics.Metrics {
		if series.Name != "paperboat_runtime_connector_retries_total" {
			continue
		}
		transport := series.Labels["transport"]
		if transport == "" || transport == "none" {
			continue
		}
		if series.Value > 0 {
			transports = append(transports, transport)
		}
		switch series.Labels["result"] {
		case "connected":
			connected = connected || series.Value > 0
		case "failed":
			failed = failed || series.Value > 0
		}
	}
	if len(transports) == 0 {
		return tunnelDoctorUnavailableCheck("transport_fallback", "The local typed runtime snapshot has no observed connector transport.")
	}
	sort.Strings(transports)
	transports = uniqueStrings(transports)
	check := tunnelDoctorEvidenceCheck{Code: "transport_fallback", Status: "ready", Summary: "The local typed runtime snapshot exposes connector transport activity.", Value: strings.Join(transports, ",")}
	if failed && !connected {
		check.Status = "degraded"
		check.Summary = "The local typed runtime snapshot exposes connector transport failures."
	}
	return check
}

func doctorTypedConfigurationCheck(diagnostics tunnelHostDiagnostics) tunnelDoctorEvidenceCheck {
	var generation uint64
	for _, event := range diagnostics.Events {
		if event.Generations.Config > generation {
			generation = event.Generations.Config
		}
	}
	if generation == 0 {
		return tunnelDoctorUnavailableCheck("configuration_generation", "The local typed runtime snapshot has no observed configuration generation.")
	}
	return tunnelDoctorEvidenceCheck{Code: "configuration_generation", Status: "ready", Summary: "The local typed runtime event log exposes a configuration generation.", Value: strconv.FormatUint(generation, 10)}
}

func doctorTypedRollbackCheck(diagnostics tunnelHostDiagnostics) tunnelDoctorEvidenceCheck {
	for _, series := range diagnostics.Metrics {
		if series.Name != "paperboat_runtime_update_rollbacks_total" {
			continue
		}
		check := tunnelDoctorEvidenceCheck{Code: "rollback_slot", Status: "ready", Summary: "The local typed runtime exposes update rollback evidence.", Value: strconv.FormatFloat(series.Value, 'f', -1, 64)}
		if series.Value > 0 {
			check.Status = "degraded"
			check.Summary = "The local typed runtime recorded one or more update rollbacks."
		}
		return check
	}
	return tunnelDoctorUnavailableCheck("rollback_slot", "The local typed runtime snapshot has no update rollback evidence.")
}

func uniqueStrings(values []string) []string {
	if len(values) < 2 {
		return values
	}
	result := values[:1]
	for _, value := range values[1:] {
		if value != result[len(result)-1] {
			result = append(result, value)
		}
	}
	return result
}

func doctorRuntimeCheck(state string) tunnelDoctorEvidenceCheck {
	switch state {
	case "ready":
		return tunnelDoctorEvidenceCheck{Code: "service_running", Status: "ready", Summary: "The local runtime health endpoint is ready."}
	case "unhealthy", "invalid_local_endpoint":
		return tunnelDoctorEvidenceCheck{Code: "service_running", Status: "degraded", Summary: "The local runtime health endpoint is not ready."}
	default:
		return tunnelDoctorEvidenceCheck{Code: "service_running", Status: "unavailable", Summary: "The local runtime health endpoint is unavailable."}
	}
}

func doctorStateCheck(state string) tunnelDoctorEvidenceCheck {
	if state == "valid" || state == "available" {
		return tunnelDoctorEvidenceCheck{Code: "local_state_integrity", Status: "ready", Summary: "Private local identity and registration references are available; their contents were not collected."}
	}
	return tunnelDoctorEvidenceCheck{Code: "local_state_integrity", Status: "unavailable", Summary: "Local identity integrity evidence is unavailable."}
}

func doctorCredentialCheck(state string) tunnelDoctorEvidenceCheck {
	switch state {
	case "valid", "available":
		return tunnelDoctorEvidenceCheck{Code: "credential_availability", Status: "ready", Summary: "A private credential reference is available; credential material was not read."}
	case "grace":
		return tunnelDoctorEvidenceCheck{Code: "credential_availability", Status: "degraded", Summary: "Credential authority is in its renewal grace period; credential material was not read."}
	default:
		return tunnelDoctorEvidenceCheck{Code: "credential_availability", Status: "unavailable", Summary: "Usable credential authority is unavailable; credential material was not read."}
	}
}

func doctorDNSCheck(ready bool) tunnelDoctorEvidenceCheck {
	if ready {
		return tunnelDoctorEvidenceCheck{Code: "dns_sanity", Status: "ready", Summary: "The local resolver can resolve the loopback host name."}
	}
	return tunnelDoctorEvidenceCheck{Code: "dns_sanity", Status: "unavailable", Summary: "The local resolver could not resolve the loopback host name."}
}

func doctorReportedCheck(code, status, summary string) tunnelDoctorEvidenceCheck {
	switch status {
	case "ready", "healthy":
		status = "ready"
	case "degraded", "down", "unknown", "not_applicable":
	default:
		status = "unavailable"
	}
	return tunnelDoctorEvidenceCheck{Code: code, Status: status, Summary: summary}
}
