package run

// executorContract keeps runtime capabilities independent of version spelling.
func executorContract(version string) (adapter string, adjudication, known bool) {
	switch version {
	case ProviderAuditExecutorVersion:
		return "", false, true // Historical registered adapters share this runtime.
	case SupportAgentExecutorVersion, SupportRecoveryExecutorVersion, SupportApprovalExecutorVersion:
		return "support-agent-v1", false, true
	case SupportFixedComparisonExecutorVersion:
		return "support-fixed-v1", false, true
	case SupportAdjudicationExecutorVersion, SupportAdjudicationV2ExecutorVersion,
		SupportAdjudicationV3ExecutorVersion, SupportAdjudicationV4ExecutorVersion, SupportAdjudicationV5ExecutorVersion:
		return "support-agent-v1", true, true
	case SupportFixedAdjudicationExecutorVersion, SupportFixedBundleExecutorVersion:
		return "support-fixed-v1", true, true
	default:
		return "", false, false
	}
}

// IsAuditedExecutor recognizes only installed, versioned audited runtimes.
func IsAuditedExecutor(version string) bool {
	_, _, known := executorContract(version)
	return known
}

// IsAdjudicationExecutor identifies runtimes with the separate final model stage.
func IsAdjudicationExecutor(version string) bool {
	_, adjudication, _ := executorContract(version)
	return adjudication
}

// IsAgentAdjudicationExecutor excludes fixed baselines from approval and recovery.
func IsAgentAdjudicationExecutor(version string) bool {
	adapter, adjudication, _ := executorContract(version)
	return adjudication && adapter == "support-agent-v1"
}

// ExecutorMatchesAdapter preserves the historical audit runtime's open registry.
func ExecutorMatchesAdapter(version, adapter string) bool {
	want, _, known := executorContract(version)
	return known && (want == adapter || want == "" && adapter != "support-agent-v1")
}

func supportExecutor(schema int) string {
	switch schema {
	case 1:
		return ProviderAuditExecutorVersion
	case 2:
		return SupportAgentExecutorVersion
	case 3:
		return SupportRecoveryExecutorVersion
	case 4, 6, 7, 9:
		return SupportApprovalExecutorVersion
	case 5, 8, 10:
		return SupportFixedComparisonExecutorVersion
	case 11:
		return SupportAdjudicationExecutorVersion
	case 12:
		return SupportFixedAdjudicationExecutorVersion
	case 13:
		return SupportAdjudicationV2ExecutorVersion
	case 14:
		return SupportAdjudicationV3ExecutorVersion
	case 15:
		return SupportAdjudicationV4ExecutorVersion
	case 16, 18:
		return SupportAdjudicationV5ExecutorVersion
	case 17:
		return SupportFixedBundleExecutorVersion
	default:
		return ""
	}
}
